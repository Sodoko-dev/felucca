//! hearth-agent — node agent. Registers with the control plane, heartbeats
//! every 5s, and manages firecracker microVMs over its local REST API
//! (0.0.0.0:9090).
const std = @import("std");
const Io = std.Io;
const common = @import("common");
const http = common.http;
const jsonh = common.jsonh;
const router = common.router;
const client = common.client;

const vmmod = @import("vm.zig");
const Manager = vmmod.Manager;
const ipalloc = @import("ipalloc.zig");
const cfgmod = common.config;

/// hearth-agent configuration. Defaults are production defaults per API-V2.md
/// §6; `bind` host:port drives the listen port. `advertise_addr` auto-detects
/// when left empty.
const Config = struct {
    bind: []const u8 = "0.0.0.0:9090",
    control_plane: []const u8 = "http://127.0.0.1:8080",
    advertise_addr: []const u8 = "",
    data_dir: []const u8 = "/srv/ignis",
    token: []const u8 = "",
    net: bool = true,
    net_cidr: []const u8 = "10.231.0.0/24",
    pool_size: u32 = 0,
    port: u16 = 9090,
};

const Agent = struct {
    gpa: std.mem.Allocator,
    io: Io,
    cfg: Config,
    mgr: Manager,
    node_id_buf: [64]u8 = undefined,
    node_id_len: usize = 0,
    id_mu: common.SpinLock = .{},

    fn nodeId(self: *Agent) []const u8 {
        self.id_mu.lock();
        defer self.id_mu.unlock();
        return self.node_id_buf[0..self.node_id_len];
    }

    fn setNodeId(self: *Agent, id: []const u8) void {
        self.id_mu.lock();
        defer self.id_mu.unlock();
        const n = @min(id.len, self.node_id_buf.len);
        @memcpy(self.node_id_buf[0..n], id[0..n]);
        self.node_id_len = n;
    }
};

pub fn main(init: std.process.Init) !void {
    const gpa = init.gpa;
    const io = init.io;

    var cfg = try loadConfig(gpa, io, init.environ_map, init.minimal.args);

    // Auto-detect advertise_addr when unset: which local address reaches the
    // control plane (route-aware UDP-connect + getsockname).
    if (cfg.advertise_addr.len == 0) {
        cfg.advertise_addr = detectAdvertiseAddr(gpa, cfg.control_plane) orelse "127.0.0.1";
    }

    const cidr = ipalloc.Cidr.parse(cfg.net_cidr) orelse ipalloc.Cidr.parse("10.231.0.0/24").?;

    var agent = Agent{
        .gpa = gpa,
        .io = io,
        .cfg = cfg,
        .mgr = Manager.init(gpa, io, cfg.data_dir, cfg.net, cidr, cfg.pool_size),
    };

    std.log.info("hearth-agent: control_plane={s} data_dir={s} advertise={s} port={d} net={s} cidr={s} pool={d} auth={s}", .{
        cfg.control_plane, cfg.data_dir, cfg.advertise_addr, cfg.port,
        if (cfg.net) "on" else "off",                                  cfg.net_cidr, cfg.pool_size,
        if (cfg.token.len > 0) "on" else "off",
    });

    // Host networking + IP allocator, reconcile persisted instances, prewarm pool.
    agent.mgr.setupHost();
    {
        var arena_state = std.heap.ArenaAllocator.init(gpa);
        defer arena_state.deinit();
        agent.mgr.reconcile(arena_state.allocator());
    }

    // Background registration + heartbeat loop.
    const hb = try std.Thread.spawn(.{}, heartbeatLoop, .{&agent});
    hb.detach();

    // Async warm-pool prewarm/refill thread.
    if (cfg.pool_size > 0) {
        const pt = try std.Thread.spawn(.{}, poolLoop, .{&agent});
        pt.detach();
    }

    try common.serve(gpa, io, cfg.port, @ptrCast(&agent), handle);
}

fn loadConfig(gpa: std.mem.Allocator, io: Io, env: *cfgmod.EnvMap, args: std.process.Args) !Config {
    var cfg = Config{};
    const specs = [_]cfgmod.Spec{
        cfgmod.strSpec("bind", "HEARTH_AGENT_BIND", "--bind", &cfg.bind),
        cfgmod.strSpec("control_plane", "HEARTH_CONTROL_PLANE", "--control-plane", &cfg.control_plane),
        cfgmod.strSpec("advertise_addr", "HEARTH_ADVERTISE_ADDR", "--advertise-addr", &cfg.advertise_addr),
        cfgmod.strSpec("data_dir", "HEARTH_DATA_DIR", "--data-dir", &cfg.data_dir),
        cfgmod.strSpec("token", "HEARTH_TOKEN", "--token", &cfg.token),
        cfgmod.boolSpec("net", "HEARTH_NET", "--net", &cfg.net),
        cfgmod.strSpec("net_cidr", "HEARTH_NET_CIDR", "--net-cidr", &cfg.net_cidr),
        cfgmod.u32Spec("pool_size", "HEARTH_POOL_SIZE", "--pool-size", &cfg.pool_size),
        cfgmod.u16Spec("port", "HEARTH_AGENT_PORT", "--port", &cfg.port),
    };
    try cfgmod.load(gpa, io, env, args, &specs);
    if (std.mem.lastIndexOfScalar(u8, cfg.bind, ':')) |colon| {
        if (std.fmt.parseInt(u16, cfg.bind[colon + 1 ..], 10)) |p| {
            cfg.port = p;
        } else |_| {}
    }
    return cfg;
}

