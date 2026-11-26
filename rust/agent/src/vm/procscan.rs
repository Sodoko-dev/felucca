//! Which processes are this agent's Firecrackers.
//!
//! A Firecracker outlives the agent that spawned it (ADR-0011): the unit runs
//! with `KillMode=process`, so a restart, a crash or an upgrade of the agent
//! leaves every VM running and the next agent picks them up from disk. That
//! makes "is pid N still this instance's VMM?" a question the agent asks about
//! processes it did not start, and a bare `kill(pid, 0)` answers a different
//! one: whether *some* process has that number. After a reboot, or once pids
//! wrap, that is a stranger.
//!
//! The answer here comes from what the process is rather than from its number:
//! it is owned by the agent's uid, it was started as `firecracker --api-sock
//! <P>`, and `<P>` is `fc.sock` inside the instance's own directory. Paths are
//! compared after canonicalisation, so a `data_dir` spelled differently from
//! the one the VMM was started with (a trailing slash, a symlink) is still the
//! same directory.
//!
//! Every lookup has three outcomes, not two. A VM wrongly declared dead is
//! marked `error` while its VMM keeps running, and the next start-up would
//! then kill that VMM as an orphan — so an I/O error while looking (fd
//! exhaustion, a transient ENOMEM) must never read as "not ours". Only a
//! positive finding does; see [`Identity::Unknown`].

use std::ffi::OsStr;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};

/// File name of a Firecracker API socket inside its instance directory.
pub const SOCK_NAME: &str = "fc.sock";

/// What a pid turned out to be, relative to one instance.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Identity {
    /// The process is this instance's Firecracker.
    Match,
    /// There is no such process, or it is positively something else.
    Mismatch,
    /// It could not be determined. Callers must treat this as "still there":
    /// acting on it would turn a failed read into a lost VM.
    Unknown,
}

/// A Firecracker serving one instance directory of this agent's data dir.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FcProc {
    pub pid: i32,
    /// Name of the instance directory its `--api-sock` lives in.
    pub dir_name: String,
}

enum Lookup<T> {
    Found(T),
    /// Positively not there / not one of ours.
    No,
    /// Could not tell.
    Unknown,
}

/// ENOENT, or ESRCH — which is what reading a `/proc/<pid>/` file returns when
/// the process exits between the open and the read.
fn gone(e: &io::Error) -> bool {
    e.kind() == io::ErrorKind::NotFound || e.raw_os_error() == Some(libc::ESRCH)
}

/// `PF_EXITING` in the `flags` field of `/proc/<pid>/stat`: the task has
/// entered `do_exit` and is on its way out.
const PF_EXITING: u64 = 0x4;

/// `PF_KTHREAD`: a kernel thread. It has no command line and never will.
const PF_KTHREAD: u64 = 0x0020_0000;

/// `(state, flags)` out of a `/proc/<pid>/stat` line. The command name sits in
/// parentheses and may itself contain spaces and parentheses, so the fields
/// are counted from the LAST `)`.
fn parse_stat(stat: &str) -> Option<(char, u64)> {
    let rest = &stat[stat.rfind(')')? + 1..];
    let mut fields = rest.split_whitespace();
    let state = fields.next()?.chars().next()?;
    // state ppid pgrp session tty_nr tpgid flags
    let flags = fields.nth(5)?.parse::<u64>().ok()?;
    Some((state, flags))
}

/// The `--api-sock` argument of a Firecracker command line, when `cmdline`
/// (NUL-separated, as `/proc/<pid>/cmdline` has it) is one. Both spellings the
/// option parser accepts are recognised. An empty command line — a kernel
/// thread, or a zombie that has not been reaped yet — is not a Firecracker.
pub fn api_sock_arg(cmdline: &[u8]) -> Option<PathBuf> {
    let mut args = cmdline.split(|b| *b == 0);
    let argv0 = args.next()?;
    if Path::new(OsStr::from_bytes(argv0)).file_name()? != OsStr::new("firecracker") {
        return None;
    }
    let mut value_follows = false;
    for a in args {
        if value_follows {
            return (!a.is_empty()).then(|| PathBuf::from(OsStr::from_bytes(a)));
        }
        if a == b"--api-sock" {
            value_follows = true;
        } else if let Some(v) = a.strip_prefix(b"--api-sock=") {
            return (!v.is_empty()).then(|| PathBuf::from(OsStr::from_bytes(v)));
        }
    }
    None
}

