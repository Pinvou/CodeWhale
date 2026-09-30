//! Best-effort startup timeline shared by the Rust backend and WebView frontend.
//!
//! Windows release builds do not have a console, so startup diagnostics must be
//! persisted.  Runs are appended to one bounded file and contain no credentials
//! or user content.

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::Path;
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

use chrono::Utc;
use serde::{Deserialize, Serialize};

const MAX_LOG_BYTES: u64 = 2 * 1024 * 1024;
static STARTED_AT: OnceLock<Instant> = OnceLock::new();
static LOG_FILE: OnceLock<Mutex<Option<File>>> = OnceLock::new();
static RUN_ID: OnceLock<String> = OnceLock::new();
static LIFECYCLE_RECEIPT: OnceLock<Mutex<Option<LifecycleReceipt>>> = OnceLock::new();

const LIFECYCLE_SCHEMA_VERSION: u32 = 1;

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
enum LifecycleState {
    Starting,
    Ready,
    CleanExit,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct LifecycleReceipt {
    schema_version: u32,
    run_id: String,
    pid: u32,
    state: LifecycleState,
    started_at: String,
    updated_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    exit_reason: Option<String>,
}

struct RotationInfo {
    previous_bytes: u64,
    rotated: bool,
    error: Option<String>,
}

fn elapsed() -> Duration {
    STARTED_AT.get_or_init(Instant::now).elapsed()
}

pub fn elapsed_ms() -> u64 {
    elapsed().as_millis().min(u128::from(u64::MAX)) as u64
}

fn clean_field(value: &str) -> String {
    crate::platform::credential_store::redact_secret(value)
        .chars()
        .map(|c| {
            if matches!(c, '\r' | '\n' | '\t') {
                ' '
            } else {
                c
            }
        })
        .take(500)
        .collect()
}

fn write_line(source: &str, stage: &str, elapsed_ms: f64, detail: Option<&str>) {
    let source = clean_field(source);
    let stage = clean_field(stage);
    let detail = detail.map(clean_field).unwrap_or_default();
    let line = format!(
        "{} +{:>10.3}ms [run={}] [{}] {}{}\n",
        Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
        elapsed_ms,
        RUN_ID.get().map(String::as_str).unwrap_or("unknown"),
        source,
        stage,
        if detail.is_empty() {
            String::new()
        } else {
            format!(" | {detail}")
        }
    );
    eprint!("[startup] {line}");
    if let Some(lock) = LOG_FILE.get() {
        if let Ok(mut file) = lock.lock() {
            if let Some(file) = file.as_mut() {
                let _ = file.write_all(line.as_bytes());
                let _ = file.flush();
            }
        }
    }
}

fn rotate_if_oversized(path: &Path) -> RotationInfo {
    let previous_bytes = std::fs::metadata(path).map(|m| m.len()).unwrap_or(0);
    if previous_bytes <= MAX_LOG_BYTES {
        return RotationInfo {
            previous_bytes,
            rotated: false,
            error: None,
        };
    }
    // A concurrent disappearance between the two stats is "nothing to
    // rotate": the log is gone either way.
    match crate::platform::filesystem::rotate_log_if_oversized(path, MAX_LOG_BYTES, None) {
        Ok(_) => RotationInfo {
            previous_bytes,
            rotated: true,
            error: None,
        },
        Err(error) => RotationInfo {
            previous_bytes,
            rotated: false,
            error: Some(error.to_string()),
        },
    }
}

fn lifecycle_receipt_path() -> std::path::PathBuf {
    crate::platform::paths::pinvou3_home()
        .join("logs")
        .join("app-lifecycle.json")
}

fn load_lifecycle_receipt(path: &Path) -> Result<Option<LifecycleReceipt>, String> {
    match std::fs::read(path) {
        Ok(bytes) => {
            let receipt: LifecycleReceipt = serde_json::from_slice(&bytes)
                .map_err(|error| format!("decode {}: {error}", path.display()))?;
            if receipt.schema_version != LIFECYCLE_SCHEMA_VERSION {
                return Err(format!(
                    "decode {}: unsupported schema version {}",
                    path.display(),
                    receipt.schema_version
                ));
            }
            Ok(Some(receipt))
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(format!("read {}: {error}", path.display())),
    }
}

fn persist_lifecycle_receipt(path: &Path, receipt: &LifecycleReceipt) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|error| format!("create {}: {error}", parent.display()))?;
    }
    let payload = serde_json::to_vec_pretty(receipt)
        .map_err(|error| format!("encode lifecycle receipt: {error}"))?;
    crate::platform::filesystem::atomic_write_private(path, &payload)
        .map_err(|error| format!("write {}: {error}", path.display()))
}

