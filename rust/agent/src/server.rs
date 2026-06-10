//! HTTP server (axum on 0.0.0.0:<port>): GET /healthz open, /v1/* bearer-protected.
//!
//! All response bodies are Content-Length framed (axum sized bodies).
//! Exact response shapes per API-V2.md and main.zig handler bodies.

use axum::{
    body::Body,
    extract::{Path, Request, State},
    http::{HeaderMap, StatusCode},
    response::Response,
    routing::{delete, get, post},
    Router,
};
use std::sync::Arc;

use crate::config::authorized;
use crate::vm::Manager;

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

pub fn build_router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/v1/vms", get(list_vms).post(create_vm))
        .route("/v1/vms/:id/sleep", post(sleep_vm))
        .route("/v1/vms/:id/wake", post(wake_vm))
        .route("/v1/vms/:id/fork", post(fork_vm))
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

    match state.mgr.create(&id, &name, vcpus, mem_mib).await {
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

    match state.mgr.fork(&parent_id, &child_id, &child_name).await {
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