/// A view of the process table, scoped to one data dir.
pub struct ProcTable {
    root: PathBuf,
    uid: u32,
    /// `<data_dir>/instances`, as configured (canonicalised at each use: the
    /// directory may not exist yet when the table is built).
    instances: PathBuf,
}

impl ProcTable {
    /// The real `/proc`, the agent's own uid.
    #[cfg(test)]
    pub fn for_data_dir(data_dir: &str) -> Self {
        Self::at(Path::new("/proc"), data_dir)
    }

    /// Any procfs-shaped tree (unit tests build one in a temp dir).
    pub fn at(root: &Path, data_dir: &str) -> Self {
        ProcTable {
            root: root.to_path_buf(),
            // SAFETY: geteuid has no preconditions and cannot fail.
            uid: unsafe { libc::geteuid() },
            instances: Path::new(data_dir).join("instances"),
        }
    }

    #[cfg(test)]
    fn with_uid(mut self, uid: u32) -> Self {
        self.uid = uid;
        self
    }

    /// The canonical directory the Firecracker at `pid` keeps its API socket in.
    fn fc_dir(&self, pid: i32) -> Lookup<PathBuf> {
        let proc_dir = self.root.join(pid.to_string());
        match std::fs::metadata(&proc_dir) {
            // Someone else's process can put anything in its argv. Ours run as us.
            Ok(m) if m.uid() != self.uid => return Lookup::No,
            Ok(_) => {}
            Err(e) if gone(&e) => return Lookup::No,
            Err(_) => return Lookup::Unknown,
        }
        let cmdline = match std::fs::read(proc_dir.join("cmdline")) {
            Ok(c) => c,
            Err(e) if gone(&e) => return Lookup::No,
            Err(_) => return Lookup::Unknown,
        };
        if cmdline.is_empty() {
            // No command line. Nearly always that is a process that is dead or
            // dying, or a kernel thread — none of which is a VMM. But a
            // process that was exec'd a moment ago also has none, for the
            // instant before the new image's argv is published, and that one
            // is very much alive: it must not be declared gone.
            let stat = match std::fs::read_to_string(proc_dir.join("stat")) {
                Ok(s) => s,
                Err(e) if gone(&e) => return Lookup::No,
                Err(_) => return Lookup::Unknown,
            };
            return match parse_stat(&stat) {
                Some(('Z' | 'X', _)) => Lookup::No,
                Some((_, flags)) if flags & (PF_EXITING | PF_KTHREAD) != 0 => Lookup::No,
                _ => Lookup::Unknown,
            };
        }
        let Some(sock) = api_sock_arg(&cmdline) else {
            return Lookup::No;
        };
        if sock.file_name() != Some(OsStr::new(SOCK_NAME)) {
            return Lookup::No;
        }
        let Some(parent) = sock.parent() else {
            return Lookup::No;
        };
        // A relative socket path is relative to where the VMM was started.
        let parent = if parent.is_absolute() {
            parent.to_path_buf()
        } else {
            match std::fs::read_link(proc_dir.join("cwd")) {
                Ok(cwd) => cwd.join(parent),
                Err(e) if gone(&e) => return Lookup::No,
                Err(_) => return Lookup::Unknown,
            }
        };
        match std::fs::canonicalize(&parent) {
            Ok(c) => Lookup::Found(c),
            // Its directory no longer exists, so it is not serving one that does.
            Err(e) if gone(&e) => Lookup::No,
            Err(_) => Lookup::Unknown,
        }
    }

    /// Is `pid` the Firecracker of the instance in `<instances>/<dir_name>`?
    pub fn identify(&self, pid: i32, dir_name: &str) -> Identity {
        let want = match std::fs::canonicalize(self.instances.join(dir_name)) {
            Ok(w) => w,
            Err(e) if gone(&e) => return Identity::Mismatch,
            Err(_) => return Identity::Unknown,
        };
        match self.fc_dir(pid) {
            Lookup::Found(dir) if dir == want => Identity::Match,
            Lookup::Found(_) | Lookup::No => Identity::Mismatch,
            Lookup::Unknown => Identity::Unknown,
        }
    }

