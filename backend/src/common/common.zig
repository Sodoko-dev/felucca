//! Root of the shared `common` module: re-exports and a small server runtime.
const std = @import("std");
const Io = std.Io;
const net = std.Io.net;

pub const http = @import("http.zig");
pub const client = @import("client.zig");
pub const jsonh = @import("json.zig");
pub const router = @import("router.zig");
pub const models = @import("models.zig");
pub const config = @import("config.zig");
pub const netdetect = @import("netdetect.zig");

/// Bearer-token check for an inbound request. Returns true if the request is
/// authorized: either no token is configured (auth disabled) or the request
/// carries `Authorization: Bearer <token>` matching `expected` (constant time).
pub fn authorized(expected: []const u8, auth_header: ?[]const u8) bool {
    if (expected.len == 0) return true; // auth disabled
    const hdr = auth_header orelse return false;
    const prefix = "Bearer ";
    if (!std.mem.startsWith(u8, hdr, prefix)) return false;
    const presented = std.mem.trim(u8, hdr[prefix.len..], " \t");
    return config.constantTimeEql(expected, presented);
}

/// A minimal blocking spinlock built on std.atomic.Mutex, which in this std only
/// exposes tryLock/unlock. Critical sections here are short, so spinning with a
/// yield is acceptable and avoids threading `Io` through every state mutation.
pub const SpinLock = struct {
    inner: std.atomic.Mutex = .unlocked,

    pub fn lock(self: *SpinLock) void {
        while (!self.inner.tryLock()) {
            std.Thread.yield() catch {};
        }
    }

    pub fn unlock(self: *SpinLock) void {
        self.inner.unlock();
    }
};

/// Current wall-clock time in whole unix seconds.
pub fn nowUnix(io: Io) i64 {
    const ts = Io.Timestamp.now(io, .real);
    return @intCast(@divTrunc(ts.nanoseconds, std.time.ns_per_s));
}

/// Signature of a per-request handler. It receives a request-scoped arena, the
/// parsed request, the response writer, and an opaque context pointer.
pub const Handler = *const fn (
    ctx: *anyopaque,
    arena: std.mem.Allocator,
    req: http.Request,
    w: *Io.Writer,
) anyerror!void;

/// Run a blocking thread-per-connection HTTP server on 0.0.0.0:port.
pub fn serve(
    gpa: std.mem.Allocator,
    io: Io,
    port: u16,
    ctx: *anyopaque,
    handler: Handler,
) !void {
    const any = net.IpAddress.parse("0.0.0.0", port) catch unreachable;
    var server = try any.listen(io, .{ .reuse_address = true });
    defer server.deinit(io);

    while (true) {
        const stream = server.accept(io) catch |err| {
            std.log.warn("accept failed: {s}", .{@errorName(err)});
            continue;
        };
        const conn = gpa.create(Conn) catch {
            stream.close(io);
            continue;
        };
        conn.* = .{ .gpa = gpa, .io = io, .stream = stream, .ctx = ctx, .handler = handler };
        const t = std.Thread.spawn(.{}, Conn.run, .{conn}) catch {
            handleConn(conn);
            gpa.destroy(conn);
            continue;
        };
        t.detach();
    }
}

const Conn = struct {
    gpa: std.mem.Allocator,
    io: Io,
    stream: net.Stream,
    ctx: *anyopaque,
    handler: Handler,

    fn run(self: *Conn) void {
        handleConn(self);
        self.gpa.destroy(self);
    }
};

fn handleConn(c: *Conn) void {
    defer c.stream.close(c.io);

    var arena_state = std.heap.ArenaAllocator.init(c.gpa);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    var rbuf: [http.max_header_bytes]u8 = undefined;
    var sr = c.stream.reader(c.io, &rbuf);
    const r = &sr.interface;

    var wbuf: [64 * 1024]u8 = undefined;
    var sw = c.stream.writer(c.io, &wbuf);
    const w = &sw.interface;

    const req = http.readRequest(arena, r) catch {
        http.writeJson(w, 400, "{\"error\":\"bad request\"}") catch {};
        return;
    };

    c.handler(c.ctx, arena, req, w) catch |err| {
        std.log.warn("handler error: {s}", .{@errorName(err)});
        http.writeJson(w, 500, "{\"error\":\"internal\"}") catch {};
    };
}

test {
    std.testing.refAllDecls(@This());
}
