//! Library surface for hearth-guest, so integration tests can exercise the
//! per-connection handler over a socket pair without a real vsock device.

pub mod exec;
pub mod netcfg;
pub mod protocol;
pub mod vsock;

pub use protocol::handle_connection;
