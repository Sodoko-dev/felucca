//! Source-IP auto-detection for the agent's `advertise_addr`.
//!
//! When `advertise_addr` is unset we determine which local address the kernel
//! would use to reach the control plane: open a UDP socket, `connect()` it
//! toward the control-plane host (no packet is actually sent for a datagram
//! socket — connect just sets the default route/peer and binds a source
//! address), then read the bound local address with `getsockname`. This is the
//! standard, route-aware way to discover "my address as seen from there".
//!
//! Implemented with raw Linux syscalls so it works on the fully-static musl
//! builds (no libc dependency).
const std = @import("std");
const linux = std.os.linux;

/// Detect the local IPv4 address used to reach `host:port`. `host` must be a
/// dotted-quad IPv4 literal (the control-plane addresses always are in this
/// deployment; hostnames would need resolution which is out of scope here).
/// Returns the address as a dotted-quad string in `out`, or null on any failure.
pub fn sourceAddrFor(host: []const u8, port: u16, out: []u8) ?[]const u8 {
    const dst = parseIp4(host) orelse return null;

    const fd_usize = linux.socket(linux.AF.INET, linux.SOCK.DGRAM, 0);
    if (linux.errno(fd_usize) != .SUCCESS) return null;
    const fd: i32 = @intCast(fd_usize);
    defer _ = linux.close(fd);

    var peer = linux.sockaddr.in{
        .port = std.mem.nativeToBig(u16, port),
        .addr = dst,
    };
    const crc = linux.connect(fd, @ptrCast(&peer), @sizeOf(linux.sockaddr.in));
    if (linux.errno(crc) != .SUCCESS) return null;

    var local: linux.sockaddr.in = undefined;
    var slen: linux.socklen_t = @sizeOf(linux.sockaddr.in);
    const grc = linux.getsockname(fd, @ptrCast(&local), &slen);
    if (linux.errno(grc) != .SUCCESS) return null;

    const b: [4]u8 = @bitCast(local.addr);
    return std.fmt.bufPrint(out, "{d}.{d}.{d}.{d}", .{ b[0], b[1], b[2], b[3] }) catch null;
}

/// Parse "a.b.c.d" into a network-byte-order u32 suitable for sockaddr.in.addr.
fn parseIp4(s: []const u8) ?u32 {
    var bytes: [4]u8 = undefined;
    var it = std.mem.splitScalar(u8, s, '.');
    var i: usize = 0;
    while (it.next()) |part| : (i += 1) {
        if (i >= 4) return null;
        bytes[i] = std.fmt.parseInt(u8, part, 10) catch return null;
    }
    if (i != 4) return null;
    // sockaddr.in.addr is stored big-endian (network order); the byte array
    // [a,b,c,d] reinterpreted as a u32 already gives that on any host.
    return @bitCast(bytes);
}

test "parseIp4 round trip" {
    const v = parseIp4("192.168.104.3").?;
    const b: [4]u8 = @bitCast(v);
    try std.testing.expectEqual([4]u8{ 192, 168, 104, 3 }, b);
    try std.testing.expect(parseIp4("not.an.ip.addr") == null);
    try std.testing.expect(parseIp4("1.2.3") == null);
    try std.testing.expect(parseIp4("1.2.3.4.5") == null);
}
