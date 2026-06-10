//! Shared configuration loading for both binaries.
//!
//! Precedence (highest first): command-line flags > environment variables >
//! JSON config file > built-in defaults. Nothing hardcodes localhost or Lima
//! paths in production code paths — every value is overridable by config so the
//! local lab and remote servers differ only by configuration.
//!
//! A small typed `Spec` describes each key (its JSON name, env var, and flag).
//! Callers build a list of specs, then call `load`, which walks the layers in
//! precedence order. String values are duped into the supplied allocator.
const std = @import("std");
const Io = std.Io;
const jsonh = @import("json.zig");

pub const EnvMap = std.process.Environ.Map;

/// One configurable key. `dest` points at the field to populate.
pub const Kind = enum { string, u16, u32, u64, bool };

pub const Spec = struct {
    /// JSON object key in the config file.
    json: []const u8,
    /// Environment variable name.
    env: []const u8,
    /// Command-line flag, e.g. "--port". Empty means no flag for this key.
    flag: []const u8,
    kind: Kind,
    /// Type-erased pointer to the destination field.
    dest: *anyopaque,
};

pub fn strSpec(json: []const u8, env: []const u8, flag: []const u8, dest: *[]const u8) Spec {
    return .{ .json = json, .env = env, .flag = flag, .kind = .string, .dest = @ptrCast(dest) };
}
pub fn u16Spec(json: []const u8, env: []const u8, flag: []const u8, dest: *u16) Spec {
    return .{ .json = json, .env = env, .flag = flag, .kind = .u16, .dest = @ptrCast(dest) };
}
pub fn u32Spec(json: []const u8, env: []const u8, flag: []const u8, dest: *u32) Spec {
    return .{ .json = json, .env = env, .flag = flag, .kind = .u32, .dest = @ptrCast(dest) };
}
pub fn u64Spec(json: []const u8, env: []const u8, flag: []const u8, dest: *u64) Spec {
    return .{ .json = json, .env = env, .flag = flag, .kind = .u64, .dest = @ptrCast(dest) };
}
pub fn boolSpec(json: []const u8, env: []const u8, flag: []const u8, dest: *bool) Spec {
    return .{ .json = json, .env = env, .flag = flag, .kind = .bool, .dest = @ptrCast(dest) };
}

fn setString(dest: *anyopaque, alloc: std.mem.Allocator, v: []const u8) !void {
    const p: *[]const u8 = @ptrCast(@alignCast(dest));
    p.* = try alloc.dupe(u8, v);
}
fn setInt(comptime T: type, dest: *anyopaque, v: []const u8) void {
    const p: *T = @ptrCast(@alignCast(dest));
    p.* = std.fmt.parseInt(T, v, 10) catch p.*;
}
/// Accept on/off (per contract `net = on|off`) plus true/false/1/0.
pub fn parseBool(v: []const u8) ?bool {
    if (std.ascii.eqlIgnoreCase(v, "on") or std.ascii.eqlIgnoreCase(v, "true") or std.mem.eql(u8, v, "1")) return true;
    if (std.ascii.eqlIgnoreCase(v, "off") or std.ascii.eqlIgnoreCase(v, "false") or std.mem.eql(u8, v, "0")) return false;
    return null;
}
fn setBool(dest: *anyopaque, v: []const u8) void {
    if (parseBool(v)) |b| {
        const p: *bool = @ptrCast(@alignCast(dest));
        p.* = b;
    }
}

fn applyString(spec: Spec, alloc: std.mem.Allocator, v: []const u8) !void {
    switch (spec.kind) {
        .string => try setString(spec.dest, alloc, v),
        .u16 => setInt(u16, spec.dest, v),
        .u32 => setInt(u32, spec.dest, v),
        .u64 => setInt(u64, spec.dest, v),
        .bool => setBool(spec.dest, v),
    }
}

