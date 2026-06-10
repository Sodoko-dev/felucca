//! hearthd — Hearth control plane. Listens on 0.0.0.0:8080, exposes the REST
//! API, schedules sandboxes onto agents, serves the static UI, and persists
//! in-memory state to a JSON file.
const std = @import("std");
const Io = std.Io;
const common = @import("common");
const http = common.http;
const jsonh = common.jsonh;
const router = common.router;
const models = common.models;
const client = common.client;

const State = @import("state.zig").State;
const cfgmod = common.config;

/// hearthd configuration. Defaults are production defaults per API-V2.md §6;
/// the local lab overrides them via flags/env/config-file. `bind` is parsed
/// into `port` for the listener (the bind host is always 0.0.0.0 in-binary).
const Config = struct {
    bind: []const u8 = "0.0.0.0:8080",
    ui_dir: []const u8 = "/usr/share/hearth/ui",
    state_path: []const u8 = "/var/lib/hearth/state.json",
    token: []const u8 = "",
    port: u16 = 8080,
};

const App = struct {
    gpa: std.mem.Allocator,
    io: Io,
    cfg: Config,
    state: State,
};

pub fn main(init: std.process.Init) !void {
    const gpa = init.gpa;
    const io = init.io;

    const cfg = try loadConfig(gpa, io, init.environ_map, init.minimal.args);

    var app = App{
        .gpa = gpa,
        .io = io,
        .cfg = cfg,
        .state = State.init(gpa),
    };
    app.state.load(io, cfg.state_path) catch |err| {
        std.log.warn("could not load state from {s}: {s}", .{ cfg.state_path, @errorName(err) });
    };

    std.log.info("hearthd listening on 0.0.0.0:{d} (ui_dir={s}, state={s}, auth={s})", .{
        cfg.port, cfg.ui_dir, cfg.state_path, if (cfg.token.len > 0) "on" else "off",
    });
    try common.serve(gpa, io, cfg.port, @ptrCast(&app), handle);
}

fn loadConfig(gpa: std.mem.Allocator, io: Io, env: *cfgmod.EnvMap, args: std.process.Args) !Config {
    var cfg = Config{};
    // `--port` and `HEARTH_PORT` remain accepted for backwards compat; `bind`
    // (per contract) is authoritative and its port overrides afterwards.
    const specs = [_]cfgmod.Spec{
        cfgmod.strSpec("bind", "HEARTH_BIND", "--bind", &cfg.bind),
        cfgmod.strSpec("ui_dir", "HEARTH_UI_DIR", "--ui-dir", &cfg.ui_dir),
        cfgmod.strSpec("state_path", "HEARTH_STATE", "--state", &cfg.state_path),
        cfgmod.strSpec("token", "HEARTH_TOKEN", "--token", &cfg.token),
        cfgmod.u16Spec("port", "HEARTH_PORT", "--port", &cfg.port),
    };
    try cfgmod.load(gpa, io, env, args, &specs);
    // Derive the listen port from `bind` host:port unless an explicit --port
    // was the source (we let the explicit port win only if bind has no port).
    if (std.mem.lastIndexOfScalar(u8, cfg.bind, ':')) |colon| {
        if (std.fmt.parseInt(u16, cfg.bind[colon + 1 ..], 10)) |p| {
            cfg.port = p;
        } else |_| {}
    }
    return cfg;
}

fn handle(ctx: *anyopaque, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    const app: *App = @ptrCast(@alignCast(ctx));
    app.state.bumpRequests();

    const path = req.path;

    // Health.
    if (std.mem.eql(u8, path, "/healthz")) {
        return http.writeJson(w, 200, "{\"ok\":true}");
    }
    // Metrics.
    if (std.mem.eql(u8, path, "/metrics")) {
        return metrics(app, arena, w);
    }

    // API routes. Guarded by the bearer token when one is configured;
    // /healthz, /metrics and the static UI stay open (handled above/below).
    if (std.mem.startsWith(u8, path, "/api/")) {
        if (!common.authorized(app.cfg.token, req.authorization)) {
            return http.writeJson(w, 401, "{\"error\":\"unauthorized\"}");
        }
        return api(app, arena, req, w);
    }

    // Otherwise serve static UI.
    return serveStatic(app, arena, path, w);
}

