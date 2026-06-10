//! Startup reconciliation: rescan instances/*/meta.json and rebuild in-memory state.
//!
//! Port of Manager.reconcile / Manager.reconcileOne in backend/src/agent/vm.zig,
//! branch-for-branch.

use super::{Vm, VmState};
use crate::ipalloc::Allocator;
use crate::vm::meta::Meta;
use std::path::Path;

/// Scan data_dir/instances/ and return a Vec of reconciled Vm records.
/// Also calls `alloc.reserve(slot)` for every record that has a slot set.
pub fn reconcile(data_dir: &str, mut alloc: Option<&mut Allocator>) -> Vec<Vm> {
    let instances_path = format!("{}/instances", data_dir);
    let dir = match std::fs::read_dir(&instances_path) {
        Ok(d) => d,
        Err(_) => return Vec::new(),
    };

    let mut vms = Vec::new();
    for entry in dir.flatten() {
        let ft = match entry.file_type() {
            Ok(t) => t,
            Err(_) => continue,
        };
        if !ft.is_dir() { continue; }
        let name = entry.file_name();
        let dir_name = name.to_string_lossy();
        match reconcile_one(data_dir, &dir_name, alloc.as_deref_mut()) {
            Some(vm) => vms.push(vm),
            None => {
                eprintln!("reconcile {}: skipped (no valid meta.json)", dir_name);
            }
        }
    }
    vms
}

fn reconcile_one(data_dir: &str, dir_name: &str, alloc: Option<&mut Allocator>) -> Option<Vm> {
    let meta_path = format!("{}/instances/{}/meta.json", data_dir, dir_name);
    let data = std::fs::read_to_string(&meta_path).ok()?;
    let meta: Meta = serde_json::from_str(&data).ok()?;

    let id = meta.id.clone();
    let vm_name = meta.name.clone();
    // The on-disk dir name is authoritative for dir_id.
    let dir_id = meta.dir_id.clone();
    let vcpus = meta.vcpus;
    let mem_mib = meta.mem_mib;
    let slot = meta.slot;
    let meta_pid = meta.pid;
    let ip = meta.ip.clone();
    let vsock = meta.vsock;

    let mut state: VmState = meta.state.clone();
    let mut pid: Option<i32> = None;

    // Pool VMs from a previous run are not reclaimed; treat them as stopped orphans.
    if state == VmState::Pooled {
        state = VmState::Stopped;
    }

    // Is the recorded pid still a live firecracker?
    if (state == VmState::Running || state == VmState::Paused) && meta_pid.is_some() {
        let mpid = meta_pid.unwrap();
        if pid_alive(mpid) {
            pid = Some(mpid);
        } else {
            // Process gone. If a snapshot exists → sleeping; else stopped.
            let vmstate_path = format!("{}/instances/{}/vmstate.bin", data_dir, dir_name);
            if Path::new(&vmstate_path).exists() {
                state = VmState::Sleeping;
            } else {
                state = VmState::Stopped;
            }
        }
    }

    // Reserve the slot so it is not handed to a new VM.
    if let (Some(alloc), Some(s)) = (alloc, slot) {
        alloc.reserve(s);
    }

    Some(Vm {
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
    })
}

/// Check if a pid is alive (kill(pid, 0)).
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

        let vms = reconcile(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-1").expect("vm-1");
        // pid 999999 is almost certainly dead → sleeping (vmstate.bin present).
        assert_eq!(vm.state, VmState::Sleeping, "should be sleeping with vmstate.bin");
        assert_eq!(vm.pid, None);
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

        let vms = reconcile(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-2").expect("vm-2");
        assert_eq!(vm.state, VmState::Stopped);
        assert_eq!(vm.pid, None);
    }

    #[test]
    fn test_pooled_becomes_stopped() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"pool-abc","name":"pool-abc","dir_id":"pool-abc","vcpus":1,"mem_mib":256,"pid":12345,"slot":2,"ip":"10.231.0.4","state":"pooled"}"#;
        write_meta(&instances_dir, "pool-abc", meta);

        let vms = reconcile(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "pool-abc").expect("pool-abc");
        assert_eq!(vm.state, VmState::Stopped, "pooled → stopped on startup");
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
        let vms = reconcile(data_dir, Some(&mut alloc));

        // Slot 2 should be reserved — next claim should skip it.
        let claimed = alloc.claim().unwrap();
        assert_ne!(claimed, 2, "slot 2 should be reserved");
    }

    #[test]
    fn test_sleeping_state_preserved() {
        let tmp = make_temp_dir();
        let data_dir = tmp.path().to_str().unwrap();
        let instances_dir = tmp.path().join("instances");
        fs::create_dir_all(&instances_dir).unwrap();

        let meta = r#"{"id":"vm-sl","name":"vm-sl","dir_id":"vm-sl","vcpus":1,"mem_mib":256,"pid":null,"slot":1,"ip":"10.231.0.3","state":"sleeping"}"#;
        write_meta(&instances_dir, "vm-sl", meta);

        let vms = reconcile(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "vm-sl").expect("vm-sl");
        assert_eq!(vm.state, VmState::Sleeping);
        assert_eq!(vm.pid, None);
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

        let vms = reconcile(data_dir, None);
        let vm = vms.iter().find(|v| v.id == "sb-real-id").expect("sb-real-id");
        assert_eq!(vm.dir_id, "pool-xyz");
    }
}
