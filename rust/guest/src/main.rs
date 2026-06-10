//! hearth-guest: a tiny vsock server that runs inside Firecracker guests.
//!
//! Listens on AF_VSOCK (any CID), port 52. One JSON request per connection:
//! `exec` (run a command) or `set_ip` (reconfigure the primary interface).
//! See docs/API-V3-EXEC.md §1 for the binding protocol.

use std::thread;
use std::time::Duration;

use hearth_guest::handle_connection;
use hearth_guest::vsock::VsockListener;

/// vsock port the guest agent listens on (per the v3 contract).
const VSOCK_PORT: u32 = 52;
/// listen(2) backlog.
const BACKLOG: i32 = 128;
/// Retry interval when the vsock device is not yet available.
const RETRY: Duration = Duration::from_secs(5);

fn main() {
    eprintln!("hearth-guest: starting, listening on vsock port {VSOCK_PORT}");

    // Bind with retry-forever: on old VMs without a vsock device, socket/bind can
    // fail; we must not crash-loop hard (the unit restarts us, but spinning is
    // wasteful and noisy), so wait and retry.
    let listener = loop {
        match VsockListener::bind(VSOCK_PORT, BACKLOG) {
            Ok(l) => break l,
            Err(e) => {
                eprintln!("hearth-guest: vsock bind failed ({e}); retrying in {RETRY:?}");
                thread::sleep(RETRY);
            }
        }
    };

    eprintln!("hearth-guest: ready");
    serve(listener);
}

/// Accept loop: one thread per connection.
fn serve(listener: VsockListener) {
    loop {
        match listener.accept() {
            Ok(stream) => {
                thread::spawn(move || {
                    if let Err(e) = handle_connection(stream) {
                        eprintln!("hearth-guest: connection error: {e}");
                    }
                });
            }
            Err(e) => {
                // Transient accept errors shouldn't kill the server.
                eprintln!("hearth-guest: accept failed: {e}");
                thread::sleep(Duration::from_millis(100));
            }
        }
    }
}
