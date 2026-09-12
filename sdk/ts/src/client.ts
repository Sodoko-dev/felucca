// FeluccaClient — thin typed client over feluccad's REST API (docs/API-V2.md).
// Zero runtime dependencies: global fetch (Node >= 18 / browsers), ESM.

import type {
  CreateSandboxRequest,
  ExecRequest,
  ExecResult,
  ExecStreamHandlers,
  ExecStreamResult,
  Expose,
  ExposeRequest,
  ForkSandboxRequest,
  Sandbox,
  Template,
  UsageTotals,
} from "./types.ts";

export type {
  CreateSandboxRequest,
  ExecRequest,
  ExecResult,
  ExecStreamHandlers,
  ExecStreamResult,
  Expose,
  ExposeRequest,
  ForkSandboxRequest,
  Sandbox,
  SandboxState,
  Template,
  UsageTotals,
} from "./types.ts";

/** Every non-2xx response (and broken exec streams) rejects with this. */
export class FeluccaError extends Error {
  /** HTTP status code (200 for in-stream exec failures). */
  readonly status: number;
  /** Raw response body (or stream error text). */
  readonly body: string;
  /**
   * feluccad's X-Felucca-Request-Id for this call (v4 P6) — quote it in bug
   * reports; the operator can grep both feluccad's and the agent's journals
   * for it. Empty when the server predates P6 or the failure was client-side.
   */
  readonly requestId: string;

  constructor(status: number, body: string, requestId = "") {
    super(
      `feluccad request failed: ${status}: ${body}` +
        (requestId ? ` (request_id ${requestId})` : ""),
    );
    this.name = "FeluccaError";
    this.status = status;
    this.body = body;
    this.requestId = requestId;
  }
}

export interface FeluccaClientOptions {
  /** e.g. "http://192.168.104.3:8080" or "https://felucca.example.com". */
  baseUrl: string;
  /** Admin token or tenant key ("felucca_sk_…"); sent as a bearer token. */
  apiKey: string;
}

/** One parsed SSE data frame from a streamed exec. */
type ExecStreamFrame =
  | { stream: "stdout" | "stderr"; data: string }
  | { done: true; ok: true; exit_code: number; truncated: boolean }
  | { done: true; ok: false; error: string };

export class FeluccaClient {
  readonly #baseUrl: string;
  readonly #apiKey: string;

  constructor(opts: FeluccaClientOptions) {
    this.#baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.#apiKey = opts.apiKey;
  }

  // ---- Sandbox lifecycle ----

  /** POST /api/v1/sandboxes */
  async createSandbox(req: CreateSandboxRequest = {}): Promise<Sandbox> {
    return this.#json("POST", "/api/v1/sandboxes", req);
  }

  /** GET /api/v1/sandboxes/{id} */
  async getSandbox(id: string): Promise<Sandbox> {
    return this.#json("GET", `/api/v1/sandboxes/${encodeURIComponent(id)}`);
  }

