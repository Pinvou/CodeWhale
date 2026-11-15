//! Read-only compatibility guard for scripts started before inherited download leases.
//! Receipts are additional evidence of live writers, never an absence proof for
//! unregistered external jobs. No discovered process authorizes sending signals.
use super::{RunRecord, lock, record_file};
use std::fs;
use std::time::{Duration, Instant};

const UNKNOWN: &str = "无法确认下载脚本是否已退出，已保留容量预留，请检查残留任务后重试。";

struct Identity {
    state: u8,
    pgid: i32,
    starttime: u64,
}

fn identity(pid: i32) -> Result<Option<Identity>, String> {
    let raw = match fs::read_to_string(format!("/proc/{pid}/stat")) {
        Ok(raw) => raw,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(UNKNOWN.into()),
    };
    let end = raw.rfind(')').ok_or(UNKNOWN)?;
    let fields = raw[end + 1..].split_whitespace().collect::<Vec<_>>();
    let state = fields
        .first()
        .and_then(|value| value.as_bytes().first())
        .copied()
        .ok_or(UNKNOWN)?;
    let pgid = fields.get(2).ok_or(UNKNOWN)?.parse().map_err(|_| UNKNOWN)?;
    let starttime = fields
        .get(19)
        .ok_or(UNKNOWN)?
        .parse()
        .map_err(|_| UNKNOWN)?;
    Ok(Some(Identity {
        state,
        pgid,
        starttime,
    }))
}

fn alive(identity: &Identity) -> bool {
    !matches!(identity.state, b'Z' | b'X' | b'x')
}

fn has_descendants(record: &RunRecord, deadline: Instant) -> Result<bool, String> {
    if record.pgid != record.pid {
        // Old privileged launchers may share the app's group. A vanished
        // launcher cannot prove that its elevated descendants have stopped.
        return Err(UNKNOWN.into());
    }
    for entry in fs::read_dir("/proc").map_err(|_| UNKNOWN)? {
        if Instant::now() > deadline {
            return Err(UNKNOWN.into());
        }
        let entry = entry.map_err(|_| UNKNOWN)?;
        let Ok(pid) = entry.file_name().to_string_lossy().parse::<i32>() else {
            continue;
        };
        if let Some(current) = identity(pid)? {
            if current.pgid == record.pgid
                && current.starttime >= record.starttime
                && alive(&current)
            {
                return Ok(true);
            }
        }
    }
    Ok(false)
}

/// Inspect old receipts while the caller holds the card and download leases.
/// Missing receipts cannot describe a task; malformed/unreadable receipts or
/// uncertain process identity refuse recovery instead of silently clearing it.
pub(in super::super) fn is_recovery_blocked(card_id: &str) -> Result<bool, String> {
    let _guard = lock();
    let raw = match fs::read_to_string(record_file()) {
        Ok(raw) => raw,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err(UNKNOWN.into()),
    };
    let records: Vec<RunRecord> = serde_json::from_str(&raw).map_err(|_| UNKNOWN)?;
    let relevant = records.into_iter().filter(|record| {
        record
            .card_id
            .as_deref()
            .is_none_or(|id| id.is_empty() || id == card_id)
    });
    let boot = fs::read_to_string("/proc/sys/kernel/random/boot_id").map_err(|_| UNKNOWN)?;
    let boot = uuid::Uuid::parse_str(boot.trim()).map_err(|_| UNKNOWN)?;
    let deadline = Instant::now() + Duration::from_secs(5);
    for record in relevant {
        if !record.boot_id.is_empty() {
            let recorded_boot = uuid::Uuid::parse_str(&record.boot_id).map_err(|_| UNKNOWN)?;
            if recorded_boot != boot {
                continue;
            }
        }
        if record.pid <= 1 || record.pgid <= 0 || record.starttime == 0 {
            return Err(UNKNOWN.into());
        }
        if Instant::now() > deadline {
            return Err(UNKNOWN.into());
        }
        if let Some(current) = identity(record.pid)? {
            if current.starttime != record.starttime {
                continue;
            }
            if alive(&current) {
                return Ok(true);
            }
        }
        if has_descendants(&record, deadline)? {
            return Ok(true);
        }
    }
    Ok(false)
}

#[cfg(test)]
mod tests;
