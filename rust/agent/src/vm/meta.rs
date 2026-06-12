//! meta.json schema — per-instance persisted metadata.
//!
//! Keys MUST appear in this declaration order (matching the Zig writeMeta output):
//! id, name, dir_id, vcpus, mem_mib, pid, slot, ip, state, vsock[, tenant_id].
//! `vsock` is the v3 addition: optional, serialized LAST for mandatory keys,
//! defaults false when absent so older meta.json files parse unchanged.
//! `tenant_id` is the v4 addition: optional, serialized only when Some (after vsock),
//! absent in older meta.json files → None.
//! `exposes` is the v4 P3 addition: ingress DNAT mappings, serialized only when
//! non-empty (after tenant_id), absent in older meta.json files → empty.
//! `image`/`disk_gb` are the v4 P4 additions: template image name and rootfs
//! grow size, serialized only when Some (after exposes). The default shape
//! (ubuntu-base, no resize) writes None so pre-P4 bytes stay identical.
//! Options serialize as explicit null — NO skip_serializing_if (except tenant_id).
//! State strings: creating|running|paused|stopped|sleeping|error|pooled.

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum VmState {
    Creating,
    Running,
    Paused,
    Stopped,
    Sleeping,
    #[serde(rename = "error")]
    Error,
    Pooled,
}

impl VmState {
    pub fn as_str(&self) -> &'static str {
        match self {
            VmState::Creating => "creating",
            VmState::Running => "running",
            VmState::Paused => "paused",
            VmState::Stopped => "stopped",
            VmState::Sleeping => "sleeping",
            VmState::Error => "error",
            VmState::Pooled => "pooled",
        }
    }
}

impl std::fmt::Display for VmState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl std::str::FromStr for VmState {
    type Err = ();
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "creating" => Ok(VmState::Creating),
            "running" => Ok(VmState::Running),
            "paused" => Ok(VmState::Paused),
            "stopped" => Ok(VmState::Stopped),
            "sleeping" => Ok(VmState::Sleeping),
            "error" => Ok(VmState::Error),
            "pooled" => Ok(VmState::Pooled),
            _ => Ok(VmState::Stopped),
        }
    }
}

/// One ingress DNAT mapping: host node_port → guest_ip:guest_port (v4 P3).
/// Both ports come from our own allocation (never user strings into nft).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ExposeEntry {
    pub guest_port: u16,
    pub node_port: u16,
}

/// On-disk metadata for one VM instance.
/// Field order matches the Zig writeMeta output exactly.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Meta {
    pub id: String,
    pub name: String,
    pub dir_id: String,
    pub vcpus: u32,
    pub mem_mib: u64,
    pub pid: Option<i32>,
    pub slot: Option<u32>,
    pub ip: Option<String>,
    pub state: VmState,
    /// v3: VM has a Firecracker vsock device (guest agent reachable). Optional,
    /// defaults false when absent in older meta.json files. Serialized LAST of
    /// mandatory keys.
    #[serde(default)]
    pub vsock: bool,
    /// v4: optional tenant identifier for nftables isolation. Absent in older
    /// meta.json files → None. Serialized only when Some (after vsock).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tenant_id: Option<String>,
    /// v4 P3: ingress DNAT mappings. Absent in older meta.json files → empty.
    /// Serialized only when non-empty (after tenant_id).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub exposes: Vec<ExposeEntry>,
    /// v4 P4: template image the rootfs was copied from. None means the
    /// default "ubuntu-base" (kept None so pre-P4 meta bytes stay identical).
    /// Serialized only when Some (after exposes).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image: Option<String>,
    /// v4 P4: rootfs grow size in GiB. None means 0 / no resize (kept None so
    /// pre-P4 meta bytes stay identical). Serialized only when Some (after image).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub disk_gb: Option<u32>,
    /// v4 P4: sha256 the image was verified against at create/prewarm time.
    /// None = trusted local file. Part of the pool match shape, so a pooled
    /// VM prewarmed from an old capture is never claimed for a new sha.
    /// Serialized only when Some (after disk_gb) — pre-P4 bytes stay identical.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_sha256: Option<String>,
}

