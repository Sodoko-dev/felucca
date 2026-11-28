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

/// The `state` field of a `GET /` response body.
pub fn parse_instance_state(body: &str) -> Option<String> {
    serde_json::from_str::<serde_json::Value>(body)
        .ok()?
        .get("state")?
        .as_str()
        .map(str::to_string)
}

/// GET / — what the VMM says its guest is doing: "Not started", "Running" or
/// "Paused" (Firecracker's `InstanceInfo.state`).
///
/// The agent's own record says what the last completed operation left. This is
/// the other half: an agent that takes over a Firecracker it did not start
/// (ADR-0011) has no other way to learn that a sleep or a fork was cut short
/// after it had paused the guest.
pub async fn get_instance_state(sock: &str) -> Result<String> {
    let (status, body) = request_uds(sock, Method::GET, "/", "").await?;
    if status != 200 {
        return Err(FcError(format!("GET / -> {}: {}", status, body)));
    }
    parse_instance_state(&body).ok_or_else(|| FcError(format!("GET /: no state in {}", body)))
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

/// PATCH /drives/rootfs — point the root drive of a loaded, not yet resumed
/// VM at another image (fork: the child's own copy).
///
/// A snapshot records the drive's absolute path, and `snapshot/load` has no
/// override for it: without this a fork child opens its PARENT's image, and
/// the two guests write over each other. Lab-verified on FC 1.16: 204 on a
/// paused, loaded VM; Firecracker closes the old image and opens the new one;
/// the guest sees one `virtio_blk ... new size` line at the resume.
pub async fn patch_drive_rootfs(sock: &str, rootfs_path: &str) -> Result<()> {
    patch(sock, "/drives/rootfs", &build_drive_rootfs_patch(rootfs_path)).await
}

/// Build the PATCH /drives/rootfs JSON body — used in tests too.
pub fn build_drive_rootfs_patch(rootfs_path: &str) -> String {
    format!("{{\"drive_id\":\"rootfs\",\"path_on_host\":{}}}", json_str(rootfs_path))
}

/// GET /vm/config — the image the root drive is open on now.
pub async fn get_root_drive_path(sock: &str) -> Result<String> {
    let (status, body) = request_uds(sock, Method::GET, "/vm/config", "").await?;
    if status != 200 {
        return Err(FcError(format!("GET /vm/config -> {}: {}", status, body)));
    }
    parse_root_drive_path(&body).ok_or_else(|| FcError(format!("GET /vm/config: no rootfs drive path in {}", body)))
}

/// `path_on_host` of the drive `rootfs` in a `GET /vm/config` body, wherever
/// it is in the list. `None` when there is no such drive or it has no path.
pub fn parse_root_drive_path(body: &str) -> Option<String> {
    serde_json::from_str::<serde_json::Value>(body)
        .ok()?
        .get("drives")?
        .as_array()?
        .iter()
        .find(|d| d.get("drive_id").and_then(|v| v.as_str()) == Some("rootfs"))?
        .get("path_on_host")?
        .as_str()
        .map(str::to_string)
}

/// PUT /network-interfaces/eth0 (only when networking on).
///
/// `guest_mac` pins the NIC's L2 identity to the address the host's per-tap
/// anti-spoof chain matches on. It is a `GuestMac` rather than a string so the
/// two halves cannot drift apart: there is no "let Firecracker pick" body, and
/// therefore no NIC whose address the tap's pin does not know.
pub async fn put_network_interface(sock: &str, tap: &str, guest_mac: &crate::net::GuestMac) -> Result<()> {
    let body = build_network_interface(tap, guest_mac);
    put(sock, "/network-interfaces/eth0", &body).await
}

/// Build network-interfaces JSON body — used in tests too.
pub fn build_network_interface(tap: &str, guest_mac: &crate::net::GuestMac) -> String {
    format!(
        "{{\"iface_id\":\"eth0\",\"host_dev_name\":{},\"guest_mac\":{}}}",
        json_str(tap),
        json_str(guest_mac.as_str())
    )
}

/// PUT /machine-config
pub async fn put_machine_config(sock: &str, vcpus: u32, mem_mib: u64) -> Result<()> {
    let body = format!("{{\"vcpu_count\":{},\"mem_size_mib\":{}}}", vcpus, mem_mib);
    put(sock, "/machine-config", &body).await
}

/// PUT /vsock — attach a hybrid-vsock device (guest_cid + host UDS path).
/// v3: only emitted at cold boot when the guest-agent marker is present.
pub async fn put_vsock(sock: &str, guest_cid: u32, uds_path: &str) -> Result<()> {
    let body = build_vsock(guest_cid, uds_path);
    put(sock, "/vsock", &body).await
}

/// Build /vsock JSON body — used in tests too.
pub fn build_vsock(guest_cid: u32, uds_path: &str) -> String {
    format!(
        "{{\"guest_cid\":{},\"uds_path\":{}}}",
        guest_cid,
        json_str(uds_path)
    )
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

/// PATCH /vm, with Firecracker's answer whatever its status: `Ok((status,
/// body))` whenever an HTTP answer came back, `Err` when none did (the VMM
/// died or the reply was lost). A wake tells the two apart (FLU-4 S7'): only a
/// resume Firecracker answered with a refusal is proof that the guest did not
/// run.
pub async fn patch_vm_state_answer(sock: &str, state: &str) -> Result<(u16, String)> {
    let body = format!("{{\"state\":\"{}\"}}", state);
    request_uds(sock, Method::PATCH, "/vm", &body).await
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

/// PUT /snapshot/load {snapshot_path, mem_backend, network_overrides?, vsock_override?, resume_vm}
///
/// `vsock_uds` (v3 fork): when the snapshotted VM had a vsock device, the
/// baked uds_path is the PARENT's absolute path. FC v1.16 re-binds that path on
/// restore, which collides with a still-running parent (EADDRINUSE). Passing a
/// `vsock_override` rebinds the device to the child's own UDS path. (Validated
/// on FC v1.16: load+vsock_override -> 204, child binds its own v.sock.)
///
/// `resume_vm` false leaves the restored guest `Paused` until a
/// `PATCH /vm {"state":"Resumed"}` (wake: the record must say `running` on
/// disk before the guest executes again). Lab-verified on FC 1.16: the load
/// answers 204 with no network or vsock activity, and after the resume both
/// work at once.
pub async fn put_snapshot_load(
    sock: &str,
    vmstate_path: &str,
    mem_path: &str,
    net_on: bool,
    slot: Option<u32>,
    vsock_uds: Option<&str>,
    resume_vm: bool,
) -> Result<()> {
    let body = build_snapshot_load(vmstate_path, mem_path, net_on, slot, vsock_uds, resume_vm);
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
    vsock_uds: Option<&str>,
    resume_vm: bool,
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
    if let Some(uds) = vsock_uds {
        body.push_str(&format!(
            ",\"vsock_override\":{{\"guest_cid\":3,\"uds_path\":{}}}",
            json_str(uds)
        ));
    }
    body.push_str(&format!(",\"resume_vm\":{}}}", resume_vm));
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
    fn test_build_network_interface_pins_the_guest_mac() {
        // Was: the NIC was attached with no guest_mac, so nothing fixed an L2
        // identity for the host's per-tap filter to match.
        let body = build_network_interface("hth-3", &crate::net::guest_mac_for_slot(3));
        assert_eq!(
            body,
            r#"{"iface_id":"eth0","host_dev_name":"hth-3","guest_mac":"02:48:54:00:00:03"}"#
        );
    }

    #[test]
    fn test_build_network_interface_always_carries_a_mac() {
        // There is no MAC-less body any more: the argument is a GuestMac, which
        // has no empty inhabitant, so a NIC can never be attached with an
        // address its tap's pin does not know about.
        for slot in [0u32, 1, 7, 253] {
            let body = build_network_interface(
                &crate::net::tap_name(slot),
                &crate::net::guest_mac_for_slot(slot),
            );
            assert!(
                body.contains(&format!(
                    "\"guest_mac\":\"{}\"",
                    crate::net::guest_mac_for_slot(slot)
                )),
                "slot {slot}: {body}"
            );
        }
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
            None,
            true,
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
            None,
            true,
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
            None,
            true,
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/a/vmstate.bin","mem_backend":{"backend_path":"/a/mem.bin","backend_type":"File"},"resume_vm":true}"#
        );
    }

    #[test]
    fn test_build_vsock() {
        // Exact body the cold-boot path PUTs to /vsock.
        let body = build_vsock(3, "/srv/ignis/instances/vm-1/v.sock");
        assert_eq!(
            body,
            r#"{"guest_cid":3,"uds_path":"/srv/ignis/instances/vm-1/v.sock"}"#
        );
    }

    #[test]
    fn test_build_snapshot_load_with_vsock_override() {
        // Fork restore: vsock_override rebinds the child's own UDS path.
        let body = build_snapshot_load(
            "/srv/ignis/instances/child-1/vmstate.bin",
            "/srv/ignis/instances/child-1/mem.bin",
            true,
            Some(3),
            Some("/srv/ignis/instances/child-1/v.sock"),
            true,
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/srv/ignis/instances/child-1/vmstate.bin","mem_backend":{"backend_path":"/srv/ignis/instances/child-1/mem.bin","backend_type":"File"},"network_overrides":[{"iface_id":"eth0","host_dev_name":"hth-3"}],"vsock_override":{"guest_cid":3,"uds_path":"/srv/ignis/instances/child-1/v.sock"},"resume_vm":true}"#
        );
    }

    #[test]
    fn test_build_snapshot_load_vsock_override_no_net() {
        let body = build_snapshot_load(
            "/a/vmstate.bin",
            "/a/mem.bin",
            false,
            None,
            Some("/a/v.sock"),
            true,
        );
        assert_eq!(
            body,
            r#"{"snapshot_path":"/a/vmstate.bin","mem_backend":{"backend_path":"/a/mem.bin","backend_type":"File"},"vsock_override":{"guest_cid":3,"uds_path":"/a/v.sock"},"resume_vm":true}"#
        );
    }

    #[test]
    fn test_build_drive_rootfs_patch() {
        // Fork: the child's drive is pointed at its own image before it runs.
        assert_eq!(
            build_drive_rootfs_patch("/srv/ignis/instances/child-1/rootfs.ext4"),
            r#"{"drive_id":"rootfs","path_on_host":"/srv/ignis/instances/child-1/rootfs.ext4"}"#
        );
        // The path is escaped, not pasted.
        assert_eq!(
            build_drive_rootfs_patch("/a \"b\"\\c/rootfs.ext4"),
            r#"{"drive_id":"rootfs","path_on_host":"/a \"b\"\\c/rootfs.ext4"}"#
        );
    }

    #[test]
    fn test_parse_root_drive_path() {
        // As Firecracker 1.16 answers (lab, round 4), trimmed.
        let one = r#"{"balloon":null,"drives":[{"drive_id":"rootfs","partuuid":null,"is_root_device":true,"cache_type":"Unsafe","is_read_only":false,"path_on_host":"/srv/ignis/instances/kid/rootfs.ext4","rate_limiter":null,"io_engine":"Sync","socket":null}],"machine-config":{"vcpu_count":1,"mem_size_mib":256}}"#;
        assert_eq!(parse_root_drive_path(one).as_deref(), Some("/srv/ignis/instances/kid/rootfs.ext4"));
        // More than one drive, the root drive not first: found by its id.
        let two = r#"{"drives":[{"drive_id":"data","path_on_host":"/d.img"},{"drive_id":"rootfs","path_on_host":"/r.img"}]}"#;
        assert_eq!(parse_root_drive_path(two).as_deref(), Some("/r.img"));
        // Escaped characters come back as the path they stand for.
        let esc = r#"{"drives":[{"drive_id":"rootfs","path_on_host":"/a \"b\"\\c\u00e9/rootfs.ext4"}]}"#;
        assert_eq!(parse_root_drive_path(esc).as_deref(), Some("/a \"b\"\\c\u{e9}/rootfs.ext4"));
        // Nothing to compare: no path, no rootfs drive, no drives, not JSON.
        for body in [
            r#"{"drives":[{"drive_id":"rootfs"}]}"#,
            r#"{"drives":[{"drive_id":"rootfs","path_on_host":null}]}"#,
            r#"{"drives":[{"drive_id":"data","path_on_host":"/d.img"}]}"#,
            r#"{"drives":[]}"#,
            r#"{"machine-config":{}}"#,
            "not json",
            "",
        ] {
            assert_eq!(parse_root_drive_path(body), None, "{body}");
        }
    }

    #[test]
    fn test_build_snapshot_load_without_resume() {
        // Wake loads paused and resumes only once the record says `running`.
        let body = build_snapshot_load("/a/vmstate.bin", "/a/mem.bin", true, Some(3), None, false);
        assert_eq!(
            body,
            r#"{"snapshot_path":"/a/vmstate.bin","mem_backend":{"backend_path":"/a/mem.bin","backend_type":"File"},"network_overrides":[{"iface_id":"eth0","host_dev_name":"hth-3"}],"resume_vm":false}"#
        );
    }
}
