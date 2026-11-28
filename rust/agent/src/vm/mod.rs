//! Firecracker microVM lifecycle manager for felucca-agent (v2).
//!
//! Port of backend/src/agent/vm.zig — Manager struct, all lifecycle methods.
//! Data layout: {data_dir}/instances/{dir_id}/ with rootfs.ext4, fc.sock,
//! serial.log, meta.json, vmstate.bin, mem.bin.

pub mod image;
pub mod meta;
pub mod pool;
pub mod procscan;
pub mod reconcile;
pub mod watch;

use crate::fc;
use crate::ipalloc::{Allocator, Cidr};
use crate::net;
use tracing::{error, info, warn};
use meta::{ExposeEntry, Meta, VmState};
use procscan::{FcProc, Identity, ProcTable};
use serde::Deserialize;
use std::path::Path;
use std::sync::Arc;
use tokio::sync::Mutex;
use tokio::time::{sleep, Duration, Instant};

/// In-memory VM record.
#[derive(Debug, Clone)]
pub struct Vm {
    pub id: String,
    pub name: String,
    /// On-disk instance directory id (path component). Normally equals `id`,
    /// but a pool-claimed VM keeps the pool dir_id so the FC drive path baked
    /// into snapshots remains valid.
    pub dir_id: String,
    pub vcpus: u32,
    pub mem_mib: u64,
    pub state: VmState,
    pub pid: Option<i32>,
    pub slot: Option<u32>,
    pub ip: Option<String>,
    /// v3: VM has a Firecracker vsock device (guest agent reachable).
    pub vsock: bool,
    /// v4: optional tenant identifier for nftables isolation.
    pub tenant_id: Option<String>,
    /// v4 P3: ingress DNAT mappings (node_port → this VM's ip:guest_port).
    pub exposes: Vec<ExposeEntry>,
    /// v4 P4: template image the rootfs was copied from ("ubuntu-base" default).
    pub image: String,
    /// v4 P4: rootfs grow size in GiB (0 = image size, no resize).
    pub disk_gb: u32,
    /// v4 P4: sha256 the image was verified against at create/prewarm time
    /// (None = trusted local file). Part of the pool match shape, so a pooled
    /// VM prewarmed from an old capture is never claimed for a new sha.
    pub image_sha256: Option<String>,
    /// FLU-4: the snapshot files are complete and belong to this `sleeping`
    /// record (`Meta::snapshot_valid`). Kept here because meta.json is
    /// rebuilt from this record on every write — an expose of a sleeping VM
    /// must not drop it.
    pub snapshot_valid: bool,
}

/// Default template image; pre-P4 metas and requests without `image` map here.
pub const DEFAULT_IMAGE: &str = "ubuntu-base";

/// Strict VM-id check, the same discipline `image::valid_image_name` applies
/// for the same reason: an id becomes `{data_dir}/instances/{id}` and every
/// file under it (rootfs.ext4, serial.log, meta.json, fc.sock, v.sock,
/// vmstate.bin, mem.bin). `[A-Za-z0-9._-]`, 1..=64, no leading '.' or '-' and
/// no ".." — so an id can only ever name a direct child of the instances
/// directory, never traverse out of it.
///
/// The character class is wider than an image name's (ids from feluccad are
/// mixed case, e.g. `sb-<hex>`), but the traversal-relevant rules are identical.
pub fn valid_vm_id(id: &str) -> bool {
    if id.is_empty() || id.len() > 64 {
        return false;
    }
    let first = id.as_bytes()[0];
    if first == b'.' || first == b'-' {
        return false;
    }
    if id.contains("..") {
        return false;
    }
    id.bytes()
        .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'_' || b == b'-')
}

/// The instance directory for a dir id, or None when the id is not a plain
/// path component. Every path the manager derives goes through here: the
/// handlers reject bad ids first, but a path built from an unvalidated id is a
/// root-level write and a recursive delete on the worker, so it is re-checked
/// at the point of use rather than trusted.
fn instance_path(data_dir: &str, dir_id: &str) -> Option<String> {
    if !valid_vm_id(dir_id) {
        return None;
    }
    Some(format!("{}/instances/{}", data_dir, dir_id))
}

/// All create parameters in one place (v4 P4 grew the list past comfortable
/// positional args). `image` is pre-validated by the server handler;
/// `image_sha256` is None for "trust the local file".
#[derive(Debug, Clone)]
pub struct CreateSpec {
    pub id: String,
    pub name: String,
    pub vcpus: u32,
    pub mem_mib: u64,
    pub tenant_id: Option<String>,
    pub image: String,
    pub image_sha256: Option<String>,
    pub disk_gb: u32,
}

/// One feluccad-managed warm-pool template (v4 P4). `image_sha256` may be
/// empty ("trust the local file"); `count` is the refill target.
#[derive(Debug, Clone, PartialEq, Deserialize)]
pub struct PoolSpec {
    pub image: String,
    #[serde(default)]
    pub image_sha256: String,
    pub vcpus: u32,
    pub mem_mib: u64,
    #[serde(default)]
    pub disk_gb: u32,
    pub count: u32,
}

impl PoolSpec {
    /// The sha in matching form: empty string ("trust the local file") is None.
    fn sha_opt(&self) -> Option<&str> {
        if self.image_sha256.is_empty() { None } else { Some(&self.image_sha256) }
    }
}

/// Backoff-map key for one pool spec: every matched dimension, so editing a
/// spec (e.g. a fixed sha) retries immediately instead of inheriting the
/// broken spec's backoff.
fn spec_key(s: &PoolSpec) -> String {
    format!("{}|{}|{}|{}|{}", s.image, s.image_sha256, s.vcpus, s.mem_mib, s.disk_gb)
}

/// The backoff-map key of the pool a pooled VM record belongs to — the same
/// key `spec_key` gives the spec it was prewarmed from.
fn pool_spec_key_of(v: &Vm) -> String {
    spec_key(&PoolSpec {
        image: v.image.clone(),
        image_sha256: v.image_sha256.clone().unwrap_or_default(),
        vcpus: v.vcpus,
        mem_mib: v.mem_mib,
        disk_gb: v.disk_gb,
        count: 0,
    })
}

/// Per-spec refill backoff: true when `key` may not be retried at `now`.
/// Expired entries are removed (the map only ever holds failing specs).
fn backoff_active(map: &mut std::collections::HashMap<String, Instant>, key: &str, now: Instant) -> bool {
    match map.get(key) {
        Some(&next) if next > now => true,
        Some(_) => {
            map.remove(key);
            false
        }
        None => false,
    }
}

/// Wait this long after a failed prewarm before retrying the same spec
/// (a bad sha must not re-download gigabytes every 5s tick).
const REFILL_BACKOFF: Duration = Duration::from_secs(300);

/// Validate one PoolSpec before it can reach paths/curl/ext4 tooling.
/// Bounds mirror the create endpoint's caps; image name shares the create
/// validation (it becomes a path component and a URL segment).
pub fn validate_pool_spec(s: &PoolSpec) -> Result<(), String> {
    if !image::valid_image_name(&s.image) {
        return Err(format!("invalid image name: {:?}", s.image));
    }
    if !s.image_sha256.is_empty() && !image::valid_sha256(&s.image_sha256) {
        return Err(format!("invalid image_sha256 for {}", s.image));
    }
    if !(1..=16).contains(&s.vcpus) {
        return Err(format!("vcpus out of range (1..=16): {}", s.vcpus));
    }
    if !(64..=32768).contains(&s.mem_mib) {
        return Err(format!("mem_mib out of range (64..=32768): {}", s.mem_mib));
    }
    if s.disk_gb > 128 {
        return Err(format!("disk_gb above cap (128): {}", s.disk_gb));
    }
    if s.count > 8 {
        return Err(format!("count above cap (8): {}", s.count));
    }
    Ok(())
}

/// THE pool shape predicate — claim, refill counting and drain all share it.
/// A pooled FC's machine config and rootfs are baked at prewarm time, so a
/// VM matches only on exactly (image, image_sha256, vcpus, mem, disk); a
/// re-captured template (same name, new sha) never matches old pooled VMs.
fn pooled_shape_matches(
    v: &Vm,
    img: &str,
    image_sha256: Option<&str>,
    vcpus: u32,
    mem_mib: u64,
    disk_gb: u32,
) -> bool {
    v.state == VmState::Pooled
        && v.image == img
        && v.image_sha256.as_deref() == image_sha256
        && v.vcpus == vcpus
        && v.mem_mib == mem_mib
        && v.disk_gb == disk_gb
}

/// Index of the first parked pool VM matching the requested shape.
fn find_pooled_match(
    vms: &[Vm],
    img: &str,
    image_sha256: Option<&str>,
    vcpus: u32,
    mem_mib: u64,
    disk_gb: u32,
) -> Option<usize> {
    vms.iter().position(|v| pooled_shape_matches(v, img, image_sha256, vcpus, mem_mib, disk_gb))
}

/// Ids of pooled VMs no current spec wants: shape matches no spec at all, or
/// the VM is in excess of its spec's count (the first `count` matches, in
/// list order, are kept). The refill loop drains these — an empty
/// PUT /v1/pools genuinely tears pools down.
fn surplus_pooled_ids(vms: &[Vm], specs: &[PoolSpec]) -> Vec<String> {
    let mut kept: Vec<u32> = vec![0; specs.len()];
    let mut out = Vec::new();
    for v in vms {
        if v.state != VmState::Pooled {
            continue;
        }
        let mut keep = false;
        for (i, s) in specs.iter().enumerate() {
            if pooled_shape_matches(v, &s.image, s.sha_opt(), s.vcpus, s.mem_mib, s.disk_gb) {
                if kept[i] < s.count {
                    kept[i] += 1;
                    keep = true;
                }
                // First matching spec governs this VM (duplicated shapes
                // count against the earliest spec only).
                break;
            }
        }
        if !keep {
            out.push(v.id.clone());
        }
    }
    out
}

/// Pool VMs the refill loop may drain right now.
///
/// Template pool specs have exactly one delivery channel: feluccad pushes them
/// (PUT /v1/pools) after the node registers. Until that has happened in this
/// process's lifetime, an empty template list means "not told yet", and a
/// pooled VM that matches no known spec may simply belong to a template this
/// agent has not been reminded of. Draining then would kill every template
/// pool VM that survived the restart, seconds before feluccad asks for the
/// same pool again. Only the config pool is known from the start, so only its
/// own excess is drainable before the first push.
fn drainable_pooled_ids(
    vms: &[Vm],
    specs: &[PoolSpec],
    legacy: Option<&PoolSpec>,
    synced: bool,
) -> Vec<String> {
    let surplus = surplus_pooled_ids(vms, specs);
    if synced {
        return surplus;
    }
    let Some(l) = legacy else {
        return Vec::new();
    };
    surplus
        .into_iter()
        .filter(|id| {
            vms.iter().any(|v| {
                &v.id == id
                    && pooled_shape_matches(v, &l.image, l.sha_opt(), l.vcpus, l.mem_mib, l.disk_gb)
            })
        })
        .collect()
}

/// `oom_score_adj` of every Firecracker (-1000..=1000; the agent stays at 0).
///
/// When a worker runs out of memory the guests are what filled it, and the
/// process to lose should be a VMM — one sandbox — rather than the agent,
/// sshd or journald, whose loss costs the whole node. The kernel ranks
/// victims by resident memory plus this value, in thousandths of RAM: 300
/// puts any Firecracker ahead of a daemon using less than about a third of
/// the machine more than it does, while a genuine host-side leak that grows
/// past that is still the one taken.
///
/// Set by the agent per process, not as `OOMScoreAdjust=` on the unit: a
/// unit-level value is inherited by every VMM, and it is a floor an
/// unprivileged process may lower itself back to.
const FC_OOM_SCORE_ADJ: i32 = 300;

/// Mark the process at `pid` as a preferred OOM victim (see
/// `FC_OOM_SCORE_ADJ`). Raising the value needs no privilege.
fn set_oom_score_adj(proc_root: &Path, pid: i32) -> std::io::Result<()> {
    std::fs::write(
        proc_root.join(pid.to_string()).join("oom_score_adj"),
        format!("{}\n", FC_OOM_SCORE_ADJ),
    )
}

/// How long start-up waits for an adopted Firecracker to answer on its API
/// socket. It answers in well under a millisecond when healthy; a VMM that
/// does not is left as recorded rather than held against the whole start.
const ADOPT_API_TIMEOUT: Duration = Duration::from_secs(2);

/// How long to wait for a SIGKILLed Firecracker to be gone. Tearing down a
/// guest with many GiB of touched memory is not instant.
const KILL_WAIT: Duration = Duration::from_secs(10);

/// One step of the start-up orphan sweep (see `plan_sweep`).
#[derive(Debug, Clone, PartialEq, Eq)]
enum SweepStep {
    /// Kill a Firecracker that belongs to no committed VM.
    Kill { pid: i32, dir_name: String },
    /// A Firecracker in the directory of a record that holds none and says the
    /// VM is asleep or stopped: ask it. A guest that is running is adopted, any
    /// other answer (or none) is a `Kill`. See `Manager::adopt_running_vmm`.
    Probe { pid: i32, dir_name: String },
    /// Remove an instance directory that never got a `meta.json`.
    RemoveDir { dir_name: String },
    /// A Firecracker next to a record that does not load: leave it running.
    LeaveUnreadable { pid: i32, dir_name: String },
    /// A Firecracker this unit did not start: not ours to kill.
    LeaveForeign { pid: i32, dir_name: String },
}

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct SweepOutcome {
    killed: usize,
    /// VMMs of a wake or start that had worked but was never written down:
    /// the guest was running, so the record was brought up to it.
    rolled_forward: usize,
    dirs_removed: usize,
    /// Pool VMs removed because their VMM was gone (`remove_dead_pool_vms`).
    pool_removed: usize,
    left_unreadable: usize,
    left_foreign: usize,
}

/// Decide what start-up does with every Firecracker found under this data dir
/// that no record adopted, and with every directory that has no record.
///
/// * `found` — Firecrackers whose API socket is in `instances/<dir>/`.
/// * `owned` — pids that records adopted. Never touched.
/// * `record_dirs` — directories that loaded as a record.
/// * `adoptable` — of those, the ones whose record holds no VMM and says the
///   VM is asleep, stopped or in error: where a wake or start that the previous
///   agent did not finish writing down leaves its Firecracker.
/// * `unreadable` — directories whose `meta.json` exists and does not load.
/// * `metaless` — directories with no `meta.json`.
/// * `mine` — is this pid in the agent's own cgroup?
///
/// The rule that matters most: a process is killed only when it is certain
/// that no caller was ever told its VM exists, or that the record that does
/// exist has moved on without it. A VMM beside a damaged record is very likely
/// a healthy tenant VM, and is left alone. And a VMM beside a record that says
/// "asleep" may be a guest that is already awake and writing to its disk —
/// killing that one would leave the record pointing at a snapshot the disk has
/// since outrun — so it is asked first (`SweepStep::Probe`).
fn plan_sweep(
    found: &[FcProc],
    owned: &std::collections::HashSet<i32>,
    record_dirs: &std::collections::HashSet<String>,
    adoptable: &std::collections::HashSet<String>,
    unreadable: &[String],
    metaless: &[String],
    mine: impl Fn(i32) -> bool,
) -> Vec<SweepStep> {
    let mut steps = Vec::new();
    // Unfinished directories a process we are not killing still lives in.
    let mut occupied: std::collections::HashSet<&str> = std::collections::HashSet::new();
    for p in found {
        if owned.contains(&p.pid) {
            continue;
        }
        let dir = p.dir_name.as_str();
        if unreadable.iter().any(|d| d == dir) {
            steps.push(SweepStep::LeaveUnreadable { pid: p.pid, dir_name: p.dir_name.clone() });
        } else if !mine(p.pid) {
            occupied.insert(dir);
            steps.push(SweepStep::LeaveForeign { pid: p.pid, dir_name: p.dir_name.clone() });
        } else if metaless.iter().any(|d| d == dir) {
            // No meta.json: the create never completed.
            steps.push(SweepStep::Kill { pid: p.pid, dir_name: p.dir_name.clone() });
        } else if adoptable.contains(dir) {
            // The record holds no VMM and says the VM is not running: a wake
            // or start the previous agent did not finish writing down.
            steps.push(SweepStep::Probe { pid: p.pid, dir_name: p.dir_name.clone() });
        } else if record_dirs.contains(dir) {
            // A second VMM on a directory whose record adopted another one.
            steps.push(SweepStep::Kill { pid: p.pid, dir_name: p.dir_name.clone() });
        }
        // Anything else is in a directory this agent does not manage at all.
    }
    for d in metaless {
        if !occupied.contains(d.as_str()) {
            steps.push(SweepStep::RemoveDir { dir_name: d.clone() });
        }
    }
    steps
}

/// What `settle_adopted_run_states` did.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct Settled {
    resumed: usize,
    paused: usize,
    failed: usize,
    /// VMMs that did not answer. They are asked again from the liveness loop
    /// (`Manager::settle_unanswered`) until they do.
    unreachable: usize,
}

/// What became of one adopted VM when its Firecracker was asked.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SettleResult {
    /// The VMM agrees with the record (or the record has moved on).
    AsRecorded,
    Resumed,
    Paused,
    Failed,
}

/// One Firecracker's answer to "what is your guest doing?".
#[derive(Debug, Clone, PartialEq, Eq)]
enum Probe {
    /// `GET /` returned this state.
    Says(String),
    /// No answer in time, or the socket refused.
    Silent,
    /// The record's directory id does not make a path (never asked).
    NoSocketPath,
}

/// A VM whose Firecracker did not answer when start-up asked what its guest is
/// doing — typically one that is still writing the snapshot of a sleep or fork
/// the previous agent started. Its record may say `running` over a guest that
/// is paused, so until the VMM has answered once:
///
/// * the liveness loop keeps asking (`Manager::settle_unanswered`), and
/// * `pause` / `resume` do not trust the record enough to answer "already so".
///
/// The two never talk to the same VMM at once: a lifecycle operation takes a
/// `Pending` entry away before it starts (and then establishes the truth
/// itself), and waits out a `Settling` one.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Unsettled {
    /// Nobody is looking at it. The pid is the VMM that was asked.
    Pending(i32),
    /// `settle_unanswered` is talking to its VMM right now.
    Settling,
}

/// A std mutex that is only ever held for a few instructions and never across
/// an await. A panic while holding one leaves data that is still valid here
/// (a set of ids), so poisoning is not a reason to stop.
fn lock_std<T>(m: &std::sync::Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Ask a Firecracker what its guest is doing. `None` when it does not answer
/// within `ADOPT_API_TIMEOUT` — its API serves one request at a time, so one
/// that is busy writing a multi-GiB snapshot answers when that is done.
async fn probe_vmm(sock: &str) -> Option<String> {
    match tokio::time::timeout(ADOPT_API_TIMEOUT, fc::get_instance_state(sock)).await {
        Ok(Ok(state)) => Some(state),
        Ok(Err(_)) | Err(_) => None,
    }
}

/// Puts an id on the "meta.json is owed" list when it is dropped while still
/// armed: a `write_meta` that failed, or one whose caller went away in the
/// middle of it. See `Manager::flush_unwritten_meta`.
struct OwedMeta<'a> {
    owed: &'a std::sync::Mutex<std::collections::HashSet<String>>,
    id: &'a str,
    armed: bool,
}

impl Drop for OwedMeta<'_> {
    fn drop(&mut self) {
        if self.armed {
            lock_std(self.owed).insert(self.id.to_string());
        }
    }
}

/// Write `bytes` to `dst` so that a crash at any point leaves either the old
/// file or the new one, and so that the new one is really on disk when this
/// returns: temp file, fsync, rename, fsync of the directory.
///
/// The fsyncs are what make an acknowledged operation survive a power cut.
/// Without them a `meta.json` written seconds before can come back empty or
/// not at all, and start-up treats a directory without one as a create that
/// never finished and removes it.
fn replace_file_durably(dir: &str, name: &str, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let tmp = format!("{}/{}.tmp", dir, name);
    let dst = format!("{}/{}", dir, name);
    let mut f = std::fs::File::create(&tmp)?;
    f.write_all(bytes)?;
    f.sync_all()?;
    drop(f);
    std::fs::rename(&tmp, &dst)?;
    std::fs::File::open(dir)?.sync_all()
}

/// Why a lifecycle operation (sleep, wake, start, stop, pause, resume) did not
/// do what it was asked. The handlers map each to a status; `Display` gives
/// the text they have always answered with.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum OpError {
    /// Another lifecycle operation holds this sandbox, or it is being stopped
    /// or deleted (→ 409 `{"error":"busy"}`, no `state`: try again).
    Busy,
    /// The operation does not apply to the state the record is in
    /// (→ 409 `{"error": msg, "state": state}`). `state` is what the record
    /// says at the moment of the refusal; the control plane reads it.
    InvalidState { msg: String, state: VmState },
    /// Everything else, verbatim.
    Other(String),
}

impl OpError {
    fn invalid(state: &VmState, msg: impl Into<String>) -> Self {
        OpError::InvalidState { msg: msg.into(), state: state.clone() }
    }
}

/// What an operation answers when, at its commit point, the sandbox turns
/// out to be stopped or deleted meanwhile, or no longer to hold the VMM the
/// operation was talking to. It has then undone what was its own and
/// written nothing.
fn aborted(id: &str) -> OpError {
    OpError::Other(format!("Aborted: {} was stopped, deleted or lost its VMM while the operation ran", id))
}

impl std::fmt::Display for OpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            OpError::Busy => f.write_str(BUSY),
            OpError::InvalidState { msg, .. } => f.write_str(msg),
            OpError::Other(s) => f.write_str(s),
        }
    }
}

impl From<String> for OpError {
    fn from(s: String) -> Self {
        OpError::Other(s)
    }
}

impl From<&str> for OpError {
    fn from(s: &str) -> Self {
        OpError::Other(s.to_string())
    }
}

/// The error text of a busy sandbox. `delete` and `fork`, which answer with a
/// plain string, use it too.
pub const BUSY: &str = "busy";

/// The refusal of a fork whose parent was itself forked before this release
/// (FLU-72 K5), running, paused or sleeping. No path, no other sandbox: the
/// control plane relays it to the tenant word for word; the log has both.
pub const FORK_REFUSED_FORKED_BEFORE: &str = "InvalidState: this sandbox was forked before this release and runs on \
    its parent's disk image, not on its own; it cannot be forked";

/// The refusal of a wake of such a sandbox whose parent's image moved on
/// (FLU-72 K6). Same reasoning.
pub const WAKE_REFUSED_FORKED_BEFORE: &str = "InvalidState: this sandbox was forked before this release and runs on \
    its parent's disk image, which has changed since it slept; it cannot be woken and is now stopped (use start)";

/// What stop and delete answer when the operation they pre-empted has not
/// let go within `CLOSE_WAIT` (409, with `"state":"stopped"`: stop has
/// written that record). The caller retries.
pub const STOP_GAVE_UP: &str = "busy: the VM is stopped, but an operation in progress has not let go of it yet; try again";

/// The lifecycle operations that hold a sandbox while they run (FLU-4). At
/// most one is in progress per sandbox: each of them reads the record, talks
/// to the VMM and writes the record back, and two of them interleaved on one
/// sandbox — a wake spawning a VMM while a sleep is killing one, a start
/// cold-booting over a snapshot a sleep is still writing — leave a record and
/// files that describe neither.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum OpKind {
    Sleep,
    Wake,
    /// On the parent, until its snapshot files have been written or read.
    Fork,
    Start,
    Pause,
    Resume,
}

/// What holds one sandbox right now. An entry exists only while something
/// does.
#[derive(Debug, Default)]
struct BusyMark {
    op: Option<OpKind>,
    /// Forks of this sandbox while it is asleep (S4'). They only read its
    /// snapshot files and its disk, so any number of them run side by side;
    /// to everything else they are an operation in progress.
    readers: usize,
    /// Stops and deletes in progress (delete stops first, so two can nest).
    /// While any runs, no operation starts and none commits.
    closing: usize,
}

impl BusyMark {
    fn idle(&self) -> bool {
        self.op.is_none() && self.readers == 0 && self.closing == 0
    }
}

type BusyMap = std::sync::Mutex<std::collections::HashMap<String, BusyMark>>;

/// The longest a second request of the same kind waits for the one in
/// progress (S4'); then it answers 409 `busy`. Within it, it answers from
/// the state the first one left: a retried sleep finds the sandbox asleep and
/// says so, instead of a 409 its caller would have to interpret. Below the
/// control plane's 30 s worker-call timeout: a waiter that outlived its
/// caller would carry the operation out for nobody (it used to wait 120 s).
const SAME_OP_LIMIT: Duration = Duration::from_secs(25);
const SAME_OP_WAIT: Duration = if cfg!(test) { Duration::from_secs(3) } else { SAME_OP_LIMIT };

/// How long stop and delete wait for an operation in progress to let go
/// after they have ended its VMM. There is no timeout on a Firecracker API
/// call, so they cannot wait for ever: a hung call would make the sandbox
/// impossible to stop or delete. Shorter under test, so the tests of the
/// limit itself do not take 15 s each.
const CLOSE_WAIT: Duration = if cfg!(test) { Duration::from_secs(2) } else { Duration::from_secs(15) };

/// How often a waiting request looks again.
const BUSY_POLL: Duration = Duration::from_millis(20);

/// Holds a sandbox for one lifecycle operation; letting go is in `Drop`, so
/// every exit path releases it — an early return, a `?`, a panic, and the
/// caller of the future going away. (`inner` is a tokio mutex and cannot be
/// taken in `Drop`, hence the std map.)
struct OpGuard<'a> {
    busy: &'a BusyMap,
    id: String,
    /// One of the `readers` of a sleeping parent, not its `op`.
    shared: bool,
}

impl OpGuard<'_> {
    /// Turn a fork's hold on its parent into a shared one: the parent is
    /// asleep, so other forks of it may read its files alongside this one.
    /// Called inside the `inner` section that read the parent's state, which
    /// cannot change while the parent is held.
    fn share(&mut self) {
        let mut map = lock_std(self.busy);
        if let Some(m) = map.get_mut(&self.id) {
            if !self.shared && m.op == Some(OpKind::Fork) {
                m.op = None;
                m.readers += 1;
                self.shared = true;
            }
        }
    }
}

impl Drop for OpGuard<'_> {
    fn drop(&mut self) {
        let mut map = lock_std(self.busy);
        if let Some(m) = map.get_mut(&self.id) {
            if self.shared {
                m.readers = m.readers.saturating_sub(1);
            } else {
                m.op = None;
            }
            if m.idle() {
                map.remove(&self.id);
            }
        }
    }
}

/// Marks a sandbox as being stopped or deleted for as long as it lives.
struct CloseGuard<'a> {
    busy: &'a BusyMap,
    id: String,
}

impl Drop for CloseGuard<'_> {
    fn drop(&mut self) {
        let mut map = lock_std(self.busy);
        if let Some(m) = map.get_mut(&self.id) {
            m.closing = m.closing.saturating_sub(1);
            if m.idle() {
                map.remove(&self.id);
            }
        }
    }
}

/// File names of a sleeping sandbox's snapshot, in its instance directory.
const VMSTATE: &str = "vmstate.bin";
const VMSTATE_TMP: &str = "vmstate.bin.tmp";
const MEM: &str = "mem.bin";
/// The suffix of a snapshot file set aside as stale (S3', `set_snapshot_aside`).
const OUTRUN: &str = ".outrun";

/// Are `a` and `b` the same file (device and inode)?
fn same_file(a: &str, b: &str) -> std::io::Result<bool> {
    use std::os::unix::fs::MetadataExt;
    let (a, b) = (std::fs::metadata(a)?, std::fs::metadata(b)?);
    Ok((a.dev(), a.ino()) == (b.dev(), b.ino()))
}

/// Was `image` modified after `vmstate` was written (strictly later, at the
/// file system's own precision)? The guest of that snapshot ran since (S3).
fn modified_after(image: &str, vmstate: &str) -> std::io::Result<bool> {
    let disk = std::fs::metadata(image)?.modified()?;
    let snapshot = std::fs::metadata(vmstate)?.modified()?;
    Ok(disk > snapshot)
}

/// What looking at one file found. "Could not look" is its own answer: a
/// failed `stat` is not a missing file, and acting on it as one would discard
/// a good snapshot.
#[derive(Debug, Clone, PartialEq, Eq)]
enum FileCheck {
    /// There, with this many bytes.
    There(u64),
    Missing,
    Unknown(String),
}

fn check_file(path: &str) -> FileCheck {
    match std::fs::metadata(path) {
        Ok(m) => FileCheck::There(m.len()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => FileCheck::Missing,
        Err(e) => FileCheck::Unknown(e.to_string()),
    }
}

/// Remove one file that may or may not be there. Returns false only when it
/// is there and could not be removed (logged).
fn remove_if_present(id: &str, path: &str) -> bool {
    match std::fs::remove_file(path) {
        Ok(()) => {
            info!(vm = %id, file = %path, "removed a snapshot file that can no longer be used");
            true
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => true,
        Err(e) => {
            warn!(vm = %id, file = %path, err = %e, "could not remove a snapshot file");
            false
        }
    }
}

/// (major, minor) of a `st_dev`, in the encoding glibc and the kernel's
/// `new_encode_dev` use — the one `/proc/<pid>/maps` prints as `MM:mm` hex.
fn dev_major_minor(dev: u64) -> (u64, u64) {
    let major = ((dev >> 32) & 0xffff_f000) | ((dev >> 8) & 0x0fff);
    let minor = ((dev >> 12) & 0xffff_ff00) | (dev & 0x00ff);
    (major, minor)
}

/// Does this `/proc/<pid>/maps` text map the file with device `major:minor`
/// and inode `ino`? `None` when a line does not parse: "cannot tell".
///
/// A line is `start-end perms offset MM:mm inode [path]`. Only the device and
/// the inode are read — the path may contain spaces or end in " (deleted)",
/// and a mapping of a file that was replaced since is not this file.
fn maps_contain(maps: &str, major: u64, minor: u64, ino: u64) -> Option<bool> {
    for line in maps.lines() {
        if line.trim().is_empty() {
            continue;
        }
        let mut fields = line.split_whitespace();
        let dev = fields.nth(3)?;
        let inode = fields.next()?.parse::<u64>().ok()?;
        let (ma, mi) = dev.split_once(':')?;
        let ma = u64::from_str_radix(ma, 16).ok()?;
        let mi = u64::from_str_radix(mi, 16).ok()?;
        if (ma, mi, inode) == (major, minor, ino) {
            return Some(true);
        }
    }
    Some(false)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SettleAction {
    None,
    Resume,
    Pause,
    /// The VMM is up but never booted a guest: there is no VM to adopt.
    Discard,
}

/// What to do with an adopted VM whose record says `state` while its
/// Firecracker reports `vmm` (the `state` of `GET /`: "Not started",
/// "Running" or "Paused"). A value this agent does not know is left alone.
fn settle_action(state: &VmState, vmm: &str) -> SettleAction {
    match (state, vmm) {
        (_, "Not started") => SettleAction::Discard,
        (VmState::Running, "Paused") => SettleAction::Resume,
        (VmState::Paused | VmState::Pooled, "Running") => SettleAction::Pause,
        _ => SettleAction::None,
    }
}

/// Errors from the rootfs capture lookup; the server handler maps each to a status.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RootfsError {
    /// Unknown VM id (→ 404).
    NotFound,
    /// VM exists but is not Stopped (→ 409 {"error":"not stopped"}).
    NotStopped,
    /// Another capture is already streaming this VM's rootfs
    /// (→ 409 {"error":"capture in progress"}).
    CaptureInProgress,
}

/// Errors from expose/unexpose; the server handler maps each to a status.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ExposeError {
    /// Unknown VM id (→ 404).
    NotFound,
    /// VM holds no IP, nothing to DNAT to (→ 409).
    NoIp,
    /// Every node port in NODE_PORT_MIN..=NODE_PORT_MAX is taken or to be
    /// avoided (→ 409).
    Exhausted,
    /// A new mapping's DNAT rule could not be installed; the mapping was
    /// taken back out of the record (→ 500).
    RuleFailed,
}

/// Worker-side ingress node-port range (inclusive).
const NODE_PORT_MIN: u16 = 20000;
const NODE_PORT_MAX: u16 = 29999;

/// Choose the node port for a new mapping (FLU-6, R3 and R4). Returns the
/// port and where the cursor goes next, or None when nothing is left.
///
/// `in_use` spans every expose of every VM on this worker; `avoid` is what
/// the control plane says must not be handed out (the ports it has recorded
/// for this worker, and the ones released less than the quarantine ago).
///
/// `prefer` (the port this same sandbox and guest port held last) is
/// returned whenever it is in the range and no sandbox here holds it, even
/// though it is in `avoid`: it is in `avoid` BECAUSE it is quarantined for
/// this mapping, and getting it back is what makes a sleep and wake release
/// nothing. It leaves the cursor where it is.
///
/// Otherwise round-robin: the first port at or after `cursor` that is neither
/// in use nor avoided, wrapping once around the range; the cursor moves to the
/// port after it. Lowest-free, which this replaces, handed a port that had
/// just been released to the very next expose — another tenant's — while the
/// gateways still routed the old hostname to it. Pure for unit tests.
fn pick_node_port(
    in_use: &std::collections::HashSet<u16>,
    avoid: &std::collections::HashSet<u16>,
    prefer: Option<u16>,
    cursor: u16,
    range: std::ops::RangeInclusive<u16>,
) -> Option<(u16, u16)> {
    if range.is_empty() {
        return None;
    }
    let (min, max) = (*range.start(), *range.end());
    if let Some(p) = prefer {
        if range.contains(&p) && !in_use.contains(&p) {
            return Some((p, cursor));
        }
    }
    let span = u32::from(max - min) + 1;
    let start = if range.contains(&cursor) { cursor } else { min };
    (0..span)
        .map(|i| (u32::from(min) + (u32::from(start - min) + i) % span) as u16)
        .find(|p| !in_use.contains(p) && !avoid.contains(p))
        .map(|p| (p, if p == max { min } else { p + 1 }))
}

/// Where the node-port cursor starts when the agent starts: a random port of
/// the range, so that an agent restart does not begin handing out the same
/// ports again from one fixed place. Nothing about the allocator is kept on
/// disk: the ports in use come back from the records, and the control
/// plane's `avoid` covers the ones released before the restart.
fn random_port_in(range: std::ops::RangeInclusive<u16>) -> u16 {
    use std::io::Read;
    let mut b = [0u8; 8];
    let seed = match std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut b)) {
        Ok(()) => u64::from_ne_bytes(b),
        // Not a secret, only a spread: the clock and the pid will do.
        Err(_) => {
            let nanos = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_nanos() as u64)
                .unwrap_or(0);
            nanos ^ (u64::from(std::process::id()) << 32)
        }
    };
    port_from_seed(seed, range)
}

/// `seed` mapped onto `range` (non-empty).
fn port_from_seed(seed: u64, range: std::ops::RangeInclusive<u16>) -> u16 {
    let (min, max) = (*range.start(), *range.end());
    if min >= max {
        return min;
    }
    min + (seed % (u64::from(max - min) + 1)) as u16
}

/// Map an images-dir entry name to the cache image it belongs to, or None for
/// files the image cache did not create (which the GC must never touch).
/// Cache files per pull_image/ensure_image: `<image>.ext4`, its
/// `<image>.ext4.sha256` sidecar, and the in-flight `.<image>.partial`.
fn gc_image_for_entry(fname: &str) -> Option<String> {
    let image = if let Some(stem) = fname.strip_suffix(".ext4.sha256") {
        stem
    } else if let Some(stem) = fname.strip_suffix(".ext4") {
        stem
    } else if let Some(stem) = fname
        .strip_prefix('.')
        .and_then(|r| r.strip_suffix(".partial"))
    {
        stem
    } else {
        return None;
    };
    if image::valid_image_name(image) {
        Some(image.to_string())
    } else {
        None
    }
}

/// Result of a guest exec attempt; the server handler maps each to a status.
pub enum ExecOutcome {
    /// Guest replied — carries the response JSON verbatim (→ 200).
    Ok(serde_json::Value),
    /// Unknown VM id (→ 500 {"error":"NotFound"}, consistent with other handlers).
    NotFound,
    /// VM exists but is not running (→ 409 {"error":"not running"}).
    NotRunning,
    /// VM has no vsock device — pre-v3 or marker absent (→ 501).
    Unavailable,
    /// Connect/handshake/round-trip failed (→ 501 {"error":"guest agent unavailable"}).
    Failed(String),
}

/// Result of opening a streamed exec (v5 P5.1). Setup failures share
/// `ExecOutcome`'s status mapping; success carries the live frame stream.
pub enum ExecStreamOutcome {
    /// Handshake + request sent — the stream yields the guest's NDJSON frames.
    /// The `GuestSlot` is this exec's round-trip permit and must be held for
    /// as long as the stream is read.
    Ok(tokio::net::UnixStream, crate::guestclient::GuestSlot),
    NotFound,
    NotRunning,
    Unavailable,
    Failed(String),
}

struct Inner {
    vms: Vec<Vm>,
    allocator: Option<Allocator>,
    /// v4 P4: feluccad-managed warm-pool templates (set via PUT /v1/pools or
    /// the register response). The legacy config pool (pool_target) is NOT in
    /// this list — it is a separate always-present default spec.
    template_pools: Vec<PoolSpec>,
    /// Whether `template_pools` has been set in THIS process's lifetime.
    /// feluccad pushes the specs after the node registers; until then an empty
    /// list means "not told yet", not "no pools". See `drainable_pooled_ids`.
    pools_synced: bool,
    /// VM ids whose rootfs is being streamed to a capture right now: start()
    /// must not spawn an FC over the file mid-stream. Entries are removed by
    /// the capture stream's drop guard (covers client disconnects too).
    capturing: std::collections::HashSet<String>,
    /// FLU-6 R4: where the next node-port search starts (`pick_node_port`).
    /// In memory only; a random port of the range at start-up.
    node_port_cursor: u16,
    /// Slots of deleted sandboxes whose rule rebuilds failed: reserved in
    /// the allocator, released by `settle_slots` (FLU-8, A).
    held_slots: std::collections::BTreeSet<u32>,
    /// No slot is handed out (FLU-8, B): see `start_ingress`.
    slots_withheld: bool,
}

pub struct Manager {
    data_dir: String,
    net_on: bool,
    cidr: Cidr,
    pool_target: u32,
    /// v4 P4: control-plane base URL + bearer token for image pulls.
    control_plane: String,
    token: String,
    inner: Mutex<Inner>,
    // Serializes nft ruleset rebuilds (isolation + ingress): never held
    // together with `inner`.
    ruleset_lock: Mutex<()>,
    // Per-image download locks (downloads of ONE image must not race on its
    // cache files, but a multi-GB pull of one image must never block creates
    // whose image is already cached). The map mutex is held only to fetch an
    // entry; the per-image mutex is NEVER held together with `inner`.
    image_locks: Mutex<std::collections::HashMap<String, Arc<Mutex<()>>>>,
    // Per-spec prewarm failure backoff (REFILL_BACKOFF): a broken spec must
    // not re-attempt (and possibly re-download gigabytes) every 5s tick.
    pool_backoff: Mutex<std::collections::HashMap<String, Instant>>,
    // Serializes meta.json writes. Each one snapshots the record and replaces
    // the file as a single step, so two writers for one VM can neither
    // interleave inside the temp file nor let the staler snapshot land last.
    // Taken BEFORE `inner`, never while holding it.
    meta_write_lock: Mutex<()>,
    // Exclusive lock on `{data_dir}/agent.lock`: taken once at start-up and
    // held (through this open file) for the life of the process. `None` once
    // tried and refused.
    data_dir_lock: std::sync::OnceLock<Option<std::fs::File>>,
    // VMs whose Firecracker has not yet said what its guest is doing (see
    // `Unsettled`). Keyed by VM id.
    unsettled: std::sync::Mutex<std::collections::HashMap<String, Unsettled>>,
    // Ids whose meta.json is behind the in-memory record: a write failed or was
    // interrupted. The liveness loop writes them (`flush_unwritten_meta`).
    meta_owed: std::sync::Mutex<std::collections::HashSet<String>>,
    // Sandboxes a lifecycle operation, a stop or a delete holds right now
    // (FLU-4, see `OpKind`). Keyed by VM id. Taken while holding `inner` at
    // a commit point, never the other way round, and never across an await.
    busy: BusyMap,
    // procfs root — always /proc outside unit tests.
    proc_root: std::path::PathBuf,
    // The Firecracker binary — always `firecracker` (from PATH) outside unit
    // tests, which start a stand-in.
    fc_bin: String,
    // The rule rebuilds and delete's tap removal — always `HostRules` (nft
    // and ip) outside unit tests, which put a stand-in here.
    rules: Arc<dyn RuleOps>,
    // FLU-8: per chain, the hash last applied (or that its last rebuild
    // failed), and what the isolation watch last saw. Written only while
    // holding `ruleset_lock`; read by `isolation_ok` at any time. A std
    // mutex: never held across an await, and never while taking `inner`.
    watch: std::sync::Mutex<watch::Record>,
}

/// The host side of the rule rebuilds, of removing a sandbox's tap, and of
/// everything the isolation watch looks at and repairs (`kernel`).
trait RuleOps: Send + Sync {
    /// Whether these touch anything at all on a node with the network
    /// `net_on`.
    fn active(&self, net_on: bool) -> bool;
    /// What the transaction did, and whether isolation is in force.
    fn rebuild_isolation(&self, members: &[(String, String)]) -> (net::Applied, bool);
    fn rebuild_ingress(&self, members: &[(u16, String, u16)]) -> net::Applied;
    fn remove_tap(&self, tap: &str);
    /// Create `tap` if needed and pin it to `ip` and `mac`; false (and the
    /// tap removed) when it cannot be pinned. A fork's child.
    fn pin_tap(&self, tap: &str, ip: Option<&str>, mac: &net::GuestMac) -> bool;
    /// Pin the tap of a guest adopted at start-up; a port that cannot be
    /// pinned is taken down, not removed (`Unpinned::TakeDown`).
    fn pin_adopted_tap(&self, tap: &str, ip: &str, mac: &net::GuestMac) -> Result<(), PinFailure>;
    /// Create the guest bridge, its address, forwarding, IPv6 off on it:
    /// (the bridge is usable, IPv6 is off on it).
    fn ensure_bridge(&self, cidr: Cidr) -> (bool, bool);
    /// Record start-up's verdict on the host-side fences.
    fn note_host_fences(&self, ok: bool);
    /// Start-up's and the last rule change's verdict (`net::netfilter_ready`).
    fn fences_ready(&self) -> bool;
    /// Rulesets, listings, switches, links and the route.
    fn kernel(&self) -> &dyn net::Kernel;
}

/// `RuleOps` on the host: nft and ip.
struct HostRules;

impl RuleOps for HostRules {
    fn active(&self, net_on: bool) -> bool {
        net_on
    }
    fn ensure_bridge(&self, cidr: Cidr) -> (bool, bool) {
        net::ensure_bridge(cidr)
    }
    fn note_host_fences(&self, ok: bool) {
        net::set_host_filter_ok(ok);
    }
    fn fences_ready(&self) -> bool {
        net::netfilter_ready()
    }
    fn kernel(&self) -> &dyn net::Kernel {
        &net::HostKernel
    }
    fn rebuild_isolation(&self, members: &[(String, String)]) -> (net::Applied, bool) {
        net::rebuild_isolation(members)
    }
    fn rebuild_ingress(&self, members: &[(u16, String, u16)]) -> net::Applied {
        net::rebuild_ingress(members)
    }
    fn remove_tap(&self, tap: &str) {
        // Before the tap goes: the pin is keyed on the device name, which
        // the next VM to claim this slot will reuse.
        net::clear_tap_antispoof(tap);
        net::delete_tap(tap);
    }
    fn pin_tap(&self, tap: &str, ip: Option<&str>, mac: &net::GuestMac) -> bool {
        ensure_pinned_tap(tap, ip, mac, Unpinned::Remove).is_ok()
    }
    fn pin_adopted_tap(&self, tap: &str, ip: &str, mac: &net::GuestMac) -> Result<(), PinFailure> {
        ensure_pinned_tap(tap, Some(ip), mac, Unpinned::TakeDown)
    }
}

impl Manager {
    pub fn new(
        data_dir: String,
        net_on: bool,
        cidr: Cidr,
        pool_target: u32,
        control_plane: String,
        token: String,
    ) -> Arc<Self> {
        Self::with_proc_root(data_dir, net_on, cidr, pool_target, control_plane, token, "/proc".into())
    }

    /// `new`, reading the process table from `proc_root` instead of /proc.
    fn with_proc_root(
        data_dir: String,
        net_on: bool,
        cidr: Cidr,
        pool_target: u32,
        control_plane: String,
        token: String,
        proc_root: std::path::PathBuf,
    ) -> Arc<Self> {
        Arc::new(Manager {
            data_dir,
            net_on,
            cidr,
            pool_target,
            control_plane,
            token,
            inner: Mutex::new(Inner {
                vms: Vec::new(),
                allocator: None,
                template_pools: Vec::new(),
                pools_synced: false,
                capturing: std::collections::HashSet::new(),
                node_port_cursor: random_port_in(NODE_PORT_MIN..=NODE_PORT_MAX),
                held_slots: std::collections::BTreeSet::new(),
                slots_withheld: false,
            }),
            ruleset_lock: Mutex::new(()),
            image_locks: Mutex::new(std::collections::HashMap::new()),
            pool_backoff: Mutex::new(std::collections::HashMap::new()),
            meta_write_lock: Mutex::new(()),
            data_dir_lock: std::sync::OnceLock::new(),
            unsettled: std::sync::Mutex::new(std::collections::HashMap::new()),
            meta_owed: std::sync::Mutex::new(std::collections::HashSet::new()),
            busy: std::sync::Mutex::new(std::collections::HashMap::new()),
            proc_root,
            fc_bin: "firecracker".into(),
            rules: Arc::new(HostRules),
            watch: std::sync::Mutex::new(watch::Record::default()),
        })
    }

    /// Re-pin the taps of guests adopted at start-up (`reconcile` step 3).
    /// One that cannot be pinned is taken down, not removed, and handed to
    /// the watch (W8), which pins and raises it on a later tick. Says what
    /// happened (X12): a tap that could not even be set up was not taken
    /// down. Returns how many did not come out pinned.
    fn repin_adopted(&self, taps: &[(String, u32, String)]) -> usize {
        let mut not_pinned = 0;
        for (id, slot, ip) in taps {
            let (tap, mac) = tap_pin_for_slot(*slot);
            match self.rules.pin_adopted_tap(&tap, ip, &mac) {
                Ok(()) => {}
                Err(why) => {
                    not_pinned += 1;
                    lock_std(&self.watch).hold_tap_down(&tap);
                    let what = match why {
                        PinFailure::NotSetUp => "adopted VM's tap could not be set up (created, put on the guest bridge, \
                                                 IPv6 off) — it may not be on the bridge at all; nothing was taken down; \
                                                 the isolation watch pins it once it is a port of the bridge",
                        PinFailure::Unpinned => "adopted VM's tap could not be pinned — the port has been taken down \
                                                 (not removed), so this guest has no network until the isolation watch \
                                                 (or an agent start) pins it",
                        PinFailure::NotRaised => "adopted VM's tap was pinned but could not be brought up — this guest \
                                                  has no network until the isolation watch (or an agent start) raises it",
                    };
                    error!(vm = %id, tap = %tap, "{}", what);
                }
            }
        }
        not_pinned
    }

    /// The process table, scoped to this agent's data dir.
    fn procs(&self) -> ProcTable {
        ProcTable::at(&self.proc_root, &self.data_dir)
    }

    /// Bring up bridge/NAT (if net on) and init the IP allocator. Call once before serving.
    ///
    /// In order: the blackhole route for the guest network (FLU-8 N6': before
    /// anything that may take the bridge down, so guest addresses are dropped
    /// here rather than sent out by the default route whenever the bridge
    /// and its connected route are gone), the bridge, then NAT and the two
    /// host-side fences, one transaction each, each remembered with its
    /// marker for the isolation watch. Off the runtime threads, under the
    /// rule lock.
    pub async fn setup_host(&self) {
        if self.net_on {
            let alloc = Allocator::new(self.cidr);
            let mut g = self.inner.lock().await;
            g.allocator = Some(alloc);
        }
        if !self.rules_on() { return; }
        let _rules = self.ruleset_lock.lock().await;
        let rules = Arc::clone(&self.rules);
        let cidr = self.cidr;
        let built = tokio::task::spawn_blocking(move || {
            // W9: `unreachable`, re-asserted by the watch whenever it holds the
            // bridge down; a failure is an error (`watch::ensure_guest_route`).
            watch::ensure_guest_route(rules.kernel(), cidr, None);
            let (usable, v6_ok) = rules.ensure_bridge(cidr);
            if !usable {
                warn!("bridge setup failed; guests may lack networking");
            }
            // Each is evaluated, none short-circuited — a partial failure
            // must still install everything else it can.
            let mut built = Vec::new();
            for chain in [net::Chain::Nat, net::Chain::Input, net::Chain::L2] {
                let Some(rs) = net::host_chain_ruleset(&chain, cidr) else { continue };
                let outcome = rules.kernel().apply(&rs);
                net::report_built(&chain, &outcome);
                built.push((chain, outcome.ok().then(|| net::marker_of(&rs)).flatten()));
            }
            let fences_ok = built.iter().filter(|(c, _)| c.fences()).all(|(_, h)| h.is_some());
            rules.note_host_fences(fences_ok && v6_ok);
            built
        })
        .await
        .unwrap_or_default();
        let mut w = lock_std(&self.watch);
        for (chain, hash) in built {
            w.built(chain, hash);
        }
    }

    /// Rebuild state from `data_dir/instances/*` and take over whatever the
    /// previous agent left running.
    ///
    /// Firecrackers outlive the agent (ADR-0011), so on an ordinary restart
    /// every VM on the node is still up when this runs. In order:
    ///
    /// 1. Load the records. A record keeps its VMM only if the recorded pid is
    ///    positively that instance's Firecracker (`procscan`).
    /// 2. Sweep what belongs to no committed VM: the Firecracker of a create
    ///    that never finished, directories that never got a `meta.json`, and
    ///    pool VMs whose VMM is gone. The Firecracker of a wake or start that
    ///    was never written down is asked first: a guest that is already
    ///    running is adopted, anything else is killed.
    /// 3. Re-pin the tap of every adopted guest. The pin is host state, not VM
    ///    state: the netdev chains are gone after a host reboot, and an agent
    ///    upgraded in place adopts guests that were started before the pin
    ///    existed. Without this those ports stay on the bridge unfiltered for
    ///    the whole life of the sandbox — the one window an attacker does not
    ///    have to trigger, they only have to outlive.
    /// 4. Make each adopted VMM's run state match its record. Sleep and fork
    ///    pause the guest first, so an agent that stopped in the middle of one
    ///    leaves a VM paused under a record that says running.
    ///
    /// All of it runs before any background loop or listener exists, so
    /// nothing here can race a create.
    ///
    /// A tap that cannot be pinned is taken DOWN, not removed. Dropping its
    /// traffic is the deliberate direction (a guest whose NIC carries a MAC a
    /// pre-pin agent let Firecracker choose loses its network rather than
    /// keeping an unfiltered port). But the guest behind it is alive: deleting
    /// the device leaves Firecracker holding a dead fd, and nothing short of a
    /// cold boot gives that guest its network back. Set down, the next agent
    /// start brings the port up again as soon as the pin holds.
    pub async fn reconcile(&self) {
        let procs = self.procs();
        let (unreadable, metaless, dead_pool, rewrite, undecided, outrun, adopted) = {
            let mut g = self.inner.lock().await;
            let rec = reconcile::reconcile(&self.data_dir, g.allocator.as_mut(), &procs);
            g.vms = rec.vms;
            let adopted = g.vms.iter().filter(|v| v.pid.is_some()).count();
            (rec.unreadable, rec.metaless, rec.dead_pool, rec.rewrite, rec.undecided, rec.outrun, adopted)
        };

        let mut swept = self.sweep_orphans(&procs, &unreadable, &metaless, &dead_pool).await;
        swept.pool_removed = self.remove_dead_pool_vms(&procs, &dead_pool).await;
        // After the sweep: whether a VMM was adopted for a sandbox decides
        // what of its snapshot may go.
        self.settle_snapshot_files_at_start(&rewrite, &undecided, &outrun).await;
        // Counted after the sweep: it may have adopted a VMM itself.
        let adopted = adopted + swept.rolled_forward;

        let mut taps_down = 0usize;
        if self.net_on {
            let taps = {
                let g = self.inner.lock().await;
                taps_to_repin(&g.vms)
            };
            // Outside the state lock: these shell out to ip/nft.
            taps_down = self.repin_adopted(&taps);
        }

        // A VMM started by an agent that predates the OOM marking is still
        // at 0; every adopted one gets it (again). Its identity was checked a
        // moment ago, when its record adopted it.
        let adopted_pids: Vec<(String, i32)> = {
            let g = self.inner.lock().await;
            g.vms.iter().filter_map(|v| v.pid.map(|p| (v.id.clone(), p))).collect()
        };
        for (id, pid) in adopted_pids {
            if let Err(e) = set_oom_score_adj(&self.proc_root, pid) {
                warn!(vm = %id, pid, err = %e, "could not set oom_score_adj on an adopted firecracker");
            }
        }

        let settled = self.settle_adopted_run_states().await;

        info!(
            adopted,
            resumed = settled.resumed,
            paused = settled.paused,
            failed = settled.failed,
            unreachable = settled.unreachable,
            rolled_forward = swept.rolled_forward,
            orphans_killed = swept.killed,
            dirs_removed = swept.dirs_removed,
            dead_pool_removed = swept.pool_removed,
            left_unreadable = swept.left_unreadable,
            left_foreign = swept.left_foreign,
            taps_down,
            "reconcile: took over what the previous agent left"
        );
    }

    /// Take the exclusive lock on `{data_dir}/agent.lock` (once; it is then
    /// held for the life of the process). Whoever holds it is the one agent
    /// that manages this data dir.
    ///
    /// `main` takes it before anything else at start-up and exits without it.
    /// A second agent started by hand against the same data dir would
    /// otherwise run the whole of start-up against the first agent's live
    /// VMs, from records that are already stale by the time it acts on them:
    /// kill its in-flight creates (they have no `meta.json` yet), pause a pool
    /// VM it handed to a tenant a moment ago, rebuild the isolation and
    /// ingress rules without the VMs created since — and only then fail to
    /// bind. The sweep and the pool clean-up check it again on their own, so
    /// nothing that kills or removes runs without it.
    pub fn hold_data_dir_lock(&self) -> bool {
        self.data_dir_lock
            .get_or_init(|| {
                use std::os::unix::io::AsRawFd;
                // A data dir that does not exist yet is created by the first
                // VM; the lock file must not be what fails on a fresh node.
                let _ = std::fs::create_dir_all(&self.data_dir);
                let path = format!("{}/agent.lock", self.data_dir);
                // std opens with O_CLOEXEC, so no Firecracker inherits the
                // descriptor: the lock dies with the agent and nothing else.
                let f = match std::fs::OpenOptions::new()
                    .create(true)
                    .truncate(false)
                    .write(true)
                    .open(&path)
                {
                    Ok(f) => f,
                    Err(e) => {
                        error!(path = %path, err = %e, "cannot open the data-dir lock file");
                        return None;
                    }
                };
                // A few attempts, not one. The lock belongs to the open file
                // description, and a child the previous agent was in the
                // middle of spawning holds a copy of it for the instant
                // between its fork and its exec — on a fast restart that can
                // be this very moment. A second agent that is really running
                // still holds it after 200ms.
                for attempt in 0..10 {
                    if attempt > 0 {
                        std::thread::sleep(std::time::Duration::from_millis(20));
                    }
                    // SAFETY: flock on a descriptor this function owns.
                    let rc = unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
                    if rc == 0 {
                        return Some(f);
                    }
                }
                None
            })
            .is_some()
    }

    /// Step 2 of `reconcile`: kill Firecrackers that belong to no committed VM
    /// and remove the directories of creates that never finished.
    async fn sweep_orphans(
        &self,
        procs: &ProcTable,
        unreadable: &[String],
        metaless: &[String],
        dead_pool: &[String],
    ) -> SweepOutcome {
        let mut out = SweepOutcome::default();
        if !self.hold_data_dir_lock() {
            error!(
                data_dir = %self.data_dir,
                "could not take the data-dir lock (is another felucca-agent running on it?) — \
                 not sweeping orphan Firecrackers or unfinished instance directories on this start"
            );
            return out;
        }
        let (owned, record_dirs, adoptable) = {
            let g = self.inner.lock().await;
            let owned: std::collections::HashSet<i32> = g.vms.iter().filter_map(|v| v.pid).collect();
            let dirs: std::collections::HashSet<String> =
                g.vms.iter().map(|v| v.dir_id.clone()).collect();
            // Pool VMs that lost their VMM are on their way out
            // (`remove_dead_pool_vms`); nothing is brought back for them.
            let adoptable: std::collections::HashSet<String> = g
                .vms
                .iter()
                .filter(|v| Self::may_adopt_running_vmm(v) && !dead_pool.contains(&v.id))
                .map(|v| v.dir_id.clone())
                .collect();
            (owned, dirs, adoptable)
        };
        let found = procs.scan();
        let plan = plan_sweep(&found, &owned, &record_dirs, &adoptable, unreadable, metaless, |pid| {
            procs.in_my_cgroup(pid)
        });
        for step in plan {
            match step {
                SweepStep::Kill { pid, dir_name } => {
                    warn!(
                        pid,
                        dir = %dir_name,
                        "reconcile: killing a Firecracker that belongs to no committed VM \
                         (a create the previous agent did not finish, or a second VMM in a VM's directory)"
                    );
                    self.kill_fc_and_wait(pid, &dir_name).await;
                    out.killed += 1;
                }
                SweepStep::Probe { pid, dir_name } => {
                    if self.adopt_running_vmm(pid, &dir_name).await {
                        out.rolled_forward += 1;
                    } else {
                        warn!(
                            pid,
                            dir = %dir_name,
                            "reconcile: killing the Firecracker of a wake or start the previous agent \
                             did not finish (its guest is not running, so the state on disk is still current)"
                        );
                        self.kill_fc_and_wait(pid, &dir_name).await;
                        out.killed += 1;
                    }
                }
                SweepStep::RemoveDir { dir_name } => {
                    let Some(path) = instance_path(&self.data_dir, &dir_name) else {
                        continue;
                    };
                    match std::fs::remove_dir_all(&path) {
                        Ok(()) => {
                            info!(dir = %dir_name, "reconcile: removed an instance directory that never got a meta.json (unfinished create)");
                            out.dirs_removed += 1;
                        }
                        Err(e) => warn!(path = %path, err = %e, "reconcile: could not remove an unfinished instance directory"),
                    }
                }
                SweepStep::LeaveUnreadable { pid, dir_name } => {
                    error!(
                        pid,
                        dir = %dir_name,
                        "reconcile: a Firecracker is running in an instance directory whose meta.json \
                         does not load. It has been LEFT RUNNING and this agent does not manage it: \
                         repair meta.json and restart the agent to adopt it, or kill the process to \
                         discard the VM"
                    );
                    out.left_unreadable += 1;
                }
                SweepStep::LeaveForeign { pid, dir_name } => {
                    warn!(
                        pid,
                        dir = %dir_name,
                        "reconcile: a Firecracker under this data dir was not started by this unit \
                         (it is in another cgroup); leaving it alone"
                    );
                    out.left_foreign += 1;
                }
            }
        }
        out
    }

    /// Is this a record that a wake or a start may have run ahead of — it
    /// holds no VMM, and says the VM is asleep, stopped or in error?
    fn may_adopt_running_vmm(v: &Vm) -> bool {
        v.pid.is_none() && matches!(v.state, VmState::Sleeping | VmState::Stopped | VmState::Error)
    }

    /// A Firecracker is running in the directory of a record that holds none
    /// and says the VM is asleep, stopped or in error. The previous agent woke
    /// or started that VM and went away (or failed to write) before the record
    /// said so. Returns true when the VMM was adopted.
    ///
    /// If the guest is RUNNING, the record is what is behind: the guest has
    /// been writing to its disk since, and killing it would leave a `sleeping`
    /// record on top of a snapshot the disk has outrun — waking that restores
    /// old memory over a newer filesystem. So the VMM is adopted and the
    /// record brought up to it.
    ///
    /// Anything else — never started, still paused after a snapshot was
    /// loaded or written, or no answer — means the guest has not run since
    /// the state on disk was taken. Killing that VMM loses nothing, and the
    /// caller does.
    async fn adopt_running_vmm(&self, pid: i32, dir_name: &str) -> bool {
        let Some(dir) = instance_path(&self.data_dir, dir_name) else {
            return false;
        };
        let sock = format!("{}/{}", dir, procscan::SOCK_NAME);
        if probe_vmm(&sock).await.as_deref() != Some("Running") {
            return false;
        }
        let (id, was) = {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.dir_id == dir_name) {
                Some(v) if Self::may_adopt_running_vmm(v) => {
                    let was = v.state.clone();
                    v.pid = Some(pid);
                    v.state = VmState::Running;
                    // The guest has run since the snapshot was taken.
                    v.snapshot_valid = false;
                    (v.id.clone(), was)
                }
                _ => return false,
            }
        };
        warn!(
            vm = %id,
            pid,
            recorded = %was,
            "reconcile: this VM's guest is running although its record said otherwise (a wake or \
             start that the previous agent did not finish writing down); adopted as running"
        );
        if let Err(e) = self.write_meta(&id).await {
            warn!(vm = %id, err = %e, "reconcile: could not write the adopted VM's record yet");
        }
        if was == VmState::Sleeping {
            // What the wake did not get to: the state file goes, so that no
            // later reading (an older agent's, after a rollback) takes this
            // running guest for one asleep. mem.bin stays: the guest runs on it.
            remove_if_present(&id, &format!("{}/{}", dir, VMSTATE));
        }
        true
    }

    /// Remove pool VMs that reconcile found without their VMM. Returns how
    /// many were removed.
    ///
    /// They were prewarmed, never handed to a tenant, and cannot be claimed
    /// any more; left alone each one keeps a slot, a tap and a full rootfs
    /// copy as a `stopped` record nothing ever deletes — one per pool VM after
    /// every host reboot. Same conditions as the sweep: only the agent holding
    /// the data-dir lock does it, and a directory that a Firecracker is still
    /// running out of (one this unit did not start, which the sweep left
    /// alone) is not touched.
    async fn remove_dead_pool_vms(&self, procs: &ProcTable, dead_pool: &[String]) -> usize {
        if dead_pool.is_empty() || !self.hold_data_dir_lock() {
            return 0;
        }
        let occupied: std::collections::HashSet<String> =
            procs.scan().into_iter().map(|p| p.dir_name).collect();
        let mut removed = 0;
        for id in dead_pool {
            let dir_id = {
                let g = self.inner.lock().await;
                match g.vms.iter().find(|v| &v.id == id) {
                    Some(v) => v.dir_id.clone(),
                    None => continue,
                }
            };
            if occupied.contains(&dir_id) {
                warn!(vm = %id, "reconcile: a Firecracker is still running in a dead pool VM's directory; leaving it");
                continue;
            }
            match self.delete(id).await {
                Ok(()) => {
                    info!(vm = %id, "reconcile: removed a pool VM whose Firecracker is gone");
                    removed += 1;
                }
                Err(e) => warn!(vm = %id, err = %e, "reconcile: could not remove a dead pool VM"),
            }
        }
        removed
    }

    /// Step 4 of `reconcile`: ask each adopted Firecracker what its guest is
    /// doing and make that agree with the record.
    ///
    /// The record is what the last completed operation left; anything else is
    /// an operation that was cut short and that nobody was told had worked, so
    /// the VMM is brought back to the record, not the other way round.
    ///
    /// All of them are asked at once, so start-up waits `ADOPT_API_TIMEOUT`
    /// in total for the ones that do not answer, not that long for each. A VMM
    /// that does not answer is not given up on: it goes on the `unsettled`
    /// list and is asked again every few seconds (`settle_unanswered`).
    async fn settle_adopted_run_states(&self) -> Settled {
        let adopted: Vec<(String, String, i32)> = {
            let g = self.inner.lock().await;
            g.vms
                .iter()
                .filter_map(|v| v.pid.map(|p| (v.id.clone(), v.dir_id.clone(), p)))
                .collect()
        };
        let answers = self.probe_all(&adopted).await;
        let mut out = Settled::default();
        for ((id, dir_id, pid), answer) in adopted.into_iter().zip(answers) {
            match answer {
                Probe::NoSocketPath => {}
                Probe::Silent => {
                    warn!(
                        vm = %id,
                        pid,
                        "adopted VM's Firecracker did not answer on its API socket (busy writing a \
                         snapshot, or stuck); its record is kept as it is and it will be asked again"
                    );
                    lock_std(&self.unsettled).insert(id, Unsettled::Pending(pid));
                    out.unreachable += 1;
                }
                Probe::Says(vmm) => match self.apply_settle(&id, &dir_id, pid, &vmm).await {
                    SettleResult::AsRecorded => {}
                    SettleResult::Resumed => out.resumed += 1,
                    SettleResult::Paused => out.paused += 1,
                    SettleResult::Failed => out.failed += 1,
                },
            }
        }
        out
    }

    /// Ask every listed VMM (id, dir_id, pid) what its guest is doing, all at
    /// the same time. Answers come back in the order of `vms`.
    async fn probe_all(&self, vms: &[(String, String, i32)]) -> Vec<Probe> {
        let mut answers: Vec<Probe> = vms.iter().map(|_| Probe::NoSocketPath).collect();
        let mut asking = tokio::task::JoinSet::new();
        for (i, (_, dir_id, _)) in vms.iter().enumerate() {
            let Some(dir) = instance_path(&self.data_dir, dir_id) else {
                continue;
            };
            let sock = format!("{}/{}", dir, procscan::SOCK_NAME);
            answers[i] = Probe::Silent;
            asking.spawn(async move { (i, probe_vmm(&sock).await) });
        }
        while let Some(done) = asking.join_next().await {
            if let Ok((i, Some(state))) = done {
                answers[i] = Probe::Says(state);
            }
        }
        answers
    }

    /// Bring one adopted VM's Firecracker, which reports `vmm`, in line with
    /// its record — the record as it is NOW: the answer may have been a while
    /// in coming. Does nothing when the record no longer holds `pid`.
    async fn apply_settle(&self, id: &str, dir_id: &str, pid: i32, vmm: &str) -> SettleResult {
        let state = {
            let g = self.inner.lock().await;
            match g.vms.iter().find(|v| v.id == id) {
                Some(v) if v.pid == Some(pid) => v.state.clone(),
                _ => return SettleResult::AsRecorded,
            }
        };
        let Some(dir) = instance_path(&self.data_dir, dir_id) else {
            return SettleResult::AsRecorded;
        };
        let sock = format!("{}/{}", dir, procscan::SOCK_NAME);
        match settle_action(&state, vmm) {
            SettleAction::None => SettleResult::AsRecorded,
            SettleAction::Resume => {
                let done = tokio::time::timeout(ADOPT_API_TIMEOUT, fc::patch_vm_state(&sock, "Resumed")).await;
                if matches!(done, Ok(Ok(()))) {
                    warn!(vm = %id, pid, "adopted VM was paused inside Firecracker under a record that says running (the previous agent stopped in the middle of a sleep, fork or pause); resumed");
                    SettleResult::Resumed
                } else {
                    // Could not resume it. Record what is true instead, so
                    // that `resume` is a real request and not a no-op on a
                    // VM that is listed as running and frozen.
                    error!(vm = %id, pid, "adopted VM is paused inside Firecracker and could not be resumed; recording it as paused");
                    self.set_state(id, VmState::Paused).await;
                    let _ = self.write_meta(id).await;
                    SettleResult::Failed
                }
            }
            SettleAction::Pause => {
                let done = tokio::time::timeout(ADOPT_API_TIMEOUT, fc::patch_vm_state(&sock, "Paused")).await;
                if matches!(done, Ok(Ok(()))) {
                    warn!(vm = %id, pid, state = %state, "adopted VM was running inside Firecracker under a record that says it is not (the previous agent stopped in the middle of a resume or a pool claim); paused again");
                    SettleResult::Paused
                } else if state == VmState::Paused {
                    error!(vm = %id, pid, "adopted VM is running inside Firecracker and could not be paused; recording it as running");
                    self.set_state(id, VmState::Running).await;
                    let _ = self.write_meta(id).await;
                    SettleResult::Failed
                } else {
                    // A pooled VM that would not pause is still claimable:
                    // the claim's resume is a no-op on a running guest.
                    error!(vm = %id, pid, "adopted pool VM is running inside Firecracker and could not be paused");
                    SettleResult::Failed
                }
            }
            SettleAction::Discard => {
                error!(vm = %id, pid, "adopted Firecracker never started its guest; killing it");
                self.kill_fc_and_wait(pid, dir_id).await;
                {
                    let mut g = self.inner.lock().await;
                    if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                        if v.pid == Some(pid) {
                            v.state = VmState::Error;
                            v.pid = None;
                        }
                    }
                }
                let _ = self.write_meta(id).await;
                SettleResult::Failed
            }
        }
    }

    /// Ask the Firecrackers that did not answer at start-up again (liveness
    /// loop, every 5 s), and settle the ones that answer now.
    ///
    /// The case this exists for: the agent is restarted while a guest with
    /// several GiB of memory is being put to sleep or forked. Firecracker
    /// finishes writing the snapshot on its own — for seconds to minutes,
    /// during which its API answers nothing — and leaves the guest PAUSED,
    /// under a record that says `running`. Start-up cannot wait that long.
    pub async fn settle_unanswered(&self) {
        // Mark the whole batch as being looked at: a lifecycle operation that
        // arrives for one of these VMs now waits for this pass to finish.
        let batch: Vec<(String, i32)> = {
            let mut list = lock_std(&self.unsettled);
            let pending: Vec<(String, i32)> = list
                .iter()
                .filter_map(|(id, u)| match u {
                    Unsettled::Pending(pid) => Some((id.clone(), *pid)),
                    Unsettled::Settling => None,
                })
                .collect();
            for (id, _) in &pending {
                list.insert(id.clone(), Unsettled::Settling);
            }
            pending
        };
        if batch.is_empty() {
            return;
        }
        // Only VMs that still hold the VMM that was asked.
        let mut current: Vec<(String, String, i32)> = Vec::new();
        {
            let g = self.inner.lock().await;
            for (id, pid) in &batch {
                match g.vms.iter().find(|v| &v.id == id && v.pid == Some(*pid)) {
                    Some(v) => current.push((id.clone(), v.dir_id.clone(), *pid)),
                    None => {
                        // Stopped, slept, deleted or dead since: settled.
                        lock_std(&self.unsettled).remove(id);
                    }
                }
            }
        }
        let answers = self.probe_all(&current).await;
        for ((id, dir_id, pid), answer) in current.into_iter().zip(answers) {
            match answer {
                Probe::Says(vmm) => {
                    let result = self.apply_settle(&id, &dir_id, pid, &vmm).await;
                    info!(vm = %id, pid, vmm = %vmm, result = ?result, "adopted VM's Firecracker answers now; its run state is settled");
                    lock_std(&self.unsettled).remove(&id);
                }
                Probe::Silent => {
                    lock_std(&self.unsettled).insert(id, Unsettled::Pending(pid));
                }
                Probe::NoSocketPath => {
                    lock_std(&self.unsettled).remove(&id);
                }
            }
        }
    }

    /// Called by every operation that is about to change a VM's run state,
    /// before it talks to the VMM. Returns true when start-up never heard from
    /// this VM's Firecracker, i.e. when the run state on its record is not
    /// known to be true (see `Unsettled`).
    ///
    /// Either way the VM is off the list afterwards: the caller is about to
    /// establish what its guest is doing, which is all the list is for.
    async fn claim_unsettled(&self, id: &str) -> bool {
        loop {
            {
                let mut list = lock_std(&self.unsettled);
                match list.get(id) {
                    None => return false,
                    Some(Unsettled::Pending(_)) => {
                        list.remove(id);
                        return true;
                    }
                    // `settle_unanswered` is talking to this VMM; it is done
                    // within two API timeouts.
                    Some(Unsettled::Settling) => {}
                }
            }
            sleep(Duration::from_millis(10)).await;
        }
    }

    /// Hold `id` for one lifecycle operation of `kind` (FLU-4). The guard
    /// lets go when it is dropped, on every exit path.
    ///
    /// * Nothing holds the sandbox: it is this operation's.
    /// * An operation of the same kind holds it: wait for that one (at most
    ///   `SAME_OP_WAIT`, then `Busy`), then go on — the caller then answers
    ///   from the state the first one left, so a retried sleep says "already
    ///   asleep".
    /// * An operation of another kind holds it, forks of it read its files
    ///   while it sleeps, or it is being stopped or deleted: `Busy`, at once.
    ///   Carrying out a wake queued behind a sleep, or anything queued behind
    ///   a stop, would act on a state its caller never saw.
    ///
    /// Fork does not come through here (`begin_fork`).
    ///
    /// Composes with `claim_unsettled`, which every operation still calls
    /// after this: the mark keeps operations apart from each other, the
    /// unsettled list keeps them apart from the start-up probe.
    async fn begin_op(&self, id: &str, kind: OpKind) -> Result<OpGuard<'_>, OpError> {
        let deadline = Instant::now() + SAME_OP_WAIT;
        loop {
            {
                let mut map = lock_std(&self.busy);
                let m = map.entry(id.to_string()).or_default();
                if m.closing > 0 {
                    return Err(OpError::Busy);
                }
                match m.op {
                    None if m.readers == 0 => {
                        m.op = Some(kind);
                        return Ok(OpGuard { busy: &self.busy, id: id.to_string(), shared: false });
                    }
                    Some(k) if k == kind && Instant::now() < deadline => {}
                    _ => return Err(OpError::Busy),
                }
            }
            sleep(BUSY_POLL).await;
        }
    }

    /// Hold `id` for a fork (S4'), without waiting: a fork queued behind
    /// another operation would act on a state its caller never saw, and its
    /// caller (the control plane) gives up after 30 s anyway. Anything that
    /// holds the sandbox answers `Busy`, except forks of it while it sleeps:
    /// this one joins them (`readers`). Otherwise the hold is exclusive; the
    /// fork shares it (`OpGuard::share`) once it has read that the sandbox is
    /// asleep.
    ///
    /// Also the mark of a fork's CHILD, from its reservation to the end of
    /// the fork: no record has that id before, so nothing else holds it.
    fn begin_fork(&self, id: &str) -> Result<OpGuard<'_>, OpError> {
        let mut map = lock_std(&self.busy);
        let m = map.entry(id.to_string()).or_default();
        if m.closing > 0 || m.op.is_some() {
            return Err(OpError::Busy);
        }
        if m.readers > 0 {
            m.readers += 1;
            return Ok(OpGuard { busy: &self.busy, id: id.to_string(), shared: true });
        }
        m.op = Some(OpKind::Fork);
        Ok(OpGuard { busy: &self.busy, id: id.to_string(), shared: false })
    }

    /// Mark `id` as being stopped or deleted, until the guard drops: from now
    /// on no operation starts on it and none in progress commits.
    ///
    /// Stop and delete do not queue behind an operation. There is no timeout
    /// on a Firecracker API call, so an operation stuck in one would make the
    /// sandbox impossible to stop or delete; instead they end the VMM (which
    /// fails the call in progress) and then wait for the operation to let go
    /// (`wait_op_released`).
    fn begin_close(&self, id: &str) -> CloseGuard<'_> {
        lock_std(&self.busy).entry(id.to_string()).or_default().closing += 1;
        CloseGuard { busy: &self.busy, id: id.to_string() }
    }

    /// Wait, at most `CLOSE_WAIT`, until no operation holds `id`. False when
    /// one still does.
    async fn wait_op_released(&self, id: &str) -> bool {
        let deadline = Instant::now() + CLOSE_WAIT;
        loop {
            if !self.op_in_progress(id) {
                return true;
            }
            if Instant::now() >= deadline {
                return false;
            }
            sleep(BUSY_POLL).await;
        }
    }

    /// Does a lifecycle operation, or a fork reading its files, hold `id`
    /// right now?
    fn op_in_progress(&self, id: &str) -> bool {
        matches!(lock_std(&self.busy).get(id), Some(m) if m.op.is_some() || m.readers > 0)
    }

    /// Is `id` being stopped or deleted right now?
    fn closing(&self, id: &str) -> bool {
        matches!(lock_std(&self.busy).get(id), Some(m) if m.closing > 0)
    }

    /// May an operation that started out from VMM `pid` commit to this
    /// record? Called inside the `inner` critical section that commits. Not
    /// when the sandbox is being stopped or deleted (that has ended or is
    /// ending the VMM, and its record is not the operation's to write any
    /// more), and not when the record no longer holds the VMM the operation
    /// was talking to (the exit reaper recorded its death, or a stop took it).
    fn may_commit(&self, v: &Vm, pid: Option<i32>) -> bool {
        v.pid == pid && !self.closing(&v.id)
    }

    /// Is `mem` mapped by a Firecracker of the instance `dir_id` (one of the
    /// processes `scan` listed), by device and inode in `/proc/<pid>/maps`?
    /// `None` when that cannot be told.
    ///
    /// A guest that was woken runs on a private mapping of its `mem.bin` for
    /// the rest of its life. Unlinking the file under it is harmless (the lab
    /// showed it: the mapping keeps its pages); what this check is for is
    /// keeping the file a running guest was loaded from in its directory, as
    /// long as that guest runs. A wrong "not mapped" is harmless for the same
    /// reason — btrfs subvolumes, for one, print another device number in
    /// `maps` than `stat` reports.
    fn mem_mapped(&self, dir_id: &str, mem: &str, scan: &[FcProc]) -> Option<bool> {
        use std::os::unix::fs::MetadataExt;
        let st = match std::fs::metadata(mem) {
            Ok(m) => m,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Some(false),
            Err(_) => return None,
        };
        let (major, minor) = dev_major_minor(st.dev());
        for p in scan.iter().filter(|p| p.dir_name == dir_id) {
            match std::fs::read_to_string(self.proc_root.join(p.pid.to_string()).join("maps")) {
                Ok(text) => match maps_contain(&text, major, minor, st.ino()) {
                    Some(false) => {}
                    other => return other,
                },
                // Exited since the scan.
                Err(e) if e.kind() == std::io::ErrorKind::NotFound
                    || e.raw_os_error() == Some(libc::ESRCH) => {}
                Err(_) => return None,
            }
        }
        Some(false)
    }

    /// Remove the snapshot files of an instance whose snapshot can no longer
    /// be used (S8, S9): `vmstate.bin`, its temp file, and `mem.bin` unless a
    /// live VMM of the instance maps it (or that cannot be told).
    ///
    /// `vmstate.bin` goes first: it is what an older agent takes as "asleep",
    /// so it must not outlive the `sleeping` record (S11).
    fn discard_snapshot_files(&self, id: &str, dir_id: &str) {
        self.discard_snapshot_files_in(id, dir_id, &self.procs().scan());
    }

    /// `discard_snapshot_files`, with the Firecrackers of this data dir
    /// already listed (`scan`).
    fn discard_snapshot_files_in(&self, id: &str, dir_id: &str, scan: &[FcProc]) {
        let Some(dir) = instance_path(&self.data_dir, dir_id) else {
            return;
        };
        remove_if_present(id, &format!("{}/{}", dir, VMSTATE_TMP));
        remove_if_present(id, &format!("{}/{}", dir, VMSTATE));
        let mem = format!("{}/{}", dir, MEM);
        match self.mem_mapped(dir_id, &mem, scan) {
            Some(false) => {
                remove_if_present(id, &mem);
            }
            Some(true) => info!(vm = %id, "mem.bin kept: a live Firecracker of this instance maps it"),
            None => warn!(vm = %id, "mem.bin kept: could not tell whether a live Firecracker maps it"),
        }
    }

    /// Set a stale snapshot aside (S3'): `vmstate.bin` and `mem.bin` are
    /// renamed to `*.outrun`, not removed. The verdict that the guest ran
    /// after the snapshot rests on file times (or, for a wake, on a reply
    /// that never came), and a wrong one — a restore that lost the times, say
    /// — must not destroy every old snapshot on a node at once. Nothing takes
    /// a `*.outrun` file for a snapshot. Only `start`, `stop` and the next
    /// sleep of the sandbox remove them (`remove_outrun`), and delete with the
    /// directory; start-up leaves them. The temporary state file goes: it is
    /// never a snapshot.
    fn set_snapshot_aside(&self, id: &str, dir_id: &str) {
        let Some(dir) = instance_path(&self.data_dir, dir_id) else {
            return;
        };
        remove_if_present(id, &format!("{}/{}", dir, VMSTATE_TMP));
        // The state file first: it is what an older agent takes as "asleep".
        for name in [VMSTATE, MEM] {
            let (from, to) = (format!("{}/{}", dir, name), format!("{}/{}{}", dir, name, OUTRUN));
            match std::fs::rename(&from, &to) {
                Ok(()) => warn!(vm = %id, file = %from, to = %to, "a stale snapshot file was set aside"),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => warn!(vm = %id, file = %from, err = %e, "could not set a stale snapshot file aside"),
            }
        }
    }

    /// Remove what `set_snapshot_aside` kept.
    fn remove_outrun(&self, id: &str, dir_id: &str) {
        let Some(dir) = instance_path(&self.data_dir, dir_id) else {
            return;
        };
        for name in [VMSTATE, MEM] {
            remove_if_present(id, &format!("{}/{}{}", dir, name, OUTRUN));
        }
    }

    /// Start-up, after the sweep (S1, S3, S9): write the records whose state
    /// start-up changed, and remove the snapshot files nothing can use any
    /// more — or, for a sleeper whose disk outran its snapshot (`outrun`),
    /// set them aside (S3'). Only by the agent that holds the data-dir lock,
    /// and only for records that loaded — a directory whose `meta.json` does
    /// not load is not touched. Of a record whose files could not be
    /// examined (`undecided`) only the temporary state file goes.
    async fn settle_snapshot_files_at_start(&self, rewrite: &[String], undecided: &[String], outrun: &[String]) {
        if !self.hold_data_dir_lock() {
            return;
        }
        for id in rewrite {
            if let Err(e) = self.write_meta(id).await {
                warn!(vm = %id, err = %e, "reconcile: could not write the record start-up corrected yet");
            }
        }
        let vms: Vec<(String, String, VmState, bool)> = {
            let g = self.inner.lock().await;
            g.vms
                .iter()
                .map(|v| (v.id.clone(), v.dir_id.clone(), v.state.clone(), v.pid.is_some()))
                .collect()
        };
        // One look at the process table for all of them (it used to be one
        // per stopped record).
        let scan = self.procs().scan();
        for (id, dir_id, state, has_vmm) in vms {
            let Some(dir) = instance_path(&self.data_dir, &dir_id) else {
                continue;
            };
            // A sleep the previous agent did not finish: never part of a
            // snapshot anything relies on, whatever else is undecided (S9).
            remove_if_present(&id, &format!("{}/{}", dir, VMSTATE_TMP));
            if state == VmState::Sleeping || undecided.contains(&id) {
                continue;
            }
            if outrun.contains(&id) {
                self.set_snapshot_aside(&id, &dir_id);
            } else if has_vmm {
                // The guest may run on mem.bin (it was woken): only the state
                // file goes.
                remove_if_present(&id, &format!("{}/{}", dir, VMSTATE));
            } else {
                self.discard_snapshot_files_in(&id, &dir_id, &scan);
            }
        }
    }

    /// SIGKILL an instance's Firecracker. Returns whether there is a dying
    /// process worth waiting for (`wait_fc_gone`).
    ///
    /// There is no await in here on purpose: a caller can change the record
    /// and send the signal as one step that nothing — not even its own
    /// request being dropped — can come between.
    ///
    /// The identity check comes first: the pid is only signalled while it is
    /// positively (or, on a failed read, possibly) this instance's VMM, never
    /// merely because a process has that number.
    fn kill_fc(&self, pid: i32, dir_id: &str) -> bool {
        use nix::sys::signal::{kill, Signal};
        use nix::unistd::Pid;
        let procs = self.procs();
        if procs.identify(pid, dir_id) == Identity::Mismatch {
            // Not this instance's VMM (any more). If that is because it is
            // already on its way out, give it the moment it needs to let go
            // of the tap; a stranger with a recycled pid is not waited for.
            return procs.exiting(pid);
        }
        let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
        true
    }

    /// Wait until a killed Firecracker is gone, for at most `KILL_WAIT`.
    ///
    /// Waiting matters for VMMs this agent did not spawn — `waitpid` returns
    /// at once for a process that is not a child, and a caller that went on
    /// to respawn (sleep → wake) would find the tap and the sockets still
    /// held by the dying one. "Gone" means every thread has exited, which is
    /// when the memory, the tap and the sockets are released — not merely
    /// that the process no longer looks like a Firecracker (see
    /// `ProcTable::exited`).
    async fn wait_fc_gone(&self, pid: i32, dir_id: &str) {
        let procs = self.procs();
        let deadline = Instant::now() + KILL_WAIT;
        while !procs.exited(pid) {
            if Instant::now() >= deadline {
                warn!(pid, dir = %dir_id, "firecracker still present {:?} after SIGKILL", KILL_WAIT);
                break;
            }
            sleep(Duration::from_millis(20)).await;
        }
    }

    /// SIGKILL an instance's Firecracker and wait until the process is gone.
    async fn kill_fc_and_wait(&self, pid: i32, dir_id: &str) {
        if self.kill_fc(pid, dir_id) {
            self.wait_fc_gone(pid, dir_id).await;
        }
    }

    /// Whether this node's guest network fences are in force. Cheap enough
    /// for a health handler or a 5s heartbeat.
    ///
    /// A `--net off` node has no guest network at all, so there is nothing to
    /// isolate and nothing to report as broken — the underlying atomics only
    /// mean anything once host networking has been set up.
    ///
    /// FLU-8 N9': once the isolation watch has completed a tick, its verdict
    /// (`watch::Record::verdict`); before that, and with the watch off,
    /// start-up's and the last rule change's (`net::netfilter_ready`).
    pub fn isolation_ok(&self) -> bool {
        if !self.rules_on() { return true; }
        let verdict = lock_std(&self.watch).verdict(Instant::now());
        verdict.unwrap_or_else(|| self.rules.fences_ready())
    }

    /// Remove slot `s`'s tap (its anti-spoof chain first) under the rule lock,
    /// so that a watch tick never pins a tap that is being taken away, nor
    /// counts its vanishing as a failed repair.
    async fn remove_tap_locked(&self, s: u32) {
        let _rules = self.ruleset_lock.lock().await;
        self.rules.remove_tap(&net::tap_name(s));
    }

    /// After a rule rebuild, under the rule lock: what the kernel now holds
    /// may let guest addresses go again.
    ///
    /// A. Slots a delete held because a rebuild failed are freed by the first
    ///    state in which the latest isolation AND the latest ingress rebuild
    ///    both succeeded: both ran after those sandboxes left the membership,
    ///    so no rule names their addresses any more.
    /// B. A start-up whose ingress chain could be neither rebuilt nor flushed
    ///    hands out no address until the chain has been rebuilt: the previous
    ///    run's `dport -> address` rules would otherwise put each new sandbox
    ///    behind somebody else's node ports.
    fn settle_slots(&self, g: &mut Inner) {
        let (pair_ok, ingress_ok) = {
            let r = lock_std(&self.watch);
            (r.applied(&net::Chain::Forward) && r.applied(&net::Chain::Ingress), r.applied(&net::Chain::Ingress))
        };
        if ingress_ok && g.slots_withheld {
            g.slots_withheld = false;
            info!("the ingress DNAT chain has been rebuilt: guest addresses are handed out again");
        }
        if pair_ok && !g.held_slots.is_empty() {
            let held: Vec<u32> = std::mem::take(&mut g.held_slots).into_iter().collect();
            for s in &held {
                Self::free_slot(g, *s);
            }
            info!(slots = ?held, "both rule rebuilds succeeded: the slots held since a failed rebuild are free again");
        }
    }

    /// Start-up's ingress rebuild (B). When it fails, a bare flush of the
    /// chain is tried — one small transaction, the chain with no rule but its
    /// marker — and when that fails as well, no guest address is handed out
    /// until the chain has been rebuilt (the watch retries it every tick).
    /// Returns whether the rebuild succeeded.
    pub async fn start_ingress(&self) -> bool {
        if self.refresh_ingress().await { return true; }
        if !self.rules_on() { return true; }
        let flushed = {
            let _rules = self.ruleset_lock.lock().await;
            let rules = Arc::clone(&self.rules);
            tokio::task::spawn_blocking(move || rules.kernel().apply(&net::ingress_ruleset(&[])).ok())
                .await
                .unwrap_or(false)
        };
        if flushed {
            warn!("the ingress DNAT chain could not be rebuilt at start-up and has been emptied instead: \
                   exposed ports do not answer until a rebuild succeeds");
        } else {
            self.inner.lock().await.slots_withheld = true;
            error!("the ingress DNAT chain could be neither rebuilt nor emptied at start-up: it may still hold \
                    the previous run's node-port rules for addresses no sandbox holds now, so creates, forks \
                    and pool refills are refused (no guest address is handed out) until it is rebuilt");
        }
        false
    }

    /// Rebuild the cross-tenant isolation ruleset from the current VM list
    /// (every VM holding an IP, keyed by tenant). Snapshots membership under
    /// the state lock, releases it, then shells out to nft. Rebuilds are
    /// serialized so concurrent create/fork/delete can't interleave nft
    /// commands; each rebuild snapshots at its start, so the last one to run
    /// leaves the freshest state.
    ///
    /// Returns false when isolation is NOT in force afterwards — the caller
    /// that triggered the rebuild must not leave a VM running on a node where
    /// every sandbox shares one flat network.
    pub async fn refresh_isolation(&self) -> bool {
        if !self.rules_on() { return true; }
        let _guard = self.ruleset_lock.lock().await;
        let members = Self::isolation_members(&*self.inner.lock().await);
        let (outcome, ok) = self.rules.rebuild_isolation(&members);
        // FLU-8: what the watch compares the kernel with, under this lock.
        lock_std(&self.watch).built(net::Chain::Forward, outcome.ok().then(|| net::marker_of(&net::isolation_ruleset(&members))).flatten());
        self.settle_slots(&mut *self.inner.lock().await);
        ok
    }

    /// `(tenant, ip)` of every VM holding an IP: the isolation membership.
    fn isolation_members(g: &Inner) -> Vec<(String, String)> {
        g.vms.iter()
            .filter_map(|v| {
                v.ip.clone().map(|ip| (v.tenant_id.clone().unwrap_or_default(), ip))
            })
            .collect()
    }

    /// `(node_port, ip, guest_port)` of every expose of every VM holding an
    /// IP: the ingress membership.
    fn ingress_members(g: &Inner) -> Vec<(u16, String, u16)> {
        let mut out = Vec::new();
        for v in &g.vms {
            if let Some(ip) = &v.ip {
                for e in &v.exposes {
                    out.push((e.node_port, ip.clone(), e.guest_port));
                }
            }
        }
        out
    }

    /// Rebuild the ingress DNAT ruleset from the current VM list (every expose
    /// of every VM holding an IP). Same discipline as `refresh_isolation`:
    /// snapshot membership under the state lock, release it, then shell out to
    /// nft under the ruleset lock so rebuilds never interleave.
    ///
    /// Returns false when the rules were not applied (the kernel keeps the
    /// previous chain): an expose must not answer success for a rule that is
    /// not there.
    pub async fn refresh_ingress(&self) -> bool {
        if !self.rules_on() { return true; }
        let _guard = self.ruleset_lock.lock().await;
        self.rebuild_ingress_locked().await
    }

    /// `refresh_ingress` for a caller that holds the rule lock already.
    async fn rebuild_ingress_locked(&self) -> bool {
        // As `refresh_ingress`: nothing to touch on a node without rules. The
        // expose path called this unguarded from r6 on, and a `--net off`
        // manager in a unit test applied a real DNAT rule to the host.
        if !self.rules_on() { return true; }
        let members = Self::ingress_members(&*self.inner.lock().await);
        let ok = self.rules.rebuild_ingress(&members).ok();
        lock_std(&self.watch).built(net::Chain::Ingress, ok.then(|| net::marker_of(&net::ingress_ruleset(&members))).flatten());
        self.settle_slots(&mut *self.inner.lock().await);
        ok
    }

    /// Whether the rule rebuilds and the tap removal touch anything.
    fn rules_on(&self) -> bool {
        self.rules.active(self.net_on)
    }

    // ---- ingress expose (v4 P3) ----

    /// Map a host node port to `id`'s ip:guest_port (TCP DNAT). Idempotent per
    /// (vm, guest_port): an existing entry returns its node_port unchanged,
    /// whatever `avoid` says (the control plane never lists that port in it).
    ///
    /// A new mapping gets `prefer` when no sandbox here holds it, and
    /// otherwise the next port round-robin that is neither in use nor in
    /// `avoid` (`pick_node_port`). The answer is success only once the DNAT
    /// rule is in the kernel: a rebuild that fails takes the mapping back out
    /// of the record and answers `RuleFailed`.
    ///
    /// The whole of it — the mapping into the record, the rebuild, and on a
    /// failure the mapping out again and a second rebuild — runs under the
    /// rule lock, and `meta.json` is written only once the rule is in. The
    /// mapping used to be written first: a roll-back write that did not land
    /// before a restart brought it back, possibly beside the sandbox that had
    /// got the port since; and a rebuild for another sandbox, run between the
    /// push and the failure, installed the doomed rule for good.
    pub async fn expose(
        &self,
        id: &str,
        guest_port: u16,
        avoid: &std::collections::HashSet<u16>,
        prefer: Option<u16>,
    ) -> Result<u16, ExposeError> {
        // The answers that need no rule change, without waiting for the rule
        // lock (a delete in progress holds it while the record has no ip).
        {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or(ExposeError::NotFound)?;
            if v.ip.is_none() { return Err(ExposeError::NoIp); }
            if let Some(e) = v.exposes.iter().find(|e| e.guest_port == guest_port) {
                return Ok(e.node_port);
            }
        }
        let rules = self.ruleset_lock.lock().await;
        // Looked at again below: anything may have changed while waiting.
        let node_port = {
            let mut g = self.inner.lock().await;
            // node ports are unique across ALL VMs — collect before the
            // mutable borrow of the target record.
            let used: std::collections::HashSet<u16> = g.vms.iter()
                .flat_map(|v| v.exposes.iter().map(|e| e.node_port))
                .collect();
            let cursor = g.node_port_cursor;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or(ExposeError::NotFound)?;
            if v.ip.is_none() { return Err(ExposeError::NoIp); }
            if let Some(e) = v.exposes.iter().find(|e| e.guest_port == guest_port) {
                // Idempotent: mapping already live, nothing to persist or rebuild.
                return Ok(e.node_port);
            }
            let (np, next) = pick_node_port(&used, avoid, prefer, cursor, NODE_PORT_MIN..=NODE_PORT_MAX)
                .ok_or(ExposeError::Exhausted)?;
            v.exposes.push(ExposeEntry { guest_port, node_port: np });
            g.node_port_cursor = next;
            np
        };
        if self.rebuild_ingress_locked().await {
            drop(rules);
            // Persisted only now that the rule is in. Best-effort like other
            // meta writes off the happy path: the in-memory entry is
            // authoritative and a failed write is owed (`meta_owed`).
            if let Err(e) = self.write_meta(id).await {
                warn!(vm = %id, err = %e, "expose persist failed");
            }
            return Ok(node_port);
        }
        // The rule is not there. Answering with the port would send the
        // gateways to a node port nothing forwards — or, once the next
        // rebuild succeeds, it would appear without anyone having been told
        // it failed. Take this mapping back out, and say so. (A concurrent
        // expose of the same guest port that arrived between the push above
        // and this point was answered idempotently from this mapping; that
        // caller has been told success for a rule that is not there.)
        {
            let mut g = self.inner.lock().await;
            if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                v.exposes.retain(|e| !(e.guest_port == guest_port && e.node_port == node_port));
            }
        }
        // Again without it: a rebuild that "failed" by outrunning its time
        // limit may have committed all the same, and no rule for a mapping
        // nobody was told about may stay. Nothing was written to meta.json.
        if !self.rebuild_ingress_locked().await {
            error!(vm = %id, node_port, "expose: the rebuild without the failed mapping failed as well; \
                   the watch retries the ingress chain");
        }
        drop(rules);
        // X9: another write of this record (a sleep, a stop) may have landed
        // between the push and the roll-back, with the doomed mapping in it;
        // the record on disk is the one without it again (best effort).
        if let Err(e) = self.write_meta(id).await {
            warn!(vm = %id, err = %e, "expose roll-back persist failed");
        }
        error!(vm = %id, guest_port, node_port, "expose: the ingress rule could not be installed; mapping removed");
        Err(ExposeError::RuleFailed)
    }

    /// Remove the (vm, guest_port) mapping. A missing entry is OK (idempotent);
    /// NotFound only when the VM itself is unknown.
    ///
    /// A rebuild that fails after the mapping left the record still answers
    /// success: the mapping is gone, and a retry would find nothing to remove
    /// and answer success anyway, so an error here could not be acted on. The
    /// stale rule still forwards to this same guest, and the next successful
    /// rebuild (any expose, unexpose or delete on this worker) removes it; the
    /// port is not handed out again meanwhile unless the control plane, which
    /// quarantines it, lets it.
    pub async fn unexpose(&self, id: &str, guest_port: u16) -> Result<(), ExposeError> {
        let changed = {
            let mut g = self.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or(ExposeError::NotFound)?;
            let before = v.exposes.len();
            v.exposes.retain(|e| e.guest_port != guest_port);
            v.exposes.len() != before
        };
        if changed {
            if let Err(e) = self.write_meta(id).await {
                warn!(vm = %id, err = %e, "unexpose persist failed");
            }
            if !self.refresh_ingress().await {
                error!(vm = %id, guest_port, "unexpose: the ingress rules could not be rebuilt; the old rule stays until the next rebuild that succeeds");
            }
        }
        Ok(())
    }

    pub async fn live_count(&self) -> u32 {
        let g = self.inner.lock().await;
        g.vms.iter().filter(|v| v.state == VmState::Running || v.state == VmState::Paused).count() as u32
    }

    /// Total parked pool VMs across every shape (heartbeat's pool_size).
    /// Derived from VM state — there is no separate claim queue anymore.
    pub async fn pool_count(&self) -> u32 {
        let g = self.inner.lock().await;
        g.vms.iter().filter(|v| v.state == VmState::Pooled).count() as u32
    }

    /// Replace the feluccad-managed template pools (PUT /v1/pools / register
    /// response). Applied atomically or not at all: every spec is validated
    /// before any state changes. The legacy config pool (pool_target) is
    /// unaffected. The 5s refill loop picks the new set up on its next tick.
    pub async fn set_pools(&self, specs: Vec<PoolSpec>) -> Result<(), String> {
        for s in &specs {
            validate_pool_spec(s)?;
        }
        let mut g = self.inner.lock().await;
        g.template_pools = specs;
        // From here on an empty list really does mean "no template pools".
        g.pools_synced = true;
        Ok(())
    }

    /// Build the /v1/vms JSON response. Exactly 7 keys per element, ip/pid explicit null.
    pub async fn list_json(&self) -> String {
        let g = self.inner.lock().await;
        let mut out = String::from("{\"vms\":[");
        let mut first = true;
        for v in &g.vms {
            if !first { out.push(','); }
            first = false;
            out.push('{');
            push_kv_str(&mut out, "id", &v.id); out.push(',');
            push_kv_str(&mut out, "name", &v.name); out.push(',');
            push_kv_str(&mut out, "state", v.state.as_str()); out.push(',');
            out.push_str(&format!("\"vcpus\":{},", v.vcpus));
            out.push_str(&format!("\"mem_mib\":{},", v.mem_mib));
            out.push_str("\"ip\":");
            match &v.ip {
                Some(ip) => out.push_str(&fc::json_str(ip)),
                None => out.push_str("null"),
            }
            out.push_str(",\"pid\":");
            match v.pid {
                Some(p) => out.push_str(&p.to_string()),
                None => out.push_str("null"),
            }
            out.push('}');
        }
        out.push_str("]}");
        out
    }

    pub async fn ip_of(&self, id: &str) -> Option<String> {
        let g = self.inner.lock().await;
        g.vms.iter().find(|v| v.id == id).and_then(|v| v.ip.clone())
    }

    /// Begin a rootfs capture (v4 P4): only a Stopped VM may be captured
    /// (any live FC could still be writing the file), only one capture per
    /// VM at a time, and `start()` refuses while the id is captured. The
    /// caller MUST pair this with `end_capture` (the server wraps the
    /// response stream in a drop guard so disconnects release it too).
    pub async fn begin_capture(&self, id: &str) -> Result<String, RootfsError> {
        let mut g = self.inner.lock().await;
        let v = g.vms.iter().find(|v| v.id == id).ok_or(RootfsError::NotFound)?;
        if v.state != VmState::Stopped {
            return Err(RootfsError::NotStopped);
        }
        let dir = instance_path(&self.data_dir, &v.dir_id).ok_or(RootfsError::NotFound)?;
        let path = format!("{}/rootfs.ext4", dir);
        if !g.capturing.insert(id.to_string()) {
            return Err(RootfsError::CaptureInProgress);
        }
        Ok(path)
    }

    /// Release a capture begun with `begin_capture`. Idempotent.
    pub async fn end_capture(&self, id: &str) {
        let mut g = self.inner.lock().await;
        g.capturing.remove(id);
    }

    // ---- paths ----

    /// The on-disk dir id for `id`. An unknown id falls back to itself, which
    /// is why `instance_path` must validate what comes out: the fallback is
    /// exactly the case where a caller-supplied string becomes a path
    /// component with no VM record vouching for it.
    fn dir_id_for_sync(vms: &[Vm], id: &str) -> String {
        vms.iter().find(|v| v.id == id).map(|v| v.dir_id.clone()).unwrap_or_else(|| id.to_string())
    }

    fn instance_dir_sync(vms: &[Vm], data_dir: &str, id: &str) -> Option<String> {
        instance_path(data_dir, &Self::dir_id_for_sync(vms, id))
    }

    fn sock_path_sync(vms: &[Vm], data_dir: &str, id: &str) -> Option<String> {
        Self::instance_dir_sync(vms, data_dir, id).map(|d| format!("{}/fc.sock", d))
    }

    async fn instance_dir(&self, id: &str) -> Option<String> {
        let g = self.inner.lock().await;
        Self::instance_dir_sync(&g.vms, &self.data_dir, id)
    }

    async fn sock_path(&self, id: &str) -> Option<String> {
        let g = self.inner.lock().await;
        Self::sock_path_sync(&g.vms, &self.data_dir, id)
    }

    // ---- slot allocation ----

    /// Whether a new guest may be put on (or a guest brought back onto) this
    /// node now. "IsolationFailed":
    /// * W3: while a fence this agent owns (input, forward or L2 chain, a
    ///   switch, a dormant table, the bridge gone) is POSITIVELY unrepaired —
    ///   the condition that takes the bridge down in `enforce` — in
    ///   `enforce` and in `report` alike (`watch::Record::refusal`). Not for
    ///   the host fence, a stale verdict, NAT or ingress, or "unknown".
    /// * W11: while start-up could not settle the ingress chain
    ///   (`start_ingress`); checked before any work, not only at the slot.
    async fn refuse_new_guest(&self, op: &str, id: &str) -> Result<(), String> {
        match self.new_guest_refusal().await {
            Some(why) => {
                warn!(vm = %id, op = %op, reason = %why, "refused: no new guest on this node now");
                Err("IsolationFailed".into())
            }
            None => Ok(()),
        }
    }

    /// Why no new guest may be put on this node now (`refuse_new_guest`),
    /// without logging it.
    async fn new_guest_refusal(&self) -> Option<&'static str> {
        let why = lock_std(&self.watch).refusal();
        if why.is_some() {
            return why;
        }
        if self.inner.lock().await.slots_withheld {
            return Some("the ingress DNAT chain has not been rebuilt since start-up");
        }
        None
    }

    /// A free guest slot, or why there is none: "IsolationFailed" while
    /// start-up could not settle the ingress chain (`start_ingress`), the
    /// answer a create already gets when isolation is not in force.
    fn claim_slot(inner: &mut Inner) -> Result<u32, &'static str> {
        if inner.slots_withheld {
            warn!("no guest address is handed out: the ingress DNAT chain has not been rebuilt since start-up \
                   and may still name addresses of the previous run");
            return Err("IsolationFailed");
        }
        inner.allocator.as_mut().and_then(|a| a.claim()).ok_or("IpPoolExhausted")
    }

    fn free_slot(inner: &mut Inner, idx: u32) {
        if let Some(a) = &mut inner.allocator { a.free(idx); }
    }

    fn reserve_slot(inner: &mut Inner, idx: u32) {
        if let Some(a) = &mut inner.allocator { a.reserve(idx); }
    }

    fn ip_for_slot(inner: &Inner, idx: u32) -> Option<String> {
        Some(inner.allocator.as_ref()?.ip_for(idx))
    }

    /// What slot `slot`'s tap is pinned to: the tap's name, the address and
    /// the MAC. ONE derivation for create, fork, wake, start and the
    /// isolation watch (FLU-8 N4'): the address of the record that holds the
    /// slot when there is one (None when that record has none: nothing to
    /// pin to), and the slot's own address when no record holds it — a VMM
    /// whose record did not load keeps running by design, and its tap still
    /// needs its pin. The MAC is always the slot's (`tap_pin_for_slot`).
    fn tap_pin(&self, inner: &Inner, slot: u32) -> (String, Option<String>, net::GuestMac) {
        let (tap, mac) = tap_pin_for_slot(slot);
        let ip = match inner.vms.iter().find(|v| v.slot == Some(slot)) {
            Some(v) => v.ip.clone(),
            None => Some(crate::ipalloc::fmt_ip(self.cidr.host_at(slot))),
        };
        (tap, ip, mac)
    }

    // ---- create ----

    pub async fn create(self: &Arc<Self>, spec: &CreateSpec) -> Result<(), String> {
        // Rejected before a record exists, so a bad id never reaches the
        // instances tree even as a Creating placeholder.
        if !valid_vm_id(&spec.id) {
            return Err("InvalidId".into());
        }
        let r = self.create_checked(spec).await;
        // W18: the worker's journal says why, not only the caller.
        if let Err(e) = &r {
            error!(vm = %spec.id, err = %e, "create failed");
        }
        r
    }

    async fn create_checked(self: &Arc<Self>, spec: &CreateSpec) -> Result<(), String> {
        // Before any work: a refused create used to copy the image first.
        self.refuse_new_guest("create", &spec.id).await?;
        // Try to claim a warm-pool VM of the exact requested shape
        // (image + vcpus + mem + disk); else cold boot.
        if self.claim_from_pool(spec).await? {
            return self.commit_or_isolate(&spec.id).await;
        }
        self.cold_create(spec, VmState::Running, None).await?;
        self.commit_or_isolate(&spec.id).await
    }

    /// Publish a freshly created VM into the tenant ruleset, or tear it down.
    ///
    /// A VM that is running while the isolation transaction is not in force
    /// sits on a flat network with every other tenant on the node, and nothing
    /// on either tenant's side would show it. Refusing the create is the only
    /// answer that fails closed; feluccad retries elsewhere.
    async fn commit_or_isolate(self: &Arc<Self>, id: &str) -> Result<(), String> {
        // W3: checked again here, where the new VM is committed — a fence may
        // have gone while it booted.
        if self.refresh_isolation().await && self.refuse_new_guest("create", id).await.is_ok() {
            return Ok(());
        }
        warn!(vm = %id, "cross-tenant isolation is not in force; removing the new vm");
        if let Err(e) = self.delete(id).await {
            warn!(vm = %id, err = %e, "isolation rollback failed");
        }
        Err("IsolationFailed".into())
    }

    /// Cold-boot a fresh VM. `force_slot` reuses a specific slot (pool refill).
    async fn cold_create(
        self: &Arc<Self>,
        spec: &CreateSpec,
        final_state: VmState,
        force_slot: Option<u32>,
    ) -> Result<(), String> {
        let id = spec.id.as_str();
        {
            let mut g = self.inner.lock().await;
            if g.vms.iter().any(|v| v.id == id) {
                return Err("AlreadyExists".into());
            }
            g.vms.push(Vm {
                id: spec.id.clone(),
                name: spec.name.clone(),
                dir_id: spec.id.clone(),
                vcpus: spec.vcpus,
                mem_mib: spec.mem_mib,
                state: VmState::Creating,
                pid: None,
                slot: None,
                ip: None,
                vsock: false,
                tenant_id: spec.tenant_id.clone(),
                exposes: Vec::new(),
                image: spec.image.clone(),
                image_sha256: spec.image_sha256.clone(),
                disk_gb: spec.disk_gb,
                snapshot_valid: false,
            });
        }

        // On error, mark as error state.
        let result = self.cold_create_inner(spec, final_state, force_slot).await;
        if result.is_err() {
            self.set_state(id, VmState::Error).await;
        }
        result
    }

    async fn cold_create_inner(
        self: &Arc<Self>,
        spec: &CreateSpec,
        final_state: VmState,
        force_slot: Option<u32>,
    ) -> Result<(), String> {
        let id = spec.id.as_str();
        let (vcpus, mem_mib) = (spec.vcpus, spec.mem_mib);
        let dir_path = self.instance_dir(id).await.ok_or("InvalidId")?;
        create_instance_dir(&dir_path).map_err(|e| e.to_string())?;

        // Pull-and-cache the template image first (no-op when already local).
        self.ensure_image(&spec.image, spec.image_sha256.as_deref()).await?;

        let rootfs_dst = format!("{}/rootfs.ext4", dir_path);
        let rootfs_src = format!("{}/images/{}.ext4", self.data_dir, spec.image);
        copy_rootfs(&rootfs_src, &rootfs_dst)?;

        // Grow the per-instance copy when a bigger disk was requested.
        if spec.disk_gb > 0 {
            self.grow_rootfs(&rootfs_dst, spec.disk_gb as u64).await?;
        }

        // Networking: claim a slot, derive IP + tap, bring the tap up.
        let mut ip_str: Option<String> = None;
        let mut slot: Option<u32> = None;

        // `rules_on`, the same as `net_on` outside unit tests, which put the
        // rule fake here and so run the slot claim and the pin.
        if self.rules_on() {
            let s = {
                let mut g = self.inner.lock().await;
                if let Some(fs) = force_slot {
                    Self::reserve_slot(&mut g, fs);
                    fs
                } else {
                    Self::claim_slot(&mut g)?
                }
            };
            slot = Some(s);
            // Cold boot sets the NIC's MAC itself (configure_guest), so the
            // L2 half of the pin applies from the first frame.
            let (tap, ip, mac) = {
                let g = self.inner.lock().await;
                self.tap_pin(&g, s)
            };
            ip_str = ip;
            if !self.rules.pin_tap(&tap, ip_str.as_deref(), &mac) {
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
                return Err("TapSetupFailed".into());
            }
            // X4: pinned and up — not the watch's to hold down any more.
            lock_std(&self.watch).release_tap(&tap);
        }

        let (pid, vsock_on) = self.spawn_and_configure(id, &dir_path, &rootfs_dst, vcpus, mem_mib, slot, ip_str.as_deref()).await?;

        // For pool/paused final state: pause now.
        if final_state == VmState::Pooled || final_state == VmState::Paused {
            let sock = self.sock_path(id).await.ok_or("InvalidId")?;
            fc::patch_vm_state(&sock, "Paused").await
                .map_err(|e| e.to_string())?;
        }

        {
            let mut g = self.inner.lock().await;
            if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                v.pid = Some(pid);
                v.slot = slot;
                v.state = final_state.clone();
                v.ip = ip_str.clone();
                v.vsock = vsock_on;
            }
        }
        self.write_meta(id).await?;
        Ok(())
    }

    // ---- image cache (v4 P4) ----

    /// Ensure `{data_dir}/images/{image}.ext4` is present and (when a sha is
    /// given) matches it, pulling from the control plane otherwise. The whole
    /// operation holds `image_lock` so concurrent creates/refills never race
    /// a download; it NEVER touches `inner`, so VM state stays responsive
    /// during a long pull. `image` is pre-validated by the caller's entry
    /// point (server handler / pool spec validation).
    pub async fn ensure_image(&self, image_name: &str, expected_sha: Option<&str>) -> Result<(), String> {
        let path = format!("{}/images/{}.ext4", self.data_dir, image_name);
        // Fast path, no lock: cached-and-trusted is pure filesystem reads
        // (a racing self-heal sidecar write is tmp+rename of identical
        // content — benign).
        if image::decide(&path, expected_sha, image_name).await? == image::ImageDecision::UseExisting {
            return Ok(());
        }
        // Slow path: serialize per image, then re-check under the lock —
        // another task may have completed the pull while we waited.
        let lock = {
            let mut m = self.image_locks.lock().await;
            Arc::clone(m.entry(image_name.to_string()).or_insert_with(|| Arc::new(Mutex::new(()))))
        };
        let _guard = lock.lock().await;
        match image::decide(&path, expected_sha, image_name).await? {
            image::ImageDecision::UseExisting => Ok(()),
            image::ImageDecision::Pull => {
                // decide() only returns Pull when an expected sha exists.
                let want = expected_sha.ok_or("pull without sha256 (internal)")?;
                self.pull_image(image_name, want, &path).await
            }
        }
    }

    /// Download the image to a dot-prefixed partial file, verify its sha256,
    /// then atomically move it (and its sidecar) into place. Token goes to
    /// curl via config-on-stdin — never argv.
    async fn pull_image(&self, image_name: &str, want_sha: &str, path: &str) -> Result<(), String> {
        let images_dir = format!("{}/images", self.data_dir);
        std::fs::create_dir_all(&images_dir).map_err(|e| e.to_string())?;
        let tmp = format!("{}/.{}.partial", images_dir, image_name);
        let _ = std::fs::remove_file(&tmp);

        let url = format!(
            "{}/api/v1/images/{}",
            self.control_plane.trim_end_matches('/'),
            image_name
        );
        info!(image = %image_name, "pulling image from control plane");
        image::curl_download(&url, &self.token, &tmp).await?;

        let got = image::sha256_of(&tmp).await?;
        if got != want_sha {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!(
                "image {} sha256 mismatch: got {} want {}",
                image_name, got, want_sha
            ));
        }

        // Verified: publish the sidecar FIRST (temp+rename), then move the
        // image into place. A crash between the two leaves sidecar-without-
        // image, which the missing-file path simply repulls; the reverse
        // order would leave image-without-sidecar — a state that used to pin
        // a stale image forever.
        image::write_sidecar(path, want_sha)?;
        std::fs::rename(&tmp, path)
            .map_err(|e| format!("rename {} -> {}: {}", tmp, path, e))?;
        info!(image = %image_name, sha = %want_sha, "image cached");
        Ok(())
    }

    /// Grow the per-instance rootfs copy to `disk_gb` GiB: extend the file
    /// with set_len (sparse, never truncates), repair with `e2fsck -fp`
    /// (exit 0 clean, 1/2 mean errors were fixed — acceptable), then
    /// `resize2fs` to fill the new size. Shrinks are rejected.
    async fn grow_rootfs(&self, path: &str, disk_gb: u64) -> Result<(), String> {
        let current = std::fs::metadata(path)
            .map_err(|e| format!("stat {}: {}", path, e))?
            .len();
        let Some(target) = image::resize_target(disk_gb, current)? else {
            return Ok(());
        };

        let f = std::fs::OpenOptions::new()
            .write(true)
            .open(path)
            .map_err(|e| format!("open {}: {}", path, e))?;
        f.set_len(target)
            .map_err(|e| format!("set_len {}: {}", path, e))?;
        drop(f);

        let status = tokio::process::Command::new("e2fsck")
            .arg("-fp")
            .arg(path)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .await
            .map_err(|e| format!("e2fsck spawn: {}", e))?;
        match status.code() {
            // 0 = clean, 1/2 = errors found and fixed — all safe to resize.
            Some(0) | Some(1) | Some(2) => {}
            c => return Err(format!("e2fsck {} failed (exit {:?})", path, c)),
        }

        let ok = tokio::process::Command::new("resize2fs")
            .arg(path)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .await
            .map(|s| s.success())
            .unwrap_or(false);
        if !ok {
            return Err(format!("resize2fs {} failed", path));
        }
        Ok(())
    }

    /// v4 P5.4 (ADR-0008 deferral): collect image-cache entries nothing
    /// references anymore. Referenced = the image of any VM record (in-flight
    /// `Creating` records are inserted BEFORE ensure_image runs, so creates
    /// are covered) or of any feluccad-pushed pool spec; `DEFAULT_IMAGE` is
    /// never collected (deploy-provisioned base). `min_age_secs` gates on
    /// mtime so a file being pulled or just published is never a candidate
    /// (tests pass 0). Each candidate is re-checked under its per-image
    /// download lock before the unlink; the remaining race (a create's
    /// lock-free decide() fast path between re-check and unlink) at worst
    /// fails that create with a retryable 502 — the retry repulls.
    pub async fn gc_images(&self, min_age_secs: u64) {
        let images_dir = format!("{}/images", self.data_dir);
        let entries = match std::fs::read_dir(&images_dir) {
            Ok(e) => e,
            Err(_) => return, // no cache yet
        };
        let now = std::time::SystemTime::now();
        for entry in entries.flatten() {
            let fname = entry.file_name().to_string_lossy().into_owned();
            // Only files this module created are ours to delete.
            let Some(image) = gc_image_for_entry(&fname) else {
                continue;
            };
            if image == DEFAULT_IMAGE {
                continue;
            }
            let old_enough = entry
                .metadata()
                .and_then(|m| m.modified())
                .ok()
                .and_then(|m| now.duration_since(m).ok())
                .map(|age| age.as_secs() >= min_age_secs)
                .unwrap_or(false);
            if !old_enough || self.image_referenced(&image).await {
                continue;
            }
            // Serialize with an in-flight pull of the same image, then
            // re-check: a create registers its VM record before its pull.
            let lock = {
                let mut m = self.image_locks.lock().await;
                Arc::clone(
                    m.entry(image.clone())
                        .or_insert_with(|| Arc::new(Mutex::new(()))),
                )
            };
            let _guard = lock.lock().await;
            if self.image_referenced(&image).await {
                continue;
            }
            if std::fs::remove_file(entry.path()).is_ok() {
                info!(file = %fname, "image gc: removed (unreferenced)");
            }
        }
    }

    /// True when any VM record or pool spec references `image`. The legacy
    /// config pool's image is DEFAULT_IMAGE, which the GC skips outright.
    async fn image_referenced(&self, image: &str) -> bool {
        let g = self.inner.lock().await;
        g.vms.iter().any(|v| v.image == image)
            || g.template_pools.iter().any(|s| s.image == image)
    }

    async fn spawn_and_configure(
        self: &Arc<Self>,
        id: &str,
        dir_path: &str,
        rootfs_path: &str,
        vcpus: u32,
        mem_mib: u64,
        slot: Option<u32>,
        ip: Option<&str>,
    ) -> Result<(i32, bool), String> {
        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        // Remove stale socket.
        let _ = std::fs::remove_file(&sock);

        let pid = self.spawn_firecracker(id, &sock, dir_path)?;

        // Poll the API socket up to 3000ms in 50ms steps.
        let deadline = Instant::now() + Duration::from_millis(3000);
        loop {
            if Path::new(&sock).exists() { break; }
            if Instant::now() >= deadline {
                kill_and_reap(pid); // don't leak the half-spawned FC
                return Err("SocketNeverAppeared".into());
            }
            sleep(Duration::from_millis(50)).await;
        }

        // Cold boot only attaches a vsock device when the guest-agent marker is
        // present in the rootfs image; absent marker → exactly the v2 sequence.
        let vsock_on = self.guest_agent_marker_present();
        if let Err(e) = self.configure_guest(&sock, dir_path, rootfs_path, vcpus, mem_mib, slot, ip, vsock_on).await {
            kill_and_reap(pid); // don't leak the half-configured FC
            return Err(e);
        }
        Ok((pid, vsock_on))
    }

    /// Start the Firecracker of the instance in `dir_path`, console to its
    /// `serial.log`, and hand its exit to a reaper task. Returns the pid.
    ///
    /// THE spawn site: cold boot (create, start), wake and fork all come
    /// through here, so that everything every VMM must get, it gets — there
    /// used to be three copies of this, and a fourth would have been the one
    /// that missed something (`test_there_is_exactly_one_firecracker_spawn_site`).
    fn spawn_firecracker(self: &Arc<Self>, id: &str, sock: &str, dir_path: &str) -> Result<i32, String> {
        let log_path = format!("{}/serial.log", dir_path);
        let log_out = open_serial_log(&log_path)
            .map_err(|e| format!("open serial.log: {}", e))?;
        // stdout and stderr each own a descriptor. Both used to be built from
        // ONE raw fd, so it was closed twice — and in a multi-threaded runtime
        // the number can be handed to another thread between the two closes,
        // whose connection or guest socket the second close then takes away.
        let log_err = log_out
            .try_clone()
            .map_err(|e| format!("dup serial.log: {}", e))?;

        // stdin null, stdout/stderr → serial.log.
        let child = tokio::process::Command::new(&self.fc_bin)
            .arg("--api-sock").arg(sock)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::from(log_out))
            .stderr(std::process::Stdio::from(log_err))
            .kill_on_drop(false) // ALWAYS false per spec
            .spawn()
            .map_err(|e| format!("spawn firecracker: {}", e))?;

        let pid = child.id().ok_or("NoPid")? as i32;
        // While `child` is still in this scope the process cannot have been
        // reaped, so the pid is certain to be this Firecracker's — the write
        // cannot land on a stranger. A failure costs the preference, not the VM.
        if let Err(e) = set_oom_score_adj(&self.proc_root, pid) {
            warn!(vm = %id, pid, err = %e, "could not set oom_score_adj on the new firecracker");
        }
        // Detached reaper: collects the exit status whenever FC dies (our
        // SIGKILL on sleep, guest shutdown, crash) so it never zombies, and
        // flags unexpected exits (pid still recorded) as state=error.
        // kill_on_drop is false, so dropping after wait never kills anything.
        let mgr = Arc::clone(self);
        let rid = id.to_string();
        tokio::spawn(async move {
            let mut child = child;
            let _ = child.wait().await;
            mgr.on_fc_exit(&rid, pid).await;
        });
        Ok(pid)
    }

    /// True when `{data_dir}/images/.felucca-guest-v1` exists: the rootfs has the
    /// guest agent baked in, so cold boots should attach a vsock device.
    fn guest_agent_marker_present(&self) -> bool {
        let marker = format!("{}/images/.felucca-guest-v1", self.data_dir);
        Path::new(&marker).exists()
    }

    #[allow(clippy::too_many_arguments)]
    async fn configure_guest(
        &self,
        sock: &str,
        dir_path: &str,
        rootfs_path: &str,
        vcpus: u32,
        mem_mib: u64,
        slot: Option<u32>,
        ip: Option<&str>,
        vsock_on: bool,
    ) -> Result<(), String> {
        let kernel = format!("{}/kernels/vmlinux", self.data_dir);

        let (gw, mask) = if self.net_on {
            (crate::ipalloc::fmt_ip(self.cidr.gateway()), crate::ipalloc::fmt_ip(self.cidr.mask()))
        } else {
            (String::new(), String::new())
        };

        fc::put_boot_source(sock, &kernel, self.net_on, ip, &gw, &mask).await
            .map_err(|e| e.to_string())?;
        fc::put_drive_rootfs(sock, rootfs_path).await
            .map_err(|e| e.to_string())?;

        if self.net_on {
            if let Some(s) = slot {
                // The MAC the tap's anti-spoof chain matches on, from the same
                // derivation the pin used. It must be set here or the
                // `ether saddr` pin would drop every frame.
                let (tap, mac) = tap_pin_for_slot(s);
                fc::put_network_interface(sock, &tap, &mac).await
                    .map_err(|e| e.to_string())?;
            }
        }

        fc::put_machine_config(sock, vcpus, mem_mib).await
            .map_err(|e| e.to_string())?;
        // v3: attach the vsock device after /machine-config and before InstanceStart.
        if vsock_on {
            let uds_path = format!("{}/v.sock", dir_path);
            let _ = std::fs::remove_file(&uds_path);
            fc::put_vsock(sock, 3, &uds_path).await
                .map_err(|e| e.to_string())?;
        }
        fc::put_instance_start(sock).await
            .map_err(|e| e.to_string())?;
        Ok(())
    }

    // ---- pause / resume ----

    pub async fn pause(&self, id: &str) -> Result<(), OpError> {
        let _op = self.begin_op(id, OpKind::Pause).await?;
        // A VM whose Firecracker never answered at start-up may be running
        // under a record that says paused. Then "already paused" is not an
        // answer: the request has to reach the VMM.
        let unsettled = self.claim_unsettled(id).await;
        let pid = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Paused if !unsettled => return Ok(()), // idempotent
                VmState::Paused | VmState::Running => {}
                _ => return Err(OpError::invalid(&v.state, format!("InvalidState: {}", v.state.as_str()))),
            }
            v.pid
        };
        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        fc::patch_vm_state(&sock, "Paused").await.map_err(|e| e.to_string())?;
        self.commit_run_state(id, pid, VmState::Paused).await?;
        Ok(self.write_meta(id).await?)
    }

    pub async fn resume(&self, id: &str) -> Result<(), OpError> {
        let _op = self.begin_op(id, OpKind::Resume).await?;
        // As in `pause`. The usual case: a guest left paused by a sleep or a
        // fork the previous agent did not finish, under a record that still
        // says running — where answering "already running" would leave it
        // frozen.
        let unsettled = self.claim_unsettled(id).await;
        let pid = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Running if !unsettled => return Ok(()), // idempotent
                VmState::Running | VmState::Paused => {}
                _ => return Err(OpError::invalid(&v.state, format!("InvalidState: {}", v.state.as_str()))),
            }
            v.pid
        };
        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        fc::patch_vm_state(&sock, "Resumed").await.map_err(|e| e.to_string())?;
        self.commit_run_state(id, pid, VmState::Running).await?;
        Ok(self.write_meta(id).await?)
    }

    /// The commit point of pause and resume: the record takes the new run
    /// state, unless a stop or delete came in, or the VMM the operation
    /// talked to is no longer the record's (`may_commit`).
    async fn commit_run_state(&self, id: &str, pid: Option<i32>, state: VmState) -> Result<(), OpError> {
        let mut g = self.inner.lock().await;
        let v = g.vms.iter_mut().find(|v| v.id == id).ok_or("NotFound")?;
        if !self.may_commit(v, pid) {
            return Err(aborted(id));
        }
        v.state = state;
        Ok(())
    }

    // ---- sleep / wake ----

    /// Put a running or paused VM to sleep: its state and memory go to
    /// `vmstate.bin` and `mem.bin`, and its Firecracker goes away.
    ///
    /// The order is what makes a `sleeping` record on disk mean "the snapshot
    /// beside me is complete and durable" (FLU-4, FLU-70): pause; snapshot,
    /// the state file under its temporary name; fsync the disk image; rename
    /// the state file; fsync the directory; record `sleeping` with the
    /// snapshot marker, written; only THEN the VMM is killed. Killed first, an
    /// agent that stopped before the write left a `running` record with a
    /// dead VMM — which start-up now turns into `stopped`, throwing a complete
    /// snapshot away.
    ///
    /// Every failure before the kill is undone with the guest still alive
    /// (`undo_snapshot`): a failed `snapshot/create` leaves a complete-looking
    /// state file and a full-size, partly written memory file (lab), and the
    /// guest paused under a record that says `running`.
    pub async fn sleep_vm(&self, id: &str) -> Result<(), OpError> {
        let _op = self.begin_op(id, OpKind::Sleep).await?;
        self.claim_unsettled(id).await;
        let (was, pid, dir_id) = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Sleeping => return Ok(()), // idempotent
                VmState::Running | VmState::Paused => {}
                _ => {
                    return Err(OpError::invalid(
                        &v.state,
                        format!("InvalidState: {} (only a running or paused VM can sleep)", v.state.as_str()),
                    ))
                }
            }
            (v.state.clone(), v.pid, v.dir_id.clone())
        };

        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        let dir_path = self.instance_dir(id).await.ok_or("InvalidId")?;
        // A new snapshot is on its way: one set aside as stale is of no use
        // any more (S3'), and its memory file takes the space this one needs.
        self.remove_outrun(id, &dir_id);
        // The image the snapshot goes with is the one Firecracker has open as
        // the root drive (K7): for every sandbox created or forked by this
        // release that is its own `rootfs.ext4`; one forked before runs on its
        // parent's, and that is the one whose writes must be on disk.
        let own_rootfs = format!("{}/rootfs.ext4", dir_path);
        let rootfs = fc::get_root_drive_path(&sock).await.map_err(|e| e.to_string())?;
        if rootfs != own_rootfs {
            warn!(vm = %id, image = %rootfs, "sleep: this sandbox runs on another image than its own rootfs.ext4 (it was forked before this release); that image is synced");
        }

        fc::patch_vm_state(&sock, "Paused").await.map_err(|e| e.to_string())?;
        // Only a guest this sleep paused is resumed when it fails: a sandbox
        // that was `paused` before stays paused, as its record says.
        let paused_here = was == VmState::Running;

        // The state file is written under a name nothing takes for a
        // snapshot, and gets its real name only once Firecracker said the
        // whole snapshot was written. (`mem.bin` is written in place: a woken
        // guest runs on a private mapping of it, which Firecracker supports
        // writing over.)
        let vmstate_tmp = format!("{}/{}", dir_path, VMSTATE_TMP);
        let vmstate = format!("{}/{}", dir_path, VMSTATE);
        let mem = format!("{}/{}", dir_path, MEM);
        if let Err(e) = fc::put_snapshot_create(&sock, &vmstate_tmp, &mem).await {
            return Err(self.undo_snapshot(id, &dir_id, &sock, paused_here, pid, e.to_string()).await);
        }

        // Durable before it is acknowledged. Firecracker fsyncs both snapshot
        // files itself; what it does not sync is the disk image the snapshot
        // goes with, and the directory entry the rename makes. Without these,
        // a power cut after the answer can bring back a `sleeping` record
        // whose state file is missing, or whose disk is older than its memory.
        // On a blocking thread: an fsync of a multi-GiB image takes seconds.
        let durable = {
            let (dir, tmp, dst) = (dir_path.clone(), vmstate_tmp.clone(), vmstate.clone());
            tokio::task::spawn_blocking(move || -> Result<(), String> {
                std::fs::File::open(&rootfs)
                    .and_then(|f| f.sync_all())
                    .map_err(|e| format!("fsync {}: {}", rootfs, e))?;
                std::fs::rename(&tmp, &dst).map_err(|e| format!("rename {} -> {}: {}", tmp, dst, e))?;
                std::fs::File::open(&dir)
                    .and_then(|f| f.sync_all())
                    .map_err(|e| format!("fsync {}: {}", dir, e))
            })
            .await
            .unwrap_or_else(|e| Err(format!("sleep: fsync task: {}", e)))
        };
        if let Err(e) = durable {
            return Err(self.undo_snapshot(id, &dir_id, &sock, paused_here, pid, e).await);
        }

        // The commit point. The pid stays in the record for the moment: the
        // VMM is still there, and is not killed before the record is on disk.
        {
            let mut g = self.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or("NotFound")?;
            if !self.may_commit(v, pid) {
                drop(g);
                // Whoever took the VMM (a stop, the exit reaper) does not
                // want this snapshot either.
                self.discard_snapshot_files(id, &dir_id);
                return Err(aborted(id));
            }
            v.state = VmState::Sleeping;
            v.snapshot_valid = true;
        }
        if let Err(e) = self.write_meta(id).await {
            // Not on disk, so not asleep: the record goes back to what it
            // was (the meta.json write owed from here then writes that), and
            // the snapshot is undone with the guest still alive — the state
            // file first, so that nothing, not even a `sleeping` file that
            // did land before a failed directory fsync, can be woken from it.
            {
                let mut g = self.inner.lock().await;
                if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                    if v.pid == pid && v.state == VmState::Sleeping {
                        v.state = was;
                        v.snapshot_valid = false;
                        // Owed from this same section: the failed write's own
                        // claim on the list can have been settled meanwhile
                        // by another writer (the liveness flush, an expose)
                        // that wrote `sleeping` — leaving it on disk under a
                        // record that no longer says so.
                        lock_std(&self.meta_owed).insert(id.to_string());
                    }
                }
            }
            return Err(self.undo_snapshot(id, &dir_id, &sock, paused_here, pid, e).await);
        }

        // The record is on disk: from here the VM is asleep. The record loses
        // its pid and the VMM is told to go in ONE step with no await in it.
        // Clearing the pid before the kill makes the exit reaper see a pid
        // mismatch and treat this as a deliberate kill (no error transition).
        // The pid-less record is owed to meta.json from this same step, so a
        // caller that goes away before the write below cannot leave the
        // file naming a dead VMM.
        let (dying, dir_id) = {
            let mut g = self.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or("NotFound")?;
            // A stop that came in since the commit has taken the pid,
            // signalled the VMM and recorded `stopped`: this sleep did not
            // happen, and answering 200 would report a sandbox asleep that is
            // not. (Its snapshot is the stop's to remove, once this lets go.)
            if v.state != VmState::Sleeping || v.pid != pid {
                return Err(aborted(id));
            }
            let p = v.pid.take();
            lock_std(&self.meta_owed).insert(id.to_string());
            (p, v.dir_id.clone())
        };
        let dying = dying.filter(|p| self.kill_fc(*p, &dir_id));

        if let Err(e) = self.write_meta(id).await {
            // The file already says `sleeping`; only the pid in it is stale,
            // and start-up ignores the pid of a sleeping record.
            warn!(vm = %id, err = %e, "sleep: could not write the record without its pid yet");
        }
        if let Some(p) = dying {
            // Waits for the process to be gone even when it is not this
            // agent's child (a VMM adopted at start-up): the wake that follows
            // needs the tap and the sockets it holds.
            self.wait_fc_gone(p, &dir_id).await;
        }
        Ok(())
    }

    /// Undo a sleep's snapshot that failed before its VMM was killed (F2):
    /// the snapshot files go — `mem.bin` only when no live VMM of the
    /// instance maps it (a woken guest runs on it) — and the guest is
    /// resumed if this operation paused it. The record is not changed, except
    /// to `paused` when that resume fails too. Returns `err` as the answer.
    ///
    /// Resumed, the guest works at once (lab: exec 30 of 30 after a failed
    /// create and a resume); left paused under a record that says `running`,
    /// nothing would ever resume it.
    async fn undo_snapshot(
        &self,
        id: &str,
        dir_id: &str,
        sock: &str,
        paused_here: bool,
        pid: Option<i32>,
        err: String,
    ) -> OpError {
        warn!(vm = %id, err = %err, "the snapshot failed; its files are removed and the guest is left as it was");
        // The state files first: they are small, and they are what makes a
        // snapshot look valid. Then the guest is given back, and only then
        // `mem.bin` is dealt with — the `/proc` scan and the unlink of a
        // multi-GiB file used to keep the guest paused for their length.
        if let Some(dir) = instance_path(&self.data_dir, dir_id) {
            remove_if_present(id, &format!("{}/{}", dir, VMSTATE_TMP));
            remove_if_present(id, &format!("{}/{}", dir, VMSTATE));
        }
        if paused_here {
            self.resume_or_record_paused(id, sock, pid).await;
        }
        self.discard_snapshot_files(id, dir_id);
        OpError::Other(err)
    }

    /// Resume a guest an operation paused and is now giving back. When the
    /// resume fails the guest stays paused, and the record says so, on disk:
    /// `running` would be a guest nobody can reach that nothing resumes. Not
    /// when the record no longer holds the VMM `pid` or the sandbox is being
    /// stopped or deleted (`may_commit`): the record is not this
    /// operation's to write any more. Returns whether the guest was resumed.
    async fn resume_or_record_paused(&self, id: &str, sock: &str, pid: Option<i32>) -> bool {
        let Err(e) = fc::patch_vm_state(sock, "Resumed").await else {
            return true;
        };
        let changed = {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.state == VmState::Running && self.may_commit(v, pid) => {
                    v.state = VmState::Paused;
                    true
                }
                _ => false,
            }
        };
        warn!(vm = %id, err = %e, "could not resume the guest this operation paused; it stays paused");
        if changed {
            if let Err(we) = self.write_meta(id).await {
                warn!(vm = %id, err = %we, "could not write the paused record yet");
            }
        }
        false
    }

    /// Spawn firecracker → snapshot/load → record `running` → resume.
    /// Returns wake latency in ms.
    ///
    /// The record says `running`, on disk, before the guest executes again
    /// (FLU-4): a guest that runs writes to its disk, and a record that still
    /// said `sleeping` after that would hand the next start-up a snapshot the
    /// disk has outrun. Hence the load without resume, the record, and only
    /// then the resume.
    pub async fn wake_vm(self: &Arc<Self>, id: &str) -> Result<u64, OpError> {
        let _op = self.begin_op(id, OpKind::Wake).await?;
        let (slot, marker, pid0, dir_id) = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Running => return Ok(0), // idempotent
                VmState::Sleeping => {}
                _ => {
                    return Err(OpError::invalid(
                        &v.state,
                        format!("InvalidState: {} (only a sleeping VM can be woken)", v.state.as_str()),
                    ))
                }
            }
            (v.slot, v.snapshot_valid, v.pid, v.dir_id.clone())
        };
        // W3: no guest back onto a network whose fences are positively gone.
        self.refuse_new_guest("wake", id).await.map_err(OpError::Other)?;

        let start = Instant::now();
        let dir_path = instance_path(&self.data_dir, &dir_id).ok_or("InvalidId")?;
        let vmstate = format!("{}/{}", dir_path, VMSTATE);
        let mem = format!("{}/{}", dir_path, MEM);

        // Both snapshot files, before anything is spawned. A file that cannot
        // even be looked at changes nothing (a failed stat is not a missing
        // snapshot); one that is missing or empty means there is nothing to
        // wake.
        let mut incomplete: Option<&str> = None;
        for (name, path) in [(VMSTATE, &vmstate), (MEM, &mem)] {
            match check_file(path) {
                FileCheck::There(len) if len > 0 => {}
                FileCheck::There(_) | FileCheck::Missing => {
                    incomplete.get_or_insert(name);
                }
                FileCheck::Unknown(e) => {
                    return Err(OpError::Other(format!("cannot examine {}: {}", path, e)));
                }
            }
        }
        if let Some(name) = incomplete {
            return Err(self.discard_incomplete_snapshot(id, pid0, name).await);
        }
        // A record without the marker was written by an older agent: the
        // judgement start-up makes of it (S3) is made again here. A record
        // start-up could not judge ("cannot tell") stayed `sleeping`, and
        // nothing else would ever look at its disk before waking it.
        if !marker {
            match reconcile::judge_sleeper(&dir_path, false) {
                reconcile::Sleeper::Keep => {}
                reconcile::Sleeper::Incomplete(name) => {
                    return Err(self.discard_incomplete_snapshot(id, pid0, name).await);
                }
                reconcile::Sleeper::Outrun { disk, snapshot } => {
                    let reason = format!(
                        "InvalidState: the disk was written after the snapshot (rootfs.ext4 {}, vmstate.bin {}): \
                         the guest ran since; the VM is stopped (use start)",
                        reconcile::epoch(disk),
                        reconcile::epoch(snapshot)
                    );
                    return Err(self.stop_stale_wake(id, pid0, None, reason).await);
                }
                reconcile::Sleeper::CannotTell(e) => {
                    return Err(OpError::Other(format!("cannot examine the snapshot of {}: {}", id, e)));
                }
            }
        }

        // Re-assert tap exists (idempotent) and re-pin it, MAC included: the
        // nft chains do not survive a host reboot.
        //
        // The MAC is safe to pin here because the slot survives sleep/wake
        // (read from the record above, and `put_snapshot_load`'s network
        // override only renames the host device), so the NIC restored from the
        // snapshot still carries the MAC `configure_guest` gave it at cold
        // boot — the same `guest_mac_for_slot(slot)`. This used to pass "" and
        // render no `ether saddr` rule, which made sleep/wake a tenant-driven
        // API for putting one's own VM into an L2-unpinned state and then
        // sourcing frames as a victim to steal their inbound traffic.
        //
        // A snapshot taken by a pre-pin agent carries a Firecracker-chosen MAC
        // instead and goes dark on wake until a stop/start cold-boots it onto
        // the derived one. Dark is the correct direction for that trade.
        // Through the rule seam (`pin_tap`), so that unit tests run the pin.
        if self.rules_on() {
            if let Some(s) = slot {
                let (tap, pin_ip, mac) = {
                    let g = self.inner.lock().await;
                    self.tap_pin(&g, s)
                };
                if !self.rules.pin_tap(&tap, pin_ip.as_deref(), &mac) {
                    return Err("TapSetupFailed".into());
                }
                // X4: pinned and up — not the watch's to hold down any more.
                lock_std(&self.watch).release_tap(&tap);
            }
        }

        let sock = format!("{}/{}", dir_path, procscan::SOCK_NAME);
        let _ = std::fs::remove_file(&sock);
        // A SIGKILLed FC (sleep) leaves the vsock host UDS behind; the restore
        // re-binds the snapshot-baked path and fails with EADDRINUSE if the
        // stale file is still there.
        let _ = std::fs::remove_file(format!("{}/v.sock", dir_path));

        let pid = self.spawn_firecracker(id, &sock, &dir_path)?;

        // Poll socket.
        let deadline = Instant::now() + Duration::from_millis(3000);
        loop {
            if Path::new(&sock).exists() { break; }
            if Instant::now() >= deadline {
                kill_and_reap(pid); // don't leak the half-spawned FC
                return Err("SocketNeverAppeared".into());
            }
            sleep(Duration::from_millis(50)).await;
        }

        // Wake restores into the VM's OWN dir, so the baked vsock uds_path is
        // already correct — no override needed (None re-binds the original
        // path). Loaded paused: the guest does not run until the record says so.
        if let Err(e) = fc::put_snapshot_load(&sock, &vmstate, &mem, self.net_on, slot, None, false).await {
            kill_and_reap(pid); // don't leak the FC that failed to restore
            return Err(e.to_string().into());
        }

        // Which image the loaded guest's disk is (K6). A sandbox forked
        // before this release runs on its PARENT's image (the snapshot names
        // it, and nothing re-pointed it). That image may have been written
        // since this snapshot by the parent; waking would then pair this
        // memory with a disk that moved on without it.
        let own_rootfs = format!("{}/rootfs.ext4", dir_path);
        let drive = match fc::get_root_drive_path(&sock).await {
            Ok(p) => p,
            Err(e) => {
                kill_and_reap(pid); // never ran
                return Err(e.to_string().into());
            }
        };
        if drive != own_rootfs {
            match modified_after(&drive, &vmstate) {
                Ok(false) => warn!(
                    vm = %id,
                    image = %drive,
                    "wake: this sandbox runs on another image than its own rootfs.ext4 (it was forked before this \
                     release); that image has not been written since the snapshot, so it is woken on it"
                ),
                Ok(true) => {
                    // The path goes to the log only: the control plane
                    // relays the answer to the tenant word for word.
                    warn!(vm = %id, image = %drive,
                        "wake refused: this sandbox runs on another image than its own rootfs.ext4 (it was forked \
                         before this release), and that image was written after the snapshot; it is stopped");
                    return Err(self.stop_stale_wake(id, pid0, Some(pid), WAKE_REFUSED_FORKED_BEFORE.into()).await);
                }
                Err(e) => {
                    kill_and_reap(pid); // never ran
                    return Err(OpError::Other(format!("cannot examine {}: {}", drive, e)));
                }
            }
        }

        // The commit point: the record takes the new VMM and says `running`.
        {
            let mut g = self.inner.lock().await;
            let committed = match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.state == VmState::Sleeping && self.may_commit(v, pid0) => {
                    v.pid = Some(pid);
                    v.state = VmState::Running;
                    v.snapshot_valid = false;
                    true
                }
                _ => false,
            };
            if !committed {
                drop(g);
                kill_and_reap(pid); // never ran: nothing of the guest is lost
                return Err(aborted(id));
            }
        }
        if let Err(e) = self.write_meta(id).await {
            // The file still says `sleeping`, which is true: the guest has not
            // run. Memory goes back to that, without the VMM.
            self.unwind_wake(id, pid, marker).await;
            return Err(e.into());
        }

        match fc::patch_vm_state_answer(&sock, "Resumed").await {
            Ok((status, _)) if status < 300 => {}
            answer => {
                let (answered, err) = match answer {
                    Ok((status, body)) => (true, format!("FcError: PATCH /vm -> {}: {}", status, body)),
                    Err(e) => (false, e.to_string()),
                };
                // Back to `sleeping` only on proof that the guest did not
                // run (S7'): Firecracker refused the resume, and says the VM
                // is still paused. Without an answer the request may well
                // have reached it and the vCPUs run before the VMM died or
                // the reply was lost — and waking that snapshot again would
                // restore old memory over a disk the guest has written.
                if answered && probe_vmm(&sock).await.as_deref() == Some("Paused") {
                    if self.unwind_wake(id, pid, marker).await {
                        if let Err(we) = self.write_meta(id).await {
                            warn!(vm = %id, err = %we, "wake: could not write the record back to sleeping yet");
                        }
                    }
                    return Err(err.into());
                }
                let reason = format!(
                    "InvalidState: the resume failed ({}) and the guest may have run; the VM is stopped (use start)",
                    err
                );
                return Err(self.stop_stale_wake(id, Some(pid), Some(pid), reason).await);
            }
        }
        let elapsed = start.elapsed().as_millis() as u64;

        // The guest runs: its state file describes a past it has left. mem.bin
        // stays — the guest runs on a private mapping of it.
        remove_if_present(id, &vmstate);
        Ok(elapsed)
    }

    /// Undo a wake whose record named the new VMM `pid`: the record goes back
    /// to `sleeping` with the snapshot marker it had, without a VMM, and the
    /// VMM is killed — pid first, then the signal, with no await between, so
    /// the exit reaper takes it for a deliberate kill. Returns false, and
    /// does nothing, when the record no longer names `pid` (a stop took it
    /// and ended the VMM itself).
    async fn unwind_wake(&self, id: &str, pid: i32, marker: bool) -> bool {
        {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.pid == Some(pid) => {
                    v.pid = None;
                    v.state = VmState::Sleeping;
                    v.snapshot_valid = marker;
                    // Owed from this same section, as in sleep's revert: a
                    // writer that settled the failed write's claim meanwhile
                    // wrote `running` with this pid.
                    lock_std(&self.meta_owed).insert(id.to_string());
                }
                _ => return false,
            }
        }
        kill_and_reap(pid);
        true
    }

    /// A wake found its snapshot stale, or possibly so (S3', S7', K6): the
    /// guest ran after it was taken, or may have. The record becomes
    /// `stopped`, on disk too; the VMM this wake spawned (`vmm`) is killed —
    /// the record lets go of it first, so the exit reaper takes it for a
    /// deliberate kill; the snapshot files are set aside, not removed
    /// (`set_snapshot_aside`). The answer is the 409 that says `stopped`
    /// (`reason`). Not when the record no longer holds `expect` or the
    /// sandbox is being stopped or deleted (`may_commit`): the record is not
    /// this wake's to write any more, and only the VMM goes.
    async fn stop_stale_wake(&self, id: &str, expect: Option<i32>, vmm: Option<i32>, reason: String) -> OpError {
        let dir_id = {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if self.may_commit(v, expect) => {
                    v.state = VmState::Stopped;
                    v.pid = None;
                    v.snapshot_valid = false;
                    lock_std(&self.meta_owed).insert(id.to_string());
                    Some(v.dir_id.clone())
                }
                _ => None,
            }
        };
        if let Some(p) = vmm {
            kill_and_reap(p);
        }
        let Some(dir_id) = dir_id else {
            return aborted(id);
        };
        warn!(vm = %id, reason = %reason, "wake: the snapshot is stale or may be; the VM is stopped and its snapshot set aside");
        self.set_snapshot_aside(id, &dir_id);
        if let Err(e) = self.write_meta(id).await {
            warn!(vm = %id, err = %e, "wake: could not write the stopped record yet");
        }
        OpError::invalid(&VmState::Stopped, reason)
    }

    /// A wake found a snapshot file missing or empty (`missing` names it):
    /// there is nothing to wake. The record becomes `stopped` on disk, the
    /// rest of the snapshot goes, and the answer is the 409 that says so.
    async fn discard_incomplete_snapshot(&self, id: &str, pid: Option<i32>, missing: &str) -> OpError {
        let dir_id = {
            let mut g = self.inner.lock().await;
            let Some(v) = g.vms.iter_mut().find(|v| v.id == id) else {
                return OpError::Other("NotFound".into());
            };
            if v.state != VmState::Sleeping || !self.may_commit(v, pid) {
                return aborted(id);
            }
            v.state = VmState::Stopped;
            v.snapshot_valid = false;
            v.dir_id.clone()
        };
        warn!(vm = %id, file = %missing, "wake: the snapshot is incomplete; the sandbox is stopped and what is left of the snapshot is removed");
        if let Err(e) = self.write_meta(id).await {
            warn!(vm = %id, err = %e, "wake: could not write the stopped record yet");
        }
        self.discard_snapshot_files(id, &dir_id);
        OpError::invalid(
            &VmState::Stopped,
            format!("InvalidState: snapshot incomplete ({} missing or empty); the VM is stopped (use start)", missing),
        )
    }

    // ---- fork ----

    /// Fork the parent: reserve the child, write (running or paused parent)
    /// or copy (sleeping parent) a snapshot into the child's own directory,
    /// reflink the rootfs, spawn the child's FC and restore. Child inherits
    /// parent's guest-internal IP (v2 documented caveat; fixed in v3).
    /// `Err("AlreadyExists")` (→ 409) when the child id or its directory is
    /// taken, before anything is written.
    ///
    /// The plain-text form the unit tests use; the handler takes
    /// `fork_answer`. (Not `cfg(test)`: the structural tests read this
    /// file's production half up to its first test-only attribute.)
    #[cfg_attr(not(test), allow(dead_code))]
    pub async fn fork(self: &Arc<Self>, parent_id: &str, child_id: &str, child_name: &str) -> Result<Option<String>, String> {
        self.fork_inner(parent_id, child_id, child_name, &mut None).await
    }

    /// `fork`, answered the way sleep, wake, stop and start answer: `Busy`;
    /// a refusal for the parent's state as `InvalidState` carrying the state
    /// the agent holds for the parent when the refusal is built (the control
    /// plane records it); everything else `Other`, verbatim.
    pub async fn fork_answer(
        self: &Arc<Self>,
        parent_id: &str,
        child_id: &str,
        child_name: &str,
    ) -> Result<Option<String>, OpError> {
        let mut refused = None;
        match self.fork_inner(parent_id, child_id, child_name, &mut refused).await {
            Ok(ip) => Ok(ip),
            Err(e) if e == BUSY => Err(OpError::Busy),
            Err(e) => Err(match refused {
                Some(state) => OpError::InvalidState { msg: e, state },
                None => OpError::Other(e),
            }),
        }
    }

    /// The parent's state as the record says now, or `fallback`.
    async fn state_now(&self, id: &str, fallback: &VmState) -> VmState {
        let g = self.inner.lock().await;
        g.vms.iter().find(|v| v.id == id).map(|v| v.state.clone()).unwrap_or_else(|| fallback.clone())
    }

    /// The body of `fork`. A refusal for the parent's state also sets
    /// `refused` to that state.
    async fn fork_inner(
        self: &Arc<Self>,
        parent_id: &str,
        child_id: &str,
        child_name: &str,
        refused: &mut Option<VmState>,
    ) -> Result<Option<String>, String> {
        if !valid_vm_id(child_id) {
            return Err("InvalidId".into());
        }
        // The child id comes from the request BODY, not a URL capture, so it
        // is validated here as well as at the handler.
        let child_dir = instance_path(&self.data_dir, child_id).ok_or("InvalidId")?;
        // The parent is held until the child's files are complete: a sleep,
        // wake, stop or delete of the parent in between would move or remove
        // the parent's files under the copy, or kill the VMM being snapshot.
        // A parent that is busy answers busy at once, and nothing is reserved.
        // W3, W11: before anything is reserved, and long before the pause.
        self.refuse_new_guest("fork", child_id).await?;
        let mut parent_op = self.begin_fork(parent_id).map_err(|e| e.to_string())?;
        // Fork pauses, snapshots and resumes the parent itself.
        self.claim_unsettled(parent_id).await;

        // The child id is reserved before anything is written (F4), in the
        // same critical section that reads the parent. It used to be checked
        // only after the files were written — and a fork onto an existing id
        // had by then copied the parent's disk and snapshot over that
        // sandbox's own. A pool VM that was claimed lives in a directory
        // named after its pool id, so a directory id counts as taken too.
        let (parent, _child_op) = {
            let mut g = self.inner.lock().await;
            let p = g.vms.iter().find(|v| v.id == parent_id).ok_or("NotFound")?;
            // Only a guest that exists can be forked: a `pooled` or
            // `creating` parent whose VMM is alive used to be snapshotted
            // too. Refused before anything is reserved.
            if !matches!(p.state, VmState::Running | VmState::Paused | VmState::Sleeping) {
                *refused = Some(p.state.clone());
                return Err(format!(
                    "InvalidState: {} (only a running, paused or sleeping VM can be forked)",
                    p.state.as_str()
                ));
            }
            let child = Vm {
                id: child_id.to_string(),
                name: child_name.to_string(),
                dir_id: child_id.to_string(),
                vcpus: p.vcpus,
                mem_mib: p.mem_mib,
                state: VmState::Creating,
                pid: None,
                slot: None,
                ip: None,
                // The snapshot carries the parent's vsock device; the child
                // inherits it (rebound to its own UDS via vsock_override below).
                vsock: p.vsock,
                // Child inherits parent's tenant_id.
                tenant_id: p.tenant_id.clone(),
                // Exposes are NOT inherited: the parent's node ports keep
                // pointing at the parent; feluccad re-exposes the child.
                exposes: Vec::new(),
                // The child's rootfs is a reflink of the parent's — same
                // image and disk size for accounting (no image interaction).
                image: p.image.clone(),
                image_sha256: p.image_sha256.clone(),
                disk_gb: p.disk_gb,
                snapshot_valid: false,
            };
            let parent = (
                p.state.clone(),
                p.pid,
                Self::instance_dir_sync(&g.vms, &self.data_dir, parent_id).ok_or("InvalidId")?,
                Self::sock_path_sync(&g.vms, &self.data_dir, parent_id).ok_or("InvalidId")?,
                p.vsock,
            );
            if g.vms.iter().any(|v| v.id == child_id || v.dir_id == child_id) {
                return Err("AlreadyExists".into());
            }
            // A parent that is asleep is only read from: other forks of it
            // may run alongside this one (S4'). A stop or delete of it that
            // came in since this fork joined them has the last word.
            if parent.0 == VmState::Sleeping {
                parent_op.share();
            }
            if self.closing(parent_id) {
                return Err(BUSY.into());
            }
            // The child is this fork's from its reservation to the end
            // (S4'): a stop or delete of it meanwhile pre-empts the fork (it
            // kills the child's VMM, and the fork aborts at its next check)
            // instead of racing it.
            let child_op = self.begin_fork(child_id).map_err(|e| e.to_string())?;
            g.vms.push(child);
            (parent, child_op)
        };
        let (parent_state, parent_pid, parent_dir, parent_sock, parent_vsock) = parent;

        // The child's directory is its own from the start: created
        // exclusively. One that exists although no record owns it can be a
        // VM whose meta.json did not load at start-up, still running out of
        // it — it is never written into.
        if let Err(e) = create_new_instance_dir(&child_dir) {
            self.drop_unborn_child(child_id, None, None).await;
            if e.kind() == std::io::ErrorKind::AlreadyExists {
                warn!(vm = %child_id, "fork: the child's directory exists although no record owns it; left alone");
                return Err("AlreadyExists".into());
            }
            return Err(format!("create {}: {}", child_dir, e));
        }

        // The child's network slot, before the parent is touched: a node out
        // of addresses used to pause the parent and write the whole snapshot
        // first, for nothing. From here every failure gives it back. And the
        // reservation is looked at once more: a stop or delete of the child
        // that came in meanwhile ends the fork here.
        let (child_slot, child_ip) = {
            let mut g = self.inner.lock().await;
            if !self.child_reserved(&g, child_id) {
                drop(g);
                self.drop_unborn_child(child_id, Some(&child_dir), None).await;
                return Err(aborted(child_id).to_string());
            }
            if self.rules_on() {
                let s = match Self::claim_slot(&mut g) {
                    Ok(s) => s,
                    Err(e) => {
                        drop(g);
                        self.drop_unborn_child(child_id, Some(&child_dir), None).await;
                        return Err(e.into());
                    }
                };
                (Some(s), Self::ip_for_slot(&g, s))
            } else {
                (None, None)
            }
        };

        let parent_rootfs = format!("{}/rootfs.ext4", parent_dir);
        let child_rootfs = format!("{}/rootfs.ext4", child_dir);
        let child_vmstate = format!("{}/{}", child_dir, VMSTATE);
        let child_mem_file = format!("{}/{}", child_dir, MEM);

        // 1) The child's snapshot. Written straight into the child's own
        // directory: the parent's directory gets no snapshot files (a
        // `vmstate.bin` there would say "asleep" to an older agent, S11), and
        // two forks of one parent no longer write over one pair of files.
        if parent_state == VmState::Sleeping {
            // Its snapshot is complete (the record says so), and it cannot
            // move while the parent is held.
            let parent_vmstate = format!("{}/{}", parent_dir, VMSTATE);
            let parent_mem_file = format!("{}/{}", parent_dir, MEM);
            for (src, dst) in [(&parent_rootfs, &child_rootfs), (&parent_vmstate, &child_vmstate), (&parent_mem_file, &child_mem_file)] {
                if let Err(e) = copy_rootfs(src, dst) {
                    self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                    return Err(e);
                }
            }
        } else {
            // Only a parent this fork paused is resumed by it: a `paused`
            // parent stays paused.
            let paused_here = parent_state == VmState::Running;
            // A parent that does not run on its own disk is not forked (K5):
            // a sandbox forked before this release runs on ITS parent's image,
            // and its own `rootfs.ext4` is the disk as it was at that fork —
            // copying that beside this memory would pair it with a stale disk.
            // Asked before the pause, so that such a fork costs nothing; the
            // child's own check after its load is the one that decides.
            match fc::get_root_drive_path(&parent_sock).await {
                Ok(p) if same_file(&p, &parent_rootfs).unwrap_or(false) => {}
                Ok(p) => {
                    // The path and the ids go to the log only: the control
                    // plane relays the answer to the tenant word for word.
                    warn!(vm = %parent_id, child = %child_id, image = %p,
                        "fork refused: the parent runs on another image than its own rootfs.ext4 (it was forked \
                         before this release)");
                    self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                    *refused = Some(self.state_now(parent_id, &parent_state).await);
                    return Err(FORK_REFUSED_FORKED_BEFORE.into());
                }
                Err(e) => {
                    self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                    return Err(e.to_string());
                }
            }
            if paused_here {
                if let Err(e) = fc::patch_vm_state(&parent_sock, "Paused").await {
                    self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                    return Err(e.to_string());
                }
            }
            // The state file under its temporary name, renamed once
            // Firecracker said the whole snapshot was written: a failed
            // create leaves a complete-looking state file behind.
            //
            // The disk is copied while the parent is still paused (FLU-72),
            // so that it is the disk of the same instant as the memory. Copied
            // after the resume, as it used to be, it held writes the child's
            // memory knows nothing of — a disk and a page cache that disagree.
            let child_vmstate_tmp = format!("{}/{}", child_dir, VMSTATE_TMP);
            let made = match fc::put_snapshot_create(&parent_sock, &child_vmstate_tmp, &child_mem_file).await {
                Ok(()) => match std::fs::rename(&child_vmstate_tmp, &child_vmstate) {
                    Ok(()) => self.copy_disk_in_pause(parent_id, &parent_rootfs, &child_rootfs).await,
                    Err(e) => Err(format!("rename {} -> {}: {}", child_vmstate_tmp, child_vmstate, e)),
                },
                Err(e) => Err(e.to_string()),
            };
            // The parent is given back first, the child's files removed
            // after: the parent need not stay paused for the unlink of a
            // multi-GiB memory file. If it cannot be resumed, its record says
            // `paused`.
            let resumed = !paused_here || self.resume_or_record_paused(parent_id, &parent_sock, parent_pid).await;
            if let Err(e) = made {
                warn!(vm = %parent_id, child = %child_id, err = %e, "fork: the snapshot failed; the child is removed");
                self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                return Err(e);
            }
            if !resumed {
                self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                return Err(format!("fork: {} could not be resumed after its snapshot and stays paused", parent_id));
            }
        }

        if let Some(s) = child_slot {
            // Pinned to the child's OWN MAC before its Firecracker is even
            // spawned, not after the in-guest re-MAC below: the clone restores
            // still carrying the parent's L2 identity, and a window in which it
            // can source frames under any MAC is exactly the hole. The pin
            // holds the clone's frames off the bridge until the guest takes the
            // MAC that matches it — and the re-MAC travels over vsock, not over
            // this tap, so it is unaffected.
            let (tap, pin_ip, mac) = {
                let g = self.inner.lock().await;
                self.tap_pin(&g, s)
            };
            if !self.rules.pin_tap(&tap, pin_ip.as_deref(), &mac) {
                return Err(self.fail_child(child_id, &child_dir, None, child_slot, "TapSetupFailed".into()).await);
            }
            // X4: pinned and up — not the watch's to hold down any more.
            lock_std(&self.watch).release_tap(&tap);
        }

        // 4) Spawn child FC and restore.
        let child_sock = format!("{}/fc.sock", child_dir);
        let _ = std::fs::remove_file(&child_sock);

        let child_pid = match self.spawn_firecracker(child_id, &child_sock, &child_dir) {
            Ok(p) => p,
            Err(e) => return Err(self.fail_child(child_id, &child_dir, None, child_slot, e).await),
        };

        let deadline = Instant::now() + Duration::from_millis(3000);
        loop {
            if Path::new(&child_sock).exists() { break; }
            if Instant::now() >= deadline {
                // Don't leak the half-spawned child FC.
                return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, "SocketNeverAppeared".into()).await);
            }
            sleep(Duration::from_millis(50)).await;
        }

        // If the parent had a vsock device, the snapshot's baked uds_path points
        // at the PARENT's absolute v.sock. On FC v1.16 that path is re-bound on
        // restore, which would collide with the still-running parent (EADDRINUSE).
        // `vsock_override` rebinds the device to the child's own UDS path.
        // (Validated on FC v1.16: load+vsock_override -> 204, child binds its own
        // v.sock, parent path untouched.)
        let child_vsock_uds = format!("{}/v.sock", child_dir);
        let vsock_override = if parent_vsock {
            let _ = std::fs::remove_file(&child_vsock_uds);
            Some(child_vsock_uds.as_str())
        } else {
            None
        };
        // Loaded paused: the snapshot names the PARENT's disk image (it
        // records the drive's absolute path, and the load has no override
        // for it), so the child must not run before its drive is its own.
        if let Err(e) = fc::put_snapshot_load(&child_sock, &child_vmstate, &child_mem_file, self.net_on, child_slot, vsock_override, false).await {
            // Don't leak the child FC that failed to restore.
            return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e.to_string()).await);
        }
        // Loaded: its state file is used up and describes nothing the child
        // will be (S11: `vmstate.bin` only beside a `sleeping` record).
        // mem.bin stays — the child runs on a private mapping of it.
        remove_if_present(child_id, &child_vmstate);

        // The image the loaded state names is the one the parent really ran
        // on when the snapshot was taken. It must be the parent's own
        // `rootfs.ext4` (K5) — the same file, by device and inode, not by the
        // spelling of its path. Otherwise the parent was forked before this
        // release and runs on its own parent's image; the copy made above is
        // the disk of an older instant than this memory.
        match fc::get_root_drive_path(&child_sock).await {
            Ok(p) if same_file(&p, &parent_rootfs).unwrap_or(false) => {}
            Ok(p) => {
                // A refusal like the running parent's (409, not 500: the
                // request can never succeed); the path and the ids are logged.
                warn!(vm = %parent_id, child = %child_id, image = %p,
                    "fork refused: the parent ran on another image than its own rootfs.ext4 when it was \
                     snapshotted (it was forked before this release); the child is not started");
                let e = self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, FORK_REFUSED_FORKED_BEFORE.into()).await;
                *refused = Some(self.state_now(parent_id, &parent_state).await);
                return Err(e);
            }
            Err(e) => return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e.to_string()).await),
        }

        // Its own disk (FLU-72), and checked: a child resumed on its parent's
        // image writes over the parent's filesystem from a second kernel.
        if let Err(e) = fc::patch_drive_rootfs(&child_sock, &child_rootfs).await {
            return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e.to_string()).await);
        }
        match fc::get_root_drive_path(&child_sock).await {
            Ok(p) if p == child_rootfs => {}
            Ok(p) => {
                let e = format!("fork: the child's drive is {} after the PATCH, not {}", p, child_rootfs);
                return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e).await);
            }
            Err(e) => return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e.to_string()).await),
        }
        // Everything from here is the child's. The parent was held until now
        // (FK-7): the child's load reads the parent's image until its drive
        // is re-pointed, and a delete of the parent that was waiting would
        // otherwise remove that image under it.
        drop(parent_op);

        {
            let mut g = self.inner.lock().await;
            let closing = self.closing(child_id);
            match g.vms.iter_mut().find(|v| v.id == child_id) {
                Some(v) if v.state == VmState::Creating && !closing => {
                    v.pid = Some(child_pid);
                    v.slot = child_slot;
                    v.state = VmState::Running;
                    v.ip = child_ip.clone();
                    v.vsock = parent_vsock;
                }
                // Stopped or deleted while it was being made: its VMM is not
                // wanted, nor anything else of it (E2).
                _ => {
                    drop(g);
                    kill_and_reap(child_pid);
                    self.drop_unborn_child(child_id, Some(&child_dir), child_slot).await;
                    return Err(aborted(child_id).to_string());
                }
            }
        }
        if let Err(e) = self.write_meta(child_id).await {
            return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e).await);
        }
        // Only now does the child run: its record says `running` on disk, and
        // a start-up that finds this VMM still paused resumes it — on its own
        // drive by then.
        if let Err(e) = fc::patch_vm_state(&child_sock, "Resumed").await {
            return Err(self.fail_child(child_id, &child_dir, Some(child_pid), child_slot, e.to_string()).await);
        }

        // Fork re-IP: the child restored with the parent's guest-internal IP.
        // If it has a working vsock, tell the guest to reconfigure eth0 to its
        // own allocated IP. Best-effort per contract: neither this nor the
        // re-MAC below fails the fork — but neither is load-bearing for
        // isolation any more either, because the tap was pinned before the
        // child's Firecracker was spawned.
        if parent_vsock {
            if let (Some(ip), Some(s), true) = (child_ip.as_deref(), child_slot, self.net_on) {
                // The child is a memory-clone, so it also inherits the parent's
                // guest MAC. Two ports with one MAC make the bridge FDB flap
                // and one guest goes dark — give the child its own
                // locally-administered MAC BEFORE re-IPing (plain exec; no
                // guest-agent contract change needed). It is the slot's derived
                // MAC, the same value its tap was pinned to above, rather than
                // a clock-salted one the pin could never match.
                //
                // This is the guest catching up to the pin, not the pin waiting
                // on the guest: the tap is already filtering. A failure here
                // therefore leaves the child isolated rather than free to
                // source frames as its parent — the previous order added the L2
                // rule only `if re_mac_ok`, and dropped that call's result, so
                // a failed exec silently left the port L2-unpinned.
                let (_, mac) = tap_pin_for_slot(s);
                let cmd: Vec<String> = ["ip", "link", "set", "dev", "eth0", "address", mac.as_str()]
                    .iter().map(|s| s.to_string()).collect();
                let re_mac_ok = match crate::guestclient::exec(&child_dir, &cmd, 5_000).await {
                    Ok(v) => {
                        let ok = v.get("exit_code").and_then(|c| c.as_i64()) == Some(0);
                        if !ok {
                            warn!(vm = %child_id, resp = %v, "fork re-MAC command returned non-zero");
                        }
                        ok
                    }
                    Err(e) => {
                        warn!(vm = %child_id, err = %e, "fork re-MAC exec failed");
                        false
                    }
                };
                if !re_mac_ok {
                    error!(
                        vm = %child_id,
                        mac = %mac,
                        "fork re-MAC failed — the child still carries its parent's MAC and \
                         its tap stays pinned, so it has NO network until it is restarted"
                    );
                }
                let gw = crate::ipalloc::fmt_ip(self.cidr.gateway());
                match crate::guestclient::set_ip(&child_dir, ip, self.cidr.prefix, &gw).await {
                    Ok(()) => {}
                    Err(e) => warn!(vm = %child_id, err = %e, "fork re-IP failed (best-effort)"),
                }
            }
        } else if self.net_on && child_slot.is_some() {
            // No guest agent, so nothing can re-MAC or re-IP this clone from
            // the host side. It keeps its parent's MAC and address, and its own
            // tap's pin drops both — where before it would have shared the
            // parent's L2 identity on the bridge and flapped its FDB entry.
            warn!(
                vm = %child_id,
                "forked without a guest agent: the child still carries its parent's MAC \
                 and address, and its tap is pinned to its own, so it has no network \
                 until the guest reconfigures itself"
            );
        }
        // The child's allocated (post-re-IP) address must join its tenant's
        // pair set before the fork returns.
        self.commit_or_isolate(child_id).await?;
        Ok(child_ip)
    }

    /// Copy the parent's disk image while the parent is paused for a fork
    /// (K1, K8): `cp --reflink=auto`, as a child process the fork awaits, so
    /// that the runtime is not blocked for the length of a multi-GiB copy and
    /// the copy can be ended. There is no fallback: after a `cp` that failed
    /// (out of space, say) a second, hole-less copy from the start would only
    /// keep the parent paused longer. A stop or delete of the parent ends the
    /// copy and the fork.
    async fn copy_disk_in_pause(&self, parent_id: &str, src: &str, dst: &str) -> Result<(), String> {
        let mut cp = tokio::process::Command::new("cp")
            .arg("--reflink=auto")
            .arg(src)
            .arg(dst)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| format!("copy {} -> {}: {}", src, dst, e))?;
        loop {
            tokio::select! {
                st = cp.wait() => {
                    return match st {
                        Ok(st) if st.success() => Ok(()),
                        Ok(st) => Err(format!("copy {} -> {}: cp {}", src, dst, st)),
                        Err(e) => Err(format!("copy {} -> {}: {}", src, dst, e)),
                    };
                }
                _ = sleep(BUSY_POLL) => {
                    if self.closing(parent_id) {
                        let _ = cp.kill().await;
                        return Err(format!("fork: {} is being stopped or deleted; the copy of its disk was ended", parent_id));
                    }
                }
            }
        }
    }

    /// A fork that failed before its child had a VMM leaves nothing behind
    /// (F4): the child's directory, when this fork created it (`child_dir`),
    /// the record it reserved, and the network slot and tap it claimed
    /// (`slot`).
    async fn drop_unborn_child(&self, child_id: &str, child_dir: Option<&str>, slot: Option<u32>) {
        if let Some(d) = child_dir {
            if let Err(e) = std::fs::remove_dir_all(d) {
                if e.kind() != std::io::ErrorKind::NotFound {
                    warn!(vm = %child_id, path = %d, err = %e, "fork: could not remove the child's directory");
                }
            }
        }
        {
            let mut g = self.inner.lock().await;
            // A stop of the child that came in meanwhile has made the
            // reserved record `stopped`; it is still the reservation (the
            // child id is this fork's, nothing else can have written it), and
            // goes with the directory it would otherwise outlive.
            if let Some(pos) = g.vms.iter().position(|v| {
                v.id == child_id && v.pid.is_none() && matches!(v.state, VmState::Creating | VmState::Stopped)
            }) {
                g.vms.remove(pos);
                lock_std(&self.meta_owed).remove(child_id);
            }
        }
        self.release_child_slot(slot).await;
    }

    /// Give back the network slot a fork claimed for its child, and the tap
    /// of that slot: the tap first, then the slot, so that the next sandbox
    /// to take the slot never finds this one's device (FLU-6 R7's order).
    async fn release_child_slot(&self, slot: Option<u32>) {
        let Some(s) = slot else { return };
        if self.rules_on() {
            self.remove_tap_locked(s).await;
        }
        let mut g = self.inner.lock().await;
        Self::free_slot(&mut g, s);
    }

    /// Is the child's reservation still this fork's: the record is there, in
    /// `creating`, and no stop or delete of it has come in?
    fn child_reserved(&self, g: &Inner, child_id: &str) -> bool {
        g.vms.iter().any(|v| v.id == child_id && v.state == VmState::Creating) && !self.closing(child_id)
    }

    /// A fork that failed once its child had, or was about to get, a VMM
    /// (F4): that VMM is killed — the record lets go of its pid first, so the
    /// exit reaper takes it for a deliberate kill — the child's snapshot
    /// files go, the slot and the tap this fork claimed (`slot`) are given
    /// back, and the child stays as an `error` record for its owner to see
    /// and delete. A record that was already written (`running`) is written
    /// again. Returns `err` as the answer.
    ///
    /// A reservation that is no longer this fork's — the record gone, or a
    /// stop or delete of the child pre-empted the fork (most likely why it
    /// failed) — leaves nothing of the child behind (`drop_unborn_child`).
    async fn fail_child(&self, child_id: &str, child_dir: &str, child_pid: Option<i32>, slot: Option<u32>, err: String) -> String {
        let found = {
            let mut g = self.inner.lock().await;
            let closing = self.closing(child_id);
            match g.vms.iter_mut().find(|v| v.id == child_id) {
                Some(v) if !closing && matches!(v.state, VmState::Creating | VmState::Running) => {
                    let written = v.state != VmState::Creating;
                    v.state = VmState::Error;
                    v.pid = None;
                    v.snapshot_valid = false;
                    // The slot goes back below: the record must not keep it.
                    if v.slot == slot {
                        v.slot = None;
                        v.ip = None;
                    }
                    Some((v.dir_id.clone(), written))
                }
                _ => None,
            }
        };
        if let Some(p) = child_pid {
            kill_and_reap(p);
        }
        warn!(vm = %child_id, err = %err, "fork: the child failed; its VMM is gone and its snapshot files are removed");
        let Some((dir_id, written)) = found else {
            self.drop_unborn_child(child_id, Some(child_dir), slot).await;
            return err;
        };
        self.discard_snapshot_files(child_id, &dir_id);
        self.release_child_slot(slot).await;
        if written {
            if let Err(e) = self.write_meta(child_id).await {
                warn!(vm = %child_id, err = %e, "fork: could not write the child's error record yet");
            }
        }
        err
    }

    // ---- exec (v3 vsock guest agent) ----

    /// Run a command inside the guest via the vsock guest agent.
    /// Returns the guest's JSON response verbatim on success. Error variants map
    /// to HTTP statuses in the server handler.
    pub async fn exec(&self, id: &str, cmd: &[String], timeout_ms: u64) -> ExecOutcome {
        let (state, vsock, dir_id) = {
            let g = self.inner.lock().await;
            match g.vms.iter().find(|v| v.id == id) {
                Some(v) => (v.state.clone(), v.vsock, v.dir_id.clone()),
                None => return ExecOutcome::NotFound,
            }
        };
        if state != VmState::Running {
            return ExecOutcome::NotRunning;
        }
        if !vsock {
            return ExecOutcome::Unavailable;
        }
        let Some(dir) = instance_path(&self.data_dir, &dir_id) else {
            return ExecOutcome::NotFound;
        };
        match crate::guestclient::exec(&dir, cmd, timeout_ms).await {
            Ok(v) => ExecOutcome::Ok(v),
            // Connect/handshake/round-trip failure → guest agent unavailable.
            Err(e) => ExecOutcome::Failed(e.0),
        }
    }

    /// Open a streamed exec (v5 P5.1): same preconditions as `exec`, but on
    /// success returns the guest connection for the caller to forward frames
    /// from. The data-phase deadline is the caller's job (the setup phase is
    /// bounded inside guestclient).
    pub async fn exec_stream(
        &self,
        id: &str,
        cmd: &[String],
        timeout_ms: u64,
    ) -> ExecStreamOutcome {
        let (state, vsock, dir_id) = {
            let g = self.inner.lock().await;
            match g.vms.iter().find(|v| v.id == id) {
                Some(v) => (v.state.clone(), v.vsock, v.dir_id.clone()),
                None => return ExecStreamOutcome::NotFound,
            }
        };
        if state != VmState::Running {
            return ExecStreamOutcome::NotRunning;
        }
        if !vsock {
            return ExecStreamOutcome::Unavailable;
        }
        let Some(dir) = instance_path(&self.data_dir, &dir_id) else {
            return ExecStreamOutcome::NotFound;
        };
        match crate::guestclient::exec_stream(&dir, cmd, timeout_ms).await {
            Ok((s, slot)) => ExecStreamOutcome::Ok(s, slot),
            Err(e) => ExecStreamOutcome::Failed(e.0),
        }
    }

    // ---- warm pool ----

    /// The full refill plan: the legacy config pool (always present when
    /// pool_target > 0, exactly the pre-P4 shape) plus the feluccad-managed
    /// template pools.
    async fn pool_specs(&self) -> Vec<PoolSpec> {
        let mut specs: Vec<PoolSpec> = self.legacy_pool_spec().into_iter().collect();
        let g = self.inner.lock().await;
        specs.extend(g.template_pools.iter().cloned());
        specs
    }

    /// The config pool (`pool_size`), when there is one — exactly the pre-P4
    /// shape. Unlike the template pools it is known from the first tick.
    fn legacy_pool_spec(&self) -> Option<PoolSpec> {
        (self.pool_target > 0).then(|| PoolSpec {
            image: DEFAULT_IMAGE.to_string(),
            image_sha256: String::new(),
            vcpus: 1,
            mem_mib: 256,
            disk_gb: 0,
            count: self.pool_target,
        })
    }

    /// Parked pool VMs matching one spec's shape.
    async fn pooled_count_matching(&self, s: &PoolSpec) -> u32 {
        let g = self.inner.lock().await;
        g.vms.iter()
            .filter(|v| {
                v.state == VmState::Pooled
                    && v.image == s.image
                    && v.vcpus == s.vcpus
                    && v.mem_mib == s.mem_mib
                    && v.disk_gb == s.disk_gb
            })
            .count() as u32
    }

    pub async fn refill_pool(self: &Arc<Self>) {
        let specs = self.pool_specs().await;

        // Drain BEFORE topping up: pooled VMs that match no current spec
        // (deleted templates, re-captured shas) or exceed their spec's count
        // are deleted — an empty PUT /v1/pools genuinely tears pools down,
        // and a stale-image pool VM can never linger claimable.
        //
        // Pool VMs survive an agent restart, the template specs do not: until
        // feluccad has pushed them to this process, only the config pool's
        // own excess is drained (`drainable_pooled_ids`).
        let surplus = {
            let g = self.inner.lock().await;
            drainable_pooled_ids(&g.vms, &specs, self.legacy_pool_spec().as_ref(), g.pools_synced)
        };
        for sid in surplus {
            // The list above is a moment old, and every delete below takes a
            // few hundred milliseconds. A create may have claimed this VM in
            // between — it then answers to the caller's id, in the SAME
            // directory. Only a VM that is still pooled is drained.
            if !self.retire_pooled(&sid).await {
                continue;
            }
            info!(vm = %sid, "draining surplus pool vm");
            if let Err(e) = self.delete(&sid).await {
                warn!(vm = %sid, err = %e, "pool drain failed");
            }
        }

        // W3, W11: no new guest while a fence is positively gone or start-up
        // could not settle the ingress chain — before any work.
        if !specs.is_empty() {
            let why = self.new_guest_refusal().await;
            // X11: logged when it starts and when it ends, not every 5 s.
            if net::state_changed_to("pool-refill-refused", why.map(str::to_string)) {
                match why {
                    Some(why) => warn!(reason = %why, "pool refill refused: no new guest on this node now"),
                    None => info!("pool refill: new guests are allowed again"),
                }
            }
            if why.is_some() {
                return;
            }
        }
        for spec in specs {
            // Skip specs in failure backoff.
            {
                let mut bo = self.pool_backoff.lock().await;
                if backoff_active(&mut bo, &spec_key(&spec), Instant::now()) {
                    continue;
                }
            }
            loop {
                let count = self.pooled_count_matching(&spec).await;
                if count >= spec.count { break; }

                let id = format!("pool-{:x}", now_ms());
                let cs = CreateSpec {
                    id: id.clone(),
                    name: "pool".to_string(),
                    vcpus: spec.vcpus,
                    mem_mib: spec.mem_mib,
                    tenant_id: None,
                    image: spec.image.clone(),
                    image_sha256: if spec.image_sha256.is_empty() {
                        None
                    } else {
                        Some(spec.image_sha256.clone())
                    },
                    disk_gb: spec.disk_gb,
                };
                if let Err(e) = self.cold_create(&cs, VmState::Pooled, None).await {
                    warn!(vm = %id, image = %spec.image, err = %e, "pool prewarm failed");
                    // Remove the failed record + instance dir (each tick
                    // would otherwise leak one Error VM) and back the spec
                    // off so a broken sha doesn't retry every 5s.
                    if let Err(de) = self.delete(&id).await {
                        warn!(vm = %id, err = %de, "pool prewarm cleanup failed");
                    }
                    let mut bo = self.pool_backoff.lock().await;
                    bo.insert(spec_key(&spec), Instant::now() + REFILL_BACKOFF);
                    break;
                }
            }
        }
    }

    /// Take a pool VM out of the claimable set — if it is still in it.
    ///
    /// One critical section, the same lock `claim_from_pool` retags under:
    /// either the claim got there first (no record has this id any more, or
    /// it is no longer `Pooled`) and this returns false, or the VM stops being
    /// `Pooled` here and no claim can match it from now on. `delete` then
    /// tears it down like any stopped VM.
    async fn retire_pooled(&self, id: &str) -> bool {
        let mut g = self.inner.lock().await;
        match g.vms.iter_mut().find(|v| v.id == id) {
            Some(v) if v.state == VmState::Pooled => {
                v.state = VmState::Stopped;
                true
            }
            _ => false,
        }
    }

    /// Claim a parked pool VM of the requested shape and retag it to the real
    /// id/name. Returns true if claimed. The find-and-retag happens under one
    /// lock acquisition (state Pooled → Creating marks the claim), so two
    /// concurrent creates can never grab the same pool VM.
    async fn claim_from_pool(&self, spec: &CreateSpec) -> Result<bool, String> {
        let id = spec.id.as_str();
        let pooled_id = {
            let mut g = self.inner.lock().await;
            let Some(idx) = find_pooled_match(&g.vms, &spec.image, spec.image_sha256.as_deref(), spec.vcpus, spec.mem_mib, spec.disk_gb) else {
                return Ok(false);
            };
            let v = &mut g.vms[idx];
            let old = v.id.clone();
            // Retag to the real id/name but KEEP dir_id (the pool dir).
            // Creating marks the record as claimed until the resume lands.
            v.id = id.to_string();
            v.name = spec.name.clone();
            v.tenant_id = spec.tenant_id.clone();
            v.state = VmState::Creating;
            old
        };

        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        if let Err(e) = fc::patch_vm_state(&sock, "Resumed").await {
            // Pool VM is unusable (its Firecracker is gone, or does not
            // answer). Give the record its pool id back, remove it — it was
            // never handed to anyone and cannot be now — and let the caller
            // fall through to a cold boot.
            warn!(pool_vm = %pooled_id, vm = %id, err = %e, "pool claim failed; removing the pool vm");
            let spec = {
                let mut g = self.inner.lock().await;
                g.vms.iter_mut().find(|v| v.id == id).map(|v| {
                    v.id = pooled_id.clone();
                    v.name = "pool".to_string();
                    v.tenant_id = None;
                    v.state = VmState::Error;
                    pool_spec_key_of(v)
                })
            };
            if let Some(key) = spec {
                self.remove_dead_pool_vm(&pooled_id, key).await;
            }
            return Ok(false);
        }
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await?;
        Ok(true)
    }

    // ---- stop / start / delete ----

    pub async fn stop(&self, id: &str) -> Result<(), OpError> {
        use nix::sys::signal::{kill, Signal};
        use nix::unistd::Pid;
        // From here no operation starts on this VM, and none in progress
        // commits. Stop does not queue behind one: ending the VMM below is
        // what makes a Firecracker call in progress fail.
        let _closing = self.begin_close(id);
        self.claim_unsettled(id).await;
        // Read the pid and clear the record in ONE critical section: the exit
        // reaper then sees a pid mismatch and treats this as a deliberate kill
        // (no error transition), and nothing can change the record between
        // the read and the write. Stopping a sleeping VM discards its
        // snapshot (below), so the marker goes with the state.
        let (pid, dir_id) = {
            let mut g = self.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or("NotFound")?;
            let out = (v.pid, v.dir_id.clone());
            v.state = VmState::Stopped;
            v.pid = None;
            v.snapshot_valid = false;
            out
        };
        // The signal follows with no await in between: a stop is never left
        // as "the record says stopped and nobody told the VMM".
        let procs = self.procs();
        let mut wait_for: Option<i32> = None;
        let mut signal_err: Option<String> = None;
        if let Some(p) = pid {
            // Signal the pid only while it is still this instance's VMM. A
            // Firecracker adopted at start-up is not this agent's child, so
            // once it has exited its number is free for any other process.
            if procs.identify(p, &dir_id) != Identity::Mismatch {
                match kill(Pid::from_raw(p), Signal::SIGTERM) {
                    Ok(_) | Err(nix::errno::Errno::ESRCH) => wait_for = Some(p),
                    Err(e) => signal_err = Some(e.to_string()),
                }
            } else if procs.exiting(p) {
                wait_for = Some(p);
            }
            // Otherwise it is gone long ago, or a stranger now has the number:
            // there is nothing of ours to wait for.
        }
        // Pre-empting an operation in progress (S4'): every Firecracker of
        // this unit in the instance directory is killed, not only the one the
        // record names — a wake, a start or a fork child whose new VMM is not
        // in the record yet holds its operation in a call to that VMM, and
        // only its death ends the call. Killed outright, not asked: a stop
        // that gives up below must not leave a signalled VMM alive.
        if self.op_in_progress(id) {
            self.kill_instance_vmms(id, &dir_id);
        }
        // On disk straight away. A stop that was never written down reads as
        // `running` with a dead pid at the next agent start.
        let persisted = self.write_meta(id).await;
        if let Some(e) = signal_err {
            return Err(e.into());
        }
        // An operation in progress lets go once its Firecracker call fails or
        // it reaches its commit point and finds the VM being stopped. Until it
        // has, the files below may still be its to write or read.
        if !self.wait_op_released(id).await {
            // The record says `stopped`, on disk: that is the answer, so
            // that the control plane records it (S10). A plain `busy` says
            // "nothing was decided", and it would go on listing the sandbox
            // as running or asleep.
            warn!(vm = %id, "stop: an operation in progress did not let go of this VM in time");
            return Err(OpError::invalid(&VmState::Stopped, STOP_GAVE_UP));
        }
        if let Some(p) = wait_for {
            // Wait for the FC process to actually exit: "stopped" gates the
            // rootfs capture endpoint, and a dying FC may still be flushing
            // rootfs.ext4. Escalate to SIGKILL at 5s, give up at 10s (the
            // reaper owns whatever is left).
            let start = Instant::now();
            let mut killed = false;
            loop {
                if procs.exited(p) {
                    break; // gone, every thread of it
                }
                let waited = start.elapsed();
                if !killed && waited >= Duration::from_secs(5) {
                    // Only while it is still ours: by now it may be mid-exit
                    // (nothing to add), but it is never somebody else's.
                    if procs.identify(p, &dir_id) != Identity::Mismatch {
                        let _ = kill(Pid::from_raw(p), Signal::SIGKILL);
                    }
                    killed = true;
                }
                if waited >= Duration::from_secs(10) {
                    warn!(vm = %id, pid = p, "stop: pid still present after 10s");
                    break;
                }
                sleep(Duration::from_millis(100)).await;
            }
        }
        // A stopped VM is never woken: whatever snapshot it had is of no use
        // any more (and, left there, an older agent would read the VM as
        // asleep). mem.bin is kept if a VMM that did not exit still maps it.
        self.discard_snapshot_files(id, &dir_id);
        self.remove_outrun(id, &dir_id);
        Ok(persisted?)
    }

    /// SIGKILL every Firecracker this unit started that serves the instance
    /// directory `dir_id` (the scan delete makes, `in_my_cgroup`), whether or
    /// not a record names it. Not without the data-dir lock, as there.
    fn kill_instance_vmms(&self, id: &str, dir_id: &str) {
        if !valid_vm_id(dir_id) || !self.hold_data_dir_lock() {
            return;
        }
        let procs = self.procs();
        for p in procs.scan().into_iter().filter(|p| p.dir_name == dir_id) {
            if procs.in_my_cgroup(p.pid) {
                warn!(vm = %id, pid = p.pid, "stop: killing a Firecracker of this VM an operation in progress is waiting on");
                self.kill_fc(p.pid, dir_id);
            }
        }
    }

    pub async fn start(self: &Arc<Self>, id: &str) -> Result<(), OpError> {
        let _op = self.begin_op(id, OpKind::Start).await?;
        let (vcpus, mem_mib, slot, ip, pid0, dir_id) = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            // A capture stream is reading this instance's rootfs: spawning
            // an FC over it would corrupt the published template bytes.
            if g.capturing.contains(id) {
                return Err(OpError::invalid(&v.state, "InvalidState: capture in progress"));
            }
            // start is a cold boot: only valid when no FC owns the instance.
            // paused/sleeping VMs have live state to lose — spawning over them
            // would orphan the FC holding it (use resume/wake instead).
            match v.state {
                VmState::Running => return Ok(()),
                VmState::Stopped | VmState::Error => {}
                VmState::Paused => return Err(OpError::invalid(&v.state, "InvalidState: paused (use resume)")),
                VmState::Sleeping => return Err(OpError::invalid(&v.state, "InvalidState: sleeping (use wake)")),
                VmState::Creating | VmState::Pooled => {
                    return Err(OpError::invalid(&v.state, format!("InvalidState: {}", v.state.as_str())));
                }
            }
            if let Some(p) = v.pid {
                use nix::sys::signal::kill;
                use nix::unistd::Pid;
                if kill(Pid::from_raw(p), None).is_ok() {
                    return Err(OpError::invalid(&v.state, format!("InvalidState: process {} still attached", p)));
                }
            }
            (v.vcpus, v.mem_mib, v.slot, v.ip.clone(), v.pid, v.dir_id.clone())
        };
        // W3: no guest back onto a network whose fences are positively gone.
        self.refuse_new_guest("start", id).await.map_err(OpError::Other)?;

        let dir_path = instance_path(&self.data_dir, &dir_id).ok_or("InvalidId")?;
        let rootfs_dst = format!("{}/rootfs.ext4", dir_path);

        // A cold boot starts from the disk alone; a snapshot from before it
        // would describe a guest that never was (S8), and one set aside as
        // stale is of no use from here either (S3').
        self.discard_snapshot_files(id, &dir_id);
        self.remove_outrun(id, &dir_id);

        // Cold boot: configure_guest sets the NIC's MAC, so the full pin
        // (address, ARP and L2) applies from the first frame.
        // Through the rule seam (`pin_tap`), so that unit tests run the pin.
        if self.rules_on() {
            if let Some(s) = slot {
                let (tap, pin_ip, mac) = {
                    let g = self.inner.lock().await;
                    self.tap_pin(&g, s)
                };
                if !self.rules.pin_tap(&tap, pin_ip.as_deref(), &mac) {
                    return Err("TapSetupFailed".into());
                }
                // X4: pinned and up — not the watch's to hold down any more.
                lock_std(&self.watch).release_tap(&tap);
            }
        }

        let (pid, vsock_on) = self.spawn_and_configure(id, &dir_path, &rootfs_dst, vcpus, mem_mib, slot, ip.as_deref()).await?;
        // The commit point.
        {
            let mut g = self.inner.lock().await;
            let committed = match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if self.may_commit(v, pid0) => {
                    v.pid = Some(pid);
                    v.vsock = vsock_on;
                    v.state = VmState::Running;
                    v.snapshot_valid = false;
                    true
                }
                _ => false,
            };
            if !committed {
                drop(g);
                kill_and_reap(pid);
                return Err(aborted(id));
            }
        }
        Ok(self.write_meta(id).await?)
    }

    pub async fn delete(&self, id: &str) -> Result<(), String> {
        // As in `stop`: no operation starts or commits from here on.
        let _closing = self.begin_close(id);
        self.claim_unsettled(id).await;
        // One critical section decides what this delete is about.
        let (dir_id, lives_there) = {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) => {
                    // A pool VM leaves the claimable set here, under the lock
                    // a claim retags under. Either the claim got there first —
                    // then no record has this id any more and the branch
                    // below refuses — or no claim can match it from now on.
                    // Without this, a delete of a pooled id that raced a
                    // create removed the tap, the slot and the directory of
                    // the sandbox that had just been claimed from it.
                    if v.state == VmState::Pooled {
                        v.state = VmState::Stopped;
                    }
                    (v.dir_id.clone(), None)
                }
                // No record has this id, yet a VM may live in the directory
                // of that name: a pool VM that was claimed keeps its pool
                // directory under its new id.
                None => (
                    id.to_string(),
                    g.vms.iter().find(|v| v.dir_id == id).map(|v| v.id.clone()),
                ),
            }
        };
        // delete does not require the VM to exist, and for an unknown id the
        // directory it removes is the one named after the id. That must never
        // be a directory some other VM is running out of: deleting a stale
        // pool id used to remove the rootfs, meta.json and sockets of the
        // sandbox that had been claimed from it.
        if let Some(owner) = lives_there {
            warn!(vm = %id, owner = %owner, "delete: no VM has this id and its directory belongs to another VM; nothing removed");
            return Ok(());
        }

        // stop is best-effort (idempotent delete) — except when an operation
        // in progress did not let go of the VM: the directory removed below
        // may still hold the files it is writing or reading. The caller
        // retries; the VM is `stopped` by then, and the answer says so.
        if let Err(OpError::InvalidState { msg, .. }) = self.stop(id).await {
            if msg == STOP_GAVE_UP {
                return Err(STOP_GAVE_UP.into());
            }
        }

        // Nothing may be left running out of the directory this is about to
        // remove. stop() dealt with the VMM the record holds; what can remain
        // is one no record holds — a VM whose meta.json did not load at
        // start-up is left running and unmanaged (`SweepStep::LeaveUnreadable`),
        // and the control plane still knows its id. Removing the directory
        // under it would leave a Firecracker with its memory, its tap and its
        // egress that no later start can even find: the scan locates VMMs by
        // their socket's directory.
        if valid_vm_id(&dir_id) {
            let procs = self.procs();
            let strays: Vec<FcProc> =
                procs.scan().into_iter().filter(|p| p.dir_name == dir_id).collect();
            if !strays.is_empty() && !self.hold_data_dir_lock() {
                return Err("InvalidState: a Firecracker is running in this VM's directory and this agent does not hold the data-dir lock".into());
            }
            for p in strays {
                if !procs.in_my_cgroup(p.pid) {
                    warn!(vm = %id, pid = p.pid, "delete: a Firecracker this unit did not start is running in the VM's directory; nothing removed");
                    return Err(format!(
                        "InvalidState: firecracker {} in this VM's directory was not started by this agent",
                        p.pid
                    ));
                }
                warn!(vm = %id, pid = p.pid, "delete: killing the Firecracker still running in the VM's directory");
                self.kill_fc_and_wait(p.pid, &dir_id).await;
                if !procs.exited(p.pid) {
                    return Err(format!(
                        "InvalidState: firecracker {} is still running in this VM's directory",
                        p.pid
                    ));
                }
            }
        }

        // FLU-6 R7: the guest IP goes back to the pool only once no rule
        // names it any more. It used to be freed first and the rules rebuilt
        // last, so for that moment the next sandbox to take the address sat
        // behind the previous owner's node ports and tenant pairs.
        //
        // (1) The sandbox leaves the membership, in one critical section.
        // From here the record is on its way out: still in the list (its id
        // and its directory stay taken until the directory is gone, and its
        // slot stays reserved), marked closing (`begin_close`, so no
        // operation commits to it), with no address and no exposes — it no
        // longer counts for either rebuild, and an expose of it answers "no
        // ip". Address and exposes go in the same section, so nothing reads
        // the one without the other. (A listing taken meanwhile shows the
        // record with `ip` null.)
        //
        // The slot is taken OFF the record here, in the same section, and
        // only this value is used below: of two overlapping deletes of one
        // sandbox exactly one gets it, and the other frees, holds and
        // un-taps nothing. Both used to read it at entry, so the second freed
        // it again after a create had claimed it — removing the NEW
        // sandbox's tap and pin, and handing its address to the next one.
        let slot = {
            let mut g = self.inner.lock().await;
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) => {
                    v.ip = None;
                    v.exposes.clear();
                    v.slot.take()
                }
                None => None,
            }
        };
        // (2) Both rebuilds, each reporting whether it was applied.
        let isolation_ok = self.refresh_isolation().await;
        if !isolation_ok {
            // Teardown goes on regardless: the VM is already gone, and
            // returning an error here would only make delete non-idempotent.
            warn!(vm = %id, "cross-tenant isolation not in force after delete");
        }
        let ingress_ok = self.refresh_ingress().await;
        // (3) The tap, and only after rules that no longer name its address,
        // the slot.
        if self.rules_on() {
            if let Some(s) = slot {
                // The guest is gone: its tap goes either way.
                self.remove_tap_locked(s).await;
                let mut g = self.inner.lock().await;
                if isolation_ok && ingress_ok {
                    Self::free_slot(&mut g, s);
                } else {
                    // Held, not leaked (A): the first later pair of rebuilds
                    // that both succeed lets it go (`settle_slots`). A create
                    // rolled back by `commit_or_isolate` comes through here
                    // too, so while a rebuild keeps failing each attempt
                    // used to cost one address until a restart.
                    g.held_slots.insert(s);
                    error!(
                        vm = %id,
                        slot = s,
                        isolation_rebuilt = isolation_ok,
                        ingress_rebuilt = ingress_ok,
                        "delete: a rule rebuild failed, so the old rules may still name this sandbox's address; \
                         its IP slot is held until both rebuilds succeed again rather than go to the next sandbox"
                    );
                }
            }
        }

        // An id that is not a plain path component never reaches remove_dir_all:
        // delete does not require the VM to exist, so this is the one call in
        // the manager that would recursively delete an attacker-chosen path.
        match instance_path(&self.data_dir, &dir_id) {
            Some(dir_path) => {
                if let Err(e) = std::fs::remove_dir_all(&dir_path) {
                    if e.kind() != std::io::ErrorKind::NotFound {
                        warn!(path = %dir_path, err = %e, "remove_dir_all failed");
                    }
                }
            }
            None => warn!(vm = %id, "delete: rejected id that is not a valid path component"),
        }

        {
            let mut g = self.inner.lock().await;
            if let Some(pos) = g.vms.iter().position(|v| v.id == id) {
                g.vms.remove(pos);
            }
        }
        // The record is gone, and with it anything still owed to its file.
        lock_std(&self.meta_owed).remove(id);
        // Idempotent: unknown id is fine (no error).
        Ok(())
    }

    // ---- helpers ----

    async fn set_state(&self, id: &str, state: VmState) {
        let mut g = self.inner.lock().await;
        if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
            v.state = state;
        }
    }

    /// React to an FC process exit (reaper task or liveness sweep). Acts ONLY
    /// when `pid` is still the recorded pid and the state implies a live
    /// process; deliberate kills (sleep/stop/delete) clear the pid under the
    /// lock before signalling, so they no-op here.
    ///
    /// A sandbox goes to `error` and stays for its owner to see. A pool VM
    /// nobody claimed is removed instead: it cannot be claimed any more, and
    /// as an `error` record it would keep a slot, a tap and a rootfs copy that
    /// nothing ever deletes — neither the drain (it looks at `pooled` VMs) nor
    /// a tenant (there is none).
    async fn on_fc_exit(&self, id: &str, pid: i32) {
        let (transitioned, dead_pool, dir_id) = {
            let mut g = self.inner.lock().await;
            // A dead pool VM leaves the claimable set by this very transition:
            // claims match on state == Pooled, and Error is not claimable.
            // While an operation holds the VM, its record is that operation's
            // to settle (S7'): a wake whose new VMM died before the resume
            // answered must decide what that means, and an `error` written
            // here first would leave its intact snapshot under `error`.
            // Whatever the operation leaves, `sweep_dead` settles within 5 s.
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.pid == Some(pid)
                    && matches!(v.state, VmState::Creating | VmState::Running | VmState::Paused | VmState::Pooled)
                    && !self.op_in_progress(id) =>
                {
                    let dead_pool = (v.state == VmState::Pooled).then(|| pool_spec_key_of(v));
                    v.state = VmState::Error;
                    v.pid = None;
                    v.snapshot_valid = false;
                    (true, dead_pool, v.dir_id.clone())
                }
                _ => (false, None, String::new()),
            }
        };
        if !transitioned {
            return;
        }
        // A guest that died took its memory with it: no snapshot beside it
        // can be woken into what it was. Unless an operation holds the VM —
        // then the files are that operation's to clean up on its own error
        // path, and may be in the middle of being written.
        if dead_pool.is_none() && !self.op_in_progress(id) {
            self.discard_snapshot_files(id, &dir_id);
        }
        match dead_pool {
            Some(key) => {
                warn!(vm = %id, pid = pid, "a pool vm's firecracker exited; removing the pool vm");
                self.remove_dead_pool_vm(id, key).await;
            }
            None => {
                warn!(vm = %id, pid = pid, "firecracker exited unexpectedly; state -> error");
                // Best-effort: the instance dir may be mid-delete.
                let _ = self.write_meta(id).await;
            }
        }
    }

    /// Remove a pool VM whose Firecracker is gone or unusable, and give its
    /// pool a rest: whatever killed it (most likely the node running out of
    /// memory) is not helped by prewarming a replacement five seconds later.
    async fn remove_dead_pool_vm(&self, id: &str, spec_key: String) {
        {
            let mut bo = self.pool_backoff.lock().await;
            bo.insert(spec_key, Instant::now() + REFILL_BACKOFF);
        }
        if let Err(e) = self.delete(id).await {
            warn!(vm = %id, err = %e, "could not remove a dead pool vm");
        }
    }

    /// Mark VMs whose Firecracker is gone as error. Covers VMMs adopted after
    /// an agent restart, which have no in-process reaper task — since
    /// ADR-0011 that is every VM that was up when the agent last started.
    ///
    /// "Gone" is decided by what the pid is, not by whether some process has
    /// it: an adopted VMM is not this agent's child, so after it dies its
    /// number can be handed to an unrelated process, and `kill(pid, 0)` would
    /// keep reporting the VM as running for as long as that process lives.
    /// A pid that cannot be inspected is left alone (`Identity::Unknown`).
    pub async fn sweep_dead(&self) {
        let candidates: Vec<(String, String, i32)> = {
            let g = self.inner.lock().await;
            g.vms.iter()
                .filter(|v| matches!(v.state, VmState::Creating | VmState::Running | VmState::Paused | VmState::Pooled))
                .filter_map(|v| v.pid.map(|p| (v.id.clone(), v.dir_id.clone(), p)))
                .collect()
        };
        let procs = self.procs();
        for (id, dir_id, pid) in candidates {
            if procs.identify(pid, &dir_id) == Identity::Mismatch {
                self.on_fc_exit(&id, pid).await;
            }
        }
    }

    /// Write the current in-memory record for `id` to its meta.json.
    ///
    /// The file is replaced, never rewritten in place, and is on disk when
    /// this returns (`replace_file_durably`): an agent killed mid-write used
    /// to leave a truncated meta.json, and a VM whose record does not load is
    /// a VM start-up can no longer manage.
    ///
    /// A write that fails, or whose caller goes away in the middle of it, is
    /// not forgotten: the id stays on the `meta_owed` list and the liveness
    /// loop writes it (`flush_unwritten_meta`). Without that, a wake that hit
    /// a full disk left `sleeping` on disk under a running VM for good — the
    /// retried request is answered "already running" from memory and never
    /// writes again.
    async fn write_meta(&self, id: &str) -> Result<(), String> {
        let mut owed = OwedMeta { owed: &self.meta_owed, id, armed: true };
        let res = self.write_meta_now(id).await;
        let settled = match &res {
            Ok(()) => true,
            // No such record (any more): there is nothing left to write.
            Err(e) => e == "NotFound" || e == "InvalidId",
        };
        if settled {
            owed.armed = false;
            lock_std(&self.meta_owed).remove(id);
        }
        res
    }

    async fn write_meta_now(&self, id: &str) -> Result<(), String> {
        // Held across the snapshot AND the replace, so the newest snapshot is
        // always the one that lands (see the field's comment).
        let _writing = self.meta_write_lock.lock().await;
        let meta = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            Meta {
                id: v.id.clone(),
                name: v.name.clone(),
                dir_id: v.dir_id.clone(),
                vcpus: v.vcpus,
                mem_mib: v.mem_mib,
                pid: v.pid,
                slot: v.slot,
                ip: v.ip.clone(),
                state: v.state.clone(),
                vsock: v.vsock,
                tenant_id: v.tenant_id.clone(),
                exposes: v.exposes.clone(),
                // Default shape stays None so pre-P4 meta bytes are identical.
                image: if v.image == DEFAULT_IMAGE { None } else { Some(v.image.clone()) },
                disk_gb: if v.disk_gb == 0 { None } else { Some(v.disk_gb) },
                image_sha256: v.image_sha256.clone(),
                snapshot_valid: v.snapshot_valid,
            }
        };

        let dir_path = instance_path(&self.data_dir, &meta.dir_id).ok_or("InvalidId")?;
        replace_file_durably(&dir_path, "meta.json", meta.to_json().as_bytes())
            .map_err(|e| format!("write meta {}/meta.json: {}", dir_path, e))
    }

    /// Write the records whose last write did not land (liveness loop, every
    /// 5 s). See `write_meta`.
    pub async fn flush_unwritten_meta(&self) {
        let owed: Vec<String> = lock_std(&self.meta_owed).iter().cloned().collect();
        for id in owed {
            match self.write_meta(&id).await {
                Ok(()) => info!(vm = %id, "meta.json written (an earlier write had failed or was interrupted)"),
                Err(e) if e == "NotFound" || e == "InvalidId" => {}
                Err(e) => warn!(vm = %id, err = %e, "meta.json still cannot be written; the record on disk is behind this VM's real state"),
            }
        }
    }
}

// ---- utilities ----

/// Emit `"key":"value"` with the value escaped. `name` reaches us verbatim
/// from a tenant's sandbox-create request, so a raw push let a name like
/// `x","state":"running","pid":1,"z":"` forge fields in this document or make
/// it unparseable. Every other JSON writer in the agent escapes (meta.rs,
/// fc.rs, registration.rs); this one now shares fc.rs's escaper.
fn push_kv_str(out: &mut String, key: &str, val: &str) {
    out.push_str(&fc::json_str(key));
    out.push(':');
    out.push_str(&fc::json_str(val));
}

/// The tap name and the MAC every path must pin for one slot.
///
/// One derivation, one choke point: cold create, start, wake, fork and startup
/// adoption all pin through this, so no path can quietly pin a different L2
/// identity than the one `configure_guest` gave the NIC — or, as wake and fork
/// once did, none at all. Pure for unit tests.
fn tap_pin_for_slot(slot: u32) -> (String, net::GuestMac) {
    (net::tap_name(slot), net::guest_mac_for_slot(slot))
}

/// The taps that must be re-pinned when persisted records are adopted at
/// startup: every VM with a live Firecracker still attached to a tap. That
/// includes pooled VMs — they are adopted with their VMM (ADR-0011), and the
/// next create hands one to a tenant without the claim path pinning anything.
///
/// The netdev chains do not survive a host reboot, and an in-place agent
/// upgrade adopts guests that were started before the pin existed at all —
/// either way their ports sit on the bridge unfiltered until they are stopped
/// and started again, which for a long-lived sandbox is never. Sleeping and
/// stopped VMs are excluded: they hold no tap now and are pinned by `wake`/
/// `start` before they get one. Pure for unit tests.
fn taps_to_repin(vms: &[Vm]) -> Vec<(String, u32, String)> {
    vms.iter()
        .filter(|v| matches!(v.state, VmState::Running | VmState::Paused | VmState::Pooled))
        .filter_map(|v| Some((v.id.clone(), v.slot?, v.ip.clone()?)))
        .collect()
}

/// Bring a guest's tap up and pin it to the address that slot was allocated.
/// Returns false when the port is NOT pinned, which the caller must treat as
/// a failure to bring the VM up.
///
/// The tenant ruleset keys on `ip saddr . ip daddr`, and both of those are
/// addresses the guest configures for itself. Without a filter at the tap's
/// own ingress a guest can add a same-tenant peer's address and reach that
/// tenant's VMs, or ARP for any victim's address and receive the ingress
/// DNAT'd to it. A tap on the bridge that could not be pinned is exactly the
/// dangerous state, so a failure tears it back down rather than leaving it.
///
/// `guest_mac` is not optional, on any path. It used to be, and the two paths
/// that passed "" — wake and fork — rendered a chain with no `ether saddr`
/// rule at all: that guest still could not claim another address, but it could
/// emit frames carrying another tenant's MAC, which moves that tenant's FDB
/// entry onto this port and delivers their inbound traffic here. Sleep/wake is
/// tenant-driven, so an attacker could put their own VM into that state on
/// demand. The MAC is derived from the slot (`tap_pin_for_slot`), and the slot
/// survives sleep, wake and fork, so every path has one.
///
/// `unpinned` says what happens to a port that could not be pinned. Either way
/// it must not carry traffic; see `Unpinned` for why the two cases differ.
fn ensure_pinned_tap(
    tap: &str,
    ip: Option<&str>,
    guest_mac: &net::GuestMac,
    unpinned: Unpinned,
) -> Result<(), PinFailure> {
    if !net::ensure_tap(tap) {
        return Err(PinFailure::NotSetUp);
    }
    // No allocated address means nothing to pin the port to, so the guest
    // behind it could claim any.
    let Some(ip) = ip else {
        warn!(tap = %tap, "tap has no allocated address to pin");
        match unpinned {
            Unpinned::Remove => net::delete_tap(tap),
            Unpinned::TakeDown => net::set_link_down(tap),
        }
        return Err(PinFailure::Unpinned);
    };
    if !net::ensure_tap_antispoof(tap, ip, guest_mac) {
        match unpinned {
            Unpinned::Remove => {
                net::clear_tap_antispoof(tap);
                net::delete_tap(tap);
            }
            // Whatever chain is already on the device stays; the link is down.
            Unpinned::TakeDown => net::set_link_down(tap),
        }
        return Err(PinFailure::Unpinned);
    }
    // Pinned first, raised after (W8): an adopted guest's tap found down
    // used to come up before its pin was back.
    if !net::raise_tap(tap) {
        match unpinned {
            Unpinned::Remove => {
                net::clear_tap_antispoof(tap);
                net::delete_tap(tap);
            }
            Unpinned::TakeDown => net::set_link_down(tap),
        }
        return Err(PinFailure::NotRaised);
    }
    Ok(())
}

/// Why `ensure_pinned_tap` did not pin a tap (X12: start-up says which).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum PinFailure {
    /// The tap could not be created, put on the bridge or have IPv6 turned
    /// off: nothing was taken down, there may be nothing on the bridge.
    NotSetUp,
    /// No address, or the pin was refused: the port is down (or removed).
    Unpinned,
    /// Pinned, but it could not be brought up.
    NotRaised,
}

/// What `ensure_pinned_tap` does with a port it could not pin.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Unpinned {
    /// Nothing is attached to the tap yet (create, wake, fork, start): remove
    /// it. The caller fails the operation and no guest ever sees the device.
    Remove,
    /// A live guest is attached (a VMM adopted at agent start-up): take the
    /// link down and keep the device. Removing it would leave Firecracker
    /// holding a dead fd, and a tap created later under the same name is a
    /// different device that the running guest is not on — its network would
    /// be gone until a cold boot, however soon nft works again. A down link
    /// drops every frame just the same, and `net::ensure_tap` brings it back
    /// up at the next agent start that manages to pin it.
    TakeDown,
}

/// Create an instance directory 0700, tightening one an older agent left
/// world-traversable.
///
/// Everything a tenant owns lands in here: rootfs.ext4, vmstate.bin and
/// mem.bin — a verbatim dump of the guest's RAM — plus the Firecracker API
/// socket and the guest vsock, both created by the FC child inheriting our
/// umask. Under the default 0022 that directory is 0755 and any local account
/// on the worker can read another tenant's memory or drive their VM. The mode
/// goes on the directory rather than each file so FC's own files are covered
/// too. Same explicit-mode discipline as wg.rs's key and state files.
fn create_instance_dir(path: &str) -> std::io::Result<()> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(path)?;
    // recursive(true) is a no-op on an existing directory, which is exactly
    // the upgrade case: instance dirs outlive the agent that made them.
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
}

/// Create an instance directory that must not exist yet, 0700. Unlike
/// `create_instance_dir`, an existing directory is an error
/// (`AlreadyExists`): a fork child's directory is new or it is someone
/// else's.
fn create_new_instance_dir(path: &str) -> std::io::Result<()> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    std::fs::DirBuilder::new().mode(0o700).create(path)?;
    // The mode above is subject to the umask; as in `create_instance_dir`.
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
}

/// Open (and truncate) an instance's serial.log 0600. The guest writes its
/// console here, which routinely carries boot secrets and application logs.
fn open_serial_log(path: &str) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)
}

/// `cp --reflink=auto src dst` (CoW when possible, plain copy fallback).
fn copy_rootfs(src: &str, dst: &str) -> Result<(), String> {
    let ok = std::process::Command::new("cp")
        .arg("--reflink=auto")
        .arg(src).arg(dst)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false);

    if ok { return Ok(()); }

    // Plain copy fallback.
    std::fs::copy(src, dst)
        .map(|_| ())
        .map_err(|e| format!("copy {} → {}: {}", src, dst, e))
}

fn kill_and_reap(pid: i32) {
    use nix::sys::signal::{kill, Signal};
    use nix::unistd::Pid;
    let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
    // Reap best-effort; FCs adopted after agent restart are reparented to init.
    unsafe {
        libc::waitpid(pid, std::ptr::null_mut(), 0);
    }
}

fn now_ms() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    // ---- id validation (the id is a filesystem path component) ----

    #[test]
    fn test_valid_vm_id_accepts_the_ids_we_actually_issue() {
        assert!(valid_vm_id("sb-0123456789ab"));
        assert!(valid_vm_id("sb-aabbccdd-1"));
        assert!(valid_vm_id("pool-18f3a2b1c9d"));
        assert!(valid_vm_id("vm-1"));
        assert!(valid_vm_id("A"));
        assert!(valid_vm_id("a.b_c-d"));
        assert!(valid_vm_id(&"a".repeat(64)));
    }

    #[test]
    fn test_valid_vm_id_rejects_traversal() {
        // Was: these went straight into {data_dir}/instances/<id>, giving a
        // root-level write and a recursive delete anywhere the unit allows.
        assert!(!valid_vm_id("../../../../etc/felucca"));
        // axum percent-decodes :id captures, so this is what the handler sees.
        assert!(!valid_vm_id("../../../etc/felucca"));
        assert!(!valid_vm_id(".."));
        assert!(!valid_vm_id("."));
        assert!(!valid_vm_id("a/b"));
        assert!(!valid_vm_id("/etc/felucca"));
        assert!(!valid_vm_id("a..b"));
        assert!(!valid_vm_id(""));
        assert!(!valid_vm_id(&"a".repeat(65)));
        // Leading '.' / '-' are hidden files and option smuggling.
        assert!(!valid_vm_id(".hidden"));
        assert!(!valid_vm_id("-rf"));
        // Nothing outside the class, including separators and NULs.
        assert!(!valid_vm_id("vm 1"));
        assert!(!valid_vm_id("vm\n1"));
        assert!(!valid_vm_id("vm\01"));
        assert!(!valid_vm_id("vm$(id)"));
    }

    #[test]
    fn test_instance_path_refuses_to_leave_the_instances_dir() {
        assert_eq!(
            instance_path("/srv/felucca", "vm-1").as_deref(),
            Some("/srv/felucca/instances/vm-1")
        );
        assert!(instance_path("/srv/felucca", "../../etc/felucca").is_none());
        assert!(instance_path("/srv/felucca", "").is_none());
    }

    fn mgr_for(data_dir: String) -> Arc<Manager> {
        Manager::new(
            data_dir,
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
        )
    }

    #[tokio::test]
    async fn test_delete_traversal_id_does_not_remove_anything_outside_instances() {
        // DELETE /v1/vms/..%2F..%2F..%2Fetc%2Ffelucca used to reach
        // remove_dir_all on the decoded path without the VM having to exist.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().to_str().unwrap().to_string();
        let data_dir = format!("{}/srv", root);
        std::fs::create_dir_all(format!("{}/instances", data_dir)).unwrap();
        let victim = format!("{}/etc/felucca", root);
        std::fs::create_dir_all(&victim).unwrap();
        std::fs::write(format!("{}/agent.json", victim), b"token").unwrap();

        let mgr = mgr_for(data_dir);
        mgr.delete("../../etc/felucca").await.expect("delete is idempotent");

        assert!(std::fs::metadata(&victim).is_ok(), "config dir must survive");
        assert!(std::fs::metadata(format!("{}/agent.json", victim)).is_ok());
    }

    #[tokio::test]
    async fn test_create_rejects_traversal_id_before_touching_the_filesystem() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().to_str().unwrap().to_string();
        let data_dir = format!("{}/srv", root);
        std::fs::create_dir_all(format!("{}/instances", data_dir)).unwrap();

        let mgr = mgr_for(data_dir.clone());
        let spec = CreateSpec {
            id: "../../etc/felucca".into(),
            name: "x".into(),
            vcpus: 1,
            mem_mib: 256,
            tenant_id: None,
            image: DEFAULT_IMAGE.into(),
            image_sha256: None,
            disk_gb: 0,
        };
        assert_eq!(mgr.create(&spec).await.unwrap_err(), "InvalidId");
        // No record, and nothing created outside the instances tree.
        assert!(mgr.list_json().await.contains("\"vms\":[]"));
        assert!(std::fs::metadata(format!("{}/etc", root)).is_err());
    }

    #[tokio::test]
    async fn test_fork_rejects_traversal_child_id() {
        let dir = tempfile::tempdir().unwrap();
        let data_dir = dir.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances", data_dir)).unwrap();
        let mgr = mgr_for(data_dir);
        // Rejected before the parent lookup, so an unknown parent is not what
        // this asserts.
        assert_eq!(
            mgr.fork("vm-1", "../../etc/felucca", "child").await.unwrap_err(),
            "InvalidId"
        );
    }

    // ---- per-tap anti-spoof wiring ----

    #[test]
    fn test_fc_nic_mac_is_the_mac_the_tap_is_pinned_to() {
        // Was: the NIC was attached with no guest_mac at all, so there was no
        // stable L2 identity to filter on. The two derivations must stay the
        // same value — a NIC configured with any other MAC has every frame
        // dropped by its own tap's pin.
        for slot in [0u32, 1, 3, 253] {
            let (tap, mac) = tap_pin_for_slot(slot);
            let body = fc::build_network_interface(&tap, &mac);
            assert!(
                body.contains(&format!("\"guest_mac\":\"{}\"", mac)),
                "slot {slot}: {body}"
            );
            assert!(body.contains(&format!("\"host_dev_name\":\"{}\"", tap)));
        }
    }

    #[test]
    fn test_every_path_pins_a_slot_to_one_tap_and_one_mac() {
        // create, start, wake, fork and startup adoption all pin through
        // `tap_pin_for_slot`, so none of them can pin a different L2 identity
        // than `configure_guest` gave the NIC — and, since a GuestMac has no
        // empty inhabitant, none of them can pin no identity at all. That was
        // the residual hole: `ensure_pinned_tap(&tap, ip, "")` on the wake and
        // fork paths compiled, and rendered a chain with no `ether saddr` rule.
        for slot in [0u32, 1, 7, 253] {
            let (tap, mac) = tap_pin_for_slot(slot);
            assert_eq!(tap, net::tap_name(slot));
            assert_eq!(mac, net::guest_mac_for_slot(slot));
            assert!(!mac.as_str().is_empty(), "slot {slot} pinned to no MAC");
        }
        // Distinct per slot, or one tenant's pin would admit another's frames.
        assert_ne!(tap_pin_for_slot(7).1, tap_pin_for_slot(8).1);
        assert_ne!(tap_pin_for_slot(7).0, tap_pin_for_slot(8).0);
    }

    #[test]
    fn test_fork_re_mac_targets_the_pinned_mac() {
        // The fork child used to get a clock-salted MAC, which no per-tap pin
        // could ever match. It must be the slot's derived MAC, and the same one
        // a cold boot would give that slot — the fork path now takes both from
        // `tap_pin_for_slot(child_slot)` rather than re-deriving from an
        // `unwrap_or(0)` that would have re-MACed to slot 0's address.
        let slot = 7u32;
        assert_eq!(tap_pin_for_slot(slot).1, net::guest_mac_for_slot(slot));
        assert_ne!(net::guest_mac_for_slot(slot), net::guest_mac_for_slot(slot + 1));
    }

    #[test]
    fn test_there_is_exactly_one_firecracker_spawn_site() {
        // Create/start, wake and fork each used to carry their own copy of
        // the spawn, and each copy closed the serial.log descriptor twice.
        // Whatever every VMM must get at birth is only guaranteed while there
        // is one place that gives it. Structural, like the tap test below.
        let src = include_str!("mod.rs");
        let prod = src.split("#[cfg(test)]").next().expect("production half of mod.rs");
        let code: String = prod
            .lines()
            .map(|l| l.split("//").next().unwrap_or(""))
            .collect::<Vec<_>>()
            .join("\n");
        // The binary is `self.fc_bin` ("firecracker" outside unit tests).
        let needle = ["Command::new(", "&self.fc_bin)"].concat();
        assert_eq!(
            code.matches(needle.as_str()).count(),
            1,
            "every Firecracker must be started by Manager::spawn_firecracker"
        );
        // No handle is built from a raw descriptor any more: that is how one
        // fd came to be owned, and closed, twice.
        let raw = ["from_raw", "_fd("].concat();
        assert_eq!(code.matches(raw.as_str()).count(), 0, "a raw fd wrapped in an owning handle");
    }

    #[test]
    fn test_tap_is_created_before_it_is_pinned_on_every_path() {
        // The netdev chain binds to the tap itself
        // (`hook ingress device "hth-N"`), so if the device does not exist yet
        // `nft -f -` rejects the ENTIRE transaction with ENOENT — not just the
        // chain line. `ensure_tap_antispoof` then returns false and
        // `ensure_pinned_tap` tears the port back down, which is the right
        // fail-closed direction but means a path that pins before it creates
        // can never bring a guest up at all.
        //
        // The inverse is the dangerous one: a path that creates a tap and
        // never pins it leaves a port on the bridge with no ingress filter,
        // and the guest behind it can claim any tenant's IP or MAC. Ordering
        // is therefore not a convention to remember — both calls are funnelled
        // through `ensure_pinned_tap`, which is the only place either name
        // appears. This test is what keeps that true: a second call site, or a
        // reordering inside the funnel, fails here rather than in production.
        //
        // Scanning the source is deliberate. The alternative is mocking the
        // `ip`/`nft` subprocesses, which would test the mock; the property
        // being defended is structural ("there is exactly one call site"), so
        // the check is structural too.
        let src = include_str!("mod.rs");
        let prod = src
            .split("#[cfg(test)]")
            .next()
            .expect("production half of mod.rs");
        // Comments discuss these functions by name; only real calls count.
        let code: String = prod
            .lines()
            .map(|l| l.split("//").next().unwrap_or(""))
            .collect::<Vec<_>>()
            .join("\n");

        let create = code.match_indices("net::ensure_tap(").collect::<Vec<_>>();
        let pin = code
            .match_indices("net::ensure_tap_antispoof(")
            .collect::<Vec<_>>();
        assert_eq!(
            create.len(),
            1,
            "expected exactly one net::ensure_tap call site, found {} — every tap \
             must be created inside ensure_pinned_tap so it cannot skip the pin",
            create.len()
        );
        assert_eq!(
            pin.len(),
            1,
            "expected exactly one net::ensure_tap_antispoof call site, found {} — a \
             second one could run before the tap exists, where nft rejects the whole \
             transaction and the pin silently does not apply",
            pin.len()
        );

        // Both inside ensure_pinned_tap, creation first.
        let fn_start = code
            .find("fn ensure_pinned_tap(")
            .expect("ensure_pinned_tap must exist: it is the funnel both calls go through");
        let fn_end = code[fn_start..]
            .find("\nfn ")
            .map(|o| fn_start + o)
            .unwrap_or(code.len());
        assert!(
            create[0].0 > fn_start && create[0].0 < fn_end,
            "net::ensure_tap is called outside ensure_pinned_tap"
        );
        assert!(
            pin[0].0 > fn_start && pin[0].0 < fn_end,
            "net::ensure_tap_antispoof is called outside ensure_pinned_tap"
        );
        assert!(
            create[0].0 < pin[0].0,
            "ensure_pinned_tap pins the tap before creating it: nft rejects a netdev \
             chain whose device does not exist, so the guest would never come up"
        );
    }

    #[test]
    fn test_taps_to_repin_covers_every_adopted_live_guest() {
        // Nothing re-pinned an adopted VM's tap: the netdev chains are gone
        // after a host reboot, and an agent upgraded in place adopts guests
        // started before the pin existed, so those ports stayed unfiltered on
        // the bridge for the life of the sandbox.
        let mut running = pooled_vm("sb-run", DEFAULT_IMAGE, 1, 256, 0);
        running.state = VmState::Running;
        running.slot = Some(3);
        running.ip = Some("10.231.0.5".into());
        let mut paused = pooled_vm("sb-paused", DEFAULT_IMAGE, 1, 256, 0);
        paused.state = VmState::Paused;
        paused.slot = Some(4);
        paused.ip = Some("10.231.0.6".into());
        // No live tap of their own: `wake`/`start` pin these before they get one.
        let mut sleeping = pooled_vm("sb-sleep", DEFAULT_IMAGE, 1, 256, 0);
        sleeping.state = VmState::Sleeping;
        sleeping.slot = Some(5);
        let mut stopped = pooled_vm("sb-stop", DEFAULT_IMAGE, 1, 256, 0);
        stopped.state = VmState::Stopped;
        stopped.slot = Some(6);
        // Running but holding no address — nothing to pin the port to.
        let mut no_ip = pooled_vm("sb-noip", DEFAULT_IMAGE, 1, 256, 0);
        no_ip.state = VmState::Running;
        no_ip.slot = Some(7);
        no_ip.ip = None;
        // A pool VM adopted with its VMM (ADR-0011). The claim path pins
        // nothing, so start-up is the only place this port gets re-pinned
        // before the VM is handed to a tenant.
        let mut pooled = pooled_vm("pool-8", DEFAULT_IMAGE, 1, 256, 0);
        pooled.slot = Some(8);
        pooled.ip = Some("10.231.0.10".into());

        let repin = taps_to_repin(&[running, paused, sleeping, stopped, no_ip, pooled]);
        assert_eq!(
            repin,
            vec![
                ("sb-run".to_string(), 3u32, "10.231.0.5".to_string()),
                ("sb-paused".to_string(), 4u32, "10.231.0.6".to_string()),
                ("pool-8".to_string(), 8u32, "10.231.0.10".to_string()),
            ]
        );
        // And each one is re-pinned to the same tap+MAC its cold boot used.
        let (tap, mac) = tap_pin_for_slot(repin[0].1);
        assert_eq!(tap, "hth-3");
        assert_eq!(mac, net::guest_mac_for_slot(3));
    }

    #[tokio::test]
    async fn test_isolation_status_is_vacuously_true_with_networking_off() {
        // A --net off node has no guest network to fence, so it must not
        // report itself unisolated to the health probe or the heartbeat, and
        // creates on it must not be failed closed.
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        assert!(mgr.isolation_ok());
        assert!(mgr.refresh_isolation().await);
    }

    // ---- /v1/vms JSON escaping ----

    #[tokio::test]
    async fn test_list_json_escapes_a_forging_name() {
        // Was: push_kv_str concatenated the raw value, so this name — which a
        // tenant chooses freely on sandbox create — forged state/pid fields.
        let forge = r#"x","state":"running","pid":1,"z":""#;
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        {
            let mut g = mgr.inner.lock().await;
            let mut vm = pooled_vm("sb-1", DEFAULT_IMAGE, 1, 256, 0);
            vm.name = forge.to_string();
            vm.state = VmState::Stopped;
            vm.pid = None;
            g.vms.push(vm);
        }

        let body = mgr.list_json().await;
        let parsed: serde_json::Value = serde_json::from_str(&body).expect("body must parse");
        let vm = &parsed["vms"][0];
        // The payload survives as data, inert: it is the name and nothing else.
        assert_eq!(vm["name"], serde_json::json!(forge));
        assert_eq!(vm["state"], serde_json::json!("stopped"));
        assert_eq!(vm["pid"], serde_json::Value::Null);
        assert_eq!(parsed["vms"].as_array().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn test_list_json_escapes_control_characters_in_id_and_name() {
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        {
            let mut g = mgr.inner.lock().await;
            let mut vm = pooled_vm("sb-1", DEFAULT_IMAGE, 1, 256, 0);
            vm.name = "line\nbreak\ttab\\slash\"quote".into();
            g.vms.push(vm);
        }
        let body = mgr.list_json().await;
        let parsed: serde_json::Value = serde_json::from_str(&body).expect("body must parse");
        assert_eq!(
            parsed["vms"][0]["name"],
            serde_json::json!("line\nbreak\ttab\\slash\"quote")
        );
        assert_eq!(parsed["vms"][0]["ip"], serde_json::json!("10.231.0.2"));
    }

    // ---- instance dir / serial.log permissions ----

    #[test]
    fn test_instance_dir_is_root_only() {
        use std::os::unix::fs::PermissionsExt;
        // Was: create_dir_all with no mode → 0755 under the default umask, so
        // any local account could read mem.bin (the guest's whole RAM).
        let dir = tempfile::tempdir().unwrap();
        let path = format!("{}/instances/vm-1", dir.path().to_str().unwrap());
        create_instance_dir(&path).unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o700, "got {:o}", mode);
        // The parent the recursive create made is not a way around it either.
        let parent = format!("{}/instances", dir.path().to_str().unwrap());
        let pmode = std::fs::metadata(&parent).unwrap().permissions().mode() & 0o777;
        assert_eq!(pmode, 0o700, "got {:o}", pmode);
    }

    #[test]
    fn test_instance_dir_tightens_a_world_readable_dir_from_an_older_agent() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let path = format!("{}/vm-old", dir.path().to_str().unwrap());
        std::fs::create_dir_all(&path).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
        create_instance_dir(&path).unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o700, "got {:o}", mode);
    }

    #[test]
    fn test_serial_log_is_root_only() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let path = format!("{}/serial.log", dir.path().to_str().unwrap());
        let f = open_serial_log(&path).unwrap();
        drop(f);
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "got {:o}", mode);
    }

    // ---- FLU-6: node ports round-robin, avoid and prefer ----
    //
    // These replace four tests that pinned lowest-free on purpose: lowest-free
    // handed the port a sandbox had just released to the very next expose,
    // another tenant's, while the gateways still sent the old hostname there.

    fn ports(p: &[u16]) -> HashSet<u16> {
        p.iter().copied().collect()
    }

    const RANGE: std::ops::RangeInclusive<u16> = NODE_PORT_MIN..=NODE_PORT_MAX;

    #[test]
    fn test_node_ports_go_round_robin_from_the_cursor() {
        let none = HashSet::new();
        let mut used = HashSet::new();
        let mut cursor = 24000;
        let mut got = Vec::new();
        for _ in 0..4 {
            let (p, next) = pick_node_port(&used, &none, None, cursor, RANGE).unwrap();
            got.push(p);
            used.insert(p);
            cursor = next;
        }
        assert_eq!(got, [24000, 24001, 24002, 24003]);
        // The cursor is the port after the last one handed out.
        assert_eq!(cursor, 24004);
        // A port released behind the cursor is not the next one handed out.
        used.remove(&24001);
        assert_eq!(pick_node_port(&used, &none, None, cursor, RANGE), Some((24004, 24005)));
    }

    #[test]
    fn test_node_ports_wrap_around_once() {
        let none = HashSet::new();
        // At the top of the range: the cursor wraps to the bottom.
        assert_eq!(pick_node_port(&none, &none, None, NODE_PORT_MAX, RANGE), Some((NODE_PORT_MAX, NODE_PORT_MIN)));
        // Everything from the cursor up is taken: the search wraps.
        let used: HashSet<u16> = (29990..=NODE_PORT_MAX).collect();
        assert_eq!(pick_node_port(&used, &none, None, 29990, RANGE), Some((NODE_PORT_MIN, NODE_PORT_MIN + 1)));
        // Only a port below the cursor is free: found after the wrap.
        let used: HashSet<u16> = RANGE.filter(|p| *p != 21000).collect();
        assert_eq!(pick_node_port(&used, &none, None, 25000, RANGE), Some((21000, 21001)));
        // A cursor outside the range starts at the bottom.
        assert_eq!(pick_node_port(&none, &none, None, 80, RANGE), Some((NODE_PORT_MIN, NODE_PORT_MIN + 1)));
    }

    #[test]
    fn test_node_ports_skip_ports_in_use_and_avoided() {
        let used = ports(&[20000, 20002]);
        let avoid = ports(&[20001, 20003]);
        assert_eq!(pick_node_port(&used, &avoid, None, 20000, RANGE), Some((20004, 20005)));
        // Exhaustively over a small range: never a port in use or avoided,
        // always the first one after the cursor that is neither.
        let small = 100..=109u16;
        for mask in 0u32..(1 << 10) {
            let used: HashSet<u16> = (0..10).filter(|i| mask & (1 << i) != 0).map(|i| 100 + i as u16).collect();
            for avoid_mask in [0u32, 0b1010101010, 0b0000011111] {
                let avoid: HashSet<u16> = (0..10).filter(|i| avoid_mask & (1 << i) != 0).map(|i| 100 + i as u16).collect();
                for cursor in small.clone() {
                    let want = (0..10u16)
                        .map(|i| 100 + (cursor - 100 + i) % 10)
                        .find(|p| !used.contains(p) && !avoid.contains(p));
                    let got = pick_node_port(&used, &avoid, None, cursor, small.clone());
                    assert_eq!(got.map(|g| g.0), want, "used {used:?} avoid {avoid:?} cursor {cursor}");
                    if let Some((p, next)) = got {
                        assert_eq!(next, if p == 109 { 100 } else { p + 1 });
                    }
                }
            }
        }
    }

    #[test]
    fn test_node_ports_prefer_is_honoured_only_when_free_and_in_range() {
        let none = HashSet::new();
        // Free: returned, even though it is in avoid (it is in avoid because
        // it is quarantined for this very mapping), and the cursor stays.
        assert_eq!(pick_node_port(&none, &ports(&[21000]), Some(21000), 24000, RANGE), Some((21000, 24000)));
        // In use by a sandbox here: ignored, round-robin instead.
        assert_eq!(pick_node_port(&ports(&[21000]), &none, Some(21000), 24000, RANGE), Some((24000, 24001)));
        // Outside the node-port range: ignored.
        assert_eq!(pick_node_port(&none, &none, Some(8080), 24000, RANGE), Some((24000, 24001)));
        assert_eq!(pick_node_port(&none, &none, Some(30000), 24000, RANGE), Some((24000, 24001)));
        // Nothing else left: prefer still wins when it is free.
        let avoid: HashSet<u16> = RANGE.collect();
        assert_eq!(pick_node_port(&none, &avoid, Some(29999), 20000, RANGE), Some((29999, 20000)));
    }

    #[test]
    fn test_node_ports_exhausted_when_in_use_and_avoid_cover_the_range() {
        let used: HashSet<u16> = (NODE_PORT_MIN..25000).collect();
        let mut avoid: HashSet<u16> = (25000..=NODE_PORT_MAX).collect();
        for cursor in [NODE_PORT_MIN, 24999, 25000, NODE_PORT_MAX] {
            assert_eq!(pick_node_port(&used, &avoid, None, cursor, RANGE), None);
            // A prefer that is in use does not help.
            assert_eq!(pick_node_port(&used, &avoid, Some(20000), cursor, RANGE), None);
        }
        // Lifting any one port makes exactly that port allocatable again.
        avoid.remove(&25555);
        assert_eq!(pick_node_port(&used, &avoid, None, 20000, RANGE), Some((25555, 25556)));
    }

    #[test]
    fn test_node_ports_avoid_entries_outside_the_range_are_harmless() {
        let none = HashSet::new();
        let avoid = ports(&[0, 80, 8069, 19999, 30000, 65535]);
        assert_eq!(pick_node_port(&none, &avoid, None, 20000, RANGE), Some((20000, 20001)));
        assert_eq!(pick_node_port(&none, &avoid, None, NODE_PORT_MAX, RANGE), Some((NODE_PORT_MAX, NODE_PORT_MIN)));
        // Ports in use outside the range do not block either.
        assert_eq!(pick_node_port(&ports(&[80, 30000]), &none, None, 20000, RANGE), Some((20000, 20001)));
    }

    #[test]
    fn test_the_node_port_cursor_starts_at_a_random_port_of_the_range() {
        for _ in 0..1000 {
            assert!(RANGE.contains(&random_port_in(RANGE)));
        }
        // Not one fixed place (1000 draws from 10 000 ports, all equal: no).
        let draws: HashSet<u16> = (0..1000).map(|_| random_port_in(RANGE)).collect();
        assert!(draws.len() > 1);
        // The fallback seed's mapping stays in the range at both ends.
        for seed in [0u64, 1, 9_999, 10_000, u64::MAX, u64::MAX - 1] {
            assert!(RANGE.contains(&port_from_seed(seed, RANGE)), "{seed}");
        }
        assert_eq!(port_from_seed(0, RANGE), NODE_PORT_MIN);
        assert_eq!(port_from_seed(9_999, RANGE), NODE_PORT_MAX);
        // A manager starts its cursor inside the range.
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        assert!(RANGE.contains(&mgr.inner.try_lock().unwrap().node_port_cursor));
    }

    // ---- v4 P4: pool specs + matching ----

    fn good_spec() -> PoolSpec {
        PoolSpec {
            image: "odoo-v18".into(),
            image_sha256: "a".repeat(64),
            vcpus: 2,
            mem_mib: 2048,
            disk_gb: 8,
            count: 2,
        }
    }

    #[test]
    fn test_pool_spec_validation_accepts_good() {
        assert!(validate_pool_spec(&good_spec()).is_ok());
        // Empty sha means "trust the local file".
        let mut s = good_spec();
        s.image_sha256 = String::new();
        assert!(validate_pool_spec(&s).is_ok());
        // count 0 drains the pool — valid.
        let mut s = good_spec();
        s.count = 0;
        assert!(validate_pool_spec(&s).is_ok());
        // Boundary values.
        let mut s = good_spec();
        s.vcpus = 16;
        s.mem_mib = 32768;
        s.disk_gb = 128;
        s.count = 8;
        assert!(validate_pool_spec(&s).is_ok());
        let mut s = good_spec();
        s.vcpus = 1;
        s.mem_mib = 64;
        s.disk_gb = 0;
        assert!(validate_pool_spec(&s).is_ok());
    }

    #[test]
    fn test_pool_spec_validation_rejects_bad() {
        let mut s = good_spec();
        s.image = "../etc".into();
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.image = "UPPER".into();
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.image_sha256 = "xyz".into();
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.vcpus = 0;
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.vcpus = 17;
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.mem_mib = 63;
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.mem_mib = 32769;
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.disk_gb = 129;
        assert!(validate_pool_spec(&s).is_err());
        let mut s = good_spec();
        s.count = 9;
        assert!(validate_pool_spec(&s).is_err());
    }

    #[test]
    fn test_pool_spec_wire_shape() {
        // The pinned two-sided contract: feluccad sends this exact shape.
        let body = r#"[{"image":"odoo-v18","image_sha256":"","vcpus":2,"mem_mib":2048,"disk_gb":8,"count":2}]"#;
        let specs: Vec<PoolSpec> = serde_json::from_str(body).expect("parse pool specs");
        assert_eq!(specs.len(), 1);
        assert_eq!(specs[0].image, "odoo-v18");
        assert_eq!(specs[0].image_sha256, "");
        assert_eq!(specs[0].vcpus, 2);
        assert_eq!(specs[0].mem_mib, 2048);
        assert_eq!(specs[0].disk_gb, 8);
        assert_eq!(specs[0].count, 2);
        // image_sha256/disk_gb may be omitted; they default.
        let body = r#"[{"image":"ubuntu-base","vcpus":1,"mem_mib":256,"count":1}]"#;
        let specs: Vec<PoolSpec> = serde_json::from_str(body).expect("parse minimal spec");
        assert_eq!(specs[0].image_sha256, "");
        assert_eq!(specs[0].disk_gb, 0);
    }

    // ---- v4 P5.4: image-cache GC ----

    #[test]
    fn test_gc_image_for_entry_owned_files_only() {
        assert_eq!(gc_image_for_entry("odoo-v18.ext4"), Some("odoo-v18".into()));
        assert_eq!(
            gc_image_for_entry("odoo-v18.ext4.sha256"),
            Some("odoo-v18".into())
        );
        assert_eq!(
            gc_image_for_entry(".odoo-v18.partial"),
            Some("odoo-v18".into())
        );
        // Not ours: never candidates.
        assert_eq!(gc_image_for_entry("notes.txt"), None);
        assert_eq!(gc_image_for_entry("rootfs.ext4.bak"), None);
        assert_eq!(gc_image_for_entry(".hidden"), None);
        assert_eq!(gc_image_for_entry("ext4"), None);
        // Invalid image names (traversal etc.) are not ours either.
        assert_eq!(gc_image_for_entry("..%2Fetc.ext4"), None);
        assert_eq!(gc_image_for_entry(".ext4"), None);
    }

    #[tokio::test]
    async fn test_gc_images_sweeps_only_unreferenced() {
        let dir = tempfile::tempdir().unwrap();
        let data_dir = dir.path().to_str().unwrap().to_string();
        let images = format!("{}/images", data_dir);
        std::fs::create_dir_all(&images).unwrap();
        for f in [
            "ubuntu-base.ext4",        // DEFAULT_IMAGE: never collected
            "vm-ref.ext4",             // referenced by a VM record
            "pool-ref.ext4",           // referenced by a pool spec
            "pool-ref.ext4.sha256",    // sidecar of a referenced image
            "stale.ext4",              // unreferenced -> collected
            "stale.ext4.sha256",       // unreferenced sidecar -> collected
            ".stale.partial",          // unreferenced partial -> collected
            "stranger.txt",            // not ours -> untouched
        ] {
            std::fs::write(format!("{}/{}", images, f), b"x").unwrap();
        }

        let mgr = Manager::new(
            data_dir,
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
        );
        {
            let mut g = mgr.inner.lock().await;
            g.vms.push(pooled_vm("vm-1", "vm-ref", 1, 256, 0));
            g.template_pools.push(PoolSpec {
                image: "pool-ref".into(),
                image_sha256: "a".repeat(64),
                vcpus: 1,
                mem_mib: 256,
                disk_gb: 0,
                count: 1,
            });
        }

        mgr.gc_images(0).await;

        let left: std::collections::HashSet<String> = std::fs::read_dir(&images)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        let want: std::collections::HashSet<String> = [
            "ubuntu-base.ext4",
            "vm-ref.ext4",
            "pool-ref.ext4",
            "pool-ref.ext4.sha256",
            "stranger.txt",
        ]
        .into_iter()
        .map(String::from)
        .collect();
        assert_eq!(left, want);
    }

    #[tokio::test]
    async fn test_gc_images_age_gate_spares_fresh_files() {
        let dir = tempfile::tempdir().unwrap();
        let data_dir = dir.path().to_str().unwrap().to_string();
        let images = format!("{}/images", data_dir);
        std::fs::create_dir_all(&images).unwrap();
        std::fs::write(format!("{}/fresh.ext4", images), b"x").unwrap();

        let mgr = Manager::new(
            data_dir,
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
        );
        // Unreferenced, but younger than the age gate -> spared.
        mgr.gc_images(3600).await;
        assert!(std::fs::metadata(format!("{}/fresh.ext4", images)).is_ok());
    }

    fn pooled_vm(id: &str, image: &str, vcpus: u32, mem_mib: u64, disk_gb: u32) -> Vm {
        Vm {
            id: id.into(),
            name: "pool".into(),
            dir_id: id.into(),
            vcpus,
            mem_mib,
            state: VmState::Pooled,
            pid: Some(1),
            slot: Some(0),
            ip: Some("10.231.0.2".into()),
            vsock: true,
            tenant_id: None,
            exposes: Vec::new(),
            image: image.into(),
            image_sha256: None,
            disk_gb,
            snapshot_valid: false,
        }
    }

    #[test]
    fn test_pool_match_picks_only_matching_shape() {
        let vms = vec![
            pooled_vm("pool-1", "ubuntu-base", 1, 256, 0),
            pooled_vm("pool-2", "odoo-v18", 2, 2048, 8),
        ];
        // Exact match on each shape.
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 1, 256, 0), Some(0));
        assert_eq!(find_pooled_match(&vms, "odoo-v18", None, 2, 2048, 8), Some(1));
        // Any differing dimension misses.
        assert_eq!(find_pooled_match(&vms, "odoo-v18", None, 1, 256, 0), None);
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 2, 256, 0), None);
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 1, 512, 0), None);
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 1, 256, 8), None);
        assert_eq!(find_pooled_match(&vms, "docker-base", None, 2, 2048, 8), None);
    }

    #[test]
    fn test_pool_match_requires_same_sha() {
        // A re-captured template (same name, new sha) must NEVER be served a
        // pooled VM prewarmed from the old image.
        let old_sha = "a".repeat(64);
        let new_sha = "b".repeat(64);
        let mut warm = pooled_vm("pool-1", "odoo-v18", 2, 2048, 8);
        warm.image_sha256 = Some(old_sha.clone());
        let vms = vec![warm];
        assert_eq!(find_pooled_match(&vms, "odoo-v18", Some(&old_sha), 2, 2048, 8), Some(0));
        assert_eq!(find_pooled_match(&vms, "odoo-v18", Some(&new_sha), 2, 2048, 8), None);
        // Sha-less request never claims a sha-pinned pool VM and vice versa.
        assert_eq!(find_pooled_match(&vms, "odoo-v18", None, 2, 2048, 8), None);
        let bare = vec![pooled_vm("pool-2", "odoo-v18", 2, 2048, 8)];
        assert_eq!(find_pooled_match(&bare, "odoo-v18", Some(&old_sha), 2, 2048, 8), None);
    }

    #[test]
    fn test_pool_match_skips_non_pooled_states() {
        let mut running = pooled_vm("vm-1", "ubuntu-base", 1, 256, 0);
        running.state = VmState::Running;
        let mut errored = pooled_vm("pool-x", "ubuntu-base", 1, 256, 0);
        errored.state = VmState::Error;
        let mut claimed = pooled_vm("pool-y", "ubuntu-base", 1, 256, 0);
        claimed.state = VmState::Creating; // mid-claim marker
        let vms = vec![running, errored, claimed];
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 1, 256, 0), None);
    }

    #[test]
    fn test_pool_match_first_in_order_wins() {
        let vms = vec![
            pooled_vm("pool-a", "ubuntu-base", 1, 256, 0),
            pooled_vm("pool-b", "ubuntu-base", 1, 256, 0),
        ];
        assert_eq!(find_pooled_match(&vms, "ubuntu-base", None, 1, 256, 0), Some(0));
    }

    // ---- ADR-0011: taking over what the previous agent left ----

    fn fcp(pid: i32, dir: &str) -> FcProc {
        FcProc { pid, dir_name: dir.into() }
    }

    fn names(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| s.to_string()).collect()
    }

    fn name_set(items: &[&str]) -> HashSet<String> {
        items.iter().map(|s| s.to_string()).collect()
    }

    fn kill(pid: i32, dir: &str) -> SweepStep {
        SweepStep::Kill { pid, dir_name: dir.into() }
    }

    fn rmdir(dir: &str) -> SweepStep {
        SweepStep::RemoveDir { dir_name: dir.into() }
    }

    #[test]
    fn test_plan_sweep_never_touches_an_adopted_vmm() {
        let found = vec![fcp(10, "vm-a"), fcp(11, "pool-1")];
        let owned: HashSet<i32> = [10, 11].into_iter().collect();
        let plan = plan_sweep(&found, &owned, &name_set(&["vm-a", "pool-1"]), &HashSet::new(), &[], &[], |_| true);
        assert!(plan.is_empty(), "{plan:?}");
    }

    #[test]
    fn test_plan_sweep_kills_only_what_no_caller_was_told_exists() {
        let found = vec![
            fcp(10, "vm-a"),     // adopted
            fcp(12, "vm-a"),     // a second VMM on the same directory
            fcp(20, "vm-half"),  // create that never wrote meta.json
            fcp(30, "vm-sleep"), // wake that was never written down: the record still says sleeping
        ];
        let owned: HashSet<i32> = [10].into_iter().collect();
        let plan = plan_sweep(
            &found,
            &owned,
            &name_set(&["vm-a", "vm-sleep"]),
            &name_set(&["vm-sleep"]),
            &[],
            &names(&["vm-half", "vm-empty"]),
            |_| true,
        );
        assert_eq!(
            plan,
            vec![
                kill(12, "vm-a"),
                kill(20, "vm-half"),
                // Not killed outright: its guest may be awake and writing.
                SweepStep::Probe { pid: 30, dir_name: "vm-sleep".into() },
                // Kills come first: a directory is removed after its VMM is gone.
                rmdir("vm-half"),
                rmdir("vm-empty"),
            ]
        );
    }

    #[test]
    fn test_plan_sweep_leaves_a_vmm_whose_record_does_not_load() {
        // A truncated or unparseable meta.json next to a live Firecracker is
        // most likely a healthy tenant VM. It is neither killed nor cleaned up.
        let found = vec![fcp(40, "vm-damaged")];
        let plan = plan_sweep(
            &found,
            &HashSet::new(),
            &HashSet::new(),
            &HashSet::new(),
            &names(&["vm-damaged"]),
            &[],
            |_| true,
        );
        assert_eq!(plan, vec![SweepStep::LeaveUnreadable { pid: 40, dir_name: "vm-damaged".into() }]);
    }

    #[test]
    fn test_plan_sweep_does_not_cross_into_another_cgroup() {
        // Started from a shell against this data dir, not by this unit: not
        // ours to kill, and the directory it lives in is not ours to remove.
        let found = vec![fcp(50, "vm-half"), fcp(51, "vm-sleep")];
        let plan = plan_sweep(
            &found,
            &HashSet::new(),
            &name_set(&["vm-sleep"]),
            &name_set(&["vm-sleep"]),
            &[],
            &names(&["vm-half", "vm-empty"]),
            |_| false,
        );
        assert_eq!(
            plan,
            vec![
                SweepStep::LeaveForeign { pid: 50, dir_name: "vm-half".into() },
                SweepStep::LeaveForeign { pid: 51, dir_name: "vm-sleep".into() },
                rmdir("vm-empty"),
            ]
        );
    }

    #[test]
    fn test_plan_sweep_ignores_directories_this_agent_does_not_manage() {
        // Not a record, not unreadable, not unfinished: reconcile never looked
        // at this directory (its name is not a valid instance id).
        let found = vec![fcp(60, "lost+found")];
        let plan = plan_sweep(&found, &HashSet::new(), &HashSet::new(), &HashSet::new(), &[], &[], |_| true);
        assert!(plan.is_empty(), "{plan:?}");
    }

    #[test]
    fn test_settle_action_brings_the_vmm_back_to_its_record() {
        use SettleAction::*;
        // Already consistent.
        assert_eq!(settle_action(&VmState::Running, "Running"), None);
        assert_eq!(settle_action(&VmState::Paused, "Paused"), None);
        assert_eq!(settle_action(&VmState::Pooled, "Paused"), None);
        // The agent stopped after a sleep or fork had paused the guest: the
        // record says running, `resume` is a no-op on a running record, and
        // the tenant would be left with a VM that is listed running and frozen.
        assert_eq!(settle_action(&VmState::Running, "Paused"), Resume);
        // The agent stopped between resuming the guest and recording it: a
        // resume, or a pool claim nobody got a 201 for.
        assert_eq!(settle_action(&VmState::Paused, "Running"), Pause);
        assert_eq!(settle_action(&VmState::Pooled, "Running"), Pause);
        // A VMM that never booted a guest is not a VM.
        assert_eq!(settle_action(&VmState::Running, "Not started"), Discard);
        assert_eq!(settle_action(&VmState::Pooled, "Not started"), Discard);
        // A state a newer Firecracker might report: do nothing rather than guess.
        assert_eq!(settle_action(&VmState::Running, "Migrating"), None);
    }

    fn pool_spec(image: &str, vcpus: u32, mem_mib: u64, count: u32) -> PoolSpec {
        PoolSpec { image: image.into(), image_sha256: String::new(), vcpus, mem_mib, disk_gb: 0, count }
    }

    #[test]
    fn test_template_pool_vms_are_not_drained_before_the_specs_arrive() {
        // What a restarted agent holds: pool VMs that survived (ADR-0011) and
        // no template specs yet — feluccad pushes those after registration.
        let vms = vec![
            pooled_vm("pool-l1", DEFAULT_IMAGE, 1, 256, 0),
            pooled_vm("pool-l2", DEFAULT_IMAGE, 1, 256, 0),
            pooled_vm("pool-t1", "odoo-v18", 2, 2048, 0),
            pooled_vm("pool-t2", "odoo-v18", 2, 2048, 0),
            pooled_vm("pool-t3", "odoo-v18", 2, 2048, 0),
        ];
        let legacy = pool_spec(DEFAULT_IMAGE, 1, 256, 1);

        // Before the push only the config pool is known: its own excess goes,
        // the template VMs stay. (Was: all three template VMs deleted on the
        // first tick, then cold-booted again seconds later.)
        let before = drainable_pooled_ids(&vms, std::slice::from_ref(&legacy), Some(&legacy), false);
        assert_eq!(before, vec!["pool-l2"]);
        // With no config pool nothing is known, so nothing is drained.
        assert!(drainable_pooled_ids(&vms, &[], None, false).is_empty());

        // After the push the full rule applies again.
        let pushed = vec![legacy.clone(), pool_spec("odoo-v18", 2, 2048, 2)];
        assert_eq!(drainable_pooled_ids(&vms, &pushed, Some(&legacy), true), vec!["pool-l2", "pool-t3"]);
        // ... including "feluccad says there are no template pools".
        assert_eq!(
            drainable_pooled_ids(&vms, std::slice::from_ref(&legacy), Some(&legacy), true),
            vec!["pool-l2", "pool-t1", "pool-t2", "pool-t3"]
        );
    }

    #[tokio::test]
    async fn test_set_pools_marks_the_specs_as_received() {
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        assert!(!mgr.inner.lock().await.pools_synced);
        // An empty push is a real answer ("no template pools"), not silence.
        mgr.set_pools(Vec::new()).await.unwrap();
        assert!(mgr.inner.lock().await.pools_synced);
    }

    // ---- FLU-2: every Firecracker is a preferred OOM victim ----

    #[test]
    fn test_set_oom_score_adj_writes_the_value_for_that_pid() {
        let tmp = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(tmp.path().join("4321")).unwrap();
        set_oom_score_adj(tmp.path(), 4321).unwrap();
        assert_eq!(std::fs::read_to_string(tmp.path().join("4321").join("oom_score_adj")).unwrap(), "300\n");
        // A process that is gone is an error for the caller to log, not a panic.
        assert!(set_oom_score_adj(tmp.path(), 9999).is_err());
    }

    #[test]
    fn test_the_oom_score_is_inside_the_range_the_kernel_accepts() {
        // Above the agent's own 0, so a VMM is taken before the agent; well
        // below 1000, so "always first" is not what we ask for.
        assert!((1..1000).contains(&FC_OOM_SCORE_ADJ));
    }

    #[tokio::test]
    async fn test_adoption_marks_the_vmm_as_an_oom_victim() {
        // On the real /proc, against a real process: the kernel has to accept
        // the write from an unprivileged parent (raising needs no capability),
        // and a VMM started by an agent that predates the marking sits at 0.
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-run", data_dir)).unwrap();
        let vmm = FakeVmm::spawn(&data_dir, "vm-run");
        let adj = || std::fs::read_to_string(format!("/proc/{}/oom_score_adj", vmm.pid())).unwrap();
        let before = adj();
        std::fs::write(
            format!("{}/instances/vm-run/meta.json", data_dir),
            meta_line("vm-run", Some(vmm.pid()), "running"),
        )
        .unwrap();

        let mgr = mgr_for(data_dir);
        mgr.reconcile().await;

        assert_eq!(adj().trim(), "300", "was {:?} before adoption", before.trim());
    }

    #[test]
    fn test_every_spawned_firecracker_gets_the_oom_score() {
        // The mark is applied in the one spawn function, before the child is
        // handed to its reaper. Structural, because spawning needs a real
        // Firecracker: the call has to be inside that function, between the
        // spawn and the reaper.
        let src = include_str!("mod.rs");
        let prod = src.split("#[cfg(test)]").next().expect("production half of mod.rs");
        let start = prod.find("fn spawn_firecracker(").expect("spawn_firecracker");
        let body = &prod[start..];
        let body = &body[..body.find("\n    }\n").expect("end of spawn_firecracker")];
        let spawn = body.find(".spawn()").expect("the spawn");
        let mark = body.find("set_oom_score_adj(").expect("spawn_firecracker must set the OOM score");
        let reaper = body.find("tokio::spawn(").expect("the reaper");
        assert!(spawn < mark && mark < reaper, "the OOM score must be set after the spawn and before the reaper owns the child");
    }

    // ---- FLU-60: a pool VM claimed while the drain is running ----

    #[tokio::test]
    async fn test_drain_skips_a_pool_vm_that_was_claimed_since_the_list_was_taken() {
        let dir = tempfile::tempdir().unwrap();
        let mgr = mgr_for(dir.path().to_str().unwrap().to_string());
        {
            let mut g = mgr.inner.lock().await;
            g.vms.push(pooled_vm("pool-1", DEFAULT_IMAGE, 1, 256, 0));
            g.vms.push(pooled_vm("pool-2", DEFAULT_IMAGE, 1, 256, 0));
            // What claim_from_pool does to pool-1 between the drain taking
            // its list and reaching this entry: new id, same directory.
            let v = &mut g.vms[0];
            v.id = "sb-tenant".into();
            v.state = VmState::Running;
        }
        // The drain still holds the id "pool-1".
        assert!(!mgr.retire_pooled("pool-1").await, "a claimed VM must not be drained");
        // An untouched pool VM is retired, and from then on cannot be claimed.
        assert!(mgr.retire_pooled("pool-2").await);
        assert!(!mgr.retire_pooled("pool-2").await);
        let g = mgr.inner.lock().await;
        assert_eq!(find_pooled_match(&g.vms, DEFAULT_IMAGE, None, 1, 256, 0), None);
        assert_eq!(g.vms[0].state, VmState::Running);
    }

    #[tokio::test]
    async fn test_delete_of_a_stale_pool_id_leaves_the_claimed_vms_directory_alone() {
        // Was: 6 of 6 sandboxes claimed during a pool teardown came back 201
        // and `running` with rootfs.ext4, meta.json and both sockets deleted.
        let dir = tempfile::tempdir().unwrap();
        let data_dir = dir.path().to_str().unwrap().to_string();
        let inst = format!("{}/instances/pool-1", data_dir);
        std::fs::create_dir_all(&inst).unwrap();
        std::fs::write(format!("{}/rootfs.ext4", inst), b"tenant data").unwrap();

        let mgr = mgr_for(data_dir);
        {
            let mut claimed = pooled_vm("sb-tenant", DEFAULT_IMAGE, 1, 256, 0);
            claimed.dir_id = "pool-1".into();
            claimed.state = VmState::Running;
            claimed.pid = None;
            mgr.inner.lock().await.vms.push(claimed);
        }

        // The id the VM had while it was in the pool.
        mgr.delete("pool-1").await.unwrap();
        assert!(Path::new(&format!("{}/rootfs.ext4", inst)).exists(), "a running VM's directory was removed");
        assert_eq!(mgr.inner.lock().await.vms.len(), 1);

        // Deleting the VM by the id it has now still works.
        mgr.delete("sb-tenant").await.unwrap();
        assert!(!Path::new(&inst).exists());
        assert!(mgr.inner.lock().await.vms.is_empty());
    }

    /// Block until `/proc/<pid>/cmdline` starts with `argv0`. `spawn()` returns
    /// as soon as the child has entered exec, which is an instant before the
    /// new image's argv is visible; a test that looks sooner sees a process
    /// with no command line at all.
    fn wait_for_argv0(pid: u32, argv0: &str) {
        let want = format!("{}\0", argv0).into_bytes();
        let path = format!("/proc/{}/cmdline", pid);
        for _ in 0..2000 {
            if std::fs::read(&path).map(|c| c.starts_with(&want)).unwrap_or(false) {
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(1));
        }
        panic!("pid {pid} never showed argv[0] {argv0:?}");
    }

    /// A real process that looks, in /proc, like the Firecracker of
    /// `<data_dir>/instances/<dir>`: argv[0] is `firecracker`, and
    /// `--api-sock <dir>/fc.sock` follows. It is a shell blocked on a pipe this
    /// struct holds, so it lives exactly until it is killed or dropped.
    struct FakeVmm {
        child: std::process::Child,
        _stdin: std::process::ChildStdin,
    }

    impl FakeVmm {
        fn spawn(data_dir: &str, dir: &str) -> Self {
            use std::os::unix::process::CommandExt;
            let sock = format!("{}/instances/{}/fc.sock", data_dir, dir);
            let mut child = std::process::Command::new("sh")
                .arg0("firecracker")
                .args(["-c", "read x", "sh", "--api-sock", &sock])
                .stdin(std::process::Stdio::piped())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .spawn()
                .expect("spawn a fake vmm");
            let stdin = child.stdin.take().expect("piped stdin");
            wait_for_argv0(child.id(), "firecracker");
            FakeVmm { child, _stdin: stdin }
        }

        fn pid(&self) -> i32 {
            self.child.id() as i32
        }

        fn alive(&mut self) -> bool {
            self.child.try_wait().expect("try_wait").is_none()
        }
    }

    impl Drop for FakeVmm {
        fn drop(&mut self) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }

    fn meta_line(id: &str, pid: Option<i32>, state: &str) -> String {
        let pid = pid.map(|p| p.to_string()).unwrap_or_else(|| "null".into());
        format!(
            r#"{{"id":"{id}","name":"{id}","dir_id":"{id}","vcpus":1,"mem_mib":256,"pid":{pid},"slot":null,"ip":null,"state":"{state}"}}"#
        )
    }

    /// The record a finished sleep writes: `sleeping` with the snapshot marker.
    fn sleeper_line(id: &str) -> String {
        let m = meta_line(id, None, "sleeping");
        format!("{},\"snapshot_valid\":true}}", &m[..m.len() - 1])
    }

    /// The disk image of an instance directory.
    fn disk_image(dir: &str) {
        std::fs::write(format!("{}/rootfs.ext4", dir), b"parent disk").unwrap();
    }

    /// Both snapshot files, non-empty, in an instance directory.
    fn snapshot_files(dir: &str) {
        std::fs::write(format!("{}/vmstate.bin", dir), b"device state").unwrap();
        std::fs::write(format!("{}/mem.bin", dir), b"guest memory").unwrap();
    }

    #[tokio::test]
    async fn test_reconcile_takes_over_live_vmms_and_sweeps_what_never_became_a_vm() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let inst = format!("{}/instances", data_dir);
        for d in ["vm-run", "pool-1", "vm-half", "vm-gone", "vm-damaged", "vm-sleep"] {
            std::fs::create_dir_all(format!("{}/{}", inst, d)).unwrap();
        }
        let mut run = FakeVmm::spawn(&data_dir, "vm-run");
        let mut pool = FakeVmm::spawn(&data_dir, "pool-1");
        let mut half = FakeVmm::spawn(&data_dir, "vm-half"); // create that never wrote meta.json
        let mut damaged = FakeVmm::spawn(&data_dir, "vm-damaged"); // healthy VM, record truncated
        let mut woke = FakeVmm::spawn(&data_dir, "vm-sleep"); // wake that never committed

        // A pool VM from before a host reboot: recorded pid long gone.
        std::fs::create_dir_all(format!("{}/pool-dead", inst)).unwrap();
        std::fs::write(format!("{}/pool-dead/meta.json", inst), meta_line("pool-dead", Some(999_999), "pooled")).unwrap();
        std::fs::write(format!("{}/pool-dead/rootfs.ext4", inst), "x").unwrap();
        std::fs::write(format!("{}/vm-run/meta.json", inst), meta_line("vm-run", Some(run.pid()), "running")).unwrap();
        std::fs::write(format!("{}/pool-1/meta.json", inst), meta_line("pool-1", Some(pool.pid()), "pooled")).unwrap();
        std::fs::write(format!("{}/vm-sleep/meta.json", inst), sleeper_line("vm-sleep")).unwrap();
        snapshot_files(&format!("{}/vm-sleep", inst));
        std::fs::write(format!("{}/vm-damaged/meta.json", inst), "").unwrap();
        std::fs::write(format!("{}/vm-half/rootfs.ext4", inst), "x").unwrap();
        // vm-gone: an empty directory, nothing running in it.

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;

        let recs: std::collections::HashMap<String, (VmState, Option<i32>)> = {
            let g = mgr.inner.lock().await;
            g.vms.iter().map(|v| (v.id.clone(), (v.state.clone(), v.pid))).collect()
        };
        // Adopted with their VMMs, both of them untouched.
        assert_eq!(recs.get("vm-run"), Some(&(VmState::Running, Some(run.pid()))));
        assert_eq!(recs.get("pool-1"), Some(&(VmState::Pooled, Some(pool.pid()))));
        assert!(run.alive() && pool.alive());
        // Never became a VM: process killed, directory removed.
        assert!(!half.alive(), "the VMM of an unfinished create must be killed");
        assert!(!Path::new(&format!("{}/vm-half", inst)).exists());
        assert!(!Path::new(&format!("{}/vm-gone", inst)).exists());
        // A record that does not load is not a reason to kill a running VM.
        assert!(damaged.alive(), "a VMM beside a damaged record must be left running");
        assert!(Path::new(&format!("{}/vm-damaged/meta.json", inst)).exists());
        assert!(!recs.contains_key("vm-damaged"));
        // The record moved on without this process: killed, record kept.
        assert!(!woke.alive(), "the VMM of an uncommitted wake must be killed");
        assert_eq!(recs.get("vm-sleep"), Some(&(VmState::Sleeping, None)));
        // A pool VM without its VMM is removed, record and directory (F-26:
        // it used to stay behind as a `stopped` record after every restart).
        assert!(!recs.contains_key("pool-dead"), "a dead pool VM must not linger as a record");
        assert!(!Path::new(&format!("{}/pool-dead", inst)).exists());
    }

    #[tokio::test]
    async fn test_only_the_agent_holding_the_data_dir_lock_sweeps() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let half_dir = format!("{}/instances/vm-half", data_dir);
        std::fs::create_dir_all(&half_dir).unwrap();
        let mut half = FakeVmm::spawn(&data_dir, "vm-half");

        // The running agent: to it, vm-half is a create in flight.
        let first = mgr_for(data_dir.clone());
        assert!(first.hold_data_dir_lock());
        // A second agent started by hand on the same data dir must not decide
        // that the first one's in-flight create is an orphan.
        let second = mgr_for(data_dir.clone());
        second.reconcile().await;
        assert!(half.alive(), "an agent without the lock must not sweep");
        assert!(Path::new(&half_dir).exists());
        drop(second);

        // Once the first agent is really gone, its successor sweeps.
        drop(first);
        let third = mgr_for(data_dir.clone());
        third.reconcile().await;
        assert!(!half.alive());
        assert!(!Path::new(&half_dir).exists());
    }

    fn running_vm(id: &str, pid: i32) -> Vm {
        let mut v = pooled_vm(id, DEFAULT_IMAGE, 1, 256, 0);
        v.state = VmState::Running;
        v.pid = Some(pid);
        v.slot = None;
        v.ip = None;
        v
    }

    #[tokio::test]
    async fn test_sweep_dead_goes_by_what_the_pid_is_not_by_whether_it_exists() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().join("data");
        let proc_root = tmp.path().join("proc");
        let inst = data_dir.join("instances");
        for d in ["vm-a", "vm-b", "vm-c"] {
            std::fs::create_dir_all(inst.join(d)).unwrap();
        }
        let data_dir = data_dir.to_str().unwrap().to_string();
        // 4000 is vm-a's Firecracker.
        let sock = format!("{}/instances/vm-a/fc.sock", data_dir);
        std::fs::create_dir_all(proc_root.join("4000")).unwrap();
        std::fs::write(proc_root.join("4000").join("cmdline"), format!("firecracker\0--api-sock\0{}\0", sock)).unwrap();
        // 4001 exists, as something else: vm-b's VMM died and its pid was reused.
        std::fs::create_dir_all(proc_root.join("4001")).unwrap();
        std::fs::write(proc_root.join("4001").join("cmdline"), "sleep\0600\0").unwrap();
        // 4002 cannot be read at all.
        std::fs::create_dir_all(proc_root.join("4002").join("cmdline")).unwrap();

        let mgr = Manager::with_proc_root(
            data_dir,
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
            proc_root,
        );
        {
            let mut g = mgr.inner.lock().await;
            g.vms.push(running_vm("vm-a", 4000));
            g.vms.push(running_vm("vm-b", 4001));
            g.vms.push(running_vm("vm-c", 4002));
        }
        mgr.sweep_dead().await;

        let g = mgr.inner.lock().await;
        let st = |id: &str| g.vms.iter().find(|v| v.id == id).map(|v| (v.state.clone(), v.pid)).unwrap();
        assert_eq!(st("vm-a"), (VmState::Running, Some(4000)));
        // `kill(4001, 0)` succeeds forever; the VM is dead all the same.
        assert_eq!(st("vm-b"), (VmState::Error, None));
        // A failed read is not a death certificate.
        assert_eq!(st("vm-c"), (VmState::Running, Some(4002)));
    }

    #[tokio::test]
    async fn test_stop_does_not_signal_a_process_that_merely_has_the_recorded_pid() {
        // An adopted VMM is not this agent's child: once it has exited, its
        // pid can belong to anything. stop() used to SIGTERM (then SIGKILL)
        // whatever had the number.
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-1", data_dir)).unwrap();
        // Killed and reaped when the test ends, however it ends.
        struct Reaped(std::process::Child);
        impl Drop for Reaped {
            fn drop(&mut self) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }
        let mut bystander = Reaped(std::process::Command::new("sleep").arg("300").spawn().expect("spawn sleep"));
        // Past exec, so it is positively "some other program" — which is what
        // a recycled pid looks like in practice.
        wait_for_argv0(bystander.0.id(), "sleep");

        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", bystander.0.id() as i32));
        mgr.stop("vm-1").await.unwrap();

        std::thread::sleep(std::time::Duration::from_millis(200));
        let survived = bystander.0.try_wait().unwrap().is_none();
        assert!(survived, "stop() signalled a process that is not this instance's Firecracker");
        let g = mgr.inner.lock().await;
        assert_eq!((g.vms[0].state.clone(), g.vms[0].pid), (VmState::Stopped, None));
    }

    #[tokio::test]
    async fn test_stop_still_stops_the_instances_own_vmm() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-1", data_dir)).unwrap();
        let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");

        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        mgr.stop("vm-1").await.unwrap();
        assert!(!vmm.alive(), "stop() must still terminate the instance's own Firecracker");
        // And the record on disk says so. An unwritten stop reads as `running`
        // with a dead pid at the next start, and from there as `sleeping`
        // whenever an old snapshot is lying in the directory.
        let written = std::fs::read_to_string(format!("{}/instances/vm-1/meta.json", data_dir)).unwrap();
        let meta: Meta = serde_json::from_str(&written).expect("meta.json must parse");
        assert_eq!((meta.state, meta.pid), (VmState::Stopped, None));
    }

    #[tokio::test]
    async fn test_write_meta_replaces_the_file_and_leaves_no_temp_behind() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        // What a crash mid-write used to leave, plus a stale temp file.
        std::fs::write(format!("{}/meta.json", dir), "{\"id\":\"vm-1\",\"na").unwrap();
        std::fs::write(format!("{}/meta.json.tmp", dir), "garbage").unwrap();

        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", 4242));
        mgr.write_meta("vm-1").await.unwrap();

        let written = std::fs::read_to_string(format!("{}/meta.json", dir)).unwrap();
        let meta: Meta = serde_json::from_str(&written).expect("meta.json must parse");
        assert_eq!((meta.id.as_str(), meta.state, meta.pid), ("vm-1", VmState::Running, Some(4242)));
        assert!(!Path::new(&format!("{}/meta.json.tmp", dir)).exists());
    }

    /// Stands in for a Firecracker's API socket. Every request is answered 200
    /// with an `InstanceInfo` carrying the current state; `PATCH /vm` changes
    /// that state the way a VMM would. The request lines are kept.
    ///
    /// `Behavior` makes it refuse, hold or act on chosen requests (FLU-4).
    struct FakeApi {
        state: Arc<std::sync::Mutex<String>>,
        seen: Arc<std::sync::Mutex<Vec<String>>>,
        bodies: Arc<std::sync::Mutex<Vec<String>>>,
        meta_seen: Arc<std::sync::Mutex<Vec<(String, String)>>>,
        task: tokio::task::JoinHandle<()>,
    }

    /// What a `FakeApi` does besides answering 200.
    #[derive(Clone, Default)]
    struct Behavior {
        /// Requests whose line starts with one of these are answered 400.
        fail: Vec<&'static str>,
        /// Requests whose body contains one of these are answered 400 (a
        /// resume, but not the pause before it: `"Resumed"`).
        fail_body: Vec<&'static str>,
        /// A request whose line starts with this is never answered while the
        /// process `pid` lives (None: never) — a Firecracker busy with it;
        /// its API goes away with it.
        hang: Option<(&'static str, Option<i32>)>,
        /// Answered only after this long.
        delay: Option<(&'static str, Duration)>,
        /// `snapshot/create` writes both files it names, as Firecracker does.
        write_snapshot: bool,
        /// Read this meta.json at every request and keep (request line, contents).
        watch_meta: Option<String>,
        /// The root drive's `path_on_host` that `GET /vm/config` answers
        /// (what a loaded snapshot names). `PATCH /drives/rootfs` changes it,
        /// as Firecracker does, unless `keep_drive`.
        drive: Option<String>,
        keep_drive: bool,
        /// Requests whose line starts with one of these are read and then
        /// left without an answer: the connection is closed, as when the
        /// reply is lost or the VMM dies before it answers.
        drop: Vec<&'static str>,
        /// A request whose line starts with this is held until the process
        /// whose pid is in this file (once it exists) is gone, and then left
        /// unanswered: a call to a VMM a wake or a start has just spawned.
        hang_pidfile: Option<(&'static str, String)>,
        /// `forking`: the parent's `snapshot/create` answers success and
        /// writes nothing.
        writes_nothing: bool,
        /// The state a refused `PATCH /vm` leaves (the request was refused,
        /// but the VM did change: no proof of anything).
        refused_sets: Option<&'static str>,
    }

    /// Has `pid` exited (gone, or a zombie nobody has reaped yet)?
    fn proc_gone(pid: i32) -> bool {
        match std::fs::read_to_string(format!("/proc/{}/stat", pid)) {
            Ok(stat) => stat.rfind(')').is_some_and(|i| stat[i + 1..].trim_start().starts_with('Z')),
            Err(_) => true,
        }
    }

    impl FakeApi {
        fn serve(sock: &str, state: &str) -> Self {
            Self::serve_with(sock, state, Behavior::default())
        }

        fn serve_with(sock: &str, state: &str, behavior: Behavior) -> Self {
            let listener = tokio::net::UnixListener::bind(sock).expect("bind the fake api socket");
            let state = Arc::new(std::sync::Mutex::new(state.to_string()));
            let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
            let bodies = Arc::new(std::sync::Mutex::new(Vec::new()));
            let meta_seen = Arc::new(std::sync::Mutex::new(Vec::new()));
            // Unless told otherwise, the root drive is the image beside the
            // socket: the instance's own `rootfs.ext4`.
            let own = Path::new(sock).parent().map(|d| d.join("rootfs.ext4").to_string_lossy().into_owned()).unwrap_or_default();
            let drive = Arc::new(std::sync::Mutex::new(behavior.drive.clone().unwrap_or(own)));
            let (st, sn, bd, ms) = (Arc::clone(&state), Arc::clone(&seen), Arc::clone(&bodies), Arc::clone(&meta_seen));
            let task = tokio::spawn(async move {
                loop {
                    let Ok((mut conn, _)) = listener.accept().await else { return };
                    let (st, sn, bd, ms, behavior, dr) =
                        (Arc::clone(&st), Arc::clone(&sn), Arc::clone(&bd), Arc::clone(&ms), behavior.clone(), Arc::clone(&drive));
                    tokio::spawn(async move {
                        use tokio::io::{AsyncReadExt, AsyncWriteExt};
                        // Headers, then as many body bytes as they announce.
                        let mut buf: Vec<u8> = Vec::new();
                        let mut chunk = [0u8; 2048];
                        let request = loop {
                            let text = String::from_utf8_lossy(&buf).to_string();
                            if let Some(end) = text.find("\r\n\r\n") {
                                let want = text[..end]
                                    .lines()
                                    .find_map(|l| l.to_ascii_lowercase().strip_prefix("content-length:").map(|v| v.trim().parse::<usize>().unwrap_or(0)))
                                    .unwrap_or(0);
                                if buf.len() >= end + 4 + want {
                                    break text;
                                }
                            }
                            match conn.read(&mut chunk).await {
                                Ok(0) | Err(_) => break text,
                                Ok(n) => buf.extend_from_slice(&chunk[..n]),
                            }
                        };
                        let line = request.lines().next().unwrap_or("").to_string();
                        let body = request.split_once("\r\n\r\n").map(|(_, b)| b.to_string()).unwrap_or_default();
                        if let Some(path) = &behavior.watch_meta {
                            let now = std::fs::read_to_string(path).unwrap_or_default();
                            ms.lock().unwrap().push((line.clone(), now));
                        }
                        sn.lock().unwrap().push(line.clone());
                        bd.lock().unwrap().push(body.clone());
                        if let Some((prefix, pid)) = behavior.hang {
                            if line.starts_with(prefix) {
                                // Held until the VMM dies; then the connection
                                // goes away unanswered, as it does when a
                                // Firecracker is killed in the middle of a call.
                                loop {
                                    if pid.is_some_and(proc_gone) {
                                        return;
                                    }
                                    sleep(Duration::from_millis(10)).await;
                                }
                            }
                        }
                        if behavior.drop.iter().any(|p| line.starts_with(p)) {
                            return;
                        }
                        if let Some((prefix, pidfile)) = &behavior.hang_pidfile {
                            if line.starts_with(prefix) {
                                loop {
                                    let pid = std::fs::read_to_string(pidfile).ok().and_then(|s| s.trim().parse::<i32>().ok());
                                    if pid.is_some_and(proc_gone) {
                                        return;
                                    }
                                    sleep(Duration::from_millis(10)).await;
                                }
                            }
                        }
                        if let Some((prefix, d)) = behavior.delay {
                            if line.starts_with(prefix) {
                                sleep(d).await;
                            }
                        }
                        let failed = behavior.fail.iter().any(|p| line.starts_with(p))
                            || behavior.fail_body.iter().any(|b| body.contains(b));
                        if failed && line.starts_with("PATCH /vm") {
                            if let Some(to) = behavior.refused_sets {
                                *st.lock().unwrap() = to.into();
                            }
                        }
                        if !failed {
                            // A snapshot is loaded paused unless told to resume.
                            if line.starts_with("PUT /snapshot/load") {
                                *st.lock().unwrap() = if body.contains("\"resume_vm\":true") { "Running" } else { "Paused" }.into();
                            }
                            if line.starts_with("PATCH /vm") {
                                if request.contains("Resumed") {
                                    *st.lock().unwrap() = "Running".into();
                                } else if request.contains("Paused") {
                                    *st.lock().unwrap() = "Paused".into();
                                }
                            }
                            if behavior.write_snapshot && line.starts_with("PUT /snapshot/create") {
                                let v: serde_json::Value = serde_json::from_str(&body).unwrap();
                                std::fs::write(v["snapshot_path"].as_str().unwrap(), b"device state").unwrap();
                                std::fs::write(v["mem_file_path"].as_str().unwrap(), b"guest memory").unwrap();
                            }
                            if line.starts_with("PATCH /drives/rootfs") && !behavior.keep_drive {
                                let v: serde_json::Value = serde_json::from_str(&body).unwrap();
                                *dr.lock().unwrap() = v["path_on_host"].as_str().unwrap().to_string();
                            }
                        }
                        let (status, body) = if failed {
                            ("400 Bad Request", "{\"fault_message\":\"refused by the test\"}".to_string())
                        } else if line.starts_with("GET /vm/config") {
                            let drive = dr.lock().unwrap().clone();
                            let drives = serde_json::json!([
                                { "drive_id": "rootfs", "path_on_host": drive, "is_root_device": true, "is_read_only": false }
                            ]);
                            ("200 OK", serde_json::json!({ "drives": drives }).to_string())
                        } else {
                            (
                                "200 OK",
                                format!(
                                    "{{\"id\":\"t\",\"state\":\"{}\",\"vmm_version\":\"1.16.0\",\"app_name\":\"Firecracker\"}}",
                                    st.lock().unwrap()
                                ),
                            )
                        };
                        let resp = format!(
                            "HTTP/1.1 {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                            status,
                            body.len(),
                            body
                        );
                        let _ = conn.write_all(resp.as_bytes()).await;
                        let _ = conn.shutdown().await;
                    });
                }
            });
            FakeApi { state, seen, bodies, meta_seen, task }
        }

        fn requests(&self) -> Vec<String> {
            self.seen.lock().unwrap().clone()
        }

        fn bodies(&self) -> Vec<String> {
            self.bodies.lock().unwrap().clone()
        }

        /// (request line, meta.json at that moment), for `Behavior::watch_meta`.
        fn meta_seen(&self) -> Vec<(String, String)> {
            self.meta_seen.lock().unwrap().clone()
        }

        fn state(&self) -> String {
            self.state.lock().unwrap().clone()
        }
    }

    impl Drop for FakeApi {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    async fn record_of(mgr: &Manager, id: &str) -> Option<(VmState, Option<i32>)> {
        let g = mgr.inner.lock().await;
        g.vms.iter().find(|v| v.id == id).map(|v| (v.state.clone(), v.pid))
    }

    fn meta_on_disk(data_dir: &str, dir: &str) -> Meta {
        let written = std::fs::read_to_string(format!("{}/instances/{}/meta.json", data_dir, dir)).unwrap();
        serde_json::from_str(&written).expect("meta.json must parse")
    }

    #[tokio::test]
    async fn test_a_wake_or_start_nobody_wrote_down_is_adopted_when_its_guest_runs() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let inst = format!("{}/instances", data_dir);
        for d in ["vm-woke", "vm-loading", "vm-started"] {
            std::fs::create_dir_all(format!("{}/{}", inst, d)).unwrap();
        }
        // Three records that hold no VMM, each with a Firecracker in its
        // directory: what an agent leaves that was woken or started a VM and
        // went away before it wrote the record.
        let mut woke = FakeVmm::spawn(&data_dir, "vm-woke");
        let mut loading = FakeVmm::spawn(&data_dir, "vm-loading");
        let mut started = FakeVmm::spawn(&data_dir, "vm-started");
        std::fs::write(format!("{}/vm-woke/meta.json", inst), sleeper_line("vm-woke")).unwrap();
        std::fs::write(format!("{}/vm-loading/meta.json", inst), sleeper_line("vm-loading")).unwrap();
        // FLU-4: a sleeping record keeps its state only with its snapshot.
        snapshot_files(&format!("{}/vm-woke", inst));
        snapshot_files(&format!("{}/vm-loading", inst));
        std::fs::write(format!("{}/vm-started/meta.json", inst), meta_line("vm-started", None, "stopped")).unwrap();
        let _woke_api = FakeApi::serve(&format!("{}/vm-woke/fc.sock", inst), "Running");
        let _loading_api = FakeApi::serve(&format!("{}/vm-loading/fc.sock", inst), "Paused");
        let _started_api = FakeApi::serve(&format!("{}/vm-started/fc.sock", inst), "Running");

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;

        // A guest that is running has been writing to its disk: killing it
        // would leave `sleeping` on top of a snapshot the disk has outrun.
        assert_eq!(record_of(&mgr, "vm-woke").await, Some((VmState::Running, Some(woke.pid()))));
        assert!(woke.alive(), "a guest that is already awake must not be killed");
        assert_eq!(record_of(&mgr, "vm-started").await, Some((VmState::Running, Some(started.pid()))));
        assert!(started.alive());
        let on_disk = meta_on_disk(&data_dir, "vm-woke");
        assert_eq!((on_disk.state, on_disk.pid), (VmState::Running, Some(woke.pid())));
        // FLU-4: the snapshot the guest has run past is no longer valid, and
        // its state file goes; the memory file stays (the guest runs on it).
        assert!(!on_disk.snapshot_valid);
        assert!(!Path::new(&format!("{}/vm-woke/vmstate.bin", inst)).exists());
        assert!(Path::new(&format!("{}/vm-woke/mem.bin", inst)).exists());
        // Loaded but never resumed: nothing has run since the snapshot, so
        // the state on disk is still current and the VMM is simply removed.
        assert_eq!(record_of(&mgr, "vm-loading").await, Some((VmState::Sleeping, None)));
        assert!(!loading.alive(), "a VMM whose guest never ran must be killed");
        assert!(meta_on_disk(&data_dir, "vm-loading").snapshot_valid);
        assert!(Path::new(&format!("{}/vm-loading/vmstate.bin", inst)).exists());
        assert!(Path::new(&format!("{}/vm-loading/mem.bin", inst)).exists());
    }

    #[tokio::test]
    async fn test_a_vmm_that_did_not_answer_at_start_up_is_asked_again() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-busy", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        // Adopted as running. Its Firecracker is still writing the snapshot
        // of a fork the previous agent started, and answers nothing.
        let mut vmm = FakeVmm::spawn(&data_dir, "vm-busy");
        std::fs::write(format!("{}/meta.json", dir), meta_line("vm-busy", Some(vmm.pid()), "running")).unwrap();

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;
        assert_eq!(record_of(&mgr, "vm-busy").await, Some((VmState::Running, Some(vmm.pid()))));
        assert_eq!(lock_std(&mgr.unsettled).get("vm-busy"), Some(&Unsettled::Pending(vmm.pid())));

        // Still silent: nothing is decided, and it stays on the list.
        mgr.settle_unanswered().await;
        assert_eq!(lock_std(&mgr.unsettled).get("vm-busy"), Some(&Unsettled::Pending(vmm.pid())));

        // The snapshot is written. Firecracker answers again — and its guest
        // is paused, under a record that says running.
        let api = FakeApi::serve(&format!("{}/fc.sock", dir), "Paused");
        mgr.settle_unanswered().await;
        assert_eq!(api.state(), "Running", "the guest must be resumed: {:?}", api.requests());
        assert!(lock_std(&mgr.unsettled).is_empty());
        assert_eq!(record_of(&mgr, "vm-busy").await, Some((VmState::Running, Some(vmm.pid()))));
        assert!(vmm.alive());
    }

    #[tokio::test]
    async fn test_an_unsettled_vm_that_was_stopped_since_is_dropped_from_the_list() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-1", data_dir)).unwrap();
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        lock_std(&mgr.unsettled).insert("vm-1".into(), Unsettled::Pending(vmm.pid()));

        // An operation on the VM takes it off the list before it starts...
        mgr.stop("vm-1").await.unwrap();
        assert!(lock_std(&mgr.unsettled).is_empty());
        // ...and an entry for a VMM the record no longer holds is dropped.
        lock_std(&mgr.unsettled).insert("vm-1".into(), Unsettled::Pending(vmm.pid()));
        mgr.settle_unanswered().await;
        assert!(lock_std(&mgr.unsettled).is_empty());
    }

    #[tokio::test]
    async fn test_resume_reaches_the_vmm_while_its_run_state_is_unsettled() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let api = FakeApi::serve(&format!("{}/fc.sock", dir), "Paused");
        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        // Settled: "already running" is answered from the record.
        mgr.resume("vm-1").await.unwrap();
        assert!(api.requests().is_empty(), "{:?}", api.requests());

        // Start-up never heard from this VMM: the record is not known to be
        // true, so the request has to reach it.
        lock_std(&mgr.unsettled).insert("vm-1".into(), Unsettled::Pending(vmm.pid()));
        mgr.resume("vm-1").await.unwrap();
        assert_eq!(api.state(), "Running", "{:?}", api.requests());
        assert!(lock_std(&mgr.unsettled).is_empty());
    }

    #[tokio::test]
    async fn test_sleep_never_leaves_a_running_record_without_a_pid() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");
        // FLU-70: a sleep succeeds only once the snapshot is there and the
        // disk image could be synced, so both have to exist here.
        disk_image(&dir);
        let _api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { write_snapshot: true, ..Default::default() });
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        // The caller hangs up the moment the record lets go of the pid — the
        // point at which the request used to be droppable with the record
        // still saying `running`, which nothing could move afterwards.
        // `biased`, watcher first: it looks at the record every time the
        // sleep yields, so it cannot miss that moment.
        //
        // FLU-4 changed the order on purpose, and this test with it: the
        // record goes to disk as `sleeping` with the snapshot marker BEFORE
        // the VMM is killed. It used to be kill first, write afterwards — and
        // an agent stopped in between left a complete snapshot under a
        // `running` record with a dead VMM, which start-up now turns into
        // `stopped`. A thread watches the VMM and reads meta.json the moment
        // it sees it die.
        mgr.write_meta("vm-1").await.unwrap(); // `running`, as before the sleep
        let (meta_path, vpid) = (format!("{}/meta.json", dir), vmm.pid());
        let at_death = std::thread::spawn(move || {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
            while !proc_gone(vpid) {
                assert!(std::time::Instant::now() < deadline, "the VMM was never killed");
                std::thread::sleep(std::time::Duration::from_micros(100));
            }
            std::fs::read_to_string(&meta_path).unwrap()
        });
        let watcher = Arc::clone(&mgr);
        tokio::select! {
            biased;
            _ = async move {
                loop {
                    if watcher.inner.lock().await.vms[0].pid.is_none() {
                        break;
                    }
                    tokio::task::yield_now().await;
                }
            } => {}
            r = mgr.sleep_vm("vm-1") => r.unwrap(),
        }
        let at_death: Meta = serde_json::from_str(&at_death.join().unwrap()).expect("meta.json when the VMM died");
        assert_eq!(
            (at_death.state, at_death.snapshot_valid),
            (VmState::Sleeping, true),
            "the record must be on disk, asleep and marked, before the VMM is killed"
        );
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)));
        // The VMM was told to go in that same step.
        for _ in 0..300 {
            if !vmm.alive() {
                break;
            }
            sleep(Duration::from_millis(10)).await;
        }
        assert!(!vmm.alive(), "the Firecracker of a sleeping VM must be gone");
        // Whatever the dropped request still owed the record's file is written.
        mgr.flush_unwritten_meta().await;
        let on_disk = meta_on_disk(&data_dir, "vm-1");
        assert_eq!((on_disk.state, on_disk.pid), (VmState::Sleeping, None));
    }

    #[tokio::test]
    async fn test_delete_does_not_remove_a_directory_from_under_a_running_vmm() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-lost", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        // A healthy VM whose record does not load: start-up leaves it running
        // and unmanaged. The control plane still knows its id.
        let mut vmm = FakeVmm::spawn(&data_dir, "vm-lost");
        std::fs::write(format!("{}/meta.json", dir), "").unwrap();

        let first = mgr_for(data_dir.clone());
        first.reconcile().await;
        assert!(vmm.alive());
        assert!(record_of(&first, "vm-lost").await.is_none());

        // An agent that does not hold the data dir decides nothing about it.
        let second = mgr_for(data_dir.clone());
        assert!(second.delete("vm-lost").await.is_err());
        assert!(vmm.alive() && Path::new(&dir).exists());
        drop(second);

        // The one that does removes the VM for real: process first, then files.
        first.delete("vm-lost").await.unwrap();
        assert!(!vmm.alive(), "delete must not leave a Firecracker running with no directory");
        assert!(!Path::new(&dir).exists());
    }

    #[tokio::test]
    async fn test_a_pool_vm_whose_vmm_dies_is_removed_and_its_pool_rests() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/pool-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(format!("{}/rootfs.ext4", dir), "x").unwrap();
        let vmm = FakeVmm::spawn(&data_dir, "pool-1");
        let mgr = mgr_for(data_dir);
        {
            let mut v = pooled_vm("pool-1", DEFAULT_IMAGE, 1, 256, 0);
            v.pid = Some(vmm.pid());
            v.slot = None;
            v.ip = None;
            mgr.inner.lock().await.vms.push(v);
        }
        // Still alive: nothing happens.
        mgr.sweep_dead().await;
        assert_eq!(record_of(&mgr, "pool-1").await.map(|r| r.0), Some(VmState::Pooled));

        drop(vmm); // the Firecracker dies (an OOM kill, say)
        mgr.sweep_dead().await;
        // Not an `error` record that keeps a slot and a rootfs copy for ever:
        assert!(record_of(&mgr, "pool-1").await.is_none(), "a dead pool VM must not linger as a record");
        assert!(!Path::new(&dir).exists());
        // and no replacement five seconds later.
        let key = spec_key(&pool_spec(DEFAULT_IMAGE, 1, 256, 1));
        assert!(mgr.pool_backoff.lock().await.contains_key(&key));
    }

    #[tokio::test]
    async fn test_a_meta_write_that_fails_is_owed_and_written_later() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        let mgr = mgr_for(data_dir.clone());
        // Its directory does not exist: the write cannot land.
        mgr.inner.lock().await.vms.push(running_vm("vm-1", 4242));
        assert!(mgr.write_meta("vm-1").await.is_err());
        assert!(lock_std(&mgr.meta_owed).contains("vm-1"));
        // Still failing: still owed.
        mgr.flush_unwritten_meta().await;
        assert!(lock_std(&mgr.meta_owed).contains("vm-1"));
        // The disk is back.
        std::fs::create_dir_all(&dir).unwrap();
        mgr.flush_unwritten_meta().await;
        assert!(lock_std(&mgr.meta_owed).is_empty());
        let on_disk = meta_on_disk(&data_dir, "vm-1");
        assert_eq!((on_disk.state, on_disk.pid), (VmState::Running, Some(4242)));
        // A record that does not exist owes nothing.
        assert!(mgr.write_meta("vm-nope").await.is_err());
        assert!(lock_std(&mgr.meta_owed).is_empty());
    }

    #[tokio::test]
    async fn test_a_meta_write_whose_caller_goes_away_is_owed() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-1", data_dir)).unwrap();
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", 4242));

        // Another write is in progress, so this one has to wait — and its
        // caller is dropped while it does.
        let busy = mgr.meta_write_lock.lock().await;
        {
            let write = mgr.write_meta("vm-1");
            tokio::pin!(write);
            tokio::select! {
                biased;
                _ = &mut write => panic!("the write cannot finish while the lock is held"),
                _ = tokio::task::yield_now() => {}
            }
        }
        drop(busy);
        assert!(lock_std(&mgr.meta_owed).contains("vm-1"));
        mgr.flush_unwritten_meta().await;
        assert!(lock_std(&mgr.meta_owed).is_empty());
        assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Running);
    }

    // ---- FLU-4: a sandbox is asleep only when its record says so ----

    /// A stand-in `firecracker` for the paths that spawn one (wake, start):
    /// it links the API socket the test serves at `<sock>.fake` to the path it
    /// was given, writes its pid to `<sock>.pid`, and lives until it is killed
    /// — under a command line procscan reads as that instance's Firecracker.
    /// Written once per test process and left to settle, so that no exec of
    /// it can meet a descriptor another test thread's fork still holds open
    /// for writing (ETXTBSY).
    ///
    /// It lives beside the test binary — a directory that can certainly
    /// execute, where the temp dir may be mounted noexec — and is removed when
    /// the test process exits.
    fn fake_fc_bin() -> String {
        static DIR: std::sync::OnceLock<std::path::PathBuf> = std::sync::OnceLock::new();
        static BIN: std::sync::OnceLock<String> = std::sync::OnceLock::new();
        extern "C" fn remove_fake_fc_dir() {
            if let Some(dir) = DIR.get() {
                let _ = std::fs::remove_dir_all(dir);
            }
        }
        BIN.get_or_init(|| {
            use std::os::unix::fs::PermissionsExt;
            let exe = std::env::current_exe().expect("the test binary's path");
            let dir = DIR.get_or_init(|| exe.parent().unwrap().join(format!("felucca-fake-fc-{}", std::process::id())));
            std::fs::create_dir_all(dir).unwrap();
            // SAFETY: registers a plain function; it only reads a OnceLock.
            unsafe {
                libc::atexit(remove_fake_fc_dir);
            }
            let path = dir.join("firecracker");
            // The socket is linked to `<sock>.fake`, or, when there is none,
            // to `<instance dir>.fake.sock` beside the instance directory: a
            // fork creates its child's directory itself, so the child's fake
            // API has to be bound outside it, before the fork starts. The pid
            // is written first: a caller that sees the socket may kill the
            // stand-in at once, and `spawned_pid` must still find it.
            std::fs::write(
                &path,
                "#!/bin/bash\nt=\"$2.fake\"; [ -e \"$t\" ] || t=\"$(dirname \"$2\").fake.sock\"\n\
                 echo $$ > \"$2.pid\"\nln -sf \"$t\" \"$2\"\n\
                 exec -a firecracker bash -c 'while :; do sleep 1; done' firecracker --api-sock \"$2\"\n",
            )
            .unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
            std::thread::sleep(std::time::Duration::from_millis(200));
            path.to_str().unwrap().to_string()
        })
        .clone()
    }

    fn mgr_with_fc(data_dir: String, fc_bin: &str) -> Arc<Manager> {
        let mut mgr = mgr_for(data_dir);
        Arc::get_mut(&mut mgr).expect("a fresh manager").fc_bin = fc_bin.to_string();
        mgr
    }

    /// The pid the stand-in Firecracker of `dir` wrote, once it has.
    async fn spawned_pid(dir: &str) -> i32 {
        for _ in 0..200 {
            if let Ok(s) = std::fs::read_to_string(format!("{}/fc.sock.pid", dir)) {
                if let Ok(p) = s.trim().parse() {
                    return p;
                }
            }
            sleep(Duration::from_millis(10)).await;
        }
        panic!("the stand-in firecracker in {dir} never wrote its pid");
    }

    async fn gone_soon(pid: i32) -> bool {
        for _ in 0..200 {
            if proc_gone(pid) {
                return true;
            }
            sleep(Duration::from_millis(10)).await;
        }
        false
    }

    fn sleeping_vm(id: &str) -> Vm {
        let mut v = running_vm(id, 0);
        v.state = VmState::Sleeping;
        v.pid = None;
        v.snapshot_valid = true;
        v
    }

    async fn marker_of(mgr: &Manager, id: &str) -> bool {
        mgr.inner.lock().await.vms.iter().find(|v| v.id == id).map(|v| v.snapshot_valid).unwrap()
    }

    fn exists(path: &str) -> bool {
        std::fs::symlink_metadata(path).is_ok()
    }

    /// A meta.json that cannot be written: its temp file's name is taken by a
    /// directory.
    fn break_meta_writes(dir: &str) {
        std::fs::create_dir_all(format!("{}/meta.json.tmp", dir)).unwrap();
    }

    // -- start-up --

    #[tokio::test]
    async fn test_start_up_writes_what_it_decided_and_removes_snapshots_nothing_can_use() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let inst = format!("{}/instances", data_dir);
        for d in ["vm-dead", "vm-asleep", "vm-half", "vm-damaged", "vm-alive"] {
            std::fs::create_dir_all(format!("{}/{}", inst, d)).unwrap();
            std::fs::write(format!("{}/{}/rootfs.ext4", inst, d), b"disk").unwrap();
            snapshot_files(&format!("{}/{}", inst, d));
            std::fs::write(format!("{}/{}/vmstate.bin.tmp", inst, d), b"half a state").unwrap();
        }
        // Rebooted under a running guest that had slept and woken before.
        std::fs::write(format!("{}/vm-dead/meta.json", inst), meta_line("vm-dead", Some(999_999), "running")).unwrap();
        // Asleep, properly.
        std::fs::write(format!("{}/vm-asleep/meta.json", inst), sleeper_line("vm-asleep")).unwrap();
        // Asleep, but its state file is gone.
        std::fs::write(format!("{}/vm-half/meta.json", inst), sleeper_line("vm-half")).unwrap();
        std::fs::remove_file(format!("{}/vm-half/vmstate.bin", inst)).unwrap();
        // A record that does not load: nothing in its directory is touched.
        std::fs::write(format!("{}/vm-damaged/meta.json", inst), "{\"id\":\"vm-da").unwrap();
        // Running, with its VMM, after a wake: the state file goes, the memory
        // the guest runs on stays.
        let vmm = FakeVmm::spawn(&data_dir, "vm-alive");
        let _api = FakeApi::serve(&format!("{}/vm-alive/fc.sock", inst), "Running");
        std::fs::write(format!("{}/vm-alive/meta.json", inst), meta_line("vm-alive", Some(vmm.pid()), "running")).unwrap();

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;

        let on_disk = meta_on_disk(&data_dir, "vm-dead");
        assert_eq!((on_disk.state, on_disk.pid), (VmState::Stopped, None), "the corrected state is written");
        for f in ["vmstate.bin", "mem.bin", "vmstate.bin.tmp"] {
            assert!(!exists(&format!("{}/vm-dead/{}", inst, f)), "vm-dead/{f}");
        }
        assert_eq!(record_of(&mgr, "vm-asleep").await, Some((VmState::Sleeping, None)));
        assert!(meta_on_disk(&data_dir, "vm-asleep").snapshot_valid);
        assert!(exists(&format!("{}/vm-asleep/vmstate.bin", inst)) && exists(&format!("{}/vm-asleep/mem.bin", inst)));
        assert!(!exists(&format!("{}/vm-asleep/vmstate.bin.tmp", inst)), "a temp state file is never part of a snapshot");
        let half = meta_on_disk(&data_dir, "vm-half");
        assert_eq!((half.state, half.snapshot_valid), (VmState::Stopped, false));
        assert!(!exists(&format!("{}/vm-half/mem.bin", inst)), "what is left of an incomplete snapshot goes");
        for f in ["vmstate.bin", "mem.bin", "vmstate.bin.tmp", "meta.json"] {
            assert!(exists(&format!("{}/vm-damaged/{}", inst, f)), "vm-damaged/{f} must be left alone");
        }
        assert_eq!(record_of(&mgr, "vm-alive").await, Some((VmState::Running, Some(vmm.pid()))));
        assert!(!exists(&format!("{}/vm-alive/vmstate.bin", inst)));
        assert!(exists(&format!("{}/vm-alive/mem.bin", inst)), "a live guest's memory file must stay");
    }

    #[tokio::test]
    async fn test_start_up_changes_no_file_without_the_data_dir_lock() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-dead", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        snapshot_files(&dir);
        std::fs::write(format!("{}/meta.json", dir), meta_line("vm-dead", Some(999_999), "running")).unwrap();

        let first = mgr_for(data_dir.clone());
        assert!(first.hold_data_dir_lock());
        let second = mgr_for(data_dir.clone());
        second.reconcile().await;
        assert_eq!(meta_on_disk(&data_dir, "vm-dead").state, VmState::Running, "no record is written");
        assert!(exists(&format!("{}/vmstate.bin", dir)) && exists(&format!("{}/mem.bin", dir)), "no file is removed");
        drop(second);
        drop(first);

        let third = mgr_for(data_dir.clone());
        third.reconcile().await;
        assert_eq!(meta_on_disk(&data_dir, "vm-dead").state, VmState::Stopped);
        assert!(!exists(&format!("{}/vmstate.bin", dir)) && !exists(&format!("{}/mem.bin", dir)));
    }

    // -- the busy mark --

    #[tokio::test]
    async fn test_a_second_sleep_waits_for_the_first_and_other_operations_are_refused() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir); // FLU-70: synced before the sleep is recorded
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let api = FakeApi::serve_with(
            &format!("{}/fc.sock", dir),
            "Running",
            Behavior { write_snapshot: true, delay: Some(("PUT /snapshot/create", Duration::from_millis(1500))), ..Default::default() },
        );
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        let first = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.sleep_vm("vm-1").await })
        };
        while !mgr.op_in_progress("vm-1") {
            sleep(Duration::from_millis(5)).await;
        }
        // Another kind: refused at once.
        let t = Instant::now();
        assert_eq!(mgr.wake_vm("vm-1").await, Err(OpError::Busy));
        assert_eq!(mgr.pause("vm-1").await, Err(OpError::Busy));
        assert_eq!(mgr.resume("vm-1").await, Err(OpError::Busy));
        assert_eq!(mgr.start("vm-1").await, Err(OpError::Busy));
        assert_eq!(mgr.fork("vm-1", "vm-2", "c").await, Err(BUSY.to_string()));
        // Well below the 1.5 s the first one takes: a refusal must not wait
        // (a tighter bound flaked under load).
        assert!(t.elapsed() < Duration::from_millis(1000), "a refusal must not wait");
        // The same kind: waits, then answers from what the first one left.
        assert_eq!(mgr.sleep_vm("vm-1").await, Ok(()));
        assert_eq!(first.await.unwrap(), Ok(()));
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)));
        let creates = api.requests().iter().filter(|l| l.starts_with("PUT /snapshot/create")).count();
        assert_eq!(creates, 1, "the second sleep must not snapshot again: {:?}", api.requests());
        assert!(lock_std(&mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_nothing_starts_while_a_vm_is_being_stopped_and_marks_go_with_their_guards() {
        let tmp = tempfile::tempdir().unwrap();
        let mgr = mgr_for(tmp.path().to_str().unwrap().to_string());
        let closing = mgr.begin_close("vm-1");
        assert!(matches!(mgr.begin_op("vm-1", OpKind::Sleep).await, Err(OpError::Busy)));
        drop(closing);
        let op = mgr.begin_op("vm-1", OpKind::Sleep).await.expect("free again");
        assert!(matches!(mgr.begin_op("vm-1", OpKind::Wake).await, Err(OpError::Busy)));
        // Another sandbox is not affected.
        drop(mgr.begin_op("vm-2", OpKind::Wake).await.expect("vm-2 is free"));
        drop(op);
        assert!(lock_std(&mgr.busy).is_empty(), "no entry outlives its guards");
    }

    #[tokio::test]
    async fn test_every_error_path_of_sleep_lets_go_of_the_vm() {
        for case in ["PATCH /vm", "PUT /snapshot/create", "meta.json", "unknown", "stopped"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let fail = if case.starts_with("P") { vec![case] } else { vec![] };
            // FLU-70: for the meta.json case to reach the record write, the
            // snapshot and the disk image must be there.
            disk_image(&dir);
            let api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { fail, write_snapshot: true, ..Default::default() });
            let mgr = mgr_for(data_dir.clone());
            let mut v = running_vm("vm-1", vmm.pid());
            if case == "stopped" {
                v.state = VmState::Stopped;
            }
            mgr.inner.lock().await.vms.push(v);
            if case == "meta.json" {
                break_meta_writes(&dir);
            }
            let id = if case == "unknown" { "vm-nope" } else { "vm-1" };

            let r = mgr.sleep_vm(id).await;
            assert!(r.is_err(), "{case}: {r:?}");
            assert!(lock_std(&mgr.busy).is_empty(), "{case}: the mark must be released");
            drop(mgr.begin_op(id, OpKind::Wake).await.unwrap_or_else(|e| panic!("{case}: {e}")));
            match case {
                "stopped" => assert!(matches!(r, Err(OpError::InvalidState { state: VmState::Stopped, .. }))),
                "unknown" => assert_eq!(r, Err(OpError::Other("NotFound".into()))),
                _ => {
                    // Nothing committed: the VM runs on, with its VMM.
                    assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(vmm.pid()))), "{case}");
                    assert!(!marker_of(&mgr, "vm-1").await, "{case}");
                    assert!(vmm.alive(), "{case}: the VMM must be left alive");
                    // FLU-70: and the guest is running again. A failed sleep
                    // used to leave it paused under a `running` record.
                    assert_eq!(api.state(), "Running", "{case}: {:?}", api.requests());
                    for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                        assert!(!exists(&format!("{}/{}", dir, f)), "{case}: {f}");
                    }
                }
            }
        }
    }

    #[tokio::test]
    async fn test_every_error_path_of_wake_lets_go_of_the_vm_and_leaves_no_vmm() {
        for case in ["load", "resume", "meta.json", "spawn", "missing", "running"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            snapshot_files(&dir);
            std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
            let fail = match case {
                "load" => vec!["PUT /snapshot/load"],
                "resume" => vec!["PATCH /vm"],
                _ => vec![],
            };
            let _api = FakeApi::serve_with(&format!("{}/fc.sock.fake", dir), "Not started", Behavior { fail, ..Default::default() });
            let bin = if case == "spawn" { "/nonexistent/firecracker".to_string() } else { fake_fc_bin() };
            let mgr = mgr_with_fc(data_dir.clone(), &bin);
            let mut v = sleeping_vm("vm-1");
            if case == "running" {
                v.state = VmState::Running;
            }
            mgr.inner.lock().await.vms.push(v);
            if case == "meta.json" {
                break_meta_writes(&dir);
            }
            if case == "missing" {
                std::fs::remove_file(format!("{}/mem.bin", dir)).unwrap();
            }

            let r = mgr.wake_vm("vm-1").await;
            assert!(lock_std(&mgr.busy).is_empty(), "{case}: the mark must be released");
            drop(mgr.begin_op("vm-1", OpKind::Sleep).await.unwrap_or_else(|e| panic!("{case}: {e}")));
            match case {
                // Idempotent: nothing is spawned.
                "running" => {
                    assert_eq!(r, Ok(0));
                    assert!(!exists(&format!("{}/fc.sock.pid", dir)));
                }
                "missing" => {
                    assert!(matches!(r, Err(OpError::InvalidState { state: VmState::Stopped, .. })), "{r:?}");
                    assert!(!exists(&format!("{}/fc.sock.pid", dir)), "nothing may be spawned for an incomplete snapshot");
                }
                _ => {
                    assert!(matches!(r, Err(OpError::Other(_))), "{case}: {r:?}");
                    // Back where it was: asleep, marker as it was, no VMM.
                    assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)), "{case}");
                    assert!(marker_of(&mgr, "vm-1").await, "{case}");
                    assert!(exists(&format!("{}/vmstate.bin", dir)) && exists(&format!("{}/mem.bin", dir)), "{case}");
                    let disk = meta_on_disk(&data_dir, "vm-1");
                    assert_eq!((disk.state, disk.pid, disk.snapshot_valid), (VmState::Sleeping, None, true), "{case}");
                    if case != "spawn" {
                        let pid = spawned_pid(&dir).await;
                        assert!(gone_soon(pid).await, "{case}: the VMM the wake spawned must be gone");
                    }
                }
            }
        }
    }

    #[tokio::test]
    async fn test_stop_and_delete_end_a_sleep_stuck_in_a_firecracker_call() {
        for delete in [false, true] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            let vmm = FakeVmm::spawn(&data_dir, "vm-1");
            // snapshot/create is not answered while the VMM lives.
            let api = FakeApi::serve_with(
                &format!("{}/fc.sock", dir),
                "Running",
                Behavior { hang: Some(("PUT /snapshot/create", Some(vmm.pid()))), ..Default::default() },
            );
            // What a snapshot in progress has written so far.
            snapshot_files(&dir);
            let mgr = mgr_for(data_dir.clone());
            mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

            let sleeping = {
                let m = Arc::clone(&mgr);
                tokio::spawn(async move { m.sleep_vm("vm-1").await })
            };
            while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
                sleep(Duration::from_millis(5)).await;
            }
            let t = Instant::now();
            if delete {
                mgr.delete("vm-1").await.unwrap();
            } else {
                mgr.stop("vm-1").await.unwrap();
            }
            assert!(t.elapsed() < CLOSE_WAIT, "took {:?}", t.elapsed());
            let r = sleeping.await.unwrap();
            assert!(matches!(r, Err(OpError::Other(_))), "the sleep must fail, not commit: {r:?}");
            if delete {
                assert!(record_of(&mgr, "vm-1").await.is_none());
                assert!(!Path::new(&dir).exists());
            } else {
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Stopped, None)));
                assert!(!marker_of(&mgr, "vm-1").await);
                let disk = meta_on_disk(&data_dir, "vm-1");
                assert_eq!((disk.state, disk.snapshot_valid), (VmState::Stopped, false));
                assert!(!exists(&format!("{}/vmstate.bin", dir)) && !exists(&format!("{}/mem.bin", dir)));
            }
            assert!(lock_std(&mgr.busy).is_empty());
        }
    }

    #[tokio::test]
    async fn test_stop_answers_busy_when_the_operation_does_not_let_go() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        // A Firecracker call that never returns, VMM or not.
        let api = FakeApi::serve_with(
            &format!("{}/fc.sock", dir),
            "Running",
            Behavior { hang: Some(("PUT /snapshot/create", None)), ..Default::default() },
        );
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        let sleeping = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.sleep_vm("vm-1").await })
        };
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
            sleep(Duration::from_millis(5)).await;
        }

        let t = Instant::now();
        // FLU-4 S4' changed the answer on purpose: stop has written
        // `stopped`, so it says so (it used to answer a plain `busy`).
        assert_eq!(mgr.stop("vm-1").await, Err(OpError::invalid(&VmState::Stopped, STOP_GAVE_UP)));
        assert!(t.elapsed() >= CLOSE_WAIT && t.elapsed() < CLOSE_WAIT + Duration::from_secs(8), "{:?}", t.elapsed());
        // The VMM was ended and the record says so, on disk too: a retry finds
        // a stopped VM.
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Stopped, None)));
        assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Stopped);
        assert_eq!(mgr.delete("vm-1").await, Err(STOP_GAVE_UP.to_string()));
        assert!(Path::new(&dir).exists(), "nothing may be removed under an operation that holds the VM");

        // The stuck operation goes away (its caller was dropped): the mark
        // goes with it, and the retry gets through.
        sleeping.abort();
        let _ = sleeping.await;
        assert!(!mgr.op_in_progress("vm-1"));
        mgr.delete("vm-1").await.unwrap();
        assert!(!Path::new(&dir).exists());
        drop(vmm);
    }

    // -- sleep --

    #[tokio::test]
    async fn test_a_sleep_whose_record_cannot_be_written_leaves_the_vm_as_it_was() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");
        disk_image(&dir); // FLU-70: so that the sleep gets as far as the record
        let api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Paused", Behavior { write_snapshot: true, ..Default::default() });
        let mgr = mgr_for(data_dir.clone());
        let mut v = running_vm("vm-1", vmm.pid());
        v.state = VmState::Paused;
        mgr.inner.lock().await.vms.push(v);
        break_meta_writes(&dir);

        assert!(mgr.sleep_vm("vm-1").await.is_err());
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Paused, Some(vmm.pid()))), "the state it had");
        assert!(!marker_of(&mgr, "vm-1").await);
        assert!(vmm.alive(), "a VMM whose sleep was not recorded must not be killed");
        // FLU-70: the snapshot is undone; a guest that was paused before the
        // sleep is not resumed by its failure.
        for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
            assert!(!exists(&format!("{}/{}", dir, f)), "{f}");
        }
        assert!(!api.bodies().iter().any(|b| b.contains("Resumed")), "{:?}", api.requests());
        assert_eq!(api.state(), "Paused");
    }

    /// A manager whose `/proc` is the directory `proc_root`, in which pid
    /// 4000 is the Firecracker of `<data_dir>/instances/vm-1` and maps that
    /// instance's `mem.bin` (a guest that was woken runs on it).
    fn mgr_with_mapped_mem(data_dir: &str, proc_root: &Path) -> Arc<Manager> {
        use std::os::unix::fs::MetadataExt;
        let dir = format!("{}/instances/vm-1", data_dir);
        let p = proc_root.join("4000");
        std::fs::create_dir_all(&p).unwrap();
        std::fs::write(p.join("cmdline"), format!("firecracker\0--api-sock\0{}/fc.sock\0", dir)).unwrap();
        let mem = format!("{}/mem.bin", dir);
        let st = std::fs::metadata(&mem).unwrap();
        let (ma, mi) = dev_major_minor(st.dev());
        std::fs::write(
            p.join("maps"),
            format!("7f0000000000-7f0010000000 rw-p 00000000 {:02x}:{:02x} {} {}\n", ma, mi, st.ino(), mem),
        )
        .unwrap();
        Manager::with_proc_root(
            data_dir.to_string(),
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
            proc_root.to_path_buf(),
        )
    }

    #[tokio::test]
    async fn test_a_refused_snapshot_create_is_undone_and_the_guest_given_back() {
        // (record state, a live VMM maps mem.bin)
        for (was, mapped) in [(VmState::Running, false), (VmState::Paused, false), (VmState::Running, true)] {
            let what = format!("{was} mapped={mapped}");
            let tmp = tempfile::tempdir().unwrap();
            let base = std::fs::canonicalize(tmp.path()).unwrap();
            let data_dir = base.join("data").to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            disk_image(&dir);
            // What a create that failed part-way leaves (lab, round 3 E): a
            // complete-looking state file and a partly written memory file.
            std::fs::write(format!("{}/vmstate.bin.tmp", dir), b"device state").unwrap();
            std::fs::write(format!("{}/mem.bin", dir), b"half the memory").unwrap();
            let api = FakeApi::serve_with(
                &format!("{}/fc.sock", dir),
                if was == VmState::Paused { "Paused" } else { "Running" },
                Behavior { fail: vec!["PUT /snapshot/create"], ..Default::default() },
            );
            let mgr = if mapped { mgr_with_mapped_mem(&data_dir, &base.join("proc")) } else { mgr_for(data_dir.clone()) };
            let mut v = running_vm("vm-1", 4000);
            v.state = was.clone();
            mgr.inner.lock().await.vms.push(v);

            let r = mgr.sleep_vm("vm-1").await;
            assert!(matches!(&r, Err(OpError::Other(m)) if m.contains("snapshot/create")), "{what}: {r:?}");
            assert!(!exists(&format!("{}/vmstate.bin", dir)), "{what}");
            assert!(!exists(&format!("{}/vmstate.bin.tmp", dir)), "{what}");
            // Removed, unless the guest runs on it.
            assert_eq!(exists(&format!("{}/mem.bin", dir)), mapped, "{what}");
            // Resumed when this sleep paused it; a paused sandbox stays paused.
            let resumes = api.bodies().iter().filter(|b| b.contains("Resumed")).count();
            assert_eq!(resumes, usize::from(was == VmState::Running), "{what}: {:?}", api.requests());
            assert_eq!(api.state(), if was == VmState::Paused { "Paused" } else { "Running" }, "{what}");
            // The record does not change, and nothing is written.
            assert_eq!(record_of(&mgr, "vm-1").await, Some((was.clone(), Some(4000))), "{what}");
            assert!(!marker_of(&mgr, "vm-1").await, "{what}");
            assert!(!exists(&format!("{}/meta.json", dir)), "{what}");
            assert!(lock_std(&mgr.busy).is_empty(), "{what}: the mark must be released");
        }
    }

    #[tokio::test]
    async fn test_a_sleep_whose_create_and_resume_both_fail_records_paused() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let _api = FakeApi::serve_with(
            &format!("{}/fc.sock", dir),
            "Running",
            Behavior { fail: vec!["PUT /snapshot/create"], fail_body: vec!["Resumed"], ..Default::default() },
        );
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        assert!(mgr.sleep_vm("vm-1").await.is_err());
        // The guest is paused and nothing will resume it: the record says so.
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Paused, Some(vmm.pid()))));
        let disk = meta_on_disk(&data_dir, "vm-1");
        assert_eq!((disk.state, disk.pid, disk.snapshot_valid), (VmState::Paused, Some(vmm.pid()), false));
        assert!(lock_std(&mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_a_sleep_that_fails_after_the_create_is_undone_like_a_failed_create() {
        for case in ["fsync", "rename", "meta.json"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            match case {
                // No disk image to sync.
                "fsync" => {}
                // A non-empty directory is in the way of the state file's name.
                "rename" => {
                    disk_image(&dir);
                    std::fs::create_dir_all(format!("{}/vmstate.bin/x", dir)).unwrap();
                }
                _ => {
                    disk_image(&dir);
                    break_meta_writes(&dir);
                }
            }
            let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { write_snapshot: true, ..Default::default() });
            let mgr = mgr_for(data_dir.clone());
            mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

            let r = mgr.sleep_vm("vm-1").await;
            assert!(matches!(r, Err(OpError::Other(_))), "{case}: {r:?}");
            assert!(std::fs::metadata(format!("{}/vmstate.bin", dir)).map(|m| !m.is_file()).unwrap_or(true), "{case}: no state file");
            assert!(!exists(&format!("{}/vmstate.bin.tmp", dir)), "{case}");
            assert!(!exists(&format!("{}/mem.bin", dir)), "{case}");
            assert_eq!(api.state(), "Running", "{case}: the guest must be resumed: {:?}", api.requests());
            assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(vmm.pid()))), "{case}");
            assert!(!marker_of(&mgr, "vm-1").await, "{case}");
            assert!(vmm.alive(), "{case}: the VMM must be left alive");
            assert!(lock_std(&mgr.busy).is_empty(), "{case}");
        }
    }

    #[tokio::test]
    async fn test_a_sleep_is_complete_renamed_and_recorded_before_its_vmm_is_killed() {
        // Observable here: the files and the record at the moment the VMM
        // dies. Whether the fsyncs happened is not (nothing in a unit test
        // can tell a synced file from an unsynced one).
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { write_snapshot: true, ..Default::default() });
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        let (d, vpid) = (dir.clone(), vmm.pid());
        let at_death = std::thread::spawn(move || {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
            while !proc_gone(vpid) {
                assert!(std::time::Instant::now() < deadline, "the VMM was never killed");
                std::thread::sleep(std::time::Duration::from_micros(100));
            }
            (
                exists(&format!("{}/vmstate.bin", d)),
                exists(&format!("{}/vmstate.bin.tmp", d)),
                std::fs::read_to_string(format!("{}/meta.json", d)).unwrap(),
            )
        });
        mgr.sleep_vm("vm-1").await.unwrap();
        let (state_file, tmp_file, meta) = at_death.join().unwrap();
        assert!(state_file && !tmp_file, "renamed before the kill: vmstate.bin {state_file}, .tmp {tmp_file}");
        let meta: Meta = serde_json::from_str(&meta).unwrap();
        assert_eq!((meta.state, meta.snapshot_valid), (VmState::Sleeping, true));
        // Firecracker was told to write the state file under the temp name.
        let create = api.bodies().into_iter().find(|b| b.contains("snapshot_path")).unwrap();
        let v: serde_json::Value = serde_json::from_str(&create).unwrap();
        assert_eq!(v["snapshot_path"], serde_json::json!(format!("{}/vmstate.bin.tmp", dir)));
        assert_eq!(v["mem_file_path"], serde_json::json!(format!("{}/mem.bin", dir)));
        assert!(exists(&format!("{}/mem.bin", dir)));
        drop(vmm);
    }

    // -- wake --

    #[tokio::test]
    async fn test_wake_is_refused_from_every_state_but_sleeping() {
        let tmp = tempfile::tempdir().unwrap();
        let mgr = mgr_with_fc(tmp.path().to_str().unwrap().to_string(), "/nonexistent/firecracker");
        for state in [VmState::Stopped, VmState::Error, VmState::Paused, VmState::Creating, VmState::Pooled] {
            let mut v = running_vm("vm-1", 4242);
            v.state = state.clone();
            mgr.inner.lock().await.vms = vec![v];
            match mgr.wake_vm("vm-1").await {
                Err(OpError::InvalidState { state: s, msg }) => {
                    assert_eq!(s, state);
                    assert!(msg.starts_with("InvalidState"), "{msg}");
                }
                other => panic!("{state}: {other:?}"),
            }
            assert_eq!(record_of(&mgr, "vm-1").await.unwrap().0, state, "a refusal changes nothing");
        }
        // Already running: success, nothing done.
        mgr.inner.lock().await.vms = vec![running_vm("vm-1", 4242)];
        assert_eq!(mgr.wake_vm("vm-1").await, Ok(0));
        // And sleep's own table.
        for state in [VmState::Stopped, VmState::Error, VmState::Creating, VmState::Pooled] {
            let mut v = running_vm("vm-1", 4242);
            v.state = state.clone();
            mgr.inner.lock().await.vms = vec![v];
            assert!(matches!(mgr.sleep_vm("vm-1").await, Err(OpError::InvalidState { state: ref s, .. }) if *s == state));
        }
        mgr.inner.lock().await.vms = vec![sleeping_vm("vm-1")];
        assert_eq!(mgr.sleep_vm("vm-1").await, Ok(()));
    }

    #[tokio::test]
    async fn test_wake_checks_both_snapshot_files_before_it_spawns_anything() {
        for case in ["no vmstate", "no mem", "empty vmstate", "empty mem", "unreadable vmstate"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            snapshot_files(&dir);
            std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
            let (vmstate, mem) = (format!("{}/vmstate.bin", dir), format!("{}/mem.bin", dir));
            match case {
                "no vmstate" => std::fs::remove_file(&vmstate).unwrap(),
                "no mem" => std::fs::remove_file(&mem).unwrap(),
                "empty vmstate" => std::fs::write(&vmstate, b"").unwrap(),
                "empty mem" => std::fs::write(&mem, b"").unwrap(),
                _ => {
                    std::fs::remove_file(&vmstate).unwrap();
                    std::os::unix::fs::symlink("vmstate.bin", &vmstate).unwrap(); // ELOOP
                }
            }
            // A spawn would fail loudly with this binary.
            let mgr = mgr_with_fc(data_dir.clone(), "/nonexistent/firecracker");
            mgr.inner.lock().await.vms.push(sleeping_vm("vm-1"));

            let r = mgr.wake_vm("vm-1").await;
            if case == "unreadable vmstate" {
                // Could not look: 500, and nothing changes.
                assert!(matches!(&r, Err(OpError::Other(m)) if m.starts_with("cannot examine")), "{r:?}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)));
                assert!(marker_of(&mgr, "vm-1").await);
                assert!(exists(&vmstate) && exists(&mem));
                assert!(meta_on_disk(&data_dir, "vm-1").snapshot_valid);
            } else {
                assert!(matches!(r, Err(OpError::InvalidState { state: VmState::Stopped, .. })), "{case}: {r:?}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Stopped, None)), "{case}");
                let disk = meta_on_disk(&data_dir, "vm-1");
                assert_eq!((disk.state, disk.snapshot_valid), (VmState::Stopped, false), "{case}");
                assert!(!exists(&vmstate) && !exists(&mem), "{case}: the rest of the snapshot goes");
            }
        }
    }

    #[tokio::test]
    async fn test_wake_writes_running_before_the_guest_resumes() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        snapshot_files(&dir);
        std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
        let api = FakeApi::serve_with(
            &format!("{}/fc.sock.fake", dir),
            "Not started",
            Behavior { watch_meta: Some(format!("{}/meta.json", dir)), ..Default::default() },
        );
        let mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
        mgr.inner.lock().await.vms.push(sleeping_vm("vm-1"));

        mgr.wake_vm("vm-1").await.unwrap();
        let pid = spawned_pid(&dir).await;

        let lines = api.requests();
        let load = lines.iter().position(|l| l.starts_with("PUT /snapshot/load")).expect("a load");
        let resume = lines.iter().position(|l| l.starts_with("PATCH /vm")).expect("a resume");
        assert!(load < resume, "{lines:?}");
        assert!(api.bodies()[load].contains("\"resume_vm\":false"), "loaded paused: {}", api.bodies()[load]);
        assert!(api.bodies()[resume].contains("Resumed"));
        // What meta.json said when the resume reached the VMM.
        let (_, at_resume) = api.meta_seen().into_iter().find(|(l, _)| l.starts_with("PATCH /vm")).unwrap();
        let at_resume: Meta = serde_json::from_str(&at_resume).expect("meta.json at the resume");
        assert_eq!((at_resume.state, at_resume.pid, at_resume.snapshot_valid), (VmState::Running, Some(pid), false));
        // Afterwards: running on the new VMM, the state file gone, the memory kept.
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(pid))));
        assert!(!marker_of(&mgr, "vm-1").await);
        assert!(!exists(&format!("{}/vmstate.bin", dir)));
        assert!(exists(&format!("{}/mem.bin", dir)), "the guest runs on mem.bin");

        // And a stop ends that VMM.
        mgr.stop("vm-1").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    // -- review fixes (FLU-4) --

    /// A sleeping `vm-1` with its snapshot, its disk image and its record on
    /// disk, under a manager whose Firecracker is `fc_bin`; the API of the
    /// VMM a wake or start spawns is served with `behavior`.
    async fn asleep(marker: bool, behavior: Behavior, fc_bin: &str) -> (tempfile::TempDir, String, String, Arc<Manager>, FakeApi) {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        snapshot_files(&dir);
        let line = if marker { sleeper_line("vm-1") } else { meta_line("vm-1", None, "sleeping") };
        std::fs::write(format!("{}/meta.json", dir), line).unwrap();
        let api = FakeApi::serve_with(&format!("{}/fc.sock.fake", dir), "Not started", behavior);
        let mgr = mgr_with_fc(data_dir.clone(), fc_bin);
        let mut v = sleeping_vm("vm-1");
        v.snapshot_valid = marker;
        mgr.inner.lock().await.vms.push(v);
        (tmp, data_dir, dir, mgr, api)
    }

    fn set_mtime(path: &str, t: std::time::SystemTime) {
        std::fs::File::options().write(true).open(path).unwrap().set_modified(t).unwrap();
    }

    /// The disk image of `dir` made newer (`later`) or older than its state file.
    fn disk_vs_snapshot(dir: &str, image: &str, later: bool) {
        let t0 = std::fs::metadata(format!("{}/vmstate.bin", dir)).unwrap().modified().unwrap();
        let t = if later { t0 + Duration::from_secs(5) } else { t0 - Duration::from_secs(5) };
        set_mtime(image, t);
    }

    fn set_aside(dir: &str) -> bool {
        !exists(&format!("{}/vmstate.bin", dir))
            && !exists(&format!("{}/mem.bin", dir))
            && read(&format!("{}/vmstate.bin.outrun", dir)) == "device state"
            && read(&format!("{}/mem.bin.outrun", dir)) == "guest memory"
    }

    #[tokio::test]
    async fn test_a_failed_resume_goes_back_to_sleep_only_on_proof_that_the_guest_did_not_run() {
        for case in ["refused, still paused", "refused, but running", "no answer"] {
            let behavior = match case {
                "refused, still paused" => Behavior { fail: vec!["PATCH /vm"], ..Default::default() },
                "refused, but running" => Behavior { fail: vec!["PATCH /vm"], refused_sets: Some("Running"), ..Default::default() },
                _ => Behavior { drop: vec!["PATCH /vm"], ..Default::default() },
            };
            let (_tmp, data_dir, dir, mgr, _api) = asleep(true, behavior, &fake_fc_bin()).await;

            let r = mgr.wake_vm("vm-1").await;
            let pid = spawned_pid(&dir).await;
            assert!(gone_soon(pid).await, "{case}: the VMM the wake spawned must be gone");
            assert!(lock_std(&mgr.busy).is_empty(), "{case}");
            if case == "refused, still paused" {
                // Firecracker answered, and the VM never left `Paused`.
                assert!(matches!(r, Err(OpError::Other(_))), "{case}: {r:?}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)), "{case}");
                assert!(marker_of(&mgr, "vm-1").await, "{case}");
                let disk = meta_on_disk(&data_dir, "vm-1");
                assert_eq!((disk.state, disk.snapshot_valid), (VmState::Sleeping, true), "{case}");
                assert!(exists(&format!("{}/vmstate.bin", dir)) && exists(&format!("{}/mem.bin", dir)), "{case}");
            } else {
                // The guest may have run: never woken from this snapshot again.
                assert!(matches!(r, Err(OpError::InvalidState { state: VmState::Stopped, .. })), "{case}: {r:?}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Stopped, None)), "{case}");
                let disk = meta_on_disk(&data_dir, "vm-1");
                assert_eq!((disk.state, disk.pid, disk.snapshot_valid), (VmState::Stopped, None, false), "{case}");
                assert!(set_aside(&dir), "{case}: the snapshot is set aside, not removed");
                // start and stop clear what was set aside.
                if case == "no answer" {
                    mgr.start("vm-1").await.unwrap();
                    let started = record_of(&mgr, "vm-1").await.unwrap().1.unwrap();
                    assert!(!exists(&format!("{}/vmstate.bin.outrun", dir)) && !exists(&format!("{}/mem.bin.outrun", dir)));
                    mgr.stop("vm-1").await.unwrap();
                    assert!(gone_soon(started).await);
                }
            }
        }
    }

    #[tokio::test]
    async fn test_a_vmm_that_dies_under_a_wake_leaves_the_record_to_the_wake() {
        // The VMM dies after the commit and before the resume answers: the
        // exit reaper used to record `error` first, and the wake answered 500
        // with its snapshot under that `error`.
        let (_tmp, data_dir, dir, mgr, _api) =
            asleep(true, Behavior { delay: Some(("PATCH /vm", Duration::from_millis(400))), ..Default::default() }, &fake_fc_bin())
                .await;
        let waking = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.wake_vm("vm-1").await })
        };
        let pid = spawned_pid(&dir).await;
        while record_of(&mgr, "vm-1").await.map(|r| r.0) != Some(VmState::Running) {
            sleep(Duration::from_millis(5)).await;
        }
        kill_and_reap(pid);
        // The reaper has run by now: it left the record alone.
        sleep(Duration::from_millis(100)).await;
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(pid))));
        // The resume is answered (the stand-in API outlives the VMM), and
        // the wake goes on; the next sweep finds the VMM dead.
        let _ = waking.await.unwrap();
        mgr.sweep_dead().await;
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Error, None)));
        assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Error);
    }

    #[tokio::test]
    async fn test_wake_judges_a_record_without_the_marker_before_it_spawns_anything() {
        for later in [true, false] {
            let (_tmp, data_dir, dir, mgr, _api) = asleep(false, Behavior::default(), &fake_fc_bin()).await;
            disk_vs_snapshot(&dir, &format!("{}/rootfs.ext4", dir), later);

            let r = mgr.wake_vm("vm-1").await;
            if later {
                assert!(matches!(&r, Err(OpError::InvalidState { state: VmState::Stopped, msg }) if msg.contains("rootfs.ext4")), "{r:?}");
                assert!(!exists(&format!("{}/fc.sock.pid", dir)), "nothing may be spawned for a stale snapshot");
                assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Stopped);
                assert!(set_aside(&dir));
            } else {
                assert!(r.is_ok(), "{r:?}");
                let pid = spawned_pid(&dir).await;
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(pid))));
                mgr.stop("vm-1").await.unwrap();
                assert!(gone_soon(pid).await);
            }
        }
    }

    #[tokio::test]
    async fn test_a_wake_on_another_image_goes_on_only_while_that_image_is_unchanged() {
        for later in [false, true] {
            let tmp = tempfile::tempdir().unwrap();
            let image = format!("{}/parents-rootfs.ext4", tmp.path().to_str().unwrap());
            std::fs::write(&image, b"the parent's disk").unwrap();
            let behavior = Behavior { drive: Some(image.clone()), ..Default::default() };
            let (_tmp, data_dir, dir, mgr, api) = asleep(true, behavior, &fake_fc_bin()).await;
            disk_vs_snapshot(&dir, &image, later);

            let r = mgr.wake_vm("vm-1").await;
            let pid = spawned_pid(&dir).await;
            // Asked after the load, before anything was committed or resumed.
            let lines = api.requests();
            let (load, config) = (
                lines.iter().position(|l| l.starts_with("PUT /snapshot/load")).unwrap(),
                lines.iter().position(|l| l.starts_with("GET /vm/config")).expect("the drive is asked for"),
            );
            assert!(load < config, "{lines:?}");
            if later {
                // Changed on purpose (fix list item 6): no path in the
                // answer; the image is in the log.
                assert!(matches!(&r, Err(OpError::InvalidState { state: VmState::Stopped, msg }) if msg == WAKE_REFUSED_FORKED_BEFORE), "{r:?}");
                assert!(gone_soon(pid).await);
                assert!(!api.bodies().iter().any(|b| b.contains("Resumed")), "never resumed: {lines:?}");
                assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Stopped);
                assert!(set_aside(&dir));
            } else {
                assert!(r.is_ok(), "{r:?}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(pid))));
                mgr.stop("vm-1").await.unwrap();
                assert!(gone_soon(pid).await);
            }
        }
    }

    #[tokio::test]
    async fn test_start_up_sets_an_outrun_snapshot_aside_and_keeps_it_there() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-old", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        snapshot_files(&dir);
        // An older agent's sleeper whose guest ran after its snapshot.
        std::fs::write(format!("{}/meta.json", dir), meta_line("vm-old", None, "sleeping")).unwrap();
        disk_vs_snapshot(&dir, &format!("{}/rootfs.ext4", dir), true);

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;
        assert_eq!(meta_on_disk(&data_dir, "vm-old").state, VmState::Stopped);
        assert!(set_aside(&dir), "set aside, not removed");
        drop(mgr);
        // The next start-up does not remove them either.
        let again = mgr_for(data_dir.clone());
        again.reconcile().await;
        assert!(set_aside(&dir), "start-up leaves what was set aside");
        // stop does.
        again.stop("vm-old").await.unwrap();
        assert!(!exists(&format!("{}/vmstate.bin.outrun", dir)) && !exists(&format!("{}/mem.bin.outrun", dir)));
    }

    #[tokio::test]
    async fn test_start_up_leaves_a_sleeper_it_cannot_judge_but_removes_its_temp_state_file() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-q", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        std::fs::write(format!("{}/mem.bin", dir), b"guest memory").unwrap();
        std::fs::write(format!("{}/vmstate.bin.tmp", dir), b"half a state").unwrap();
        // stat fails with ELOOP: "cannot tell".
        std::os::unix::fs::symlink("vmstate.bin", format!("{}/vmstate.bin", dir)).unwrap();
        let line = meta_line("vm-q", None, "sleeping");
        std::fs::write(format!("{}/meta.json", dir), &line).unwrap();

        let mgr = mgr_for(data_dir.clone());
        mgr.reconcile().await;
        assert_eq!(record_of(&mgr, "vm-q").await, Some((VmState::Sleeping, None)));
        assert_eq!(read(&format!("{}/meta.json", dir)), line, "nothing is written");
        assert!(exists(&format!("{}/vmstate.bin", dir)) && exists(&format!("{}/mem.bin", dir)), "nothing is removed");
        assert!(!exists(&format!("{}/vmstate.bin.tmp", dir)), "a temporary state file is never a snapshot (S9)");
    }

    #[tokio::test]
    async fn test_stop_ends_a_vmm_no_record_names_yet() {
        // A wake whose load never answers: the new VMM is not in the record,
        // and only its death ends the call the wake waits in.
        let tmp = tempfile::tempdir().unwrap();
        let pidfile = format!("{}/instances/vm-1/fc.sock.pid", tmp.path().to_str().unwrap());
        let behavior = Behavior { hang_pidfile: Some(("PUT /snapshot/load", pidfile)), ..Default::default() };
        let (_tmp, data_dir, dir, mgr, api) = {
            // `asleep` makes its own temp dir: build the same in `tmp`.
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            disk_image(&dir);
            snapshot_files(&dir);
            std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
            let api = FakeApi::serve_with(&format!("{}/fc.sock.fake", dir), "Not started", behavior);
            let mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
            mgr.inner.lock().await.vms.push(sleeping_vm("vm-1"));
            (tmp, data_dir, dir, mgr, api)
        };
        let waking = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.wake_vm("vm-1").await })
        };
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/load")) {
            sleep(Duration::from_millis(5)).await;
        }
        let pid = spawned_pid(&dir).await;
        let t = Instant::now();
        mgr.stop("vm-1").await.unwrap();
        assert!(t.elapsed() < CLOSE_WAIT, "took {:?}", t.elapsed());
        assert!(gone_soon(pid).await, "the VMM the wake spawned must be killed");
        assert!(waking.await.unwrap().is_err());
        assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Stopped);
        mgr.delete("vm-1").await.unwrap();
        assert!(!exists(&dir));
    }

    #[tokio::test]
    async fn test_a_late_firecracker_answer_after_a_stop_commits_nothing() {
        for op in ["sleep", "wake", "start"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            disk_image(&dir);
            let late = |what| Some((what, Duration::from_millis(600)));
            let mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
            let mut vmm = None;
            let api = match op {
                "sleep" => {
                    let fake = FakeVmm::spawn(&data_dir, "vm-1");
                    mgr.inner.lock().await.vms.push(running_vm("vm-1", fake.pid()));
                    vmm = Some(fake);
                    let b = Behavior { write_snapshot: true, delay: late("PUT /snapshot/create"), ..Default::default() };
                    FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", b)
                }
                "wake" => {
                    snapshot_files(&dir);
                    std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
                    mgr.inner.lock().await.vms.push(sleeping_vm("vm-1"));
                    let b = Behavior { delay: late("PUT /snapshot/load"), ..Default::default() };
                    FakeApi::serve_with(&format!("{}/fc.sock.fake", dir), "Not started", b)
                }
                _ => {
                    let mut v = running_vm("vm-1", 0);
                    v.state = VmState::Stopped;
                    v.pid = None;
                    mgr.inner.lock().await.vms.push(v);
                    let b = Behavior { delay: late("PUT /actions"), ..Default::default() };
                    FakeApi::serve_with(&format!("{}/fc.sock.fake", dir), "Not started", b)
                }
            };
            let running = {
                let m = Arc::clone(&mgr);
                let op = op.to_string();
                tokio::spawn(async move {
                    match op.as_str() {
                        "sleep" => m.sleep_vm("vm-1").await,
                        "wake" => m.wake_vm("vm-1").await.map(|_| ()),
                        _ => m.start("vm-1").await,
                    }
                })
            };
            let call = match op {
                "sleep" => "PUT /snapshot/create",
                "wake" => "PUT /snapshot/load",
                _ => "PUT /actions",
            };
            while !api.requests().iter().any(|l| l.starts_with(call)) {
                sleep(Duration::from_millis(5)).await;
            }
            mgr.stop("vm-1").await.unwrap();
            let r = running.await.unwrap();
            assert!(matches!(&r, Err(OpError::Other(m)) if m.starts_with("Aborted")), "{op}: {r:?}");
            assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Stopped, None)), "{op}");
            let disk = meta_on_disk(&data_dir, "vm-1");
            assert_eq!((disk.state, disk.pid, disk.snapshot_valid), (VmState::Stopped, None, false), "{op}");
            for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                assert!(!exists(&format!("{}/{}", dir, f)), "{op}: {f}");
            }
            if op == "sleep" {
                assert!(!vmm.as_mut().unwrap().alive(), "{op}");
            } else {
                let pid = spawned_pid(&dir).await;
                assert!(gone_soon(pid).await, "{op}: the VMM it spawned must be gone");
            }
            assert!(lock_std(&mgr.busy).is_empty(), "{op}");
        }
    }

    #[tokio::test]
    async fn test_a_sleep_overtaken_by_a_stop_before_its_kill_step_does_not_answer_success() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let _api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { write_snapshot: true, ..Default::default() });
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));

        // The sleep commits `sleeping` and then waits for the meta.json lock.
        let writing = mgr.meta_write_lock.lock().await;
        let sleeping = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.sleep_vm("vm-1").await })
        };
        while record_of(&mgr, "vm-1").await.map(|r| r.0) != Some(VmState::Sleeping) {
            sleep(Duration::from_millis(5)).await;
        }
        // A stop takes the record meanwhile.
        let stopping = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.stop("vm-1").await })
        };
        while record_of(&mgr, "vm-1").await.map(|r| r.0) != Some(VmState::Stopped) {
            sleep(Duration::from_millis(5)).await;
        }
        drop(writing);
        let r = sleeping.await.unwrap();
        assert!(matches!(&r, Err(OpError::Other(m)) if m.starts_with("Aborted")), "{r:?}");
        stopping.await.unwrap().unwrap();
        assert_eq!(meta_on_disk(&data_dir, "vm-1").state, VmState::Stopped);
        for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
            assert!(!exists(&format!("{}/{}", dir, f)), "{f}");
        }
        drop(vmm);
    }

    #[tokio::test]
    async fn test_a_sleep_whose_record_write_failed_owes_the_reverted_record() {
        // Another writer settles the failed write's claim on the owed list
        // between the failure and the revert (here: the test, standing in for
        // the liveness flush).
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let _api = FakeApi::serve_with(&format!("{}/fc.sock", dir), "Running", Behavior { write_snapshot: true, ..Default::default() });
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        break_meta_writes(&dir);

        let writing = mgr.meta_write_lock.lock().await;
        let sleeping = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.sleep_vm("vm-1").await })
        };
        while record_of(&mgr, "vm-1").await.map(|r| r.0) != Some(VmState::Sleeping) {
            sleep(Duration::from_millis(5)).await;
        }
        // Single-threaded runtime: from here the order is fixed. The sleep
        // gets the write lock and waits for `inner` (held here)...
        let held = mgr.inner.lock().await;
        drop(writing);
        for _ in 0..10 {
            tokio::task::yield_now().await;
        }
        // ...takes it for its snapshot of the record, fails to write, and
        // waits for `inner` again to revert — behind this second hold.
        drop(held);
        let held = mgr.inner.lock().await;
        assert!(lock_std(&mgr.meta_owed).contains("vm-1"), "the failed write is owed");
        lock_std(&mgr.meta_owed).remove("vm-1");
        drop(held);
        assert!(sleeping.await.unwrap().is_err());
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(vmm.pid()))));
        assert!(lock_std(&mgr.meta_owed).contains("vm-1"), "the reverted record must be owed to meta.json");
    }

    #[tokio::test]
    async fn test_a_same_kind_wait_is_bounded_and_answers_from_what_the_first_left() {
        // Below the control plane's 30 s call timeout (S4').
        assert!(SAME_OP_LIMIT < Duration::from_secs(30));
        let tmp = tempfile::tempdir().unwrap();
        let mgr = mgr_with_fc(tmp.path().to_str().unwrap().to_string(), "/nonexistent/firecracker");
        // Bounded: then busy.
        mgr.inner.lock().await.vms.push(running_vm("vm-1", 4242));
        let held = mgr.begin_op("vm-1", OpKind::Sleep).await.unwrap();
        let t = Instant::now();
        assert_eq!(mgr.sleep_vm("vm-1").await, Err(OpError::Busy));
        assert!(t.elapsed() >= SAME_OP_WAIT, "{:?}", t.elapsed());
        drop(held);
        // Wake and start wait for the first one, then answer from its result.
        for kind in [OpKind::Wake, OpKind::Start] {
            let mut v = sleeping_vm("vm-1");
            if kind == OpKind::Start {
                v.state = VmState::Stopped;
            }
            mgr.inner.lock().await.vms = vec![v];
            let held = mgr.begin_op("vm-1", kind).await.unwrap();
            let second = {
                let m = Arc::clone(&mgr);
                tokio::spawn(async move {
                    if kind == OpKind::Wake { m.wake_vm("vm-1").await.map(|_| ()) } else { m.start("vm-1").await }
                })
            };
            sleep(Duration::from_millis(200)).await;
            assert!(!second.is_finished(), "{kind:?}: the second one waits");
            mgr.inner.lock().await.vms[0].state = VmState::Running;
            drop(held);
            assert_eq!(second.await.unwrap(), Ok(()), "{kind:?}: already running");
        }
    }

    #[tokio::test]
    async fn test_a_fork_never_waits_for_its_parent() {
        let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        let held = f.mgr.begin_fork("vm-1").unwrap();
        let t = Instant::now();
        assert_eq!(f.mgr.fork("vm-1", "vm-2", "c").await, Err(BUSY.to_string()));
        assert!(t.elapsed() < Duration::from_secs(1), "{:?}", t.elapsed());
        assert!(record_of(&f.mgr, "vm-2").await.is_none(), "nothing reserved");
        assert!(!exists(&f.child_dir));
        drop(held);
    }

    #[tokio::test]
    async fn test_forks_of_a_sleeping_parent_share_it_and_everything_else_waits_or_is_refused() {
        let f = forking(VmState::Sleeping, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        // Another fork of the sleeping parent reads its files right now.
        let mut other = f.mgr.begin_fork("vm-1").unwrap();
        other.share();
        // This one joins it.
        f.mgr.fork("vm-1", "vm-2", "c").await.unwrap();
        let child_pid = spawned_pid(&f.child_dir).await;
        // To anything else the parent is busy.
        assert_eq!(f.mgr.wake_vm("vm-1").await, Err(OpError::Busy));
        assert_eq!(f.mgr.sleep_vm("vm-1").await, Err(OpError::Busy));
        assert_eq!(f.mgr.start("vm-1").await, Err(OpError::Busy));
        // A stop cannot pre-empt a file copy: it waits, and gives up saying
        // what it did.
        assert_eq!(f.mgr.stop("vm-1").await, Err(OpError::invalid(&VmState::Stopped, STOP_GAVE_UP)));
        drop(other);
        assert!(lock_std(&f.mgr.busy).is_empty());
        f.mgr.stop("vm-2").await.unwrap();
        assert!(gone_soon(child_pid).await);
    }

    #[tokio::test]
    async fn test_a_stop_of_the_child_pre_empts_its_fork() {
        let f = forking_with(
            VmState::Running,
            |_, child_dir| {
                let pidfile = format!("{}/fc.sock.pid", child_dir);
                (Behavior::default(), Behavior { hang_pidfile: Some(("PUT /snapshot/load", pidfile)), ..Default::default() })
            },
            &fake_fc_bin(),
        )
        .await;
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        while !f.child_api.requests().iter().any(|l| l.starts_with("PUT /snapshot/load")) {
            sleep(Duration::from_millis(5)).await;
        }
        let child_pid = spawned_pid(&f.child_dir).await;
        let t = Instant::now();
        f.mgr.stop("vm-2").await.unwrap();
        assert!(t.elapsed() < CLOSE_WAIT, "took {:?}", t.elapsed());
        assert!(gone_soon(child_pid).await, "the child's VMM must be killed");
        let r = tokio::time::timeout(Duration::from_secs(10), forked).await.expect("the fork must end").unwrap();
        assert!(r.is_err(), "{r:?}");
        // The reservation was the fork's: nothing of the child is left
        // (FLU-70 E2; the record used to stay as the stop's `stopped`).
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        assert!(!exists(&f.child_dir));
        assert!(lock_std(&f.mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_expose_of_a_sleeping_sandbox_keeps_its_marker_on_disk() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/instances/vm-1", data_dir)).unwrap();
        let mgr = mgr_for(data_dir.clone());
        let mut v = sleeping_vm("vm-1");
        v.ip = Some("10.231.0.2".into());
        mgr.inner.lock().await.vms.push(v);
        mgr.expose("vm-1", 8080, &HashSet::new(), None).await.unwrap();
        let disk = meta_on_disk(&data_dir, "vm-1");
        assert_eq!((disk.state, disk.snapshot_valid, disk.exposes.len()), (VmState::Sleeping, true, 1));
        mgr.unexpose("vm-1", 8080).await.unwrap();
        assert!(meta_on_disk(&data_dir, "vm-1").snapshot_valid);
    }

    // -- review fixes (FLU-70) --

    /// Networking "on" for a fork: the rule stand-in, a slot allocator, and
    /// the free slots before anything happened.
    fn netted(f: &mut Forking) -> (Arc<super::rule_fake::FakeRules>, Vec<u32>) {
        let rules = super::rule_fake::install(&mut f.mgr);
        let free = rules.free_slots();
        (rules, free)
    }

    /// The fork gave back what it claimed: every slot free that was, and the
    /// tap of each slot it pinned removed again.
    fn gave_back(rules: &super::rule_fake::FakeRules, free: &[u32], what: &str) {
        use super::rule_fake::RuleCall;
        assert_eq!(rules.free_slots(), free, "{what}: the child's slot must be free again");
        let calls = rules.calls();
        for c in &calls {
            if let RuleCall::PinTap { tap } = c {
                assert!(
                    calls.iter().any(|r| matches!(r, RuleCall::RemoveTap { tap: t, .. } if t == tap)),
                    "{what}: the tap {tap} must be removed: {calls:?}"
                );
            }
        }
    }

    #[tokio::test]
    async fn test_a_fork_refuses_a_parent_that_is_not_a_guest_before_it_reserves_anything() {
        for state in [VmState::Pooled, VmState::Creating, VmState::Stopped, VmState::Error] {
            let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
            f.mgr.inner.lock().await.vms[0].state = state.clone();
            let r = f.mgr.fork("vm-1", "vm-2", "c").await;
            assert!(matches!(&r, Err(e) if e.starts_with("InvalidState") && e.contains(state.as_str())), "{state}: {r:?}");
            assert!(record_of(&f.mgr, "vm-2").await.is_none(), "{state}");
            assert!(!exists(&f.child_dir), "{state}");
            assert!(f.parent_api.as_ref().unwrap().requests().is_empty(), "{state}: {:?}", f.parent_api.as_ref().unwrap().requests());
            assert!(lock_std(&f.mgr.busy).is_empty(), "{state}");
        }
    }

    #[tokio::test]
    async fn test_a_fork_without_a_free_slot_never_pauses_its_parent() {
        let mut f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        let (_rules, _) = netted(&mut f);
        {
            let mut g = f.mgr.inner.lock().await;
            let a = g.allocator.as_mut().unwrap();
            while a.claim().is_some() {}
        }
        assert_eq!(f.mgr.fork("vm-1", "vm-2", "c").await, Err("IpPoolExhausted".to_string()));
        assert!(f.parent_api.as_ref().unwrap().requests().is_empty(), "{:?}", f.parent_api.as_ref().unwrap().requests());
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        assert!(!exists(&f.child_dir));
    }

    #[tokio::test]
    async fn test_a_fork_is_refused_while_start_up_could_not_settle_the_ingress_chain() {
        // FLU-8 B: the kernel may still hold the previous run's node-port
        // rules for addresses no sandbox holds now; none is handed out.
        let mut f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        let (rules, free) = netted(&mut f);
        f.mgr.inner.lock().await.slots_withheld = true;
        assert_eq!(f.mgr.fork("vm-1", "vm-2", "c").await, Err("IsolationFailed".to_string()));
        assert!(f.parent_api.as_ref().unwrap().requests().is_empty(), "{:?}", f.parent_api.as_ref().unwrap().requests());
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        assert_eq!(rules.free_slots(), free, "no slot was taken");
        // A successful ingress rebuild ends it.
        assert!(f.mgr.refresh_ingress().await);
        assert!(!f.mgr.inner.lock().await.slots_withheld);
    }

    #[tokio::test]
    async fn test_every_failure_of_a_fork_gives_back_the_slot_and_the_tap() {
        for case in ["tap", "load", "socket", "rename", "child resume"] {
            let parent = if case == "rename" { Behavior { writes_nothing: true, ..Default::default() } } else { Behavior::default() };
            let child = match case {
                "load" => Behavior { fail: vec!["PUT /snapshot/load"], ..Default::default() },
                "child resume" => Behavior { fail_body: vec!["Resumed"], ..Default::default() },
                _ => Behavior::default(),
            };
            // A Firecracker that never makes its socket.
            let bin = if case == "socket" { "/bin/true".to_string() } else { fake_fc_bin() };
            let mut f = forking(VmState::Running, parent, child, &bin).await;
            let (rules, free) = netted(&mut f);
            if case == "tap" {
                rules.fail_pin.store(true, std::sync::atomic::Ordering::SeqCst);
            }
            let r = f.mgr.fork("vm-1", "vm-2", "c").await;
            assert!(r.is_err(), "{case}: {r:?}");
            gave_back(&rules, &free, case);
            match case {
                // Before the child had a VMM: nothing of it is left.
                "rename" => {
                    assert!(record_of(&f.mgr, "vm-2").await.is_none(), "{case}");
                    assert!(!exists(&f.child_dir), "{case}");
                }
                _ => {
                    assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Error, None)), "{case}");
                    let ip = f.mgr.inner.lock().await.vms.iter().find(|v| v.id == "vm-2").unwrap().ip.clone();
                    assert_eq!(ip, None, "{case}: the error record holds no address");
                    for name in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                        assert!(!exists(&format!("{}/{}", f.child_dir, name)), "{case}: {name}");
                    }
                }
            }
            // The parent runs on.
            assert_eq!(f.parent_api.as_ref().unwrap().state(), "Running", "{case}");
            assert!(lock_std(&f.mgr.busy).is_empty(), "{case}");
        }
    }

    #[tokio::test]
    async fn test_a_fork_whose_child_record_cannot_be_written_leaves_an_error_record_and_no_vmm() {
        let f = forking(
            VmState::Running,
            Behavior::default(),
            Behavior { delay: Some(("PATCH /drives/rootfs", Duration::from_millis(300))), ..Default::default() },
            &fake_fc_bin(),
        )
        .await;
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        while !f.child_api.requests().iter().any(|l| l.starts_with("PATCH /drives/rootfs")) {
            sleep(Duration::from_millis(5)).await;
        }
        break_meta_writes(&f.child_dir);
        assert!(forked.await.unwrap().is_err());
        let pid = spawned_pid(&f.child_dir).await;
        assert!(gone_soon(pid).await);
        assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Error, None)));
        assert!(!f.child_api.bodies().iter().any(|b| b.contains("Resumed")), "never resumed");
        assert!(!exists(&format!("{}/mem.bin", f.child_dir)));
    }

    #[tokio::test]
    async fn test_a_fork_whose_child_is_stopped_before_its_commit_leaves_nothing() {
        let mut f = forking(
            VmState::Running,
            Behavior::default(),
            Behavior { delay: Some(("GET /vm/config", Duration::from_millis(600))), ..Default::default() },
            &fake_fc_bin(),
        )
        .await;
        let (rules, free) = netted(&mut f);
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        while !f.child_api.requests().iter().any(|l| l.starts_with("GET /vm/config")) {
            sleep(Duration::from_millis(5)).await;
        }
        let pid = spawned_pid(&f.child_dir).await;
        f.mgr.stop("vm-2").await.unwrap();
        let r = forked.await.unwrap();
        assert!(matches!(&r, Err(e) if e.starts_with("Aborted")), "{r:?}");
        assert!(gone_soon(pid).await);
        assert!(record_of(&f.mgr, "vm-2").await.is_none(), "the reservation was the fork's: it goes");
        assert!(!exists(&f.child_dir));
        gave_back(&rules, &free, "stopped before the commit");
        assert!(lock_std(&f.mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_a_failed_sleep_gives_the_guest_back_before_it_deals_with_mem_bin() {
        // What the snapshot left, at the moment the resume reaches the VMM.
        for (watch, want) in [("vmstate.bin.tmp", ""), ("mem.bin", "half the memory")] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            disk_image(&dir);
            std::fs::write(format!("{}/vmstate.bin.tmp", dir), b"device state").unwrap();
            std::fs::write(format!("{}/mem.bin", dir), b"half the memory").unwrap();
            let api = FakeApi::serve_with(
                &format!("{}/fc.sock", dir),
                "Running",
                Behavior { fail: vec!["PUT /snapshot/create"], watch_meta: Some(format!("{}/{}", dir, watch)), ..Default::default() },
            );
            let mgr = mgr_for(data_dir.clone());
            mgr.inner.lock().await.vms.push(running_vm("vm-1", 4000));
            assert!(mgr.sleep_vm("vm-1").await.is_err());
            let seen = api.meta_seen();
            let resume = seen.iter().rposition(|(l, _)| l.starts_with("PATCH /vm")).unwrap();
            assert_eq!(seen[resume].1, want, "{watch} at the resume");
            assert!(!exists(&format!("{}/mem.bin", dir)), "and mem.bin goes after it");
        }
    }

    #[tokio::test]
    async fn test_a_parent_whose_vmm_dies_during_a_fork_loses_its_snapshot_files_too() {
        // Its VMM dies while it writes the child's snapshot. The fork fails;
        // the record of the parent is settled by the next sweep, and with it
        // a mem.bin from an earlier wake (S8).
        let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        drop(f.parent_api);
        let _ = std::fs::remove_file(format!("{}/fc.sock", f.parent_dir));
        let vmm_pid = f.vmm.as_ref().unwrap().pid();
        let api = FakeApi::serve_with(
            &format!("{}/fc.sock", f.parent_dir),
            "Running",
            Behavior { hang: Some(("PUT /snapshot/create", Some(vmm_pid))), ..Default::default() },
        );
        std::fs::write(format!("{}/mem.bin", f.parent_dir), b"woken from").unwrap();
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
            sleep(Duration::from_millis(5)).await;
        }
        drop(f.vmm);
        assert!(forked.await.unwrap().is_err());
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        f.mgr.sweep_dead().await;
        assert_eq!(record_of(&f.mgr, "vm-1").await, Some((VmState::Error, None)));
        assert!(!exists(&format!("{}/mem.bin", f.parent_dir)));
    }

    #[tokio::test]
    async fn test_a_sleep_that_outlives_a_stop_removes_its_own_snapshot() {
        // The stop gives up (the create is answered only after its limit),
        // so the files the late create wrote are the sleep's alone to remove.
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let vmm = FakeVmm::spawn(&data_dir, "vm-1");
        let api = FakeApi::serve_with(
            &format!("{}/fc.sock", dir),
            "Running",
            Behavior { write_snapshot: true, delay: Some(("PUT /snapshot/create", CLOSE_WAIT + Duration::from_millis(800))), ..Default::default() },
        );
        let mgr = mgr_for(data_dir.clone());
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        let sleeping = {
            let m = Arc::clone(&mgr);
            tokio::spawn(async move { m.sleep_vm("vm-1").await })
        };
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
            sleep(Duration::from_millis(5)).await;
        }
        assert_eq!(mgr.stop("vm-1").await, Err(OpError::invalid(&VmState::Stopped, STOP_GAVE_UP)));
        let r = sleeping.await.unwrap();
        assert!(matches!(&r, Err(OpError::Other(m)) if m.starts_with("Aborted")), "{r:?}");
        for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
            assert!(!exists(&format!("{}/{}", dir, f)), "{f} beside a stopped record");
        }
        drop(vmm);
    }

    // -- review fixes (FLU-72) --

    /// An image beside the test's instances, as a sandbox forked before this
    /// release runs on (its own parent's).
    fn foreign_image(f: &Forking) -> String {
        let image = format!("{}/grandparent-rootfs.ext4", f.data_dir);
        std::fs::write(&image, b"the grandparent's disk").unwrap();
        image
    }

    #[tokio::test]
    async fn test_a_parent_that_runs_on_another_image_is_refused_before_its_pause() {
        let image = std::env::temp_dir().join(format!("felucca-r5-foreign-{}.ext4", std::process::id()));
        std::fs::write(&image, b"x").unwrap();
        let image = image.to_str().unwrap().to_string();
        let f = forking(VmState::Running, Behavior { drive: Some(image.clone()), ..Default::default() }, Behavior::default(), &fake_fc_bin()).await;
        let r = f.mgr.fork_answer("vm-1", "vm-2", "c").await;
        let _ = std::fs::remove_file(&image);
        // Changed on purpose (fix list item 6): the answer reaches the tenant
        // word for word, so it names no path; the log has the image.
        assert_eq!(
            r,
            Err(OpError::InvalidState { msg: FORK_REFUSED_FORKED_BEFORE.into(), state: VmState::Running })
        );
        let api = f.parent_api.as_ref().unwrap();
        assert!(!api.requests().iter().any(|l| l.starts_with("PATCH /vm") || l.starts_with("PUT /snapshot")), "{:?}", api.requests());
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        assert!(!exists(&f.child_dir));
        assert!(lock_std(&f.mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_a_child_whose_snapshot_names_another_image_than_its_parents_is_never_started() {
        for state in [VmState::Running, VmState::Paused, VmState::Sleeping] {
            // The child's loaded state names an image that is not the
            // parent's own rootfs.ext4.
            let mut f = forking(state.clone(), Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
            let image = foreign_image(&f);
            drop(std::mem::replace(
                &mut f.child_api,
                {
                    let _ = std::fs::remove_file(format!("{}.fake.sock", f.child_dir));
                    FakeApi::serve_with(&format!("{}.fake.sock", f.child_dir), "Not started", Behavior { drive: Some(image.clone()), ..Default::default() })
                },
            ));
            let r = f.mgr.fork_answer("vm-1", "vm-2", "c").await;
            // Changed on purpose (items 6, 7): a 409 refusal with the
            // parent's state for every parent state — the sleeping parent's
            // used to be a 500 — and no path in it.
            assert_eq!(
                r,
                Err(OpError::InvalidState { msg: FORK_REFUSED_FORKED_BEFORE.into(), state: state.clone() }),
                "{state}"
            );
            let lines = f.child_api.requests();
            assert!(!lines.iter().any(|l| l.starts_with("PATCH")), "{state}: neither re-pointed nor resumed: {lines:?}");
            let pid = spawned_pid(&f.child_dir).await;
            assert!(gone_soon(pid).await, "{state}");
            assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Error, None)), "{state}");
            assert!(!exists(&format!("{}/mem.bin", f.child_dir)), "{state}");
        }
        // The same file under another spelling of its path is the same file.
        let mut f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        let spelled = format!("{}/./rootfs.ext4", f.parent_dir);
        drop(std::mem::replace(&mut f.child_api, {
            let _ = std::fs::remove_file(format!("{}.fake.sock", f.child_dir));
            FakeApi::serve_with(&format!("{}.fake.sock", f.child_dir), "Not started", Behavior { drive: Some(spelled), ..Default::default() })
        }));
        f.mgr.fork("vm-1", "vm-2", "c").await.unwrap();
        let pid = spawned_pid(&f.child_dir).await;
        f.mgr.stop("vm-2").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    #[tokio::test]
    async fn test_a_sleep_syncs_the_image_its_firecracker_reports() {
        for case in ["another image", "a missing image"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            // No rootfs.ext4 of its own: only the reported image can be synced.
            let image = format!("{}/parents-rootfs.ext4", data_dir);
            if case == "another image" {
                std::fs::write(&image, b"the parent's disk").unwrap();
            }
            let vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let api = FakeApi::serve_with(
                &format!("{}/fc.sock", dir),
                "Running",
                Behavior { write_snapshot: true, drive: Some(image.clone()), ..Default::default() },
            );
            let mgr = mgr_for(data_dir.clone());
            mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
            let r = mgr.sleep_vm("vm-1").await;
            let first = api.requests().first().cloned().unwrap_or_default();
            assert!(first.starts_with("GET /vm/config"), "{case}: asked before the pause: {:?}", api.requests());
            if case == "another image" {
                assert_eq!(r, Ok(()), "{case}");
                assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Sleeping, None)));
            } else {
                assert!(matches!(&r, Err(OpError::Other(m)) if m.contains(&image)), "{case}: {r:?}");
                assert_eq!(api.state(), "Running", "{case}: undone");
            }
            drop(vmm);
        }
    }

    #[tokio::test]
    async fn test_a_stop_of_the_parent_ends_the_copy_of_its_disk() {
        // A disk image that cannot be read to the end (a FIFO nobody
        // writes): the copy inside the pause never finishes by itself.
        let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        let rootfs = format!("{}/rootfs.ext4", f.parent_dir);
        std::fs::remove_file(&rootfs).unwrap();
        let c = std::ffi::CString::new(rootfs.clone()).unwrap();
        // SAFETY: a plain mkfifo(3) on a path we own.
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        let api = f.parent_api.as_ref().unwrap();
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
            sleep(Duration::from_millis(5)).await;
        }
        sleep(Duration::from_millis(100)).await;
        f.mgr.stop("vm-1").await.unwrap();
        let r = tokio::time::timeout(Duration::from_secs(10), forked).await.expect("the copy must be ended").unwrap();
        assert!(matches!(&r, Err(e) if e.contains("being stopped or deleted")), "{r:?}");
        assert!(record_of(&f.mgr, "vm-2").await.is_none());
        assert!(!exists(&f.child_dir));
        assert!(lock_std(&f.mgr.busy).is_empty());
    }

    // -- fork (FLU-70) --

    /// A parent `vm-1` in `state` with its disk image, under a manager whose
    /// Firecracker is the stand-in. A running or paused parent has a VMM and
    /// an API that writes the snapshot it is asked for (`parent` adds to
    /// that); a sleeping one has its snapshot and its record on disk. The
    /// child `vm-2`'s API is bound beside its (not yet existing) directory.
    struct Forking {
        _tmp: tempfile::TempDir,
        data_dir: String,
        parent_dir: String,
        child_dir: String,
        mgr: Arc<Manager>,
        vmm: Option<FakeVmm>,
        parent_api: Option<FakeApi>,
        child_api: FakeApi,
    }

    async fn forking(state: VmState, parent: Behavior, child: Behavior, fc_bin: &str) -> Forking {
        forking_with(state, |_, _| (parent, child), fc_bin).await
    }

    /// `forking`, with the parent's and the child's behaviours made from the
    /// parent's and the child's directories.
    async fn forking_with(
        state: VmState,
        behaviors: impl FnOnce(&str, &str) -> (Behavior, Behavior),
        fc_bin: &str,
    ) -> Forking {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let parent_dir = format!("{}/instances/vm-1", data_dir);
        let child_dir = format!("{}/instances/vm-2", data_dir);
        let (parent, child) = behaviors(&parent_dir, &child_dir);
        std::fs::create_dir_all(&parent_dir).unwrap();
        disk_image(&parent_dir);
        // A loaded snapshot names the parent's disk image (FLU-72).
        let child = Behavior { drive: child.drive.clone().or_else(|| Some(format!("{}/rootfs.ext4", parent_dir))), ..child };
        let child_api = FakeApi::serve_with(&format!("{}.fake.sock", child_dir), "Not started", child);
        let mgr = mgr_with_fc(data_dir.clone(), fc_bin);
        let (vmm, parent_api) = if state == VmState::Sleeping {
            snapshot_files(&parent_dir);
            std::fs::write(format!("{}/meta.json", parent_dir), sleeper_line("vm-1")).unwrap();
            mgr.inner.lock().await.vms.push(sleeping_vm("vm-1"));
            (None, None)
        } else {
            let vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let initial = if state == VmState::Paused { "Paused" } else { "Running" };
            let api = FakeApi::serve_with(
                &format!("{}/fc.sock", parent_dir),
                initial,
                Behavior { write_snapshot: !parent.writes_nothing, ..parent },
            );
            let mut v = running_vm("vm-1", vmm.pid());
            v.state = state;
            mgr.inner.lock().await.vms.push(v);
            (Some(vmm), Some(api))
        };
        Forking { _tmp: tmp, data_dir, parent_dir, child_dir, mgr, vmm, parent_api, child_api }
    }

    fn read(path: &str) -> String {
        std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{path}: {e}"))
    }

    #[tokio::test]
    async fn test_a_fork_onto_a_taken_id_writes_nothing() {
        // vm-2 is a sandbox; or a claimed pool VM lives in a directory vm-2.
        for owner in ["vm-2", "vm-claimed"] {
            let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
            std::fs::create_dir_all(&f.child_dir).unwrap();
            std::fs::write(format!("{}/rootfs.ext4", f.child_dir), b"their disk").unwrap();
            std::fs::write(format!("{}/mem.bin", f.child_dir), b"their memory").unwrap();
            std::fs::write(format!("{}/meta.json", f.child_dir), meta_line(owner, None, "stopped")).unwrap();
            let mut theirs = running_vm(owner, 0);
            theirs.state = VmState::Stopped;
            theirs.pid = None;
            theirs.dir_id = "vm-2".into();
            f.mgr.inner.lock().await.vms.push(theirs);

            assert_eq!(f.mgr.fork("vm-1", "vm-2", "c").await, Err("AlreadyExists".to_string()), "{owner}");
            assert_eq!(read(&format!("{}/rootfs.ext4", f.child_dir)), "their disk", "{owner}");
            assert_eq!(read(&format!("{}/mem.bin", f.child_dir)), "their memory", "{owner}");
            assert_eq!(read(&format!("{}/meta.json", f.child_dir)), meta_line(owner, None, "stopped"), "{owner}");
            assert!(!exists(&format!("{}/vmstate.bin", f.child_dir)), "{owner}");
            // Refused before the parent was even paused.
            assert!(f.parent_api.as_ref().unwrap().requests().is_empty(), "{owner}");
            assert_eq!(f.mgr.inner.lock().await.vms.len(), 2, "{owner}: no record was added");
            assert!(lock_std(&f.mgr.busy).is_empty(), "{owner}");
        }
    }

    #[tokio::test]
    async fn test_a_fork_into_a_directory_no_record_owns_leaves_it_alone() {
        let f = forking(VmState::Running, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        // A VM whose meta.json did not load at start-up.
        std::fs::create_dir_all(&f.child_dir).unwrap();
        std::fs::write(format!("{}/meta.json", f.child_dir), "{\"id\":\"vm-2").unwrap();
        std::fs::write(format!("{}/rootfs.ext4", f.child_dir), b"their disk").unwrap();

        assert_eq!(f.mgr.fork("vm-1", "vm-2", "c").await, Err("AlreadyExists".to_string()));
        assert_eq!(read(&format!("{}/meta.json", f.child_dir)), "{\"id\":\"vm-2");
        assert_eq!(read(&format!("{}/rootfs.ext4", f.child_dir)), "their disk");
        let names: Vec<_> = std::fs::read_dir(&f.child_dir).unwrap().map(|e| e.unwrap().file_name()).collect();
        assert_eq!(names.len(), 2, "nothing added: {names:?}");
        assert!(record_of(&f.mgr, "vm-2").await.is_none(), "the reserved record is gone again");
        assert!(f.parent_api.as_ref().unwrap().requests().is_empty());
        assert!(lock_std(&f.mgr.busy).is_empty());
    }

    #[tokio::test]
    async fn test_a_fork_writes_the_snapshot_into_the_childs_directory() {
        for state in [VmState::Running, VmState::Paused] {
            let f = forking(state.clone(), Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
            f.mgr.fork("vm-1", "vm-2", "c").await.unwrap_or_else(|e| panic!("{state}: {e}"));
            let child_pid = spawned_pid(&f.child_dir).await;
            let api = f.parent_api.as_ref().unwrap();

            let create = api.bodies().into_iter().find(|b| b.contains("snapshot_path")).expect("a create");
            let v: serde_json::Value = serde_json::from_str(&create).unwrap();
            assert_eq!(v["snapshot_path"], serde_json::json!(format!("{}/vmstate.bin.tmp", f.child_dir)), "{state}");
            assert_eq!(v["mem_file_path"], serde_json::json!(format!("{}/mem.bin", f.child_dir)), "{state}");
            for name in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                assert!(!exists(&format!("{}/{}", f.parent_dir, name)), "{state}: the parent's directory gets no {name}");
            }
            // Paused and resumed only by the fork that paused it.
            let pauses = api.bodies().iter().filter(|b| b.contains("\"Paused\"")).count();
            let resumes = api.bodies().iter().filter(|b| b.contains("Resumed")).count();
            let by_fork = usize::from(state == VmState::Running);
            assert_eq!((pauses, resumes), (by_fork, by_fork), "{state}: {:?}", api.requests());
            assert_eq!(record_of(&f.mgr, "vm-1").await.unwrap().0, state);

            // The child loaded its own files; its state file is gone after.
            let load = f.child_api.bodies().into_iter().find(|b| b.contains("mem_backend")).expect("a load");
            let v: serde_json::Value = serde_json::from_str(&load).unwrap();
            assert_eq!(v["snapshot_path"], serde_json::json!(format!("{}/vmstate.bin", f.child_dir)));
            assert_eq!(v["mem_backend"]["backend_path"], serde_json::json!(format!("{}/mem.bin", f.child_dir)));
            assert!(!exists(&format!("{}/vmstate.bin", f.child_dir)), "{state}");
            assert!(!exists(&format!("{}/vmstate.bin.tmp", f.child_dir)), "{state}");
            assert_eq!(read(&format!("{}/mem.bin", f.child_dir)), "guest memory");
            assert_eq!(read(&format!("{}/rootfs.ext4", f.child_dir)), "parent disk");
            assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Running, Some(child_pid))), "{state}");
            assert_eq!(meta_on_disk(&f.data_dir, "vm-2").state, VmState::Running);
            assert!(lock_std(&f.mgr.busy).is_empty());
            f.mgr.stop("vm-2").await.unwrap();
            assert!(gone_soon(child_pid).await);
        }
    }

    #[tokio::test]
    async fn test_a_fork_of_a_sleeping_parent_copies_its_snapshot_and_leaves_it_asleep() {
        let f = forking(VmState::Sleeping, Behavior::default(), Behavior::default(), &fake_fc_bin()).await;
        f.mgr.fork("vm-1", "vm-2", "c").await.unwrap();
        let child_pid = spawned_pid(&f.child_dir).await;

        assert_eq!(read(&format!("{}/vmstate.bin", f.parent_dir)), "device state");
        assert_eq!(read(&format!("{}/mem.bin", f.parent_dir)), "guest memory");
        assert_eq!(read(&format!("{}/rootfs.ext4", f.parent_dir)), "parent disk");
        assert_eq!(record_of(&f.mgr, "vm-1").await, Some((VmState::Sleeping, None)));
        assert!(marker_of(&f.mgr, "vm-1").await);
        let disk = meta_on_disk(&f.data_dir, "vm-1");
        assert_eq!((disk.state, disk.snapshot_valid), (VmState::Sleeping, true));

        assert!(!exists(&format!("{}/vmstate.bin", f.child_dir)), "the child's state file goes after its load");
        assert_eq!(read(&format!("{}/mem.bin", f.child_dir)), "guest memory");
        assert_eq!(read(&format!("{}/rootfs.ext4", f.child_dir)), "parent disk");
        assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Running, Some(child_pid))));
        assert!(lock_std(&f.mgr.busy).is_empty());
        f.mgr.stop("vm-2").await.unwrap();
        assert!(gone_soon(child_pid).await);
    }

    #[tokio::test]
    async fn test_every_failure_of_a_fork_ends_as_specified() {
        let cases = [
            "pause", "create", "create paused", "parent resume", "copy", "sleeping copy", "spawn", "load",
        ];
        for case in cases {
            let state = match case {
                "create paused" => VmState::Paused,
                "sleeping copy" => VmState::Sleeping,
                _ => VmState::Running,
            };
            let parent = match case {
                "pause" => Behavior { fail: vec!["PATCH /vm"], ..Default::default() },
                "create" | "create paused" => Behavior { fail: vec!["PUT /snapshot/create"], ..Default::default() },
                "parent resume" => Behavior { fail_body: vec!["Resumed"], ..Default::default() },
                _ => Behavior::default(),
            };
            let child = if case == "load" { Behavior { fail: vec!["PUT /snapshot/load"], ..Default::default() } } else { Behavior::default() };
            let bin = if case == "spawn" { "/nonexistent/firecracker".to_string() } else { fake_fc_bin() };
            let f = forking(state.clone(), parent, child, &bin).await;
            match case {
                "copy" => std::fs::remove_file(format!("{}/rootfs.ext4", f.parent_dir)).unwrap(),
                "sleeping copy" => std::fs::remove_file(format!("{}/mem.bin", f.parent_dir)).unwrap(),
                _ => {}
            }

            let r = f.mgr.fork("vm-1", "vm-2", "c").await;
            assert!(r.is_err(), "{case}: {r:?}");
            assert!(lock_std(&f.mgr.busy).is_empty(), "{case}: the mark must be released");
            match case {
                "spawn" | "load" => {
                    // The child had (or was about to get) a VMM: it stays, as
                    // `error`, without snapshot files and without a VMM.
                    assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Error, None)), "{case}");
                    for name in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                        assert!(!exists(&format!("{}/{}", f.child_dir, name)), "{case}: {name}");
                    }
                    if case == "load" {
                        let pid = spawned_pid(&f.child_dir).await;
                        assert!(gone_soon(pid).await, "{case}: the child's VMM must be gone");
                    }
                }
                _ => {
                    // Before the child had a VMM: nothing of it is left.
                    assert!(record_of(&f.mgr, "vm-2").await.is_none(), "{case}");
                    assert!(!exists(&f.child_dir), "{case}: the child's directory must be gone");
                }
            }
            // The parent: given back as it was, or `paused` when that failed.
            match case {
                "sleeping copy" => {
                    assert_eq!(record_of(&f.mgr, "vm-1").await, Some((VmState::Sleeping, None)), "{case}");
                    assert!(exists(&format!("{}/vmstate.bin", f.parent_dir)), "{case}");
                }
                "parent resume" => {
                    let pid = f.vmm.as_ref().unwrap().pid();
                    assert_eq!(record_of(&f.mgr, "vm-1").await, Some((VmState::Paused, Some(pid))), "{case}");
                    assert_eq!(meta_on_disk(&f.data_dir, "vm-1").state, VmState::Paused, "{case}");
                }
                _ => {
                    let api = f.parent_api.as_ref().unwrap();
                    assert_eq!(record_of(&f.mgr, "vm-1").await.unwrap().0, state, "{case}");
                    let want = if state == VmState::Paused { "Paused" } else { "Running" };
                    assert_eq!(api.state(), want, "{case}: {:?}", api.requests());
                    for name in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                        assert!(!exists(&format!("{}/{}", f.parent_dir, name)), "{case}: {name}");
                    }
                }
            }
        }
    }

    #[tokio::test]
    async fn test_a_fork_holds_the_parent_until_the_childs_drive_is_its_own() {
        let f = forking(
            VmState::Running,
            Behavior { delay: Some(("PUT /snapshot/create", Duration::from_millis(300))), ..Default::default() },
            Behavior { delay: Some(("PUT /snapshot/load", Duration::from_millis(300))), ..Default::default() },
            &fake_fc_bin(),
        )
        .await;
        let forked = {
            let m = Arc::clone(&f.mgr);
            tokio::spawn(async move { m.fork("vm-1", "vm-2", "c").await })
        };
        let api = f.parent_api.as_ref().unwrap();
        while !api.requests().iter().any(|l| l.starts_with("PUT /snapshot/create")) {
            sleep(Duration::from_millis(5)).await;
        }
        assert!(f.mgr.op_in_progress("vm-1"), "held while its snapshot is written");
        assert_eq!(f.mgr.sleep_vm("vm-1").await, Err(OpError::Busy));
        while !f.child_api.requests().iter().any(|l| l.starts_with("PUT /snapshot/load")) {
            sleep(Duration::from_millis(5)).await;
        }
        // FLU-72 E4 changed this on purpose: the parent used to be let go
        // once the child's files were complete, but the child's load still
        // reads the parent's image until its drive is re-pointed.
        assert!(f.mgr.op_in_progress("vm-1"), "held until the child's drive is its own");
        forked.await.unwrap().unwrap();
        assert!(!f.mgr.op_in_progress("vm-1"));
        assert!(lock_std(&f.mgr.busy).is_empty());
        let child_pid = spawned_pid(&f.child_dir).await;
        f.mgr.stop("vm-2").await.unwrap();
        assert!(gone_soon(child_pid).await);
    }

    // -- fork: the child's own disk (FLU-72) --

    #[tokio::test]
    async fn test_a_fork_copies_the_disk_while_the_parent_is_paused() {
        // `watch_meta` reads any file: here the child's disk image, at every
        // request the parent's API receives.
        let f = forking_with(
            VmState::Running,
            |_, child_dir| {
                let watch = Behavior { watch_meta: Some(format!("{}/rootfs.ext4", child_dir)), ..Default::default() };
                (watch, Behavior::default())
            },
            &fake_fc_bin(),
        )
        .await;
        f.mgr.fork("vm-1", "vm-2", "c").await.unwrap();
        let api = f.parent_api.as_ref().unwrap();
        let seen = api.meta_seen();
        let at = |what: &str| {
            seen.iter()
                .position(|(l, _)| l.starts_with(what))
                .unwrap_or_else(|| panic!("no {what}: {:?}", api.requests()))
        };
        let (pause, create) = (at("PATCH /vm"), at("PUT /snapshot/create"));
        let resume = seen.iter().rposition(|(l, _)| l.starts_with("PATCH /vm")).unwrap();
        assert!(pause < create && create < resume, "{:?}", api.requests());
        assert!(api.bodies()[pause].contains("Paused") && api.bodies()[resume].contains("Resumed"));
        assert_eq!(seen[create].1, "", "not copied before the snapshot");
        assert_eq!(seen[resume].1, "parent disk", "the child's disk must be there when the parent's resume arrives");
        let child_pid = spawned_pid(&f.child_dir).await;
        f.mgr.stop("vm-2").await.unwrap();
        assert!(gone_soon(child_pid).await);
    }

    #[tokio::test]
    async fn test_a_fork_child_gets_its_own_disk_before_it_runs() {
        for state in [VmState::Running, VmState::Sleeping] {
            let f = forking_with(
                state.clone(),
                |_, child_dir| {
                    let watch = Behavior { watch_meta: Some(format!("{}/meta.json", child_dir)), ..Default::default() };
                    (Behavior::default(), watch)
                },
                &fake_fc_bin(),
            )
            .await;
            f.mgr.fork("vm-1", "vm-2", "c").await.unwrap_or_else(|e| panic!("{state}: {e}"));
            let child_pid = spawned_pid(&f.child_dir).await;
            let child_rootfs = format!("{}/rootfs.ext4", f.child_dir);

            // Exactly this, in this order.
            let lines = f.child_api.requests();
            let kinds: Vec<&str> = lines.iter().map(|l| l.rsplit_once(' ').map(|(a, _)| a).unwrap_or(l)).collect();
            assert_eq!(
                kinds,
                // FLU-72 E1 added the first GET on purpose: which image the
                // loaded state names, before the drive is re-pointed.
                ["PUT /snapshot/load", "GET /vm/config", "PATCH /drives/rootfs", "GET /vm/config", "PATCH /vm"],
                "{state}: {lines:?}"
            );
            let bodies = f.child_api.bodies();
            assert!(bodies[0].contains("\"resume_vm\":false"), "{state}: loaded paused: {}", bodies[0]);
            assert_eq!(bodies[2], fc::build_drive_rootfs_patch(&child_rootfs), "{state}");
            assert!(bodies[4].contains("Resumed"), "{state}");
            // When the resume reached the child, its record said `running`
            // with its VMM, on disk.
            let (_, at_resume) = f.child_api.meta_seen().into_iter().find(|(l, _)| l.starts_with("PATCH /vm")).unwrap();
            let at_resume: Meta = serde_json::from_str(&at_resume).expect("meta.json at the resume");
            assert_eq!((at_resume.state, at_resume.pid), (VmState::Running, Some(child_pid)), "{state}");
            assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Running, Some(child_pid))), "{state}");
            f.mgr.stop("vm-2").await.unwrap();
            assert!(gone_soon(child_pid).await);
        }
    }

    #[tokio::test]
    async fn test_a_fork_child_whose_start_fails_is_never_resumed_on_any_disk() {
        for case in ["load", "drive patch", "drive check", "child resume"] {
            let child = match case {
                "load" => Behavior { fail: vec!["PUT /snapshot/load"], ..Default::default() },
                "drive patch" => Behavior { fail: vec!["PATCH /drives/rootfs"], ..Default::default() },
                // The PATCH is answered, but the drive still names the
                // parent's image.
                "drive check" => Behavior { keep_drive: true, ..Default::default() },
                _ => Behavior { fail_body: vec!["Resumed"], ..Default::default() },
            };
            let f = forking(VmState::Running, Behavior::default(), child, &fake_fc_bin()).await;

            let r = f.mgr.fork("vm-1", "vm-2", "c").await;
            assert!(r.is_err(), "{case}: {r:?}");
            if case == "drive check" {
                let e = r.unwrap_err();
                assert!(e.contains(&format!("{}/rootfs.ext4", f.parent_dir)), "{case}: names the foreign drive: {e}");
            }
            // Never resumed — refused, in the last case.
            let resumes = f.child_api.bodies().iter().filter(|b| b.contains("Resumed")).count();
            assert_eq!(resumes, usize::from(case == "child resume"), "{case}: {:?}", f.child_api.requests());
            let pid = spawned_pid(&f.child_dir).await;
            assert!(gone_soon(pid).await, "{case}: the child's VMM must be gone");
            assert_eq!(record_of(&f.mgr, "vm-2").await, Some((VmState::Error, None)), "{case}");
            for name in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                assert!(!exists(&format!("{}/{}", f.child_dir, name)), "{case}: {name}");
            }
            if case == "child resume" {
                // Its record had been written as `running`: it is written again.
                let disk = meta_on_disk(&f.data_dir, "vm-2");
                assert_eq!((disk.state, disk.pid), (VmState::Error, None), "{case}");
            }
            // The parent is not affected.
            assert_eq!(f.parent_api.as_ref().unwrap().state(), "Running", "{case}");
            assert_eq!(record_of(&f.mgr, "vm-1").await.unwrap().0, VmState::Running, "{case}");
            assert!(lock_std(&f.mgr.busy).is_empty(), "{case}");
        }
    }

    // -- clean-up of snapshot files --

    #[tokio::test]
    async fn test_stop_discards_the_snapshot_from_running_and_from_sleeping() {
        for from_sleep in [false, true] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            snapshot_files(&dir);
            std::fs::write(format!("{}/vmstate.bin.tmp", dir), b"x").unwrap();
            let mut vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let mgr = mgr_for(data_dir.clone());
            let v = if from_sleep { sleeping_vm("vm-1") } else { running_vm("vm-1", vmm.pid()) };
            mgr.inner.lock().await.vms.push(v);

            mgr.stop("vm-1").await.unwrap();
            for f in ["vmstate.bin", "vmstate.bin.tmp", "mem.bin"] {
                assert!(!exists(&format!("{}/{}", dir, f)), "from_sleep={from_sleep}: {f}");
            }
            let disk = meta_on_disk(&data_dir, "vm-1");
            assert_eq!((disk.state, disk.snapshot_valid), (VmState::Stopped, false));
            assert_eq!(!vmm.alive(), !from_sleep);
        }
    }

    #[tokio::test]
    async fn test_start_discards_an_old_snapshot_before_the_cold_boot() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        snapshot_files(&dir);
        let _api = FakeApi::serve(&format!("{}/fc.sock.fake", dir), "Not started");
        let mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
        let mut v = running_vm("vm-1", 0);
        v.state = VmState::Stopped;
        v.pid = None;
        mgr.inner.lock().await.vms.push(v);

        mgr.start("vm-1").await.unwrap();
        let pid = spawned_pid(&dir).await;
        assert!(!exists(&format!("{}/vmstate.bin", dir)) && !exists(&format!("{}/mem.bin", dir)));
        assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Running, Some(pid))));
        mgr.stop("vm-1").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    // ---- FLU-8 r7 W19: the lifecycle with networking ON, through the rule fake ----

    /// The pin of `slot` in the fake kernel: its chain's rules, and whether
    /// the tap is up.
    fn pin_in(rules: &super::rule_fake::FakeRules, slot: u32) -> (Vec<String>, Option<bool>) {
        let tap = net::tap_name(slot);
        let chain = format!("as_{}", tap.replace('-', "_"));
        let r = rules.k().chain_mut("netdev", "felucca", &chain).map(|c| c.rules.clone()).unwrap_or_default();
        (r, rules.link_up(&tap))
    }

    #[tokio::test]
    async fn test_start_pins_the_tap_with_networking_on() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        let _api = FakeApi::serve(&format!("{}/fc.sock.fake", dir), "Not started");
        let mut mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
        let rules = super::rule_fake::install(&mut mgr);
        let mut v = running_vm("vm-1", 0);
        v.state = VmState::Stopped;
        v.pid = None;
        v.slot = Some(3);
        v.ip = Some("10.231.0.5".into());
        {
            let mut g = mgr.inner.lock().await;
            Manager::reserve_slot(&mut g, 3);
            g.vms.push(v);
        }
        mgr.start("vm-1").await.unwrap();
        let pid = spawned_pid(&dir).await;
        assert!(rules.calls().iter().any(|c| matches!(c, super::rule_fake::RuleCall::PinTap { tap } if tap == "hth-3")));
        let (chain, up) = pin_in(&rules, 3);
        assert!(chain.contains(&"ip saddr 10.231.0.5 accept".to_string()), "{chain:?}");
        assert!(chain.contains(&format!("ether saddr != {} drop", net::guest_mac_for_slot(3))), "{chain:?}");
        assert_eq!(up, Some(true), "pinned, then raised");
        mgr.stop("vm-1").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    #[tokio::test]
    async fn test_wake_pins_the_tap_with_networking_on() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        disk_image(&dir);
        snapshot_files(&dir);
        std::fs::write(format!("{}/meta.json", dir), sleeper_line("vm-1")).unwrap();
        let _api = FakeApi::serve(&format!("{}/fc.sock.fake", dir), "Not started");
        let mut mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
        let rules = super::rule_fake::install(&mut mgr);
        let mut v = sleeping_vm("vm-1");
        v.snapshot_valid = true;
        v.slot = Some(4);
        v.ip = Some("10.231.0.6".into());
        {
            let mut g = mgr.inner.lock().await;
            Manager::reserve_slot(&mut g, 4);
            g.vms.push(v);
        }
        mgr.wake_vm("vm-1").await.unwrap();
        let pid = spawned_pid(&dir).await;
        let (chain, up) = pin_in(&rules, 4);
        assert!(chain.contains(&"ip saddr 10.231.0.6 accept".to_string()), "{chain:?}");
        assert_eq!(up, Some(true));
        mgr.stop("vm-1").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    #[tokio::test]
    async fn test_a_create_on_a_slot_a_delete_just_freed_gets_its_own_tap_and_pin() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        std::fs::create_dir_all(format!("{}/images", data_dir)).unwrap();
        std::fs::write(format!("{}/images/{}.ext4", data_dir, DEFAULT_IMAGE), b"image").unwrap();
        let mut mgr = mgr_with_fc(data_dir.clone(), &fake_fc_bin());
        let rules = super::rule_fake::install(&mut mgr);
        // The sandbox that held slot 0, with its tap and pin, deleted.
        let old_ip = super::rule_fake::add_vm(&mgr, "vm-old", 0, "t1").await;
        assert!(rules.pin_tap("hth-0", Some(&old_ip), &net::guest_mac_for_slot(0)));
        mgr.delete("vm-old").await.unwrap();
        assert!(!rules.k().links.contains_key("hth-0"));
        assert!(rules.free_slots().contains(&0));
        // A create right after claims it, and pins its own tap.
        let _api = FakeApi::serve(&format!("{}/instances/vm-new.fake.sock", data_dir), "Not started");
        let spec = CreateSpec {
            id: "vm-new".into(),
            name: "vm-new".into(),
            vcpus: 1,
            mem_mib: 256,
            tenant_id: Some("t2".into()),
            image: DEFAULT_IMAGE.into(),
            image_sha256: None,
            disk_gb: 0,
        };
        mgr.create(&spec).await.unwrap();
        let pid = spawned_pid(&format!("{}/instances/vm-new", data_dir)).await;
        assert_eq!(mgr.inner.lock().await.vms.iter().find(|v| v.id == "vm-new").unwrap().slot, Some(0));
        let (chain, up) = pin_in(&rules, 0);
        assert!(chain.contains(&format!("ip saddr {} accept", old_ip)), "{chain:?}");
        assert_eq!(up, Some(true));
        assert!(!rules.free_slots().contains(&0));
        mgr.delete("vm-new").await.unwrap();
        assert!(gone_soon(pid).await);
    }

    #[test]
    fn test_an_adopted_tap_is_pinned_before_it_is_raised() {
        // W8: `ensure_pinned_tap` brought the tap up first and pinned it
        // after; a guest's port found down came up before its pin was back.
        let src = include_str!("mod.rs");
        let prod = src.split("#[cfg(test)]").next().unwrap();
        let start = prod.find("fn ensure_pinned_tap(").unwrap();
        let end = start + prod[start..].find("\n}\n").unwrap();
        let body = &prod[start..end];
        let pin = body.find("net::ensure_tap_antispoof(").expect("the pin");
        let raise = body.find("net::raise_tap(").expect("the raise");
        assert!(pin < raise, "raised before pinned");
        let net_src = include_str!("../net.rs");
        let et = net_src.find("pub fn ensure_tap(").unwrap();
        let et_end = et + net_src[et..].find("\n}\n").unwrap();
        assert!(!net_src[et..et_end].contains("\"up\""), "ensure_tap must not bring the tap up");
    }

    #[tokio::test]
    async fn test_a_vmm_that_dies_takes_its_snapshot_files_unless_an_operation_holds_them() {
        for held in [false, true] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let dir = format!("{}/instances/vm-1", data_dir);
            std::fs::create_dir_all(&dir).unwrap();
            snapshot_files(&dir);
            let vmm = FakeVmm::spawn(&data_dir, "vm-1");
            let mgr = mgr_for(data_dir.clone());
            mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
            let op = if held { Some(mgr.begin_op("vm-1", OpKind::Sleep).await.unwrap()) } else { None };

            let pid = vmm.pid();
            drop(vmm); // the Firecracker dies
            mgr.sweep_dead().await;
            // FLU-4 S7' changed this on purpose: while an operation holds the
            // VM, the record is that operation's to settle too (it used to go
            // to `error` here, with the files left).
            let want = if held { (VmState::Running, Some(pid)) } else { (VmState::Error, None) };
            assert_eq!(record_of(&mgr, "vm-1").await, Some(want), "held={held}");
            assert!(!marker_of(&mgr, "vm-1").await);
            let left = exists(&format!("{}/vmstate.bin", dir)) && exists(&format!("{}/mem.bin", dir));
            assert_eq!(left, held, "held={held}");
            drop(op);
            // Whatever the operation left, the next sweep settles.
            mgr.sweep_dead().await;
            assert_eq!(record_of(&mgr, "vm-1").await, Some((VmState::Error, None)), "held={held}");
            assert!(!exists(&format!("{}/vmstate.bin", dir)) && !exists(&format!("{}/mem.bin", dir)), "held={held}");
        }
    }

    #[tokio::test]
    async fn test_mem_bin_is_kept_while_a_live_vmm_of_the_instance_maps_it() {
        use std::os::unix::fs::MetadataExt;
        let tmp = tempfile::tempdir().unwrap();
        let base = std::fs::canonicalize(tmp.path()).unwrap();
        let data_dir = base.join("data").to_str().unwrap().to_string();
        let proc_root = base.join("proc");
        let dir = format!("{}/instances/vm-1", data_dir);
        std::fs::create_dir_all(&dir).unwrap();
        let p = proc_root.join("4000");
        std::fs::create_dir_all(&p).unwrap();
        std::fs::write(p.join("cmdline"), format!("firecracker\0--api-sock\0{}/fc.sock\0", dir)).unwrap();
        let mgr = Manager::with_proc_root(
            data_dir.clone(),
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
            proc_root,
        );
        let mem = format!("{}/mem.bin", dir);
        for (maps, kept) in [("mapped", true), ("other", false), ("unreadable", true)] {
            snapshot_files(&dir);
            let st = std::fs::metadata(&mem).unwrap();
            let (ma, mi) = dev_major_minor(st.dev());
            let _ = std::fs::remove_dir_all(p.join("maps"));
            let _ = std::fs::remove_file(p.join("maps"));
            match maps {
                "mapped" => std::fs::write(
                    p.join("maps"),
                    format!("7f0000000000-7f0010000000 rw-p 00000000 {:02x}:{:02x} {} {}\n", ma, mi, st.ino(), mem),
                )
                .unwrap(),
                "other" => std::fs::write(
                    p.join("maps"),
                    format!("7f0000000000-7f0010000000 rw-p 00000000 {:02x}:{:02x} {} /elsewhere\n", ma, mi, st.ino() + 1),
                )
                .unwrap(),
                _ => std::fs::create_dir_all(p.join("maps")).unwrap(), // EISDIR: cannot tell
            }
            mgr.discard_snapshot_files("vm-1", "vm-1");
            assert!(!exists(&format!("{}/vmstate.bin", dir)), "{maps}");
            assert_eq!(exists(&mem), kept, "{maps}");
        }
    }

    #[test]
    fn test_maps_lines_are_read_by_device_and_inode() {
        let maps = "\
55d0c0a00000-55d0c0b00000 r-xp 00000000 fd:01 1048577                    /usr/bin/firecracker
7f1000000000-7f1100000000 rw-p 00000000 fd:01 2097153                    /srv/felucca/instances/sb 1/mem.bin (deleted)
7f2000000000-7f2100000000 rw-p 00000000 103:02 12345                     /srv/my data/instances/sb-2/mem.bin
7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0                          [stack]
";
        // A path with spaces, a "(deleted)" suffix: only device and inode count.
        assert_eq!(maps_contain(maps, 0xfd, 0x01, 2_097_153), Some(true));
        assert_eq!(maps_contain(maps, 0x103, 0x02, 12345), Some(true));
        // Same inode on another device, or another inode: not this file.
        assert_eq!(maps_contain(maps, 0xfd, 0x02, 2_097_153), Some(false));
        assert_eq!(maps_contain(maps, 0xfd, 0x01, 2_097_154), Some(false));
        assert_eq!(maps_contain("", 0xfd, 0x01, 1), Some(false));
        // A line that does not parse: cannot tell.
        assert_eq!(maps_contain("garbage\n", 0xfd, 0x01, 1), None);
        assert_eq!(maps_contain("a-b rw-p 0 zz:01 5 /x\n", 0xfd, 0x01, 5), None);
        // st_dev as glibc encodes it.
        let makedev = |major: u64, minor: u64| {
            ((major & 0xffff_f000) << 32) | ((major & 0x0fff) << 8) | ((minor & 0xffff_ff00) << 12) | (minor & 0x00ff)
        };
        assert_eq!(dev_major_minor(0xfd01), (0xfd, 0x01));
        for (major, minor) in [(0xfd, 0x01), (0x103, 0x12345), (0x1_2345, 0xab)] {
            assert_eq!(dev_major_minor(makedev(major, minor)), (major, minor));
        }
    }
}

/// Unit tests: a stand-in for `HostRules`, so the rule rebuilds and delete's
/// tap removal can be watched and made to fail without touching the host.
/// Here, after `mod tests`, because several tests there read everything
/// above the first `#[cfg(test)]` of this file as its production half.
#[cfg(test)]
pub(crate) mod rule_fake {
    use super::*;
    use std::collections::BTreeMap;
    use std::sync::atomic::{AtomicBool, Ordering};

    /// One call the manager made, with the guest slots that were free at
    /// that moment.
    #[derive(Debug, Clone, PartialEq, Eq)]
    pub(crate) enum RuleCall {
        /// The isolation members' addresses, sorted.
        Isolation { ips: Vec<String>, free_slots: Vec<u32> },
        Ingress { members: Vec<(u16, String, u16)>, free_slots: Vec<u32> },
        RemoveTap { tap: String, free_slots: Vec<u32> },
        PinTap { tap: String },
    }

    #[derive(Default)]
    pub(crate) struct FakeRules {
        calls: std::sync::Mutex<Vec<RuleCall>>,
        pub(crate) fail_isolation: AtomicBool,
        pub(crate) fail_ingress: AtomicBool,
        pub(crate) fail_pin: AtomicBool,
        mgr: std::sync::OnceLock<std::sync::Weak<Manager>>,
        /// FLU-8: the in-memory kernel the rulesets land in.
        pub(crate) kernel: std::sync::Mutex<FakeKernel>,
        /// Every kernel operation, in order ("route …", "apply <chain>",
        /// "list <family> <table>", "link <dev> up|down", "write <path>").
        ops: std::sync::Mutex<Vec<String>>,
        /// Every look answers "could not look".
        pub(crate) fail_looks: AtomicBool,
        /// A ruleset whose text contains one of these is rejected.
        pub(crate) reject: std::sync::Mutex<Vec<String>>,
        /// Switch writes fail.
        pub(crate) fail_switch_write: AtomicBool,
        /// The next apply first removes this link (a delete racing a tick).
        pub(crate) vanish_before_apply: std::sync::Mutex<Option<String>>,
        /// `remove_tap` calls made without the rule lock held.
        pub(crate) unlocked_tap_removals: std::sync::atomic::AtomicUsize,
        /// `fences_ready` answers false.
        pub(crate) fences_down: AtomicBool,
        /// The outcomes of the next ingress rebuilds, in order (true =
        /// fail); once empty, `fail_ingress` decides.
        pub(crate) ingress_script: std::sync::Mutex<std::collections::VecDeque<bool>>,
        /// Run once, inside the SECOND `remove_tap` call, before it removes
        /// anything (two overlapping deletes).
        #[allow(clippy::type_complexity)]
        pub(crate) on_second_remove_tap: std::sync::Mutex<Option<Box<dyn FnOnce(&FakeRules, &Manager) + Send>>>,
        /// FLU-8 r7. A ruleset containing one of these gets no answer (the
        /// repair's outcome is unknown); nothing is applied.
        pub(crate) no_answer: std::sync::Mutex<Vec<String>>,
        /// Once an apply has been made, every look answers "could not look"
        /// (a second look that cannot be made).
        pub(crate) looks_fail_after_apply: AtomicBool,
        applied_once: AtomicBool,
        /// Every listing takes this long (the tick's budget).
        pub(crate) slow_list_ms: std::sync::atomic::AtomicU64,
        /// The next listing panics (a tick that panics).
        pub(crate) panic_on_list: AtomicBool,
        /// Rewrites every listing (a kernel that prints differently).
        #[allow(clippy::type_complexity)]
        pub(crate) rewrite_listing: std::sync::Mutex<Option<Box<dyn Fn(String) -> String + Send>>>,
        /// Tables listed as `flags dormant`. A transaction with a plain `add
        /// table` clears the flag, as on the lab (X14) — unless
        /// `dormant_sticky`.
        pub(crate) dormant: std::sync::Mutex<std::collections::BTreeSet<(String, String)>>,
        pub(crate) dormant_sticky: AtomicBool,
        /// What `pin_adopted_tap` answers (None: it pins).
        pub(crate) adopted_pin_fails: std::sync::Mutex<Option<PinFailure>>,
        /// Tables whose listing answers "could not look" (one table only).
        pub(crate) unknown_tables: std::sync::Mutex<std::collections::BTreeSet<(String, String)>>,
        /// Routes installed while the watch's mutex was held (X8).
        pub(crate) route_under_watch_lock: std::sync::atomic::AtomicUsize,
        /// Run once inside the next ingress rebuild (X9).
        #[allow(clippy::type_complexity)]
        pub(crate) on_ingress_rebuild: std::sync::Mutex<Option<Box<dyn FnOnce() + Send>>>,
    }

    /// One chain: its spec, its rules as `nft list` prints them.
    #[derive(Clone, Debug, Default)]
    pub(crate) struct FakeChain {
        spec: String,
        pub(crate) rules: Vec<String>,
    }

    /// One link: up or down, held by a process (carrier when up), a port of
    /// the guest bridge.
    #[derive(Clone, Debug)]
    pub(crate) struct FakeLink {
        pub(crate) up: bool,
        pub(crate) held: bool,
        pub(crate) port: bool,
    }

    /// The parts of a kernel the agent touches.
    #[derive(Clone, Debug, Default)]
    pub(crate) struct FakeKernel {
        /// (family, table) -> chain name -> chain.
        pub(crate) tables: BTreeMap<(String, String), BTreeMap<String, FakeChain>>,
        pub(crate) links: BTreeMap<String, FakeLink>,
        pub(crate) switches: BTreeMap<String, String>,
        pub(crate) paths: std::collections::BTreeSet<String>,
        pub(crate) routes: Vec<String>,
    }

    impl FakeKernel {
        /// Apply one `nft -f -` file, all or nothing.
        fn apply(&mut self, ruleset: &str) -> bool {
            let mut next = self.clone();
            for line in ruleset.lines().filter(|l| !l.trim().is_empty()) {
                if !next.apply_line(line) {
                    return false;
                }
            }
            *self = next;
            true
        }

        fn apply_line(&mut self, line: &str) -> bool {
            let w: Vec<&str> = line.splitn(6, ' ').collect();
            let key = |i: usize| (w.get(i).copied().unwrap_or("").to_string(), w.get(i + 1).copied().unwrap_or("").to_string());
            match (w.first().copied(), w.get(1).copied()) {
                (Some("add"), Some("table")) => {
                    self.tables.entry(key(2)).or_default();
                    true
                }
                (Some("add" | "flush"), Some("set" | "element")) => self.tables.contains_key(&key(2)),
                (Some("add"), Some("chain")) => {
                    let spec = line.split_once('{').map(|(_, s)| s.trim_end_matches('}').trim().to_string()).unwrap_or_default();
                    // A netdev chain names its device; nft resolves it at load.
                    if let Some(dev) = spec.split("device \"").nth(1).and_then(|s| s.split('"').next()) {
                        if !self.links.contains_key(dev) {
                            return false;
                        }
                    }
                    let Some(t) = self.tables.get_mut(&key(2)) else { return false };
                    let name = w.get(4).copied().unwrap_or("").to_string();
                    t.entry(name).or_insert(FakeChain { spec, rules: Vec::new() });
                    true
                }
                (Some("flush"), Some("chain")) => {
                    match self.tables.get_mut(&key(2)).and_then(|t| t.get_mut(w.get(4).copied().unwrap_or(""))) {
                        Some(c) => {
                            c.rules.clear();
                            true
                        }
                        None => false,
                    }
                }
                (Some("delete"), Some("chain")) => {
                    self.tables.get_mut(&key(2)).and_then(|t| t.remove(w.get(4).copied().unwrap_or(""))).is_some()
                }
                (Some("add"), Some("rule")) => {
                    let Some(c) = self.tables.get_mut(&key(2)).and_then(|t| t.get_mut(w.get(4).copied().unwrap_or(""))) else {
                        return false;
                    };
                    let rule = w.get(5).copied().unwrap_or("");
                    // nft lists a counter with its values.
                    let listed = match rule.strip_prefix("counter ") {
                        Some(rest) => format!("counter packets 0 bytes 0 {}", rest),
                        None => rule.to_string(),
                    };
                    c.rules.push(listed);
                    true
                }
                _ => false,
            }
        }

        /// `nft list table`, as nft prints it.
        pub(crate) fn listing(&self, family: &str, table: &str) -> Option<String> {
            let t = self.tables.get(&(family.to_string(), table.to_string()))?;
            let mut s = format!("table {} {} {{\n", family, table);
            for (name, c) in t {
                s.push_str(&format!("\tchain {} {{\n\t\t{}\n", name, c.spec));
                for r in &c.rules {
                    s.push_str(&format!("\t\t{}\n", r));
                }
                s.push_str("\t}\n");
            }
            s.push_str("}\n");
            Some(s)
        }

        pub(crate) fn chain_mut(&mut self, family: &str, table: &str, chain: &str) -> Option<&mut FakeChain> {
            self.tables.get_mut(&(family.to_string(), table.to_string()))?.get_mut(chain)
        }
    }

    impl FakeRules {
        fn op(&self, s: String) {
            lock_std(&self.ops).push(s);
        }

        pub(crate) fn ops(&self) -> Vec<String> {
            lock_std(&self.ops).clone()
        }

        pub(crate) fn k(&self) -> std::sync::MutexGuard<'_, FakeKernel> {
            lock_std(&self.kernel)
        }

        /// A tap enslaved to the guest bridge, up, held by a VMM or not.
        pub(crate) fn add_port(&self, tap: &str, held: bool) {
            self.k().links.insert(tap.into(), FakeLink { up: true, held, port: true });
        }

        /// What the kernel shows at the head of a chain (`net::chain_marker`).
        pub(crate) fn marker(&self, family: &str, table: &str, chain: &str) -> Option<Option<String>> {
            net::chain_marker(&self.k().listing(family, table)?, chain)
        }

        pub(crate) fn link_up(&self, dev: &str) -> Option<bool> {
            self.k().links.get(dev).map(|l| l.up)
        }

        /// Forget the applies made so far (`looks_fail_after_apply`).
        pub(crate) fn reset_applied(&self) {
            self.applied_once.store(false, Ordering::SeqCst);
        }
        pub(crate) fn calls(&self) -> Vec<RuleCall> {
            lock_std(&self.calls).clone()
        }

        pub(crate) fn clear(&self) {
            lock_std(&self.calls).clear();
        }

        /// The free guest slots, read off the manager's allocator. The
        /// manager makes these calls with `inner` released; one made while
        /// holding it panics here.
        ///
        /// CONSTRAINT: `try_lock`, not a wait — it is called from inside the
        /// (synchronous) rule calls, where awaiting is impossible and a
        /// blocking wait would deadlock the one runtime thread. It therefore
        /// also panics when ANOTHER task holds `inner` at that moment, which
        /// cannot happen on the current-thread runtime `#[tokio::test]` uses
        /// (the caller is the only task running), but can on a multi-thread
        /// one. Tests that use this fake must stay on the current-thread
        /// runtime; a test that needs another task holding `inner` must not
        /// make rule calls meanwhile.
        pub(crate) fn free_slots(&self) -> Vec<u32> {
            let Some(mgr) = self.mgr.get().and_then(|w| w.upgrade()) else { return Vec::new() };
            let mut g = mgr.inner.try_lock().expect("a rule call was made while holding `inner`");
            let Some(a) = g.allocator.as_mut() else { return Vec::new() };
            let mut free = Vec::new();
            while let Some(s) = a.claim() {
                free.push(s);
            }
            for s in &free {
                a.free(*s);
            }
            free
        }
    }

    impl RuleOps for FakeRules {
        fn active(&self, _net_on: bool) -> bool {
            true
        }
        fn rebuild_isolation(&self, members: &[(String, String)]) -> (net::Applied, bool) {
            let mut ips: Vec<String> = members.iter().map(|m| m.1.clone()).collect();
            ips.sort();
            let free_slots = self.free_slots();
            lock_std(&self.calls).push(RuleCall::Isolation { ips, free_slots });
            let outcome = if self.fail_isolation.load(Ordering::SeqCst) {
                net::Applied::Refused("Error: the fake refuses isolation".into())
            } else {
                net::Kernel::apply(self, &net::isolation_ruleset(members))
            };
            let ok = outcome.ok();
            (outcome, ok)
        }
        fn rebuild_ingress(&self, members: &[(u16, String, u16)]) -> net::Applied {
            let mut sorted = members.to_vec();
            sorted.sort();
            let free_slots = self.free_slots();
            lock_std(&self.calls).push(RuleCall::Ingress { members: sorted, free_slots });
            let hook = lock_std(&self.on_ingress_rebuild).take();
            if let Some(hook) = hook {
                hook();
            }
            let fail = lock_std(&self.ingress_script).pop_front().unwrap_or(self.fail_ingress.load(Ordering::SeqCst));
            if fail {
                return net::Applied::Refused("Error: the fake refuses ingress".into());
            }
            net::Kernel::apply(self, &net::ingress_ruleset(members))
        }
        fn remove_tap(&self, tap: &str) {
            let free_slots = self.free_slots();
            let earlier = lock_std(&self.calls).iter().filter(|c| matches!(c, RuleCall::RemoveTap { .. })).count();
            if let Some(mgr) = self.mgr.get().and_then(|w| w.upgrade()) {
                if mgr.ruleset_lock.try_lock().is_ok() {
                    self.unlocked_tap_removals.fetch_add(1, Ordering::SeqCst);
                }
                if earlier == 1 {
                    let hook = lock_std(&self.on_second_remove_tap).take();
                    if let Some(hook) = hook {
                        hook(self, &mgr);
                    }
                }
            }
            lock_std(&self.calls).push(RuleCall::RemoveTap { tap: tap.to_string(), free_slots });
            let mut k = self.k();
            if let Some(t) = k.tables.get_mut(&("netdev".to_string(), "felucca".to_string())) {
                t.remove(&format!("as_{}", tap.replace('-', "_")));
            }
            k.links.remove(tap);
        }
        fn pin_tap(&self, tap: &str, ip: Option<&str>, mac: &net::GuestMac) -> bool {
            lock_std(&self.calls).push(RuleCall::PinTap { tap: tap.to_string() });
            if self.fail_pin.load(Ordering::SeqCst) {
                return false;
            }
            // As `ensure_pinned_tap`: the tap first (no VMM holds it yet),
            // then its chain, then up.
            self.k().links.entry(tap.to_string()).or_insert(FakeLink { up: false, held: false, port: true });
            let pinned = match ip.and_then(|ip| net::antispoof_ruleset(tap, ip, mac)) {
                Some(rs) => net::Kernel::apply(self, &rs).ok(),
                None => false,
            };
            if pinned {
                if let Some(l) = self.k().links.get_mut(tap) {
                    l.up = true;
                }
            }
            pinned
        }
        fn pin_adopted_tap(&self, tap: &str, ip: &str, mac: &net::GuestMac) -> Result<(), PinFailure> {
            lock_std(&self.calls).push(RuleCall::PinTap { tap: tap.to_string() });
            if let Some(why) = *lock_std(&self.adopted_pin_fails) {
                if why == PinFailure::Unpinned {
                    if let Some(l) = self.k().links.get_mut(tap) {
                        l.up = false;
                    }
                }
                return Err(why);
            }
            match net::antispoof_ruleset(tap, ip, mac).map(|rs| net::Kernel::apply(self, &rs).ok()) {
                Some(true) => Ok(()),
                _ => Err(PinFailure::Unpinned),
            }
        }
        fn ensure_bridge(&self, _cidr: Cidr) -> (bool, bool) {
            self.op("bridge".into());
            (true, true)
        }
        fn note_host_fences(&self, _ok: bool) {}
        fn fences_ready(&self) -> bool {
            !self.fences_down.load(Ordering::SeqCst)
        }
        fn kernel(&self) -> &dyn net::Kernel {
            self
        }
    }

    impl net::Kernel for FakeRules {
        fn apply(&self, ruleset: &str) -> net::Applied {
            let what = ruleset
                .lines()
                .find(|l| l.starts_with("add chain "))
                .map(|l| l.split_whitespace().take(5).skip(2).collect::<Vec<_>>().join(" "))
                .unwrap_or_default();
            self.op(format!("apply {}", what));
            self.applied_once.store(true, Ordering::SeqCst);
            if let Some(dev) = lock_std(&self.vanish_before_apply).take() {
                self.k().links.remove(&dev);
            }
            if lock_std(&self.no_answer).iter().any(|r| ruleset.contains(r.as_str())) {
                return net::Applied::Unknown;
            }
            if lock_std(&self.reject).iter().any(|r| ruleset.contains(r.as_str())) {
                return net::Applied::Refused("Error: Could not process rule: the fake refuses it".into());
            }
            if self.k().apply(ruleset) {
                // X14: a plain `add table` clears a table's dormant flag.
                if !self.dormant_sticky.load(Ordering::SeqCst) {
                    let mut d = lock_std(&self.dormant);
                    for l in ruleset.lines() {
                        let w: Vec<&str> = l.split_whitespace().collect();
                        if w.len() == 4 && w[0] == "add" && w[1] == "table" {
                            d.remove(&(w[2].to_string(), w[3].to_string()));
                        }
                    }
                }
                net::Applied::Committed
            } else {
                // nft's answer for a device that does not exist, a rule for a
                // missing chain, and the like.
                net::Applied::Refused("Error: No such file or directory".into())
            }
        }
        fn list_table(&self, family: &str, table: &str) -> net::Listing {
            self.op(format!("list {} {}", family, table));
            if self.panic_on_list.swap(false, Ordering::SeqCst) {
                panic!("the fake kernel panics in a listing");
            }
            let ms = self.slow_list_ms.load(Ordering::SeqCst);
            if ms > 0 {
                std::thread::sleep(std::time::Duration::from_millis(ms));
            }
            if self.fail_looks.load(Ordering::SeqCst)
                || (self.looks_fail_after_apply.load(Ordering::SeqCst) && self.applied_once.load(Ordering::SeqCst))
                || lock_std(&self.unknown_tables).contains(&(family.to_string(), table.to_string()))
            {
                return net::Listing::Unknown;
            }
            let listed = self.k().listing(family, table);
            match listed {
                Some(mut l) => {
                    if lock_std(&self.dormant).contains(&(family.to_string(), table.to_string())) {
                        l = l.replacen(" {\n", " {\n\tflags dormant\n", 1);
                    }
                    if let Some(f) = lock_std(&self.rewrite_listing).as_ref() {
                        l = f(l);
                    }
                    net::Listing::Listed(l)
                }
                None => net::Listing::Missing,
            }
        }
        fn bridge_ports(&self) -> Option<Vec<net::Port>> {
            if self.fail_looks.load(Ordering::SeqCst) {
                return None;
            }
            Some(
                self.k()
                    .links
                    .iter()
                    .filter(|(_, l)| l.port)
                    .map(|(n, l)| net::Port { name: n.clone(), carrier: l.up && l.held, up: l.up })
                    .collect(),
            )
        }
        fn read_switch(&self, path: &str) -> net::Switch {
            if self.fail_looks.load(Ordering::SeqCst) {
                return net::Switch::Unknown;
            }
            match self.k().switches.get(path) {
                Some(v) => net::Switch::Value(v.clone()),
                None => net::Switch::Missing,
            }
        }
        fn write_switch(&self, path: &str, value: &str) -> bool {
            self.op(format!("write {}", path));
            if self.fail_switch_write.load(Ordering::SeqCst) {
                return false;
            }
            match self.k().switches.get_mut(path) {
                Some(v) => {
                    *v = value.to_string();
                    true
                }
                None => false,
            }
        }
        fn path_exists(&self, path: &str) -> bool {
            self.k().paths.contains(path)
        }
        fn set_link(&self, dev: &str, up: bool) -> bool {
            self.op(format!("link {} {}", dev, if up { "up" } else { "down" }));
            match self.k().links.get_mut(dev) {
                Some(l) => {
                    l.up = up;
                    true
                }
                None => false,
            }
        }
        fn link_exists(&self, dev: &str) -> Option<bool> {
            if self.fail_looks.load(Ordering::SeqCst) {
                return None;
            }
            Some(self.k().links.contains_key(dev))
        }
        fn link_up(&self, dev: &str) -> Option<bool> {
            self.k().links.get(dev).map(|l| l.up)
        }
        fn guest_route(&self, cidr: &str) -> bool {
            if let Some(mgr) = self.mgr.get().and_then(|w| w.upgrade()) {
                if mgr.watch.try_lock().is_err() {
                    self.route_under_watch_lock.fetch_add(1, Ordering::SeqCst);
                }
            }
            self.op(format!("route {}", cidr));
            self.k().routes.push(cidr.to_string());
            true
        }
    }

    /// Give a fresh `mgr` a `FakeRules` and the guest-slot allocator that
    /// `setup_host` gives a node with the network on (nothing is brought up).
    /// The fake kernel starts as a healthy worker's: the guest bridge, both
    /// switches on, and the host fence loaded.
    pub(crate) fn install(mgr: &mut Arc<Manager>) -> Arc<FakeRules> {
        let fake = Arc::new(FakeRules::default());
        {
            let mut k = fake.k();
            k.links.insert(net::BRIDGE_NAME.into(), FakeLink { up: true, held: false, port: false });
            k.switches.insert(net::BR_NF_SWITCH.into(), "1".into());
            k.switches.insert(net::bridge_v6_switch(), "1".into());
            k.paths.insert(net::IPV6_ROOT.into());
            let host = ("inet".to_string(), "felucca_host".to_string());
            k.tables.entry(host).or_default().insert(
                "input".into(),
                FakeChain {
                    spec: "type filter hook input priority filter; policy accept;".into(),
                    rules: vec![format!("iifname \"{}\" drop", net::BRIDGE_NAME)],
                },
            );
        }
        let m = Arc::get_mut(mgr).expect("a fresh manager");
        m.rules = fake.clone();
        let cidr = m.cidr;
        m.inner.get_mut().allocator = Some(Allocator::new(cidr));
        fake.mgr.set(Arc::downgrade(mgr)).expect("installed once");
        fake
    }

    /// Add a stopped sandbox holding guest slot `slot` (reserved, when there
    /// is an allocator) and its address, with its directory, as start-up
    /// leaves one. Returns the address.
    pub(crate) async fn add_vm(mgr: &Manager, id: &str, slot: u32, tenant: &str) -> String {
        std::fs::create_dir_all(format!("{}/instances/{}", mgr.data_dir, id)).unwrap();
        let ip = crate::ipalloc::fmt_ip(mgr.cidr.host_at(slot));
        let mut g = mgr.inner.lock().await;
        Manager::reserve_slot(&mut g, slot);
        g.vms.push(Vm {
            id: id.into(),
            name: id.into(),
            dir_id: id.into(),
            vcpus: 1,
            mem_mib: 256,
            state: VmState::Stopped,
            pid: None,
            slot: Some(slot),
            ip: Some(ip.clone()),
            vsock: false,
            tenant_id: Some(tenant.into()),
            exposes: Vec::new(),
            image: DEFAULT_IMAGE.into(),
            image_sha256: None,
            disk_gb: 0,
            snapshot_valid: false,
        });
        ip
    }

    /// Hold the lock every rule rebuild takes, so a rebuild waits.
    pub(crate) async fn hold_rule_lock(mgr: &Manager) -> tokio::sync::MutexGuard<'_, ()> {
        mgr.ruleset_lock.lock().await
    }

    /// `id`'s mappings, (guest port, node port).
    pub(crate) async fn exposes_of(mgr: &Manager, id: &str) -> Vec<(u16, u16)> {
        let g = mgr.inner.lock().await;
        g.vms
            .iter()
            .find(|v| v.id == id)
            .map(|v| v.exposes.iter().map(|e| (e.guest_port, e.node_port)).collect())
            .unwrap_or_default()
    }
}

#[cfg(test)]
mod rule_tests {
    use super::rule_fake::{add_vm, exposes_of, hold_rule_lock, install, FakeRules, RuleCall};
    use super::*;
    use std::collections::HashSet;
    use std::sync::atomic::Ordering;

    fn mgr_with_rules(data_dir: &str) -> (Arc<Manager>, Arc<FakeRules>) {
        let mut mgr = Manager::new(
            data_dir.to_string(),
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
        );
        let fake = install(&mut mgr);
        (mgr, fake)
    }

    fn exposes_on_disk(data_dir: &str, id: &str) -> Vec<(u16, u16)> {
        let written = std::fs::read_to_string(format!("{}/instances/{}/meta.json", data_dir, id)).unwrap();
        let m: Meta = serde_json::from_str(&written).expect("meta.json must parse");
        m.exposes.iter().map(|e| (e.guest_port, e.node_port)).collect()
    }

    fn none() -> HashSet<u16> {
        HashSet::new()
    }

    async fn set_cursor(mgr: &Manager, at: u16) {
        mgr.inner.lock().await.node_port_cursor = at;
    }

    // ---- FLU-6: expose ----

    #[tokio::test]
    async fn test_the_idempotent_expose_answer_ignores_avoid() {
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, _rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        let p = mgr.expose("vm-a", 8080, &none(), None).await.unwrap();
        // An avoid list that names it and every other port, and a prefer
        // elsewhere: the mapping that exists is still the answer.
        let all: HashSet<u16> = (NODE_PORT_MIN..=NODE_PORT_MAX).collect();
        assert_eq!(mgr.expose("vm-a", 8080, &all, Some(20000)).await, Ok(p));
        assert_eq!(exposes_of(&mgr, "vm-a").await, vec![(8080, p)]);
    }

    #[tokio::test]
    async fn test_two_exposes_get_different_ports_and_a_released_one_is_not_next() {
        // Was: lowest free — the port vm-a released went to vm-b at once.
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, _rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        add_vm(&mgr, "vm-b", 1, "t2").await;
        let p1 = mgr.expose("vm-a", 80, &none(), None).await.unwrap();
        let p2 = mgr.expose("vm-a", 81, &none(), None).await.unwrap();
        assert_ne!(p1, p2);
        mgr.unexpose("vm-a", 80).await.unwrap();
        let p3 = mgr.expose("vm-b", 80, &none(), None).await.unwrap();
        assert_ne!(p3, p1, "the port just released went straight to another sandbox");
        assert_ne!(p3, p2);
        // Round-robin: each one the port after the previous one.
        let after = |p: u16| if p == NODE_PORT_MAX { NODE_PORT_MIN } else { p + 1 };
        assert_eq!((p2, p3), (after(p1), after(p2)));
    }

    #[tokio::test]
    async fn test_expose_respects_avoid_and_prefer_across_sandboxes() {
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, _rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        add_vm(&mgr, "vm-b", 1, "t2").await;
        set_cursor(&mgr, 20000).await;
        let avoid: HashSet<u16> = [20000, 20001].into_iter().collect();
        assert_eq!(mgr.expose("vm-a", 80, &avoid, None).await, Ok(20002));
        // vm-b does not get vm-a's port, avoided or not.
        set_cursor(&mgr, 20002).await;
        let avoid: HashSet<u16> = [20003].into_iter().collect();
        assert_eq!(mgr.expose("vm-b", 80, &avoid, None).await, Ok(20004));
        // A prefer that another sandbox here holds is not honoured...
        assert_eq!(mgr.expose("vm-b", 81, &none(), Some(20002)).await, Ok(20005));
        // ...one that is free is, although it is in avoid,
        let avoid: HashSet<u16> = [21000].into_iter().collect();
        assert_eq!(mgr.expose("vm-b", 82, &avoid, Some(21000)).await, Ok(21000));
        // and the cursor did not move for it.
        assert_eq!(mgr.expose("vm-b", 83, &none(), None).await, Ok(20006));
        // Nothing left: 409 as before, and nothing recorded.
        let all: HashSet<u16> = (NODE_PORT_MIN..=NODE_PORT_MAX).collect();
        assert_eq!(mgr.expose("vm-a", 90, &all, None).await, Err(ExposeError::Exhausted));
        assert_eq!(exposes_of(&mgr, "vm-a").await, vec![(80, 20002)]);
    }

    #[tokio::test]
    async fn test_an_expose_whose_rule_is_not_installed_is_taken_back_out() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let (mgr, rules) = mgr_with_rules(&data_dir);
        let ip = add_vm(&mgr, "vm-a", 0, "t1").await;
        let p1 = mgr.expose("vm-a", 80, &none(), None).await.unwrap();
        assert_eq!(exposes_on_disk(&data_dir, "vm-a"), vec![(80, p1)]);

        // Was: the rule's result was ignored and the port answered anyway.
        rules.fail_ingress.store(true, Ordering::SeqCst);
        assert_eq!(mgr.expose("vm-a", 81, &none(), None).await, Err(ExposeError::RuleFailed));
        assert_eq!(exposes_of(&mgr, "vm-a").await, vec![(80, p1)]);
        assert_eq!(exposes_on_disk(&data_dir, "vm-a"), vec![(80, p1)]);
        // The idempotent answer does not rebuild, so it is not affected.
        assert_eq!(mgr.expose("vm-a", 80, &none(), None).await, Ok(p1));

        rules.fail_ingress.store(false, Ordering::SeqCst);
        let p2 = mgr.expose("vm-a", 81, &none(), None).await.unwrap();
        assert_eq!(exposes_on_disk(&data_dir, "vm-a"), vec![(80, p1), (81, p2)]);
        let mut want = vec![(p1, ip.clone(), 80), (p2, ip, 81)];
        want.sort();
        match rules.calls().last() {
            Some(RuleCall::Ingress { members, .. }) => assert_eq!(members, &want),
            other => panic!("the last call must be the ingress rebuild: {other:?}"),
        }
    }

    #[tokio::test]
    async fn test_an_unexpose_whose_rebuild_fails_still_answers_success() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let (mgr, rules) = mgr_with_rules(&data_dir);
        add_vm(&mgr, "vm-a", 0, "t1").await;
        mgr.expose("vm-a", 80, &none(), None).await.unwrap();
        rules.fail_ingress.store(true, Ordering::SeqCst);
        // The mapping is gone; a retry finds nothing and answers the same.
        assert_eq!(mgr.unexpose("vm-a", 80).await, Ok(()));
        assert_eq!(mgr.unexpose("vm-a", 80).await, Ok(()));
        assert!(exposes_of(&mgr, "vm-a").await.is_empty());
        assert!(exposes_on_disk(&data_dir, "vm-a").is_empty());
        assert_eq!(mgr.unexpose("vm-nope", 80).await, Err(ExposeError::NotFound));
    }

    // ---- FLU-6 R7: the order of a delete ----

    #[tokio::test]
    async fn test_delete_takes_the_sandbox_out_of_the_rules_before_it_frees_its_slot() {
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let (mgr, rules) = mgr_with_rules(&data_dir);
        add_vm(&mgr, "vm-a", 0, "t1").await;
        let ip_b = add_vm(&mgr, "vm-b", 1, "t1").await;
        mgr.expose("vm-a", 80, &none(), None).await.unwrap();
        let pb = mgr.expose("vm-b", 80, &none(), None).await.unwrap();
        let reserved = rules.free_slots();
        assert!(!reserved.contains(&0) && !reserved.contains(&1));
        rules.clear();

        mgr.delete("vm-a").await.unwrap();
        // Was: tap and slot first, then the directory, the record, and the
        // two rebuilds last — the address was free while rules still named
        // it. Now: both rebuilds without vm-a while its slot is still
        // reserved, then its tap,
        assert_eq!(
            rules.calls(),
            vec![
                RuleCall::Isolation { ips: vec![ip_b.clone()], free_slots: reserved.clone() },
                RuleCall::Ingress { members: vec![(pb, ip_b, 80)], free_slots: reserved.clone() },
                RuleCall::RemoveTap { tap: net::tap_name(0), free_slots: reserved },
            ]
        );
        // then the slot.
        let free = rules.free_slots();
        assert!(free.contains(&0) && !free.contains(&1));
        assert!(mgr.inner.lock().await.vms.iter().all(|v| v.id != "vm-a"));
        assert!(!Path::new(&format!("{}/instances/vm-a", data_dir)).exists());
    }

    #[tokio::test]
    async fn test_delete_keeps_the_slot_reserved_when_a_rebuild_fails() {
        for which in ["isolation", "ingress"] {
            let tmp = tempfile::tempdir().unwrap();
            let data_dir = tmp.path().to_str().unwrap().to_string();
            let (mgr, rules) = mgr_with_rules(&data_dir);
            add_vm(&mgr, "vm-a", 0, "t1").await;
            mgr.expose("vm-a", 80, &none(), None).await.unwrap();
            let flag = if which == "isolation" { &rules.fail_isolation } else { &rules.fail_ingress };
            flag.store(true, Ordering::SeqCst);
            rules.clear();

            mgr.delete("vm-a").await.unwrap();
            let calls = rules.calls();
            // Both rebuilds were made, without vm-a, whichever one failed,
            // and the guest's tap went (the guest is gone)...
            assert!(
                matches!(
                    calls.as_slice(),
                    [
                        RuleCall::Isolation { ips, .. },
                        RuleCall::Ingress { members, .. },
                        RuleCall::RemoveTap { tap, .. },
                    ] if ips.is_empty() && members.is_empty() && *tap == net::tap_name(0)
                ),
                "{which}: {calls:?}"
            );
            // ...but its address does not go to the next sandbox.
            assert!(!rules.free_slots().contains(&0), "{which}: slot 0 was freed");
            // The record and the directory go as before.
            assert!(mgr.inner.lock().await.vms.is_empty(), "{which}");
            assert!(!Path::new(&format!("{}/instances/vm-a", data_dir)).exists(), "{which}");

            // A second delete stays idempotent and touches no tap.
            rules.clear();
            mgr.delete("vm-a").await.unwrap();
            let calls = rules.calls();
            assert!(calls.iter().all(|c| !matches!(c, RuleCall::RemoveTap { .. })), "{which}: {calls:?}");
            assert!(!rules.free_slots().contains(&0), "{which}: slot 0 was freed by the second delete");
        }
    }

    /// A sandbox `id` in slot `slot`, as `add_vm` makes one, put straight
    /// into the manager from inside a (synchronous) rule call, with its tap
    /// held by a VMM and pinned.
    fn create_inside_rule_call(rules: &FakeRules, mgr: &Manager, id: &str, slot: u32) {
        let ip = crate::ipalloc::fmt_ip(mgr.cidr.host_at(slot));
        {
            let mut g = mgr.inner.try_lock().expect("inner is free inside a rule call");
            assert!(Manager::claim_slot(&mut g) == Ok(slot), "slot {slot} must be the lowest free one by now");
            g.vms.push(Vm {
                id: id.into(),
                name: id.into(),
                dir_id: id.into(),
                vcpus: 1,
                mem_mib: 256,
                state: VmState::Running,
                pid: None,
                slot: Some(slot),
                ip: Some(ip.clone()),
                vsock: false,
                tenant_id: Some("t9".into()),
                exposes: Vec::new(),
                image: DEFAULT_IMAGE.into(),
                image_sha256: None,
                disk_gb: 0,
                snapshot_valid: false,
            });
        }
        let (tap, mac) = tap_pin_for_slot(slot);
        rules.k().links.insert(tap.clone(), super::rule_fake::FakeLink { up: true, held: true, port: true });
        assert!(net::Kernel::apply(rules, &net::antispoof_ruleset(&tap, &ip, &mac).unwrap()).ok());
    }

    #[tokio::test]
    async fn test_two_overlapping_deletes_free_the_slot_once() {
        // RQ-1: both deletes read the slot at entry. The second one to finish
        // removed the tap and pin of a sandbox created in between (it had got
        // the slot, the lowest free) and freed its slot again.
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        // Slot 0 must be the lowest free one once vm-a's goes.
        rules.clear();
        // If a second tap removal happens, a create gets in just before it.
        *lock_std(&rules.on_second_remove_tap) = Some(Box::new(|r: &FakeRules, m: &Manager| {
            create_inside_rule_call(r, m, "vm-new", 0)
        }));
        let held = hold_rule_lock(&mgr).await;
        let d1 = {
            let mgr = Arc::clone(&mgr);
            tokio::spawn(async move { mgr.delete("vm-a").await })
        };
        let d2 = {
            let mgr = Arc::clone(&mgr);
            tokio::spawn(async move { mgr.delete("vm-a").await })
        };
        // Both past their first step, parked at the rule lock.
        sleep(Duration::from_millis(50)).await;
        drop(held);
        d1.await.unwrap().unwrap();
        d2.await.unwrap().unwrap();
        let removals: Vec<RuleCall> =
            rules.calls().into_iter().filter(|c| matches!(c, RuleCall::RemoveTap { .. })).collect();
        assert_eq!(removals.len(), 1, "the tap was removed twice: {removals:?}");
        // The sandbox created afterwards (or in between) keeps its slot,
        // its tap and its chain.
        if mgr.inner.lock().await.vms.iter().all(|v| v.id != "vm-new") {
            create_inside_rule_call(&rules, &mgr, "vm-new", 0);
        }
        assert!(!rules.free_slots().contains(&0), "vm-new's slot was freed");
        assert!(rules.k().links.contains_key("hth-0"), "vm-new lost its tap");
        assert!(matches!(rules.marker("netdev", "felucca", "as_hth_0"), Some(Some(_))), "vm-new lost its pin");
        assert!(mgr.inner.lock().await.held_slots.is_empty());

        // The same with the rebuilds failing: held exactly once, freed once.
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        rules.fail_isolation.store(true, Ordering::SeqCst);
        let held = hold_rule_lock(&mgr).await;
        let (m1, m2) = (Arc::clone(&mgr), Arc::clone(&mgr));
        let d1 = tokio::spawn(async move { m1.delete("vm-a").await });
        let d2 = tokio::spawn(async move { m2.delete("vm-a").await });
        sleep(Duration::from_millis(50)).await;
        drop(held);
        d1.await.unwrap().unwrap();
        d2.await.unwrap().unwrap();
        assert_eq!(mgr.inner.lock().await.held_slots.iter().copied().collect::<Vec<_>>(), vec![0]);
        rules.fail_isolation.store(false, Ordering::SeqCst);
        assert!(mgr.refresh_isolation().await);
        assert!(rules.free_slots().contains(&0));
    }

    #[tokio::test]
    async fn test_a_failed_expose_writes_nothing_and_leaves_no_rule() {
        // RQ-13: the mapping was written to meta.json before the rule was
        // known to be in; a roll-back write that did not land before a
        // restart brought it back.
        let tmp = tempfile::tempdir().unwrap();
        let data_dir = tmp.path().to_str().unwrap().to_string();
        let (mgr, rules) = mgr_with_rules(&data_dir);
        add_vm(&mgr, "vm-a", 0, "t1").await;
        let p1 = mgr.expose("vm-a", 80, &none(), None).await.unwrap();
        let meta = format!("{}/instances/vm-a/meta.json", data_dir);
        let before = std::fs::read(&meta).unwrap();
        rules.fail_ingress.store(true, Ordering::SeqCst);
        set_cursor(&mgr, 21000).await;
        assert_eq!(mgr.expose("vm-a", 81, &none(), None).await, Err(ExposeError::RuleFailed));
        // Nothing written: what a restart reads is what was there before.
        assert_eq!(std::fs::read(&meta).unwrap(), before, "meta.json was written for a failed expose");
        assert_eq!(exposes_on_disk(&data_dir, "vm-a"), vec![(80, p1)]);
        assert_eq!(exposes_of(&mgr, "vm-a").await, vec![(80, p1)]);
        // And no rule for it.
        rules.fail_ingress.store(false, Ordering::SeqCst);
        let ingress = rules.k().listing("ip", "felucca").unwrap();
        assert!(!ingress.contains("dport 21000"), "{ingress}");
        // The rebuild without it ran after the failure.
        let last_two: Vec<RuleCall> = rules.calls().into_iter().rev().take(2).collect();
        assert!(matches!(&last_two[0], RuleCall::Ingress { members, .. } if members.iter().all(|m| m.0 != 21000)), "{last_two:?}");
    }

    #[tokio::test]
    async fn test_a_concurrent_rebuild_cannot_install_a_failing_exposes_rule() {
        // RQ-13: another sandbox's rebuild, run between the push of the new
        // mapping and its own failed rebuild, installed the doomed rule.
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        add_vm(&mgr, "vm-b", 1, "t2").await;
        set_cursor(&mgr, 22000).await;
        // The other sandbox's rebuild succeeds; this expose's fails.
        lock_std(&rules.ingress_script).extend([false, true]);
        let held = hold_rule_lock(&mgr).await;
        let other = {
            let mgr = Arc::clone(&mgr);
            tokio::spawn(async move { mgr.refresh_ingress().await })
        };
        sleep(Duration::from_millis(20)).await;
        let doomed = {
            let mgr = Arc::clone(&mgr);
            tokio::spawn(async move { mgr.expose("vm-a", 80, &HashSet::new(), None).await })
        };
        sleep(Duration::from_millis(20)).await;
        drop(held);
        assert!(other.await.unwrap());
        assert_eq!(doomed.await.unwrap(), Err(ExposeError::RuleFailed));
        let ingress = rules.k().listing("ip", "felucca").unwrap();
        assert!(!ingress.contains("dport 22000"), "the doomed rule is in the kernel: {ingress}");
        // Not even for a moment: only the expose's own rebuild saw it.
        let saw: Vec<RuleCall> = rules
            .calls()
            .into_iter()
            .filter(|c| matches!(c, RuleCall::Ingress { members, .. } if members.iter().any(|m| m.0 == 22000)))
            .collect();
        assert_eq!(saw.len(), 1, "another rebuild carried the doomed mapping: {saw:?}");
    }

    #[tokio::test]
    async fn test_an_expose_of_a_sandbox_being_deleted_answers_no_ip() {
        // While the rules are rebuilt the record is on its way out: no
        // address, no exposes. An expose arriving then must not add a
        // mapping (and a rule) for an address about to be freed.
        let tmp = tempfile::tempdir().unwrap();
        let (mgr, _rules) = mgr_with_rules(tmp.path().to_str().unwrap());
        add_vm(&mgr, "vm-a", 0, "t1").await;
        let held = hold_rule_lock(&mgr).await;
        let deleting = {
            let mgr = Arc::clone(&mgr);
            tokio::spawn(async move { mgr.delete("vm-a").await })
        };
        let mut out = false;
        for _ in 0..1000 {
            if mgr.inner.lock().await.vms.iter().any(|v| v.id == "vm-a" && v.ip.is_none()) {
                out = true;
                break;
            }
            sleep(Duration::from_millis(5)).await;
        }
        assert!(out, "the delete never took the sandbox out of the membership");
        assert_eq!(mgr.expose("vm-a", 80, &none(), None).await, Err(ExposeError::NoIp));
        assert!(exposes_of(&mgr, "vm-a").await.is_empty());
        drop(held);
        deleting.await.unwrap().unwrap();
    }
}