/// Determine the source IP used to reach the control plane host.
fn detectAdvertiseAddr(gpa: std.mem.Allocator, control_plane: []const u8) ?[]const u8 {
    const host, const port = splitHostPort(control_plane);
    var buf: [16]u8 = undefined;
    const detected = common.netdetect.sourceAddrFor(host, port, &buf) orelse return null;
    return gpa.dupe(u8, detected) catch null;
}

fn poolLoop(agent: *Agent) void {
    while (true) {
        var arena_state = std.heap.ArenaAllocator.init(agent.gpa);
        agent.mgr.refillPool(arena_state.allocator());
        arena_state.deinit();
        sleepSecs(agent.io, 5);
    }
}

// ---- registration + heartbeat ----

fn heartbeatLoop(agent: *Agent) void {
    while (true) {
        registerOnce(agent) catch |err| {
            std.log.warn("register failed: {s}", .{@errorName(err)});
        };
        if (agent.nodeId().len > 0) break;
        sleepSecs(agent.io, 5);
    }
    while (true) {
        heartbeatOnce(agent) catch |err| {
            std.log.warn("heartbeat failed: {s}", .{@errorName(err)});
        };
        sleepSecs(agent.io, 5);
    }
}

fn sleepSecs(io: Io, secs: u64) void {
    Io.sleep(io, .{ .nanoseconds = @intCast(secs * std.time.ns_per_s) }, .awake) catch {};
}

fn registerOnce(agent: *Agent) !void {
    var arena_state = std.heap.ArenaAllocator.init(agent.gpa);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const hostname = readHostname(agent.io, arena);
    const cpus = readCpus(agent.io, arena);
    const mem_total = readMemTotalMib(agent.io, arena);

    const addr = try std.fmt.allocPrint(arena, "{s}:{d}", .{ agent.cfg.advertise_addr, agent.cfg.port });

    var b = jsonh.Builder.init(arena);
    try b.byte('{');
    try b.key("hostname");
    try b.str(hostname);
    try b.raw(",");
    try b.key("addr");
    try b.str(addr);
    try b.raw(",");
    try b.key("cpus");
    try b.print("{d}", .{cpus});
    try b.raw(",");
    try b.key("mem_total_mib");
    try b.print("{d}", .{mem_total});
    try b.byte('}');

    const host, const port = splitHostPort(agent.cfg.control_plane);
    const resp = client.requestTcpAuth(arena, agent.io, host, port, "POST", "/api/v1/agents/register", b.items(), agent.cfg.token) catch return error.RegisterFailed;
    if (resp.status >= 300) return error.RegisterRejected;

    var p = jsonh.parse(arena, resp.body) catch return error.BadRegisterResponse;
    defer p.deinit();
    const id = jsonh.getString(p.root(), "id") orelse return error.NoIdInResponse;
    agent.setNodeId(id);
    std.log.info("registered as node {s}", .{id});
}

fn heartbeatOnce(agent: *Agent) !void {
    const id = agent.nodeId();
    if (id.len == 0) return error.NotRegistered;

    var arena_state = std.heap.ArenaAllocator.init(agent.gpa);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const mem_free = readMemAvailableMib(agent.io, arena);
    const vm_count = agent.mgr.liveCount();
    const pool_size = agent.mgr.poolCount();

    var b = jsonh.Builder.init(arena);
    try b.byte('{');
    try b.key("id");
    try b.str(id);
    try b.raw(",");
    try b.key("mem_free_mib");
    try b.print("{d}", .{mem_free});
    try b.raw(",");
    try b.key("vm_count");
    try b.print("{d}", .{vm_count});
    try b.raw(",");
    try b.key("pool_size");
    try b.print("{d}", .{pool_size});
    try b.byte('}');

    const host, const port = splitHostPort(agent.cfg.control_plane);
    const resp = client.requestTcpAuth(arena, agent.io, host, port, "POST", "/api/v1/agents/heartbeat", b.items(), agent.cfg.token) catch return error.HeartbeatFailed;
    if (resp.status >= 300) return error.HeartbeatRejected;
}

