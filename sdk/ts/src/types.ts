// Wire types for the hearthd REST API (docs/API-V2.md + conformance goldens).
// Field names mirror the wire exactly (snake_case) — no renaming.

/** Sandbox lifecycle states (API-V2 §1). */
export type SandboxState =
  | "creating"
  | "running"
  | "paused"
  | "stopped"
  | "sleeping"
  | "error";

/**
 * One published service port on a sandbox (API-V2 §3d).
 *
 * `hostname` and `url` are present in expose responses
 * (POST /api/v1/sandboxes/{id}/expose); the `exposes` array embedded in
 * sandbox JSON carries only `name`/`guest_port`/`node_port`. `url` is only
 * rendered when hearthd is configured with an `ingress_domain`.
 */
export interface Expose {
  name: string;
  guest_port: number;
  node_port: number;
  hostname?: string;
  url?: string;
}

/**
 * Sandbox JSON as returned by every sandbox endpoint
 * (conformance golden hearthd/sandbox-create).
 */
export interface Sandbox {
  id: string;
  name: string;
  namespace: string;
  node_id: string | null;
  state: SandboxState;
  vcpus: number;
  mem_mib: number;
  /** Guest IP when networking is on; null with net off. */
  ip: string | null;
  /** Unix seconds. */
  created_at: number;
  parent_id: string | null;
  /** Omitted by hearthd when empty (frozen pre-P3 wire shape). */
  exposes?: Expose[];
  /** Omitted when false. */
  allow_dynamic_ports?: boolean;
  /** Omitted when the sandbox was not created from a template. */
  template?: string;
  /** Omitted when unset (unresized base image). */
  disk_gb?: number;
  /** Only present on wake responses (POST .../wake). */
  wake_ms?: number;
}

/** Template catalog row (GET /api/v1/templates, API-V2 §3e). */
export interface Template {
  id: string;
  name: string;
  image: string;
  image_sha256: string;
  vcpus: number;
  mem_mib: number;
  /** Default sandbox disk; always >= image_size_gb. */
  disk_gb: number;
  /** Image file size rounded up to whole GiB — the per-sandbox disk floor. */
  image_size_gb: number;
  pool_size: number;
  /** Unix seconds. */
  created_at: number;
}

/** Body for POST /api/v1/sandboxes. */
export interface CreateSandboxRequest {
  name?: string;
  vcpus?: number;
  mem_mib?: number;
  disk_gb?: number;
  template?: string;
  allow_dynamic_ports?: boolean;
  /**
   * Lifecycle overrides (v4 P5.2), seconds: 0 (or absent) inherits the
   * tenant default, -1 disables the policy for this sandbox.
   */
  idle_sleep_s?: number;
  asleep_delete_s?: number;
}

/** GET /api/v1/tenants/{id}/usage response (v4 P5.3). */
export interface UsageTotals {
  tenant_id: string;
  /** Window bounds, unix seconds. */
  from: number;
  to: number;
  sandbox_hours: number;
  vcpu_hours: number;
  mem_gib_hours: number;
  disk_gb_hours: number;
  /** Exec attempts in the window. */
  execs: number;
  /** All usage events in the window. */
  events: number;
}

/** Body for POST /api/v1/sandboxes/{id}/fork. */
export interface ForkSandboxRequest {
  name?: string;
}

/** Body for POST /api/v1/sandboxes/{id}/exec (buffered and streamed). */
export interface ExecRequest {
  cmd: string[];
  timeout_ms?: number;
}

/** Buffered exec result (conformance golden hearthd/exec). */
export interface ExecResult {
  ok: boolean;
  exit_code: number;
  stdout: string;
  stderr: string;
  truncated: boolean;
}

/** Callbacks for streamed exec output chunks. */
export interface ExecStreamHandlers {
  onStdout?: (chunk: string) => void;
  onStderr?: (chunk: string) => void;
}

/** Resolution value of a successful streamed exec. */
export interface ExecStreamResult {
  exitCode: number;
  truncated: boolean;
}

/** Body for POST /api/v1/sandboxes/{id}/expose. */
export interface ExposeRequest {
  /** Guest port to publish. */
  port: number;
  /** Label: [a-z0-9-], 1..=32, no edge/double dash, not all digits. */
  name: string;
}
