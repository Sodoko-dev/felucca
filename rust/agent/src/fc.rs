//! Firecracker API client over a per-VM Unix domain socket.
//!
//! Port of the fc/UDS calls in backend/src/agent/vm.zig.
//! All PUT/PATCH bodies match the Zig byte-for-byte.

use hyperlocal::{UnixClientExt, Uri as UnixUri};
use hyper::body::Bytes;
use hyper::{Method, Request};
use hyper_util::client::legacy::Client;
use http_body_util::{BodyExt, Full};

#[derive(Debug)]
pub struct FcError(pub String);

impl std::fmt::Display for FcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "FcError: {}", self.0)
    }
}
impl std::error::Error for FcError {}

pub type Result<T> = std::result::Result<T, FcError>;

/// Build a Hyper client that talks over a Unix domain socket.
fn make_client() -> Client<hyperlocal::UnixConnector, Full<Bytes>> {
    Client::unix()
}

async fn request_uds(sock: &str, method: Method, path: &str, body: &str) -> Result<(u16, String)> {
    let client = make_client();
    let uri = UnixUri::new(sock, path);

    let req = Request::builder()
        .method(method)
        .uri(uri)
        .header("Content-Type", "application/json")
        .header("Content-Length", body.len().to_string())
        .body(Full::new(Bytes::copy_from_slice(body.as_bytes())))
        .map_err(|e| FcError(format!("build request: {e}")))?;

    let resp = client.request(req).await
        .map_err(|e| FcError(format!("send request: {e}")))?;

    let status = resp.status().as_u16();
    let body_bytes = resp.into_body().collect().await
        .map_err(|e| FcError(format!("read body: {e}")))?
        .to_bytes();
    let body_str = String::from_utf8_lossy(&body_bytes).into_owned();
    Ok((status, body_str))
}

async fn put(sock: &str, path: &str, body: &str) -> Result<()> {
    let (status, resp_body) = request_uds(sock, Method::PUT, path, body).await?;
    if status >= 300 {
        return Err(FcError(format!("PUT {} -> {}: {}", path, status, resp_body)));
    }
    Ok(())
}

async fn patch(sock: &str, path: &str, body: &str) -> Result<()> {
    let (status, resp_body) = request_uds(sock, Method::PATCH, path, body).await?;
    if status >= 300 {
        return Err(FcError(format!("PATCH {} -> {}: {}", path, status, resp_body)));
    }
    Ok(())
}

/// PUT /boot-source — kernel path + boot args.
/// When net_on and ip is Some, injects `ip=<ip>::<gw>:<mask>::eth0:off` boot arg.
pub async fn put_boot_source(
    sock: &str,
    kernel: &str,
    net_on: bool,
    ip: Option<&str>,
    gw: &str,
    mask: &str,
) -> Result<()> {
    let body = build_boot_source(kernel, net_on, ip, gw, mask);
    put(sock, "/boot-source", &body).await
}

/// Build boot-source JSON body — used in tests too.
pub fn build_boot_source(
    kernel: &str,
    net_on: bool,
    ip: Option<&str>,
    gw: &str,
    mask: &str,
) -> String {
    let kernel_json = json_str(kernel);
    if net_on {
        if let Some(ip) = ip {
            let boot_args = format!(
                "console=ttyS0 reboot=k panic=1 root=/dev/vda rw ip={}::{}:{}::eth0:off",
                ip, gw, mask
            );
            return format!(
                "{{\"kernel_image_path\":{},\"boot_args\":{}}}",
                kernel_json,
                json_str(&boot_args)
            );
        }
    }
    format!(
        "{{\"kernel_image_path\":{},\"boot_args\":\"console=ttyS0 reboot=k panic=1 root=/dev/vda rw\"}}",
        kernel_json
    )
}

/// PUT /drives/rootfs
pub async fn put_drive_rootfs(sock: &str, rootfs_path: &str) -> Result<()> {
    let body = format!(
        "{{\"drive_id\":\"rootfs\",\"path_on_host\":{},\"is_root_device\":true,\"is_read_only\":false}}",
        json_str(rootfs_path)
    );
    put(sock, "/drives/rootfs", &body).await
}

/// PUT /network-interfaces/eth0 (only when networking on).
pub async fn put_network_interface(sock: &str, tap: &str) -> Result<()> {
    let body = format!(
        "{{\"iface_id\":\"eth0\",\"host_dev_name\":{}}}",
        json_str(tap)
    );
    put(sock, "/network-interfaces/eth0", &body).await
}

/// PUT /machine-config
pub async fn put_machine_config(sock: &str, vcpus: u32, mem_mib: u64) -> Result<()> {
    let body = format!("{{\"vcpu_count\":{},\"mem_size_mib\":{}}}", vcpus, mem_mib);
    put(sock, "/machine-config", &body).await
}

/// PUT /actions InstanceStart
pub async fn put_instance_start(sock: &str) -> Result<()> {
    put(sock, "/actions", "{\"action_type\":\"InstanceStart\"}").await
}

/// PATCH /vm {"state":"Paused"|"Resumed"}
pub async fn patch_vm_state(sock: &str, state: &str) -> Result<()> {
    let body = format!("{{\"state\":\"{}\"}}", state);
    patch(sock, "/vm", &body).await
}