/// Resolve the config file path: `--config <path>` flag beats `HEARTH_CONFIG`.
fn resolveConfigPath(alloc: std.mem.Allocator, env: *const EnvMap, args: std.process.Args) !?[]const u8 {
    var path: ?[]const u8 = if (env.get("HEARTH_CONFIG")) |v| try alloc.dupe(u8, v) else null;
    var it = std.process.Args.Iterator.init(args);
    _ = it.skip();
    while (it.next()) |a| {
        if (std.mem.eql(u8, a, "--config")) {
            if (it.next()) |v| path = try alloc.dupe(u8, v);
        }
    }
    return path;
}

/// Load configuration into the fields referenced by `specs`, applying the
/// layers in precedence order (file < env < flags). Defaults are whatever the
/// destination fields already hold on entry.
pub fn load(
    alloc: std.mem.Allocator,
    io: Io,
    env: *const EnvMap,
    args: std.process.Args,
    specs: []const Spec,
) !void {
    // 1) File layer (lowest precedence above defaults).
    if (try resolveConfigPath(alloc, env, args)) |path| {
        if (Io.Dir.cwd().readFileAlloc(io, path, alloc, .limited(1 << 20))) |data| {
            applyFile(alloc, data, specs) catch |err| {
                std.log.warn("config: bad json in {s}: {s}", .{ path, @errorName(err) });
            };
        } else |err| {
            std.log.warn("config: could not read {s}: {s}", .{ path, @errorName(err) });
        }
    }

    // 2) Env layer.
    for (specs) |spec| {
        if (env.get(spec.env)) |v| try applyString(spec, alloc, v);
    }

    // 3) Flag layer (highest precedence). Bool flags take an explicit value
    //    (`--net on` / `--net off`) so parsing is unambiguous.
    var it = std.process.Args.Iterator.init(args);
    _ = it.skip();
    while (it.next()) |a| {
        for (specs) |spec| {
            if (spec.flag.len == 0) continue;
            if (std.mem.eql(u8, a, spec.flag)) {
                if (it.next()) |v| try applyString(spec, alloc, v);
            }
        }
    }
}

fn applyFile(alloc: std.mem.Allocator, data: []const u8, specs: []const Spec) !void {
    var parsed = try jsonh.parse(alloc, data);
    defer parsed.deinit();
    const root = parsed.root();
    for (specs) |spec| {
        const fv = jsonh.get(root, spec.json) orelse continue;
        switch (spec.kind) {
            .string => if (jsonh.getString(root, spec.json)) |s| try setString(spec.dest, alloc, s),
            .bool => switch (fv) {
                .bool => |bv| {
                    const p: *bool = @ptrCast(@alignCast(spec.dest));
                    p.* = bv;
                },
                .string => |s| setBool(spec.dest, s),
                else => {},
            },
            else => if (jsonh.getInt(root, spec.json)) |n| {
                var buf: [24]u8 = undefined;
                const s = std.fmt.bufPrint(&buf, "{d}", .{n}) catch continue;
                try applyString(spec, alloc, s);
            },
        }
    }
}

/// Constant-time byte-slice equality. Returns true iff both slices have the
/// same length and contents. The comparison does not short-circuit on the
/// first differing byte, so its timing does not leak where a mismatch occurs.
pub fn constantTimeEql(a: []const u8, b: []const u8) bool {
    if (a.len != b.len) return false;
    var diff: u8 = 0;
    for (a, b) |x, y| diff |= x ^ y;
    return diff == 0;
}

test "constant time eql" {
    try std.testing.expect(constantTimeEql("secret", "secret"));
    try std.testing.expect(!constantTimeEql("secret", "secreT"));
    try std.testing.expect(!constantTimeEql("secret", "secret2"));
    try std.testing.expect(!constantTimeEql("", "x"));
    try std.testing.expect(constantTimeEql("", ""));
}

test "parseBool on/off true/false" {
    try std.testing.expect(parseBool("on").? == true);
    try std.testing.expect(parseBool("off").? == false);
    try std.testing.expect(parseBool("TRUE").? == true);
    try std.testing.expect(parseBool("0").? == false);
    try std.testing.expect(parseBool("garbage") == null);
}