fn api(app: *App, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    const path = req.path;
    const m = req.method;

    if (std.mem.eql(u8, path, "/api/v1/nodes") and m == .GET) {
        return listNodes(app, arena, w);
    }
    if (std.mem.eql(u8, path, "/api/v1/agents/register") and m == .POST) {
        return agentRegister(app, arena, req, w);
    }
    if (std.mem.eql(u8, path, "/api/v1/agents/heartbeat") and m == .POST) {
        return agentHeartbeat(app, arena, req, w);
    }
    if (std.mem.eql(u8, path, "/api/v1/sandboxes") and m == .GET) {
        return listSandboxes(app, arena, w);
    }
    if (std.mem.eql(u8, path, "/api/v1/sandboxes") and m == .POST) {
        return createSandbox(app, arena, req, w);
    }
    // /api/v1/sandboxes/{id}/...
    if (router.match("/api/v1/sandboxes/{id}/fork", path)) |mt| {
        if (m == .POST) return forkSandbox(app, arena, req, mt.id.?, w);
    }
    if (router.match("/api/v1/sandboxes/{id}/sleep", path)) |mt| {
        if (m == .POST) return sleepSandbox(app, arena, mt.id.?, w);
    }
    if (router.match("/api/v1/sandboxes/{id}/wake", path)) |mt| {
        if (m == .POST) return wakeSandbox(app, arena, mt.id.?, w);
    }
    inline for (.{ "stop", "start", "pause", "resume" }) |action| {
        if (router.match("/api/v1/sandboxes/{id}/" ++ action, path)) |mt| {
            if (m == .POST) return sandboxAction(app, arena, mt.id.?, action, w);
        }
    }
    if (router.match("/api/v1/sandboxes/{id}", path)) |mt| {
        if (m == .GET) return getSandbox(app, arena, mt.id.?, w);
        if (m == .DELETE) return deleteSandbox(app, arena, mt.id.?, w);
    }

    return http.writeJson(w, 404, "{\"error\":\"not found\"}");
}

// ---- Node endpoints ----

fn listNodes(app: *App, arena: std.mem.Allocator, w: *Io.Writer) anyerror!void {
    const now = common.nowUnix(app.io);
    app.state.mu.lock();
    defer app.state.mu.unlock();

    var b = jsonh.Builder.init(arena);
    try b.raw("{\"nodes\":[");
    var first = true;
    for (app.state.nodes.items) |n| {
        if (!first) try b.byte(',');
        first = false;
        try n.writeJson(&b, now);
    }
    try b.raw("]}");
    return http.writeJson(w, 200, b.items());
}

fn agentRegister(app: *App, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    var p = jsonh.parse(arena, req.body) catch return http.writeJson(w, 400, "{\"error\":\"bad json\"}");
    defer p.deinit();
    const root = p.root();

    const hostname = jsonh.getString(root, "hostname") orelse return http.writeJson(w, 400, "{\"error\":\"hostname required\"}");
    const addr = jsonh.getString(root, "addr") orelse "";
    const cpus: u32 = @intCast(jsonh.getInt(root, "cpus") orelse 0);
    const mem_total: u64 = @intCast(jsonh.getInt(root, "mem_total_mib") orelse 0);

    const now = common.nowUnix(app.io);
    const id = try app.state.registerNode(hostname, addr, cpus, mem_total, now);
    try app.state.persist(app.io, app.cfg.state_path);

    var b = jsonh.Builder.init(arena);
    try b.byte('{');
    try b.key("id");
    try b.str(id);
    try b.byte('}');
    return http.writeJson(w, 200, b.items());
}

fn agentHeartbeat(app: *App, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    var p = jsonh.parse(arena, req.body) catch return http.writeJson(w, 400, "{\"error\":\"bad json\"}");
    defer p.deinit();
    const root = p.root();

    const id = jsonh.getString(root, "id") orelse return http.writeJson(w, 400, "{\"error\":\"id required\"}");
    const mem_free: u64 = @intCast(jsonh.getInt(root, "mem_free_mib") orelse 0);
    const vm_count: u32 = @intCast(jsonh.getInt(root, "vm_count") orelse 0);
    const pool_size: u32 = @intCast(jsonh.getInt(root, "pool_size") orelse 0);
    const now = common.nowUnix(app.io);

    const ok = app.state.heartbeat(id, mem_free, vm_count, pool_size, now);
    if (!ok) return http.writeJson(w, 404, "{\"error\":\"unknown node\"}");
    return http.writeEmpty(w, 200);
}

