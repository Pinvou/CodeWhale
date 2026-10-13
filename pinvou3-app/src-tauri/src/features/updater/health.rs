use std::path::{Path, PathBuf};
use std::sync::Mutex;

use chrono::Utc;
use serde::{Deserialize, Serialize};

const UPDATE_HEALTH_SCHEMA_VERSION: u32 = 1;
const UPDATE_HEALTH_FILE: &str = "update-health.json";
const MAX_UPDATE_HEALTH_BYTES: u64 = 16 * 1024;
const MAX_VERSION_CHARS: usize = 128;
static UPDATE_HEALTH_LOCK: Mutex<()> = Mutex::new(());

#[derive(Debug, Clone, Copy, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
enum UpdateHealthPhase {
    Pending,
    Ready,
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct UpdateHealthRecord {
    schema_version: u32,
    transaction_id: String,
    from_version: String,
    target_version: String,
    platform: String,
    phase: UpdateHealthPhase,
    installed_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    ready_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct UpdateReadyCommitResult {
    pub had_pending: bool,
    pub committed: bool,
    pub backup_cleanup_attempted: bool,
    pub result: String,
}

impl UpdateReadyCommitResult {
    pub(super) fn ignored_window() -> Self {
        Self {
            had_pending: false,
            committed: false,
            backup_cleanup_attempted: false,
            result: "ignored_non_main_window".to_string(),
        }
    }
}

fn health_path() -> PathBuf {
    crate::platform::paths::updates_dir().join(UPDATE_HEALTH_FILE)
}

fn normalize_version(value: &str) -> Result<String, String> {
    let normalized = value.trim().trim_start_matches(['v', 'V']);
    if normalized.is_empty()
        || normalized.chars().count() > MAX_VERSION_CHARS
        || normalized.chars().any(char::is_whitespace)
    {
        return Err("更新健康状态包含无效版本号".to_string());
    }
    Ok(normalized.to_string())
}

fn write_record(path: &Path, record: &UpdateHealthRecord) -> Result<(), String> {
    let parent = path
        .parent()
        .ok_or_else(|| "更新健康状态路径没有父目录".to_string())?;
    std::fs::create_dir_all(parent).map_err(|error| format!("创建更新状态目录失败: {error}"))?;
    let payload = serde_json::to_vec_pretty(record)
        .map_err(|error| format!("序列化更新健康状态失败: {error}"))?;
    crate::platform::filesystem::atomic_write_private(path, &payload)
        .map_err(|error| format!("写入更新健康状态失败: {error}"))
}

fn read_record(path: &Path) -> Result<Option<UpdateHealthRecord>, String> {
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(format!("读取更新健康状态元数据失败: {error}")),
    };
    if metadata.file_type().is_symlink() || !metadata.file_type().is_file() {
        return Err("更新健康状态必须是普通非符号链接文件".to_string());
    }
    if metadata.len() > MAX_UPDATE_HEALTH_BYTES {
        return Err(format!(
            "更新健康状态超过 {} bytes 上限",
            MAX_UPDATE_HEALTH_BYTES
        ));
    }
    let payload = std::fs::read(path).map_err(|error| format!("读取更新健康状态失败: {error}"))?;
    let record: UpdateHealthRecord = serde_json::from_slice(&payload)
        .map_err(|error| format!("解析更新健康状态失败: {error}"))?;
    validate_record(record).map(Some)
}

fn validate_record(mut record: UpdateHealthRecord) -> Result<UpdateHealthRecord, String> {
    if record.schema_version != UPDATE_HEALTH_SCHEMA_VERSION {
        return Err(format!(
            "不支持的更新健康状态版本: {}",
            record.schema_version
        ));
    }
    record.from_version = normalize_version(&record.from_version)?;
    record.target_version = normalize_version(&record.target_version)?;
    if record.transaction_id.trim().is_empty()
        || record.transaction_id.len() > 64
        || record.platform.trim().is_empty()
        || record.platform.len() > 32
        || record.installed_at.trim().is_empty()
    {
        return Err("更新健康状态字段无效".to_string());
    }
    Ok(record)
}

pub(super) fn record_pending_update(
    from_version: &str,
    target_version: &str,
    platform: &str,
) -> Result<(), String> {
    let _guard = UPDATE_HEALTH_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let record = UpdateHealthRecord {
        schema_version: UPDATE_HEALTH_SCHEMA_VERSION,
        transaction_id: uuid::Uuid::new_v4().simple().to_string(),
        from_version: normalize_version(from_version)?,
        target_version: normalize_version(target_version)?,
        platform: platform.to_string(),
        phase: UpdateHealthPhase::Pending,
        installed_at: Utc::now().to_rfc3339(),
        ready_at: None,
    };
    write_record(&health_path(), &record)
}

pub(super) fn observe_pending_update(current_version: &str, platform: &str) {
    match read_record(&health_path()) {
        Ok(None) => {}
        Ok(Some(record)) => crate::platform::startup::mark_with_detail(
            "rust",
            "update_health:observed",
            &format!(
                "phase={:?} from={} target={} current={} platform={} current_platform={}",
                record.phase,
                record.from_version,
                record.target_version,
                current_version,
                record.platform,
                platform,
            ),
        ),
        Err(error) => crate::platform::startup::mark_with_detail(
            "rust",
            "update_health:invalid",
            &format!("preserving recovery artifacts: {error}"),
        ),
    }
}

pub(super) fn commit_ready<F>(
    current_version: &str,
    platform: &str,
    cleanup_backup: F,
) -> Result<UpdateReadyCommitResult, String>
where
    F: FnMut() -> Result<(), String>,
{
    commit_ready_at(&health_path(), current_version, platform, cleanup_backup)
}

fn commit_ready_at<F>(
    path: &Path,
    current_version: &str,
    platform: &str,
    mut cleanup_backup: F,
) -> Result<UpdateReadyCommitResult, String>
where
    F: FnMut() -> Result<(), String>,
{
    let _guard = UPDATE_HEALTH_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let current_version = normalize_version(current_version)?;
    let Some(mut record) = read_record(path)? else {
        // Builds predating the marker still leave a macOS backup. Delay this
        // compatibility cleanup to first commit instead of setup.
        cleanup_backup()?;
        return Ok(UpdateReadyCommitResult {
            had_pending: false,
            committed: false,
            backup_cleanup_attempted: true,
            result: "no_pending".to_string(),
        });
    };
    if record.platform != platform {
        return Ok(UpdateReadyCommitResult {
            had_pending: true,
            committed: false,
            backup_cleanup_attempted: false,
            result: "platform_mismatch".to_string(),
        });
    }
    if record.target_version != current_version {
        return Ok(UpdateReadyCommitResult {
            had_pending: true,
            committed: false,
            backup_cleanup_attempted: false,
            result: "target_mismatch".to_string(),
        });
    }

    let was_pending = record.phase == UpdateHealthPhase::Pending;
    if was_pending {
        record.phase = UpdateHealthPhase::Ready;
        record.ready_at = Some(Utc::now().to_rfc3339());
        write_record(path, &record)?;
    }
    // Persist ready before deleting the rollback anchor. A crash or cleanup
    // failure leaves an idempotent ready marker for the next first commit.
    cleanup_backup()?;
    match std::fs::remove_file(path) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(format!("清理已提交的更新健康状态失败: {error}")),
    }
    Ok(UpdateReadyCommitResult {
        had_pending: true,
        committed: true,
        backup_cleanup_attempted: true,
        result: if was_pending {
            "ready".to_string()
        } else {
            "ready_recovered".to_string()
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn record(from: &str, target: &str, platform: &str) -> UpdateHealthRecord {
        UpdateHealthRecord {
            schema_version: UPDATE_HEALTH_SCHEMA_VERSION,
            transaction_id: "txn-1".to_string(),
            from_version: from.to_string(),
            target_version: target.to_string(),
            platform: platform.to_string(),
            phase: UpdateHealthPhase::Pending,
            installed_at: "2026-10-08T00:00:00Z".to_string(),
            ready_at: None,
        }
    }

    #[test]
    fn missing_marker_only_runs_legacy_cleanup_after_first_commit() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(UPDATE_HEALTH_FILE);
        let cleanups = AtomicUsize::new(0);
        let result = commit_ready_at(&path, "1.2.3", "linux", || {
            cleanups.fetch_add(1, Ordering::Relaxed);
            Ok(())
        })
        .unwrap();
        assert_eq!(result.result, "no_pending");
        assert!(!result.had_pending);
        assert_eq!(cleanups.load(Ordering::Relaxed), 1);
        assert!(!path.exists());
    }

    #[test]
    fn target_or_platform_mismatch_preserves_marker_and_backup() {
        for (current, platform, expected) in [
            ("1.0.0", "macos", "target_mismatch"),
            ("2.0.0", "windows", "platform_mismatch"),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(UPDATE_HEALTH_FILE);
            write_record(&path, &record("1.0.0", "2.0.0", "macos")).unwrap();
            let cleanups = AtomicUsize::new(0);
            let result = commit_ready_at(&path, current, platform, || {
                cleanups.fetch_add(1, Ordering::Relaxed);
                Ok(())
            })
            .unwrap();
            assert_eq!(result.result, expected);
            assert!(!result.committed);
            assert_eq!(cleanups.load(Ordering::Relaxed), 0);
            assert!(path.exists());
        }
    }

    #[test]
    fn matching_first_commit_marks_ready_before_cleaning_and_removes_marker() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(UPDATE_HEALTH_FILE);
        write_record(&path, &record("1.0.0", "v2.0.0", "macos")).unwrap();
        let result = commit_ready_at(&path, "2.0.0", "macos", || {
            let persisted = read_record(&path)?.expect("ready marker must exist during cleanup");
            assert_eq!(persisted.phase, UpdateHealthPhase::Ready);
            assert!(persisted.ready_at.is_some());
            Ok(())
        })
        .unwrap();
        assert!(result.committed);
        assert_eq!(result.result, "ready");
        assert!(!path.exists());
    }

    #[test]
    fn cleanup_failure_keeps_ready_marker_for_idempotent_retry() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(UPDATE_HEALTH_FILE);
        write_record(&path, &record("1.0.0", "2.0.0", "macos")).unwrap();
        let error = commit_ready_at(&path, "2.0.0", "macos", || {
            Err("simulated cleanup failure".to_string())
        })
        .unwrap_err();
        assert!(error.contains("simulated cleanup failure"));
        assert_eq!(
            read_record(&path).unwrap().unwrap().phase,
            UpdateHealthPhase::Ready
        );
        let retry = commit_ready_at(&path, "2.0.0", "macos", || Ok(())).unwrap();
        assert_eq!(retry.result, "ready_recovered");
        assert!(!path.exists());
    }

