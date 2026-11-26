//! Startup reconciliation: rescan instances/*/meta.json and rebuild in-memory state.
//!
//! Port of Manager.reconcile / Manager.reconcileOne in backend/src/agent/vm.zig,
//! with two deliberate departures (ADR-0011). Firecrackers now outlive the
//! agent, so adopting them is the normal path at every start rather than a
//! rarity:
//!
//! * whether a record still has its VMM is decided by what the recorded pid
//!   *is* (see `procscan`), not by whether some process has that number;
//! * a pooled VM whose VMM is alive is adopted as pooled. It used to be
//!   written off as `stopped` with its pid dropped, which was harmless while
//!   every restart killed it anyway and leaks one live Firecracker per restart
//!   once it does not.

use super::procscan::{Identity, ProcTable};
use super::{valid_vm_id, Vm, VmState};
use crate::ipalloc::Allocator;
use crate::vm::meta::Meta;
use std::path::Path;
use tracing::{error, warn};

/// What start-up found under `instances/`.
#[derive(Debug, Default)]
pub struct Reconciled {
    pub vms: Vec<Vm>,
    /// Directories that have a `meta.json` which did not yield a record (it
    /// does not parse, or names an id/dir this agent refuses). Nothing in them
    /// is touched: if a Firecracker is running there it is most likely a
    /// healthy tenant VM whose record got damaged, and that is not a reason to
    /// kill it.
    pub unreadable: Vec<String>,
    /// Directories with no `meta.json` at all. `meta.json` is written last, so
    /// this is a create, fork or pool prewarm that never completed — no caller
    /// was ever told the VM exists.
    pub metaless: Vec<String>,
    /// Ids of pool VMs that were never claimed and have no VMM: `pooled` on
    /// disk with the Firecracker gone (a host reboot, or it died while the
    /// agent was down — these are in `vms` as `stopped`), or already `error`
    /// or `stopped` on disk (it died under an agent that did not get to
    /// remove it). A pool VM without its VMM is of no use to anyone and was
    /// never handed to a tenant: the caller removes it, or it keeps its slot,
    /// its tap and a rootfs copy forever.
    pub dead_pool: Vec<String>,
}

enum Loaded {
    /// A record, and whether it is a pool VM that lost its VMM.
    Vm(Vm, bool),
    Unreadable,
    Metaless,
}

/// Scan data_dir/instances/ and return what is there.
/// Also calls `alloc.reserve(slot)` for every record that has a slot set.
pub fn reconcile(data_dir: &str, mut alloc: Option<&mut Allocator>, procs: &ProcTable) -> Reconciled {
    let mut out = Reconciled::default();
    let instances_path = format!("{}/instances", data_dir);
    let dir = match std::fs::read_dir(&instances_path) {
        Ok(d) => d,
        Err(_) => return out,
    };

    for entry in dir.flatten() {
        let ft = match entry.file_type() {
            Ok(t) => t,
            Err(_) => continue,
        };
        if !ft.is_dir() { continue; }
        let name = entry.file_name();
        let dir_name = name.to_string_lossy();
        // Only directories this agent could have created are its business:
        // a `lost+found`, or anything else that is not a valid instance id,
        // is neither loaded nor cleaned up.
        if !valid_vm_id(&dir_name) {
            continue;
        }
        match reconcile_one(data_dir, &dir_name, alloc.as_deref_mut(), procs) {
            Loaded::Vm(vm, dead_pool) => {
                if dead_pool {
                    out.dead_pool.push(vm.id.clone());
                }
                out.vms.push(vm);
            }
            Loaded::Unreadable => {
                warn!(dir = %dir_name, "reconcile: skipped (meta.json does not load)");
                out.unreadable.push(dir_name.into_owned());
            }
            Loaded::Metaless => out.metaless.push(dir_name.into_owned()),
        }
    }
    out
}

