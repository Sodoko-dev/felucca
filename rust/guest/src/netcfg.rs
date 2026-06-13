//! `set_ip`: reconfigure the primary interface via iproute2.
//!
//! The `ip` invocations go through an injectable `IpRunner` so tests can assert
//! the exact argument sequences without ever touching the host network stack.

/// Absolute path to the iproute2 binary inside the guest rootfs.
const IP_BIN: &str = "/sbin/ip";

/// Runs `ip` with the given arguments. Returns `Ok(())` on exit status 0, or an
/// error string describing the failure.
pub trait IpRunner {
    fn run(&self, args: &[&str]) -> Result<(), String>;

    /// Best-effort presence announcement after a successful re-IP. Snapshot-
    /// restored guests answer host-side ARP only after their first transmit;
    /// sending any packet toward the gateway forces that transmit (the kernel
    /// ARPs for the gateway, broadcasting our MAC) so the host's ARP/FDB
    /// entries are live before the first organic packet. Default: no-op
    /// (tests); the production runner sends a throwaway UDP datagram.
    fn announce(&self, _gw: &str) {}
}

/// Production runner: shells out to `/sbin/ip`.
pub struct RealIpRunner;

impl IpRunner for RealIpRunner {
    fn run(&self, args: &[&str]) -> Result<(), String> {
        use std::process::Command;
        let output = Command::new(IP_BIN)
            .args(args)
            .output()
            .map_err(|e| format!("failed to run ip: {e}"))?;
        if output.status.success() {
            Ok(())
        } else {
            let stderr = String::from_utf8_lossy(&output.stderr);
            Err(format!(
                "ip {} failed: {}",
                args.join(" "),
                stderr.trim()
            ))
        }
    }

    fn announce(&self, gw: &str) {
        // One datagram to the discard port; no reply expected, errors ignored.
        // The transmit (and the ARP resolution it triggers) is the payload.
        if let Ok(sock) = std::net::UdpSocket::bind("0.0.0.0:0") {
            let _ = sock.send_to(&[0u8], (gw, 9));
        }
    }
}

/// Validate inputs, then apply the four iproute2 steps in order:
/// flush, add addr, link up, replace default route.
pub fn apply_set_ip<R: IpRunner>(
    runner: &R,
    ip: &str,
    prefix: u8,
    gw: &str,
    dev: &str,
) -> Result<(), String> {
    if !is_dotted_quad(ip) {
        return Err(format!("invalid ip: {ip}"));
    }
    if !is_dotted_quad(gw) {
        return Err(format!("invalid gw: {gw}"));
    }
    if prefix > 32 {
        return Err(format!("invalid prefix: {prefix}"));
    }
    if !is_valid_dev(dev) {
        return Err(format!("invalid dev: {dev}"));
    }

    let cidr = format!("{ip}/{prefix}");

    runner.run(&["addr", "flush", "dev", dev])?;
    runner.run(&["addr", "add", &cidr, "dev", dev])?;
    runner.run(&["link", "set", dev, "up"])?;
    runner.run(&["route", "replace", "default", "via", gw, "dev", dev])?;
    Ok(())
}

/// True iff `s` is four decimal octets (0..=255) separated by dots.
fn is_dotted_quad(s: &str) -> bool {
    let mut parts = 0;
    for octet in s.split('.') {
        parts += 1;
        if octet.is_empty() || octet.len() > 3 || !octet.bytes().all(|b| b.is_ascii_digit()) {
            return false;
        }
        match octet.parse::<u16>() {
            Ok(n) if n <= 255 => {}
            _ => return false,
        }
    }
    parts == 4
}

/// True iff `dev` matches `^[a-z0-9]+$`.
fn is_valid_dev(dev: &str) -> bool {
    !dev.is_empty()
        && dev
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit())
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use std::sync::Mutex;

    /// Test runner that records every `ip` argument vector and always succeeds.
    #[derive(Default)]
    pub struct FakeIpRunner {
        recorded: Mutex<Vec<Vec<String>>>,
        announced: Mutex<Vec<String>>,
    }

    impl FakeIpRunner {
        pub fn calls(&self) -> Vec<Vec<String>> {
            self.recorded.lock().unwrap().clone()
        }

        pub fn announced(&self) -> Vec<String> {
            self.announced.lock().unwrap().clone()
        }
    }

    impl IpRunner for FakeIpRunner {
        fn run(&self, args: &[&str]) -> Result<(), String> {
            self.recorded
                .lock()
                .unwrap()
                .push(args.iter().map(|s| s.to_string()).collect());
            Ok(())
        }

        fn announce(&self, gw: &str) {
            self.announced.lock().unwrap().push(gw.to_string());
        }
    }

    #[test]
    fn dotted_quad_accepts_valid() {
        assert!(is_dotted_quad("10.231.0.7"));
        assert!(is_dotted_quad("0.0.0.0"));
        assert!(is_dotted_quad("255.255.255.255"));
    }

    #[test]
    fn dotted_quad_rejects_invalid() {
        assert!(!is_dotted_quad("10.231.0"));
        assert!(!is_dotted_quad("10.231.0.7.1"));
        assert!(!is_dotted_quad("256.0.0.1"));
        assert!(!is_dotted_quad("10.231.0.a"));
        assert!(!is_dotted_quad(""));
        assert!(!is_dotted_quad("10..0.1"));
        assert!(!is_dotted_quad("10.231.0.7; rm -rf /"));
    }

    #[test]
    fn dev_validation() {
        assert!(is_valid_dev("eth0"));
        assert!(is_valid_dev("ens5"));
        assert!(!is_valid_dev("eth0; reboot"));
        assert!(!is_valid_dev("eth 0"));
        assert!(!is_valid_dev("ETH0"));
        assert!(!is_valid_dev(""));
        assert!(!is_valid_dev("eth-0"));
    }

    #[test]
    fn apply_emits_expected_command_sequence() {
        let runner = FakeIpRunner::default();
        apply_set_ip(&runner, "10.231.0.7", 24, "10.231.0.1", "eth0").unwrap();
        let calls = runner.calls();
        assert_eq!(
            calls,
            vec![
                vec!["addr", "flush", "dev", "eth0"],
                vec!["addr", "add", "10.231.0.7/24", "dev", "eth0"],
                vec!["link", "set", "eth0", "up"],
                vec!["route", "replace", "default", "via", "10.231.0.1", "dev", "eth0"],
            ]
        );
    }

    #[test]
    fn apply_rejects_bad_ip_before_running() {
        let runner = FakeIpRunner::default();
        let err = apply_set_ip(&runner, "999.1.1.1", 24, "10.231.0.1", "eth0").unwrap_err();
        assert!(err.contains("invalid ip"));
        assert!(runner.calls().is_empty(), "must not run ip on bad input");
    }

    #[test]
    fn apply_rejects_bad_gw_and_dev_and_prefix() {
        let runner = FakeIpRunner::default();
        assert!(apply_set_ip(&runner, "10.0.0.2", 24, "bad", "eth0")
            .unwrap_err()
            .contains("invalid gw"));
        assert!(apply_set_ip(&runner, "10.0.0.2", 24, "10.0.0.1", "eth0; x")
            .unwrap_err()
            .contains("invalid dev"));
        assert!(apply_set_ip(&runner, "10.0.0.2", 33, "10.0.0.1", "eth0")
            .unwrap_err()
            .contains("invalid prefix"));
        assert!(runner.calls().is_empty());
    }
}
