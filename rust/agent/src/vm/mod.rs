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

use crate::fc;
use crate::ipalloc::{Allocator, Cidr};
use crate::net;
use tracing::{error, info, warn};
use meta::{ExposeEntry, Meta, VmState};
use procscan::{FcProc, Identity, ProcTable};
use serde::Deserialize;
use std::os::unix::io::{FromRawFd, IntoRawFd};
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
/// * `unreadable` — directories whose `meta.json` exists and does not load.
/// * `metaless` — directories with no `meta.json`.
/// * `mine` — is this pid in the agent's own cgroup?
///
/// The rule that matters most: a process is killed only when it is certain
/// that no caller was ever told its VM exists, or that the record that does
/// exist has moved on without it. A VMM beside a damaged record is very likely
/// a healthy tenant VM, and is left alone.
fn plan_sweep(
    found: &[FcProc],
    owned: &std::collections::HashSet<i32>,
    record_dirs: &std::collections::HashSet<String>,
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
        } else if metaless.iter().any(|d| d == dir) || record_dirs.contains(dir) {
            // No meta.json: the create never completed. Or a record that did
            // not adopt this pid: a wake or start that never completed (the
            // record still says sleeping/stopped), or a second VMM on a
            // directory whose record adopted another one.
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
    unreachable: usize,
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
    /// Every node port in NODE_PORT_MIN..=NODE_PORT_MAX is taken (→ 409).
    Exhausted,
}

/// Worker-side ingress node-port range (inclusive).
const NODE_PORT_MIN: u16 = 20000;
const NODE_PORT_MAX: u16 = 29999;