fn reconcile_one(
    data_dir: &str,
    dir_name: &str,
    alloc: Option<&mut Allocator>,
    procs: &ProcTable,
) -> Loaded {
    let meta_path = format!("{}/instances/{}/meta.json", data_dir, dir_name);
    let data = match std::fs::read_to_string(&meta_path) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Loaded::Metaless,
        Err(_) => return Loaded::Unreadable,
    };
    // A record that is not adopted may still belong to a VM that is running
    // (it is left alone, see `Reconciled::unreadable`). Its network slot must
    // not be handed to the next create: that one would be given a tap the
    // running guest holds.
    let keep_slot = |alloc: Option<&mut Allocator>, slot: Option<u32>| {
        if let (Some(alloc), Some(s)) = (alloc, slot) {
            alloc.reserve(s);
        }
    };
    let meta: Meta = match serde_json::from_str(&data) {
        Ok(m) => m,
        Err(_) => {
            keep_slot(alloc, lenient_slot(&data));
            return Loaded::Unreadable;
        }
    };

    let id = meta.id.clone();
    let vm_name = meta.name.clone();
    let dir_id = meta.dir_id.clone();
    // Adopting a record whose id or dir_id is not a plain path component would
    // re-introduce the traversal the create path now rejects: every later
    // write for this VM derives its path from these two strings.
    if !valid_vm_id(&id) || !valid_vm_id(&dir_id) {
        warn!(dir = %dir_name, "reconcile: skipped (id or dir_id is not a valid path component)");
        keep_slot(alloc, meta.slot);
        return Loaded::Unreadable;
    }
    // The directory a record was read from is the one it lives in. A dir_id
    // that names a different one would point every later write, and the
    // liveness check below, at another instance's files.
    if dir_id != dir_name {
        warn!(dir = %dir_name, dir_id = %dir_id, "reconcile: skipped (dir_id does not name the directory the record is in)");
        keep_slot(alloc, meta.slot);
        return Loaded::Unreadable;
    }
    let vcpus = meta.vcpus;
    let mem_mib = meta.mem_mib;
    let slot = meta.slot;
    let meta_pid = meta.pid;
    let ip = meta.ip.clone();
    let vsock = meta.vsock;
    let tenant_id = meta.tenant_id.clone();

    let mut state: VmState = meta.state.clone();
    let mut pid: Option<i32> = None;
    let mut dead_pool = false;

    // A state that implies a running VMM: is the recorded pid still it?
    if matches!(state, VmState::Running | VmState::Paused | VmState::Pooled) {
        let adopted = match meta_pid {
            None => false,
            Some(mpid) => match procs.identify(mpid, dir_name) {
                Identity::Match => true,
                Identity::Mismatch => false,
                // The process could not be inspected. Declaring a VM dead on a
                // failed read would orphan its VMM, so fall back to the only
                // thing left to go on — the question the agent used to ask.
                Identity::Unknown => {
                    let alive = pid_alive(mpid);
                    error!(vm = %id, pid = mpid, alive,
                        "reconcile: could not inspect the recorded pid; going by whether a process has it");
                    alive
                }
            },
        };
        if adopted {
            pid = meta_pid;
        } else if state == VmState::Pooled {
            // A pool VM without its VMM is of no use to anyone.
            state = VmState::Stopped;
            dead_pool = true;
        } else {
            // Process gone. If a snapshot exists → sleeping; else stopped.
            let vmstate_path = format!("{}/instances/{}/vmstate.bin", data_dir, dir_name);
            if Path::new(&vmstate_path).exists() {
                state = VmState::Sleeping;
            } else {
                state = VmState::Stopped;
            }
        }
    } else if matches!(state, VmState::Error | VmState::Stopped) && never_claimed_pool_vm(&meta) {
        // A pool VM that died while an agent was watching (`error`), or one
        // that agent was in the middle of removing (`stopped`). It is as much
        // use as one that was still `pooled` when its VMM went: none.
        dead_pool = true;
    }

    // Reserve the slot so it is not handed to a new VM.
    keep_slot(alloc, slot);

    let vm = Vm {
        id,
        name: vm_name,
        dir_id,
        vcpus,
        mem_mib,
        state,
        pid,
        slot,
        ip,
        vsock,
        tenant_id,
        exposes: meta.exposes.clone(),
        // v4 P4: absent in pre-P4 metas → the default image / no resize.
        image: meta.image.clone().unwrap_or_else(|| super::DEFAULT_IMAGE.to_string()),
        image_sha256: meta.image_sha256.clone(),
        disk_gb: meta.disk_gb.unwrap_or(0),
    };
    Loaded::Vm(vm, dead_pool)
}

