//! In-memory control-plane state with a mutex and JSON-file persistence.
//! All string fields are owned by `gpa` (duplicated on insert).
const std = @import("std");
const Io = std.Io;
const common = @import("common");
const models = common.models;
const jsonh = common.jsonh;

pub const State = struct {
    gpa: std.mem.Allocator,
    mu: common.SpinLock = .{},
    nodes: std.ArrayList(models.Node) = .empty,
    sandboxes: std.ArrayList(models.Sandbox) = .empty,
    request_count: u64 = 0,
    seq: u64 = 0,
    // Wake/fork metrics (API-V2.md §7).
    wake_ms_last: u64 = 0,
    wake_total: u64 = 0,
    wake_ms_sum: u64 = 0,
    forks_total: u64 = 0,
    prng: std.Random.DefaultPrng = std.Random.DefaultPrng.init(0),

    pub fn init(gpa: std.mem.Allocator) State {
        var s = State{ .gpa = gpa };
        // Seed from the address of a stack variable for per-run uniqueness.
        var anchor: u8 = 0;
        s.prng = std.Random.DefaultPrng.init(@intFromPtr(&anchor));
        return s;
    }

    pub fn bumpRequests(self: *State) void {
        self.mu.lock();
        defer self.mu.unlock();
        self.request_count += 1;
    }

    // ---- ids ----
    fn nextId(self: *State, prefix: []const u8) ![]const u8 {
        self.seq += 1;
        // Ids need only be unique, not cryptographically random.
        const r = self.prng.random().int(u32);
        return std.fmt.allocPrint(self.gpa, "{s}-{x:0>8}-{d}", .{ prefix, r, self.seq });
    }

    // ---- nodes ----

    pub fn findNode(self: *State, id: []const u8) ?*models.Node {
        for (self.nodes.items) |*n| {
            if (std.mem.eql(u8, n.id, id)) return n;
        }
        return null;
    }

    fn findNodeByHostname(self: *State, hostname: []const u8) ?*models.Node {
        for (self.nodes.items) |*n| {
            if (std.mem.eql(u8, n.hostname, hostname)) return n;
        }
        return null;
    }

    /// Register (idempotent by hostname). Returns the node id (owned by state).
    pub fn registerNode(self: *State, hostname: []const u8, addr: []const u8, cpus: u32, mem_total: u64, now: i64) ![]const u8 {
        self.mu.lock();
        defer self.mu.unlock();

        if (self.findNodeByHostname(hostname)) |n| {
            // Update mutable fields, keep id.
            self.gpa.free(n.addr);
            n.addr = try self.gpa.dupe(u8, addr);
            n.cpus = cpus;
            n.mem_total_mib = mem_total;
            n.last_heartbeat = now;
            return n.id;
        }
        const id = try self.nextId("node");
        const node = models.Node{
            .id = id,
            .hostname = try self.gpa.dupe(u8, hostname),
            .addr = try self.gpa.dupe(u8, addr),
            .cpus = cpus,
            .mem_total_mib = mem_total,
            .mem_free_mib = mem_total,
            .vm_count = 0,
            .last_heartbeat = now,
        };
        try self.nodes.append(self.gpa, node);
        return id;
    }

    pub fn heartbeat(self: *State, id: []const u8, mem_free: u64, vm_count: u32, pool_size: u32, now: i64) bool {
        self.mu.lock();
        defer self.mu.unlock();
        const n = self.findNode(id) orelse return false;
        n.mem_free_mib = mem_free;
        n.vm_count = vm_count;
        n.pool_size = pool_size;
        n.last_heartbeat = now;
        return true;
    }

    /// Pick the ready node with the lowest vm_count. Caller must hold the lock.
    pub fn pickNode(self: *State, now: i64) ?*models.Node {
        var best: ?*models.Node = null;
        for (self.nodes.items) |*n| {
            if (n.status(now) != .ready) continue;
            if (best == null or n.vm_count < best.?.vm_count) best = n;
        }
        return best;
    }

    // ---- sandboxes ----

    pub fn findSandbox(self: *State, id: []const u8) ?*models.Sandbox {
        for (self.sandboxes.items) |*s| {
            if (std.mem.eql(u8, s.id, id)) return s;
        }
        return null;
    }

    /// Create a sandbox record in "creating" state. Caller must hold the lock.
    pub fn createSandbox(self: *State, name: []const u8, namespace: []const u8, node_id: []const u8, vcpus: u32, mem_mib: u64, now: i64) !*models.Sandbox {
        const id = try self.nextId("sb");
        const sb = models.Sandbox{
            .id = id,
            .name = try self.gpa.dupe(u8, name),
            .namespace = try self.gpa.dupe(u8, namespace),
            .node_id = try self.gpa.dupe(u8, node_id),
            .state = .creating,
            .vcpus = vcpus,
            .mem_mib = mem_mib,
            .ip = null,
            .created_at = now,
            .parent_id = null,
        };
        try self.sandboxes.append(self.gpa, sb);
        return &self.sandboxes.items[self.sandboxes.items.len - 1];
    }

    pub fn setSandboxState(self: *State, id: []const u8, st: models.SandboxState) void {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.findSandbox(id)) |s| s.state = st;
    }

    /// Update the `ip` field of a sandbox (duping the new value). A null clears.
    pub fn setSandboxIp(self: *State, id: []const u8, ip: ?[]const u8) void {
        self.mu.lock();
        defer self.mu.unlock();
        const s = self.findSandbox(id) orelse return;
        if (s.ip) |old| self.gpa.free(old);
        s.ip = if (ip) |v| (self.gpa.dupe(u8, v) catch null) else null;
    }

    pub fn recordWake(self: *State, ms: u64) void {
        self.mu.lock();
        defer self.mu.unlock();
        self.wake_ms_last = ms;
        self.wake_total += 1;
        self.wake_ms_sum += ms;
    }

    pub fn recordFork(self: *State) void {
        self.mu.lock();
        defer self.mu.unlock();
        self.forks_total += 1;
    }

    /// Create a child sandbox record (fork). Caller must hold the lock.
    pub fn createForkChild(
        self: *State,
        name: []const u8,
        namespace: []const u8,
        node_id: []const u8,
        vcpus: u32,
        mem_mib: u64,
        parent_id: []const u8,
        now: i64,
    ) !*models.Sandbox {
        const id = try self.nextId("sb");
        const sb = models.Sandbox{
            .id = id,
            .name = try self.gpa.dupe(u8, name),
            .namespace = try self.gpa.dupe(u8, namespace),
            .node_id = try self.gpa.dupe(u8, node_id),
            .state = .creating,
            .vcpus = vcpus,
            .mem_mib = mem_mib,
            .ip = null,
            .created_at = now,
            .parent_id = try self.gpa.dupe(u8, parent_id),
        };
        try self.sandboxes.append(self.gpa, sb);
        return &self.sandboxes.items[self.sandboxes.items.len - 1];
    }

    pub fn removeSandbox(self: *State, id: []const u8) void {
        self.mu.lock();
        defer self.mu.unlock();
        var i: usize = 0;
        while (i < self.sandboxes.items.len) : (i += 1) {
            if (std.mem.eql(u8, self.sandboxes.items[i].id, id)) {
                const s = self.sandboxes.orderedRemove(i);
                self.freeSandbox(s);
                return;
            }
        }
    }

    fn freeSandbox(self: *State, s: models.Sandbox) void {
        self.gpa.free(s.id);
        self.gpa.free(s.name);
        self.gpa.free(s.namespace);
        if (s.node_id) |v| self.gpa.free(v);
        if (s.ip) |v| self.gpa.free(v);
        if (s.parent_id) |v| self.gpa.free(v);
    }

    // ---- persistence ----

    /// Serialize the full state to JSON and atomically write it (tmp + rename).
    pub fn persist(self: *State, io: Io, path: []const u8) !void {
        self.mu.lock();
        defer self.mu.unlock();

        var arena_state = std.heap.ArenaAllocator.init(self.gpa);
        defer arena_state.deinit();
        const arena = arena_state.allocator();

        var b = jsonh.Builder.init(arena);
        try b.raw("{\"seq\":");
        try b.print("{d}", .{self.seq});
        try b.raw(",\"request_count\":");
        try b.print("{d}", .{self.request_count});
        try b.raw(",\"nodes\":[");
        var first = true;
        for (self.nodes.items) |n| {
            if (!first) try b.byte(',');
            first = false;
            try n.writeJson(&b, n.last_heartbeat); // status recomputed on load
        }
        try b.raw("],\"sandboxes\":[");
        first = true;
        for (self.sandboxes.items) |s| {
            if (!first) try b.byte(',');
            first = false;
            try s.writeJson(&b);
        }
        try b.raw("]}");

        const tmp = try std.fmt.allocPrint(arena, "{s}.tmp", .{path});
        const dir = Io.Dir.cwd();
        try dir.writeFile(io, .{ .sub_path = tmp, .data = b.items() });
        try dir.rename(tmp, dir, path, io);
    }

    /// Load state from a JSON file (best effort). Strings duped into gpa.
    pub fn load(self: *State, io: Io, path: []const u8) !void {
        self.mu.lock();
        defer self.mu.unlock();

        const dir = Io.Dir.cwd();
        const data = try dir.readFileAlloc(io, path, self.gpa, .limited(16 * 1024 * 1024));
        defer self.gpa.free(data);

        var parsed = try jsonh.parse(self.gpa, data);
        defer parsed.deinit();
        const root = parsed.root();

        if (jsonh.getInt(root, "seq")) |v| self.seq = @intCast(v);
        if (jsonh.getInt(root, "request_count")) |v| self.request_count = @intCast(@max(0, v));

        if (jsonh.get(root, "nodes")) |nodes_v| {
            if (nodes_v == .array) {
                for (nodes_v.array.items) |nv| {
                    const n = models.Node{
                        .id = try self.gpa.dupe(u8, jsonh.getString(nv, "id") orelse continue),
                        .hostname = try self.gpa.dupe(u8, jsonh.getString(nv, "hostname") orelse ""),
                        .addr = try self.gpa.dupe(u8, jsonh.getString(nv, "addr") orelse ""),
                        .cpus = @intCast(jsonh.getInt(nv, "cpus") orelse 0),
                        .mem_total_mib = @intCast(jsonh.getInt(nv, "mem_total_mib") orelse 0),
                        .mem_free_mib = @intCast(jsonh.getInt(nv, "mem_free_mib") orelse 0),
                        .vm_count = @intCast(jsonh.getInt(nv, "vm_count") orelse 0),
                        .pool_size = @intCast(jsonh.getInt(nv, "pool_size") orelse 0),
                        .last_heartbeat = jsonh.getInt(nv, "last_heartbeat") orelse 0,
                    };
                    try self.nodes.append(self.gpa, n);
                }
            }
        }
        if (jsonh.get(root, "sandboxes")) |sv| {
            if (sv == .array) {
                for (sv.array.items) |s| {
                    const state_str = jsonh.getString(s, "state") orelse "stopped";
                    const sb = models.Sandbox{
                        .id = try self.gpa.dupe(u8, jsonh.getString(s, "id") orelse continue),
                        .name = try self.gpa.dupe(u8, jsonh.getString(s, "name") orelse ""),
                        .namespace = try self.gpa.dupe(u8, jsonh.getString(s, "namespace") orelse "default"),
                        .node_id = if (jsonh.getString(s, "node_id")) |v| try self.gpa.dupe(u8, v) else null,
                        .state = models.SandboxState.fromString(state_str) orelse .stopped,
                        .vcpus = @intCast(jsonh.getInt(s, "vcpus") orelse 1),
                        .mem_mib = @intCast(jsonh.getInt(s, "mem_mib") orelse 256),
                        .ip = if (jsonh.getString(s, "ip")) |v| try self.gpa.dupe(u8, v) else null,
                        .created_at = jsonh.getInt(s, "created_at") orelse 0,
                        .parent_id = if (jsonh.getString(s, "parent_id")) |v| try self.gpa.dupe(u8, v) else null,
                    };
                    try self.sandboxes.append(self.gpa, sb);
                }
            }
        }
    }
};
