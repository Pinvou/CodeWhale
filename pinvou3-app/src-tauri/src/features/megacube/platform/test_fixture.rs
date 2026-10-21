//! Shared test-only isolation for real local client acceptance.
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};
fn sha256(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|v| format!("{v:02x}"))
        .collect()
}
/// Owns only a fresh uniquely named QA unit. Never enables it at boot.
pub(crate) struct UserUnit {
    pub(crate) name: String,
    file: PathBuf,
}

pub(crate) fn systemctl(args: &[&str]) -> String {
    let output = Command::new("systemctl")
        .arg("--user")
        .args(args)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "isolated user unit operation failed"
    );
    String::from_utf8(output.stdout).unwrap()
}

impl UserUnit {
    pub(crate) fn create(binary: &Path, config: &Path) -> Self {
        let name = format!("cmsd-pinvou-qa-{}.service", uuid::Uuid::new_v4());
        let file = crate::platform::paths::user_home_dir()
            .join(".config/systemd/user")
            .join(&name);
        assert!(!file.exists());
        let value = Self { name, file };
        let text = format!(
            "[Service]\nType=simple\nExecStart={}\nEnvironment=CMSD_CONFIG={}\nUnsetEnvironment=CMSD_SOCKET CMSD_STATE_DIR CMSD_GATEWAY_LISTEN CMSD_SYSTEMD_MODE CMSD_CARDS_DIR CMSD_ENGINE_ROOT CMSD_UNIT_DIR CMSD_MODELS_DIR CMSD_STAGING_DIR CMSD_STORE_SOCKET CMSD_PROC_ROOT CMSD_MEMINFO CMSD_DEBUG_AVAILABLE_MIB CMSD_CONTROL_LISTEN PINVOU3_HOME\nUMask=0077\n",
            serde_json::to_string(binary.to_str().unwrap()).unwrap(),
            serde_json::to_string(config.to_str().unwrap()).unwrap()
        );
        crate::platform::filesystem::atomic_write_private(&value.file, text.as_bytes()).unwrap();
        systemctl(&["daemon-reload"]);
        systemctl(&["start", &value.name]);
        value
    }
}

impl Drop for UserUnit {
    fn drop(&mut self) {
        let _ = Command::new("systemctl")
            .args(["--user", "stop", &self.name])
            .output();
        let _ = std::fs::remove_file(&self.file);
        let overrides = self.file.with_file_name(format!("{}.d", self.name));
        let _ = std::fs::remove_file(overrides.join("90-pinvou-cmsd.conf"));
        let _ = std::fs::remove_dir(overrides);
        let _ = Command::new("systemctl")
            .args(["--user", "daemon-reload"])
            .output();
        let _ = Command::new("systemctl")
            .args(["--user", "reset-failed", &self.name])
            .output();
    }
}

pub(crate) fn adopt(pinvou: &Path, binary: &Path, root: &Path) {
    let package = root.join("package");
    crate::platform::filesystem::open_private_file_directory(&package).unwrap();
    std::fs::copy(binary, package.join("cmsd")).unwrap();
    let bytes = std::fs::read(binary).unwrap();
    let hash = sha256(&bytes);
    let manifest = json!({"format":"megacube-cmsd-component-v1","component":"cmsd","version":"0.1.0","architecture":std::env::consts::ARCH,"os":"linux","api":"v1","state_format":"cmsd-json-v1","sha256":hash,"size":bytes.len()});
    std::fs::write(package.join("manifest.json"), manifest.to_string()).unwrap();
    let cli = |args: &[&str]| -> Value {
        let result = Command::new(pinvou)
            .arg("--cmsd-component-cli")
            .args(args)
            .output()
            .unwrap();
        assert!(result.status.success(), "component CLI failed");
        serde_json::from_slice(&result.stdout).unwrap()
    };
    let preview = cli(&["inspect", package.to_str().unwrap()]);
    assert_eq!(preview["manifest"]["sha256"], hash);
    let accepted = cli(&["action", "update", package.to_str().unwrap(), &hash]);
    assert!(
        accepted["operationId"].is_string(),
        "component update was not accepted"
    );
    let end = Instant::now() + Duration::from_secs(60);
    loop {
        let status = cli(&["status"]);
        if status["operation"]["id"] == accepted["operationId"]
            && !matches!(
                status["operation"]["status"].as_str(),
                Some("queued" | "running")
            )
        {
            assert_eq!(
                status["operation"]["status"], "succeeded",
                "component adoption failed: {status}"
            );
            return;
        }
        assert!(Instant::now() < end, "component adoption timed out");
        std::thread::sleep(Duration::from_millis(100));
    }
}
