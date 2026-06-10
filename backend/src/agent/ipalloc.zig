//! Sequential guest-IP allocation from a CIDR.
//!
//! The CIDR's `.1` host is the bridge gateway; guests are handed out
//! sequentially starting at `.2`. Allocation is index-based and stable: a guest
//! assigned slot N always gets the same IP, so a restored/forked VM that records
//! its slot keeps its address. Freed slots are reused lowest-first.
const std = @import("std");

pub const Cidr = struct {
    /// Network base address in host byte order (e.g. 10.231.0.0 -> 0x0AE70000).
    base: u32,
    prefix: u6,

    /// Parse "a.b.c.d/n".
    pub fn parse(s: []const u8) ?Cidr {
        const slash = std.mem.indexOfScalar(u8, s, '/') orelse return null;
        const ip = parseIp4(s[0..slash]) orelse return null;
        const prefix = std.fmt.parseInt(u6, s[slash + 1 ..], 10) catch return null;
        if (prefix > 32) return null;
        return .{ .base = ip, .prefix = prefix };
    }

    /// Host count usable for guests = 2^(32-prefix) minus network/gw/broadcast.
    pub fn hostCount(self: Cidr) u32 {
        const host_bits: u5 = @intCast(32 - @as(u32, self.prefix));
        const total = @as(u64, 1) << host_bits;
        if (total <= 3) return 0; // network + gw(.1) + broadcast
        return @intCast(total - 3);
    }

    /// The gateway address (network .1) in host byte order.
    pub fn gateway(self: Cidr) u32 {
        return netBase(self) + 1;
    }

    /// Netmask in host byte order.
    pub fn mask(self: Cidr) u32 {
        if (self.prefix == 0) return 0;
        return ~@as(u32, 0) << @intCast(32 - @as(u32, self.prefix));
    }

    fn netBase(self: Cidr) u32 {
        return self.base & self.mask();
    }

    /// IP (host byte order) for guest slot `idx` (0-based), starting at .2.
    pub fn hostAt(self: Cidr, idx: u32) u32 {
        return netBase(self) + 2 + idx;
    }
};

/// Format a host-byte-order u32 as a dotted quad into `out`.
pub fn fmtIp(v: u32, out: []u8) []const u8 {
    return std.fmt.bufPrint(out, "{d}.{d}.{d}.{d}", .{
        (v >> 24) & 0xff, (v >> 16) & 0xff, (v >> 8) & 0xff, v & 0xff,
    }) catch out[0..0];
}

fn parseIp4(s: []const u8) ?u32 {
    var out: u32 = 0;
    var it = std.mem.splitScalar(u8, s, '.');
    var i: usize = 0;
    while (it.next()) |part| : (i += 1) {
        if (i >= 4) return null;
        const b = std.fmt.parseInt(u8, part, 10) catch return null;
        out = (out << 8) | b;
    }
    if (i != 4) return null;
    return out;
}

/// Tracks which guest slots are in use within a CIDR. `tap_<idx>` and the IP
/// for a VM both derive from its slot index. Caller serializes access.
pub const Allocator = struct {
    cidr: Cidr,
    used: std.DynamicBitSetUnmanaged,
    gpa: std.mem.Allocator,

    pub fn init(gpa: std.mem.Allocator, cidr: Cidr) !Allocator {
        const n = cidr.hostCount();
        var bits = try std.DynamicBitSetUnmanaged.initEmpty(gpa, n);
        errdefer bits.deinit(gpa);
        return .{ .cidr = cidr, .used = bits, .gpa = gpa };
    }

    pub fn deinit(self: *Allocator) void {
        self.used.deinit(self.gpa);
    }

    /// Claim the lowest free slot, returning its index, or null if exhausted.
    pub fn claim(self: *Allocator) ?u32 {
        var i: usize = 0;
        while (i < self.used.bit_length) : (i += 1) {
            if (!self.used.isSet(i)) {
                self.used.set(i);
                return @intCast(i);
            }
        }
        return null;
    }

    /// Reserve a specific slot (used when reconciling persisted instances).
    pub fn reserve(self: *Allocator, idx: u32) void {
        if (idx < self.used.bit_length) self.used.set(idx);
    }

    pub fn free(self: *Allocator, idx: u32) void {
        if (idx < self.used.bit_length) self.used.unset(idx);
    }

    pub fn ipFor(self: *const Allocator, idx: u32, out: []u8) []const u8 {
        return fmtIp(self.cidr.hostAt(idx), out);
    }
};

test "cidr parse and host addressing" {
    const c = Cidr.parse("10.231.0.0/24").?;
    try std.testing.expectEqual(@as(u6, 24), c.prefix);
    var buf: [16]u8 = undefined;
    try std.testing.expectEqualStrings("10.231.0.1", fmtIp(c.gateway(), &buf));
    try std.testing.expectEqualStrings("10.231.0.2", fmtIp(c.hostAt(0), &buf));
    try std.testing.expectEqualStrings("10.231.0.12", fmtIp(c.hostAt(10), &buf));
    try std.testing.expectEqualStrings("255.255.255.0", fmtIp(c.mask(), &buf));
    try std.testing.expectEqual(@as(u32, 253), c.hostCount());
}

test "allocator hands out sequential slots and reuses freed" {
    var alloc = try Allocator.init(std.testing.allocator, Cidr.parse("10.231.0.0/24").?);
    defer alloc.deinit();
    try std.testing.expectEqual(@as(u32, 0), alloc.claim().?);
    try std.testing.expectEqual(@as(u32, 1), alloc.claim().?);
    try std.testing.expectEqual(@as(u32, 2), alloc.claim().?);
    alloc.free(1);
    try std.testing.expectEqual(@as(u32, 1), alloc.claim().?); // reuse lowest free

    var buf: [16]u8 = undefined;
    try std.testing.expectEqualStrings("10.231.0.2", alloc.ipFor(0, &buf));
}

test "reserve marks slot used" {
    var alloc = try Allocator.init(std.testing.allocator, Cidr.parse("10.231.0.0/29").?);
    defer alloc.deinit();
    // /29 -> 8 addrs, minus net/gw/bcast = 5 host slots.
    try std.testing.expectEqual(@as(u32, 5), alloc.cidr.hostCount());
    alloc.reserve(0);
    alloc.reserve(2);
    try std.testing.expectEqual(@as(u32, 1), alloc.claim().?);
    try std.testing.expectEqual(@as(u32, 3), alloc.claim().?);
}
