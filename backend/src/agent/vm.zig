//! Firecracker microVM lifecycle manager for hearth-agent (v2).
//!
//! Per instance we keep /srv/ignis/instances/{id}/ containing:
//!   rootfs.ext4   copy (or reflink) of the base image
//!   fc.sock       firecracker API socket
//!   serial.log    guest console + firecracker API log
//!   meta.json     persisted instance metadata (state, slot, pid, snapshot paths)
//!   vmstate.bin   firecracker memory-state snapshot (when sleeping)
//!   mem.bin       firecracker guest-memory snapshot (when sleeping)
//!
//! v2 adds: tap networking (bridge + per-VM tap + sequential IP + egress NAT),
//! sleep (pause -> snapshot/create -> SIGKILL firecracker), wake (spawn ->
//! snapshot/load with network_overrides), fork (snapshot the parent, reflink its
//! rootfs + copy mem.bin to the child, restore the child on its own tap), and a
//! warm pool of paused generic VMs claimed by matching creates.
const std = @import("std");
const Io = std.Io;
const common = @import("common");
const jsonh = common.jsonh;
const client = common.client;
const ipalloc = @import("ipalloc.zig");
const netmod = @import("net.zig");

pub const VmState = enum { creating, running, paused, sleeping, stopped, pooled, @"error" };

pub const Vm = struct {
    id: []const u8,
    name: []const u8,
    /// On-disk instance directory id (path component). Normally equal to `id`,
    /// but a VM claimed from the warm pool keeps the pool's directory id so the
    /// firecracker drive path baked into snapshots stays valid (renaming the
    /// dir out from under firecracker breaks snapshot/load restore).
    dir_id: []const u8,
    vcpus: u32,
    mem_mib: u64,
    state: VmState,
    pid: ?i32, // firecracker process id while alive
    slot: ?u32, // IP/tap slot index when networking is on
    ip: ?[]const u8, // guest IP (owned by gpa) or null
};

