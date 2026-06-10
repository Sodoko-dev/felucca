//! A tiny path router. Patterns may contain a single `{id}` segment wildcard and
//! an optional trailing action segment, e.g. "/api/v1/sandboxes/{id}/stop".
const std = @import("std");
const http = @import("http.zig");

pub const Match = struct {
    /// Captured value of the `{id}` segment, if the pattern had one.
    id: ?[]const u8 = null,
};

/// Returns a Match if `path` matches `pattern`, else null. Both are split on '/'.
/// A pattern segment of "{id}" captures any single non-empty segment.
pub fn match(pattern: []const u8, path: []const u8) ?Match {
    var pit = std.mem.tokenizeScalar(u8, pattern, '/');
    var sit = std.mem.tokenizeScalar(u8, path, '/');
    var result: Match = .{};
    while (true) {
        const p = pit.next();
        const s = sit.next();
        if (p == null and s == null) return result;
        if (p == null or s == null) return null;
        const pseg = p.?;
        const sseg = s.?;
        if (std.mem.eql(u8, pseg, "{id}")) {
            result.id = sseg;
        } else if (!std.mem.eql(u8, pseg, sseg)) {
            return null;
        }
    }
}

test "router matches exact paths" {
    const m = match("/api/v1/nodes", "/api/v1/nodes");
    try std.testing.expect(m != null);
    try std.testing.expect(m.?.id == null);
}

test "router rejects different paths" {
    try std.testing.expect(match("/api/v1/nodes", "/api/v1/sandboxes") == null);
    try std.testing.expect(match("/a/b", "/a/b/c") == null);
    try std.testing.expect(match("/a/b/c", "/a/b") == null);
}

test "router captures id" {
    const m = match("/api/v1/sandboxes/{id}", "/api/v1/sandboxes/abc123");
    try std.testing.expect(m != null);
    try std.testing.expectEqualStrings("abc123", m.?.id.?);
}

test "router captures id with trailing action" {
    const m = match("/api/v1/sandboxes/{id}/stop", "/api/v1/sandboxes/xyz/stop");
    try std.testing.expect(m != null);
    try std.testing.expectEqualStrings("xyz", m.?.id.?);
    try std.testing.expect(match("/api/v1/sandboxes/{id}/stop", "/api/v1/sandboxes/xyz/start") == null);
}
