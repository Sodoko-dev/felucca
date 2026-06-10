//! Aggregates unit tests for the common module (router + json round trip).
const std = @import("std");
const common = @import("common");

test {
    std.testing.refAllDecls(common.router);
    std.testing.refAllDecls(common.models);
    std.testing.refAllDecls(common.config);
    std.testing.refAllDecls(common.netdetect);
}

test "authorized: disabled when no token" {
    try std.testing.expect(common.authorized("", null));
    try std.testing.expect(common.authorized("", "Bearer whatever"));
}

test "authorized: requires matching bearer" {
    try std.testing.expect(common.authorized("sekret", "Bearer sekret"));
    try std.testing.expect(!common.authorized("sekret", "Bearer nope"));
    try std.testing.expect(!common.authorized("sekret", "sekret")); // missing prefix
    try std.testing.expect(!common.authorized("sekret", null)); // no header
}

test "json builder produces escaped output" {
    var arena_state = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    var b = common.jsonh.Builder.init(arena);
    try b.byte('{');
    try b.key("msg");
    try b.str("a\"b\nc");
    try b.byte('}');
    try std.testing.expectEqualStrings("{\"msg\":\"a\\\"b\\nc\"}", b.items());
}

test "json parse round trip" {
    var arena_state = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    var p = try common.jsonh.parse(arena, "{\"name\":\"vm1\",\"vcpus\":2,\"mem_mib\":512}");
    defer p.deinit();
    const root = p.root();
    try std.testing.expectEqualStrings("vm1", common.jsonh.getString(root, "name").?);
    try std.testing.expectEqual(@as(i64, 2), common.jsonh.getInt(root, "vcpus").?);
    try std.testing.expectEqual(@as(i64, 512), common.jsonh.getInt(root, "mem_mib").?);
    try std.testing.expect(common.jsonh.getString(root, "missing") == null);
}

test "sandbox serializes to json" {
    var arena_state = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const sb = common.models.Sandbox{
        .id = "s1",
        .name = "n",
        .namespace = "default",
        .node_id = "node1",
        .state = .running,
        .vcpus = 1,
        .mem_mib = 256,
        .ip = null,
        .created_at = 1000,
        .parent_id = null,
    };
    var b = common.jsonh.Builder.init(arena);
    try sb.writeJson(&b);

    // Re-parse to verify it is valid JSON with the right fields.
    var p = try common.jsonh.parse(arena, b.items());
    defer p.deinit();
    const root = p.root();
    try std.testing.expectEqualStrings("s1", common.jsonh.getString(root, "id").?);
    try std.testing.expectEqualStrings("running", common.jsonh.getString(root, "state").?);
    try std.testing.expect(common.jsonh.get(root, "ip").? == .null);
    try std.testing.expect(common.jsonh.get(root, "parent_id").? == .null);
}