pub const Manager = struct {
    gpa: std.mem.Allocator,
    io: Io,
    data_dir: []const u8,
    net_on: bool,
    cidr: ipalloc.Cidr,
    pool_target: u32,
    mu: common.SpinLock = .{},
    vms: std.ArrayList(Vm) = .empty,
    allocator: ?ipalloc.Allocator = null,
    pool: std.ArrayList([]const u8) = .empty, // ids of parked pool VMs (owned)
    pool_mu: common.SpinLock = .{},

    pub fn init(gpa: std.mem.Allocator, io: Io, data_dir: []const u8, net_on: bool, cidr: ipalloc.Cidr, pool_target: u32) Manager {
        return .{ .gpa = gpa, .io = io, .data_dir = data_dir, .net_on = net_on, .cidr = cidr, .pool_target = pool_target };
    }

    /// Bring up the bridge/NAT (if networking on) and initialize the IP
    /// allocator. Call once before serving.
    pub fn setupHost(self: *Manager) void {
        if (!self.net_on) return;
        self.allocator = ipalloc.Allocator.init(self.gpa, self.cidr) catch |err| blk: {
            std.log.err("ip allocator init failed: {s}", .{@errorName(err)});
            break :blk null;
        };
        if (!netmod.ensureBridge(self.gpa, self.io, self.cidr)) {
            std.log.warn("bridge setup failed; guests may lack networking", .{});
        }
    }

    pub fn liveCount(self: *Manager) u32 {
        self.mu.lock();
        defer self.mu.unlock();
        var n: u32 = 0;
        for (self.vms.items) |v| {
            if (v.state == .running or v.state == .paused) n += 1;
        }
        return n;
    }

    pub fn poolCount(self: *Manager) u32 {
        self.pool_mu.lock();
        defer self.pool_mu.unlock();
        return @intCast(self.pool.items.len);
    }

    fn find(self: *Manager, id: []const u8) ?*Vm {
        for (self.vms.items) |*v| {
            if (std.mem.eql(u8, v.id, id)) return v;
        }
        return null;
    }

    pub fn listJson(self: *Manager, arena: std.mem.Allocator) ![]const u8 {
        self.mu.lock();
        defer self.mu.unlock();
        var b = jsonh.Builder.init(arena);
        try b.raw("{\"vms\":[");
        var first = true;
        for (self.vms.items) |v| {
            if (!first) try b.byte(',');
            first = false;
            try b.byte('{');
            try b.key("id");
            try b.str(v.id);
            try b.raw(",");
            try b.key("name");
            try b.str(v.name);
            try b.raw(",");
            try b.key("state");
            try b.str(@tagName(v.state));
            try b.raw(",");
            try b.key("vcpus");
            try b.print("{d}", .{v.vcpus});
            try b.raw(",");
            try b.key("mem_mib");
            try b.print("{d}", .{v.mem_mib});
            try b.raw(",");
            try b.key("ip");
            try b.optStr(v.ip);
            try b.raw(",");
            try b.key("pid");
            if (v.pid) |p| try b.print("{d}", .{p}) else try b.raw("null");
            try b.byte('}');
        }
        try b.raw("]}");
        return b.items();
    }

    // ---- paths ----

    /// Resolve a VM id to its on-disk directory id (defaults to the id itself).
    fn dirIdOf(self: *Manager, arena: std.mem.Allocator, id: []const u8) []const u8 {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id)) |v| return arena.dupe(u8, v.dir_id) catch id;
        return id;
    }

    fn instanceDir(self: *Manager, arena: std.mem.Allocator, id: []const u8) ![]const u8 {
        const dir_id = self.dirIdOf(arena, id);
        return std.fmt.allocPrint(arena, "{s}/instances/{s}", .{ self.data_dir, dir_id });
    }

    fn sockPath(self: *Manager, arena: std.mem.Allocator, id: []const u8) ![]const u8 {
        const dir_id = self.dirIdOf(arena, id);
        return std.fmt.allocPrint(arena, "{s}/instances/{s}/fc.sock", .{ self.data_dir, dir_id });
    }

    // ---- IP/slot allocation ----

    fn claimSlot(self: *Manager) ?u32 {
        const a = &(self.allocator orelse return null);
        return a.claim();
    }

    fn freeSlot(self: *Manager, idx: u32) void {
        if (self.allocator) |*a| a.free(idx);
    }

    fn ipForSlot(self: *Manager, idx: u32, out: []u8) []const u8 {
        if (self.allocator) |*a| return a.ipFor(idx, out);
        return out[0..0];
    }

    // ---- lifecycle: create ----

    pub fn create(self: *Manager, arena: std.mem.Allocator, id: []const u8, name: []const u8, vcpus: u32, mem_mib: u64) !void {
        // Try to claim a warm-pool VM for a matching shape (1 vCPU / 256 MiB).
        if (self.pool_target > 0 and vcpus == 1 and mem_mib == 256) {
            if (try self.claimFromPool(arena, id, name)) return;
        }
        try self.coldCreate(arena, id, name, vcpus, mem_mib, .running, null);
    }

    /// Cold-boot a fresh VM. `force_slot` reuses a specific slot (pool refill).
    fn coldCreate(self: *Manager, arena: std.mem.Allocator, id: []const u8, name: []const u8, vcpus: u32, mem_mib: u64, final_state: VmState, force_slot: ?u32) !void {
        {
            self.mu.lock();
            defer self.mu.unlock();
            if (self.find(id) != null) return error.AlreadyExists;
            try self.vms.append(self.gpa, .{
                .id = try self.gpa.dupe(u8, id),
                .name = try self.gpa.dupe(u8, name),
                .dir_id = try self.gpa.dupe(u8, id),
                .vcpus = vcpus,
                .mem_mib = mem_mib,
                .state = .creating,
                .pid = null,
                .slot = null,
                .ip = null,
            });
        }
        errdefer self.setState(id, .@"error");

        const dir_path = try self.instanceDir(arena, id);
        const cwd = Io.Dir.cwd();
        cwd.createDirPath(self.io, dir_path) catch |err| switch (err) {
            error.PathAlreadyExists => {},
            else => return err,
        };

        const rootfs_dst = try std.fmt.allocPrint(arena, "{s}/rootfs.ext4", .{dir_path});
        const rootfs_src = try std.fmt.allocPrint(arena, "{s}/images/ubuntu-base.ext4", .{self.data_dir});
        try self.copyRootfs(arena, rootfs_src, rootfs_dst);

        // Networking: claim a slot, derive IP + tap, bring the tap up.
        var ip_str: ?[]const u8 = null;
        var slot: ?u32 = null;
        if (self.net_on) {
            const s = force_slot orelse self.claimSlot() orelse return error.IpPoolExhausted;
            if (force_slot != null) self.reserveSlot(s);
            slot = s;
            var ip_buf: [16]u8 = undefined;
            const ip = self.ipForSlot(s, &ip_buf);
            ip_str = try arena.dupe(u8, ip);
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(s, &tap_buf);
            if (!netmod.ensureTap(self.gpa, self.io, tap)) {
                self.freeSlot(s);
                return error.TapSetupFailed;
            }
        }

        const pid = try self.spawnAndConfigure(arena, id, dir_path, rootfs_dst, vcpus, mem_mib, slot, ip_str);

        // Snapshots are taken AFTER networking is up. For the pool/paused case,
        // pause now so the parked VM holds no CPU.
        if (final_state == .pooled or final_state == .paused) {
            const sock = try self.sockPath(arena, id);
            try self.patchVm(arena, sock, "Paused");
        }

        self.mu.lock();
        if (self.find(id)) |v| {
            v.pid = pid;
            v.slot = slot;
            v.state = final_state;
            if (ip_str) |ip| v.ip = self.gpa.dupe(u8, ip) catch null;
        }
        self.mu.unlock();
        try self.writeMeta(arena, id);
    }

    fn copyRootfs(self: *Manager, arena: std.mem.Allocator, src: []const u8, dst: []const u8) !void {
        // Prefer a reflink (CoW) copy when the filesystem supports it; fall back
        // to a plain copy. `cp --reflink=auto` does exactly this in one call.
        if (self.runCp(arena, src, dst, true)) return;
        const cwd = Io.Dir.cwd();
        cwd.copyFile(src, cwd, dst, self.io, .{}) catch |err| {
            std.log.err("rootfs copy {s} -> {s} failed: {s}", .{ src, dst, @errorName(err) });
            return err;
        };
    }

    /// Run `cp [--reflink=auto] src dst`. Returns true on success.
    fn runCp(self: *Manager, arena: std.mem.Allocator, src: []const u8, dst: []const u8, reflink: bool) bool {
        const argv: []const []const u8 = if (reflink)
            &.{ "cp", "--reflink=auto", src, dst }
        else
            &.{ "cp", src, dst };
        _ = arena;
        const res = std.process.run(self.gpa, self.io, .{ .argv = argv }) catch return false;
        defer self.gpa.free(res.stdout);
        defer self.gpa.free(res.stderr);
        return switch (res.term) {
            .exited => |code| code == 0,
            else => false,
        };
    }

    fn spawnFirecracker(self: *Manager, arena: std.mem.Allocator, id: []const u8, dir_path: []const u8) !struct { pid: i32, sock: []const u8 } {
        const sock = try self.sockPath(arena, id);
        const cwd = Io.Dir.cwd();
        cwd.deleteFile(self.io, sock) catch {};

        const log_path = try std.fmt.allocPrint(arena, "{s}/serial.log", .{dir_path});
        const log_file = try cwd.createFile(self.io, log_path, .{ .truncate = true });

        const child = try std.process.spawn(self.io, .{
            .argv = &.{ "firecracker", "--api-sock", sock },
            .stdin = .ignore,
            .stdout = .{ .file = log_file },
            .stderr = .{ .file = log_file },
        });
        log_file.close(self.io);
        const pid: i32 = if (child.id) |cid| @intCast(cid) else return error.NoPid;

        var waited: u32 = 0;
        while (waited < 3000) : (waited += 50) {
            cwd.access(self.io, sock, .{}) catch {
                self.sleepMs(50);
                continue;
            };
            break;
        }
        cwd.access(self.io, sock, .{}) catch return error.SocketNeverAppeared;
        return .{ .pid = pid, .sock = sock };
    }

    fn spawnAndConfigure(
        self: *Manager,
        arena: std.mem.Allocator,
        id: []const u8,
        dir_path: []const u8,
        rootfs_path: []const u8,
        vcpus: u32,
        mem_mib: u64,
        slot: ?u32,
        ip: ?[]const u8,
    ) !i32 {
        const spawned = try self.spawnFirecracker(arena, id, dir_path);
        try self.configureGuest(arena, spawned.sock, rootfs_path, vcpus, mem_mib, slot, ip);
        return spawned.pid;
    }

    fn configureGuest(
        self: *Manager,
        arena: std.mem.Allocator,
        sock: []const u8,
        rootfs_path: []const u8,
        vcpus: u32,
        mem_mib: u64,
        slot: ?u32,
        ip: ?[]const u8,
    ) !void {
        const kernel = try std.fmt.allocPrint(arena, "{s}/kernels/vmlinux", .{self.data_dir});

        // PUT /boot-source. When networking is on, configure the guest's static
        // IP via the kernel `ip=` boot arg: ip=<ip>::<gw>:<mask>::eth0:off.
        {
            var b = jsonh.Builder.init(arena);
            try b.raw("{\"kernel_image_path\":");
            try b.str(kernel);
            if (self.net_on and ip != null) {
                var gw_buf: [16]u8 = undefined;
                const gw = ipalloc.fmtIp(self.cidr.gateway(), &gw_buf);
                var mask_buf: [16]u8 = undefined;
                const mask = ipalloc.fmtIp(self.cidr.mask(), &mask_buf);
                const boot_args = try std.fmt.allocPrint(
                    arena,
                    "console=ttyS0 reboot=k panic=1 root=/dev/vda rw ip={s}::{s}:{s}::eth0:off",
                    .{ ip.?, gw, mask },
                );
                try b.raw(",\"boot_args\":");
                try b.str(boot_args);
            } else {
                try b.raw(",\"boot_args\":\"console=ttyS0 reboot=k panic=1 root=/dev/vda rw\"");
            }
            try b.byte('}');
            try self.putUds(arena, sock, "/boot-source", b.items());
        }
        // PUT /drives/rootfs
        {
            var b = jsonh.Builder.init(arena);
            try b.raw("{\"drive_id\":\"rootfs\",\"path_on_host\":");
            try b.str(rootfs_path);
            try b.raw(",\"is_root_device\":true,\"is_read_only\":false}");
            try self.putUds(arena, sock, "/drives/rootfs", b.items());
        }
        // PUT /network-interfaces/eth0 (only when networking is on).
        if (self.net_on and slot != null) {
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(slot.?, &tap_buf);
            var b = jsonh.Builder.init(arena);
            try b.raw("{\"iface_id\":\"eth0\",\"host_dev_name\":");
            try b.str(tap);
            try b.byte('}');
            try self.putUds(arena, sock, "/network-interfaces/eth0", b.items());
        }
        // PUT /machine-config
        {
            const body = try std.fmt.allocPrint(arena, "{{\"vcpu_count\":{d},\"mem_size_mib\":{d}}}", .{ vcpus, mem_mib });
            try self.putUds(arena, sock, "/machine-config", body);
        }
        // PUT /actions InstanceStart
        try self.putUds(arena, sock, "/actions", "{\"action_type\":\"InstanceStart\"}");
    }

    fn putUds(self: *Manager, arena: std.mem.Allocator, sock: []const u8, path: []const u8, body: []const u8) !void {
        const resp = client.requestUnix(arena, self.io, sock, "PUT", path, body) catch |err| {
            std.log.err("firecracker PUT {s} failed: {s}", .{ path, @errorName(err) });
            return error.FirecrackerApiFailed;
        };
        if (resp.status >= 300) {
            std.log.err("firecracker PUT {s} -> {d}: {s}", .{ path, resp.status, resp.body });
            return error.FirecrackerApiError;
        }
    }

    pub fn pause(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        const sock = try self.sockPath(arena, id);
        try self.patchVm(arena, sock, "Paused");
        self.setState(id, .paused);
        try self.writeMeta(arena, id);
    }

    pub fn resume_(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        const sock = try self.sockPath(arena, id);
        try self.patchVm(arena, sock, "Resumed");
        self.setState(id, .running);
        try self.writeMeta(arena, id);
    }

    fn patchVm(self: *Manager, arena: std.mem.Allocator, sock: []const u8, state: []const u8) !void {
        const body = try std.fmt.allocPrint(arena, "{{\"state\":\"{s}\"}}", .{state});
        const resp = client.requestUnix(arena, self.io, sock, "PATCH", "/vm", body) catch return error.FirecrackerApiFailed;
        if (resp.status >= 300) return error.FirecrackerApiError;
    }

    // ---- sleep / wake ----

    /// Pause -> snapshot/create (Full) -> SIGKILL firecracker + reap. The
    /// snapshot files (vmstate.bin, mem.bin) live in the instance dir.
    pub fn sleep(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        var pid: ?i32 = null;
        {
            self.mu.lock();
            const v = self.find(id) orelse {
                self.mu.unlock();
                return error.NotFound;
            };
            if (v.state == .sleeping) {
                self.mu.unlock();
                return;
            }
            pid = v.pid;
            self.mu.unlock();
        }

        const sock = try self.sockPath(arena, id);
        const dir_path = try self.instanceDir(arena, id);

        // Pause then snapshot/create (Full).
        try self.patchVm(arena, sock, "Paused");
        try self.snapshotCreate(arena, sock, dir_path);

        // Kill firecracker and reap so RAM is freed.
        if (pid) |p| self.killAndReap(p);

        self.setState(id, .sleeping);
        self.setPid(id, null);
        try self.writeMeta(arena, id);
    }

    fn snapshotCreate(self: *Manager, arena: std.mem.Allocator, sock: []const u8, dir_path: []const u8) !void {
        const vmstate = try std.fmt.allocPrint(arena, "{s}/vmstate.bin", .{dir_path});
        const mem = try std.fmt.allocPrint(arena, "{s}/mem.bin", .{dir_path});
        var b = jsonh.Builder.init(arena);
        try b.raw("{\"snapshot_type\":\"Full\",\"snapshot_path\":");
        try b.str(vmstate);
        try b.raw(",\"mem_file_path\":");
        try b.str(mem);
        try b.byte('}');
        const resp = client.requestUnix(arena, self.io, sock, "PUT", "/snapshot/create", b.items()) catch return error.FirecrackerApiFailed;
        if (resp.status >= 300) {
            std.log.err("snapshot/create -> {d}: {s}", .{ resp.status, resp.body });
            return error.SnapshotFailed;
        }
    }

    /// Spawn firecracker -> snapshot/load (File backend, resume_vm:true). Returns
    /// the measured wake latency in milliseconds.
    pub fn wake(self: *Manager, arena: std.mem.Allocator, id: []const u8) !u64 {
        var slot: ?u32 = null;
        {
            self.mu.lock();
            const v = self.find(id) orelse {
                self.mu.unlock();
                return error.NotFound;
            };
            if (v.state == .running) {
                self.mu.unlock();
                return 0;
            }
            slot = v.slot;
            self.mu.unlock();
        }

        const start_ms = self.nowMs();
        const dir_path = try self.instanceDir(arena, id);

        // Re-assert the tap exists before restore (idempotent).
        if (self.net_on and slot != null) {
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(slot.?, &tap_buf);
            _ = netmod.ensureTap(self.gpa, self.io, tap);
        }

        const spawned = try self.spawnFirecracker(arena, id, dir_path);
        try self.snapshotLoad(arena, spawned.sock, dir_path, slot);
        const elapsed = self.nowMs() - start_ms;

        self.setPid(id, spawned.pid);
        self.setState(id, .running);
        try self.writeMeta(arena, id);
        return elapsed;
    }

    fn snapshotLoad(self: *Manager, arena: std.mem.Allocator, sock: []const u8, dir_path: []const u8, slot: ?u32) !void {
        const vmstate = try std.fmt.allocPrint(arena, "{s}/vmstate.bin", .{dir_path});
        const mem = try std.fmt.allocPrint(arena, "{s}/mem.bin", .{dir_path});
        var b = jsonh.Builder.init(arena);
        try b.raw("{\"snapshot_path\":");
        try b.str(vmstate);
        try b.raw(",\"mem_backend\":{\"backend_path\":");
        try b.str(mem);
        try b.raw(",\"backend_type\":\"File\"}");
        // Re-attach the tap by overriding the host device on restore.
        if (self.net_on and slot != null) {
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(slot.?, &tap_buf);
            try b.raw(",\"network_overrides\":[{\"iface_id\":\"eth0\",\"host_dev_name\":");
            try b.str(tap);
            try b.raw("}]");
        }
        try b.raw(",\"resume_vm\":true}");
        const resp = client.requestUnix(arena, self.io, sock, "PUT", "/snapshot/load", b.items()) catch return error.FirecrackerApiFailed;
        if (resp.status >= 300) {
            std.log.err("snapshot/load -> {d}: {s}", .{ resp.status, resp.body });
            return error.RestoreFailed;
        }
    }

    // ---- fork ----

    /// Fork: ensure the parent has a snapshot (pause+snapshot if running/paused,
    /// reuse files if sleeping; resume the parent afterwards if it was running),
    /// reflink-or-copy the parent rootfs to the child dir, copy mem.bin for the
    /// child, then spawn + snapshot/load with the child's own tap.
    ///
    /// v2 keeps it simple and safe: the child gets its own private copy of the
    /// memory snapshot. (v3 optimization: share the parent's mem.bin read-only
    /// via a copy-on-write mmap to make fork near-instant and zero-copy.)
    pub fn fork(self: *Manager, arena: std.mem.Allocator, parent_id: []const u8, child_id: []const u8, child_name: []const u8) !?[]const u8 {
        var parent_was_running = false;
        var parent_vcpus: u32 = 1;
        var parent_mem: u64 = 256;
        {
            self.mu.lock();
            const p = self.find(parent_id) orelse {
                self.mu.unlock();
                return error.NotFound;
            };
            parent_was_running = p.state == .running;
            parent_vcpus = p.vcpus;
            parent_mem = p.mem_mib;
            self.mu.unlock();
        }

        const parent_dir = try self.instanceDir(arena, parent_id);
        const parent_sock = try self.sockPath(arena, parent_id);

        // 1) Ensure a parent snapshot exists.
        const parent_state = self.stateOf(parent_id) orelse return error.NotFound;
        if (parent_state != .sleeping) {
            // Pause (if running) then snapshot/create.
            if (parent_was_running) try self.patchVm(arena, parent_sock, "Paused");
            try self.snapshotCreate(arena, parent_sock, parent_dir);
            // Resume the parent if it was running before the fork.
            if (parent_was_running) {
                try self.patchVm(arena, parent_sock, "Resumed");
            }
        }

        // 2) Create the child instance dir; reflink rootfs + copy mem.bin.
        const child_dir = try self.instanceDir(arena, child_id);
        const cwd = Io.Dir.cwd();
        cwd.createDirPath(self.io, child_dir) catch |err| switch (err) {
            error.PathAlreadyExists => {},
            else => return err,
        };
        const parent_rootfs = try std.fmt.allocPrint(arena, "{s}/rootfs.ext4", .{parent_dir});
        const child_rootfs = try std.fmt.allocPrint(arena, "{s}/rootfs.ext4", .{child_dir});
        try self.copyRootfs(arena, parent_rootfs, child_rootfs);

        const parent_vmstate = try std.fmt.allocPrint(arena, "{s}/vmstate.bin", .{parent_dir});
        const child_vmstate = try std.fmt.allocPrint(arena, "{s}/vmstate.bin", .{child_dir});
        const parent_mem_file = try std.fmt.allocPrint(arena, "{s}/mem.bin", .{parent_dir});
        const child_mem_file = try std.fmt.allocPrint(arena, "{s}/mem.bin", .{child_dir});
        try self.copyRootfs(arena, parent_vmstate, child_vmstate);
        try self.copyRootfs(arena, parent_mem_file, child_mem_file);

        // 3) Register the child record + claim its own slot/tap.
        {
            self.mu.lock();
            defer self.mu.unlock();
            if (self.find(child_id) != null) return error.AlreadyExists;
            try self.vms.append(self.gpa, .{
                .id = try self.gpa.dupe(u8, child_id),
                .name = try self.gpa.dupe(u8, child_name),
                .dir_id = try self.gpa.dupe(u8, child_id),
                .vcpus = parent_vcpus,
                .mem_mib = parent_mem,
                .state = .creating,
                .pid = null,
                .slot = null,
                .ip = null,
            });
        }
        errdefer self.setState(child_id, .@"error");

        var child_ip: ?[]const u8 = null;
        var child_slot: ?u32 = null;
        if (self.net_on) {
            const s = self.claimSlot() orelse return error.IpPoolExhausted;
            child_slot = s;
            var ip_buf: [16]u8 = undefined;
            const ip = self.ipForSlot(s, &ip_buf);
            child_ip = try arena.dupe(u8, ip);
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(s, &tap_buf);
            if (!netmod.ensureTap(self.gpa, self.io, tap)) {
                self.freeSlot(s);
                return error.TapSetupFailed;
            }
        }

        // 4) Spawn the child firecracker and restore from the child snapshot.
        //    The child inherits the parent's guest-internal IP (memory state);
        //    duplicate-IP isolation is documented and fixed in v3.
        const spawned = try self.spawnFirecracker(arena, child_id, child_dir);
        try self.snapshotLoad(arena, spawned.sock, child_dir, child_slot);

        self.mu.lock();
        if (self.find(child_id)) |v| {
            v.pid = spawned.pid;
            v.slot = child_slot;
            v.state = .running;
            if (child_ip) |ip| v.ip = self.gpa.dupe(u8, ip) catch null;
        }
        self.mu.unlock();
        try self.writeMeta(arena, child_id);
        return child_ip;
    }

    // ---- warm pool ----

    /// Pre-boot the pool to `pool_target` paused VMs. Called at startup and by
    /// the async refill thread.
    pub fn refillPool(self: *Manager, arena: std.mem.Allocator) void {
        if (self.pool_target == 0) return;
        while (self.poolCount() < self.pool_target) {
            const id = self.nextPoolId(arena) catch return;
            self.coldCreate(arena, id, "pool", 1, 256, .pooled, null) catch |err| {
                std.log.warn("pool prewarm {s} failed: {s}", .{ id, @errorName(err) });
                return;
            };
            self.pool_mu.lock();
            self.pool.append(self.gpa, self.gpa.dupe(u8, id) catch {
                self.pool_mu.unlock();
                return;
            }) catch {};
            self.pool_mu.unlock();
        }
    }

    fn nextPoolId(self: *Manager, arena: std.mem.Allocator) ![]const u8 {
        const ts = self.nowMs();
        return std.fmt.allocPrint(arena, "pool-{x}", .{ts});
    }

    /// Claim a parked pool VM and rename it to the requested id (bookkeeping +
    /// resume). Returns true if a VM was claimed. Refill happens asynchronously.
    fn claimFromPool(self: *Manager, arena: std.mem.Allocator, id: []const u8, name: []const u8) !bool {
        var pooled_id: ?[]const u8 = null;
        {
            self.pool_mu.lock();
            defer self.pool_mu.unlock();
            if (self.pool.items.len == 0) return false;
            pooled_id = self.pool.orderedRemove(0);
        }
        const pid_owned = pooled_id.?;
        defer self.gpa.free(pid_owned);

        // Retag the in-memory record to the real id/name but KEEP its on-disk
        // dir_id (the pool dir). Renaming the directory would invalidate the
        // drive path baked into the firecracker device state, breaking later
        // snapshot/load restores. The pool dir stays put; only bookkeeping
        // changes (contract §4: "resume + rename bookkeeping is fine").
        {
            self.mu.lock();
            if (self.find(pid_owned)) |v| {
                self.gpa.free(v.id);
                self.gpa.free(v.name);
                v.id = self.gpa.dupe(u8, id) catch id;
                v.name = self.gpa.dupe(u8, name) catch name;
                // dir_id already points at the pool dir; leave it.
            } else {
                self.mu.unlock();
                return false;
            }
            self.mu.unlock();
        }
        const sock = try self.sockPath(arena, id);
        try self.patchVm(arena, sock, "Resumed");
        self.setState(id, .running);
        try self.writeMeta(arena, id);
        return true;
    }

    // ---- stop / start / delete ----

    pub fn stop(self: *Manager, id: []const u8) !void {
        self.mu.lock();
        const v = self.find(id) orelse {
            self.mu.unlock();
            return error.NotFound;
        };
        const pid = v.pid;
        self.mu.unlock();

        if (pid) |p| {
            std.posix.kill(p, .TERM) catch |err| switch (err) {
                error.ProcessNotFound => {},
                else => return err,
            };
        }
        self.setState(id, .stopped);
        self.setPid(id, null);
    }

    pub fn start(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        var vcpus: u32 = 1;
        var mem_mib: u64 = 256;
        var slot: ?u32 = null;
        var ip: ?[]const u8 = null;
        {
            self.mu.lock();
            const v = self.find(id) orelse {
                self.mu.unlock();
                return error.NotFound;
            };
            if (v.state == .running) {
                self.mu.unlock();
                return;
            }
            vcpus = v.vcpus;
            mem_mib = v.mem_mib;
            slot = v.slot;
            if (v.ip) |x| ip = arena.dupe(u8, x) catch null;
            self.mu.unlock();
        }

        const dir_path = try self.instanceDir(arena, id);
        const rootfs_dst = try std.fmt.allocPrint(arena, "{s}/rootfs.ext4", .{dir_path});
        if (self.net_on and slot != null) {
            var tap_buf: [16]u8 = undefined;
            const tap = netmod.tapName(slot.?, &tap_buf);
            _ = netmod.ensureTap(self.gpa, self.io, tap);
        }
        const pid = try self.spawnAndConfigure(arena, id, dir_path, rootfs_dst, vcpus, mem_mib, slot, ip);
        self.setPid(id, pid);
        self.setState(id, .running);
        try self.writeMeta(arena, id);
    }

    pub fn delete(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        // Capture slot before removal so we can free the IP + tap.
        var slot: ?u32 = null;
        {
            self.mu.lock();
            if (self.find(id)) |v| slot = v.slot;
            self.mu.unlock();
        }
        self.stop(id) catch {};

        if (self.net_on) {
            if (slot) |s| {
                var tap_buf: [16]u8 = undefined;
                const tap = netmod.tapName(s, &tap_buf);
                netmod.deleteTap(self.gpa, self.io, tap);
                self.freeSlot(s);
            }
        }

        const dir_path = try self.instanceDir(arena, id);
        Io.Dir.cwd().deleteTree(self.io, dir_path) catch |err| {
            std.log.warn("deleteTree {s}: {s}", .{ dir_path, @errorName(err) });
        };

        self.mu.lock();
        defer self.mu.unlock();
        var i: usize = 0;
        while (i < self.vms.items.len) : (i += 1) {
            if (std.mem.eql(u8, self.vms.items[i].id, id)) {
                const v = self.vms.orderedRemove(i);
                self.gpa.free(v.id);
                self.gpa.free(v.name);
                self.gpa.free(v.dir_id);
                if (v.ip) |x| self.gpa.free(x);
                return;
            }
        }
    }

    // ---- reconciliation on startup ----

    /// Scan the instances dir and rebuild in-memory records from each meta.json.
    /// A "running" instance whose pid is no longer alive is downgraded based on
    /// what's on disk: if a snapshot exists, it is `sleeping`; else `stopped`.
    pub fn reconcile(self: *Manager, arena: std.mem.Allocator) void {
        const dir_path = std.fmt.allocPrint(arena, "{s}/instances", .{self.data_dir}) catch return;
        var dir = Io.Dir.cwd().openDir(self.io, dir_path, .{ .iterate = true }) catch return;
        defer dir.close(self.io);

        var it = dir.iterate();
        while (it.next(self.io) catch null) |entry| {
            if (entry.kind != .directory) continue;
            self.reconcileOne(arena, entry.name) catch |err| {
                std.log.warn("reconcile {s}: {s}", .{ entry.name, @errorName(err) });
            };
        }
    }

    fn reconcileOne(self: *Manager, arena: std.mem.Allocator, name: []const u8) !void {
        const dir_path = try self.instanceDir(arena, name);
        const meta_path = try std.fmt.allocPrint(arena, "{s}/meta.json", .{dir_path});
        const data = Io.Dir.cwd().readFileAlloc(self.io, meta_path, arena, .limited(64 * 1024)) catch return;

        var parsed = jsonh.parse(arena, data) catch return;
        defer parsed.deinit();
        const root = parsed.root();

        const id = jsonh.getString(root, "id") orelse name;
        const vm_name = jsonh.getString(root, "name") orelse id;
        // The on-disk dir name (`name`) is authoritative for dir_id.
        const dir_id = jsonh.getString(root, "dir_id") orelse name;
        const vcpus: u32 = @intCast(jsonh.getInt(root, "vcpus") orelse 1);
        const mem_mib: u64 = @intCast(jsonh.getInt(root, "mem_mib") orelse 256);
        const state_str = jsonh.getString(root, "state") orelse "stopped";
        const slot: ?u32 = if (jsonh.getInt(root, "slot")) |v| @intCast(v) else null;
        const meta_pid: ?i32 = if (jsonh.getInt(root, "pid")) |v| @intCast(v) else null;
        const ip = jsonh.getString(root, "ip");

        var state = stateFromString(state_str);
        var pid: ?i32 = null;

        // Pool VMs from a previous run are not reclaimed; treat them as stopped
        // orphans (a fresh pool is prewarmed on startup).
        if (state == .pooled) state = .stopped;

        // Is the recorded pid still a live firecracker? If yes, keep running.
        if ((state == .running or state == .paused) and meta_pid != null and self.pidAlive(meta_pid.?)) {
            pid = meta_pid;
        } else if (state == .running or state == .paused) {
            // Process gone. If a snapshot is on disk, the VM is recoverable as
            // sleeping; otherwise it is stopped.
            const vmstate = std.fmt.allocPrint(arena, "{s}/vmstate.bin", .{dir_path}) catch return;
            if (Io.Dir.cwd().access(self.io, vmstate, .{})) |_| {
                state = .sleeping;
            } else |_| {
                state = .stopped;
            }
        }

        // Reserve the slot so it is not handed to a new VM.
        if (self.net_on and slot != null) self.reserveSlot(slot.?);

        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id) != null) return;
        self.vms.append(self.gpa, .{
            .id = try self.gpa.dupe(u8, id),
            .name = try self.gpa.dupe(u8, vm_name),
            .dir_id = try self.gpa.dupe(u8, dir_id),
            .vcpus = vcpus,
            .mem_mib = mem_mib,
            .state = state,
            .pid = pid,
            .slot = slot,
            .ip = if (ip) |x| (self.gpa.dupe(u8, x) catch null) else null,
        }) catch {};
    }

    fn reserveSlot(self: *Manager, idx: u32) void {
        if (self.allocator) |*a| a.reserve(idx);
    }

    fn pidAlive(self: *Manager, pid: i32) bool {
        _ = self;
        // signal 0 probes existence without delivering a signal.
        std.posix.kill(pid, @enumFromInt(0)) catch return false;
        return true;
    }

    // ---- helpers ----

    fn stateOf(self: *Manager, id: []const u8) ?VmState {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id)) |v| return v.state;
        return null;
    }

    pub fn ipOf(self: *Manager, arena: std.mem.Allocator, id: []const u8) ?[]const u8 {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id)) |v| {
            if (v.ip) |x| return arena.dupe(u8, x) catch null;
        }
        return null;
    }

    fn killAndReap(self: *Manager, pid: i32) void {
        std.posix.kill(pid, .KILL) catch {};
        // Reap the zombie if it is our child. Best effort (raw waitpid syscall).
        var status: u32 = 0;
        _ = std.os.linux.waitpid(pid, &status, 0);
        _ = self;
    }

    fn setState(self: *Manager, id: []const u8, st: VmState) void {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id)) |v| v.state = st;
    }

    fn setPid(self: *Manager, id: []const u8, pid: ?i32) void {
        self.mu.lock();
        defer self.mu.unlock();
        if (self.find(id)) |v| v.pid = pid;
    }

    fn sleepMs(self: *Manager, ms: u64) void {
        Io.sleep(self.io, .{ .nanoseconds = @intCast(ms * std.time.ns_per_ms) }, .awake) catch {};
    }

    fn nowMs(self: *Manager) u64 {
        const ts = Io.Timestamp.now(self.io, .awake);
        return @intCast(@divTrunc(ts.nanoseconds, std.time.ns_per_ms));
    }

    /// Write the current in-memory record for `id` to its meta.json.
    fn writeMeta(self: *Manager, arena: std.mem.Allocator, id: []const u8) !void {
        var vcpus: u32 = 1;
        var mem_mib: u64 = 256;
        var pid: ?i32 = null;
        var slot: ?u32 = null;
        var state: VmState = .stopped;
        var name: []const u8 = id;
        var dir_id: []const u8 = id;
        var ip: ?[]const u8 = null;
        {
            self.mu.lock();
            defer self.mu.unlock();
            const v = self.find(id) orelse return;
            vcpus = v.vcpus;
            mem_mib = v.mem_mib;
            pid = v.pid;
            slot = v.slot;
            state = v.state;
            name = arena.dupe(u8, v.name) catch v.name;
            dir_id = arena.dupe(u8, v.dir_id) catch v.dir_id;
            if (v.ip) |x| ip = arena.dupe(u8, x) catch null;
        }

        var b = jsonh.Builder.init(arena);
        try b.byte('{');
        try b.key("id");
        try b.str(id);
        try b.raw(",");
        try b.key("name");
        try b.str(name);
        try b.raw(",");
        try b.key("dir_id");
        try b.str(dir_id);
        try b.raw(",");
        try b.key("vcpus");
        try b.print("{d}", .{vcpus});
        try b.raw(",");
        try b.key("mem_mib");
        try b.print("{d}", .{mem_mib});
        try b.raw(",");
        try b.key("pid");
        if (pid) |p| try b.print("{d}", .{p}) else try b.raw("null");
        try b.raw(",");
        try b.key("slot");
        if (slot) |s| try b.print("{d}", .{s}) else try b.raw("null");
        try b.raw(",");
        try b.key("ip");
        try b.optStr(ip);
        try b.raw(",");
        try b.key("state");
        try b.str(@tagName(state));
        try b.byte('}');

        const dir_path = try self.instanceDir(arena, id);
        const meta_path = try std.fmt.allocPrint(arena, "{s}/meta.json", .{dir_path});
        Io.Dir.cwd().writeFile(self.io, .{ .sub_path = meta_path, .data = b.items() }) catch |err| {
            std.log.warn("write meta {s}: {s}", .{ meta_path, @errorName(err) });
        };
    }
};

fn stateFromString(s: []const u8) VmState {
    inline for (std.meta.fields(VmState)) |f| {
        if (std.mem.eql(u8, s, f.name)) return @enumFromInt(f.value);
    }
    return .stopped;
}
