//! Firecracker microVM lifecycle manager for hearth-agent (v2).
//!
//! Port of backend/src/agent/vm.zig — Manager struct, all lifecycle methods.
//! Data layout: {data_dir}/instances/{dir_id}/ with rootfs.ext4, fc.sock,
//! serial.log, meta.json, vmstate.bin, mem.bin.

pub mod image;
pub mod meta;
pub mod pool;
pub mod reconcile;

use crate::fc;
use crate::ipalloc::{Allocator, Cidr};
use crate::net;
use meta::{ExposeEntry, Meta, VmState};
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

/// One hearthd-managed warm-pool template (v4 P4). `image_sha256` may be
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
    Ok(tokio::net::UnixStream),
    NotFound,
    NotRunning,
    Unavailable,
    Failed(String),
}

struct Inner {
    vms: Vec<Vm>,
    allocator: Option<Allocator>,
    /// v4 P4: hearthd-managed warm-pool templates (set via PUT /v1/pools or
    /// the register response). The legacy config pool (pool_target) is NOT in
    /// this list — it is a separate always-present default spec.
    template_pools: Vec<PoolSpec>,
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
                capturing: std::collections::HashSet::new(),
            }),
            ruleset_lock: Mutex::new(()),
            image_locks: Mutex::new(std::collections::HashMap::new()),
            pool_backoff: Mutex::new(std::collections::HashMap::new()),
        })
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
            eprintln!("warn: bridge setup failed; guests may lack networking");
        }
    }

    /// Reconcile persisted instances from data_dir/instances/*.
    pub async fn reconcile(&self) {
        let mut g = self.inner.lock().await;
        let vms = {
            let alloc_opt = g.allocator.as_mut();
            reconcile::reconcile(&self.data_dir, alloc_opt)
        };
        g.vms = vms;
    }

    /// Rebuild the cross-tenant isolation ruleset from the current VM list
    /// (every VM holding an IP, keyed by tenant). Snapshots membership under
    /// the state lock, releases it, then shells out to nft. Rebuilds are
    /// serialized so concurrent create/fork/delete can't interleave nft
    /// commands; each rebuild snapshots at its start, so the last one to run
    /// leaves the freshest state.
    pub async fn refresh_isolation(&self) {
        if !self.net_on { return; }
        let _guard = self.ruleset_lock.lock().await;
        let members: Vec<(String, String)> = {
            let g = self.inner.lock().await;
            g.vms.iter()
                .filter_map(|v| {
                    v.ip.clone().map(|ip| (v.tenant_id.clone().unwrap_or_default(), ip))
                })
                .collect()
        };
        net::rebuild_isolation(&members);
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
            eprintln!("warn: expose persist for {} failed: {}", id, e);
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
                eprintln!("warn: unexpose persist for {} failed: {}", id, e);
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

    /// Replace the hearthd-managed template pools (PUT /v1/pools / register
    /// response). Applied atomically or not at all: every spec is validated
    /// before any state changes. The legacy config pool (pool_target) is
    /// unaffected. The 5s refill loop picks the new set up on its next tick.
    pub async fn set_pools(&self, specs: Vec<PoolSpec>) -> Result<(), String> {
        for s in &specs {
            validate_pool_spec(s)?;
        }
        let mut g = self.inner.lock().await;
        g.template_pools = specs;
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
                Some(ip) => { out.push('"'); out.push_str(ip); out.push('"'); }
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
        let path = format!("{}/instances/{}/rootfs.ext4", self.data_dir, v.dir_id);
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

    fn dir_id_for_sync(vms: &[Vm], id: &str) -> String {
        vms.iter().find(|v| v.id == id).map(|v| v.dir_id.clone()).unwrap_or_else(|| id.to_string())
    }

    fn instance_dir_sync(vms: &[Vm], data_dir: &str, id: &str) -> String {
        let dir_id = Self::dir_id_for_sync(vms, id);
        format!("{}/instances/{}", data_dir, dir_id)
    }

    fn sock_path_sync(vms: &[Vm], data_dir: &str, id: &str) -> String {
        let dir_id = Self::dir_id_for_sync(vms, id);
        format!("{}/instances/{}/fc.sock", data_dir, dir_id)
    }

    async fn instance_dir(&self, id: &str) -> String {
        let g = self.inner.lock().await;
        Self::instance_dir_sync(&g.vms, &self.data_dir, id)
    }

    async fn sock_path(&self, id: &str) -> String {
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
        // Try to claim a warm-pool VM of the exact requested shape
        // (image + vcpus + mem + disk); else cold boot.
        if self.claim_from_pool(spec).await? {
            self.refresh_isolation().await;
            return Ok(());
        }
        self.cold_create(spec, VmState::Running, None).await?;
        self.refresh_isolation().await;
        Ok(())
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
        let dir_path = self.instance_dir(id).await;
        std::fs::create_dir_all(&dir_path).map_err(|e| e.to_string())?;

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
            let tap = net::tap_name(s);
            if !net::ensure_tap(&tap) {
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
                return Err("TapSetupFailed".into());
            }
            let g = self.inner.lock().await;
            ip_str = Self::ip_for_slot(&g, s);
        }

        let (pid, vsock_on) = self.spawn_and_configure(id, &dir_path, &rootfs_dst, vcpus, mem_mib, slot, ip_str.as_deref()).await?;

        // For pool/paused final state: pause now.
        if final_state == VmState::Pooled || final_state == VmState::Paused {
            let sock = self.sock_path(id).await;
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
        eprintln!("info: pulling image {} from control plane", image_name);
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
        eprintln!("info: image {} cached ({})", image_name, want_sha);
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
    /// are covered) or of any hearthd-pushed pool spec; `DEFAULT_IMAGE` is
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
                eprintln!("info: image gc: removed {} (unreferenced)", fname);
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
        let sock = self.sock_path(id).await;
        // Remove stale socket.
        let _ = std::fs::remove_file(&sock);

        let log_path = format!("{}/serial.log", dir_path);
        let log_file = std::fs::OpenOptions::new()
            .write(true).create(true).truncate(true)
            .open(&log_path)
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

    /// True when `{data_dir}/images/.hearth-guest-v1` exists: the rootfs has the
    /// guest agent baked in, so cold boots should attach a vsock device.
    fn guest_agent_marker_present(&self) -> bool {
        let marker = format!("{}/images/.hearth-guest-v1", self.data_dir);
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
                let tap = net::tap_name(s);
                fc::put_network_interface(sock, &tap).await
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
        let sock = self.sock_path(id).await;
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
        let sock = self.sock_path(id).await;
        fc::patch_vm_state(&sock, "Resumed").await.map_err(|e| e.to_string())?;
        self.set_state(id, VmState::Running).await;
        self.write_meta(id).await
    }

    // ---- sleep / wake ----

    pub async fn sleep_vm(&self, id: &str) -> Result<(), String> {
        let pid = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            if v.state == VmState::Sleeping { return Ok(()); }
            v.pid
        };

        let sock = self.sock_path(id).await;
        let dir_path = self.instance_dir(id).await;

        fc::patch_vm_state(&sock, "Paused").await.map_err(|e| e.to_string())?;

        let vmstate = format!("{}/vmstate.bin", dir_path);
        let mem = format!("{}/mem.bin", dir_path);
        fc::put_snapshot_create(&sock, &vmstate, &mem).await
            .map_err(|e| e.to_string())?;

        // Clear the pid BEFORE killing so the exit reaper sees a pid mismatch
        // and treats this as a deliberate kill (no error transition).
        self.set_pid(id, None).await;
        if let Some(p) = pid {
            kill_and_reap(p);
        }

        self.set_state(id, VmState::Sleeping).await;
        self.write_meta(id).await
    }

    /// Spawn firecracker → snapshot/load. Returns wake latency in ms.
    pub async fn wake_vm(self: &Arc<Self>, id: &str) -> Result<u64, String> {
        let slot = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            if v.state == VmState::Running { return Ok(0); }
            v.slot
        };

        let start = Instant::now();
        let dir_path = self.instance_dir(id).await;

        // Re-assert tap exists (idempotent).
        if self.net_on {
            if let Some(s) = slot {
                let tap = net::tap_name(s);
                net::ensure_tap(&tap);
            }
        }

        let sock = self.sock_path(id).await;
        let _ = std::fs::remove_file(&sock);
        // A SIGKILLed FC (sleep) leaves the vsock host UDS behind; the restore
        // re-binds the snapshot-baked path and fails with EADDRINUSE if the
        // stale file is still there.
        let _ = std::fs::remove_file(format!("{}/v.sock", dir_path));

        let log_path = format!("{}/serial.log", dir_path);
        let log_file = std::fs::OpenOptions::new()
            .write(true).create(true).truncate(true)
            .open(&log_path)
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
        let (parent_was_running, parent_vcpus, parent_mem_mib, parent_vsock, parent_tenant_id, parent_image, parent_image_sha256, parent_disk_gb) = {
            let g = self.inner.lock().await;
            let p = g.vms.iter().find(|v| v.id == parent_id).ok_or("NotFound")?;
            (p.state == VmState::Running, p.vcpus, p.mem_mib, p.vsock, p.tenant_id.clone(), p.image.clone(), p.image_sha256.clone(), p.disk_gb)
        };

        let parent_dir = self.instance_dir(parent_id).await;
        let parent_sock = self.sock_path(parent_id).await;

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
        let child_dir = format!("{}/instances/{}", self.data_dir, child_id);
        std::fs::create_dir_all(&child_dir).map_err(|e| e.to_string())?;

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
                // pointing at the parent; hearthd re-exposes the child.
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
            let tap = net::tap_name(s);
            if !net::ensure_tap(&tap) {
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
                self.set_state(child_id, VmState::Error).await;
                return Err("TapSetupFailed".into());
            }
            let g = self.inner.lock().await;
            child_ip = Self::ip_for_slot(&g, s);
        }

        // 4) Spawn child FC and restore.
        let child_sock = format!("{}/instances/{}/fc.sock", self.data_dir, child_id);
        let _ = std::fs::remove_file(&child_sock);

        let log_path = format!("{}/serial.log", child_dir);
        let log_file = std::fs::OpenOptions::new()
            .write(true).create(true).truncate(true)
            .open(&log_path)
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
        // own allocated IP. Best-effort per contract: a failure only warns.
        if parent_vsock {
            if let (Some(ip), true) = (child_ip.as_deref(), self.net_on) {
                // The child is a memory-clone, so it also inherits the parent's
                // guest MAC. Two ports with one MAC make the bridge FDB flap
                // and one guest goes dark — give the child its own
                // locally-administered MAC BEFORE re-IPing (plain exec; no
                // guest-agent contract change needed).
                let mac = child_mac(child_slot, now_ms());
                let cmd: Vec<String> = ["ip", "link", "set", "dev", "eth0", "address", mac.as_str()]
                    .iter().map(|s| s.to_string()).collect();
                match crate::guestclient::exec(&child_dir, &cmd, 5_000).await {
                    Ok(v) if v.get("exit_code").and_then(|c| c.as_i64()) == Some(0) => {}
                    Ok(v) => eprintln!("warn: fork re-MAC for {} failed (best-effort): {}", child_id, v),
                    Err(e) => eprintln!("warn: fork re-MAC for {} failed (best-effort): {}", child_id, e),
                }
                let gw = crate::ipalloc::fmt_ip(self.cidr.gateway());
                match crate::guestclient::set_ip(&child_dir, ip, self.cidr.prefix, &gw).await {
                    Ok(()) => {}
                    Err(e) => eprintln!("warn: fork re-IP for {} failed (best-effort): {}", child_id, e),
                }
            }
        }
        // The child's allocated (post-re-IP) address must join its tenant's
        // pair set before the fork returns.
        self.refresh_isolation().await;
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
        let dir = format!("{}/instances/{}", self.data_dir, dir_id);
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
        let dir = format!("{}/instances/{}", self.data_dir, dir_id);
        match crate::guestclient::exec_stream(&dir, cmd, timeout_ms).await {
            Ok(s) => ExecStreamOutcome::Ok(s),
            Err(e) => ExecStreamOutcome::Failed(e.0),
        }
    }

    // ---- warm pool ----

    /// The full refill plan: the legacy config pool (always present when
    /// pool_target > 0, exactly the pre-P4 shape) plus the hearthd-managed
    /// template pools.
    async fn pool_specs(&self) -> Vec<PoolSpec> {
        let mut specs = Vec::new();
        if self.pool_target > 0 {
            specs.push(PoolSpec {
                image: DEFAULT_IMAGE.to_string(),
                image_sha256: String::new(),
                vcpus: 1,
                mem_mib: 256,
                disk_gb: 0,
                count: self.pool_target,
            });
        }
        let g = self.inner.lock().await;
        specs.extend(g.template_pools.iter().cloned());
        specs
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
        let surplus = {
            let g = self.inner.lock().await;
            surplus_pooled_ids(&g.vms, &specs)
        };
        for sid in surplus {
            eprintln!("info: draining surplus pool vm {}", sid);
            if let Err(e) = self.delete(&sid).await {
                eprintln!("warn: pool drain {}: {}", sid, e);
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
                    eprintln!("warn: pool prewarm {} ({}) failed: {}", id, spec.image, e);
                    // Remove the failed record + instance dir (each tick
                    // would otherwise leak one Error VM) and back the spec
                    // off so a broken sha doesn't retry every 5s.
                    if let Err(de) = self.delete(&id).await {
                        eprintln!("warn: pool prewarm cleanup {}: {}", id, de);
                    }
                    let mut bo = self.pool_backoff.lock().await;
                    bo.insert(spec_key(&spec), Instant::now() + REFILL_BACKOFF);
                    break;
                }
            }
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

        let sock = self.sock_path(id).await;
        if let Err(e) = fc::patch_vm_state(&sock, "Resumed").await {
            // Pool VM is unusable (FC likely dead). Retag the record back,
            // mark it error, and let the caller fall through to a cold boot.
            eprintln!("warn: pool claim of {} for {} failed: {}", pooled_id, id, e);
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
        let pid = {
            let g = self.inner.lock().await;
            let v = g.vms.iter().find(|v| v.id == id).ok_or("NotFound")?;
            v.pid
        };
        // Clear the record BEFORE signalling so the exit reaper sees a pid
        // mismatch and treats this as a deliberate kill (no error transition).
        self.set_state(id, VmState::Stopped).await;
        self.set_pid(id, None).await;
        if let Some(p) = pid {
            use nix::sys::signal::{kill, Signal};
            use nix::unistd::Pid;
            match kill(Pid::from_raw(p), Signal::SIGTERM) {
                Ok(_) | Err(nix::errno::Errno::ESRCH) => {}
                Err(e) => return Err(e.to_string()),
            }
            // Wait for the FC process to actually exit: "stopped" gates the
            // rootfs capture endpoint, and a dying FC may still be flushing
            // rootfs.ext4. Escalate to SIGKILL at 5s, give up at 10s (the
            // reaper owns whatever is left).
            let start = Instant::now();
            let mut killed = false;
            loop {
                if kill(Pid::from_raw(p), None).is_err() {
                    break; // ESRCH — gone (reaper collected it)
                }
                let waited = start.elapsed();
                if !killed && waited >= Duration::from_secs(5) {
                    let _ = kill(Pid::from_raw(p), Signal::SIGKILL);
                    killed = true;
                }
                if waited >= Duration::from_secs(10) {
                    eprintln!("warn: stop {}: pid {} still present after 10s", id, p);
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

        let dir_path = self.instance_dir(id).await;
        let rootfs_dst = format!("{}/rootfs.ext4", dir_path);

        if self.net_on {
            if let Some(s) = slot {
                let tap = net::tap_name(s);
                net::ensure_tap(&tap);
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
        let slot = {
            let g = self.inner.lock().await;
            g.vms.iter().find(|v| v.id == id).and_then(|v| v.slot)
        };

        // stop is best-effort (idempotent delete).
        let _ = self.stop(id).await;

        if self.net_on {
            if let Some(s) = slot {
                let tap = net::tap_name(s);
                net::delete_tap(&tap);
                let mut g = self.inner.lock().await;
                Self::free_slot(&mut g, s);
            }
        }

        let dir_path = self.instance_dir(id).await;
        if let Err(e) = std::fs::remove_dir_all(&dir_path) {
            eprintln!("warn: remove_dir_all {}: {}", dir_path, e);
        }

        {
            let mut g = self.inner.lock().await;
            if let Some(pos) = g.vms.iter().position(|v| v.id == id) {
                g.vms.remove(pos);
            }
        }
        self.refresh_isolation().await;
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
            eprintln!("warn: firecracker for {} (pid {}) exited unexpectedly; state -> error", id, pid);
            // Best-effort: the instance dir may be mid-delete.
            let _ = self.write_meta(id).await;
        }
    }

    /// Mark VMs whose FC process is gone (ESRCH) as error. Covers FCs adopted
    /// after an agent restart, which have no in-process reaper task.
    pub async fn sweep_dead(&self) {
        let candidates: Vec<(String, i32)> = {
            let g = self.inner.lock().await;
            g.vms.iter()
                .filter(|v| matches!(v.state, VmState::Creating | VmState::Running | VmState::Paused | VmState::Pooled))
                .filter_map(|v| v.pid.map(|p| (v.id.clone(), p)))
                .collect()
        };
        for (id, pid) in candidates {
            use nix::sys::signal::kill;
            use nix::unistd::Pid;
            if kill(Pid::from_raw(pid), None) == Err(nix::errno::Errno::ESRCH) {
                self.on_fc_exit(&id, pid).await;
            }
        }
    }

    /// Write the current in-memory record for `id` to its meta.json.
    async fn write_meta(&self, id: &str) -> Result<(), String> {
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

        let dir_id = meta.dir_id.clone();
        let dir_path = format!("{}/instances/{}", self.data_dir, dir_id);
        let meta_path = format!("{}/meta.json", dir_path);
        let json = meta.to_json();
        std::fs::write(&meta_path, json.as_bytes())
            .map_err(|e| format!("write meta {}: {}", meta_path, e))?;
        Ok(())
    }
}

// ---- utilities ----

fn push_kv_str(out: &mut String, key: &str, val: &str) {
    out.push('"');
    out.push_str(key);
    out.push_str("\":\"");
    out.push_str(val);
    out.push('"');
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

/// Locally-administered unicast MAC for a fork child (0a:68:…): three salt
/// bytes from the clock plus the tap slot, unique enough per bridge.
fn child_mac(slot: Option<u32>, salt: u64) -> String {
    format!(
        "0a:68:{:02x}:{:02x}:{:02x}:{:02x}",
        (salt >> 16) as u8,
        (salt >> 8) as u8,
        salt as u8,
        slot.unwrap_or(0) as u8
    )
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
        // The pinned two-sided contract: hearthd sends this exact shape.
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
}
