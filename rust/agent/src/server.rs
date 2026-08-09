//! HTTP server (axum on 0.0.0.0:<port>): GET /healthz open, /v1/* bearer-protected.
//!
//! All response bodies are Content-Length framed (axum sized bodies).
//! Exact response shapes per API-V2.md and main.zig handler bodies.

use axum::{
    body::Body,
    extract::{Path, Request, State},
    http::{HeaderMap, StatusCode},
    response::Response,
    routing::{delete, get, post, put},
    Router,
};
use std::sync::Arc;

use crate::config::authorized;
use crate::vm::{
    image, CreateSpec, ExecOutcome, ExecStreamOutcome, ExposeError, Manager, PoolSpec,
    RootfsError, DEFAULT_IMAGE,
};
use tracing::warn;

#[derive(Clone)]
pub struct AppState {
    pub mgr: Arc<Manager>,
    pub token: String,
}

fn json_response(status: StatusCode, body: &str) -> Response {
    Response::builder()
        .status(status)
        .header("Content-Type", "application/json")
        .header("Content-Length", body.len().to_string())
        .body(Body::from(body.to_string()))
        .unwrap()
}

fn empty_response(status: StatusCode) -> Response {
    Response::builder()
        .status(status)
        .header("Content-Length", "0")
        .body(Body::empty())
        .unwrap()
}

fn extract_bearer(headers: &HeaderMap) -> Option<&str> {
    headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
}

fn check_auth(token: &str, headers: &HeaderMap) -> bool {
    authorized(token, extract_bearer(headers))
}

/// Extract the X-Hearth-Request-Id header for structured log correlation.
fn req_id(headers: &HeaderMap) -> Option<String> {
    headers
        .get("x-hearth-request-id")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
}

pub fn build_router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/v1/vms", get(list_vms).post(create_vm))
        .route("/v1/vms/:id/rootfs", get(vm_rootfs))
        .route("/v1/pools", put(put_pools))
        .route("/v1/images/prefetch", post(prefetch_image))
        .route("/v1/vms/:id/sleep", post(sleep_vm))
        .route("/v1/vms/:id/wake", post(wake_vm))
        .route("/v1/vms/:id/fork", post(fork_vm))
        .route("/v1/vms/:id/exec", post(exec_vm))
        .route("/v1/vms/:id/expose", post(expose_vm).delete(unexpose_vm))
        .route("/v1/vms/:id/pause", post(vm_pause))
        .route("/v1/vms/:id/resume", post(vm_resume))
        .route("/v1/vms/:id/stop", post(vm_stop))
        .route("/v1/vms/:id/start", post(vm_start))
        .route("/v1/vms/:id", delete(delete_vm))
        .with_state(state)
}

async fn healthz() -> Response {
    json_response(StatusCode::OK, "{\"ok\":true}")
}

async fn list_vms(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let body = state.mgr.list_json().await;
    json_response(StatusCode::OK, &body)
}