// ---- Sandbox endpoints ----

fn listSandboxes(app: *App, arena: std.mem.Allocator, w: *Io.Writer) anyerror!void {
    app.state.mu.lock();
    defer app.state.mu.unlock();

    var b = jsonh.Builder.init(arena);
    try b.raw("{\"sandboxes\":[");
    var first = true;
    for (app.state.sandboxes.items) |s| {
        if (!first) try b.byte(',');
        first = false;
        try s.writeJson(&b);
    }
    try b.raw("]}");
    return http.writeJson(w, 200, b.items());
}

fn getSandbox(app: *App, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(id) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
    var b = jsonh.Builder.init(arena);
    try s.writeJson(&b);
    return http.writeJson(w, 200, b.items());
}

fn createSandbox(app: *App, arena: std.mem.Allocator, req: http.Request, w: *Io.Writer) anyerror!void {
    var p = jsonh.parse(arena, req.body) catch return http.writeJson(w, 400, "{\"error\":\"bad json\"}");
    defer p.deinit();
    const root = p.root();

    const name = jsonh.getString(root, "name") orelse return http.writeJson(w, 400, "{\"error\":\"name required\"}");
    const namespace = jsonh.getString(root, "namespace") orelse "default";
    const vcpus: u32 = @intCast(jsonh.getInt(root, "vcpus") orelse 1);
    const mem_mib: u64 = @intCast(jsonh.getInt(root, "mem_mib") orelse 256);
    const now = common.nowUnix(app.io);

    // Schedule: pick ready node with lowest vm_count. Create the sandbox record
    // in "creating" state under the lock, capturing the target agent's address.
    var agent_addr_buf: [128]u8 = undefined;
    var agent_addr_len: usize = 0;
    var sb_id_buf: [40]u8 = undefined;
    var sb_id_len: usize = 0;
    {
        app.state.mu.lock();
        defer app.state.mu.unlock();

        const node = app.state.pickNode(now) orelse
            return http.writeJson(w, 503, "{\"error\":\"no ready node\"}");
        const addr = node.addr;
        @memcpy(agent_addr_buf[0..addr.len], addr);
        agent_addr_len = addr.len;

        const sb = try app.state.createSandbox(name, namespace, node.id, vcpus, mem_mib, now);
        @memcpy(sb_id_buf[0..sb.id.len], sb.id);
        sb_id_len = sb.id.len;
    }
    try app.state.persist(app.io, app.cfg.state_path);

    const agent_addr = agent_addr_buf[0..agent_addr_len];
    const sb_id = sb_id_buf[0..sb_id_len];

    // Build the create request for the agent.
    var cb = jsonh.Builder.init(arena);
    try cb.byte('{');
    try cb.key("id");
    try cb.str(sb_id);
    try cb.raw(",");
    try cb.key("name");
    try cb.str(name);
    try cb.raw(",");
    try cb.key("vcpus");
    try cb.print("{d}", .{vcpus});
    try cb.raw(",");
    try cb.key("mem_mib");
    try cb.print("{d}", .{mem_mib});
    try cb.byte('}');

    const host, const port = splitHostPort(agent_addr);
    const resp = client.requestTcpAuth(arena, app.io, host, port, "POST", "/v1/vms", cb.items(), app.cfg.token) catch {
        app.state.setSandboxState(sb_id, .@"error");
        try app.state.persist(app.io, app.cfg.state_path);
        return http.writeJson(w, 502, "{\"error\":\"agent unreachable\"}");
    };
    if (resp.status >= 300) {
        app.state.setSandboxState(sb_id, .@"error");
        try app.state.persist(app.io, app.cfg.state_path);
        return http.writeJson(w, 502, "{\"error\":\"agent create failed\"}");
    }
    // Capture the guest IP the agent assigned (null when networking is off).
    if (jsonh.parse(arena, resp.body)) |ap_| {
        var ap = ap_;
        defer ap.deinit();
        if (jsonh.getString(ap.root(), "ip")) |assigned_ip| {
            if (assigned_ip.len > 0) app.state.setSandboxIp(sb_id, assigned_ip);
        }
    } else |_| {}
    app.state.setSandboxState(sb_id, .running);
    try app.state.persist(app.io, app.cfg.state_path);

    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(sb_id) orelse return http.writeJson(w, 500, "{\"error\":\"lost sandbox\"}");
    var b = jsonh.Builder.init(arena);
    try s.writeJson(&b);
    return http.writeJson(w, 201, b.items());
}

