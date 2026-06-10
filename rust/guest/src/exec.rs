//! Command execution with a wall-clock timeout and bounded output capture.

use std::io::Read;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use crate::protocol::MAX_OUTPUT_BYTES;

/// Exit code reported when the command is SIGKILLed for exceeding its timeout.
const TIMEOUT_EXIT_CODE: i32 = 124;

/// Result of a finished (or killed) command.
pub struct ExecOutcome {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
    pub truncated: bool,
}

/// Spawn `cmd[0]` with `cmd[1..]`, stdin null, capturing stdout/stderr. Enforces
/// `timeout`; on expiry the child is SIGKILLed and `exit_code` is 124. Output on
/// each stream is captured lossily as UTF-8 and capped at 1 MiB (the `truncated`
/// flag is set if either stream was cut).
///
/// `cmd` must be non-empty; the caller validates this.
pub fn run_exec(cmd: &[String], timeout: Duration) -> std::io::Result<ExecOutcome> {
    use std::os::unix::process::CommandExt;

    let mut command = Command::new(&cmd[0]);
    command
        .args(&cmd[1..])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // Put the child in its own process group so a timeout can SIGKILL the whole
    // tree (e.g. `sh -c "sleep 30"` where sleep would otherwise survive a kill
    // of sh and hold the output pipes open, stalling our reader threads).
    unsafe {
        command.pre_exec(|| {
            if libc::setpgid(0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let mut child = command.spawn()?;

    let pid = child.id();

    // Drain stdout/stderr on dedicated threads so a full pipe buffer can never
    // deadlock the timeout wait.
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    let out_handle = stdout.map(|s| thread::spawn(move || read_capped(s)));
    let err_handle = stderr.map(|s| thread::spawn(move || read_capped(s)));

    // Wait for the child in a helper thread; the main thread enforces the
    // timeout. The waiter owns the Child and sends back the exit status.
    let (tx, rx) = mpsc::channel();
    let waiter = thread::spawn(move || {
        let status = child.wait();
        // Reap is implicit in wait(); send the resolved exit code.
        let code = status_to_code(&status);
        let _ = tx.send(code);
    });

    let exit_code = match rx.recv_timeout(timeout) {
        Ok(code) => code,
        Err(mpsc::RecvTimeoutError::Timeout) => {
            // Exceeded the deadline: SIGKILL the child's process group, then let
            // the waiter observe the death and join.
            kill_pid(pid);
            // Drain the now-resolved status so the channel/thread shut down
            // cleanly; we ignore the real code and report 124.
            let _ = rx.recv();
            TIMEOUT_EXIT_CODE
        }
        Err(mpsc::RecvTimeoutError::Disconnected) => {
            return Err(std::io::Error::new(
                std::io::ErrorKind::Other,
                "wait thread disconnected",
            ));
        }
    };
    let _ = waiter.join();

    // Joining the reader threads only after the child has exited / been killed
    // guarantees the pipes are closed and these reads terminate.
    let (stdout_buf, out_trunc) = out_handle
        .map(|h| h.join().unwrap_or((Vec::new(), false)))
        .unwrap_or((Vec::new(), false));
    let (stderr_buf, err_trunc) = err_handle
        .map(|h| h.join().unwrap_or((Vec::new(), false)))
        .unwrap_or((Vec::new(), false));

    Ok(ExecOutcome {
        exit_code,
        stdout: String::from_utf8_lossy(&stdout_buf).into_owned(),
        stderr: String::from_utf8_lossy(&stderr_buf).into_owned(),
        truncated: out_trunc || err_trunc,
    })
}

/// SIGKILL the child's whole process group (the child is its own group leader,
/// so the pgid equals its pid). Best-effort; a vanished group is fine. Killing
/// the group ensures descendants (e.g. `sleep` under `sh -c`) die too, closing
/// the output pipes so the reader threads return promptly.
fn kill_pid(pid: u32) {
    unsafe {
        libc::kill(-(pid as libc::pid_t), libc::SIGKILL);
        // Also signal the leader directly in case the group setup raced.
        libc::kill(pid as libc::pid_t, libc::SIGKILL);
    }
}

/// Read up to `MAX_OUTPUT_BYTES` from `r`, then drain and discard the rest so the
/// writer never blocks. Returns the captured bytes and whether truncation occurred.
fn read_capped<R: Read>(mut r: R) -> (Vec<u8>, bool) {
    let mut buf = Vec::with_capacity(8192);
    let mut chunk = [0u8; 8192];
    let mut truncated = false;
    loop {
        match r.read(&mut chunk) {
            Ok(0) => break,
            Ok(n) => {
                if buf.len() < MAX_OUTPUT_BYTES {
                    let room = MAX_OUTPUT_BYTES - buf.len();
                    let take = room.min(n);
                    buf.extend_from_slice(&chunk[..take]);
                    if take < n {
                        truncated = true;
                    }
                } else {
                    truncated = true;
                }
            }
            Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => break,
        }
    }
    (buf, truncated)
}

#[cfg(unix)]
fn status_to_code(status: &std::io::Result<std::process::ExitStatus>) -> i32 {
    use std::os::unix::process::ExitStatusExt;
    match status {
        Ok(s) => {
            if let Some(code) = s.code() {
                code
            } else if let Some(sig) = s.signal() {
                128 + sig
            } else {
                -1
            }
        }
        Err(_) => -1,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sh(args: &[&str]) -> Vec<String> {
        args.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn captures_stdout_and_exit_zero() {
        let out = run_exec(&sh(&["/bin/sh", "-c", "echo hi"]), Duration::from_secs(5)).unwrap();
        assert_eq!(out.exit_code, 0);
        assert_eq!(out.stdout, "hi\n");
        assert_eq!(out.stderr, "");
        assert!(!out.truncated);
    }

    #[test]
    fn reports_nonzero_exit() {
        let out = run_exec(&sh(&["/bin/sh", "-c", "exit 7"]), Duration::from_secs(5)).unwrap();
        assert_eq!(out.exit_code, 7);
    }

    #[test]
    fn captures_stderr() {
        let out = run_exec(
            &sh(&["/bin/sh", "-c", "echo oops 1>&2"]),
            Duration::from_secs(5),
        )
        .unwrap();
        assert_eq!(out.stderr, "oops\n");
    }

    #[test]
    fn timeout_kills_and_reports_124() {
        let start = std::time::Instant::now();
        let out = run_exec(
            &sh(&["/bin/sh", "-c", "sleep 30"]),
            Duration::from_millis(300),
        )
        .unwrap();
        assert_eq!(out.exit_code, TIMEOUT_EXIT_CODE);
        // Must not have actually waited the full 30s.
        assert!(start.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn truncates_oversized_output() {
        // Emit ~2 MiB of zeros; capture must cap at 1 MiB and set truncated.
        let out = run_exec(
            &sh(&[
                "/bin/sh",
                "-c",
                "dd if=/dev/zero bs=1024 count=2048 2>/dev/null",
            ]),
            Duration::from_secs(15),
        )
        .unwrap();
        assert!(out.truncated, "expected truncation");
        assert!(out.stdout.len() <= MAX_OUTPUT_BYTES);
    }

    #[test]
    fn spawn_failure_is_err() {
        let res = run_exec(
            &sh(&["/nonexistent/definitely-not-a-binary-xyz"]),
            Duration::from_secs(5),
        );
        assert!(res.is_err());
    }

    #[test]
    fn signal_death_maps_to_128_plus_signal() {
        // Kill self with SIGTERM (15) → 128 + 15 = 143.
        let out = run_exec(
            &sh(&["/bin/sh", "-c", "kill -TERM $$"]),
            Duration::from_secs(5),
        )
        .unwrap();
        assert_eq!(out.exit_code, 143);
    }
}
