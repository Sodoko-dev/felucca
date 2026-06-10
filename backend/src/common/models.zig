//! Shared data model structs for Hearth and their JSON serialization.
const std = @import("std");
const jsonh = @import("json.zig");

pub const NodeStatus = enum { ready, down };

pub const SandboxState = enum {
    creating,
    running,
    paused,
    stopped,
    sleeping,
    @"error",

    pub fn toString(self: SandboxState) []const u8 {
        return @tagName(self);
    }

    pub fn fromString(s: []const u8) ?SandboxState {
        inline for (std.meta.fields(SandboxState)) |f| {
            if (std.mem.eql(u8, s, f.name)) return @enumFromInt(f.value);
        }
        return null;
    }
};

pub const Node = struct {
    id: []const u8,
    hostname: []const u8,
    addr: []const u8,
    cpus: u32,
    mem_total_mib: u64,
    mem_free_mib: u64,
    vm_count: u32,
    pool_size: u32 = 0, // warm-pool depth reported by the agent (v2)
    last_heartbeat: i64, // unix secs

    /// Compute status based on the supplied "now" timestamp (15s window).
    pub fn status(self: Node, now: i64) NodeStatus {
        if (now - self.last_heartbeat > 15) return .down;
        return .ready;
    }

    pub fn writeJson(self: Node, b: *jsonh.Builder, now: i64) !void {
        try b.byte('{');
        try b.key("id");
        try b.str(self.id);
        try b.raw(",");
        try b.key("hostname");
        try b.str(self.hostname);
        try b.raw(",");
        try b.key("addr");
        try b.str(self.addr);
        try b.raw(",");
        try b.key("cpus");
        try b.print("{d}", .{self.cpus});
        try b.raw(",");
        try b.key("mem_total_mib");
        try b.print("{d}", .{self.mem_total_mib});
        try b.raw(",");
        try b.key("mem_free_mib");
        try b.print("{d}", .{self.mem_free_mib});
        try b.raw(",");
        try b.key("vm_count");
        try b.print("{d}", .{self.vm_count});
        try b.raw(",");
        try b.key("pool_size");
        try b.print("{d}", .{self.pool_size});
        try b.raw(",");
        try b.key("status");
        try b.str(@tagName(self.status(now)));
        try b.raw(",");
        try b.key("last_heartbeat");
        try b.print("{d}", .{self.last_heartbeat});
        try b.byte('}');
    }
};

pub const Sandbox = struct {
    id: []const u8,
    name: []const u8,
    namespace: []const u8,
    node_id: ?[]const u8,
    state: SandboxState,
    vcpus: u32,
    mem_mib: u64,
    ip: ?[]const u8,
    created_at: i64,
    parent_id: ?[]const u8, // null in v1

    pub fn writeJson(self: Sandbox, b: *jsonh.Builder) !void {
        try b.byte('{');
        try b.key("id");
        try b.str(self.id);
        try b.raw(",");
        try b.key("name");
        try b.str(self.name);
        try b.raw(",");
        try b.key("namespace");
        try b.str(self.namespace);
        try b.raw(",");
        try b.key("node_id");
        try b.optStr(self.node_id);
        try b.raw(",");
        try b.key("state");
        try b.str(self.state.toString());
        try b.raw(",");
        try b.key("vcpus");
        try b.print("{d}", .{self.vcpus});
        try b.raw(",");
        try b.key("mem_mib");
        try b.print("{d}", .{self.mem_mib});
        try b.raw(",");
        try b.key("ip");
        try b.optStr(self.ip);
        try b.raw(",");
        try b.key("created_at");
        try b.print("{d}", .{self.created_at});
        try b.raw(",");
        try b.key("parent_id");
        try b.optStr(self.parent_id);
        try b.byte('}');
    }
};

test "sandbox json round trip via state enum" {
    try std.testing.expectEqualStrings("running", SandboxState.running.toString());
    try std.testing.expect(SandboxState.fromString("paused").? == .paused);
    try std.testing.expect(SandboxState.fromString("nope") == null);
}