impl Meta {
    /// Serialize to a JSON string with explicit nulls and exact key order.
    /// We build this manually to guarantee key ordering matches the Zig output.
    pub fn to_json(&self) -> String {
        let pid_str = match self.pid {
            Some(p) => p.to_string(),
            None => "null".to_string(),
        };
        let slot_str = match self.slot {
            Some(s) => s.to_string(),
            None => "null".to_string(),
        };
        let ip_str = match &self.ip {
            Some(ip) => format!("\"{}\"", ip),
            None => "null".to_string(),
        };
        let tenant_suffix = match &self.tenant_id {
            Some(t) => format!(",\"tenant_id\":{}", json_str(t)),
            None => String::new(),
        };
        let exposes_suffix = if self.exposes.is_empty() {
            String::new()
        } else {
            let elems: Vec<String> = self.exposes.iter()
                .map(|e| format!("{{\"guest_port\":{},\"node_port\":{}}}", e.guest_port, e.node_port))
                .collect();
            format!(",\"exposes\":[{}]", elems.join(","))
        };
        let image_suffix = match &self.image {
            Some(im) => format!(",\"image\":{}", json_str(im)),
            None => String::new(),
        };
        let disk_suffix = match self.disk_gb {
            Some(d) => format!(",\"disk_gb\":{}", d),
            None => String::new(),
        };
        let sha_suffix = match &self.image_sha256 {
            Some(s) => format!(",\"image_sha256\":{}", json_str(s)),
            None => String::new(),
        };
        format!(
            "{{\"id\":{},\"name\":{},\"dir_id\":{},\"vcpus\":{},\"mem_mib\":{},\"pid\":{},\"slot\":{},\"ip\":{},\"state\":{},\"vsock\":{}{}{}{}{}{}}}",
            json_str(&self.id),
            json_str(&self.name),
            json_str(&self.dir_id),
            self.vcpus,
            self.mem_mib,
            pid_str,
            slot_str,
            ip_str,
            json_str(self.state.as_str()),
            self.vsock,
            tenant_suffix,
            exposes_suffix,
            image_suffix,
            disk_suffix,
            sha_suffix,
        )
    }
}

