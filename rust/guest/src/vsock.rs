//! Minimal AF_VSOCK listener built directly on libc.
//!
//! Wraps a listening vsock socket and yields connected streams as owned file
//! descriptors that implement `Read + Write`, so the rest of the server stays
//! transport-agnostic.

use std::io::{self, Read, Write};
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};

/// Listen on any CID. Mirrors `VMADDR_CID_ANY`.
const VMADDR_CID_ANY: u32 = 0xFFFF_FFFF;

/// A bound + listening AF_VSOCK socket.
pub struct VsockListener {
    fd: OwnedFd,
}

impl VsockListener {
    /// Create a vsock socket, bind to `(VMADDR_CID_ANY, port)`, and listen.
    /// Returns an `io::Error` if the vsock device is unavailable (e.g. old VMs),
    /// which the caller treats as retryable.
    pub fn bind(port: u32, backlog: i32) -> io::Result<Self> {
        // socket(AF_VSOCK, SOCK_STREAM, 0)
        let raw: RawFd = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
        if raw < 0 {
            return Err(io::Error::last_os_error());
        }
        // Take ownership immediately so any early return closes the fd.
        let fd = unsafe { OwnedFd::from_raw_fd(raw) };

        let mut addr: libc::sockaddr_vm = unsafe { mem::zeroed() };
        addr.svm_family = libc::AF_VSOCK as libc::sa_family_t;
        addr.svm_cid = VMADDR_CID_ANY;
        addr.svm_port = port;

        let rc = unsafe {
            libc::bind(
                fd.as_raw_fd(),
                &addr as *const libc::sockaddr_vm as *const libc::sockaddr,
                mem::size_of::<libc::sockaddr_vm>() as libc::socklen_t,
            )
        };
        if rc < 0 {
            return Err(io::Error::last_os_error());
        }

        let rc = unsafe { libc::listen(fd.as_raw_fd(), backlog) };
        if rc < 0 {
            return Err(io::Error::last_os_error());
        }

        Ok(VsockListener { fd })
    }

    /// Block until a connection arrives, returning the accepted stream.
    pub fn accept(&self) -> io::Result<VsockStream> {
        let raw = unsafe { libc::accept(self.fd.as_raw_fd(), std::ptr::null_mut(), std::ptr::null_mut()) };
        if raw < 0 {
            return Err(io::Error::last_os_error());
        }
        let fd = unsafe { OwnedFd::from_raw_fd(raw) };
        Ok(VsockStream { fd })
    }
}

/// An accepted vsock connection.
pub struct VsockStream {
    fd: OwnedFd,
}

impl Read for VsockStream {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = unsafe {
            libc::read(
                self.fd.as_raw_fd(),
                buf.as_mut_ptr() as *mut libc::c_void,
                buf.len(),
            )
        };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

impl Write for VsockStream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let n = unsafe {
            libc::write(
                self.fd.as_raw_fd(),
                buf.as_ptr() as *const libc::c_void,
                buf.len(),
            )
        };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
