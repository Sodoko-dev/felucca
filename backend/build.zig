const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // Shared "common" module.
    const common = b.createModule(.{
        .root_source_file = b.path("src/common/common.zig"),
        .target = target,
        .optimize = optimize,
    });

    // hearthd control plane.
    const hearthd_mod = b.createModule(.{
        .root_source_file = b.path("src/hearthd/main.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "common", .module = common }},
    });
    const hearthd = b.addExecutable(.{
        .name = "hearthd",
        .root_module = hearthd_mod,
    });
    b.installArtifact(hearthd);

    // hearth-agent node agent.
    const agent_mod = b.createModule(.{
        .root_source_file = b.path("src/agent/main.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "common", .module = common }},
    });
    const agent = b.addExecutable(.{
        .name = "hearth-agent",
        .root_module = agent_mod,
    });
    b.installArtifact(agent);

    // Unit tests (run on the host/native target).
    const test_mod = b.createModule(.{
        .root_source_file = b.path("src/common/tests.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "common", .module = common }},
    });
    const unit_tests = b.addTest(.{ .root_module = test_mod });
    const run_unit_tests = b.addRunArtifact(unit_tests);
    const test_step = b.step("test", "Run unit tests");
    test_step.dependOn(&run_unit_tests.step);

    // Agent-side unit tests (IP allocator).
    const ipalloc_mod = b.createModule(.{
        .root_source_file = b.path("src/agent/ipalloc.zig"),
        .target = target,
        .optimize = optimize,
    });
    const ipalloc_tests = b.addTest(.{ .root_module = ipalloc_mod });
    const run_ipalloc_tests = b.addRunArtifact(ipalloc_tests);
    test_step.dependOn(&run_ipalloc_tests.step);

    // Common-module unit tests run directly against the common root so that
    // tests in config.zig / netdetect.zig / models.zig are collected.
    const common_tests = b.addTest(.{ .root_module = common });
    const run_common_tests = b.addRunArtifact(common_tests);
    test_step.dependOn(&run_common_tests.step);
}
