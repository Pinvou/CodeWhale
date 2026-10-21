//! Stable first-release host control. Exit is cooperative and never signals a PID.
use parking_lot::{RwLock, RwLockReadGuard};
use serde::{Deserialize, Serialize};
use std::sync::atomic::{AtomicBool, Ordering};

pub(super) const CAPABILITY: &str = "host.control.v1";
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Identity {
    pub instance: String,
    pub pid: u32,
    pub boot_id: String,
    pub start_ticks: String,
    pub version: String,
}
impl Identity {
    pub fn current() -> Self {
        let pid = std::process::id();
        Self {
            instance: uuid::Uuid::new_v4().to_string(),
            pid,
            boot_id: boot_id().unwrap_or_default(),
            start_ticks: start_ticks(pid).unwrap_or_default(),
            version: env!("CARGO_PKG_VERSION").into(),
        }
    }
    pub fn verify_peer(&self, pid: u32) -> Result<(), String> {
        if self.pid != pid
            || uuid::Uuid::parse_str(&self.instance).is_err()
            || self.boot_id.is_empty()
            || self.start_ticks.is_empty()
            || boot_id().as_deref() != Some(&self.boot_id)
            || start_ticks(pid).as_deref() != Some(&self.start_ticks)
        {
            return Err("store_protocol: host identity unverified".into());
        }
        Ok(())
    }
}
fn boot_id() -> Option<String> {
    Some(
        std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
            .ok()?
            .trim()
            .into(),
    )
}
fn start_ticks(pid: u32) -> Option<String> {
    Some(
        std::fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()?
            .rsplit_once(") ")?
            .1
            .split_whitespace()
            .nth(19)?
            .into(),
    )
}

pub(super) struct HostControl {
    pub identity: Identity,
    requests: RwLock<()>,
    retiring: AtomicBool,
    exit_requested: AtomicBool,
}
impl HostControl {
    pub fn new() -> Self {
        Self {
            identity: Identity::current(),
            requests: RwLock::new(()),
            retiring: AtomicBool::new(false),
            exit_requested: AtomicBool::new(false),
        }
    }
    /// Held through the final response write, including long polls and config I/O.
    pub fn request(&self) -> Result<RwLockReadGuard<'_, ()>, String> {
        let guard = self.requests.try_read().ok_or("store_host_draining")?;
        self.ensure_accepting()?;
        Ok(guard)
    }
    pub fn ensure_accepting(&self) -> Result<(), String> {
        if self.retiring.load(Ordering::Acquire) {
            Err("store_host_draining".into())
        } else {
            Ok(())
        }
    }
    pub fn retire(
        &self,
        expected: &Identity,
        idle: impl FnOnce() -> Result<(), String>,
    ) -> Result<(), String> {
        if expected != &self.identity {
            return Err("store_host_generation_changed".into());
        }
        let _gate = self
            .requests
            .try_write()
            .ok_or("store_host_busy: IPC request in progress")?;
        if self.retiring.load(Ordering::Acquire) {
            return Ok(());
        }
        idle()?;
        self.retiring.store(true, Ordering::Release);
        Ok(())
    }
    /// The retirement response has been written or its connection has failed.
    pub fn response_finished(&self) {
        self.exit_requested.store(true, Ordering::Release);
    }
    pub fn should_exit(&self) -> bool {
        self.exit_requested.load(Ordering::Acquire)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn drain_is_atomic_with_ipc_and_does_not_revoke_busy_work() {
        let host = HostControl::new();
        let request = host.request().unwrap();
        assert!(
            host.retire(&host.identity, || Ok(()))
                .unwrap_err()
                .contains("busy")
        );
        drop(request);
        assert!(
            host.retire(&host.identity, || Err("store_host_busy: task".into()))
                .is_err()
        );
        assert!(host.request().is_ok());
        let mut other = host.identity.clone();
        other.instance = uuid::Uuid::new_v4().to_string();
        assert!(host.retire(&other, || Ok(())).is_err());
        host.retire(&host.identity, || Ok(())).unwrap();
        assert!(host.request().is_err());
        assert!(!host.should_exit());
        host.response_finished();
        assert!(host.should_exit());
    }
}