// ---- request handling ----

fn handle(ctx: *anyopaque, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    const agent: *Agent = @ptrCast(@alignCast(ctx));
    const path = req.path;
    const m = req.method;

    if (std.mem.eql(u8, path, "/healthz") and m == .GET) {
        return http.writeJson(w, 200, "{\"ok\":true}");
    }

    // All /v1/* calls require the bearer token when one is configured.
    if (!common.authorized(agent.cfg.token, req.authorization)) {
        return http.writeJson(w, 401, "{\"error\":\"unauthorized\"}");
    }

    if (std.mem.eql(u8, path, "/v1/vms") and m == .GET) {
        return listVms(agent, arena, w);
    }
    if (std.mem.eql(u8, path, "/v1/vms") and m == .POST) {
        return createVm(agent, arena, req, w);
    }
    if (router.match("/v1/vms/{id}/sleep", path)) |mt| {
        if (m == .POST) return sleepVm(agent, arena, mt.id.?, w);
    }
    if (router.match("/v1/vms/{id}/wake", path)) |mt| {
        if (m == .POST) return wakeVm(agent, arena, mt.id.?, w);
    }
    if (router.match("/v1/vms/{id}/fork", path)) |mt| {
        if (m == .POST) return forkVm(agent, arena, req, mt.id.?, w);
    }
    inline for (.{ "pause", "resume", "stop", "start" }) |action| {
        if (router.match("/v1/vms/{id}/" ++ action, path)) |mt| {
            if (m == .POST) return vmAction(agent, arena, mt.id.?, action, w);
        }
    }
    if (router.match("/v1/vms/{id}", path)) |mt| {
        if (m == .DELETE) return deleteVm(agent, arena, mt.id.?, w);
    }
    return http.writeJson(w, 404, "{\"error\":\"not found\"}");
}

fn listVms(agent: *Agent, arena: std.mem.Allocator, w: *Io.Writer) anyerror!void {
    const body = try agent.mgr.listJson(arena);
    return http.writeJson(w, 200, body);
}

fn createVm(agent: *Agent, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    var p = jsonh.parse(arena, req.body) catch return http.writeJson(w, 400, "{\"error\":\"bad json\"}");
    defer p.deinit();
    const root = p.root();

    const id = jsonh.getString(root, "id") orelse return http.writeJson(w, 400, "{\"error\":\"id required\"}");
    const name = jsonh.getString(root, "name") orelse id;
    const vcpus: u32 = @intCast(jsonh.getInt(root, "vcpus") orelse 1);
    const mem_mib: u64 = @intCast(jsonh.getInt(root, "mem_mib") orelse 256);

    agent.mgr.create(arena, id, name, vcpus, mem_mib) catch |err| {
        std.log.err("create vm {s} failed: {s}", .{ id, @errorName(err) });
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"create failed: {s}\"}}", .{@errorName(err)}) catch "{\"error\":\"create failed\"}";
        return http.writeJson(w, 500, msg);
    };
    // Report the assigned guest IP (null when networking is off).
    var b = jsonh.Builder.init(arena);
    try b.raw("{\"ok\":true,\"ip\":");
    try b.optStr(agent.mgr.ipOf(arena, id));
    try b.byte('}');
    return http.writeJson(w, 201, b.items());
}

fn sleepVm(agent: *Agent, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    agent.mgr.sleep(arena, id) catch |err| {
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"{s}\"}}", .{@errorName(err)}) catch "{\"error\":\"sleep failed\"}";
        return http.writeJson(w, 500, msg);
    };
    return http.writeJson(w, 200, "{\"ok\":true}");
}

fn wakeVm(agent: *Agent, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    const wake_ms = agent.mgr.wake(arena, id) catch |err| {
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"{s}\"}}", .{@errorName(err)}) catch "{\"error\":\"wake failed\"}";
        return http.writeJson(w, 500, msg);
    };
    const body = try std.fmt.allocPrint(arena, "{{\"ok\":true,\"wake_ms\":{d}}}", .{wake_ms});
    return http.writeJson(w, 200, body);
}

fn forkVm(agent: *Agent, arena: std.mem.Allocator, req: http.Request, parent_id: []const u8, w: *Io.Writer) anyerror!void {
    var p = jsonh.parse(arena, req.body) catch return http.writeJson(w, 400, "{\"error\":\"bad json\"}");
    defer p.deinit();
    const root = p.root();
    const child_id = jsonh.getString(root, "id") orelse return http.writeJson(w, 400, "{\"error\":\"id required\"}");
    const child_name = jsonh.getString(root, "name") orelse child_id;

    const child_ip = agent.mgr.fork(arena, parent_id, child_id, child_name) catch |err| {
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"{s}\"}}", .{@errorName(err)}) catch "{\"error\":\"fork failed\"}";
        return http.writeJson(w, 500, msg);
    };
    var b = jsonh.Builder.init(arena);
    try b.raw("{\"ok\":true,\"ip\":");
    try b.optStr(child_ip);
    try b.byte('}');
    return http.writeJson(w, 201, b.items());
}