/// PUT /snapshot/create {snapshot_type:"Full", snapshot_path, mem_file_path}
pub async fn put_snapshot_create(sock: &str, vmstate_path: &str, mem_path: &str) -> Result<()> {
    let body = build_snapshot_create(vmstate_path, mem_path);
    let (status, resp_body) = request_uds(sock, Method::PUT, "/snapshot/create", &body).await?;
    if status >= 300 {
        return Err(FcError(format!("snapshot/create -> {}: {}", status, resp_body)));
    }
    Ok(())
}

/// Build snapshot/create JSON body — used in tests too.
pub fn build_snapshot_create(vmstate_path: &str, mem_path: &str) -> String {
    format!(
        "{{\"snapshot_type\":\"Full\",\"snapshot_path\":{},\"mem_file_path\":{}}}",
        json_str(vmstate_path),
        json_str(mem_path)
    )
}

/// PUT /snapshot/load {snapshot_path, mem_backend, resume_vm:true, network_overrides?}
pub async fn put_snapshot_load(
    sock: &str,
    vmstate_path: &str,
    mem_path: &str,
    net_on: bool,
    slot: Option<u32>,
) -> Result<()> {
    let body = build_snapshot_load(vmstate_path, mem_path, net_on, slot);
    let (status, resp_body) = request_uds(sock, Method::PUT, "/snapshot/load", &body).await?;
    if status >= 300 {
        return Err(FcError(format!("snapshot/load -> {}: {}", status, resp_body)));
    }
    Ok(())
}

/// Build snapshot/load JSON body — used in tests too.
pub fn build_snapshot_load(
    vmstate_path: &str,
    mem_path: &str,
    net_on: bool,
    slot: Option<u32>,
) -> String {
    let mut body = format!(
        "{{\"snapshot_path\":{},\"mem_backend\":{{\"backend_path\":{},\"backend_type\":\"File\"}}",
        json_str(vmstate_path),
        json_str(mem_path)
    );
    if net_on {
        if let Some(s) = slot {
            let tap = crate::net::tap_name(s);
            body.push_str(&format!(
                ",\"network_overrides\":[{{\"iface_id\":\"eth0\",\"host_dev_name\":{}}}]",
                json_str(&tap)
            ));
        }
    }
    body.push_str(",\"resume_vm\":true}");
    body
}

/// Minimal JSON string escaping.
pub fn json_str(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_build_boot_source_with_net_and_ip() {
        let body = build_boot_source(
            "/srv/ignis/kernels/vmlinux",
            true,
            Some("10.231.0.5"),
            "10.231.0.1",
            "255.255.255.0",
        );
        assert_eq!(
            body,
            r#"{"kernel_image_path":"/srv/ignis/kernels/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rw ip=10.231.0.5::10.231.0.1:255.255.255.0::eth0:off"}"#
        );
    }

    #[test]
    fn test_build_boot_source_without_net() {
        let body = build_boot_source(
            "/srv/ignis/kernels/vmlinux",
            false,
            None,
            "",
            "",
        );
        assert_eq!(
            body,
            r#"{"kernel_image_path":"/srv/ignis/kernels/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rw"}"#
        );
    }

    #[test]
    fn test_build_boot_source_net_on_no_ip() {
        // net_on but ip=None → same as no-net boot args
        let body = build_boot_source(
            "/srv/ignis/kernels/vmlinux",
            true,
            None,
            "10.231.0.1",
            "255.255.255.0",
        );
        assert_eq!(
            body,
            r#"{"kernel_image_path":"/srv/ignis/kernels/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rw"}"#
        );
    }

    #[test]
    fn test_build_snapshot_create() {
        let body = build_snapshot_create(
            "/srv/ignis/instances/vm-1/vmstate.bin",
            "/srv/ignis/instances/vm-1/mem.bin",
        );
        assert_eq!(
            body,
            r#"{"snapshot_type":"Full","snapshot_path":"/srv/ignis/instances/vm-1/vmstate.bin","mem_file_path":"/srv/ignis/instances/vm-1/mem.bin"}"#
        );
    }

    #[test]
    fn test_build_snapshot_load_with_network() {
        let body = build_snapshot_load(
            "/srv/ignis/instances/vm-1/vmstate.bin",
            "/srv/ignis/instances/vm-1/mem.bin",
            true,
            Some(3),
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/srv/ignis/instances/vm-1/vmstate.bin","mem_backend":{"backend_path":"/srv/ignis/instances/vm-1/mem.bin","backend_type":"File"},"network_overrides":[{"iface_id":"eth0","host_dev_name":"hth-3"}],"resume_vm":true}"#
        );
    }

    #[test]
    fn test_build_snapshot_load_without_network() {
        let body = build_snapshot_load(
            "/srv/ignis/instances/vm-1/vmstate.bin",
            "/srv/ignis/instances/vm-1/mem.bin",
            false,
            None,
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/srv/ignis/instances/vm-1/vmstate.bin","mem_backend":{"backend_path":"/srv/ignis/instances/vm-1/mem.bin","backend_type":"File"},"resume_vm":true}"#
        );
    }

    #[test]
    fn test_build_snapshot_load_net_on_no_slot() {
        // net_on but slot=None → no network_overrides
        let body = build_snapshot_load(
            "/a/vmstate.bin",
            "/a/mem.bin",
            true,
            None,
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/a/vmstate.bin","mem_backend":{"backend_path":"/a/mem.bin","backend_type":"File"},"resume_vm":true}"#
        );
    }
}