fn sandboxAction(app: *App, arena: std.mem.Allocator, id: []const u8, comptime action: []const u8, w: *Io.Writer) anyerror!void {
    var agent_addr_buf: [128]u8 = undefined;
    var agent_addr_len: usize = 0;
    {
        app.state.mu.lock();
        defer app.state.mu.unlock();
        const s = app.state.findSandbox(id) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
        const node_id = s.node_id orelse return http.writeJson(w, 409, "{\"error\":\"sandbox has no node\"}");
        const node = app.state.findNode(node_id) orelse return http.writeJson(w, 409, "{\"error\":\"node gone\"}");
        @memcpy(agent_addr_buf[0..node.addr.len], node.addr);
        agent_addr_len = node.addr.len;
    }
    const agent_addr = agent_addr_buf[0..agent_addr_len];
    const host, const port = splitHostPort(agent_addr);

    const agent_path = try std.fmt.allocPrint(arena, "/v1/vms/{s}/{s}", .{ id, action });
    const resp = client.requestTcpAuth(arena, app.io, host, port, "POST", agent_path, null, app.cfg.token) catch {
        return http.writeJson(w, 502, "{\"error\":\"agent unreachable\"}");
    };
    if (resp.status >= 300) return http.writeJson(w, 502, "{\"error\":\"agent action failed\"}");

    const new_state: ?models.SandboxState = comptime blk: {
        if (std.mem.eql(u8, action, "stop")) break :blk .stopped;
        if (std.mem.eql(u8, action, "start")) break :blk .running;
        if (std.mem.eql(u8, action, "pause")) break :blk .paused;
        if (std.mem.eql(u8, action, "resume")) break :blk .running;
        break :blk null;
    };
    if (new_state) |ns| {
        app.state.setSandboxState(id, ns);
        try app.state.persist(app.io, app.cfg.state_path);
    }
    return http.writeEmpty(w, 200);
}

fn deleteSandbox(app: *App, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    var agent_addr_buf: [128]u8 = undefined;
    var agent_addr_len: usize = 0;
    {
        app.state.mu.lock();
        defer app.state.mu.unlock();
        const s = app.state.findSandbox(id) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
        if (s.node_id) |node_id| {
            if (app.state.findNode(node_id)) |node| {
                @memcpy(agent_addr_buf[0..node.addr.len], node.addr);
                agent_addr_len = node.addr.len;
            }
        }
    }
    if (agent_addr_len > 0) {
        const agent_addr = agent_addr_buf[0..agent_addr_len];
        const host, const port = splitHostPort(agent_addr);
        const agent_path = try std.fmt.allocPrint(arena, "/v1/vms/{s}", .{id});
        _ = client.requestTcpAuth(arena, app.io, host, port, "DELETE", agent_path, null, app.cfg.token) catch {};
    }
    app.state.removeSandbox(id);
    try app.state.persist(app.io, app.cfg.state_path);
    return http.writeEmpty(w, 204);
}

/// Resolve the owning agent address for a sandbox into `buf`. Returns the
/// slice, or null (writing a 404/409) — caller must check for null.
fn resolveAgent(app: *App, id: []const u8, buf: []u8) ?usize {
    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(id) orelse return null;
    const node_id = s.node_id orelse return null;
    const node = app.state.findNode(node_id) orelse return null;
    @memcpy(buf[0..node.addr.len], node.addr);
    return node.addr.len;
}

