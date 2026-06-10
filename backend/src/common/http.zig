//! Minimal hand-rolled HTTP/1.1 helpers built on the Zig 0.16 std.Io
//! Reader/Writer interfaces. Both the servers and the outbound client use
//! these so the wire handling lives in exactly one place.
const std = @import("std");
const Io = std.Io;

pub const max_header_bytes = 64 * 1024;
pub const max_body_bytes = 8 * 1024 * 1024;

pub const Method = enum {
    GET,
    POST,
    PUT,
    PATCH,
    DELETE,
    OPTIONS,
    HEAD,
    other,

    pub fn parse(s: []const u8) Method {
        if (std.mem.eql(u8, s, "GET")) return .GET;
        if (std.mem.eql(u8, s, "POST")) return .POST;
        if (std.mem.eql(u8, s, "PUT")) return .PUT;
        if (std.mem.eql(u8, s, "PATCH")) return .PATCH;
        if (std.mem.eql(u8, s, "DELETE")) return .DELETE;
        if (std.mem.eql(u8, s, "OPTIONS")) return .OPTIONS;
        if (std.mem.eql(u8, s, "HEAD")) return .HEAD;
        return .other;
    }
};

/// A parsed inbound request. All slices are owned by the provided arena.
pub const Request = struct {
    method: Method,
    method_raw: []const u8,
    /// Full target including any query string, e.g. "/api/v1/foo?x=1".
    target: []const u8,
    /// Path portion only (query stripped).
    path: []const u8,
    body: []const u8,
    content_length: usize,
    /// Value of the `Authorization` header, if present (e.g. "Bearer abc").
    authorization: ?[]const u8,
};

pub const ParseError = error{
    BadRequest,
    HeadersTooLarge,
    BodyTooLarge,
    OutOfMemory,
} || Io.Reader.Error || error{ EndOfStream, StreamTooLong };

/// Read and parse a single HTTP/1.1 request from `r`. Allocations use `arena`.
pub fn readRequest(arena: std.mem.Allocator, r: *Io.Reader) ParseError!Request {
    // Request line.
    const line = try takeLine(r);
    if (line.len == 0) return error.BadRequest;
    var it = std.mem.tokenizeScalar(u8, line, ' ');
    const method_raw = it.next() orelse return error.BadRequest;
    const target = it.next() orelse return error.BadRequest;

    const method_copy = try arena.dupe(u8, method_raw);
    const target_copy = try arena.dupe(u8, target);

    var content_length: usize = 0;
    var authorization: ?[]const u8 = null;
    // Headers until blank line.
    while (true) {
        const hline = try takeLine(r);
        if (hline.len == 0) break;
        const colon = std.mem.indexOfScalar(u8, hline, ':') orelse continue;
        const name = std.mem.trim(u8, hline[0..colon], " \t");
        const value = std.mem.trim(u8, hline[colon + 1 ..], " \t");
        if (std.ascii.eqlIgnoreCase(name, "content-length")) {
            content_length = std.fmt.parseInt(usize, value, 10) catch 0;
        } else if (std.ascii.eqlIgnoreCase(name, "authorization")) {
            authorization = try arena.dupe(u8, value);
        }
    }
    if (content_length > max_body_bytes) return error.BodyTooLarge;

    var body: []const u8 = &.{};
    if (content_length > 0) {
        const buf = try arena.alloc(u8, content_length);
        try r.readSliceAll(buf);
        body = buf;
    }

    const path = blk: {
        if (std.mem.indexOfScalar(u8, target_copy, '?')) |q| break :blk target_copy[0..q];
        break :blk target_copy;
    };

    return .{
        .method = Method.parse(method_copy),
        .method_raw = method_copy,
        .target = target_copy,
        .path = path,
        .body = body,
        .content_length = content_length,
        .authorization = authorization,
    };
}

/// Takes one CRLF/LF delimited line (consuming the terminator, returning the
/// content without the trailing CR/LF).
fn takeLine(r: *Io.Reader) ParseError![]const u8 {
    const raw = r.takeDelimiterInclusive('\n') catch |err| switch (err) {
        error.StreamTooLong => return error.HeadersTooLarge,
        error.EndOfStream => return error.EndOfStream,
        error.ReadFailed => return error.ReadFailed,
    };
    var line = raw;
    if (line.len > 0 and line[line.len - 1] == '\n') line = line[0 .. line.len - 1];
    if (line.len > 0 and line[line.len - 1] == '\r') line = line[0 .. line.len - 1];
    return line;
}

/// Write a complete response with the given status, content type and body.
pub fn writeResponse(
    w: *Io.Writer,
    status: u16,
    reason: []const u8,
    content_type: []const u8,
    body: []const u8,
) Io.Writer.Error!void {
    try w.print("HTTP/1.1 {d} {s}\r\n", .{ status, reason });
    try w.print("Content-Type: {s}\r\n", .{content_type});
    try w.print("Content-Length: {d}\r\n", .{body.len});
    try w.writeAll("Connection: close\r\n\r\n");
    try w.writeAll(body);
    try w.flush();
}

pub fn writeJson(w: *Io.Writer, status: u16, body: []const u8) Io.Writer.Error!void {
    const reason = reasonPhrase(status);
    try writeResponse(w, status, reason, "application/json", body);
}

pub fn writeEmpty(w: *Io.Writer, status: u16) Io.Writer.Error!void {
    const reason = reasonPhrase(status);
    try w.print("HTTP/1.1 {d} {s}\r\n", .{ status, reason });
    try w.writeAll("Content-Length: 0\r\n");
    try w.writeAll("Connection: close\r\n\r\n");
    try w.flush();
}

pub fn reasonPhrase(status: u16) []const u8 {
    return switch (status) {
        200 => "OK",
        201 => "Created",
        204 => "No Content",
        400 => "Bad Request",
        404 => "Not Found",
        405 => "Method Not Allowed",
        500 => "Internal Server Error",
        501 => "Not Implemented",
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        else => "OK",
    };
}
