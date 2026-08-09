// node:test suite against a tiny in-process http server faking hearthd.
// Run: node --test test/client.test.ts
// (Node >= 23.6 runs .ts via type stripping; on 22.6+ add
//  --experimental-strip-types.)
//
// This file is intentionally outside the tsc program (tsconfig includes
// src/ only) so the package keeps a single devDependency (typescript) —
// node strips the types at run time.

import { test } from "node:test";
// (request-id capture test appended at the bottom of this file — v4 P6)
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { IncomingMessage, ServerResponse } from "node:http";

import { HearthClient, HearthError } from "../src/client.ts";

const API_KEY = "hearth_sk_test";

type Handler = (
  req: IncomingMessage,
  res: ServerResponse,
  body: string,
) => void;

/** Start a one-test fake hearthd; returns its base URL and a closer. */
function fakeHearthd(
  handler: Handler,
): Promise<{ baseUrl: string; close: () => Promise<void> }> {
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.setEncoding("utf8");
      req.on("data", (c: string) => (body += c));
      req.on("end", () => handler(req, res, body));
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (addr === null || typeof addr === "string") {
        throw new Error("unexpected server address");
      }
      resolve({
        baseUrl: `http://127.0.0.1:${addr.port}`,
        close: () =>
          new Promise<void>((done) => {
            server.close(() => done());
          }),
      });
    });
  });
}

function json(res: ServerResponse, status: number, payload: unknown): void {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(payload));
}

const SANDBOX = {
  id: "sb-0123456789ab",
  name: "demo",
  namespace: "default",
  node_id: "node-1",
  state: "running",
  vcpus: 1,
  mem_mib: 256,
  ip: "10.231.0.12",
  created_at: 1765000000,
  parent_id: null,
};