/// POST /api/v1/sandboxes/{id}/sleep — proxy to the agent, persist `sleeping`.
fn sleepSandbox(app: *App, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    var buf: [128]u8 = undefined;
    const alen = resolveAgent(app, id, &buf) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
    const host, const port = splitHostPort(buf[0..alen]);

    const agent_path = try std.fmt.allocPrint(arena, "/v1/vms/{s}/sleep", .{id});
    const resp = client.requestTcpAuth(arena, app.io, host, port, "POST", agent_path, null, app.cfg.token) catch {
        return http.writeJson(w, 502, "{\"error\":\"agent unreachable\"}");
    };
    if (resp.status >= 300) return http.writeJson(w, 502, "{\"error\":\"agent sleep failed\"}");

    app.state.setSandboxState(id, .sleeping);
    try app.state.persist(app.io, app.cfg.state_path);

    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(id) orelse return http.writeJson(w, 500, "{\"error\":\"lost sandbox\"}");
    var b = jsonh.Builder.init(arena);
    try s.writeJson(&b);
    return http.writeJson(w, 200, b.items());
}

/// POST /api/v1/sandboxes/{id}/wake — proxy to the agent, return wake_ms.
fn wakeSandbox(app: *App, arena: std.mem.Allocator, id: []const u8, w: *Io.Writer) anyerror!void {
    var buf: [128]u8 = undefined;
    const alen = resolveAgent(app, id, &buf) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
    const host, const port = splitHostPort(buf[0..alen]);

    const agent_path = try std.fmt.allocPrint(arena, "/v1/vms/{s}/wake", .{id});
    const resp = client.requestTcpAuth(arena, app.io, host, port, "POST", agent_path, null, app.cfg.token) catch {
        return http.writeJson(w, 502, "{\"error\":\"agent unreachable\"}");
    };
    if (resp.status >= 300) return http.writeJson(w, 502, "{\"error\":\"agent wake failed\"}");

    // The agent reports its measured wake latency in `wake_ms`.
    var wake_ms: u64 = 0;
    if (jsonh.parse(arena, resp.body)) |ap_| {
        var ap = ap_;
        defer ap.deinit();
        if (jsonh.getInt(ap.root(), "wake_ms")) |v| wake_ms = @intCast(@max(0, v));
    } else |_| {}

    app.state.setSandboxState(id, .running);
    app.state.recordWake(wake_ms);
    try app.state.persist(app.io, app.cfg.state_path);

    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(id) orelse return http.writeJson(w, 500, "{\"error\":\"lost sandbox\"}");
    var b = jsonh.Builder.init(arena);
    // sandbox JSON with wake_ms appended.
    try s.writeJson(&b);
    // Splice wake_ms in before the closing brace.
    var body = b.items();
    body = body[0 .. body.len - 1]; // drop trailing '}'
    var ob = jsonh.Builder.init(arena);
    try ob.raw(body);
    try ob.raw(",");
    try ob.key("wake_ms");
    try ob.print("{d}", .{wake_ms});
    try ob.byte('}');
    return http.writeJson(w, 200, ob.items());
}