    #[test]
    fn malformed_or_oversized_marker_fails_closed_without_cleanup() {
        let dir = tempfile::tempdir().unwrap();
        for payload in [
            b"not-json".to_vec(),
            vec![b'x'; MAX_UPDATE_HEALTH_BYTES as usize + 1],
        ] {
            let path = dir.path().join(format!("state-{}", payload.len()));
            std::fs::write(&path, payload).unwrap();
            let cleanups = AtomicUsize::new(0);
            assert!(
                commit_ready_at(&path, "2.0.0", "macos", || {
                    cleanups.fetch_add(1, Ordering::Relaxed);
                    Ok(())
                })
                .is_err()
            );
            assert_eq!(cleanups.load(Ordering::Relaxed), 0);
            assert!(path.exists());
        }
    }

    #[test]
    fn missing_marker_probe_reports_manual_latency_distribution() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(UPDATE_HEALTH_FILE);
        let mut samples = Vec::with_capacity(2_000);
        for _ in 0..2_000 {
            let started = std::time::Instant::now();
            assert!(read_record(&path).unwrap().is_none());
            samples.push(started.elapsed().as_nanos() as u64);
        }
        samples.sort_unstable();
        eprintln!(
            "update_health_missing_marker_ns p50={} p95={} max={} samples={}",
            samples[samples.len() / 2],
            samples[samples.len() * 95 / 100],
            samples[samples.len() - 1],
            samples.len()
        );
    }
}
