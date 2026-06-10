//! Hand-rolled outbound HTTP/1.1 client over std.Io streams. Used by hearthd to
//! talk to agents (TCP) and by the agent to talk to firecracker (unix socket).
const std = @import("std");
const Io = std.Io;
const net = std.Io.net;

pub const Response = struct {
    status: u16,
    body: []const u8, // owned by caller-provided arena
};

pub const Error = error{
    ConnectFailed,
    RequestFailed,
    BadResponse,
    OutOfMemory,
};

/// Connect to host:port over TCP and perform an HTTP request.
pub fn requestTcp(
    arena: std.mem.Allocator,
    io: Io,
    host: []const u8,
    port: u16,
    method: []const u8,
    path: []const u8,
    body: ?[]const u8,
) Error!Response {
    return requestTcpAuth(arena, io, host, port, method, path, body, null);
}

/// Like `requestTcp` but attaches `Authorization: Bearer <token>` when `token`
/// is non-null and non-empty.
pub fn requestTcpAuth(
    arena: std.mem.Allocator,
    io: Io,
    host: []const u8,
    port: u16,
    method: []const u8,
    path: []const u8,
    body: ?[]const u8,
    token: ?[]const u8,
) Error!Response {
    const addr = net.IpAddress.parse(host, port) catch return error.ConnectFailed;
    const stream = addr.connect(io, .{ .mode = .stream, .protocol = .tcp }) catch return error.ConnectFailed;
    defer stream.close(io);
    const host_hdr = std.fmt.allocPrint(arena, "{s}:{d}", .{ host, port }) catch return error.OutOfMemory;
    return doRequest(arena, io, stream, method, host_hdr, path, body, token);
}

/// Connect to a unix domain socket and perform an HTTP request (firecracker API).
pub fn requestUnix(
    arena: std.mem.Allocator,
    io: Io,
    sock_path: []const u8,
    method: []const u8,
    path: []const u8,
    body: ?[]const u8,
) Error!Response {
    const ua = net.UnixAddress.init(sock_path) catch return error.ConnectFailed;
    const stream = ua.connect(io) catch return error.ConnectFailed;
    defer stream.close(io);
    return doRequest(arena, io, stream, method, "localhost", path, body, null);
}

fn doRequest(
    arena: std.mem.Allocator,
    io: Io,
    stream: net.Stream,
    method: []const u8,
    host_hdr: []const u8,
    path: []const u8,
    body: ?[]const u8,
    token: ?[]const u8,
) Error!Response {
    var wbuf: [16 * 1024]u8 = undefined;
    var sw = stream.writer(io, &wbuf);
    const w = &sw.interface;

    w.print("{s} {s} HTTP/1.1\r\n", .{ method, path }) catch return error.RequestFailed;
    w.print("Host: {s}\r\n", .{host_hdr}) catch return error.RequestFailed;
    if (token) |t| {
        if (t.len > 0) w.print("Authorization: Bearer {s}\r\n", .{t}) catch return error.RequestFailed;
    }
    w.writeAll("Connection: close\r\n") catch return error.RequestFailed;
    if (body) |b| {
        w.print("Content-Type: application/json\r\nContent-Length: {d}\r\n\r\n", .{b.len}) catch return error.RequestFailed;
        w.writeAll(b) catch return error.RequestFailed;
    } else {
        w.writeAll("Content-Length: 0\r\n\r\n") catch return error.RequestFailed;
    }
    w.flush() catch return error.RequestFailed;

    var rbuf: [16 * 1024]u8 = undefined;
    var sr = stream.reader(io, &rbuf);
    const r = &sr.interface;

    // Status line.
    const status_line = takeLine(r) catch return error.BadResponse;
    var sit = std.mem.tokenizeScalar(u8, status_line, ' ');
    _ = sit.next() orelse return error.BadResponse; // HTTP/1.1
    const code_str = sit.next() orelse return error.BadResponse;
    const status = std.fmt.parseInt(u16, code_str, 10) catch return error.BadResponse;

    var content_length: ?usize = null;
    while (true) {
        const hline = takeLine(r) catch return error.BadResponse;
        if (hline.len == 0) break;
        const colon = std.mem.indexOfScalar(u8, hline, ':') orelse continue;
        const name = std.mem.trim(u8, hline[0..colon], " \t");
        const value = std.mem.trim(u8, hline[colon + 1 ..], " \t");
        if (std.ascii.eqlIgnoreCase(name, "content-length")) {
            content_length = std.fmt.parseInt(usize, value, 10) catch null;
        }
    }

    // Responses that by definition carry no body (1xx, 204, 304). Firecracker
    // replies 204 to successful PUTs and keeps the connection open without a
    // Content-Length, so we must not block waiting for EOF here.
    const no_body = status == 204 or status == 304 or (status >= 100 and status < 200);

    var body_out: []const u8 = &.{};
    if (no_body) {
        // nothing to read
    } else if (content_length) |len| {
        if (len > 0) {
            const buf = arena.alloc(u8, len) catch return error.OutOfMemory;
            r.readSliceAll(buf) catch return error.BadResponse;
            body_out = buf;
        }
    } else {
        // Read until EOF (connection-close framing).
        var list: std.ArrayList(u8) = .empty;
        r.appendRemainingUnlimited(arena, &list) catch return error.BadResponse;
        body_out = list.items;
    }

    return .{ .status = status, .body = body_out };
}

fn takeLine(r: *Io.Reader) ![]const u8 {
    const raw = try r.takeDelimiterInclusive('\n');
    var line = raw;
    if (line.len > 0 and line[line.len - 1] == '\n') line = line[0 .. line.len - 1];
    if (line.len > 0 and line[line.len - 1] == '\r') line = line[0 .. line.len - 1];
    return line;
}
