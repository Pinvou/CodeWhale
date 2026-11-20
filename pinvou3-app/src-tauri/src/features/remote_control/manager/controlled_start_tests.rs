//! Test-only inspection of manager-owned state; host adaptation stays in app.
use super::*;

impl RemoteControlManager {
    pub(crate) fn assert_controller_lane_absent_for_test(&self) {
        let inner = self.inner.lock();
        assert!(inner.controller_inbound.is_none());
        assert!(inner.controller_relay_generation.is_none());
        assert!(inner.controller_binding.is_none());
    }

    pub(crate) fn assert_controlled_start_absent_for_test(&self) {
        assert!(load_config().unwrap().is_none());
        assert!(!persistence::rpc_ledger_path().exists());
        assert!(self.inner.lock().endpoint.is_none());
        self.assert_controller_lane_absent_for_test();
    }

    pub(crate) fn controller_lifecycle_for_test(&self) -> Arc<Mutex<()>> {
        self.lifecycle.clone()
    }

    pub(crate) fn invalidate_start_config_for_test(&self) -> PathBuf {
        let path = persistence::config_path();
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, "invalid-json").unwrap();
        path
    }
}