fn json_str(s: &str) -> String {
    // Minimal JSON string escaping (printable ASCII without special chars expected in all fields).
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
    fn test_meta_json_round_trip_sleeping() {
        // Parse Zig-written fixture: sleeping VM with slot and ip, no pid.
        let fixture = r#"{"id":"sb-1a8dd244-6","name":"v2-verify","dir_id":"sb-1a8dd244-6","vcpus":1,"mem_mib":256,"pid":null,"slot":3,"ip":"10.231.0.5","state":"sleeping"}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse fixture");
        assert_eq!(meta.id, "sb-1a8dd244-6");
        assert_eq!(meta.name, "v2-verify");
        assert_eq!(meta.dir_id, "sb-1a8dd244-6");
        assert_eq!(meta.vcpus, 1);
        assert_eq!(meta.mem_mib, 256);
        assert_eq!(meta.pid, None);
        assert_eq!(meta.slot, Some(3));
        assert_eq!(meta.ip, Some("10.231.0.5".to_string()));
        assert_eq!(meta.state, VmState::Sleeping);
        assert_eq!(meta.tenant_id, None);

        // Round-trip: to_json → parse → same values.
        let json = meta.to_json();
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.id, meta.id);
        assert_eq!(meta2.state, VmState::Sleeping);
        assert_eq!(meta2.pid, None);
        assert_eq!(meta2.slot, Some(3));
        assert_eq!(meta2.tenant_id, None);
    }

    #[test]
    fn test_meta_json_round_trip_pooled() {
        // Parse Zig-written pool fixture.
        let fixture = r#"{"id":"pool-1","name":"pool-1","dir_id":"pool-1","vcpus":1,"mem_mib":256,"pid":12345,"slot":1,"ip":"10.231.0.3","state":"pooled"}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pool fixture");
        assert_eq!(meta.id, "pool-1");
        assert_eq!(meta.pid, Some(12345));
        assert_eq!(meta.slot, Some(1));
        assert_eq!(meta.ip, Some("10.231.0.3".to_string()));
        assert_eq!(meta.state, VmState::Pooled);
        assert_eq!(meta.tenant_id, None);

        let json = meta.to_json();
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.pid, Some(12345));
        assert_eq!(meta2.state, VmState::Pooled);
        assert_eq!(meta2.tenant_id, None);
    }

    #[test]
    fn test_explicit_nulls_in_to_json() {
        let meta = Meta {
            id: "vm-1".into(),
            name: "test".into(),
            dir_id: "vm-1".into(),
            vcpus: 2,
            mem_mib: 512,
            pid: None,
            slot: None,
            ip: None,
            state: VmState::Stopped,
            vsock: false,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        // All Option fields must appear as explicit null.
        assert!(json.contains("\"pid\":null"), "pid should be null, got: {}", json);
        assert!(json.contains("\"slot\":null"), "slot should be null, got: {}", json);
        assert!(json.contains("\"ip\":null"), "ip should be null, got: {}", json);
        // tenant_id absent when None.
        assert!(!json.contains("tenant_id"), "tenant_id must be absent when None, got: {}", json);
    }

    #[test]
    fn test_key_order_in_to_json() {
        let meta = Meta {
            id: "x".into(),
            name: "y".into(),
            dir_id: "x".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: Some(99),
            slot: Some(2),
            ip: Some("10.0.0.4".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        // Keys must appear in order: id, name, dir_id, vcpus, mem_mib, pid, slot, ip, state, vsock.
        let id_pos = json.find("\"id\"").unwrap();
        let name_pos = json.find("\"name\"").unwrap();
        let dir_id_pos = json.find("\"dir_id\"").unwrap();
        let vcpus_pos = json.find("\"vcpus\"").unwrap();
        let mem_mib_pos = json.find("\"mem_mib\"").unwrap();
        let pid_pos = json.find("\"pid\"").unwrap();
        let slot_pos = json.find("\"slot\"").unwrap();
        let ip_pos = json.find("\"ip\"").unwrap();
        let state_pos = json.find("\"state\"").unwrap();
        let vsock_pos = json.find("\"vsock\"").unwrap();
        assert!(id_pos < name_pos);
        assert!(name_pos < dir_id_pos);
        assert!(dir_id_pos < vcpus_pos);
        assert!(vcpus_pos < mem_mib_pos);
        assert!(mem_mib_pos < pid_pos);
        assert!(pid_pos < slot_pos);
        assert!(slot_pos < ip_pos);
        assert!(ip_pos < state_pos);
        // vsock is LAST when tenant_id is None.
        assert!(state_pos < vsock_pos);
        assert!(json.ends_with("\"vsock\":true}"), "vsock must be last when no tenant: {}", json);
    }

    #[test]
    fn test_key_order_with_tenant_id() {
        let meta = Meta {
            id: "x".into(),
            name: "y".into(),
            dir_id: "x".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: Some(99),
            slot: Some(2),
            ip: Some("10.0.0.4".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: Some("acme".into()),
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        let vsock_pos = json.find("\"vsock\"").unwrap();
        let tenant_pos = json.find("\"tenant_id\"").unwrap();
        // tenant_id must come after vsock.
        assert!(vsock_pos < tenant_pos, "tenant_id must be after vsock: {}", json);
        assert!(json.ends_with("\"tenant_id\":\"acme\"}"), "tenant_id must be last: {}", json);
    }

    #[test]
    fn test_state_strings() {
        assert_eq!(VmState::Creating.as_str(), "creating");
        assert_eq!(VmState::Running.as_str(), "running");
        assert_eq!(VmState::Paused.as_str(), "paused");
        assert_eq!(VmState::Stopped.as_str(), "stopped");
        assert_eq!(VmState::Sleeping.as_str(), "sleeping");
        assert_eq!(VmState::Error.as_str(), "error");
        assert_eq!(VmState::Pooled.as_str(), "pooled");
    }

    #[test]
    fn test_parse_zig_fixture_slot3_ip_matches_slot() {
        // Verify the fixture: slot=3 → ip="10.231.0.5" (net_base+2+3 = .5).
        // The spec example in the task mentions ip="10.231.0.3" with slot=3.
        // Let us parse as-is to verify that serde handles the field independently.
        let fixture = r#"{"id":"sb-1a8dd244-6","name":"v2-verify","dir_id":"sb-1a8dd244-6","vcpus":1,"mem_mib":256,"pid":null,"slot":3,"ip":"10.231.0.5","state":"sleeping"}"#;
        let meta: Meta = serde_json::from_str(fixture).unwrap();
        assert_eq!(meta.slot, Some(3));
        // ip is stored independently from slot in meta (set at creation time).
        assert_eq!(meta.ip, Some("10.231.0.5".into()));
    }

    #[test]
    fn test_old_meta_without_vsock_parses_false() {
        // Pre-v3 file (no vsock key) must parse with vsock defaulting to false.
        let fixture = r#"{"id":"vm-old","name":"vm-old","dir_id":"vm-old","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"sleeping"}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pre-v3 fixture");
        assert!(!meta.vsock, "absent vsock must default to false");
        assert_eq!(meta.tenant_id, None, "absent tenant_id must default to None");
    }

    #[test]
    fn test_vsock_round_trip_true() {
        let meta = Meta {
            id: "vm-v3".into(),
            name: "vm-v3".into(),
            dir_id: "vm-v3".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: Some(42),
            slot: Some(0),
            ip: Some("10.231.0.2".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        assert!(json.contains("\"vsock\":true"), "vsock should serialize true: {}", json);
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert!(meta2.vsock);
        assert_eq!(meta2.state, VmState::Running);
        assert_eq!(meta2.tenant_id, None);
    }

    #[test]
    fn test_vsock_false_serializes_explicit() {
        let meta = Meta {
            id: "vm-x".into(),
            name: "vm-x".into(),
            dir_id: "vm-x".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: None,
            slot: None,
            ip: None,
            state: VmState::Stopped,
            vsock: false,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        assert!(json.contains("\"vsock\":false"), "vsock:false must be explicit: {}", json);
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert!(!meta2.vsock);
    }

    #[test]
    fn test_tenant_id_serialized_when_some() {
        let meta = Meta {
            id: "vm-t".into(),
            name: "vm-t".into(),
            dir_id: "vm-t".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: None,
            slot: None,
            ip: None,
            state: VmState::Running,
            vsock: false,
            tenant_id: Some("acme-corp".into()),
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        assert!(json.contains("\"tenant_id\":\"acme-corp\""), "tenant_id must appear: {}", json);
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.tenant_id, Some("acme-corp".into()));
    }

    #[test]
    fn test_tenant_id_absent_when_none() {
        let meta = Meta {
            id: "vm-nt".into(),
            name: "vm-nt".into(),
            dir_id: "vm-nt".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: None,
            slot: None,
            ip: None,
            state: VmState::Running,
            vsock: false,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        assert!(!json.contains("tenant_id"), "tenant_id must not appear when None: {}", json);
    }

    #[test]
    fn test_old_meta_without_tenant_parses_none() {
        // Pre-v4 file (no tenant_id key) must parse with tenant_id defaulting to None.
        let fixture = r#"{"id":"vm-v3","name":"vm-v3","dir_id":"vm-v3","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"running","vsock":true}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pre-v4 fixture");
        assert!(meta.vsock);
        assert_eq!(meta.tenant_id, None, "absent tenant_id must default to None");
    }

    #[test]
    fn test_tenant_id_round_trip() {
        let meta = Meta {
            id: "vm-tr".into(),
            name: "vm-tr".into(),
            dir_id: "vm-tr".into(),
            vcpus: 2,
            mem_mib: 512,
            pid: Some(1234),
            slot: Some(3),
            ip: Some("10.231.0.5".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: Some("tenant-42".into()),
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip");
        assert_eq!(meta2.tenant_id, Some("tenant-42".into()));
        assert_eq!(meta2.vsock, true);
        assert_eq!(meta2.state, VmState::Running);
    }

    #[test]
    fn test_old_meta_without_exposes_parses_empty() {
        // Pre-P3 file (no exposes key) must parse with exposes defaulting to empty.
        let fixture = r#"{"id":"vm-v3","name":"vm-v3","dir_id":"vm-v3","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"running","vsock":true,"tenant_id":"acme"}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pre-P3 fixture");
        assert!(meta.exposes.is_empty(), "absent exposes must default to empty");
    }

    #[test]
    fn test_exposes_absent_when_empty() {
        let meta = Meta {
            id: "vm-e".into(),
            name: "vm-e".into(),
            dir_id: "vm-e".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: None,
            slot: None,
            ip: None,
            state: VmState::Running,
            vsock: false,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![],
        };
        let json = meta.to_json();
        assert!(!json.contains("exposes"), "exposes must not appear when empty: {}", json);
    }

    #[test]
    fn test_exposes_round_trip() {
        let meta = Meta {
            id: "vm-x".into(),
            name: "vm-x".into(),
            dir_id: "vm-x".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: Some(42),
            slot: Some(0),
            ip: Some("10.231.0.2".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: None,
            image: None,
            disk_gb: None,
            image_sha256: None,
            exposes: vec![
                ExposeEntry { guest_port: 8069, node_port: 20000 },
                ExposeEntry { guest_port: 443, node_port: 20001 },
            ],
        };
        let json = meta.to_json();
        // exposes serialize last, after the mandatory keys.
        assert!(
            json.ends_with("\"exposes\":[{\"guest_port\":8069,\"node_port\":20000},{\"guest_port\":443,\"node_port\":20001}]}"),
            "exposes must serialize in order, last: {}", json
        );
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.exposes, meta.exposes);
    }

    #[test]
    fn test_old_meta_without_image_disk_parses_none() {
        // Pre-P4 file (no image/disk_gb keys) must parse with both None.
        let fixture = r#"{"id":"vm-p3","name":"vm-p3","dir_id":"vm-p3","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"running","vsock":true,"tenant_id":"acme","exposes":[{"guest_port":8069,"node_port":20000}]}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pre-P4 fixture");
        assert_eq!(meta.image, None, "absent image must default to None");
        assert_eq!(meta.disk_gb, None, "absent disk_gb must default to None");
    }

    #[test]
    fn test_default_image_serializes_byte_identical_to_pre_p4() {
        // A default-shape VM (ubuntu-base, no resize → both None) must emit
        // EXACTLY the pre-P4 bytes — no image/disk_gb keys at all.
        let meta = Meta {
            id: "vm-d".into(),
            name: "vm-d".into(),
            dir_id: "vm-d".into(),
            vcpus: 1,
            mem_mib: 256,
            pid: Some(42),
            slot: Some(0),
            ip: Some("10.231.0.2".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: None,
            exposes: vec![],
            image: None,
            disk_gb: None,
            image_sha256: None,
        };
        assert_eq!(
            meta.to_json(),
            r#"{"id":"vm-d","name":"vm-d","dir_id":"vm-d","vcpus":1,"mem_mib":256,"pid":42,"slot":0,"ip":"10.231.0.2","state":"running","vsock":true}"#,
        );
    }

    #[test]
    fn test_image_disk_gb_round_trip() {
        let meta = Meta {
            id: "vm-i".into(),
            name: "vm-i".into(),
            dir_id: "vm-i".into(),
            vcpus: 2,
            mem_mib: 2048,
            pid: Some(7),
            slot: Some(1),
            ip: Some("10.231.0.3".into()),
            state: VmState::Running,
            vsock: true,
            tenant_id: Some("acme".into()),
            exposes: vec![ExposeEntry { guest_port: 8069, node_port: 20000 }],
            image: Some("odoo-v18".into()),
            disk_gb: Some(8),
            image_sha256: None,
        };
        let json = meta.to_json();
        // image and disk_gb serialize AFTER exposes, in that order.
        let exposes_pos = json.find("\"exposes\"").unwrap();
        let image_pos = json.find("\"image\"").unwrap();
        let disk_pos = json.find("\"disk_gb\"").unwrap();
        assert!(exposes_pos < image_pos, "image must come after exposes: {}", json);
        assert!(image_pos < disk_pos, "disk_gb must come after image: {}", json);
        assert!(json.ends_with("\"image\":\"odoo-v18\",\"disk_gb\":8}"), "got: {}", json);
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.image, Some("odoo-v18".into()));
        assert_eq!(meta2.disk_gb, Some(8));
        assert_eq!(meta2.exposes, meta.exposes);
        assert_eq!(meta2.tenant_id, Some("acme".into()));
    }

    #[test]
    fn test_image_sha256_round_trip_and_order() {
        let sha = "a".repeat(64);
        let meta = Meta {
            id: "vm-s".into(),
            name: "vm-s".into(),
            dir_id: "vm-s".into(),
            vcpus: 2,
            mem_mib: 2048,
            pid: Some(7),
            slot: Some(1),
            ip: Some("10.231.0.3".into()),
            state: VmState::Pooled,
            vsock: true,
            tenant_id: None,
            exposes: vec![],
            image: Some("odoo-v18".into()),
            disk_gb: Some(8),
            image_sha256: Some(sha.clone()),
        };
        let json = meta.to_json();
        // image_sha256 serializes LAST, after disk_gb.
        assert!(json.ends_with(&format!("\"disk_gb\":8,\"image_sha256\":\"{}\"}}", sha)), "got: {}", json);
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.image_sha256, Some(sha));
    }

    #[test]
    fn test_image_sha256_absent_when_none_and_pre_p4_parse() {
        // Pre-P4 fixture (no image_sha256 key) must parse to None.
        let fixture = r#"{"id":"vm-p","name":"vm-p","dir_id":"vm-p","vcpus":1,"mem_mib":256,"pid":null,"slot":0,"ip":"10.231.0.2","state":"running","vsock":true}"#;
        let meta: Meta = serde_json::from_str(fixture).expect("parse pre-P4 fixture");
        assert_eq!(meta.image_sha256, None);
        // And a None field must not appear in the output (bytes stay pre-P4).
        assert_eq!(meta.to_json(), fixture);
    }
}
