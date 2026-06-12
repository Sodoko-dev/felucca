//! Image-aware create support (v4 P4): name/sha validation, the pull-or-use
//! decision for the sha256-addressed image cache, rootfs grow computation,
//! and the curl download config (token piped on stdin, never argv).
//!
//! Image names are interpolated into `{data_dir}/images/{image}.ext4` and a
//! control-plane URL, so the character class is locked down hard: lowercase
//! [a-z0-9._-], 1..=64 chars, no leading '.' or '-', no "..". Everything else
//! is rejected BEFORE it reaches a path or a curl config.

use crate::wg::curl_config_escape;
use std::process::Stdio;

/// Strict image-name check: [a-z0-9._-], length 1..=64, no leading '.' or
/// '-' (hidden files / option smuggling), no ".." (path traversal). The name
/// becomes a path component and a URL path segment — nothing else may pass.
pub fn valid_image_name(name: &str) -> bool {
    if name.is_empty() || name.len() > 64 {
        return false;
    }
    let first = name.as_bytes()[0];
    if first == b'.' || first == b'-' {
        return false;
    }
    if name.contains("..") {
        return false;
    }
    name.bytes().all(|b| {
        b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'.' || b == b'_' || b == b'-'
    })
}

/// Exactly 64 lowercase hex chars (a sha256 digest as we and the control
/// plane print it; uppercase is rejected so sidecar comparison stays bytewise).
pub fn valid_sha256(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// What ensure_image must do for an image file, decided from on-disk state.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ImageDecision {
    /// File present and trusted (no expected sha, sidecar matches, or a
    /// pre-P4 locally-provisioned image without a sidecar) — use as-is.
    UseExisting,
    /// Download (file missing with a sha, or sidecar mismatches the sha).
    Pull,
}

/// Pull-or-use decision for `{path}` (+ `{path}.sha256` sidecar):
/// - file exists, no expected sha → use as-is (base images stay trusted)
/// - file exists, sidecar == expected sha → use as-is
/// - file exists, sidecar != expected sha → pull (re-fetch the right bytes)
/// - file exists, sidecar missing/unreadable, expected sha given → hash the
///   file (sha256sum, same tool pull verification uses): match → write the
///   sidecar (self-heal) and use; mismatch → pull. Trusting it blindly would
///   pin a stale image forever after a crashed pull.
/// - file missing, expected sha given → pull
/// - file missing, no sha → Err (base images come from the deploy script)
pub async fn decide(path: &str, expected_sha: Option<&str>, image: &str) -> Result<ImageDecision, String> {
    let exists = std::path::Path::new(path).exists();
    if exists {
        let Some(want) = expected_sha else {
            return Ok(ImageDecision::UseExisting);
        };
        let sidecar = format!("{}.sha256", path);
        match std::fs::read_to_string(&sidecar) {
            Ok(s) => {
                if s.trim() == want {
                    Ok(ImageDecision::UseExisting)
                } else {
                    Ok(ImageDecision::Pull)
                }
            }
            // No (readable) sidecar but a pinned sha: hash the existing file.
            Err(_) => {
                let got = sha256_of(path).await?;
                if got == want {
                    write_sidecar(path, want)?;
                    Ok(ImageDecision::UseExisting)
                } else {
                    Ok(ImageDecision::Pull)
                }
            }
        }
    } else if expected_sha.is_some() {
        Ok(ImageDecision::Pull)
    } else {
        Err(format!(
            "image {} not present and no sha256 provided (base images are provisioned by deploy/firecracker-assets.sh)",
            image
        ))
    }
}

/// Publish `{path}.sha256` atomically (temp + rename) so a crash never
/// leaves a truncated sidecar.
pub(crate) fn write_sidecar(path: &str, sha: &str) -> Result<(), String> {
    let sidecar = format!("{}.sha256", path);
    let tmp = format!("{}.tmp", sidecar);
    std::fs::write(&tmp, format!("{}\n", sha))
        .map_err(|e| format!("write {}: {}", tmp, e))?;
    std::fs::rename(&tmp, &sidecar)
        .map_err(|e| format!("rename {}: {}", sidecar, e))
}

/// Grow target for the per-instance rootfs copy. `disk_gb == 0` means "no
/// resize". Returns Ok(None) when nothing must change, Ok(Some(bytes)) when
/// the file must grow, Err on a shrink request (shrinking ext4 in place is
/// not supported).
pub fn resize_target(disk_gb: u64, current_size: u64) -> Result<Option<u64>, String> {
    if disk_gb == 0 {
        return Ok(None);
    }
    let target = disk_gb * 1024 * 1024 * 1024;
    if target < current_size {
        return Err("disk shrink not supported".into());
    }
    if target == current_size {
        return Ok(None);
    }
    Ok(Some(target))
}

