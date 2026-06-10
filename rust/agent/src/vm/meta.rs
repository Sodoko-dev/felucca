//! meta.json schema — per-instance persisted metadata.
//!
//! Keys MUST appear in this declaration order (matching the Zig writeMeta output):
//! id, name, dir_id, vcpus, mem_mib, pid, slot, ip, state.
//! Options serialize as explicit null — NO skip_serializing_if.
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
        format!(
            "{{\"id\":{},\"name\":{},\"dir_id\":{},\"vcpus\":{},\"mem_mib\":{},\"pid\":{},\"slot\":{},\"ip\":{},\"state\":{}}}",
            json_str(&self.id),
            json_str(&self.name),
            json_str(&self.dir_id),
            self.vcpus,
            self.mem_mib,
            pid_str,
            slot_str,
            ip_str,
            json_str(self.state.as_str()),
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

        // Round-trip: to_json → parse → same values.
        let json = meta.to_json();
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.id, meta.id);
        assert_eq!(meta2.state, VmState::Sleeping);
        assert_eq!(meta2.pid, None);
        assert_eq!(meta2.slot, Some(3));
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

        let json = meta.to_json();
        let meta2: Meta = serde_json::from_str(&json).expect("round-trip parse");
        assert_eq!(meta2.pid, Some(12345));
        assert_eq!(meta2.state, VmState::Pooled);
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
        };
        let json = meta.to_json();
        // All Option fields must appear as explicit null.
        assert!(json.contains("\"pid\":null"), "pid should be null, got: {}", json);
        assert!(json.contains("\"slot\":null"), "slot should be null, got: {}", json);
        assert!(json.contains("\"ip\":null"), "ip should be null, got: {}", json);
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
        };
        let json = meta.to_json();
        // Keys must appear in order: id, name, dir_id, vcpus, mem_mib, pid, slot, ip, state.
        let id_pos = json.find("\"id\"").unwrap();
        let name_pos = json.find("\"name\"").unwrap();
        let dir_id_pos = json.find("\"dir_id\"").unwrap();
        let vcpus_pos = json.find("\"vcpus\"").unwrap();
        let mem_mib_pos = json.find("\"mem_mib\"").unwrap();
        let pid_pos = json.find("\"pid\"").unwrap();
        let slot_pos = json.find("\"slot\"").unwrap();
        let ip_pos = json.find("\"ip\"").unwrap();
        let state_pos = json.find("\"state\"").unwrap();
        assert!(id_pos < name_pos);
        assert!(name_pos < dir_id_pos);
        assert!(dir_id_pos < vcpus_pos);
        assert!(vcpus_pos < mem_mib_pos);
        assert!(mem_mib_pos < pid_pos);
        assert!(pid_pos < slot_pos);
        assert!(slot_pos < ip_pos);
        assert!(ip_pos < state_pos);
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
}