/// POST /api/v1/sandboxes/{id}/fork — create a child sandbox from the parent's
/// snapshot on the same node, with its own tap. Returns 201 child JSON.
fn forkSandbox(app: *App, arena: std.mem.Allocator, req: http.Request, parent_id: []const u8, w: *Io.Writer) anyerror!void {
    var child_name: []const u8 = "fork";
    if (jsonh.parse(arena, req.body)) |p_| {
        var p = p_;
        defer p.deinit();
        if (jsonh.getString(p.root(), "name")) |n| child_name = try arena.dupe(u8, n);
    } else |_| {}

    // Snapshot the parent's placement + shape, and create the child record.
    var agent_addr_buf: [128]u8 = undefined;
    var agent_addr_len: usize = 0;
    var child_id_buf: [40]u8 = undefined;
    var child_id_len: usize = 0;
    {
        app.state.mu.lock();
        defer app.state.mu.unlock();
        const parent = app.state.findSandbox(parent_id) orelse return http.writeJson(w, 404, "{\"error\":\"not found\"}");
        switch (parent.state) {
            .running, .paused, .sleeping => {},
            else => return http.writeJson(w, 409, "{\"error\":\"parent must be running, paused, or sleeping\"}"),
        }
        const node_id = parent.node_id orelse return http.writeJson(w, 409, "{\"error\":\"parent has no node\"}");
        const node = app.state.findNode(node_id) orelse return http.writeJson(w, 409, "{\"error\":\"node gone\"}");
        @memcpy(agent_addr_buf[0..node.addr.len], node.addr);
        agent_addr_len = node.addr.len;

        const now = common.nowUnix(app.io);
        const child = try app.state.createForkChild(child_name, parent.namespace, node_id, parent.vcpus, parent.mem_mib, parent_id, now);
        @memcpy(child_id_buf[0..child.id.len], child.id);
        child_id_len = child.id.len;
    }
    try app.state.persist(app.io, app.cfg.state_path);

    const agent_addr = agent_addr_buf[0..agent_addr_len];
    const child_id = child_id_buf[0..child_id_len];
    const host, const port = splitHostPort(agent_addr);

    // Agent fork body: parent id, child id, child name.
    var cb = jsonh.Builder.init(arena);
    try cb.byte('{');
    try cb.key("id");
    try cb.str(child_id);
    try cb.raw(",");
    try cb.key("name");
    try cb.str(child_name);
    try cb.byte('}');

    const agent_path = try std.fmt.allocPrint(arena, "/v1/vms/{s}/fork", .{parent_id});
    const resp = client.requestTcpAuth(arena, app.io, host, port, "POST", agent_path, cb.items(), app.cfg.token) catch {
        app.state.setSandboxState(child_id, .@"error");
        try app.state.persist(app.io, app.cfg.state_path);
        return http.writeJson(w, 502, "{\"error\":\"agent unreachable\"}");
    };
    if (resp.status >= 300) {
        app.state.setSandboxState(child_id, .@"error");
        try app.state.persist(app.io, app.cfg.state_path);
        return http.writeJson(w, 502, "{\"error\":\"agent fork failed\"}");
    }
    if (jsonh.parse(arena, resp.body)) |ap_| {
        var ap = ap_;
        defer ap.deinit();
        if (jsonh.getString(ap.root(), "ip")) |assigned_ip| {
            if (assigned_ip.len > 0) app.state.setSandboxIp(child_id, assigned_ip);
        }
    } else |_| {}
    app.state.setSandboxState(child_id, .running);
    app.state.recordFork();
    try app.state.persist(app.io, app.cfg.state_path);

    app.state.mu.lock();
    defer app.state.mu.unlock();
    const s = app.state.findSandbox(child_id) orelse return http.writeJson(w, 500, "{\"error\":\"lost child\"}");
    var b = jsonh.Builder.init(arena);
    try s.writeJson(&b);
    return http.writeJson(w, 201, b.items());
}

// ---- Metrics ----

