//! Small JSON helpers: a string-builder for output and typed getters over a
//! parsed std.json.Value tree for input.
const std = @import("std");

pub const Value = std.json.Value;

pub const ParsedObject = struct {
    parsed: std.json.Parsed(std.json.Value),

    pub fn deinit(self: *ParsedObject) void {
        self.parsed.deinit();
    }

    pub fn root(self: *const ParsedObject) std.json.Value {
        return self.parsed.value;
    }
};

/// Parse a JSON document. Caller owns the returned object and must deinit it.
pub fn parse(alloc: std.mem.Allocator, text: []const u8) !ParsedObject {
    const parsed = try std.json.parseFromSlice(std.json.Value, alloc, text, .{});
    return .{ .parsed = parsed };
}

/// Look up a field in an object value, returning null if absent or not an object.
pub fn get(v: std.json.Value, name: []const u8) ?std.json.Value {
    switch (v) {
        .object => |o| return o.get(name),
        else => return null,
    }
}

pub fn getString(v: std.json.Value, name: []const u8) ?[]const u8 {
    const f = get(v, name) orelse return null;
    return switch (f) {
        .string => |s| s,
        else => null,
    };
}

pub fn getInt(v: std.json.Value, name: []const u8) ?i64 {
    const f = get(v, name) orelse return null;
    return switch (f) {
        .integer => |i| i,
        .float => |fl| @intFromFloat(fl),
        .number_string => |ns| std.fmt.parseInt(i64, ns, 10) catch null,
        else => null,
    };
}

/// Append-based JSON builder backed by an arena allocator.
pub const Builder = struct {
    arena: std.mem.Allocator,
    buf: std.ArrayList(u8) = .empty,

    pub fn init(arena: std.mem.Allocator) Builder {
        return .{ .arena = arena };
    }

    pub fn raw(self: *Builder, s: []const u8) !void {
        try self.buf.appendSlice(self.arena, s);
    }

    pub fn byte(self: *Builder, b: u8) !void {
        try self.buf.append(self.arena, b);
    }

    pub fn print(self: *Builder, comptime fmt: []const u8, args: anytype) !void {
        try self.buf.print(self.arena, fmt, args);
    }

    /// Append a JSON string literal (quoted + escaped).
    pub fn str(self: *Builder, s: []const u8) !void {
        try self.byte('"');
        for (s) |c| {
            switch (c) {
                '"' => try self.raw("\\\""),
                '\\' => try self.raw("\\\\"),
                '\n' => try self.raw("\\n"),
                '\r' => try self.raw("\\r"),
                '\t' => try self.raw("\\t"),
                else => {
                    if (c < 0x20) {
                        try self.print("\\u{x:0>4}", .{c});
                    } else {
                        try self.byte(c);
                    }
                },
            }
        }
        try self.byte('"');
    }

    /// Append a key (quoted) followed by a colon.
    pub fn key(self: *Builder, name: []const u8) !void {
        try self.str(name);
        try self.byte(':');
    }

    /// Append a nullable string field value (null literal or quoted string).
    pub fn optStr(self: *Builder, s: ?[]const u8) !void {
        if (s) |v| {
            try self.str(v);
        } else {
            try self.raw("null");
        }
    }

    pub fn items(self: *const Builder) []const u8 {
        return self.buf.items;
    }
};