test("createSandbox: POST /api/v1/sandboxes with bearer auth -> Sandbox", async () => {
  let seen: { method?: string; url?: string; auth?: string; body?: string } = {};
  const { baseUrl, close } = await fakeHearthd((req, res, body) => {
    seen = {
      method: req.method,
      url: req.url,
      auth: req.headers.authorization,
      body,
    };
    json(res, 201, SANDBOX);
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    const sb = await c.createSandbox({ name: "demo", vcpus: 1, mem_mib: 256 });
    assert.equal(seen.method, "POST");
    assert.equal(seen.url, "/api/v1/sandboxes");
    assert.equal(seen.auth, `Bearer ${API_KEY}`);
    assert.deepEqual(JSON.parse(seen.body ?? "{}"), {
      name: "demo",
      vcpus: 1,
      mem_mib: 256,
    });
    assert.equal(sb.id, "sb-0123456789ab");
    assert.equal(sb.state, "running");
    assert.equal(sb.ip, "10.231.0.12");
    assert.equal(sb.parent_id, null);
  } finally {
    await close();
  }
});

test("exec (buffered): POST .../exec -> ExecResult", async () => {
  let url = "";
  const { baseUrl, close } = await fakeHearthd((req, res, body) => {
    url = req.url ?? "";
    assert.deepEqual(JSON.parse(body), {
      cmd: ["/bin/sh", "-c", "echo hi"],
      timeout_ms: 5000,
    });
    json(res, 200, {
      ok: true,
      exit_code: 0,
      stdout: "hi\n",
      stderr: "",
      truncated: false,
    });
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    const out = await c.exec(SANDBOX.id, {
      cmd: ["/bin/sh", "-c", "echo hi"],
      timeout_ms: 5000,
    });
    assert.equal(url, `/api/v1/sandboxes/${SANDBOX.id}/exec`);
    assert.deepEqual(out, {
      ok: true,
      exit_code: 0,
      stdout: "hi\n",
      stderr: "",
      truncated: false,
    });
  } finally {
    await close();
  }
});

test("execStream: multi-frame SSE, data split across chunks, done frame", async () => {
  const { baseUrl, close } = await fakeHearthd((req, res) => {
    assert.equal(req.url, `/api/v1/sandboxes/${SANDBOX.id}/exec?stream=1`);
    res.writeHead(200, { "content-type": "text/event-stream" });
    // Frame 1 split across two writes mid-payload to exercise buffering.
    res.write('data: {"stream":"stdout","da');
    setTimeout(() => {
      res.write('ta":"line one\\n"}\n\n');
      res.write('data: {"stream":"stderr","data":"warn\\n"}\n\n');
      // Two messages in one write, ending with the done frame.
      res.write(
        'data: {"stream":"stdout","data":"line two\\n"}\n\n' +
          'data: {"done":true,"ok":true,"exit_code":0,"truncated":false}\n\n',
      );
      res.end();
    }, 10);
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    let stdout = "";
    let stderr = "";
    const result = await c.execStream(
      SANDBOX.id,
      { cmd: ["/bin/sh", "-c", "build"], timeout_ms: 60000 },
      {
        onStdout: (chunk) => (stdout += chunk),
        onStderr: (chunk) => (stderr += chunk),
      },
    );
    assert.deepEqual(result, { exitCode: 0, truncated: false });
    assert.equal(stdout, "line one\nline two\n");
    assert.equal(stderr, "warn\n");
  } finally {
    await close();
  }
});

test("execStream: done.ok=false rejects with HearthError(200, error)", async () => {
  const { baseUrl, close } = await fakeHearthd((_req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write('data: {"stream":"stdout","data":"partial"}\n\n');
    res.write('data: {"done":true,"ok":false,"error":"guest agent unavailable"}\n\n');
    res.end();
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    await assert.rejects(
      c.execStream(SANDBOX.id, { cmd: ["true"] }),
      (err: unknown) => {
        assert.ok(err instanceof HearthError);
        assert.equal(err.status, 200);
        assert.equal(err.body, "guest agent unavailable");
        return true;
      },
    );
  } finally {
    await close();
  }
});

test("execStream: stream ending without done frame rejects", async () => {
  const { baseUrl, close } = await fakeHearthd((_req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write('data: {"stream":"stdout","data":"so far so good"}\n\n');
    res.end(); // interrupted: no done frame
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    let got = "";
    await assert.rejects(
      c.execStream(
        SANDBOX.id,
        { cmd: ["true"] },
        { onStdout: (chunk) => (got += chunk) },
      ),
      (err: unknown) => {
        assert.ok(err instanceof HearthError);
        assert.equal(err.body, "stream ended without done frame");
        return true;
      },
    );
    assert.equal(got, "so far so good"); // chunks before the cut still arrive
  } finally {
    await close();
  }
});

test("404 maps to HearthError with status and body", async () => {
  const { baseUrl, close } = await fakeHearthd((_req, res) => {
    json(res, 404, { error: "not found" });
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    await assert.rejects(c.getSandbox("sb-nonexistent"), (err: unknown) => {
      assert.ok(err instanceof HearthError);
      assert.equal(err.status, 404);
      assert.equal(err.body, '{"error":"not found"}');
      return true;
    });
  } finally {
    await close();
  }
});

test("deleteSandbox resolves void on 204; expose/unexpose round-trip", async () => {
  const calls: string[] = [];
  const { baseUrl, close } = await fakeHearthd((req, res, body) => {
    calls.push(`${req.method} ${req.url}`);
    if (req.method === "POST" && req.url?.endsWith("/expose")) {
      assert.deepEqual(JSON.parse(body), { name: "web", port: 8069 });
      json(res, 201, {
        name: "web",
        guest_port: 8069,
        node_port: 20001,
        hostname: `web--${SANDBOX.id}`,
        url: `https://web--${SANDBOX.id}.example.com`,
      });
      return;
    }
    res.writeHead(204);
    res.end();
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    const exp = await c.expose(SANDBOX.id, { name: "web", port: 8069 });
    assert.equal(exp.hostname, `web--${SANDBOX.id}`);
    assert.equal(exp.node_port, 20001);
    await c.unexpose(SANDBOX.id, "web");
    await c.deleteSandbox(SANDBOX.id);
    assert.deepEqual(calls, [
      `POST /api/v1/sandboxes/${SANDBOX.id}/expose`,
      `DELETE /api/v1/sandboxes/${SANDBOX.id}/expose/web`,
      `DELETE /api/v1/sandboxes/${SANDBOX.id}`,
    ]);
  } finally {
    await close();
  }
});

test("listTemplates unwraps {templates:[...]}", async () => {
  const { baseUrl, close } = await fakeHearthd((req, res) => {
    assert.equal(req.url, "/api/v1/templates");
    json(res, 200, {
      templates: [
        {
          id: "tpl-1",
          name: "odoo-18",
          image: "odoo-18",
          image_sha256: "a".repeat(64),
          vcpus: 2,
          mem_mib: 2048,
          disk_gb: 8,
          image_size_gb: 4,
          pool_size: 0,
          created_at: 1765000000,
        },
      ],
    });
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    const tpls = await c.listTemplates();
    assert.equal(tpls.length, 1);
    assert.equal(tpls[0]?.name, "odoo-18");
    assert.equal(tpls[0]?.image_sha256.length, 64);
  } finally {
    await close();
  }
});

test("HearthError carries X-Hearth-Request-Id from error responses (v4 P6)", async () => {
  const { baseUrl, close } = await fakeHearthd((_req, res) => {
    res.writeHead(404, {
      "content-type": "application/json",
      "x-hearth-request-id": "req-abc123",
    });
    res.end(`{"error":"not found"}`);
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    await assert.rejects(c.getSandbox("sb-nope"), (err: unknown) => {
      assert.ok(err instanceof HearthError);
      assert.equal(err.status, 404);
      assert.equal(err.requestId, "req-abc123");
      assert.match(err.message, /request_id req-abc123/);
      return true;
    });
  } finally {
    await close();
  }
});

test("HearthError requestId is empty against pre-P6 servers", async () => {
  const { baseUrl, close } = await fakeHearthd((_req, res) => {
    res.writeHead(500, { "content-type": "application/json" });
    res.end(`{"error":"boom"}`);
  });
  try {
    const c = new HearthClient({ baseUrl, apiKey: API_KEY });
    await assert.rejects(c.getSandbox("sb-x"), (err: unknown) => {
      assert.ok(err instanceof HearthError);
      assert.equal(err.requestId, "");
      return true;
    });
  } finally {
    await close();
  }
});