fn metrics(app: *App, arena: std.mem.Allocator, w: *Io.Writer) anyerror!void {
    const now = common.nowUnix(app.io);
    app.state.mu.lock();
    defer app.state.mu.unlock();

    var ready: usize = 0;
    for (app.state.nodes.items) |n| {
        if (n.status(now) == .ready) ready += 1;
    }
    var counts = [_]usize{0} ** std.meta.fields(models.SandboxState).len;
    for (app.state.sandboxes.items) |s| {
        counts[@intFromEnum(s.state)] += 1;
    }

    var b = jsonh.Builder.init(arena);
    try b.print("# HELP hearth_nodes_ready Number of ready nodes\n# TYPE hearth_nodes_ready gauge\nhearth_nodes_ready {d}\n", .{ready});
    try b.raw("# HELP hearth_sandboxes_total Sandboxes by state\n# TYPE hearth_sandboxes_total gauge\n");
    inline for (std.meta.fields(models.SandboxState)) |f| {
        try b.print("hearth_sandboxes_total{{state=\"{s}\"}} {d}\n", .{ f.name, counts[f.value] });
    }
    try b.print("# HELP hearth_api_requests_total Total API requests\n# TYPE hearth_api_requests_total counter\nhearth_api_requests_total {d}\n", .{app.state.request_count});

    // Wake / fork metrics (API-V2.md §7).
    try b.print("# HELP hearth_wake_ms_last Last wake latency in ms\n# TYPE hearth_wake_ms_last gauge\nhearth_wake_ms_last {d}\n", .{app.state.wake_ms_last});
    try b.print("# HELP hearth_wake_total Total wakes\n# TYPE hearth_wake_total counter\nhearth_wake_total {d}\n", .{app.state.wake_total});
    try b.print("# HELP hearth_wake_ms_sum Sum of wake latencies (ms)\n# TYPE hearth_wake_ms_sum counter\nhearth_wake_ms_sum {d}\n", .{app.state.wake_ms_sum});
    try b.print("# HELP hearth_forks_total Total forks\n# TYPE hearth_forks_total counter\nhearth_forks_total {d}\n", .{app.state.forks_total});

    // Per-node warm-pool depth, aggregated from agent heartbeats.
    try b.raw("# HELP hearth_pool_size Warm-pool depth per node\n# TYPE hearth_pool_size gauge\n");
    for (app.state.nodes.items) |n| {
        try b.print("hearth_pool_size{{node=\"{s}\"}} {d}\n", .{ n.hostname, n.pool_size });
    }

    return http.writeResponse(w, 200, "OK", "text/plain; version=0.0.4", b.items());
}

// ---- Static file serving ----

fn serveStatic(app: *App, arena: std.mem.Allocator, path: []const u8, w: *Io.Writer) anyerror!void {
    var rel = path;
    if (rel.len == 0 or std.mem.eql(u8, rel, "/")) rel = "/index.html";
    // Strip leading slash and reject traversal.
    if (rel[0] == '/') rel = rel[1..];
    if (std.mem.indexOf(u8, rel, "..") != null) {
        return http.writeJson(w, 404, "{\"error\":\"not found\"}");
    }

    const full = std.fs.path.join(arena, &.{ app.cfg.ui_dir, rel }) catch {
        return http.writeJson(w, 404, "{\"error\":\"not found\"}");
    };

    const data = Io.Dir.cwd().readFileAlloc(app.io, full, arena, .limited(http.max_body_bytes)) catch {
        // Fall back to index.html for SPA routing if file missing.
        const index_full = std.fs.path.join(arena, &.{ app.cfg.ui_dir, "index.html" }) catch {
            return http.writeJson(w, 404, "{\"error\":\"not found\"}");
        };
        const index = Io.Dir.cwd().readFileAlloc(app.io, index_full, arena, .limited(http.max_body_bytes)) catch {
            return http.writeJson(w, 404, "{\"error\":\"ui not found\"}");
        };
        return http.writeResponse(w, 200, "OK", "text/html", index);
    };
    return http.writeResponse(w, 200, "OK", contentType(rel), data);
}

fn contentType(path: []const u8) []const u8 {
    if (std.mem.endsWith(u8, path, ".html")) return "text/html";
    if (std.mem.endsWith(u8, path, ".css")) return "text/css";
    if (std.mem.endsWith(u8, path, ".js")) return "application/javascript";
    if (std.mem.endsWith(u8, path, ".json")) return "application/json";
    if (std.mem.endsWith(u8, path, ".svg")) return "image/svg+xml";
    if (std.mem.endsWith(u8, path, ".png")) return "image/png";
    return "application/octet-stream";
}

// ---- helpers ----

/// Split "host:port" or a URL "http://host:port" into host and port.
fn splitHostPort(addr: []const u8) struct { []const u8, u16 } {
    var a = addr;
    if (std.mem.startsWith(u8, a, "http://")) a = a["http://".len..];
    if (std.mem.startsWith(u8, a, "https://")) a = a["https://".len..];
    // strip any trailing path
    if (std.mem.indexOfScalar(u8, a, '/')) |slash| a = a[0..slash];
    if (std.mem.lastIndexOfScalar(u8, a, ':')) |colon| {
        const host = a[0..colon];
        const port = std.fmt.parseInt(u16, a[colon + 1 ..], 10) catch 9090;
        return .{ host, port };
    }
    return .{ a, 9090 };
}
