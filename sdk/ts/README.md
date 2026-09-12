# @felucca/sdk

Thin typed TypeScript client for the Felucca control plane (`feluccad`) REST
API ([docs/API-V2.md](../../docs/API-V2.md)). Zero runtime dependencies —
uses global `fetch` (Node >= 18, browsers). ESM with bundled types.

```sh
npm run build   # tsc -> dist/
npm test        # node --test (Node >= 23.6, or 22.6+ with --experimental-strip-types)
```

## Usage

```ts
import { FeluccaClient, FeluccaError } from "@felucca/sdk";

const felucca = new FeluccaClient({
  baseUrl: "https://felucca.example.com",
  apiKey: process.env.FELUCCA_API_KEY!, // admin token or felucca_sk_… tenant key
});

// Create a sandbox (optionally from a template)
const sb = await felucca.createSandbox({
  name: "demo",
  template: "odoo-18",
  vcpus: 2,
  mem_mib: 2048,
});
console.log(sb.id, sb.state, sb.ip);

// Stream a long-running command (SSE under the hood)
const { exitCode } = await felucca.execStream(
  sb.id,
  { cmd: ["/bin/sh", "-c", "apt-get update && apt-get install -y build-essential"], timeout_ms: 300_000 },
  {
    onStdout: (chunk) => process.stdout.write(chunk),
    onStderr: (chunk) => process.stderr.write(chunk),
  },
);
console.log("exit:", exitCode);

// Or buffered
const out = await felucca.exec(sb.id, { cmd: ["/bin/sh", "-c", "echo hi"] });
console.log(out.stdout); // "hi\n"

// Publish a service port and get its public URL
const exposed = await felucca.expose(sb.id, { name: "web", port: 8069 });
console.log(exposed.url ?? exposed.hostname); // https://web--sb-….<ingress domain>

// Lifecycle
await felucca.sleepSandbox(sb.id);
const woken = await felucca.wakeSandbox(sb.id); // woken.wake_ms
const child = await felucca.forkSandbox(sb.id, { name: "demo-fork" });

// Cleanup
await felucca.unexpose(sb.id, "web");
await felucca.deleteSandbox(child.id);
await felucca.deleteSandbox(sb.id);
```

## Errors

Every non-2xx response rejects with `FeluccaError`:

```ts
try {
  await felucca.getSandbox("sb-nonexistent");
} catch (err) {
  if (err instanceof FeluccaError) {
    console.error(err.status, err.body); // 404 {"error":"not found"}
  }
}
```

Streamed execs also reject with `FeluccaError` when the server reports an
in-stream failure (`done.ok === false`, `status` is 200) or when the
connection drops before the terminal done frame
(`body === "stream ended without done frame"`).

## API

| Method | Endpoint |
|---|---|
| `createSandbox(req?)` | `POST /api/v1/sandboxes` |
| `getSandbox(id)` | `GET /api/v1/sandboxes/{id}` |
| `listSandboxes()` | `GET /api/v1/sandboxes` |
| `deleteSandbox(id)` | `DELETE /api/v1/sandboxes/{id}` |
| `sleepSandbox(id)` | `POST /api/v1/sandboxes/{id}/sleep` |
| `wakeSandbox(id)` | `POST /api/v1/sandboxes/{id}/wake` |
| `forkSandbox(id, req?)` | `POST /api/v1/sandboxes/{id}/fork` |
| `exec(id, req)` | `POST /api/v1/sandboxes/{id}/exec` |
| `execStream(id, req, handlers?)` | `POST /api/v1/sandboxes/{id}/exec?stream=1` (SSE) |
| `expose(id, req)` | `POST /api/v1/sandboxes/{id}/expose` |
| `unexpose(id, name)` | `DELETE /api/v1/sandboxes/{id}/expose/{name}` |
| `listExposes(id)` | reads `exposes` from `GET /api/v1/sandboxes/{id}` |
| `listTemplates()` | `GET /api/v1/templates` |

All wire types (`Sandbox`, `Expose`, `Template`, …) mirror the API field
names exactly (snake_case).