fn vmAction(agent: *Agent, arena: std.mem.Allocator, id: []const u8, comptime action: []const u8, w: *Io.Writer) anyerror!void {
    const ok = blk: {
        if (comptime std.mem.eql(u8, action, "pause")) break :blk agent.mgr.pause(arena, id);
        if (comptime std.mem.eql(u8, action, "resume")) break :blk agent.mgr.resume_(arena, id);
        if (comptime std.mem.eql(u8, action, "stop")) break :blk agent.mgr.stop(id);
        if (comptime std.mem.eql(u8, action, "start")) break :blk agent.mgr.start(arena, id);
        break :blk error.UnknownAction;
    } catch |err| {
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"{s}\"}}", .{@errorName(err)}) catch "{\"error\":\"action failed\"}";
        return http.writeJson(w, 500, msg);
    };
    _ = ok;
    return http.writeEmpty(w, 200);
}

fn deleteVm(agent: *Agent, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    agent.mgr.delete(arena, id) catch |err| {
        const msg = std.fmt.allocPrint(arena, "{{\"error\":\"{s}\"}}", .{@errorName(err)}) catch "{\"error\":\"delete failed\"}";
        return http.writeJson(w, 500, msg);
    };
    return http.writeEmpty(w, 204);
}

// ---- /proc + host info readers ----

fn readHostname(io: Io, arena: std.mem.Allocator) []const u8 {
    const data = Io.Dir.cwd().readFileAlloc(io, "/etc/hostname", arena, .limited(256)) catch return "unknown";
    const trimmed = std.mem.trim(u8, data, " \t\r\n");
    if (trimmed.len == 0) return "unknown";
    return trimmed;
}

fn splitHostPort(addr: []const u8) struct { []const u8, u16 } {
    var a = addr;
    if (std.mem.startsWith(u8, a, "http://")) a = a["http://".len..];
    if (std.mem.startsWith(u8, a, "https://")) a = a["https://".len..];
    if (std.mem.indexOfScalar(u8, a, '/')) |slash| a = a[0..slash];
    if (std.mem.lastIndexOfScalar(u8, a, ':')) |colon| {
        const host = a[0..colon];
        const port = std.fmt.parseInt(u16, a[colon + 1 ..], 10) catch 8080;
        return .{ host, port };
    }
    return .{ a, 8080 };
}

/// Read a procfs/special file that reports size 0 via a streaming reader, since
/// readFileAlloc trusts the (zero) reported size and would read nothing.
fn readProc(io: Io, arena: std.mem.Allocator, path: []const u8) ?[]const u8 {
    const file = Io.Dir.cwd().openFile(io, path, .{}) catch return null;
    defer file.close(io);
    var buf: [4096]u8 = undefined;
    // Streaming (read-based) mode: positional pread on procfs returns 0 at the
    // reported size of 0, so we must read sequentially until EOF.
    var fr = file.readerStreaming(io, &buf);
    return fr.interface.allocRemaining(arena, .limited(1 << 20)) catch null;
}

fn readCpus(io: Io, arena: std.mem.Allocator) u32 {
    const data = readProc(io, arena, "/proc/cpuinfo") orelse return 1;
    var count: u32 = 0;
    var it = std.mem.tokenizeScalar(u8, data, '\n');
    while (it.next()) |line| {
        if (std.mem.startsWith(u8, line, "processor")) count += 1;
    }
    return if (count == 0) 1 else count;
}

fn readMemTotalMib(io: Io, arena: std.mem.Allocator) u64 {
    return readMeminfoKey(io, arena, "MemTotal:") / 1024;
}

fn readMemAvailableMib(io: Io, arena: std.mem.Allocator) u64 {
    return readMeminfoKey(io, arena, "MemAvailable:") / 1024;
}

fn readMeminfoKey(io: Io, arena: std.mem.Allocator, key: []const u8) u64 {
    const data = readProc(io, arena, "/proc/meminfo") orelse return 0;
    var it = std.mem.tokenizeScalar(u8, data, '\n');
    while (it.next()) |line| {
        if (std.mem.startsWith(u8, line, key)) {
            var t = std.mem.tokenizeAny(u8, line[key.len..], " \t");
            const num = t.next() orelse return 0;
            return std.fmt.parseInt(u64, num, 10) catch 0;
        }
    }
    return 0;
}