    /// Every Firecracker whose API socket lives in a directory directly under
    /// this data dir's `instances/`. A process that could not be read is left
    /// out: a list that only ever names positively identified VMMs is what
    /// makes it safe to kill from.
    pub fn scan(&self) -> Vec<FcProc> {
        let Ok(instances) = std::fs::canonicalize(&self.instances) else {
            return Vec::new();
        };
        let Ok(entries) = std::fs::read_dir(&self.root) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        for entry in entries.flatten() {
            let Some(pid) = entry.file_name().to_str().and_then(|n| n.parse::<i32>().ok()) else {
                continue;
            };
            let Lookup::Found(dir) = self.fc_dir(pid) else {
                continue;
            };
            if dir.parent() != Some(instances.as_path()) {
                continue;
            }
            if let Some(name) = dir.file_name().and_then(|n| n.to_str()) {
                out.push(FcProc { pid, dir_name: name.to_string() });
            }
        }
        out.sort_by_key(|p| p.pid);
        out
    }

    /// Has the process at `pid` finished exiting — every thread of it?
    ///
    /// This is the question to ask after killing a VMM, and it is not the
    /// same as "does it still look like one". A dying process loses its
    /// command line the moment it enters `do_exit`, which is BEFORE it unmaps
    /// its memory and closes its files; for a guest with many GiB touched
    /// that gap is long enough for a caller that respawns straight away to
    /// find the tap still held. And the thread-group leader shows as a zombie
    /// as soon as *it* has exited, while the memory and the descriptors are
    /// only released when the last thread has — Firecracker runs one thread
    /// per vCPU.
    ///
    /// True when there is no such process, or its leader is a zombie and no
    /// other thread is left. False when it could not be determined.
    pub fn exited(&self, pid: i32) -> bool {
        let dir = self.root.join(pid.to_string());
        let stat = match std::fs::read_to_string(dir.join("stat")) {
            Ok(s) => s,
            Err(e) if gone(&e) => return true,
            Err(_) => return false,
        };
        match parse_stat(&stat) {
            Some(('Z' | 'X', _)) => {}
            _ => return false,
        }
        let leader = pid.to_string();
        match std::fs::read_dir(dir.join("task")) {
            Ok(tasks) => tasks.flatten().all(|t| t.file_name().to_str() == Some(leader.as_str())),
            Err(e) if gone(&e) => true,
            Err(_) => false,
        }
    }

    /// Is the process at `pid` in the middle of exiting?
    pub fn exiting(&self, pid: i32) -> bool {
        std::fs::read_to_string(self.root.join(pid.to_string()).join("stat"))
            .ok()
            .and_then(|s| parse_stat(&s))
            .is_some_and(|(_, flags)| flags & PF_EXITING != 0)
    }

