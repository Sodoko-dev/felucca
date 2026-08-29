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

use crate::config::authorized_either;
use crate::registration::NodeToken;
use crate::vm::{
    image, valid_vm_id, CreateSpec, ExecOutcome, ExecStreamOutcome, ExposeError, Manager,
    PoolSpec, RootfsError, DEFAULT_IMAGE,
};
use tracing::warn;

#[derive(Clone)]
pub struct AppState {
    pub mgr: Arc<Manager>,
    /// The shared token from this agent's own config.
    pub token: String,
    /// This node's own credential, minted by hearthd at enrollment and shared
    /// with the registration loop — so a rotation at re-enrollment is live on
    /// the inbound gate immediately, with no restart. Empty until issued.
    pub node_token: NodeToken,
}

fn json_response(status: StatusCode, body: &str) -> Response {
    Response::builder()
        .status(status)
        .header("Content-Type", "application/json")
        .header("Content-Length", body.len().to_string())
        .body(Body::from(body.to_string()))
        .unwrap()
}

/// `{"error": <msg>}` built with the serializer. Error messages interpolate
/// user-supplied ids, filesystem paths and OS strings, any of which can carry
/// a quote or a newline — a `format!`-built body lets those reshape the
/// document. Same reasoning as put_pools' validation errors.
fn error_response(status: StatusCode, msg: &str) -> Response {
    json_response(status, &serde_json::json!({ "error": msg }).to_string())
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

/// The one inbound gate. It takes the whole state, never a token string, so no
/// handler can be written that checks only one of this node's two credentials
/// — the mistake that made every hearthd call to a join-enrolled node 401.
fn check_auth(state: &AppState, headers: &HeaderMap) -> bool {
    // A poisoned lock still holds the live credential; treating it as absent
    // would 401 the control plane for the rest of the process's life.
    let node = state.node_token.read().unwrap_or_else(|e| e.into_inner());
    authorized_either(&state.token, &node, extract_bearer(headers))
}

/// Reject an id that is not a plain path component, before it reaches the
/// manager. axum percent-decodes `:id` captures, so `..%2F..%2Fetc%2Fhearth`
/// arrives here already decoded — every VM path on this node is derived from
/// this string, so the check belongs at the edge as well as at the path build.
fn reject_bad_id(id: &str) -> Option<Response> {
    if valid_vm_id(id) {
        return None;
    }
    Some(json_response(
        StatusCode::BAD_REQUEST,
        "{\"error\":\"invalid id\"}",
    ))
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

/// Open (unauthenticated) liveness probe. `ok` stays the process-is-up signal
/// the rollout scripts poll; `isolation` reports whether this node's guest
/// network fences are actually in force, so an operator can see a worker that
/// is serving tenants on a flat network. It is deliberately not folded into
/// `ok`: the agent refuses to start in that state, and a health check that
/// flips to unhealthy at runtime would restart-loop the unit instead.
async fn healthz(State(state): State<AppState>) -> Response {
    // Both values are literals, never strings, and `ok` stays the first key so
    // the body remains the documented contract with one field added.
    let body = format!("{{\"ok\":true,\"isolation\":{}}}", state.mgr.isolation_ok());
    json_response(StatusCode::OK, &body)
}

async fn list_vms(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    let body = state.mgr.list_json().await;
    json_response(StatusCode::OK, &body)
}

async fn create_vm(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state, req.headers()) {
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
    // The id becomes {data_dir}/instances/<id> and every file under it, so it
    // is held to the same class as an image name before anything is created.
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
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
            let resp = serde_json::json!({ "ok": true, "ip": ip }).to_string();
            json_response(StatusCode::CREATED, &resp)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn sleep_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.sleep_vm(&id).await {
        Ok(_) => json_response(StatusCode::OK, "{\"ok\":true}"),
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn wake_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.wake_vm(&id).await {
        Ok(ms) => {
            let body = format!("{{\"ok\":true,\"wake_ms\":{}}}", ms);
            json_response(StatusCode::OK, &body)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn fork_vm(
    State(state): State<AppState>,
    Path(parent_id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&parent_id) {
        return resp;
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
    // The child id arrives in the body, not the URL — it becomes a path
    // component exactly like the parent's does.
    if let Some(resp) = reject_bad_id(&child_id) {
        return resp;
    }
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
            let body = serde_json::json!({ "ok": true, "ip": ip }).to_string();
            json_response(StatusCode::CREATED, &body)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn exec_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
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
            ExecStreamOutcome::Ok(guest, slot) => {
                // Bound the data phase: the guest kills the command at
                // timeout_ms and writes its done frame; +60s covers transfer
                // and scheduling. On expiry the body just ends — the absent
                // done frame is the consumer's error signal.
                let deadline = std::time::Duration::from_millis(timeout_ms + 60_000);
                let stream = DeadlineStream {
                    inner: tokio_util::io::ReaderStream::new(guest),
                    sleep: Box::pin(tokio::time::sleep(deadline)),
                    remaining: MAX_STREAM_BYTES,
                    _slot: Some(slot),
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
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
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
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
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
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &format!("{:?}", e))
        }
    }
}

async fn vm_pause(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.pause(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) if e.starts_with("InvalidState") => {
            error_response(StatusCode::CONFLICT, &e)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn vm_resume(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.resume(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) if e.starts_with("InvalidState") => {
            error_response(StatusCode::CONFLICT, &e)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn vm_stop(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.stop(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn vm_start(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
    }
    match state.mgr.start(&id).await {
        Ok(_) => empty_response(StatusCode::OK),
        // Wrong lifecycle state (e.g. start on a paused VM) → 409, matching
        // the exec contract's 409 "not running" convention.
        Err(e) if e.starts_with("InvalidState") => {
            error_response(StatusCode::CONFLICT, &e)
        }
        Err(e) => {
            error_response(StatusCode::INTERNAL_SERVER_ERROR, &e)
        }
    }
}

async fn delete_vm(
    State(state): State<AppState>,
    Path(id): Path<String>,
    req: Request,
) -> Response {
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    // Rejected rather than absorbed by the idempotent 204: delete recurses
    // over the derived path without requiring the VM to exist.
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
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
    if !check_auth(&state, req.headers()) {
        return json_response(StatusCode::UNAUTHORIZED, "{\"error\":\"unauthorized\"}");
    }
    if let Some(resp) = reject_bad_id(&id) {
        return resp;
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

/// Total bytes the streamed-exec relay will forward from one guest.
///
/// The guest caps each stream at 16 MiB and stops; this is the host-side
/// backstop for a guest that does not, since the tenant is root inside the VM
/// and the bytes are relayed verbatim with no frame validation. Sized well
/// above two capped streams plus framing so no honest command is cut short.
const MAX_STREAM_BYTES: u64 = 64 * 1024 * 1024;

/// ReaderStream wrapper with an absolute deadline and a total-byte cap (v5
/// P5.1 streamed exec): when either is reached the stream ends, closing the
/// guest connection — the guest sees the hangup and SIGKILLs the command
/// group. Consumers detect the cut by the missing `done` frame.
///
/// It also owns the exec's guest round-trip permit, so the concurrency bound
/// covers the whole relay rather than just the handshake.
struct DeadlineStream<R> {
    inner: tokio_util::io::ReaderStream<R>,
    sleep: std::pin::Pin<Box<tokio::time::Sleep>>,
    remaining: u64,
    _slot: Option<crate::guestclient::GuestSlot>,
}

impl<R: tokio::io::AsyncRead + Unpin> futures_core::Stream for DeadlineStream<R> {
    type Item = std::io::Result<bytes::Bytes>;
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        use std::future::Future;
        if self.sleep.as_mut().poll(cx).is_ready() {
            return std::task::Poll::Ready(None);
        }
        if self.remaining == 0 {
            return std::task::Poll::Ready(None);
        }
        let polled = std::pin::Pin::new(&mut self.inner).poll_next(cx);
        if let std::task::Poll::Ready(Some(Ok(chunk))) = &polled {
            // A chunk that crosses the cap is forwarded whole and ends the
            // stream on the next poll: truncating mid-frame would hand the
            // consumer a half-line that parses as garbage rather than as a
            // missing done frame.
            self.remaining = self.remaining.saturating_sub(chunk.len() as u64);
        }
        polled
    }
}

/// PUT /v1/pools (v4 P4): replace the hearthd-managed warm-pool templates.
/// Body is a JSON array of PoolSpec; validation failure rejects the whole
/// set (applied atomically or not at all). The 5s refill loop converges.
async fn put_pools(State(state): State<AppState>, req: Request) -> Response {
    if !check_auth(&state, req.headers()) {
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
    if !check_auth(&state, req.headers()) {
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_reject_bad_id_turns_traversal_into_400() {
        // What axum hands the handler after percent-decoding
        // DELETE /v1/vms/..%2F..%2F..%2Fetc%2Fhearth.
        for bad in [
            "../../../etc/hearth",
            "../../../../etc/hearth",
            "..",
            ".",
            "a/b",
            "",
            "-rf",
        ] {
            let resp = reject_bad_id(bad).unwrap_or_else(|| panic!("{bad:?} must be rejected"));
            assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        }
        let long = "a".repeat(65);
        assert!(reject_bad_id(&long).is_some());
    }

    #[test]
    fn test_reject_bad_id_passes_real_ids_through() {
        for good in ["sb-0123456789ab", "pool-18f3a2b1c9d", "vm-1"] {
            assert!(reject_bad_id(good).is_none(), "{good:?} must be accepted");
        }
    }

    async fn body_of(resp: Response) -> serde_json::Value {
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 20).await.unwrap();
        serde_json::from_slice(&bytes).expect("error body must parse as JSON")
    }

    #[tokio::test]
    async fn test_error_body_escapes_the_interpolated_message() {
        // Was: format!("{{\"error\":\"{}\"}}", e) — error strings embed ids and
        // filesystem paths, so a quote in one reshaped the document.
        let msg = "copy /srv/a\" → /srv/b: No such file\n\"forged\":true";
        let resp = error_response(StatusCode::INTERNAL_SERVER_ERROR, msg);
        assert_eq!(resp.status(), StatusCode::INTERNAL_SERVER_ERROR);
        let parsed = body_of(resp).await;
        assert_eq!(parsed["error"], serde_json::json!(msg));
        assert_eq!(parsed.as_object().unwrap().len(), 1, "no forged keys");
    }

    // ---- /healthz isolation status ----

    #[tokio::test]
    async fn test_healthz_reports_the_isolation_status() {
        // Was: ensure_bridge_netfilter's verdict was discarded, so a worker
        // serving tenants on a flat network looked identical to a healthy one.
        let dir = tempfile::tempdir().unwrap();
        let state = AppState {
            mgr: Manager::new(
                dir.path().to_str().unwrap().to_string(),
                false,
                crate::ipalloc::Cidr { base: 0x0AE7_0000, prefix: 24 },
                0,
                "http://cp".into(),
                "tok".into(),
            ),
            token: "t".into(),
            node_token: Arc::new(std::sync::RwLock::new(String::new())),
        };
        let resp = healthz(State(state)).await;
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 20).await.unwrap();
        // Pinned exactly: the conformance suite asserts this string, and `ok`
        // stays first so the pre-existing contract just gains a field.
        // --net off reports isolation true — nothing to isolate, nothing broken.
        assert_eq!(
            std::str::from_utf8(&bytes).unwrap(),
            r#"{"ok":true,"isolation":true}"#
        );
    }

    // ---- inbound credentials ----

    fn auth_state(dir: &std::path::Path, shared: &str, node: &str) -> AppState {
        AppState {
            mgr: Manager::new(
                dir.to_str().unwrap().to_string(),
                false,
                crate::ipalloc::Cidr { base: 0x0AE7_0000, prefix: 24 },
                0,
                "http://cp".into(),
                shared.into(),
            ),
            token: shared.into(),
            node_token: Arc::new(std::sync::RwLock::new(node.into())),
        }
    }

    fn bearer(v: &str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert("authorization", v.parse().unwrap());
        h
    }

    #[tokio::test]
    async fn test_check_auth_accepts_either_of_this_nodes_credentials() {
        // Was: only the configured shared token was accepted, so once hearthd
        // started presenting the per-node token it minted at join, every call
        // to this node 401'd — exec, expose, sleep, wake, delete and the
        // lifecycle sweep against a join-enrolled worker all failed.
        let dir = tempfile::tempdir().unwrap();
        let shared = "9f2c1b8a7d4e6f0312a5b9c8d7e6f504";
        let node = "hearth_nt_9f2c1b8a7d4e6f0312a5b9c8d7e6f504";
        let state = auth_state(dir.path(), shared, node);

        assert!(check_auth(&state, &bearer(&format!("Bearer {}", node))));
        // The shared token keeps working through a mixed-version rollout.
        assert!(check_auth(&state, &bearer(&format!("Bearer {}", shared))));
        // Nothing else does.
        assert!(!check_auth(&state, &bearer("Bearer hearth_nt_someone_elses_node")));
        assert!(!check_auth(&state, &bearer("Bearer wrong")));
        assert!(!check_auth(&state, &HeaderMap::new()));
        // The exact "Bearer " prefix is still the requirement.
        assert!(!check_auth(&state, &bearer(node)));
        assert!(!check_auth(&state, &bearer(&format!("bearer {}", node))));
        assert!(!check_auth(&state, &bearer(&format!("Bearer  {}", node))));
    }

    #[tokio::test]
    async fn test_check_auth_before_enrollment_and_after_rotation() {
        let dir = tempfile::tempdir().unwrap();
        let shared = "9f2c1b8a7d4e6f0312a5b9c8d7e6f504";
        let state = auth_state(dir.path(), shared, "");
        // No node token yet: the shared token alone, and the empty credential
        // authorizes nobody rather than everybody.
        assert!(check_auth(&state, &bearer(&format!("Bearer {}", shared))));
        assert!(!check_auth(&state, &bearer("Bearer ")));
        assert!(!check_auth(&state, &bearer("Bearer hearth_nt_anything")));

        // A rotation lands on the handle the registration loop shares with the
        // server, so it is live on the inbound gate with no restart.
        let first = "hearth_nt_1111111111111111111111";
        *state.node_token.write().unwrap() = first.into();
        assert!(check_auth(&state, &bearer(&format!("Bearer {}", first))));
        let second = "hearth_nt_2222222222222222222222";
        *state.node_token.write().unwrap() = second.into();
        assert!(check_auth(&state, &bearer(&format!("Bearer {}", second))));
        // The superseded credential stops working.
        assert!(!check_auth(&state, &bearer(&format!("Bearer {}", first))));
        assert!(check_auth(&state, &bearer(&format!("Bearer {}", shared))));
    }

    // ---- streamed-exec relay bounds ----

    fn deadline_stream<R>(inner: R, cap: u64, deadline_ms: u64) -> DeadlineStream<R>
    where
        R: tokio::io::AsyncRead + Unpin,
    {
        DeadlineStream {
            inner: tokio_util::io::ReaderStream::new(inner),
            sleep: Box::pin(tokio::time::sleep(std::time::Duration::from_millis(deadline_ms))),
            remaining: cap,
            _slot: None,
        }
    }

    #[tokio::test]
    async fn test_stream_relay_stops_at_the_total_byte_cap() {
        // Was: guest bytes were relayed verbatim with no total cap, so a guest
        // that never sends its done frame streamed for as long as the deadline.
        use futures_core::Stream;
        use tokio::io::AsyncWriteExt;

        let (client, mut server) = tokio::io::duplex(8 * 1024);
        tokio::spawn(async move {
            let chunk = vec![b'x'; 8 * 1024];
            // Far more than the cap; the write loop ends when the reader stops.
            for _ in 0..64 {
                if server.write_all(&chunk).await.is_err() {
                    break;
                }
            }
        });

        let stream = deadline_stream(client, 32 * 1024, 30_000);
        tokio::pin!(stream);
        let mut total = 0usize;
        while let Some(chunk) = std::future::poll_fn(|cx| stream.as_mut().poll_next(cx)).await {
            total += chunk.unwrap().len();
        }
        // Ends at the cap, plus at most the chunk that crossed it.
        assert!(total >= 32 * 1024, "cut short at {}", total);
        assert!(total < 64 * 1024, "relayed {} bytes past the cap", total);
    }

    #[tokio::test]
    async fn test_stream_relay_passes_a_short_response_through_untouched() {
        use futures_core::Stream;
        use tokio::io::AsyncWriteExt;

        let (client, mut server) = tokio::io::duplex(1024);
        let frames = b"{\"stream\":\"stdout\",\"data\":\"a\"}\n{\"done\":true}\n";
        tokio::spawn(async move {
            server.write_all(frames).await.unwrap();
            server.shutdown().await.unwrap();
        });

        let stream = deadline_stream(client, MAX_STREAM_BYTES, 30_000);
        tokio::pin!(stream);
        let mut got = Vec::new();
        while let Some(chunk) = std::future::poll_fn(|cx| stream.as_mut().poll_next(cx)).await {
            got.extend_from_slice(&chunk.unwrap());
        }
        assert_eq!(got, frames);
    }

    #[tokio::test]
    async fn test_error_body_content_length_matches_the_escaped_body() {
        // json_response frames on the body it is handed, so the escaped length
        // is the one that must be advertised.
        let resp = error_response(StatusCode::CONFLICT, "InvalidState: \"paused\"");
        let len: usize = resp
            .headers()
            .get("Content-Length")
            .unwrap()
            .to_str()
            .unwrap()
            .parse()
            .unwrap();
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 20).await.unwrap();
        assert_eq!(len, bytes.len());
    }
}