/// The `slot` of a record that does not load as a `Meta`, when its text still
/// says — a record written by a newer agent, say, with a field or a state this
/// one does not know.
fn lenient_slot(data: &str) -> Option<u32> {
    let slot = serde_json::from_str::<serde_json::Value>(data).ok()?.get("slot")?.as_u64()?;
    u32::try_from(slot).ok()
}

/// Is this the record of a pool VM that was never handed to anyone? A claim
/// gives the record the caller's id, name and tenant and leaves only `dir_id`
/// naming the pool — so a record that still carries all of the pool's own
/// marks was never claimed.
fn never_claimed_pool_vm(meta: &Meta) -> bool {
    meta.id == meta.dir_id
        && meta.id.starts_with("pool-")
        && meta.name == "pool"
        && meta.tenant_id.is_none()
}

/// Does *some* process have this pid (kill(pid, 0))? Says nothing about what
/// that process is — use `ProcTable::identify` for that.
pub fn pid_alive(pid: i32) -> bool {
    use nix::unistd::Pid;
    // Signal 0 probes existence without delivering a signal.
    // ESRCH = no such process.
    nix::sys::signal::kill(Pid::from_raw(pid), None).is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ipalloc::{Allocator, Cidr};
    use std::fs;

    fn make_temp_dir() -> tempfile::TempDir {
        tempfile::tempdir().expect("tempdir")
    }

    fn write_meta(instances_dir: &Path, dir_name: &str, meta_json: &str) {
        let dir = instances_dir.join(dir_name);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("meta.json"), meta_json).unwrap();
    }

    fn write_vmstate(instances_dir: &Path, dir_name: &str) {
        let dir = instances_dir.join(dir_name);
        fs::write(dir.join("vmstate.bin"), b"fake").unwrap();
    }

    /// Reconcile against the real /proc: none of the pids these tests record
    /// is a Firecracker serving a temp dir, so no record is ever adopted.
    fn load(data_dir: &str, alloc: Option<&mut Allocator>) -> Vec<Vm> {
        reconcile(data_dir, alloc, &ProcTable::for_data_dir(data_dir)).vms
    }

    /// A procfs-shaped tree in which `pid` is a Firecracker serving
    /// `<data_dir>/instances/<dir_name>`.
    fn fake_proc_with_vmm(proc_root: &Path, data_dir: &str, pid: i32, dir_name: &str) {
        let p = proc_root.join(pid.to_string());
        fs::create_dir_all(&p).unwrap();
        let sock = format!("{}/instances/{}/fc.sock", data_dir, dir_name);
        let mut cmdline = Vec::new();
        for a in ["firecracker", "--api-sock", sock.as_str()] {
            cmdline.extend_from_slice(a.as_bytes());
            cmdline.push(0);
        }
        fs::write(p.join("cmdline"), cmdline).unwrap();
    }

    fn meta_json(id: &str, dir_id: &str, pid: Option<i32>, state: &str) -> String {
        let pid = pid.map(|p| p.to_string()).unwrap_or_else(|| "null".into());
        format!(
            r#"{{"id":"{id}","name":"{id}","dir_id":"{dir_id}","vcpus":1,"mem_mib":256,"pid":{pid},"slot":3,"ip":"10.231.0.5","state":"{state}"}}"#
        )
    }

    #[test]
    fn test_live_vmm_is_adopted_in_every_live_state() {
        // The path that became normal with KillMode=process: the agent is new,
        // the Firecrackers are not.
        let tmp = make_temp_dir();
        let data_dir = tmp.path().join("data");
        let instances_dir = data_dir.join("instances");
        let proc_root = tmp.path().join("proc");
        fs::create_dir_all(&instances_dir).unwrap();
        let data_dir = data_dir.to_str().unwrap();

        for (i, state) in ["running", "paused", "pooled"].iter().enumerate() {
            let name = format!("vm-{}", state);
            let pid = 4000 + i as i32;
            write_meta(&instances_dir, &name, &meta_json(&name, &name, Some(pid), state));
            fake_proc_with_vmm(&proc_root, data_dir, pid, &name);
        }

        let rec = reconcile(data_dir, None, &ProcTable::at(&proc_root, data_dir));
        assert!(rec.unreadable.is_empty() && rec.metaless.is_empty());
        // A pool VM whose VMM is alive is adopted, not written off.
        assert!(rec.dead_pool.is_empty());
        let by_id = |id: &str| rec.vms.iter().find(|v| v.id == id).unwrap_or_else(|| panic!("{id} not loaded"));
        assert_eq!((by_id("vm-running").state.clone(), by_id("vm-running").pid), (VmState::Running, Some(4000)));
        assert_eq!((by_id("vm-paused").state.clone(), by_id("vm-paused").pid), (VmState::Paused, Some(4001)));
        // Was: pooled → stopped with the pid dropped, i.e. one live Firecracker
        // and one tap leaked on every agent restart.
        assert_eq!((by_id("vm-pooled").state.clone(), by_id("vm-pooled").pid), (VmState::Pooled, Some(4002)));
    }

    #[test]
    fn test_recycled_pid_is_not_adopted() {
        // The recorded pid is alive, but it is not this instance's VMM: it is
        // another instance's, or not a Firecracker at all. `kill(pid, 0)`
        // adopted both.
        let tmp = make_temp_dir();
        let data_dir = tmp.path().join("data");
        let instances_dir = data_dir.join("instances");
        let proc_root = tmp.path().join("proc");
        fs::create_dir_all(&instances_dir).unwrap();
        let data_dir = data_dir.to_str().unwrap();

        write_meta(&instances_dir, "vm-a", &meta_json("vm-a", "vm-a", Some(5000), "running"));
        write_meta(&instances_dir, "vm-b", &meta_json("vm-b", "vm-b", Some(5001), "running"));
        write_vmstate(&instances_dir, "vm-b");
        write_meta(&instances_dir, "vm-c", &meta_json("vm-c", "vm-c", Some(5002), "pooled"));
        // 5000 serves vm-OTHER; 5001 is an unrelated program; 5002 does not exist.
        fs::create_dir_all(instances_dir.join("vm-other")).unwrap();
        fake_proc_with_vmm(&proc_root, data_dir, 5000, "vm-other");
        fs::create_dir_all(proc_root.join("5001")).unwrap();
        fs::write(proc_root.join("5001").join("cmdline"), b"sleep\0600\0").unwrap();

        let rec = reconcile(data_dir, None, &ProcTable::at(&proc_root, data_dir));
        let by_id = |id: &str| rec.vms.iter().find(|v| v.id == id).unwrap_or_else(|| panic!("{id} not loaded"));
        assert_eq!((by_id("vm-a").state.clone(), by_id("vm-a").pid), (VmState::Stopped, None));
        assert_eq!((by_id("vm-b").state.clone(), by_id("vm-b").pid), (VmState::Sleeping, None));
        assert_eq!((by_id("vm-c").state.clone(), by_id("vm-c").pid), (VmState::Stopped, None));
    }

    #[test]
    fn test_live_state_without_a_pid_falls_back() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();
        write_meta(&instances_dir, "vm-np", &meta_json("vm-np", "vm-np", None, "running"));

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-np").expect("vm-np");
        // "running" with no VMM to speak of is not a state anything could leave.
        assert_eq!((vm.state.clone(), vm.pid), (VmState::Stopped, None));
    }

    #[test]
    fn test_directories_without_a_usable_record_are_reported_not_dropped() {
        // What start-up does with a directory depends on WHY it has no record:
        // one with no meta.json never became a VM and can be cleaned up; one
        // whose meta.json is damaged may well hold a healthy tenant VM.
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        write_meta(&instances_dir, "vm-ok", &meta_json("vm-ok", "vm-ok", None, "stopped"));
        // Never completed: rootfs copied, meta.json not written yet.
        fs::create_dir_all(instances_dir.join("vm-half")).unwrap();
        fs::write(instances_dir.join("vm-half").join("rootfs.ext4"), b"x").unwrap();
        // Only the temp file of an atomic write: the rename never happened.
        fs::create_dir_all(instances_dir.join("vm-tmp")).unwrap();
        fs::write(instances_dir.join("vm-tmp").join("meta.json.tmp"), b"{}").unwrap();
        // Damaged records.
        write_meta(&instances_dir, "vm-empty", "");
        write_meta(&instances_dir, "vm-junk", "{\"id\":\"vm-junk\",\"na");
        write_meta(&instances_dir, "vm-newer", &meta_json("vm-newer", "vm-newer", None, "hibernating"));
        // A record that says it lives somewhere else.
        write_meta(&instances_dir, "vm-moved", &meta_json("vm-moved", "vm-ok", None, "stopped"));
        // Not an instance directory at all: left completely alone.
        fs::create_dir_all(instances_dir.join("lost+found")).unwrap();
        fs::write(instances_dir.join("stray-file"), b"x").unwrap();

        let mut rec = reconcile(data_dir, None, &ProcTable::for_data_dir(data_dir));
        rec.unreadable.sort();
        rec.metaless.sort();
        assert_eq!(rec.vms.iter().map(|v| v.id.as_str()).collect::<Vec<_>>(), vec!["vm-ok"]);
        assert_eq!(rec.metaless, vec!["vm-half", "vm-tmp"]);
        assert_eq!(rec.unreadable, vec!["vm-empty", "vm-junk", "vm-moved", "vm-newer"]);
    }

    #[test]
    fn test_dead_pid_with_vmstate_becomes_sleeping() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // A running VM with a dead pid (1 is always alive but let's use 999999 which should be dead).
        let meta = r#"{"id":"vm-1","name":"vm-1","dir_id":"vm-1","vcpus":1,"mem_mib":256,"pid":999999,"slot":null,"ip":null,"state":"running"}"#;
        write_meta(&instances_dir, "vm-1", meta);
        // Write vmstate.bin so it maps to sleeping.
        write_vmstate(&instances_dir, "vm-1");

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-1").expect("vm-1");
        // pid 999999 is almost certainly dead → sleeping (vmstate.bin present).
        assert_eq!(vm.state, VmState::Sleeping, "should be sleeping with vmstate.bin");
        assert_eq!(vm.pid, None);
        assert_eq!(vm.tenant_id, None);
    }

    #[test]
    fn test_dead_pid_without_vmstate_becomes_stopped() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"vm-2","name":"vm-2","dir_id":"vm-2","vcpus":1,"mem_mib":256,"pid":999999,"slot":null,"ip":null,"state":"running"}"#;
        write_meta(&instances_dir, "vm-2", meta);
        // No vmstate.bin → stopped.

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-2").expect("vm-2");
        assert_eq!(vm.state, VmState::Stopped);
        assert_eq!(vm.pid, None);
        assert_eq!(vm.tenant_id, None);
    }

    #[test]
    fn test_pooled_becomes_stopped() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"pool-abc","name":"pool-abc","dir_id":"pool-abc","vcpus":1,"mem_mib":256,"pid":12345,"slot":2,"ip":"10.231.0.4","state":"pooled"}"#;
        write_meta(&instances_dir, "pool-abc", meta);

        let rec = reconcile(data_dir, None, &ProcTable::for_data_dir(data_dir));
        let vm = rec.vms.iter().find(|v| v.id == "pool-abc").expect("pool-abc");
        assert_eq!(vm.state, VmState::Stopped, "a pool VM whose VMM is gone is not claimable");
        // ... and it is handed back as one to remove (F-26: it used to stay a
        // `stopped` record for good, one more after every restart).
        assert_eq!(rec.dead_pool, vec!["pool-abc"]);
    }

    #[test]
    fn test_slot_reservation() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"vm-s","name":"vm-s","dir_id":"vm-s","vcpus":1,"mem_mib":256,"pid":null,"slot":2,"ip":"10.231.0.4","state":"sleeping"}"#;
        write_meta(&instances_dir, "vm-s", meta);

        let cidr = Cidr::parse("10.231.0.0/24").unwrap();
        let mut alloc = Allocator::new(cidr);
        let vms = load(data_dir, Some(&mut alloc));

        // Slot 2 should be reserved — next claim should skip it.
        let claimed = alloc.claim().unwrap();
        assert_ne!(claimed, 2, "slot 2 should be reserved");
        let _ = vms;
    }

    #[test]
    fn test_sleeping_state_preserved() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"vm-sl","name":"vm-sl","dir_id":"vm-sl","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping"}"#;
        write_meta(&instances_dir, "vm-sl", meta);

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-sl").expect("vm-sl");
        assert_eq!(vm.state, VmState::Sleeping);
        assert_eq!(vm.pid, None);
    }

    #[test]
    fn test_traversal_dir_id_is_not_adopted() {
        // A meta whose dir_id escapes the instances tree must not become a
        // live record: every later write for that VM derives its path from it.
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"vm-evil","name":"vm-evil","dir_id":"../../etc/felucca","vcpus":1,"mem_mib":256,"pid":null,"slot":null,"ip":null,"state":"stopped"}"#;
        write_meta(&instances_dir, "vm-evil", meta);
        let bad_id = r#"{"id":"../../etc/felucca","name":"x","dir_id":"vm-evil2","vcpus":1,"mem_mib":256,"pid":null,"slot":null,"ip":null,"state":"stopped"}"#;
        write_meta(&instances_dir, "vm-evil2", bad_id);

        let vms = load(data_dir, None);
        assert!(vms.is_empty(), "traversal ids must not be adopted: {:?}", vms);
    }

    #[test]
    fn test_dir_id_from_meta() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // A pool-claimed VM: dir_id stays as pool dir, id is the real id.
        let meta = r#"{"id":"sb-real-id","name":"myvm","dir_id":"pool-xyz","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"running"}"#;
        // dir on disk is named "pool-xyz" (the pool dir)
        write_meta(&instances_dir, "pool-xyz", meta);
        write_vmstate(&instances_dir, "pool-xyz");

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "sb-real-id").expect("sb-real-id");
        assert_eq!(vm.dir_id, "pool-xyz");
    }

    #[test]
    fn test_tenant_id_carried_through_reconcile() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // v4 meta with tenant_id set.
        let meta = r#"{"id":"vm-t","name":"vm-t","dir_id":"vm-t","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping","vsock":false,"tenant_id":"acme"}"#;
        write_meta(&instances_dir, "vm-t", meta);

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-t").expect("vm-t");
        assert_eq!(vm.tenant_id, Some("acme".to_string()), "tenant_id must be carried through reconcile");
    }

    #[test]
    fn test_tenant_id_absent_in_old_meta() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // Old meta without tenant_id key.
        let meta = r#"{"id":"vm-old","name":"vm-old","dir_id":"vm-old","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping","vsock":true}"#;
        write_meta(&instances_dir, "vm-old", meta);

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-old").expect("vm-old");
        assert_eq!(vm.tenant_id, None, "absent tenant_id must default to None");
    }

    #[test]
    fn test_image_disk_gb_carried_through_reconcile() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // v4 P4 meta with image + disk_gb set.
        let meta = r#"{"id":"vm-img","name":"vm-img","dir_id":"vm-img","vcpus":2,"mem_mib":2048,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping","vsock":true,"image":"odoo-v18","disk_gb":8}"#;
        write_meta(&instances_dir, "vm-img", meta);

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-img").expect("vm-img");
        assert_eq!(vm.image, "odoo-v18");
        assert_eq!(vm.disk_gb, 8);
    }

    #[test]
    fn test_image_disk_gb_default_in_pre_p4_meta() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        // Pre-P4 meta (no image/disk_gb keys) → defaults.
        let meta = r#"{"id":"vm-pre","name":"vm-pre","dir_id":"vm-pre","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping","vsock":true}"#;
        write_meta(&instances_dir, "vm-pre", meta);

        let vms = load(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-pre").expect("vm-pre");
        assert_eq!(vm.image, "ubuntu-base", "absent image must default to ubuntu-base");
        assert_eq!(vm.disk_gb, 0, "absent disk_gb must default to 0");
    }

    #[test]
    fn test_a_record_that_does_not_load_still_keeps_its_slot() {
        // The VM behind such a record may well be running (it is left alone).
        // The next create must not be handed the tap that guest holds.
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();
        // Written by a newer agent: a state this one does not know.
        write_meta(
            &instances_dir,
            "vm-new",
            r#"{"id":"vm-new","name":"vm-new","dir_id":"vm-new","vcpus":1,"mem_mib":256,"pid":7,"slot":0,"ip":"10.231.0.2","state":"hibernating"}"#,
        );
        // A record that names another directory.
        write_meta(
            &instances_dir,
            "vm-moved",
            r#"{"id":"vm-moved","name":"vm-moved","dir_id":"vm-elsewhere","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"running"}"#,
        );
        // Cut off in the middle: nothing to go on.
        write_meta(&instances_dir, "vm-cut", r#"{"id":"vm-cut","na"#);

        let mut alloc = Allocator::new(Cidr::parse("10.231.0.0/24").unwrap());
        let rec = reconcile(data_dir, Some(&mut alloc), &ProcTable::for_data_dir(data_dir));
        assert!(rec.vms.is_empty());
        assert_eq!(rec.unreadable.len(), 3, "{:?}", rec.unreadable);
        assert_eq!(alloc.claim(), Some(2), "slots 0 and 1 belong to VMs that may be running");
    }

    #[test]
    fn test_a_never_claimed_pool_vm_without_a_vmm_is_reported_dead_in_any_state() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();
        let record = |id: &str, name: &str, dir_id: &str, state: &str, tenant: &str| {
            format!(
                r#"{{"id":"{id}","name":"{name}","dir_id":"{dir_id}","vcpus":1,"mem_mib":256,"pid":null,"slot":null,"ip":null,"state":"{state}"{tenant}}}"#
            )
        };
        // Died under an agent that marked it and did not get to remove it.
        write_meta(&instances_dir, "pool-err", &record("pool-err", "pool", "pool-err", "error", ""));
        // That agent was in the middle of removing it.
        write_meta(&instances_dir, "pool-stop", &record("pool-stop", "pool", "pool-stop", "stopped", ""));
        // Claimed from a pool: a tenant's sandbox living in a pool directory.
        write_meta(
            &instances_dir,
            "pool-claimed",
            &record("sb-1", "web", "pool-claimed", "error", r#","tenant_id":"tn-a""#),
        );
        // A sandbox that merely has a pool-like id.
        write_meta(&instances_dir, "pool-mine", &record("pool-mine", "mine", "pool-mine", "stopped", ""));

        let rec = reconcile(data_dir, None, &ProcTable::for_data_dir(data_dir));
        let mut dead = rec.dead_pool.clone();
        dead.sort();
        assert_eq!(dead, vec!["pool-err".to_string(), "pool-stop".to_string()]);
        // Reported, not dropped: the caller removes them, slot and directory too.
        assert_eq!(rec.vms.len(), 4);
    }
}