    /// Is `pid` in the same cgroup as this process?
    ///
    /// Under systemd that is the agent's own unit, which is where every VMM a
    /// previous run of this unit started still is. It is the line the orphan
    /// sweep does not cross: a Firecracker someone started from a shell against
    /// the same data dir is not this unit's to kill.
    pub fn in_my_cgroup(&self, pid: i32) -> bool {
        let mine = std::fs::read_to_string(self.root.join("self").join("cgroup"));
        let theirs = std::fs::read_to_string(self.root.join(pid.to_string()).join("cgroup"));
        matches!((mine, theirs), (Ok(a), Ok(b)) if !a.trim().is_empty() && a == b)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    /// A procfs-shaped temp tree plus a data dir with `instances/`.
    struct Fake {
        _tmp: tempfile::TempDir,
        proc_root: PathBuf,
        data_dir: PathBuf,
    }

    impl Fake {
        fn new() -> Self {
            let tmp = tempfile::tempdir().unwrap();
            // Canonical up front: on some hosts the temp dir itself sits behind a symlink.
            let base = fs::canonicalize(tmp.path()).unwrap();
            let proc_root = base.join("proc");
            let data_dir = base.join("data");
            fs::create_dir_all(&proc_root).unwrap();
            fs::create_dir_all(data_dir.join("instances")).unwrap();
            Fake { _tmp: tmp, proc_root, data_dir }
        }

        fn instance(&self, name: &str) -> PathBuf {
            let d = self.data_dir.join("instances").join(name);
            fs::create_dir_all(&d).unwrap();
            d
        }

        fn process(&self, pid: i32, argv: &[&str]) -> PathBuf {
            let d = self.proc_root.join(pid.to_string());
            fs::create_dir_all(&d).unwrap();
            let mut cmdline = Vec::new();
            for a in argv {
                cmdline.extend_from_slice(a.as_bytes());
                cmdline.push(0);
            }
            fs::write(d.join("cmdline"), cmdline).unwrap();
            d
        }

        fn fc(&self, pid: i32, instance: &str) -> PathBuf {
            let sock = self.instance(instance).join(SOCK_NAME);
            self.process(pid, &["firecracker", "--api-sock", sock.to_str().unwrap()])
        }

        fn table(&self) -> ProcTable {
            ProcTable::at(&self.proc_root, self.data_dir.to_str().unwrap())
        }
    }

    fn argv(parts: &[&str]) -> Vec<u8> {
        let mut v = Vec::new();
        for p in parts {
            v.extend_from_slice(p.as_bytes());
            v.push(0);
        }
        v
    }

    #[test]
    fn test_api_sock_arg_reads_the_command_line_the_agent_spawns() {
        assert_eq!(
            api_sock_arg(&argv(&["firecracker", "--api-sock", "/srv/felucca/instances/vm-1/fc.sock"])),
            Some(PathBuf::from("/srv/felucca/instances/vm-1/fc.sock"))
        );
        // A full path in argv[0], the `=` spelling, and other options around it.
        assert_eq!(
            api_sock_arg(&argv(&["/usr/local/bin/firecracker", "--id", "x", "--api-sock=/d/fc.sock"])),
            Some(PathBuf::from("/d/fc.sock"))
        );
    }

    #[test]
    fn test_api_sock_arg_refuses_everything_that_is_not_a_firecracker() {
        // What a recycled pid typically holds.
        assert_eq!(api_sock_arg(&argv(&["sleep", "600"])), None);
        // Same option, different program; a name that merely starts the same.
        assert_eq!(api_sock_arg(&argv(&["curl", "--api-sock", "/d/fc.sock"])), None);
        assert_eq!(api_sock_arg(&argv(&["firecracker-wrapper", "--api-sock", "/d/fc.sock"])), None);
        // No socket option, or one with nothing after it.
        assert_eq!(api_sock_arg(&argv(&["firecracker", "--version"])), None);
        assert_eq!(api_sock_arg(&argv(&["firecracker", "--api-sock"])), None);
        assert_eq!(api_sock_arg(&argv(&["firecracker", "--api-sock="])), None);
        // Kernel threads and zombies have an empty command line.
        assert_eq!(api_sock_arg(b""), None);
    }

    #[test]
    fn test_identify_matches_the_instances_own_vmm_and_nothing_else() {
        let f = Fake::new();
        f.fc(100, "vm-1");
        f.fc(101, "vm-2");
        let t = f.table();
        assert_eq!(t.identify(100, "vm-1"), Identity::Match);
        // Alive, a Firecracker, ours — but another instance's.
        assert_eq!(t.identify(101, "vm-1"), Identity::Mismatch);
        // No such process.
        assert_eq!(t.identify(999, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_identify_rejects_a_recycled_pid() {
        // The recorded pid is alive again after a reboot, as something else:
        // exactly the case `kill(pid, 0)` adopts.
        let f = Fake::new();
        f.instance("vm-1");
        f.process(100, &["/usr/sbin/sshd", "-D"]);
        f.process(101, &[]); // zombie / kernel thread: empty command line
        let t = f.table();
        assert_eq!(t.identify(100, "vm-1"), Identity::Mismatch);
        assert_eq!(t.identify(101, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_identify_an_empty_command_line_is_only_a_death_when_the_process_says_so() {
        let f = Fake::new();
        f.instance("vm-1");
        let t = f.table();
        // Just exec'd: alive, a normal user process, argv not published yet.
        // Declaring this one gone would orphan a VMM that is about to exist.
        let born = f.process(100, &[]);
        fs::write(born.join("stat"), "100 (sh) R 1 100 100 0 -1 4194304 0 0").unwrap();
        assert_eq!(t.identify(100, "vm-1"), Identity::Unknown);
        // A zombie, a process that is exiting, and a kernel thread have no
        // command line either, and none of them is a VMM.
        let zombie = f.process(101, &[]);
        fs::write(zombie.join("stat"), "101 (firecracker) Z 1 101 101 0 -1 4194564 0 0").unwrap();
        let dying = f.process(102, &[]);
        fs::write(dying.join("stat"), "102 (firecracker) R 1 102 102 0 -1 4194564 0 0").unwrap();
        let kthread = f.process(103, &[]);
        fs::write(kthread.join("stat"), "103 (kworker/0:1) I 2 0 0 0 -1 69238880 0 0").unwrap();
        for pid in [101, 102, 103] {
            assert_eq!(t.identify(pid, "vm-1"), Identity::Mismatch, "pid {pid}");
        }
        // None of the four is ever offered to the sweep.
        assert!(t.scan().is_empty());
    }

    #[test]
    fn test_identify_rejects_another_users_process_with_a_forged_command_line() {
        // argv is whatever the process says it is; the owner is not.
        let f = Fake::new();
        f.fc(100, "vm-1");
        let other_uid = unsafe { libc::geteuid() } + 1;
        assert_eq!(f.table().with_uid(other_uid).identify(100, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_identify_requires_the_api_socket_name() {
        let f = Fake::new();
        let dir = f.instance("vm-1");
        f.process(100, &["firecracker", "--api-sock", dir.join("v.sock").to_str().unwrap()]);
        assert_eq!(f.table().identify(100, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_identify_survives_a_data_dir_spelled_differently() {
        // The VMM was started with the path as configured then; the agent now
        // has the same directory under another spelling. With a string compare
        // every VM on the node stops being adopted — and stops being swept.
        let f = Fake::new();
        f.fc(100, "vm-1");
        let plain = f.data_dir.to_str().unwrap().to_string();

        let trailing = ProcTable::at(&f.proc_root, &format!("{}/", plain));
        assert_eq!(trailing.identify(100, "vm-1"), Identity::Match);
        assert_eq!(trailing.scan(), vec![FcProc { pid: 100, dir_name: "vm-1".into() }]);

        let link = f.data_dir.parent().unwrap().join("data-link");
        std::os::unix::fs::symlink(&f.data_dir, &link).unwrap();
        let linked = ProcTable::at(&f.proc_root, link.to_str().unwrap());
        assert_eq!(linked.identify(100, "vm-1"), Identity::Match);
        assert_eq!(linked.scan(), vec![FcProc { pid: 100, dir_name: "vm-1".into() }]);
    }

    #[test]
    fn test_identify_resolves_a_relative_socket_path_against_the_process_cwd() {
        let f = Fake::new();
        f.instance("vm-1");
        let p = f.process(100, &["firecracker", "--api-sock", "data/instances/vm-1/fc.sock"]);
        std::os::unix::fs::symlink(f.data_dir.parent().unwrap(), p.join("cwd")).unwrap();
        assert_eq!(f.table().identify(100, "vm-1"), Identity::Match);

        // Relative, and nothing to resolve it against: gone, not ours.
        f.process(101, &["firecracker", "--api-sock", "data/instances/vm-1/fc.sock"]);
        assert_eq!(f.table().identify(101, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_identify_is_unknown_when_the_process_cannot_be_read() {
        // Not "gone": the entry is there and reading it fails for another
        // reason. This must not be mistaken for a dead VMM.
        let f = Fake::new();
        f.instance("vm-1");
        let p = f.proc_root.join("100");
        fs::create_dir_all(p.join("cmdline")).unwrap(); // read() -> EISDIR
        assert_eq!(f.table().identify(100, "vm-1"), Identity::Unknown);
        // ... and such a process is never offered to the sweep.
        assert!(f.table().scan().is_empty());
    }

    #[test]
    fn test_identify_mismatch_when_the_instance_directory_is_gone() {
        let f = Fake::new();
        let p = f.fc(100, "vm-1");
        assert_eq!(f.table().identify(100, "vm-1"), Identity::Match);
        fs::remove_dir_all(f.data_dir.join("instances").join("vm-1")).unwrap();
        assert!(p.exists());
        assert_eq!(f.table().identify(100, "vm-1"), Identity::Mismatch);
    }

    #[test]
    fn test_scan_lists_only_firecrackers_under_this_data_dir() {
        let f = Fake::new();
        f.fc(300, "vm-b");
        f.fc(200, "vm-a");
        f.process(201, &["sleep", "1"]);
        // A second agent's VMM on the same host: another data dir.
        let other = f.data_dir.parent().unwrap().join("other").join("instances").join("vm-a");
        fs::create_dir_all(&other).unwrap();
        f.process(202, &["firecracker", "--api-sock", other.join(SOCK_NAME).to_str().unwrap()]);
        // A socket somewhere deeper than instances/<dir>/ is not an instance socket.
        let nested = f.instance("vm-a").join("sub");
        fs::create_dir_all(&nested).unwrap();
        f.process(203, &["firecracker", "--api-sock", nested.join(SOCK_NAME).to_str().unwrap()]);
        // procfs has non-numeric entries too.
        fs::create_dir_all(f.proc_root.join("self")).unwrap();
        fs::write(f.proc_root.join("uptime"), "1.0 1.0").unwrap();

        assert_eq!(
            f.table().scan(),
            vec![
                FcProc { pid: 200, dir_name: "vm-a".into() },
                FcProc { pid: 300, dir_name: "vm-b".into() },
            ]
        );
    }

    #[test]
    fn test_scan_is_empty_when_there_is_no_instances_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let t = ProcTable::at(tmp.path(), tmp.path().join("nope").to_str().unwrap());
        assert!(t.scan().is_empty());
    }

    #[test]
    fn test_parse_stat_counts_fields_from_the_last_parenthesis() {
        assert_eq!(parse_stat("100 (firecracker) S 1 100 100 0 -1 4194560 12 0"), Some(('S', 4194560)));
        // A command name is free text: spaces and parentheses included.
        assert_eq!(parse_stat("100 (fc) v2 (x)) R 1 100 100 0 -1 4 12 0"), Some(('R', 4)));
        assert_eq!(parse_stat("100 (fc) Z"), None);
        assert_eq!(parse_stat("garbage"), None);
    }

    #[test]
    fn test_exited_means_every_thread_is_gone_not_that_the_name_is() {
        let f = Fake::new();
        let t = f.table();
        // No such process at all.
        assert!(t.exited(100));

        // Alive.
        let p = f.fc(100, "vm-1");
        fs::write(p.join("stat"), "100 (firecracker) S 1 100 100 0 -1 4194560 0 0").unwrap();
        assert!(!t.exited(100));
        assert!(!t.exiting(100));

        // SIGKILLed a moment ago: no command line any more (so it no longer
        // identifies as a Firecracker), but it is still unmapping the guest's
        // memory and has not closed the tap. NOT exited.
        fs::write(p.join("cmdline"), "").unwrap();
        fs::write(p.join("stat"), "100 (firecracker) R 1 100 100 0 -1 4194564 0 0").unwrap();
        assert_eq!(t.identify(100, "vm-1"), Identity::Mismatch);
        assert!(t.exiting(100));
        assert!(!t.exited(100));

        // The leader is a zombie, a vCPU thread is not done yet: the memory
        // and the descriptors are shared with it. Still NOT exited.
        fs::write(p.join("stat"), "100 (firecracker) Z 1 100 100 0 -1 4194564 0 0").unwrap();
        fs::create_dir_all(p.join("task").join("100")).unwrap();
        fs::create_dir_all(p.join("task").join("103")).unwrap();
        assert!(!t.exited(100));

        // Last thread gone: now it has.
        fs::remove_dir_all(p.join("task").join("103")).unwrap();
        assert!(t.exited(100));
    }

    #[test]
    fn test_in_my_cgroup_compares_cgroup_membership() {
        let f = Fake::new();
        let p_same = f.fc(100, "vm-1");
        let p_other = f.fc(101, "vm-2");
        f.fc(102, "vm-3"); // no cgroup file at all
        fs::create_dir_all(f.proc_root.join("self")).unwrap();
        fs::write(f.proc_root.join("self").join("cgroup"), "0::/system.slice/felucca-agent.service\n").unwrap();
        fs::write(p_same.join("cgroup"), "0::/system.slice/felucca-agent.service\n").unwrap();
        fs::write(p_other.join("cgroup"), "0::/user.slice/user-1000.slice/session-4.scope\n").unwrap();
        let t = f.table();
        assert!(t.in_my_cgroup(100));
        assert!(!t.in_my_cgroup(101));
        assert!(!t.in_my_cgroup(102));
        assert!(!t.in_my_cgroup(999));
    }
}