/// Lowest node port not in `used` (used spans every expose of every VM).
fn lowest_free_node_port(used: &std::collections::HashSet<u16>) -> Option<u16> {
    (NODE_PORT_MIN..=NODE_PORT_MAX).find(|p| !used.contains(p))
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
    // procfs root — always /proc outside unit tests.
    proc_root: std::path::PathBuf,
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
            }),
            ruleset_lock: Mutex::new(()),
            image_locks: Mutex::new(std::collections::HashMap::new()),
            pool_backoff: Mutex::new(std::collections::HashMap::new()),
            meta_write_lock: Mutex::new(()),
            data_dir_lock: std::sync::OnceLock::new(),
            proc_root,
        })
    }

    /// The process table, scoped to this agent's data dir.
    fn procs(&self) -> ProcTable {
        ProcTable::at(&self.proc_root, &self.data_dir)
    }

    /// Bring up bridge/NAT (if net on) and init the IP allocator. Call once before serving.
    pub async fn setup_host(&self) {
        if !self.net_on { return; }
        let alloc = Allocator::new(self.cidr);
        {
            let mut g = self.inner.lock().await;
            g.allocator = Some(alloc);
        }
        if !net::ensure_bridge(self.cidr) {
            warn!("bridge setup failed; guests may lack networking");
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
    /// 2. Sweep what belongs to no committed VM: the Firecracker of a create,
    ///    wake or start that never finished, directories that never got a
    ///    `meta.json`, and pool VMs whose VMM is gone.
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
        let (unreadable, metaless, dead_pool, adopted) = {
            let mut g = self.inner.lock().await;
            let rec = reconcile::reconcile(&self.data_dir, g.allocator.as_mut(), &procs);
            g.vms = rec.vms;
            let adopted = g.vms.iter().filter(|v| v.pid.is_some()).count();
            (rec.unreadable, rec.metaless, rec.dead_pool, adopted)
        };

        let mut swept = self.sweep_orphans(&procs, &unreadable, &metaless).await;
        swept.pool_removed = self.remove_dead_pool_vms(&procs, &dead_pool).await;

        let mut taps_down = 0usize;
        if self.net_on {
            let taps = {
                let g = self.inner.lock().await;
                taps_to_repin(&g.vms)
            };
            // Outside the state lock: these shell out to ip/nft.
            for (id, slot, ip) in taps {
                let (tap, mac) = tap_pin_for_slot(slot);
                if !ensure_pinned_tap(&tap, Some(&ip), &mac, Unpinned::TakeDown) {
                    taps_down += 1;
                    error!(
                        vm = %id,
                        tap = %tap,
                        "adopted VM's tap could not be pinned — the port has been taken down \
                         (not removed), so this guest has no network until an agent start pins it"
                    );
                }
            }
        }

        let settled = self.settle_adopted_run_states().await;

        info!(
            adopted,
            resumed = settled.resumed,
            paused = settled.paused,
            failed = settled.failed,
            unreachable = settled.unreachable,
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
    /// allowed to decide that a Firecracker under this data dir belongs to
    /// nobody. A second agent started by hand against the same data dir would
    /// otherwise reconcile, find the first agent's in-flight creates — which
    /// have no `meta.json` yet — and kill them, before failing to bind.
    fn hold_data_dir_lock(&self) -> bool {
        self.data_dir_lock
            .get_or_init(|| {
                use std::os::unix::io::AsRawFd;
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
        let (owned, record_dirs) = {
            let g = self.inner.lock().await;
            let owned: std::collections::HashSet<i32> = g.vms.iter().filter_map(|v| v.pid).collect();
            let dirs: std::collections::HashSet<String> =
                g.vms.iter().map(|v| v.dir_id.clone()).collect();
            (owned, dirs)
        };
        let found = procs.scan();
        let plan = plan_sweep(&found, &owned, &record_dirs, unreadable, metaless, |pid| {
            procs.in_my_cgroup(pid)
        });
        for step in plan {
            match step {
                SweepStep::Kill { pid, dir_name } => {
                    warn!(
                        pid,
                        dir = %dir_name,
                        "reconcile: killing a Firecracker that belongs to no committed VM \
                         (a create, wake or start the previous agent did not finish, or a duplicate)"
                    );
                    self.kill_fc_and_wait(pid, &dir_name).await;
                    out.killed += 1;
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
    async fn settle_adopted_run_states(&self) -> Settled {
        let adopted: Vec<(String, String, VmState, i32)> = {
            let g = self.inner.lock().await;
            g.vms
                .iter()
                .filter_map(|v| v.pid.map(|p| (v.id.clone(), v.dir_id.clone(), v.state.clone(), p)))
                .collect()
        };
        let mut out = Settled::default();
        for (id, dir_id, state, pid) in adopted {
            let Some(dir) = instance_path(&self.data_dir, &dir_id) else {
                continue;
            };
            let sock = format!("{}/{}", dir, procscan::SOCK_NAME);
            let vmm = match tokio::time::timeout(ADOPT_API_TIMEOUT, fc::get_instance_state(&sock)).await {
                Ok(Ok(s)) => s,
                Ok(Err(e)) => {
                    warn!(vm = %id, pid, err = %e, "adopted VM's Firecracker did not answer on its API socket; keeping the record as it is");
                    out.unreachable += 1;
                    continue;
                }
                Err(_) => {
                    warn!(vm = %id, pid, "adopted VM's Firecracker did not answer on its API socket in time; keeping the record as it is");
                    out.unreachable += 1;
                    continue;
                }
            };
            match settle_action(&state, &vmm) {
                SettleAction::None => {}
                SettleAction::Resume => {
                    let done = tokio::time::timeout(ADOPT_API_TIMEOUT, fc::patch_vm_state(&sock, "Resumed")).await;
                    if matches!(done, Ok(Ok(()))) {
                        warn!(vm = %id, pid, "adopted VM was paused inside Firecracker under a record that says running (the previous agent stopped in the middle of a sleep, fork or pause); resumed");
                        out.resumed += 1;
                    } else {
                        // Could not resume it. Record what is true instead, so
                        // that `resume` is a real request and not a no-op on a
                        // VM that is listed as running and frozen.
                        error!(vm = %id, pid, "adopted VM is paused inside Firecracker and could not be resumed; recording it as paused");
                        self.set_state(&id, VmState::Paused).await;
                        let _ = self.write_meta(&id).await;
                        out.failed += 1;
                    }
                }
                SettleAction::Pause => {
                    let done = tokio::time::timeout(ADOPT_API_TIMEOUT, fc::patch_vm_state(&sock, "Paused")).await;
                    if matches!(done, Ok(Ok(()))) {
                        warn!(vm = %id, pid, state = %state, "adopted VM was running inside Firecracker under a record that says it is not (the previous agent stopped in the middle of a resume or a pool claim); paused again");
                        out.paused += 1;
                    } else if state == VmState::Paused {
                        error!(vm = %id, pid, "adopted VM is running inside Firecracker and could not be paused; recording it as running");
                        self.set_state(&id, VmState::Running).await;
                        let _ = self.write_meta(&id).await;
                        out.failed += 1;
                    } else {
                        // A pooled VM that would not pause is still claimable:
                        // the claim's resume is a no-op on a running guest.
                        error!(vm = %id, pid, "adopted pool VM is running inside Firecracker and could not be paused");
                        out.failed += 1;
                    }
                }
                SettleAction::Discard => {
                    error!(vm = %id, pid, "adopted Firecracker never started its guest; killing it");
                    self.kill_fc_and_wait(pid, &dir_id).await;
                    {
                        let mut g = self.inner.lock().await;
                        if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                            if v.pid == Some(pid) {
                                v.state = VmState::Error;
                                v.pid = None;
                            }
                        }
                    }
                    let _ = self.write_meta(&id).await;
                    out.failed += 1;
                }
            }
        }
        out
    }

    /// SIGKILL an instance's Firecracker and wait until the process is gone.
    ///
    /// The identity check comes first and is repeated while waiting: the pid
    /// is only signalled while it is positively (or, on a failed read,
    /// possibly) this instance's VMM, never merely because a process has that
    /// number. Waiting matters for VMMs this agent did not spawn — `waitpid`
    /// returns at once for a process that is not a child, and a caller that
    /// went on to respawn (sleep → wake) would find the tap and the sockets
    /// still held by the dying one.
    async fn kill_fc_and_wait(&self, pid: i32, dir_id: &str) {
        use nix::sys::signal::{kill, Signal};
        use nix::unistd::Pid;
        let procs = self.procs();
        if procs.identify(pid, dir_id) == Identity::Mismatch {
            // Not this instance's VMM (any more). If that is because it is
            // already on its way out, give it the moment it needs to let go
            // of the tap; a stranger with a recycled pid is not waited for.
            if !procs.exiting(pid) {
                return;
            }
        } else {
            let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
        }
        // "Exited" means every thread is gone, which is when the memory, the
        // tap and the sockets are released — not merely that the process no
        // longer looks like a Firecracker (see `ProcTable::exited`).
        let deadline = Instant::now() + KILL_WAIT;
        while !procs.exited(pid) {
            if Instant::now() >= deadline {
                warn!(pid, dir = %dir_id, "firecracker still present {:?} after SIGKILL", KILL_WAIT);
                break;
            }
            sleep(Duration::from_millis(20)).await;
        }
    }

    /// Whether this node's guest network fences are in force. Cheap enough
    /// for a health handler or a 5s heartbeat.
    ///
    /// A `--net off` node has no guest network at all, so there is nothing to
    /// isolate and nothing to report as broken — the underlying atomics only
    /// mean anything once host networking has been set up.
    pub fn isolation_ok(&self) -> bool {
        !self.net_on || net::netfilter_ready()
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
        if !self.net_on { return true; }
        let _guard = self.ruleset_lock.lock().await;
        let members: Vec<(String, String)> = {
            let g = self.inner.lock().await;
            g.vms.iter()
                .filter_map(|v| {
                    v.ip.clone().map(|ip| (v.tenant_id.clone().unwrap_or_default(), ip))
                })
                .collect()
        };
        net::rebuild_isolation(&members)
    }

    /// Rebuild the ingress DNAT ruleset from the current VM list (every expose
    /// of every VM holding an IP). Same discipline as `refresh_isolation`:
    /// snapshot membership under the state lock, release it, then shell out to
    /// nft under the ruleset lock so rebuilds never interleave.
    pub async fn refresh_ingress(&self) {
        if !self.net_on { return; }
        let _guard = self.ruleset_lock.lock().await;
        let members: Vec<(u16, String, u16)> = {
            let g = self.inner.lock().await;
            let mut out = Vec::new();
            for v in &g.vms {
                if let Some(ip) = &v.ip {
                    for e in &v.exposes {
                        out.push((e.node_port, ip.clone(), e.guest_port));
                    }
                }
            }
            out
        };
        net::rebuild_ingress(&members);
    }

    // ---- ingress expose (v4 P3) ----

    /// Map a host node port to `id`'s ip:guest_port (TCP DNAT). Idempotent per
    /// (vm, guest_port): an existing entry returns its node_port unchanged.
    pub async fn expose(&self, id: &str, guest_port: u16) -> Result<u16, ExposeError> {
        let node_port = {
            let mut g = self.inner.lock().await;
            // node ports are unique across ALL VMs — collect before the
            // mutable borrow of the target record.
            let used: std::collections::HashSet<u16> = g.vms.iter()
                .flat_map(|v| v.exposes.iter().map(|e| e.node_port))
                .collect();
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or(ExposeError::NotFound)?;
            if v.ip.is_none() { return Err(ExposeError::NoIp); }
            if let Some(e) = v.exposes.iter().find(|e| e.guest_port == guest_port) {
                // Idempotent: mapping already live, nothing to persist or rebuild.
                return Ok(e.node_port);
            }
            let np = lowest_free_node_port(&used).ok_or(ExposeError::Exhausted)?;
            v.exposes.push(ExposeEntry { guest_port, node_port: np });
            np
        };
        // Persist + rebuild outside the state lock (write_meta re-locks).
        // Persist is best-effort like other meta writes off the happy path:
        // the in-memory entry and the nft rule are already authoritative.
        if let Err(e) = self.write_meta(id).await {
            warn!(vm = %id, err = %e, "expose persist failed");
        }
        self.refresh_ingress().await;
        Ok(node_port)
    }

    /// Remove the (vm, guest_port) mapping. A missing entry is OK (idempotent);
    /// NotFound only when the VM itself is unknown.
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
            self.refresh_ingress().await;
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

    fn claim_slot(inner: &mut Inner) -> Option<u32> {
        inner.allocator.as_mut()?.claim()
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

    // ---- create ----

    pub async fn create(self: &Arc<Self>, spec: &CreateSpec) -> Result<(), String> {
        // Rejected before a record exists, so a bad id never reaches the
        // instances tree even as a Creating placeholder.
        if !valid_vm_id(&spec.id) {
            return Err("InvalidId".into());
        }
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
        if self.refresh_isolation().await {
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

        if self.net_on {
            let s = {
                let mut g = self.inner.lock().await;
                if let Some(fs) = force_slot {
                    Self::reserve_slot(&mut g, fs);
                    fs
                } else {
                    Self::claim_slot(&mut g).ok_or("IpPoolExhausted")?
                }
            };
            slot = Some(s);
            ip_str = {
                let g = self.inner.lock().await;
                Self::ip_for_slot(&g, s)
            };
            // Cold boot sets the NIC's MAC itself (configure_guest), so the
            // L2 half of the pin applies from the first frame.
            let (tap, mac) = tap_pin_for_slot(s);
            if !ensure_pinned_tap(&tap, ip_str.as_deref(), &mac, Unpinned::Remove) {
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
                return Err("TapSetupFailed".into());
            }
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

        let log_path = format!("{}/serial.log", dir_path);
        let log_file = open_serial_log(&log_path)
            .map_err(|e| format!("open serial.log: {}", e))?;

        let log_fd = log_file.into_raw_fd();

        // Spawn firecracker with stdin null, stdout/stderr → serial.log.
        let child = tokio::process::Command::new("firecracker")
            .arg("--api-sock").arg(&sock)
            .stdin(std::process::Stdio::null())
            .stdout(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .stderr(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .kill_on_drop(false) // ALWAYS false per spec
            .spawn()
            .map_err(|e| format!("spawn firecracker: {}", e))?;

        let pid = child.id().ok_or("NoPid")? as i32;
        // Detached reaper: collects the exit status whenever FC dies (our
        // SIGKILL on sleep, guest shutdown, crash) so it never zombies, and
        // flags unexpected exits (pid still recorded) as state=error.
        // kill_on_drop is false, so dropping after wait never kills anything.
        {
            let mgr = Arc::clone(self);
            let rid = id.to_string();
            tokio::spawn(async move {
                let mut child = child;
                let _ = child.wait().await;
                mgr.on_fc_exit(&rid, pid).await;
            });
        }

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

    pub async fn pause(&self, id: &str) -> Result<(), String> {
        {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Paused => return Ok(()), // idempotent
                VmState::Running => {}
                _ => return Err(format!("InvalidState: {}", v.state.as_str())),
            }
        }
        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        fc::patch_vm_state(&sock, "Paused").await.map_err(|e| e.to_string())?;
        self.set_state(id, VmState::Paused).await;
        self.write_meta(id).await
    }

    pub async fn resume(&self, id: &str) -> Result<(), String> {
        {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            match v.state {
                VmState::Running => return Ok(()), // idempotent
                VmState::Paused => {}
                _ => return Err(format!("InvalidState: {}", v.state.as_str())),
            }
        }
        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        fc::patch_vm_state(&sock, "Resumed").await.map_err(|e| e.to_string())?;
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await
    }

    // ---- sleep / wake ----

    pub async fn sleep_vm(&self, id: &str) -> Result<(), String> {
        let (pid, dir_id) = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            if v.state == VmState::Sleeping { return Ok(()); }
            (v.pid, v.dir_id.clone())
        };

        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        let dir_path = self.instance_dir(id).await.ok_or("InvalidId")?;

        fc::patch_vm_state(&sock, "Paused").await.map_err(|e| e.to_string())?;

        let vmstate = format!("{}/vmstate.bin", dir_path);
        let mem = format!("{}/mem.bin", dir_path);
        fc::put_snapshot_create(&sock, &vmstate, &mem).await
            .map_err(|e| e.to_string())?;

        // Clear the pid BEFORE killing so the exit reaper sees a pid mismatch
        // and treats this as a deliberate kill (no error transition).
        self.set_pid(id, None).await;
        if let Some(p) = pid {
            // Waits for the process to be gone even when it is not this
            // agent's child (a VMM adopted at start-up): the wake that follows
            // needs the tap and the sockets it holds.
            self.kill_fc_and_wait(p, &dir_id).await;
        }

        self.set_state(id, VmState::Sleeping).await;
        self.write_meta(id).await
    }

    /// Spawn firecracker → snapshot/load. Returns wake latency in ms.
    pub async fn wake_vm(self: &Arc<Self>, id: &str) -> Result<u64, String> {
        let (slot, ip) = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            if v.state == VmState::Running { return Ok(0); }
            (v.slot, v.ip.clone())
        };

        let start = Instant::now();
        let dir_path = self.instance_dir(id).await.ok_or("InvalidId")?;

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
        if self.net_on {
            if let Some(s) = slot {
                let (tap, mac) = tap_pin_for_slot(s);
                if !ensure_pinned_tap(&tap, ip.as_deref(), &mac, Unpinned::Remove) {
                    return Err("TapSetupFailed".into());
                }
            }
        }

        let sock = self.sock_path(id).await.ok_or("InvalidId")?;
        let _ = std::fs::remove_file(&sock);
        // A SIGKILLed FC (sleep) leaves the vsock host UDS behind; the restore
        // re-binds the snapshot-baked path and fails with EADDRINUSE if the
        // stale file is still there.
        let _ = std::fs::remove_file(format!("{}/v.sock", dir_path));

        let log_path = format!("{}/serial.log", dir_path);
        let log_file = open_serial_log(&log_path)
            .map_err(|e| format!("open serial.log: {}", e))?;

        let log_fd = log_file.into_raw_fd();

        let child = tokio::process::Command::new("firecracker")
            .arg("--api-sock").arg(&sock)
            .stdin(std::process::Stdio::null())
            .stdout(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .stderr(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .kill_on_drop(false)
            .spawn()
            .map_err(|e| format!("spawn firecracker: {}", e))?;

        let pid = child.id().ok_or("NoPid")? as i32;
        {
            let mgr = Arc::clone(self);
            let rid = id.to_string();
            tokio::spawn(async move {
                let mut child = child;
                let _ = child.wait().await;
                mgr.on_fc_exit(&rid, pid).await;
            });
        }

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

        let vmstate = format!("{}/vmstate.bin", dir_path);
        let mem = format!("{}/mem.bin", dir_path);
        // Wake restores into the VM's OWN dir, so the baked vsock uds_path is
        // already correct — no override needed (None re-binds the original path).
        if let Err(e) = fc::put_snapshot_load(&sock, &vmstate, &mem, self.net_on, slot, None).await {
            kill_and_reap(pid); // don't leak the FC that failed to restore
            return Err(e.to_string());
        }

        let elapsed = start.elapsed().as_millis() as u64;

        self.set_pid(id, Some(pid)).await;
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await?;
        Ok(elapsed)
    }

    // ---- fork ----

    /// Fork the parent: ensure snapshot exists, reflink rootfs + copy mem.bin,
    /// spawn child FC and restore. Child inherits parent's guest-internal IP
    /// (v2 documented caveat; fixed in v3).
    pub async fn fork(self: &Arc<Self>, parent_id: &str, child_id: &str, child_name: &str) -> Result<Option<String>, String> {
        if !valid_vm_id(child_id) {
            return Err("InvalidId".into());
        }
        let (parent_was_running, parent_vcpus, parent_mem_mib, parent_vsock, parent_tenant_id, parent_image, parent_image_sha256, parent_disk_gb) = {
            let g = self.inner.lock().await;
            let p = g.vms.iter().find(|v| v.id == parent_id).ok_or("NotFound")?;
            (p.state == VmState::Running, p.vcpus, p.mem_mib, p.vsock, p.tenant_id.clone(), p.image.clone(), p.image_sha256.clone(), p.disk_gb)
        };

        let parent_dir = self.instance_dir(parent_id).await.ok_or("InvalidId")?;
        let parent_sock = self.sock_path(parent_id).await.ok_or("InvalidId")?;
        // The child id comes from the request BODY, not a URL capture, so it
        // is validated here as well as at the handler.
        let child_dir = instance_path(&self.data_dir, child_id).ok_or("InvalidId")?;

        // 1) Ensure parent snapshot exists.
        let parent_state = {
            let g = self.inner.lock().await;
            g.vms.iter().find(|v| v.id == parent_id).map(|v| v.state.clone()).ok_or("NotFound")?
        };

        if parent_state != VmState::Sleeping {
            if parent_was_running {
                fc::patch_vm_state(&parent_sock, "Paused").await.map_err(|e| e.to_string())?;
            }
            let vmstate = format!("{}/vmstate.bin", parent_dir);
            let mem = format!("{}/mem.bin", parent_dir);
            fc::put_snapshot_create(&parent_sock, &vmstate, &mem).await
                .map_err(|e| e.to_string())?;
            if parent_was_running {
                fc::patch_vm_state(&parent_sock, "Resumed").await.map_err(|e| e.to_string())?;
            }
        }

        // 2) Create child instance dir; reflink rootfs + copy mem.bin.
        create_instance_dir(&child_dir).map_err(|e| e.to_string())?;

        let parent_rootfs = format!("{}/rootfs.ext4", parent_dir);
        let child_rootfs = format!("{}/rootfs.ext4", child_dir);
        copy_rootfs(&parent_rootfs, &child_rootfs)?;

        let parent_vmstate = format!("{}/vmstate.bin", parent_dir);
        let child_vmstate = format!("{}/vmstate.bin", child_dir);
        let parent_mem_file = format!("{}/mem.bin", parent_dir);
        let child_mem_file = format!("{}/mem.bin", child_dir);
        copy_rootfs(&parent_vmstate, &child_vmstate)?;
        copy_rootfs(&parent_mem_file, &child_mem_file)?;

        // 3) Register child record + claim slot.
        {
            let mut g = self.inner.lock().await;
            if g.vms.iter().any(|v| v.id == child_id) {
                return Err("AlreadyExists".into());
            }
            g.vms.push(Vm {
                id: child_id.to_string(),
                name: child_name.to_string(),
                dir_id: child_id.to_string(),
                vcpus: parent_vcpus,
                mem_mib: parent_mem_mib,
                state: VmState::Creating,
                pid: None,
                slot: None,
                ip: None,
                // The snapshot carries the parent's vsock device; the child
                // inherits it (rebound to its own UDS via vsock_override below).
                vsock: parent_vsock,
                // Child inherits parent's tenant_id.
                tenant_id: parent_tenant_id.clone(),
                // Exposes are NOT inherited: the parent's node ports keep
                // pointing at the parent; feluccad re-exposes the child.
                exposes: Vec::new(),
                // The child's rootfs is a reflink of the parent's — same
                // image and disk size for accounting (no image interaction).
                image: parent_image.clone(),
                image_sha256: parent_image_sha256.clone(),
                disk_gb: parent_disk_gb,
            });
        }

        let mut child_ip: Option<String> = None;
        let mut child_slot: Option<u32> = None;

        if self.net_on {
            let s = {
                let mut g = self.inner.lock().await;
                Self::claim_slot(&mut g).ok_or("IpPoolExhausted")?
            };
            child_slot = Some(s);
            child_ip = {
                let g = self.inner.lock().await;
                Self::ip_for_slot(&g, s)
            };
            // Pinned to the child's OWN MAC before its Firecracker is even
            // spawned, not after the in-guest re-MAC below: the clone restores
            // still carrying the parent's L2 identity, and a window in which it
            // can source frames under any MAC is exactly the hole. The pin
            // holds the clone's frames off the bridge until the guest takes the
            // MAC that matches it — and the re-MAC travels over vsock, not over
            // this tap, so it is unaffected.
            let (tap, mac) = tap_pin_for_slot(s);
            if !ensure_pinned_tap(&tap, child_ip.as_deref(), &mac, Unpinned::Remove) {
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
                drop(g);
                self.set_state(child_id, VmState::Error).await;
                return Err("TapSetupFailed".into());
            }
        }

        // 4) Spawn child FC and restore.
        let child_sock = format!("{}/fc.sock", child_dir);
        let _ = std::fs::remove_file(&child_sock);

        let log_path = format!("{}/serial.log", child_dir);
        let log_file = open_serial_log(&log_path)
            .map_err(|e| format!("open serial.log: {}", e))?;

        let log_fd = log_file.into_raw_fd();

        let child_proc = tokio::process::Command::new("firecracker")
            .arg("--api-sock").arg(&child_sock)
            .stdin(std::process::Stdio::null())
            .stdout(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .stderr(unsafe { std::process::Stdio::from_raw_fd(log_fd) })
            .kill_on_drop(false)
            .spawn()
            .map_err(|e| format!("spawn child firecracker: {}", e))?;

        let child_pid = child_proc.id().ok_or("NoPid")? as i32;
        {
            let mgr = Arc::clone(self);
            let rid = child_id.to_string();
            tokio::spawn(async move {
                let mut child_proc = child_proc;
                let _ = child_proc.wait().await;
                mgr.on_fc_exit(&rid, child_pid).await;
            });
        }

        let deadline = Instant::now() + Duration::from_millis(3000);
        loop {
            if Path::new(&child_sock).exists() { break; }
            if Instant::now() >= deadline {
                kill_and_reap(child_pid); // don't leak the half-spawned child FC
                self.set_state(child_id, VmState::Error).await;
                return Err("SocketNeverAppeared".into());
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
        if let Err(e) = fc::put_snapshot_load(&child_sock, &child_vmstate, &child_mem_file, self.net_on, child_slot, vsock_override).await {
            kill_and_reap(child_pid); // don't leak the child FC that failed to restore
            self.set_state(child_id, VmState::Error).await;
            return Err(e.to_string());
        }

        {
            let mut g = self.inner.lock().await;
            if let Some(v) = g.vms.iter_mut().find(|v| v.id == child_id) {
                v.pid = Some(child_pid);
                v.slot = child_slot;
                v.state = VmState::Running;
                v.ip = child_ip.clone();
                v.vsock = parent_vsock;
            }
        }
        self.write_meta(child_id).await?;

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
            // Pool VM is unusable (FC likely dead). Retag the record back,
            // mark it error, and let the caller fall through to a cold boot.
            warn!(pool_vm = %pooled_id, vm = %id, err = %e, "pool claim failed");
            let dead_pid = {
                let mut g = self.inner.lock().await;
                if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                    v.id = pooled_id.clone();
                    v.name = "pool".to_string();
                    v.tenant_id = None;
                    v.pid
                } else {
                    None
                }
            };
            if let Some(p) = dead_pid {
                self.on_fc_exit(&pooled_id, p).await;
            }
            return Ok(false);
        }
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await?;
        Ok(true)
    }

    // ---- stop / start / delete ----

    pub async fn stop(&self, id: &str) -> Result<(), String> {
        // Read the pid and clear the record in ONE critical section: the exit
        // reaper then sees a pid mismatch and treats this as a deliberate kill
        // (no error transition), and nothing can change the record between
        // the read and the write.
        let (pid, dir_id) = {
            let mut g = self.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == id).ok_or("NotFound")?;
            let out = (v.pid, v.dir_id.clone());
            v.state = VmState::Stopped;
            v.pid = None;
            out
        };
        if let Some(p) = pid {
            use nix::sys::signal::{kill, Signal};
            use nix::unistd::Pid;
            // Signal the pid only while it is still this instance's VMM. A
            // Firecracker adopted at start-up is not this agent's child, so
            // once it has exited its number is free for any other process.
            let procs = self.procs();
            if procs.identify(p, &dir_id) != Identity::Mismatch {
                match kill(Pid::from_raw(p), Signal::SIGTERM) {
                    Ok(_) | Err(nix::errno::Errno::ESRCH) => {}
                    Err(e) => return Err(e.to_string()),
                }
            } else if !procs.exiting(p) {
                // Gone long ago, or a stranger now has the number: there is
                // nothing of ours to wait for.
                return Ok(());
            }
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
        Ok(())
    }

    pub async fn start(self: &Arc<Self>, id: &str) -> Result<(), String> {
        let (vcpus, mem_mib, slot, ip) = {
            let g = self.inner.lock().await;
            // A capture stream is reading this instance's rootfs: spawning
            // an FC over it would corrupt the published template bytes.
            if g.capturing.contains(id) {
                return Err("InvalidState: capture in progress".into());
            }
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            // start is a cold boot: only valid when no FC owns the instance.
            // paused/sleeping VMs have live state to lose — spawning over them
            // would orphan the FC holding it (use resume/wake instead).
            match v.state {
                VmState::Running => return Ok(()),
                VmState::Stopped | VmState::Error => {}
                VmState::Paused => return Err("InvalidState: paused (use resume)".into()),
                VmState::Sleeping => return Err("InvalidState: sleeping (use wake)".into()),
                VmState::Creating | VmState::Pooled => {
                    return Err(format!("InvalidState: {}", v.state.as_str()));
                }
            }
            if let Some(p) = v.pid {
                use nix::sys::signal::kill;
                use nix::unistd::Pid;
                if kill(Pid::from_raw(p), None).is_ok() {
                    return Err(format!("InvalidState: process {} still attached", p));
                }
            }
            (v.vcpus, v.mem_mib, v.slot, v.ip.clone())
        };

        let dir_path = self.instance_dir(id).await.ok_or("InvalidId")?;
        let rootfs_dst = format!("{}/rootfs.ext4", dir_path);

        // Cold boot: configure_guest sets the NIC's MAC, so the full pin
        // (address, ARP and L2) applies from the first frame.
        if self.net_on {
            if let Some(s) = slot {
                let (tap, mac) = tap_pin_for_slot(s);
                if !ensure_pinned_tap(&tap, ip.as_deref(), &mac, Unpinned::Remove) {
                    return Err("TapSetupFailed".into());
                }
            }
        }

        let (pid, vsock_on) = self.spawn_and_configure(id, &dir_path, &rootfs_dst, vcpus, mem_mib, slot, ip.as_deref()).await?;
        self.set_pid(id, Some(pid)).await;
        {
            let mut g = self.inner.lock().await;
            if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
                v.vsock = vsock_on;
            }
        }
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await
    }

    pub async fn delete(&self, id: &str) -> Result<(), String> {
        // Capture slot before removal.
        let (slot, lives_there) = {
            let g = self.inner.lock().await;
            let slot = g.vms.iter().find(|v| v.id == id).and_then(|v| v.slot);
            // No record has this id, yet a VM lives in the directory of that
            // name: a pool VM that was claimed keeps its pool directory under
            // its new id.
            let lives_there = if g.vms.iter().any(|v| v.id == id) {
                None
            } else {
                g.vms.iter().find(|v| v.dir_id == id).map(|v| v.id.clone())
            };
            (slot, lives_there)
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

        // stop is best-effort (idempotent delete).
        let _ = self.stop(id).await;

        if self.net_on {
            if let Some(s) = slot {
                let tap = net::tap_name(s);
                // Before the tap goes: the pin is keyed on the device name,
                // which the next VM to claim this slot will reuse.
                net::clear_tap_antispoof(&tap);
                net::delete_tap(&tap);
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
            }
        }

        // An id that is not a plain path component never reaches remove_dir_all:
        // delete does not require the VM to exist, so this is the one call in
        // the manager that would recursively delete an attacker-chosen path.
        match self.instance_dir(id).await {
            Some(dir_path) => {
                if let Err(e) = std::fs::remove_dir_all(&dir_path) {
                    warn!(path = %dir_path, err = %e, "remove_dir_all failed");
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
        // Teardown does not fail on a broken ruleset: the VM is already gone,
        // and returning an error here would only make delete non-idempotent.
        if !self.refresh_isolation().await {
            warn!(vm = %id, "cross-tenant isolation not in force after delete");
        }
        // The VM's exposes die with it: rebuild from the remaining members.
        self.refresh_ingress().await;
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

    async fn set_pid(&self, id: &str, pid: Option<i32>) {
        let mut g = self.inner.lock().await;
        if let Some(v) = g.vms.iter_mut().find(|v| v.id == id) {
            v.pid = pid;
        }
    }

    /// React to an FC process exit (reaper task or liveness sweep). Acts ONLY
    /// when `pid` is still the recorded pid and the state implies a live
    /// process; deliberate kills (sleep/stop/delete) clear the pid under the
    /// lock before signalling, so they no-op here.
    async fn on_fc_exit(&self, id: &str, pid: i32) {
        let transitioned = {
            let mut g = self.inner.lock().await;
            // A dead pool VM leaves the claimable set by this very transition:
            // claims match on state == Pooled, and Error is not claimable.
            match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.pid == Some(pid)
                    && matches!(v.state, VmState::Creating | VmState::Running | VmState::Paused | VmState::Pooled) =>
                {
                    v.state = VmState::Error;
                    v.pid = None;
                    true
                }
                _ => false,
            }
        };
        if transitioned {
            warn!(vm = %id, pid = pid, "firecracker exited unexpectedly; state -> error");
            // Best-effort: the instance dir may be mid-delete.
            let _ = self.write_meta(id).await;
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
    /// The file is replaced, never rewritten in place: an agent killed
    /// mid-write used to leave a truncated meta.json, and a VM whose record
    /// does not load is a VM start-up can no longer manage.
    async fn write_meta(&self, id: &str) -> Result<(), String> {
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
            }
        };

        let dir_path = instance_path(&self.data_dir, &meta.dir_id).ok_or("InvalidId")?;
        let meta_path = format!("{}/meta.json", dir_path);
        let tmp_path = format!("{}/meta.json.tmp", dir_path);
        let json = meta.to_json();
        std::fs::write(&tmp_path, json.as_bytes())
            .map_err(|e| format!("write meta {}: {}", tmp_path, e))?;
        std::fs::rename(&tmp_path, &meta_path)
            .map_err(|e| format!("replace meta {}: {}", meta_path, e))?;
        Ok(())
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
) -> bool {
    if !net::ensure_tap(tap) {
        return false;
    }
    // No allocated address means nothing to pin the port to, so the guest
    // behind it could claim any.
    let Some(ip) = ip else {
        warn!(tap = %tap, "tap has no allocated address to pin");
        match unpinned {
            Unpinned::Remove => net::delete_tap(tap),
            Unpinned::TakeDown => net::set_link_down(tap),
        }
        return false;
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
        return false;
    }
    true
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

    #[test]
    fn test_node_port_alloc_lowest_free() {
        let used = HashSet::new();
        assert_eq!(lowest_free_node_port(&used), Some(20000));
    }

    #[test]
    fn test_node_port_alloc_skips_taken_across_vms() {
        // `used` spans every expose of every VM — holes are filled lowest-first.
        let used: HashSet<u16> = [20000, 20001, 20003].into_iter().collect();
        assert_eq!(lowest_free_node_port(&used), Some(20002));
        let used: HashSet<u16> = (20000..=20999).collect();
        assert_eq!(lowest_free_node_port(&used), Some(21000));
    }

    #[test]
    fn test_node_port_alloc_exhaustion() {
        let mut used: HashSet<u16> = (NODE_PORT_MIN..=NODE_PORT_MAX).collect();
        assert_eq!(lowest_free_node_port(&used), None);
        // Freeing any one port makes exactly that port allocatable again.
        used.remove(&25555);
        assert_eq!(lowest_free_node_port(&used), Some(25555));
    }

    #[test]
    fn test_node_port_alloc_ignores_out_of_range_used() {
        // Ports outside the range never block allocation (can't occur from our
        // allocator, but the set is just u16s).
        let used: HashSet<u16> = [80, 8069, 30000].into_iter().collect();
        assert_eq!(lowest_free_node_port(&used), Some(20000));
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
        let plan = plan_sweep(&found, &owned, &name_set(&["vm-a", "pool-1"]), &[], &[], |_| true);
        assert!(plan.is_empty(), "{plan:?}");
    }

    #[test]
    fn test_plan_sweep_kills_only_what_no_caller_was_told_exists() {
        let found = vec![
            fcp(10, "vm-a"),     // adopted
            fcp(12, "vm-a"),     // a second VMM on the same directory
            fcp(20, "vm-half"),  // create that never wrote meta.json
            fcp(30, "vm-sleep"), // wake that never committed: the record still says sleeping
        ];
        let owned: HashSet<i32> = [10].into_iter().collect();
        let plan = plan_sweep(
            &found,
            &owned,
            &name_set(&["vm-a", "vm-sleep"]),
            &[],
            &names(&["vm-half", "vm-empty"]),
            |_| true,
        );
        assert_eq!(
            plan,
            vec![
                kill(12, "vm-a"),
                kill(20, "vm-half"),
                kill(30, "vm-sleep"),
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
        let plan = plan_sweep(&found, &HashSet::new(), &HashSet::new(), &[], &[], |_| true);
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
        std::fs::write(format!("{}/vm-sleep/meta.json", inst), meta_line("vm-sleep", None, "sleeping")).unwrap();
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
        let mut bystander = std::process::Command::new("sleep").arg("300").spawn().expect("spawn sleep");
        // Past exec, so it is positively "some other program" — which is what
        // a recycled pid looks like in practice.
        wait_for_argv0(bystander.id(), "sleep");

        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", bystander.id() as i32));
        mgr.stop("vm-1").await.unwrap();

        std::thread::sleep(std::time::Duration::from_millis(200));
        let survived = bystander.try_wait().unwrap().is_none();
        let _ = bystander.kill();
        let _ = bystander.wait();
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

        let mgr = mgr_for(data_dir);
        mgr.inner.lock().await.vms.push(running_vm("vm-1", vmm.pid()));
        mgr.stop("vm-1").await.unwrap();
        assert!(!vmm.alive(), "stop() must still terminate the instance's own Firecracker");
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
}