/// Build the curl config for a one-shot image download. Passed on STDIN via
/// `curl --config -` so the bearer token never appears on argv (argv is
/// world-readable through /proc). `fail` turns HTTP >= 400 into a curl
/// error; no `location` line, so redirects are NOT followed (the control
/// plane serves the bytes directly).
pub fn build_download_config(url: &str, token: &str, out_path: &str) -> String {
    format!(
        concat!(
            "url = \"{}\"\n",
            "header = \"Authorization: Bearer {}\"\n",
            "output = \"{}\"\n",
            "fail\n",
            "connect-timeout = 5\n",
            "max-time = 1800\n",
        ),
        curl_config_escape(url),
        curl_config_escape(token),
        curl_config_escape(out_path),
    )
}

/// Download `url` to `out_path` with `curl -sS --config -` (config, token
/// included, piped on stdin). Async so a long pull never blocks the runtime;
/// the caller holds the Manager's image lock to serialize downloads.
pub async fn curl_download(url: &str, token: &str, out_path: &str) -> Result<(), String> {
    use tokio::io::AsyncWriteExt;
    let config = build_download_config(url, token, out_path);
    let mut child = tokio::process::Command::new("curl")
        .args(["-sS", "--config", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|e| format!("curl spawn: {}", e))?;
    {
        let mut stdin = child.stdin.take().ok_or("curl: no stdin")?;
        stdin
            .write_all(config.as_bytes())
            .await
            .map_err(|e| format!("curl stdin: {}", e))?;
        // Dropping the handle closes the pipe so curl sees EOF on its config.
    }
    let out = child
        .wait_with_output()
        .await
        .map_err(|e| format!("curl: {}", e))?;
    if !out.status.success() {
        return Err(format!(
            "curl download failed ({}): {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(())
}

/// sha256 of a file via `sha256sum` (the host tool firecracker assets are
/// verified with), lowercase hex.
pub async fn sha256_of(path: &str) -> Result<String, String> {
    let out = tokio::process::Command::new("sha256sum")
        .arg(path)
        .stderr(Stdio::null())
        .output()
        .await
        .map_err(|e| format!("sha256sum: {}", e))?;
    if !out.status.success() {
        return Err(format!("sha256sum {} failed", path));
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let digest = stdout.split_whitespace().next().unwrap_or("").to_lowercase();
    if !valid_sha256(&digest) {
        return Err(format!("sha256sum {}: malformed output", path));
    }
    Ok(digest)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_image_name_validation_table() {
        // Accepted.
        for good in ["ubuntu-base", "docker-base", "odoo-v18", "a.b-c_d", "a", "x0"] {
            assert!(valid_image_name(good), "should accept {:?}", good);
        }
        // Rejected.
        let too_long = "a".repeat(65);
        for bad in ["", "..", ".hidden", "-x", "UPPER", "a/b", "a;b", too_long.as_str(),
                    "a..b", "a b", "a\\b", "a\"b", "a$b"] {
            assert!(!valid_image_name(bad), "should reject {:?}", bad);
        }
        // 64 chars is the inclusive max.
        assert!(valid_image_name(&"a".repeat(64)));
    }

    #[test]
    fn test_sha256_validation() {
        assert!(valid_sha256(&"a".repeat(64)));
        assert!(valid_sha256(&format!("{}{}", "0123456789abcdef".repeat(3), "0123456789abcdef")));
        assert!(!valid_sha256(""));
        assert!(!valid_sha256(&"a".repeat(63)));
        assert!(!valid_sha256(&"a".repeat(65)));
        // Uppercase hex rejected (sidecar comparison is bytewise).
        assert!(!valid_sha256(&"A".repeat(64)));
        // Non-hex char.
        assert!(!valid_sha256(&format!("{}g", "a".repeat(63))));
    }

    #[tokio::test]
    async fn test_decide_exists_no_sha_uses_existing() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("ubuntu-base.ext4");
        std::fs::write(&path, b"img").unwrap();
        let d = decide(path.to_str().unwrap(), None, "ubuntu-base").await.unwrap();
        assert_eq!(d, ImageDecision::UseExisting);
    }

    #[tokio::test]
    async fn test_decide_exists_sidecar_match_uses_existing() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("odoo-v18.ext4");
        let sha = "a".repeat(64);
        std::fs::write(&path, b"img").unwrap();
        std::fs::write(format!("{}.sha256", path.to_str().unwrap()), format!("{}\n", sha)).unwrap();
        let d = decide(path.to_str().unwrap(), Some(&sha), "odoo-v18").await.unwrap();
        assert_eq!(d, ImageDecision::UseExisting);
    }

    #[tokio::test]
    async fn test_decide_exists_sidecar_mismatch_pulls() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("odoo-v18.ext4");
        std::fs::write(&path, b"img").unwrap();
        std::fs::write(format!("{}.sha256", path.to_str().unwrap()), format!("{}\n", "b".repeat(64))).unwrap();
        let d = decide(path.to_str().unwrap(), Some(&"a".repeat(64)), "odoo-v18").await.unwrap();
        assert_eq!(d, ImageDecision::Pull);
    }

    #[tokio::test]
    async fn test_decide_no_sidecar_matching_hash_self_heals() {
        // File present, sidecar missing, sha given (a crashed pre-fix pull
        // left this state): the file hash matches → use it AND write the
        // sidecar so the next decide takes the fast path.
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("odoo-v18.ext4");
        std::fs::write(&path, b"img").unwrap();
        let sha = sha256_of(path.to_str().unwrap()).await.unwrap();
        let d = decide(path.to_str().unwrap(), Some(&sha), "odoo-v18").await.unwrap();
        assert_eq!(d, ImageDecision::UseExisting);
        let sidecar = std::fs::read_to_string(format!("{}.sha256", path.to_str().unwrap()))
            .expect("sidecar self-healed");
        assert_eq!(sidecar.trim(), sha);
    }

    #[tokio::test]
    async fn test_decide_no_sidecar_mismatching_hash_pulls() {
        // File present, sidecar missing, sha given, hash does NOT match:
        // the local file is stale — repull (never trust it forever).
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("odoo-v18.ext4");
        std::fs::write(&path, b"stale").unwrap();
        let d = decide(path.to_str().unwrap(), Some(&"a".repeat(64)), "odoo-v18").await.unwrap();
        assert_eq!(d, ImageDecision::Pull);
        // No sidecar may be written for a mismatch.
        assert!(!std::path::Path::new(&format!("{}.sha256", path.to_str().unwrap())).exists());
    }

    #[tokio::test]
    async fn test_decide_missing_with_sha_pulls() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("docker-base.ext4");
        let d = decide(path.to_str().unwrap(), Some(&"a".repeat(64)), "docker-base").await.unwrap();
        assert_eq!(d, ImageDecision::Pull);
    }

    #[tokio::test]
    async fn test_decide_missing_no_sha_is_err() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("ghost.ext4");
        let err = decide(path.to_str().unwrap(), None, "ghost").await.unwrap_err();
        assert!(err.contains("image ghost not present"), "got: {}", err);
        assert!(err.contains("no sha256 provided"), "got: {}", err);
    }

    #[tokio::test]
    async fn test_decide_sidecar_without_image_pulls() {
        // The pull order (sidecar first, then image) can crash into this
        // state — the missing-file path must repull.
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("odoo-v18.ext4");
        let sha = "a".repeat(64);
        std::fs::write(format!("{}.sha256", path.to_str().unwrap()), format!("{}\n", sha)).unwrap();
        let d = decide(path.to_str().unwrap(), Some(&sha), "odoo-v18").await.unwrap();
        assert_eq!(d, ImageDecision::Pull);
    }

    #[test]
    fn test_resize_target_zero_means_no_resize() {
        assert_eq!(resize_target(0, 999_999_999).unwrap(), None);
    }

    #[test]
    fn test_resize_target_grow() {
        // 2 GiB target over a 1 GiB file.
        let gib: u64 = 1024 * 1024 * 1024;
        assert_eq!(resize_target(2, gib).unwrap(), Some(2 * gib));
    }

    #[test]
    fn test_resize_target_equal_is_noop() {
        let gib: u64 = 1024 * 1024 * 1024;
        assert_eq!(resize_target(2, 2 * gib).unwrap(), None);
    }

    #[test]
    fn test_resize_target_shrink_rejected() {
        let gib: u64 = 1024 * 1024 * 1024;
        let err = resize_target(1, 2 * gib).unwrap_err();
        assert!(err.contains("disk shrink not supported"), "got: {}", err);
    }

    #[test]
    fn test_build_download_config_shape() {
        let cfg = build_download_config(
            "http://hub:8080/api/v1/images/odoo-v18",
            "tok123",
            "/srv/ignis/images/.odoo-v18.partial",
        );
        assert!(cfg.contains("url = \"http://hub:8080/api/v1/images/odoo-v18\""));
        assert!(cfg.contains("header = \"Authorization: Bearer tok123\""));
        assert!(cfg.contains("output = \"/srv/ignis/images/.odoo-v18.partial\""));
        assert!(cfg.contains("\nfail\n"));
        assert!(cfg.contains("connect-timeout = 5"));
        assert!(cfg.contains("max-time = 1800"));
        // Redirects are never followed.
        assert!(!cfg.contains("location"));
    }
}