async fn create_vm(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let body_str = match std::str::from_utf8(&body_bytes) {
        Ok(s) => s,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let json: serde_json::Value = match serde_json::from_str(body_str) {
        Ok(v) => v,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}"),
    };

    let id = match json.get("id").and_then(|v| v.as_str()) {
        Some(s) => s.to_string(),
        None => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"id required\"}"),
    };
    let name = json.get("name").and_then(|v| v.as_str()).unwrap_or(&id).to_string();
    let vcpus = json.get("vcpus").and_then(|v| v.as_u64()).unwrap_or(1) as u32;
    let mem_mib = json.get("mem_mib").and_then(|v| v.as_u64()).unwrap_or(256);
    let tenant_id = json.get("tenant_id").and_then(|v| v.as_str()).map(|s| s.to_string());

    // v4 P4: optional template image, expected sha and disk size. The image
    // name is interpolated into a filesystem path and a control-plane URL —
    // anything outside the strict class is rejected here, before it can move.
    let image_name = json.get("image").and_then(|v| v.as_str()).unwrap_or(DEFAULT_IMAGE).to_string();
    if !image::valid_image_name(&image_name) {
        return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"invalid image\"}");
    }
    let image_sha256 = match json.get("image_sha256") {
        None | Some(serde_json::Value::Null) => None,
        Some(serde_json::Value::String(s)) if image::valid_sha256(s) => Some(s.clone()),
        _ => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"invalid image_sha256\"}"),
    };
    let disk_gb = json.get("disk_gb").and_then(|v| v.as_u64()).unwrap_or(0);
    if disk_gb > 128 {
        return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"disk_gb above cap (128)\"}");
    }

    let spec = CreateSpec {
        id: id.clone(),
        name,
        vcpus,
        mem_mib,
        tenant_id,
        image: image_name,
        image_sha256,
        disk_gb: disk_gb as u32,
    };
    // tokio::spawn detaches the create from this request future: if the
    // client times out and drops the connection (e.g. hearthd's 30s cap
    // during a first image pull), the create still converges to
    // running/error instead of leaving the record stuck in Creating.
    let mgr = Arc::clone(&state.mgr);
    let create_spec = spec.clone();
    let result = match tokio::spawn(async move { mgr.create(&create_spec).await }).await {
        Ok(r) => r,
        Err(e) => Err(format!("create task: {}", e)),
    };
    match result {
        Ok(_) => {
            let ip = state.mgr.ip_of(&id).await;
            let ip_str = match ip {
                Some(ref s) => format!("\"{}\"", s),
                None => "null".to_string(),
            };
            let resp = format!("{{\"ok\":true,\"ip\":{}}}", ip_str);
            json_response(StatusCode::CREATED, &resp)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn sleep_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.sleep_vm(&id).await {
        Ok(_) => json_response(StatusCode::OK, "{\"ok\":true}"),
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn wake_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.wake_vm(&id).await {
        Ok(ms) => {
            let body = format!("{{\"ok\":true,\"wake_ms\":{}}}", ms);
            json_response(StatusCode::OK, &body)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn fork_vm(
    State(state): State<AppState>,
    Path(parent_id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let body_str = match std::str::from_utf8(&body_bytes) {
        Ok(s) => s,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let json: serde_json::Value = match serde_json::from_str(body_str) {
        Ok(v) => v,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}"),
    };

    let child_id = match json.get("id").and_then(|v| v.as_str()) {
        Some(s) => s.to_string(),
        None => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"id required\"}"),
    };
    let child_name = json.get("name").and_then(|v| v.as_str()).unwrap_or(&child_id).to_string();

    // Same cancellation shield as create_vm: a dropped connection must not
    // abandon a half-built child record.
    let mgr = Arc::clone(&state.mgr);
    let (pid2, cid2, cname2) = (parent_id.clone(), child_id.clone(), child_name.clone());
    let result = match tokio::spawn(async move { mgr.fork(&pid2, &cid2, &cname2).await }).await {
        Ok(r) => r,
        Err(e) => Err(format!("fork task: {}", e)),
    };
    match result {
        Ok(ip) => {
            let ip_str = match ip {
                Some(ref s) => format!("\"{}\"", s),
                None => "null".to_string(),
            };
            let body = format!("{{\"ok\":true,\"ip\":{}}}", ip_str);
            json_response(StatusCode::CREATED, &body)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn exec_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let rid = req_id(req.headers()).unwrap_or_default();
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let body_str = match std::str::from_utf8(&body_bytes) {
        Ok(s) => s,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let json: serde_json::Value = match serde_json::from_str(body_str) {
        Ok(v) => v,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}"),
    };

    // cmd must be a non-empty array of strings (§1).
    let cmd: Vec<String> = match json.get("cmd").and_then(|v| v.as_array()) {
        Some(arr) if !arr.is_empty() && arr.iter().all(|e| e.is_string()) => {
            arr.iter().map(|e| e.as_str().unwrap().to_string()).collect()
        }
        _ => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"cmd required\"}"),
    };

    // timeout_ms: default 30000, max 300000 (§1).
    let timeout_ms = json
        .get("timeout_ms")
        .and_then(|v| v.as_u64())
        .unwrap_or(30_000)
        .min(300_000);

    // v5 P5.1: "stream": true switches the response to chunked NDJSON — the
    // guest's frames forwarded verbatim. Setup failures keep the buffered
    // path's status mapping.
    if json.get("stream").and_then(|v| v.as_bool()) == Some(true) {
        return match state.mgr.exec_stream(&id, &cmd, timeout_ms).await {
            ExecStreamOutcome::Ok(guest) => {
                // Bound the data phase: the guest kills the command at
                // timeout_ms and writes its done frame; +60s covers transfer
                // and scheduling. On expiry the body just ends — the absent
                // done frame is the consumer's error signal.
                let deadline = std::time::Duration::from_millis(timeout_ms + 60_000);
                let stream = DeadlineStream {
                    inner: tokio_util::io::ReaderStream::new(guest),
                    sleep: Box::pin(tokio::time::sleep(deadline)),
                };
                Response::builder()
                    .status(StatusCode::OK)
                    .header("Content-Type", "application/x-ndjson")
                    .body(Body::from_stream(stream))
                    .unwrap()
            }
            ExecStreamOutcome::NotRunning => {
                json_response(StatusCode::CONFLICT, "{\"error\":\"not running\"}")
            }
            ExecStreamOutcome::Unavailable => json_response(
                StatusCode::NOT_IMPLEMENTED,
                "{\"error\":\"guest agent unavailable\"}",
            ),
            ExecStreamOutcome::Failed(reason) => {
                warn!(vm = %id, err = %reason, request_id = %rid, "exec stream failed (guest unreachable)");
                json_response(
                    StatusCode::NOT_IMPLEMENTED,
                    "{\"error\":\"guest agent unavailable\"}",
                )
            }
            ExecStreamOutcome::NotFound => {
                json_response(StatusCode::INTERNAL_SERVER_ERROR, "{\"error\":\"NotFound\"}")
            }
        };
    }

    match state.mgr.exec(&id, &cmd, timeout_ms).await {
        ExecOutcome::Ok(v) => json_response(StatusCode::OK, &v.to_string()),
        ExecOutcome::NotRunning => {
            json_response(StatusCode::CONFLICT, "{\"error\":\"not running\"}")
        }
        ExecOutcome::Unavailable => json_response(
            StatusCode::NOT_IMPLEMENTED,
            "{\"error\":\"guest agent unavailable\"}",
        ),
        ExecOutcome::Failed(reason) => {
            // Connect/handshake failure → same 501 body; log the cause server-side.
            warn!(vm = %id, err = %reason, request_id = %rid, "exec failed (guest unreachable)");
            json_response(
                StatusCode::NOT_IMPLEMENTED,
                "{\"error\":\"guest agent unavailable\"}",
            )
        }
        // Unknown id → 500 {"error":"NotFound"}, consistent with other handlers
        // (sleep/wake/etc. surface "NotFound" as a 500 error body).
        ExecOutcome::NotFound => {
            json_response(StatusCode::INTERNAL_SERVER_ERROR, "{\"error\":\"NotFound\"}")
        }
    }
}

/// Read + parse {"guest_port": n} from a request body. Err carries the
/// ready-made 400 response. guest_port 0 (or out of u16 range) is invalid.
async fn parse_guest_port(req: Request) -> Result<u16, Response> {
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return Err(json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}")),
    };
    let body_str = match std::str::from_utf8(&body_bytes) {
        Ok(s) => s,
        Err(_) => return Err(json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}")),
    };
    let json: serde_json::Value = match serde_json::from_str(body_str) {
        Ok(v) => v,
        Err(_) => return Err(json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}")),
    };
    match json.get("guest_port").and_then(|v| v.as_u64()) {
        Some(p) if (1..=65535).contains(&p) => Ok(p as u16),
        _ => Err(json_response(StatusCode::BAD_REQUEST, "{\"error\":\"guest_port required\"}")),
    }
}

async fn expose_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let guest_port = match parse_guest_port(req).await {
        Ok(p) => p,
        Err(resp) => return resp,
    };
    match state.mgr.expose(&id, guest_port).await {
        Ok(node_port) => {
            let body = format!("{{\"node_port\":{}}}", node_port);
            json_response(StatusCode::OK, &body)
        }
        Err(ExposeError::NotFound) => {
            json_response(StatusCode::NOT_FOUND, "{\"error\":\"not found\"}")
        }
        Err(ExposeError::NoIp) => json_response(StatusCode::CONFLICT, "{\"error\":\"no ip\"}"),
        Err(ExposeError::Exhausted) => {
            json_response(StatusCode::CONFLICT, "{\"error\":\"ports exhausted\"}")
        }
    }
}

async fn unexpose_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let guest_port = match parse_guest_port(req).await {
        Ok(p) => p,
        Err(resp) => return resp,
    };
    match state.mgr.unexpose(&id, guest_port).await {
        // Missing entry is OK (idempotent) — only an unknown VM is 404.
        Ok(()) => json_response(StatusCode::OK, "{\"ok\":true}"),
        Err(ExposeError::NotFound) => {
            json_response(StatusCode::NOT_FOUND, "{\"error\":\"not found\"}")
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{:?}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn vm_pause(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.pause(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) if e.starts_with("InvalidState") => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::CONFLICT, &msg)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn vm_resume(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.resume(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) if e.starts_with("InvalidState") => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::CONFLICT, &msg)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn vm_stop(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.stop(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn vm_start(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    match state.mgr.start(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        // Wrong lifecycle state (e.g. start on a paused VM) → 409, matching
        // the exec contract's 409 "not running" convention.
        Err(e) if e.starts_with("InvalidState") => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::CONFLICT, &msg)
        }
        Err(e) => {
            let msg = format!("{{\"error\":\"{}\"}}", e);
            json_response(StatusCode::INTERNAL_SERVER_ERROR, &msg)
        }
    }
}

async fn delete_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    // delete is idempotent — unknown id also 204.
    let _ = state.mgr.delete(&id).await;
    empty_response(StatusCode::NO_CONTENT)
}

/// GET /v1/vms/:id/rootfs (v4 P4): stream the instance's rootfs.ext4 for
/// template capture. Only a Stopped VM may be captured — a live FC could
/// still be writing the file mid-stream.
async fn vm_rootfs(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let rid = req_id(req.headers()).unwrap_or_default();
    let path = match state.mgr.begin_capture(&id).await {
        Ok(p) => p,
        Err(RootfsError::NotFound) => {
            return json_response(StatusCode::NOT_FOUND, "{\"error\":\"not found\"}")
        }
        Err(RootfsError::NotStopped) => {
            return json_response(StatusCode::CONFLICT, "{\"error\":\"not stopped\"}")
        }
        Err(RootfsError::CaptureInProgress) => {
            return json_response(StatusCode::CONFLICT, "{\"error\":\"capture in progress\"}")
        }
    };
    let file = match tokio::fs::File::open(&path).await {
        Ok(f) => f,
        Err(e) => {
            warn!(vm = %id, path = %path, err = %e, request_id = %rid, "rootfs capture failed to open");
            state.mgr.end_capture(&id).await;
            return json_response(StatusCode::NOT_FOUND, "{\"error\":\"not found\"}");
        }
    };
    let len = match file.metadata().await {
        Ok(m) => m.len(),
        Err(e) => {
            warn!(vm = %id, path = %path, err = %e, request_id = %rid, "rootfs capture failed to stat");
            state.mgr.end_capture(&id).await;
            return json_response(StatusCode::INTERNAL_SERVER_ERROR, "{\"error\":\"stat failed\"}");
        }
    };
    // The capture registration lives exactly as long as the stream: normal
    // completion and client disconnect both drop CaptureStream, which
    // releases the id so start() may run again.
    let stream = CaptureStream {
        inner: tokio_util::io::ReaderStream::new(file),
        mgr: Arc::clone(&state.mgr),
        id,
    };
    Response::builder()
        .status(StatusCode::OK)
        .header("Content-Type", "application/octet-stream")
        .header("Content-Length", len.to_string())
        .body(Body::from_stream(stream))
        .unwrap()
}

/// ReaderStream wrapper whose Drop releases the VM's capture registration —
/// the only reliable hook that fires on BOTH stream completion and client
/// disconnect.
struct CaptureStream {
    inner: tokio_util::io::ReaderStream<tokio::fs::File>,
    mgr: Arc<Manager>,
    id: String,
}

impl futures_core::Stream for CaptureStream {
    type Item = std::io::Result<bytes::Bytes>;
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        std::pin::Pin::new(&mut self.inner).poll_next(cx)
    }
}

impl Drop for CaptureStream {
    fn drop(&mut self) {
        let mgr = Arc::clone(&self.mgr);
        let id = std::mem::take(&mut self.id);
        tokio::spawn(async move { mgr.end_capture(&id).await });
    }
}

/// ReaderStream wrapper with an absolute deadline (v5 P5.1 streamed exec):
/// when the timer fires the stream ends, closing the guest connection —
/// the guest sees the hangup and SIGKILLs the command group. Consumers detect
/// the cut by the missing `done` frame.
struct DeadlineStream {
    inner: tokio_util::io::ReaderStream<tokio::net::UnixStream>,
    sleep: std::pin::Pin<Box<tokio::time::Sleep>>,
}

impl futures_core::Stream for DeadlineStream {
    type Item = std::io::Result<bytes::Bytes>;
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        use std::future::Future;
        if self.sleep.as_mut().poll(cx).is_ready() {
            return std::task::Poll::Ready(None);
        }
        std::pin::Pin::new(&mut self.inner).poll_next(cx)
    }
}

/// PUT /v1/pools (v4 P4): replace the hearthd-managed warm-pool templates.
/// Body is a JSON array of PoolSpec; validation failure rejects the whole
/// set (applied atomically or not at all). The 5s refill loop converges.
async fn put_pools(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let specs: Vec<PoolSpec> = match serde_json::from_slice(&body_bytes) {
        Ok(s) => s,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}"),
    };
    match state.mgr.set_pools(specs).await {
        Ok(()) => json_response(StatusCode::OK, "{\"ok\":true}"),
        Err(e) => {
            // Validation messages embed user input ({:?} quotes included) —
            // build the body with the serializer, never format!.
            let msg = serde_json::json!({ "error": e }).to_string();
            json_response(StatusCode::BAD_REQUEST, &msg)
        }
    }
}

/// POST /v1/images/prefetch (v4 P4): warm the image cache in the background
/// so the first create from a fresh template doesn't pull inside a create
/// request. Validation mirrors create_vm exactly; the sha is mandatory
/// (a prefetch without one could never pull anything).
async fn prefetch_image(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state.token, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let rid = req_id(req.headers()).unwrap_or_default();
    let body_bytes = match axum::body::to_bytes(req.into_body(), 1 << 20).await {
        Ok(b) => b,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad request\"}"),
    };
    let json: serde_json::Value = match serde_json::from_slice(&body_bytes) {
        Ok(v) => v,
        Err(_) => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"bad json\"}"),
    };
    let image_name = match json.get("image").and_then(|v| v.as_str()) {
        Some(s) if image::valid_image_name(s) => s.to_string(),
        _ => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"invalid image\"}"),
    };
    let sha = match json.get("image_sha256").and_then(|v| v.as_str()) {
        Some(s) if image::valid_sha256(s) => s.to_string(),
        _ => return json_response(StatusCode::BAD_REQUEST, "{\"error\":\"invalid image_sha256\"}"),
    };
    let mgr = Arc::clone(&state.mgr);
    tokio::spawn(async move {
        if let Err(e) = mgr.ensure_image(&image_name, Some(&sha)).await {
            warn!(image = %image_name, err = %e, request_id = %rid, "prefetch failed");
        }
    });
    json_response(StatusCode::ACCEPTED, "{\"ok\":true}")
}
