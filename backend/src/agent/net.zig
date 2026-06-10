//! Guest networking for hearth-agent: a Linux bridge (`hearth0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! All setup is idempotent (re-running is a no-op) and runs the `ip`/`nft`
//! commands directly first, falling back to `sudo` if the unprivileged call
//! fails (in production the agent runs as root via systemd; in the lab it runs
//! as an unprivileged user with passwordless sudo).
const std = @import("std");
const Io = std.Io;
const ipalloc = @import("ipalloc.zig");

pub const bridge_name = "hearth0";

/// Run `argv`, returning true on exit code 0. Tries unprivileged first, then
/// retries with `sudo` prepended. Output is captured and discarded (logged on
/// unexpected failure by the caller via the boolean result).
fn run(gpa: std.mem.Allocator, io: Io, argv: []const []const u8) bool {
    if (runOnce(gpa, io, argv, false)) return true;
    return runOnce(gpa, io, argv, true);
}

fn runOnce(gpa: std.mem.Allocator, io: Io, argv: []const []const u8, use_sudo: bool) bool {
    var list = std.ArrayList([]const u8).empty;
    defer list.deinit(gpa);
    if (use_sudo) {
        list.append(gpa, "sudo") catch return false;
        list.append(gpa, "-n") catch return false;
    }
    list.appendSlice(gpa, argv) catch return false;

    const res = std.process.run(gpa, io, .{ .argv = list.items }) catch return false;
    defer gpa.free(res.stdout);
    defer gpa.free(res.stderr);
    return switch (res.term) {
        .exited => |code| code == 0,
        else => false,
    };
}

/// Idempotently bring up the bridge with the CIDR's gateway address, enable
/// IPv4 forwarding, and install an nftables masquerade rule for egress.
/// Returns false if the bridge could not be configured (networking unusable).
pub fn ensureBridge(gpa: std.mem.Allocator, io: Io, cidr: ipalloc.Cidr) bool {
    var gw_buf: [16]u8 = undefined;
    const gw = ipalloc.fmtIp(cidr.gateway(), &gw_buf);
    var cidr_buf: [20]u8 = undefined;
    const gw_cidr = std.fmt.bufPrint(&cidr_buf, "{s}/{d}", .{ gw, cidr.prefix }) catch return false;

    // Create the bridge (ignore "exists"); then bring it up + assign the gw IP.
    _ = run(gpa, io, &.{ "ip", "link", "add", bridge_name, "type", "bridge" });
    if (!run(gpa, io, &.{ "ip", "link", "set", bridge_name, "up" })) return false;
    // addr add is idempotent-ish: it errors if already present, which is fine.
    _ = run(gpa, io, &.{ "ip", "addr", "add", gw_cidr, "dev", bridge_name });

    // Enable forwarding (best effort; works as root, sudo-tee in the lab).
    enableForwarding(gpa, io);

    // nftables masquerade for the CIDR. Recreate the table idempotently.
    ensureNat(gpa, io, cidr);
    return true;
}

fn enableForwarding(gpa: std.mem.Allocator, io: Io) void {
    if (run(gpa, io, &.{ "sysctl", "-w", "net.ipv4.ip_forward=1" })) return;
    // Fallback: write the proc file via sh -c so a single sudo covers the redirect.
    _ = run(gpa, io, &.{ "sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward" });
}

fn ensureNat(gpa: std.mem.Allocator, io: Io, cidr: ipalloc.Cidr) void {
    var base_buf: [16]u8 = undefined;
    const base = ipalloc.fmtIp(cidr.base & cidr.mask(), &base_buf);
    var src_buf: [20]u8 = undefined;
    const src = std.fmt.bufPrint(&src_buf, "{s}/{d}", .{ base, cidr.prefix }) catch return;

    // Idempotent: ensure the table+chain exist, then add the masquerade rule
    // only if an identical one is not already present.
    _ = run(gpa, io, &.{ "nft", "add", "table", "ip", "hearth" });
    _ = run(gpa, io, &.{ "nft", "add", "chain", "ip", "hearth", "postrouting", "{ type nat hook postrouting priority 100 ; }" });
    // `nft` has no built-in "add if absent"; flush our chain then re-add so the
    // rule set stays at exactly one masquerade rule for our source range.
    _ = run(gpa, io, &.{ "nft", "flush", "chain", "ip", "hearth", "postrouting" });
    _ = run(gpa, io, &.{ "nft", "add", "rule", "ip", "hearth", "postrouting", "ip", "saddr", src, "masquerade" });
}

/// Idempotently create a tap device named `tap`, enslave it to the bridge, and
/// bring it up. Returns false on failure.
pub fn ensureTap(gpa: std.mem.Allocator, io: Io, tap: []const u8) bool {
    // tuntap add is not idempotent (errors if exists); ignore that error.
    _ = run(gpa, io, &.{ "ip", "tuntap", "add", "dev", tap, "mode", "tap" });
    _ = run(gpa, io, &.{ "ip", "link", "set", tap, "master", bridge_name });
    return run(gpa, io, &.{ "ip", "link", "set", tap, "up" });
}

/// Tear down a tap device (best effort).
pub fn deleteTap(gpa: std.mem.Allocator, io: Io, tap: []const u8) void {
    _ = run(gpa, io, &.{ "ip", "link", "del", tap });
}

/// Tap device name for a guest slot index, e.g. slot 3 -> "hth-3".
pub fn tapName(idx: u32, out: []u8) []const u8 {
    return std.fmt.bufPrint(out, "hth-{d}", .{idx}) catch out[0..0];
}