/// Starts the durable host lifecycle only after Tauri has built the app. This
/// placement lets the single-instance plugin reject a secondary process before
/// it can overwrite the primary process's receipt.
pub fn begin_lifecycle() {
    let path = lifecycle_receipt_path();
    match load_lifecycle_receipt(&path) {
        Ok(Some(previous)) if previous.state != LifecycleState::CleanExit => {
            mark_with_detail(
                "rust",
                "lifecycle:previous_unclean",
                &format!("pid={} state={:?}", previous.pid, previous.state),
            );
        }
        Ok(Some(previous)) => mark_with_detail(
            "rust",
            "lifecycle:previous_clean",
            &format!("pid={}", previous.pid),
        ),
        Ok(None) => mark("lifecycle:previous_missing"),
        Err(error) => mark_with_detail("rust", "lifecycle:previous_invalid", &error),
    }

    let now = Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
    let receipt = LifecycleReceipt {
        schema_version: LIFECYCLE_SCHEMA_VERSION,
        run_id: RUN_ID
            .get()
            .cloned()
            .unwrap_or_else(|| format!("unknown-{}", std::process::id())),
        pid: std::process::id(),
        state: LifecycleState::Starting,
        started_at: now.clone(),
        updated_at: now,
        exit_reason: None,
    };
    if let Err(error) = persist_lifecycle_receipt(&path, &receipt) {
        mark_with_detail("rust", "lifecycle:write_error", &error);
    } else {
        mark("lifecycle:starting");
    }
    let slot = LIFECYCLE_RECEIPT.get_or_init(|| Mutex::new(None));
    if let Ok(mut current) = slot.lock() {
        *current = Some(receipt);
    }
}

fn transition_lifecycle(state: LifecycleState, exit_reason: Option<&str>) {
    let Some(slot) = LIFECYCLE_RECEIPT.get() else {
        return;
    };
    let Ok(mut current) = slot.lock() else {
        mark("lifecycle:lock_poisoned");
        return;
    };
    let Some(receipt) = current.as_mut() else {
        return;
    };
    receipt.state = state;
    receipt.updated_at = Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
    receipt.exit_reason = exit_reason.map(str::to_string);
    if let Err(error) = persist_lifecycle_receipt(&lifecycle_receipt_path(), receipt) {
        mark_with_detail("rust", "lifecycle:write_error", &error);
    }
}

pub fn mark_lifecycle_ready() {
    transition_lifecycle(LifecycleState::Ready, None);
    mark("lifecycle:ready");
}

pub fn mark_lifecycle_clean_exit(reason: &str) {
    transition_lifecycle(LifecycleState::CleanExit, Some(reason));
    mark_with_detail("rust", "lifecycle:clean_exit", &format!("reason={reason}"));
}


/// Open the startup log under the current `PINVOU3_HOME` and stamp
/// `process:start`. Safe to call again: a re-init re-points the sink at the
/// freshly opened log instead of being silently dropped, so every caller's
/// marks land in the log it just opened (tests re-init under a fresh home;
/// production inits once). A re-init whose open fails keeps the previous
/// sink — marks keep landing somewhere real instead of vanishing.
pub fn init() {
    STARTED_AT.get_or_init(Instant::now);
    let started_utc = Utc::now();
    let _ = RUN_ID.set(format!(
        "{}-{}",
        started_utc.format("%Y%m%dT%H%M%S%.3fZ"),
        std::process::id()
    ));
    let path = crate::platform::paths::pinvou3_home()
        .join("logs")
        .join("startup.log");
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let rotation = rotate_if_oversized(&path);
    let file = path.parent().and_then(|parent| {
        std::fs::create_dir_all(parent).ok()?;
        OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .ok()
    });
    if let (Some(file), Ok(mut slot)) = (file, LOG_FILE.get_or_init(|| Mutex::new(None)).lock()) {
        *slot = Some(file);
    }
    mark_with_detail(
        "rust",
        "process:start",
        &format!(
            "pid={} previous_log_bytes={} log_rotated={} rotation_error={}",
            std::process::id(),
            rotation.previous_bytes,
            rotation.rotated,
            rotation.error.as_deref().unwrap_or("none"),
        ),
    );
    mark_with_detail(
        "rust",
        "log:ready",
        &format!(
            "path={} max_bytes={MAX_LOG_BYTES} mode=append",
            path.display()
        ),
    );
}

