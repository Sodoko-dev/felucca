//! Firecracker microVM lifecycle manager for hearth-agent (v2).
//!
//! Port of backend/src/agent/vm.zig — Manager struct, all lifecycle methods.
//! Data layout: {data_dir}/instances/{dir_id}/ with rootfs.ext4, fc.sock,
//! serial.log, meta.json, vmstate.bin, mem.bin.

pub mod meta;
pub mod pool;
pub mod reconcile;

use crate::fc;
use crate::ipalloc::{Allocator, Cidr};
use crate::net;
use meta::{Meta, VmState};
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

struct Inner {
    vms: Vec<Vm>,
    allocator: Option<Allocator>,
    /// ids of parked pool VMs (ordered queue).
    pool: Vec<String>,
}

pub struct Manager {
    data_dir: String,
    net_on: bool,
    cidr: Cidr,
    pool_target: u32,
    inner: Mutex<Inner>,
}

impl Manager {
    pub fn new(data_dir: String, net_on: bool, cidr: Cidr, pool_target: u32) -> Arc<Self> {
        Arc::new(Manager {
            data_dir,
            net_on,
            cidr,
            pool_target,
            inner: Mutex::new(Inner {
                vms: Vec::new(),
                allocator: None,
                pool: Vec::new(),
            }),
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

    pub async fn live_count(&self) -> u32 {
        let g = self.inner.lock().await;
        g.vms.iter().filter(|v| v.state == VmState::Running || v.state == VmState::Paused).count() as u32
    }

    pub async fn pool_count(&self) -> u32 {
        let g = self.inner.lock().await;
        g.pool.len() as u32
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

    pub async fn create(self: &Arc<Self>, id: &str, name: &str, vcpus: u32, mem_mib: u64, tenant_id: Option<String>) -> Result<(), String> {
        // Try to claim a warm-pool VM for a matching shape (1 vCPU / 256 MiB).
        if self.pool_target > 0 && vcpus == 1 && mem_mib == 256 {
            if self.claim_from_pool(id, name, tenant_id.clone()).await? { return Ok(()); }
        }
        self.cold_create(id, name, vcpus, mem_mib, VmState::Running, None, tenant_id).await
    }

    /// Cold-boot a fresh VM. `force_slot` reuses a specific slot (pool refill).
    async fn cold_create(
        self: &Arc<Self>,
        id: &str,
        name: &str,
        vcpus: u32,
        mem_mib: u64,
        final_state: VmState,
        force_slot: Option<u32>,
        tenant_id: Option<String>,
    ) -> Result<(), String> {
        {
            let mut g = self.inner.lock().await;
            if g.vms.iter().any(|v| v.id == id) {
                return Err("AlreadyExists".into());
            }
            g.vms.push(Vm {
                id: id.to_string(),
                name: name.to_string(),
                dir_id: id.to_string(),
                vcpus,
                mem_mib,
                state: VmState::Creating,
                pid: None,
                slot: None,
                ip: None,
                vsock: false,
                tenant_id: tenant_id.clone(),
            });
        }

        // On error, mark as error state.
        let result = self.cold_create_inner(id, name, vcpus, mem_mib, final_state, force_slot).await;
        if result.is_err() {
            self.set_state(id, VmState::Error).await;
        }
        result
    }

    async fn cold_create_inner(
        self: &Arc<Self>,
        id: &str,
        _name: &str,
        vcpus: u32,
        mem_mib: u64,
        final_state: VmState,
        force_slot: Option<u32>,
    ) -> Result<(), String> {
        let dir_path = self.instance_dir(id).await;
        std::fs::create_dir_all(&dir_path).map_err(|e| e.to_string())?;

        let rootfs_dst = format!("{}/rootfs.ext4", dir_path);
        let rootfs_src = format!("{}/images/ubuntu-base.ext4", self.data_dir);
        copy_rootfs(&rootfs_src, &rootfs_dst)?;

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
        let (parent_was_running, parent_vcpus, parent_mem_mib, parent_vsock, parent_tenant_id) = {
            let g = self.inner.lock().await;
            let p = g.vms.iter().find(|v| v.id == parent_id).ok_or("NotFound")?;
            (p.state == VmState::Running, p.vcpus, p.mem_mib, p.vsock, p.tenant_id.clone())
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

    // ---- warm pool ----

    pub async fn refill_pool(self: &Arc<Self>) {
        if self.pool_target == 0 { return; }
        loop {
            let count = self.pool_count().await;
            if count >= self.pool_target { break; }

            let id = format!("pool-{:x}", now_ms());
            match self.cold_create(&id, "pool", 1, 256, VmState::Pooled, None, None).await {
                Ok(_) => {
                    let mut g = self.inner.lock().await;
                    g.pool.push(id.clone());
                }
                Err(e) => {
                    eprintln!("warn: pool prewarm {} failed: {}", id, e);
                    break;
                }
            }
        }
    }

    /// Claim a parked pool VM and retag it to the real id/name. Returns true if claimed.
    async fn claim_from_pool(&self, id: &str, name: &str, tenant_id: Option<String>) -> Result<bool, String> {
        let pooled_id = {
            let mut g = self.inner.lock().await;
            if g.pool.is_empty() { return Ok(false); }
            g.pool.remove(0)
        };

        // Retag the in-memory record to the real id/name but KEEP dir_id (the pool dir).
        // Also set tenant_id at claim time.
        {
            let mut g = self.inner.lock().await;
            if let Some(v) = g.vms.iter_mut().find(|v| v.id == pooled_id) {
                v.id = id.to_string();
                v.name = name.to_string();
                v.tenant_id = tenant_id;
                // dir_id already points at the pool dir; leave it.
            } else {
                return Ok(false);
            }
        }

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
        }
        Ok(())
    }

    pub async fn start(self: &Arc<Self>, id: &str) -> Result<(), String> {
        let (vcpus, mem_mib, slot, ip) = {
            let g = self.inner.lock().await;
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

        let mut g = self.inner.lock().await;
        if let Some(pos) = g.vms.iter().position(|v| v.id == id) {
            g.vms.remove(pos);
        }
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
            let hit = match g.vms.iter_mut().find(|v| v.id == id) {
                Some(v) if v.pid == Some(pid)
                    && matches!(v.state, VmState::Creating | VmState::Running | VmState::Paused | VmState::Pooled) =>
                {
                    v.state = VmState::Error;
                    v.pid = None;
                    true
                }
                _ => false,
            };
            if hit {
                // A dead pool VM must leave the queue or it would be claimed.
                g.pool.retain(|p| p != id);
            }
            hit
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
