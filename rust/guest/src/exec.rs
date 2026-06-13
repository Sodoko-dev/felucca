//! Command execution with a wall-clock timeout and bounded output capture.

use std::io::Read;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use crate::protocol::MAX_OUTPUT_BYTES;

/// Exit code reported when the command is SIGKILLed for exceeding its timeout.
const TIMEOUT_EXIT_CODE: i32 = 124;

/// Per-stream output cap in stream mode (16 MiB). Streamed chunks are
/// forwarded, not accumulated, so the buffered path's 1 MiB memory bound
/// doesn't apply — this cap bounds transfer volume (build logs are the
/// use case and routinely exceed 1 MiB).
pub const STREAM_MAX_OUTPUT_BYTES: usize = 16 * 1024 * 1024;

/// Result of a finished (or killed) command.
pub struct ExecOutcome {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
    pub truncated: bool,
}

/// Which output stream a streamed chunk came from.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum StreamKind {
    Stdout,
    Stderr,
}

/// Terminal result of a streamed command (chunks were already emitted).
pub struct StreamEnd {
    pub exit_code: i32,
    pub truncated: bool,
}

/// Spawn `cmd[0]` with `cmd[1..]`, stdin null, capturing stdout/stderr. Enforces
/// `timeout`; on expiry the child is SIGKILLed and `exit_code` is 124. Output on
/// each stream is captured lossily as UTF-8 and capped at 1 MiB (the `truncated`
/// flag is set if either stream was cut).
///
/// `cmd` must be non-empty; the caller validates this.
pub fn run_exec(cmd: &[String], timeout: Duration) -> std::io::Result<ExecOutcome> {
    let mut child = spawn_grouped(cmd)?;

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

/// Spawn `cmd[0]` with `cmd[1..]`, stdin null, stdout/stderr piped, in its own
/// process group so a timeout can SIGKILL the whole tree (e.g. `sh -c "sleep 30"`
/// where sleep would otherwise survive a kill of sh and hold the output pipes
/// open, stalling our reader threads).
fn spawn_grouped(cmd: &[String]) -> std::io::Result<std::process::Child> {
    use std::os::unix::process::CommandExt;

    let mut command = Command::new(&cmd[0]);
    command
        .args(&cmd[1..])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    unsafe {
        command.pre_exec(|| {
            if libc::setpgid(0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    command.spawn()
}

/// Streamed variant of `run_exec`: chunks are handed to `emit` as they arrive
/// (lossy UTF-8, in read order per stream; stdout/stderr interleaving follows
/// arrival order). Enforces `timeout` the same way (SIGKILL the group, exit
/// code 124). Each stream is capped at `STREAM_MAX_OUTPUT_BYTES`; past the cap
/// the reader keeps draining (so the child never blocks) but stops emitting,
/// and the returned `truncated` flag is set.
///
/// An `Err` from `emit` (the peer hung up) kills the child group and returns
/// that error — there is no one left to stream to.
pub fn run_exec_streamed(
    cmd: &[String],
    timeout: Duration,
    emit: &mut dyn FnMut(StreamKind, &str) -> std::io::Result<()>,
) -> std::io::Result<StreamEnd> {
    let mut child = spawn_grouped(cmd)?;
    let pid = child.id();

    enum Msg {
        Chunk(StreamKind, Vec<u8>),
        Eof(StreamKind, bool), // (which stream, truncated?)
        Exited(i32),
    }

    let (tx, rx) = mpsc::channel::<Msg>();

    let spawn_reader = |stream: Option<Box<dyn Read + Send>>, kind: StreamKind, tx: mpsc::Sender<Msg>| {
        stream.map(|mut s| {
            thread::spawn(move || {
                let mut sent: usize = 0;
                let mut truncated = false;
                let mut chunk = [0u8; 8192];
                loop {
                    match s.read(&mut chunk) {
                        Ok(0) => break,
                        Ok(n) => {
                            if sent < STREAM_MAX_OUTPUT_BYTES {
                                let take = (STREAM_MAX_OUTPUT_BYTES - sent).min(n);
                                sent += take;
                                if take < n {
                                    truncated = true;
                                }
                                if tx.send(Msg::Chunk(kind, chunk[..take].to_vec())).is_err() {
                                    break; // main loop gone; stop reading
                                }
                            } else {
                                truncated = true; // keep draining, discard
                            }
                        }
                        Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                        Err(_) => break,
                    }
                }
                let _ = tx.send(Msg::Eof(kind, truncated));
            })
        })
    };

    let stdout: Option<Box<dyn Read + Send>> = child.stdout.take().map(|s| Box::new(s) as _);
    let stderr: Option<Box<dyn Read + Send>> = child.stderr.take().map(|s| Box::new(s) as _);
    let mut pending_eofs = stdout.is_some() as u32 + stderr.is_some() as u32;
    let out_handle = spawn_reader(stdout, StreamKind::Stdout, tx.clone());
    let err_handle = spawn_reader(stderr, StreamKind::Stderr, tx.clone());

    let waiter = {
        let tx = tx.clone();
        thread::spawn(move || {
            let code = status_to_code(&child.wait());
            let _ = tx.send(Msg::Exited(code));
        })
    };
    drop(tx); // main loop's rx disconnects once all senders finish

    let deadline = std::time::Instant::now() + timeout;
    // After a kill (timeout or emit failure), bound the wait for readers/waiter
    // to settle; the group SIGKILL closes the pipes so this resolves promptly.
    let grace = Duration::from_secs(10);
    let mut exit_code: Option<i32> = None;
    let mut truncated = false;
    let mut timed_out = false;
    let mut emit_err: Option<std::io::Error> = None;
    // Per-stream carry of trailing bytes that form an INCOMPLETE UTF-8
    // sequence at a chunk boundary. The buffered path hashes the whole output
    // before one lossy decode; the streamed path decodes per 8 KiB read, so a
    // multibyte char straddling a read would become replacement chars without
    // this carry. Flushed (lossily) when that stream hits EOF.
    let mut out_carry: Vec<u8> = Vec::new();
    let mut err_carry: Vec<u8> = Vec::new();

    while pending_eofs > 0 || exit_code.is_none() {
        let wait_for = if timed_out || emit_err.is_some() {
            grace
        } else {
            deadline.saturating_duration_since(std::time::Instant::now())
        };
        match rx.recv_timeout(wait_for) {
            Ok(Msg::Chunk(kind, bytes)) => {
                if emit_err.is_none() {
                    let carry = match kind {
                        StreamKind::Stdout => &mut out_carry,
                        StreamKind::Stderr => &mut err_carry,
                    };
                    if let Err(e) = emit_utf8(carry, &bytes, false, kind, emit) {
                        emit_err = Some(e);
                        kill_pid(pid);
                    }
                }
            }
            Ok(Msg::Eof(kind, t)) => {
                pending_eofs -= 1;
                truncated |= t;
                // Flush any incomplete trailing bytes lossily (a real
                // truncated multibyte char at true end-of-stream).
                if emit_err.is_none() {
                    let carry = match kind {
                        StreamKind::Stdout => &mut out_carry,
                        StreamKind::Stderr => &mut err_carry,
                    };
                    if !carry.is_empty() {
                        if let Err(e) = emit_utf8(carry, &[], true, kind, emit) {
                            emit_err = Some(e);
                            kill_pid(pid);
                        }
                    }
                }
            }
            Ok(Msg::Exited(code)) => exit_code = Some(code),
            Err(mpsc::RecvTimeoutError::Timeout) => {
                if timed_out || emit_err.is_some() {
                    break; // grace expired; stop waiting for stragglers
                }
                timed_out = true;
                kill_pid(pid);
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
    }

    let _ = waiter.join();
    if let Some(h) = out_handle {
        let _ = h.join();
    }
    if let Some(h) = err_handle {
        let _ = h.join();
    }

    if let Some(e) = emit_err {
        return Err(e);
    }
    let exit_code = if timed_out {
        TIMEOUT_EXIT_CODE
    } else {
        exit_code.unwrap_or(-1)
    };
    Ok(StreamEnd {
        exit_code,
        truncated,
    })
}

/// Append `bytes` to `carry`, emit the longest valid-UTF-8 prefix as a single
/// `&str`, and leave any incomplete trailing multibyte sequence in `carry` for
/// the next chunk. Genuinely invalid bytes (not just incomplete) are passed
/// through `from_utf8_lossy` as replacement chars so the stream never stalls.
/// When `final_flush` is set the whole remainder is emitted lossily (the
/// trailing bytes are a truly truncated char at end-of-stream, not a boundary).
fn emit_utf8(
    carry: &mut Vec<u8>,
    bytes: &[u8],
    final_flush: bool,
    kind: StreamKind,
    emit: &mut dyn FnMut(StreamKind, &str) -> std::io::Result<()>,
) -> std::io::Result<()> {
    carry.extend_from_slice(bytes);
    if carry.is_empty() {
        return Ok(());
    }
    match std::str::from_utf8(carry) {
        Ok(s) => {
            let r = emit(kind, s);
            carry.clear();
            r
        }
        Err(e) => {
            let good = e.valid_up_to();
            // valid prefix is guaranteed UTF-8.
            let prefix = unsafe { std::str::from_utf8_unchecked(&carry[..good]) };
            if !prefix.is_empty() {
                emit(kind, prefix)?;
            }
            match e.error_len() {
                // Incomplete sequence at the end: carry it unless this is the
                // final flush, where it can never be completed.
                None => {
                    let tail: Vec<u8> = carry[good..].to_vec();
                    carry.clear();
                    if final_flush && !tail.is_empty() {
                        return emit(kind, &String::from_utf8_lossy(&tail));
                    }
                    *carry = tail;
                    Ok(())
                }
                // Genuinely invalid bytes mid-stream: emit them lossily and
                // keep going (recurse over whatever follows the bad run).
                Some(bad) => {
                    let invalid: Vec<u8> = carry[good..good + bad].to_vec();
                    let rest: Vec<u8> = carry[good + bad..].to_vec();
                    emit(kind, &String::from_utf8_lossy(&invalid))?;
                    carry.clear();
                    emit_utf8(carry, &rest, final_flush, kind, emit)
                }
            }
        }
    }
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

    /// Collect all streamed chunks into (kind, text) pairs.
    fn collect_streamed(
        cmd: &[String],
        timeout: Duration,
    ) -> (std::io::Result<StreamEnd>, Vec<(StreamKind, String)>) {
        let mut chunks: Vec<(StreamKind, String)> = Vec::new();
        let end = run_exec_streamed(cmd, timeout, &mut |k, s| {
            chunks.push((k, s.to_string()));
            Ok(())
        });
        (end, chunks)
    }

    #[test]
    fn streamed_captures_chunks_and_exit_zero() {
        let (end, chunks) = collect_streamed(
            &sh(&["/bin/sh", "-c", "echo one; echo err 1>&2; echo two"]),
            Duration::from_secs(5),
        );
        let end = end.unwrap();
        assert_eq!(end.exit_code, 0);
        assert!(!end.truncated);
        let stdout: String = chunks
            .iter()
            .filter(|(k, _)| *k == StreamKind::Stdout)
            .map(|(_, s)| s.as_str())
            .collect();
        let stderr: String = chunks
            .iter()
            .filter(|(k, _)| *k == StreamKind::Stderr)
            .map(|(_, s)| s.as_str())
            .collect();
        assert_eq!(stdout, "one\ntwo\n");
        assert_eq!(stderr, "err\n");
    }

    #[test]
    fn streamed_chunks_arrive_incrementally() {
        // Two writes separated by a pause must land in (at least) two chunks —
        // the second cannot have been buffered with the first.
        let (end, chunks) = collect_streamed(
            &sh(&["/bin/sh", "-c", "printf a; sleep 1; printf b"]),
            Duration::from_secs(10),
        );
        assert_eq!(end.unwrap().exit_code, 0);
        let stdout: Vec<&str> = chunks
            .iter()
            .filter(|(k, _)| *k == StreamKind::Stdout)
            .map(|(_, s)| s.as_str())
            .collect();
        assert!(stdout.len() >= 2, "expected >=2 chunks, got {stdout:?}");
        assert_eq!(stdout.concat(), "ab");
    }

    #[test]
    fn emit_utf8_carries_split_multibyte() {
        // "€" is E2 82 AC. Split it across two chunks: the prefix before it
        // emits cleanly, the lone E2 carries, and 82 AC completes it.
        // RefCell so the sink borrows `out` only during each call, leaving it
        // readable for the asserts in between.
        let mut carry = Vec::new();
        let out = std::cell::RefCell::new(String::new());
        let mut sink = |_k: StreamKind, s: &str| {
            out.borrow_mut().push_str(s);
            Ok(())
        };
        emit_utf8(&mut carry, b"ab\xe2", false, StreamKind::Stdout, &mut sink).unwrap();
        assert_eq!(*out.borrow(), "ab"); // the lone lead byte is held back
        emit_utf8(&mut carry, b"\x82\xaccd", false, StreamKind::Stdout, &mut sink).unwrap();
        assert_eq!(*out.borrow(), "ab€cd");
        assert!(carry.is_empty());
    }

    #[test]
    fn emit_utf8_flushes_truncated_tail_lossily() {
        let mut carry = Vec::new();
        let out = std::cell::RefCell::new(String::new());
        let mut sink = |_k: StreamKind, s: &str| {
            out.borrow_mut().push_str(s);
            Ok(())
        };
        // A lone lead byte that never completes (process died mid-char).
        emit_utf8(&mut carry, b"hi\xe2", false, StreamKind::Stdout, &mut sink).unwrap();
        assert_eq!(*out.borrow(), "hi");
        emit_utf8(&mut carry, b"", true, StreamKind::Stdout, &mut sink).unwrap();
        assert_eq!(*out.borrow(), "hi\u{fffd}");
    }

    #[test]
    fn streamed_multibyte_across_reads_intact() {
        // Emit ~12 KiB of '€' (3 bytes each) so the byte stream crosses the
        // 8 KiB read boundary mid-character many times; the reassembled output
        // must be byte-identical to the source (no U+FFFD).
        let n = 4096;
        let (end, chunks) = collect_streamed(
            &sh(&[
                "/bin/sh",
                "-c",
                &format!("for i in $(seq 1 {n}); do printf '\\342\\202\\254'; done"),
            ]),
            Duration::from_secs(15),
        );
        assert_eq!(end.unwrap().exit_code, 0);
        let stdout: String = chunks
            .iter()
            .filter(|(k, _)| *k == StreamKind::Stdout)
            .map(|(_, s)| s.as_str())
            .collect();
        assert_eq!(stdout, "€".repeat(n));
        assert!(!stdout.contains('\u{fffd}'), "no replacement chars");
    }

    #[test]
    fn streamed_nonzero_exit() {
        let (end, _) = collect_streamed(&sh(&["/bin/sh", "-c", "exit 7"]), Duration::from_secs(5));
        assert_eq!(end.unwrap().exit_code, 7);
    }

    #[test]
    fn streamed_timeout_kills_and_reports_124() {
        let start = std::time::Instant::now();
        let (end, chunks) = collect_streamed(
            &sh(&["/bin/sh", "-c", "echo early; sleep 30"]),
            Duration::from_millis(300),
        );
        assert_eq!(end.unwrap().exit_code, TIMEOUT_EXIT_CODE);
        assert!(start.elapsed() < Duration::from_secs(5));
        // Output produced before the deadline was still streamed.
        assert!(chunks.iter().any(|(_, s)| s.contains("early")));
    }

    #[test]
    fn streamed_emit_error_kills_child() {
        let start = std::time::Instant::now();
        let res = run_exec_streamed(
            &sh(&["/bin/sh", "-c", "echo x; sleep 30"]),
            Duration::from_secs(60),
            &mut |_, _| Err(std::io::Error::new(std::io::ErrorKind::BrokenPipe, "gone")),
        );
        assert!(res.is_err(), "emit failure must surface as Err");
        // Must not have waited for the sleep: the child group was killed.
        assert!(start.elapsed() < Duration::from_secs(15));
    }

    #[test]
    fn streamed_spawn_failure_is_err() {
        let res = run_exec_streamed(
            &sh(&["/nonexistent/definitely-not-a-binary-xyz"]),
            Duration::from_secs(5),
            &mut |_, _| Ok(()),
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