pub fn mark(stage: &str) {
    write_line("rust", stage, elapsed().as_secs_f64() * 1000.0, None);
}

pub fn mark_with_detail(source: &str, stage: &str, detail: &str) {
    write_line(
        source,
        stage,
        elapsed().as_secs_f64() * 1000.0,
        Some(detail),
    );
}

#[derive(Debug, Deserialize)]
pub struct FrontendStartupEntry {
    stage: String,
    #[serde(default)]
    detail: String,
    since_navigation_ms: f64,
}

/// Receive batched WebView performance marks.  The frontend supplies offsets
/// from `performance.timeOrigin`; keeping that clock separate from the Rust
/// process clock makes WebView/Babel stalls immediately visible.
pub fn report_frontend_startup(entries: Vec<FrontendStartupEntry>) {
    for entry in entries {
        write_line(
            "webview",
            &entry.stage,
            entry.since_navigation_ms.max(0.0),
            (!entry.detail.is_empty()).then_some(entry.detail.as_str()),
        );
    }
}

#[cfg(test)]
mod tests {
    use super::{
        LIFECYCLE_SCHEMA_VERSION, LifecycleReceipt, LifecycleState, MAX_LOG_BYTES, clean_field,
        load_lifecycle_receipt, persist_lifecycle_receipt, rotate_if_oversized,
    };

    #[test]
    fn startup_log_fields_are_single_line_and_bounded() {
        let dirty = format!("a\nb\tc\r{}", "x".repeat(600));
        let clean = clean_field(&dirty);
        assert!(!clean.contains(['\n', '\r', '\t']));
        assert_eq!(clean.chars().count(), 500);
    }

    #[test]
    fn startup_log_limit_is_exactly_two_mib() {
        assert_eq!(MAX_LOG_BYTES, 2_097_152);
    }

    #[test]
    fn oversized_startup_log_is_deleted_before_open() {
        let root =
            std::env::temp_dir().join(format!("pinvou3-startup-rotate-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(&root).unwrap();
        let log = root.join("startup.log");
        let file = std::fs::File::create(&log).unwrap();
        file.set_len(MAX_LOG_BYTES + 1).unwrap();

        let result = rotate_if_oversized(&log);
        assert_eq!(result.previous_bytes, MAX_LOG_BYTES + 1);
        assert!(result.rotated);
        assert!(!log.exists());
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn lifecycle_receipt_distinguishes_unclean_and_clean_runs() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("logs/app-lifecycle.json");
        let mut receipt = LifecycleReceipt {
            schema_version: LIFECYCLE_SCHEMA_VERSION,
            run_id: "run-1".to_string(),
            pid: 42,
            state: LifecycleState::Starting,
            started_at: "2026-09-30T01:00:00.000Z".to_string(),
            updated_at: "2026-09-30T01:00:00.000Z".to_string(),
            exit_reason: None,
        };

        persist_lifecycle_receipt(&path, &receipt).unwrap();
        assert_eq!(
            load_lifecycle_receipt(&path).unwrap().unwrap().state,
            LifecycleState::Starting
        );

        receipt.state = LifecycleState::Ready;
        receipt.updated_at = "2026-09-30T01:00:01.000Z".to_string();
        persist_lifecycle_receipt(&path, &receipt).unwrap();
        assert_eq!(
            load_lifecycle_receipt(&path).unwrap().unwrap().state,
            LifecycleState::Ready
        );

        receipt.state = LifecycleState::CleanExit;
        receipt.updated_at = "2026-09-30T01:10:00.000Z".to_string();
        receipt.exit_reason = Some("exit_event".to_string());
        persist_lifecycle_receipt(&path, &receipt).unwrap();
        assert_eq!(load_lifecycle_receipt(&path).unwrap(), Some(receipt));
    }

    #[test]
    fn invalid_lifecycle_receipt_is_reported_instead_of_treated_as_clean() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("app-lifecycle.json");
        std::fs::write(&path, b"{truncated").unwrap();

        assert!(load_lifecycle_receipt(&path).is_err());
    }

    #[test]
    fn unknown_lifecycle_schema_is_not_trusted_as_a_clean_exit() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("app-lifecycle.json");
        std::fs::write(
            &path,
            br#"{
                "schema_version": 2,
                "run_id": "future-run",
                "pid": 42,
                "state": "clean_exit",
                "started_at": "2026-09-30T01:00:00.000Z",
                "updated_at": "2026-09-30T01:10:00.000Z",
                "exit_reason": "exit_event"
            }"#,
        )
        .unwrap();

        assert!(load_lifecycle_receipt(&path).is_err());
    }
}
