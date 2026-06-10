//! Sequential guest-IP allocation from a CIDR.
//!
//! Port of backend/src/agent/ipalloc.zig — slot↔IP mapping, .2+slot, lowest-free.
//! The CIDR's `.1` host is the bridge gateway; guests start at `.2`.


#[derive(Debug, Clone, Copy)]
pub struct Cidr {
    /// Network base address in host byte order (e.g. 10.231.0.0 → 0x0AE70000).
    pub base: u32,
    pub prefix: u8,
}

impl Cidr {
    /// Parse "a.b.c.d/n".
    pub fn parse(s: &str) -> Option<Cidr> {
        let slash = s.find('/')?;
        let ip = parse_ip4(&s[..slash])?;
        let prefix: u8 = s[slash + 1..].parse().ok()?;
        if prefix > 32 { return None; }
        Some(Cidr { base: ip, prefix })
    }

    pub fn mask(self) -> u32 {
        if self.prefix == 0 { return 0; }
        !0u32 << (32 - self.prefix as u32)
    }

    pub fn net_base(self) -> u32 {
        self.base & self.mask()
    }

    /// Gateway address (.1) in host byte order.
    pub fn gateway(self) -> u32 {
        self.net_base() + 1
    }

    /// IP (host byte order) for guest slot `idx` (0-based), starting at .2.
    pub fn host_at(self, idx: u32) -> u32 {
        self.net_base() + 2 + idx
    }

    /// Host count usable for guests = 2^(32-prefix) minus network/gw/broadcast.
    pub fn host_count(self) -> u32 {
        let host_bits = 32u32.saturating_sub(self.prefix as u32);
        let total: u64 = 1u64 << host_bits;
        if total <= 3 { return 0; }
        (total - 3) as u32
    }
}

pub fn fmt_ip(v: u32) -> String {
    format!(
        "{}.{}.{}.{}",
        (v >> 24) & 0xff,
        (v >> 16) & 0xff,
        (v >> 8) & 0xff,
        v & 0xff
    )
}

fn parse_ip4(s: &str) -> Option<u32> {
    let parts: Vec<&str> = s.split('.').collect();
    if parts.len() != 4 { return None; }
    let mut out: u32 = 0;
    for part in &parts {
        let b: u8 = part.parse().ok()?;
        out = (out << 8) | b as u32;
    }
    Some(out)
}

/// Tracks which guest slots are in use within a CIDR.
pub struct Allocator {
    pub cidr: Cidr,
    used: Vec<bool>,
}

impl Allocator {
    pub fn new(cidr: Cidr) -> Self {
        let n = cidr.host_count() as usize;
        Allocator { cidr, used: vec![false; n] }
    }

    /// Claim the lowest free slot, returning its index, or None if exhausted.
    pub fn claim(&mut self) -> Option<u32> {
        for (i, used) in self.used.iter_mut().enumerate() {
            if !*used {
                *used = true;
                return Some(i as u32);
            }
        }
        None
    }

    /// Reserve a specific slot (used when reconciling persisted instances).
    pub fn reserve(&mut self, idx: u32) {
        let i = idx as usize;
        if i < self.used.len() {
            self.used[i] = true;
        }
    }

    pub fn free(&mut self, idx: u32) {
        let i = idx as usize;
        if i < self.used.len() {
            self.used[i] = false;
        }
    }

    pub fn ip_for(&self, idx: u32) -> String {
        fmt_ip(self.cidr.host_at(idx))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_cidr_parse_and_host_addressing() {
        let c = Cidr::parse("10.231.0.0/24").unwrap();
        assert_eq!(c.prefix, 24);
        assert_eq!(fmt_ip(c.gateway()), "10.231.0.1");
        assert_eq!(fmt_ip(c.host_at(0)), "10.231.0.2");
        assert_eq!(fmt_ip(c.host_at(10)), "10.231.0.12");
        assert_eq!(fmt_ip(c.mask()), "255.255.255.0");
        assert_eq!(c.host_count(), 253);
    }

    #[test]
    fn test_slot_ip_math_10_231_0_0_24() {
        let c = Cidr::parse("10.231.0.0/24").unwrap();
        // slot 3 → .5 (net=.0, gw=.1, slot0=.2, slot1=.3, slot2=.4, slot3=.5)
        assert_eq!(fmt_ip(c.host_at(3)), "10.231.0.5");
        // The spec example: slot=3 → ip="10.231.0.3" in the meta fixture — that means
        // the fixture was written with slot=1 giving .3 which is 2+1=3. Let's verify:
        // slot=1 → net_base()+2+1 = 10.231.0.0+2+1 = 10.231.0.3
        assert_eq!(fmt_ip(c.host_at(1)), "10.231.0.3");
    }

    #[test]
    fn test_allocator_sequential_and_reuse() {
        let mut alloc = Allocator::new(Cidr::parse("10.231.0.0/24").unwrap());
        assert_eq!(alloc.claim(), Some(0));
        assert_eq!(alloc.claim(), Some(1));
        assert_eq!(alloc.claim(), Some(2));
        alloc.free(1);
        assert_eq!(alloc.claim(), Some(1)); // reuse lowest free
        assert_eq!(alloc.ip_for(0), "10.231.0.2");
    }

    #[test]
    fn test_reserve_marks_slot_used() {
        let mut alloc = Allocator::new(Cidr::parse("10.231.0.0/29").unwrap());
        // /29 -> 8 addrs, minus net/gw/bcast = 5 host slots.
        assert_eq!(alloc.cidr.host_count(), 5);
        alloc.reserve(0);
        alloc.reserve(2);
        assert_eq!(alloc.claim(), Some(1));
        assert_eq!(alloc.claim(), Some(3));
    }
}
