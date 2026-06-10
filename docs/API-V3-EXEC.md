# Hearth v3 — vsock exec + fork re-IP contract

Binding contract for the v3.1 work. Additive only: every API-V2.md behavior and
every existing conformance golden stays valid. Three components integrate over
this document: the guest agent (`rust/guest/`), the node agent (`rust/agent/`),
and the control plane (`go/`).

## 1. Guest agent (`hearth-guest`)

Static musl binary installed at `/usr/local/bin/hearth-guest` inside the base
rootfs, started by a systemd unit (`hearth-guest.service`, multi-user.target).
Listens on **vsock port 52** (AF_VSOCK, any CID).

Protocol: one request per connection. Request = single JSON line (`\n`
terminated); response = single JSON line; then the server closes.

```jsonc
// exec — run a command, capture output
{"op":"exec","cmd":["/bin/sh","-c","echo hi"],"timeout_ms":30000}
// → {"ok":true,"exit_code":0,"stdout":"hi\n","stderr":""}
//   stdout/stderr: lossy UTF-8, each capped at 1 MiB (truncated flag set if cut)
// → {"ok":true,"exit_code":124,...} on timeout the process is SIGKILLed, exit_code 124
// → {"ok":false,"error":"..."} only for malformed requests / spawn failures

// set_ip — reconfigure the primary interface (fork re-IP)
{"op":"set_ip","ip":"10.231.0.7","prefix":24,"gw":"10.231.0.1","dev":"eth0"}
// → {"ok":true}
// implementation: ip addr flush dev eth0; ip addr add <ip>/<prefix> dev eth0;
//                 ip link set eth0 up; ip route replace default via <gw>
```

Response always includes `"ok"`. Limits: request line ≤ 64 KiB, cmd must be a
non-empty array, timeout_ms default 30000, max 300000.

## 2. Rootfs injection (`infra/guest-agent-install.sh`)

Run on each worker (idempotent): loop-mounts `{data_dir}/images/ubuntu-base.ext4`
read-write, installs the binary + systemd unit + enable symlink, then writes the
marker file **`{data_dir}/images/.hearth-guest-v1`** on the worker. New VM rootfs
copies inherit the agent. Existing VM copies (running/sleeping/pooled) do NOT —
exec on them fails gracefully (below).

## 3. Node agent (`rust/agent/`)

- **Vsock device**: when the marker file exists at cold-boot configure time, add
  `PUT /vsock {"guest_cid":3,"uds_path":"<instance_dir>/v.sock"}` before
  InstanceStart. Record `vsock:true` in meta.json (new optional field; absent =
  false; older meta.json files parse unchanged).
- **Host→guest connect** (Firecracker hybrid vsock): connect to
  `<instance_dir>/v.sock` (unix), send `CONNECT 52\n`, wait for `OK <port>\n`,
  then speak the §1 protocol.
- **`POST /v1/vms/{id}/exec`** body `{"cmd":[...],"timeout_ms":N}` →
  200 with the guest's response JSON verbatim on success;
  409 `{"error":"not running"}` if the VM is not running;
  501 `{"error":"guest agent unavailable"}` if the VM has no vsock device
  (pre-v3 VM or marker absent) or the connect/handshake fails.
- **Fork re-IP**: after the child restores and resumes, if the child has
  `vsock:true`, connect and send `set_ip` with the child's allocated ip/gw
  (gw = cidr `.1`). Up to 3 attempts over ~3s (guest may still be settling).
  Failure logs a warning and does NOT fail the fork (old caveat applies).
  Snapshot semantics — **validated on FC v1.16** (these are now facts, not
  open questions):
  - The vsock UDS path is baked into vmstate and FC re-binds it on restore.
    A fork-style restore into a different directory uses the snapshot/load
    `vsock` override to re-point the UDS at the child's own `v.sock`.
  - **Stale-socket trap**: sleep SIGKILLs Firecracker, which leaves the old
    `v.sock` file on disk; the next restore then fails the whole snapshot
    load with `VsockUnixBackend: Address in use (EADDRINUSE)`. Wake (and any
    same-path restore) must `rm` the stale `v.sock` before spawning FC —
    implemented in `wake_vm`. Symptom if regressed: wake returns an agent
    error and the VM stays `sleeping`; FC serial log shows the EADDRINUSE.
- **Sleep**: never snapshot while an exec connection is open (serialize exec
  and sleep per VM, or document that FC refuses snapshots with active vsock
  connections and surface the error).

## 4. Control plane (`go/`)

- **`POST /api/v1/sandboxes/{id}/exec`** body `{"cmd":["..."],"timeout_ms":N}`:
  404 unknown id; 409 `{"error":"not running"}` unless state == running;
  proxy to the agent with an HTTP timeout of `timeout_ms + 10s`;
  agent 200 → 200 with the agent body verbatim;
  agent 501 → 501 `{"error":"guest agent unavailable"}`;
  other agent failures → 502 `{"error":"agent exec failed"}`.
- Metric: `hearth_execs_total` counter (count attempts).
- No sandbox JSON shape changes. Fork responses are unchanged — the fix is that
  the child guest now actually USES the `ip` already reported.

## 5. Acceptance (end-to-end, lab)

1. Fresh sandbox → `exec ["sh","-c","echo from-guest; id -u"]` through hearthd
   returns exit_code 0, stdout "from-guest\n0\n".
2. exec on a pre-v3 sandbox → 501 through the whole chain.
3. sleep → wake → exec still works (vsock survives snapshot/restore).
4. Fork a running sandbox → child answers ping on ITS OWN ip within ~5s; parent
   keeps its ip; exec works on both parent and child.
5. verify-v2.sh extended: fork section also pings the child ip; new §exec check.
6. Full conformance suite still green (no v2 golden changes).