  /** GET /api/v1/sandboxes */
  async listSandboxes(): Promise<Sandbox[]> {
    const res: { sandboxes: Sandbox[] } = await this.#json(
      "GET",
      "/api/v1/sandboxes",
    );
    return res.sandboxes;
  }

  /** DELETE /api/v1/sandboxes/{id} — resolves void on 204. */
  async deleteSandbox(id: string): Promise<void> {
    await this.#request(
      "DELETE",
      `/api/v1/sandboxes/${encodeURIComponent(id)}`,
    );
  }

  /** POST /api/v1/sandboxes/{id}/sleep */
  async sleepSandbox(id: string): Promise<Sandbox> {
    return this.#json(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/sleep`,
    );
  }

  /** POST /api/v1/sandboxes/{id}/wake — response carries `wake_ms`. */
  async wakeSandbox(id: string): Promise<Sandbox> {
    return this.#json(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/wake`,
    );
  }

  /** POST /api/v1/sandboxes/{id}/fork — 201 child sandbox with parent_id. */
  async forkSandbox(id: string, req: ForkSandboxRequest = {}): Promise<Sandbox> {
    return this.#json(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/fork`,
      req,
    );
  }

  // ---- Exec ----

  /** POST /api/v1/sandboxes/{id}/exec — buffered output. */
  async exec(id: string, req: ExecRequest): Promise<ExecResult> {
    return this.#json(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/exec`,
      req,
    );
  }

  /**
   * POST /api/v1/sandboxes/{id}/exec?stream=1 — SSE (text/event-stream).
   *
   * Each SSE message's `data:` payload is one JSON frame:
   * `{"stream":"stdout"|"stderr","data":string}` chunks, then a terminal
   * `{"done":true,"ok":true,"exit_code":N,"truncated":bool}` or
   * `{"done":true,"ok":false,"error":string}`.
   *
   * Rejects with FeluccaError on non-2xx, on `done.ok === false`, and when
   * the stream ends without a done frame.
   */
  async execStream(
    id: string,
    req: ExecRequest,
    handlers: ExecStreamHandlers = {},
  ): Promise<ExecStreamResult> {
    const res = await this.#request(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/exec?stream=1`,
      req,
    );
    if (!res.body) {
      throw new FeluccaError(res.status, "response has no body", reqIDOf(res));
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });

        // Drain every complete SSE message (terminated by a blank line);
        // partial messages stay buffered until the next read.
        for (;;) {
          const cut = findMessageEnd(buf);
          if (cut === null) break;
          const raw = buf.slice(0, cut.index);
          buf = buf.slice(cut.index + cut.length);
          const frame = parseFrame(raw, res.status, reqIDOf(res));
          if (frame === null) continue; // comment / heartbeat
          if ("done" in frame) {
            if (frame.ok) {
              return {
                exitCode: frame.exit_code,
                truncated: frame.truncated ?? false,
              };
            }
            throw new FeluccaError(res.status, frame.error, reqIDOf(res));
          }
          if (frame.stream === "stdout") handlers.onStdout?.(frame.data);
          else if (frame.stream === "stderr") handlers.onStderr?.(frame.data);
        }
      }
    } finally {
      reader.cancel().catch(() => {});
    }
    throw new FeluccaError(res.status, "stream ended without done frame", reqIDOf(res));
  }

  // ---- Ingress (API-V2 §3d) ----

  /** POST /api/v1/sandboxes/{id}/expose — 201 (or 200 idempotent repeat). */
  async expose(id: string, req: ExposeRequest): Promise<Expose> {
    return this.#json(
      "POST",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/expose`,
      req,
    );
  }

  /** DELETE /api/v1/sandboxes/{id}/expose/{name} — resolves void on 204. */
  async unexpose(id: string, name: string): Promise<void> {
    await this.#request(
      "DELETE",
      `/api/v1/sandboxes/${encodeURIComponent(id)}/expose/${encodeURIComponent(name)}`,
    );
  }

  /**
   * Current exposes of a sandbox. feluccad has no GET on /expose — the
   * sandbox JSON carries the `exposes` array (omitted when empty), so this
   * reads GET /api/v1/sandboxes/{id}.
   */
  async listExposes(id: string): Promise<Expose[]> {
    const sb = await this.getSandbox(id);
    return sb.exposes ?? [];
  }

  // ---- Usage (v4 P5.3) ----

  /**
   * GET /api/v1/tenants/{id}/usage?from&to — billable totals folded from the
   * usage-event stream. Admin keys read any tenant; a tenant key reads only
   * its own id. Defaults: to = now, from = to - 30 days.
   */
  async tenantUsage(
    tenantId: string,
    window: { from?: number; to?: number } = {},
  ): Promise<UsageTotals> {
    const q = new URLSearchParams();
    if (window.from !== undefined) q.set("from", String(window.from));
    if (window.to !== undefined) q.set("to", String(window.to));
    const qs = q.size > 0 ? `?${q}` : "";
    return this.#json(
      "GET",
      `/api/v1/tenants/${encodeURIComponent(tenantId)}/usage${qs}`,
    );
  }

  // ---- Templates (API-V2 §3e) ----

  /** GET /api/v1/templates — the catalog is tenant-visible. */
  async listTemplates(): Promise<Template[]> {
    const res: { templates: Template[] } = await this.#json(
      "GET",
      "/api/v1/templates",
    );
    return res.templates;
  }

  // ---- Internals ----

  async #request(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<Response> {
    const headers: Record<string, string> = {
      authorization: `Bearer ${this.#apiKey}`,
    };
    let payload: string | undefined;
    if (body !== undefined) {
      headers["content-type"] = "application/json";
      payload = JSON.stringify(body);
    }
    const res = await fetch(this.#baseUrl + path, {
      method,
      headers,
      body: payload,
    });
    if (!res.ok) {
      throw new FeluccaError(res.status, await res.text(), reqIDOf(res));
    }
    return res;
  }

  async #json<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.#request(method, path, body);
    return (await res.json()) as T;
  }
}

/** feluccad's per-request id (v4 P6); "" when absent (pre-P6 servers). */
function reqIDOf(res: Response): string {
  return res.headers.get("x-felucca-request-id") ?? "";
}

/**
 * Find the end of the first complete SSE message in `buf`. Messages are
 * terminated by a blank line — "\n\n" (what feluccad emits) or "\r\n\r\n"
 * (also legal SSE). Returns the delimiter position and length, or null when
 * no complete message is buffered yet.
 */
function findMessageEnd(
  buf: string,
): { index: number; length: number } | null {
  const lf = buf.indexOf("\n\n");
  const crlf = buf.indexOf("\r\n\r\n");
  if (lf === -1 && crlf === -1) return null;
  if (crlf !== -1 && (lf === -1 || crlf < lf)) {
    return { index: crlf, length: 4 };
  }
  return { index: lf, length: 2 };
}

/**
 * Parse one SSE message into an exec frame. Concatenates all `data:` lines
 * (per the SSE spec) and JSON-parses the payload. Returns null for messages
 * with no data (comments / keep-alives).
 */
function parseFrame(
  raw: string,
  status: number,
  requestId: string,
): ExecStreamFrame | null {
  const parts: string[] = [];
  for (const line of raw.split(/\r?\n/)) {
    if (line.startsWith("data:")) {
      // The SSE spec strips exactly one leading space after the colon.
      let v = line.slice(5);
      if (v.startsWith(" ")) v = v.slice(1);
      parts.push(v);
    }
  }
  if (parts.length === 0) return null;
  const payload = parts.join("\n");
  try {
    return JSON.parse(payload) as ExecStreamFrame;
  } catch {
    throw new FeluccaError(status, `invalid SSE frame: ${payload}`, requestId);
  }
}
