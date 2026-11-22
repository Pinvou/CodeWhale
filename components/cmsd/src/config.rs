//! 配置解析：默认值 ← 配置文件（极简 TOML 子集）← 环境变量（D7）。
//!
//! 环境变量：CMSD_CONFIG / CMSD_SOCKET / CMSD_STATE_DIR / CMSD_GATEWAY_LISTEN /
//! CMSD_SYSTEMD_MODE / CMSD_CARDS_DIR / CMSD_ENGINE_ROOT / CMSD_UNIT_DIR /
//! CMSD_MODELS_DIR / CMSD_STAGING_DIR / CMSD_HF_ENDPOINT（后三个是 P2 install
//! 注入卡片脚本的 LMM_* 环境口径）/ CMSD_DOCKER / CMSD_NVIDIA_SMI（W6 容器
//! 后端 CLI 注入，测试用 fixture 顶替）/ CMSD_STORE_SOCKET（商店服务 socket，
//! 另见 PINVOU3_HOME 重定位）。
//! 测试钩子：CMSD_PROC_ROOT / CMSD_MEMINFO / CMSD_DEBUG_AVAILABLE_MIB（仅测试用，
//! 见 V5 低内存构造）。

use std::io::Read;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

const CONFIG_LIMIT: u64 = 64 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SystemdMode {
    System,
    User,
}

impl SystemdMode {
    pub fn as_str(&self) -> &'static str {
        match self {
            SystemdMode::System => "system",
            SystemdMode::User => "user",
        }
    }
}

#[derive(Debug, Clone)]
pub struct Config {
    pub socket_path: PathBuf,
    pub state_dir: PathBuf,
    pub gateway_listen: String,
    pub systemd_mode: SystemdMode,
    /// Explicit state-scoped model units; absent preserves legacy installation names.
    pub native_unit_namespace: String,
    pub cards_dir: PathBuf,
    pub engine_root: PathBuf,
    pub unit_dir: PathBuf,
    pub pool_start: u16,
    pub pool_end: u16,
    pub proc_root: PathBuf,
    pub meminfo_path: PathBuf,
    /// V5 测试钩子：设置后 capacity 用它代替真实 MemAvailable。
    pub debug_available_mib: Option<u64>,
    /// 就绪等待上限（卡片范式 25 分钟）。
    pub ready_timeout_secs: u64,
    /// P2 install：权重落盘根目录（注入脚本 LMM_MODELS_DIR，对齐品悟约定）。
    pub models_dir: PathBuf,
    /// P2 install：下载暂存目录（注入脚本 LMM_STAGING_DIR）。
    pub staging_dir: PathBuf,
    /// P2 install：HF 镜像端点（注入脚本 LMM_HF_ENDPOINT）。
    pub hf_endpoint: String,
    /// W6：docker CLI 路径（默认 docker；测试注入 fixture 脚本）。
    pub docker_cli: String,
    /// W6：nvidia-smi 路径（显存归属；GB10 上 query-compute-apps 可用）。
    pub nvidia_smi_cli: String,
    /// 商店服务 socket（2026-10-14 拍板：品悟模型商店是唯一下载执行者，
    /// cmsd install 改为委托门面）。默认随 PINVOU3_HOME 重定位。
    pub store_socket_path: PathBuf,
    /// 镜像 worker 轮询商店任务的间隔（毫秒，默认 2000；测试钩子和
    /// debug_available_mib 同类，生产不用动）。
    pub store_poll_ms: u64,
    /// Trusted headless store reader; unset for standalone/older installations.
    pub store_inventory_program: Option<PathBuf>,
    pub store_inventory_profile: Option<PathBuf>,
}

fn home() -> PathBuf {
    PathBuf::from(std::env::var("HOME").unwrap_or_else(|_| "/root".to_string()))
}

/// 商店服务 socket 默认路径：PINVOU3_HOME 可重定位，缺省 ~/.pinvou3/model-store。
fn default_store_socket() -> PathBuf {
    match std::env::var("PINVOU3_HOME") {
        Ok(h) if !h.is_empty() => PathBuf::from(h).join("model-store/service.sock"),
        _ => home().join(".pinvou3/model-store/service.sock"),
    }
}

fn euid_is_root() -> bool {
    // std 没有 geteuid；读 /proc/self/status 的 Uid 行第二个字段（有效 uid）。
    std::fs::read_to_string("/proc/self/status")
        .ok()
        .and_then(|s| {
            s.lines()
                .find(|l| l.starts_with("Uid:"))
                .map(|l| l.to_string())
        })
        .and_then(|line| line.split_whitespace().nth(1).map(|v| v == "0"))
        .unwrap_or(false)
}

fn default_unit_dir(mode: SystemdMode) -> PathBuf {
    match mode {
        SystemdMode::System => PathBuf::from("/etc/systemd/system"),
        SystemdMode::User => PathBuf::from(
            std::env::var("XDG_CONFIG_HOME")
                .unwrap_or_else(|_| home().join(".config").to_string_lossy().into_owned()),
        )
        .join("systemd/user"),
    }
}

impl Config {
    pub fn load() -> Result<Config, String> {
        let mut cfg = Config::defaults();
        // 配置文件：CMSD_CONFIG 指定，或默认路径存在才读。
        let explicit = std::env::var_os("CMSD_CONFIG");
        if explicit.as_ref().is_some_and(|value| value.is_empty()) {
            return Err("CMSD_CONFIG must not be empty".into());
        }
        let config_path = explicit.as_ref().map(PathBuf::from).unwrap_or_else(|| {
            if euid_is_root() {
                PathBuf::from("/etc/megacube/cmsd.toml")
            } else {
                std::env::var_os("XDG_CONFIG_HOME")
                    .filter(|v| !v.is_empty())
                    .map(PathBuf::from)
                    .unwrap_or_else(|| home().join(".config"))
                    .join("megacube/cmsd.toml")
            }
        });
        cfg.read_config(&config_path, explicit.is_some())?;
        cfg.apply_env()?;
        cfg.validate()?;
        Ok(cfg)
    }

    fn read_config(&mut self, path: &Path, required: bool) -> Result<(), String> {
        // NONBLOCK prevents a mistakenly selected FIFO/device from hanging the
        // service before it can explain the configuration error. Symlinks to a
        // normal configuration file remain supported (e.g. managed configs).
        let file = match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
            .open(path)
        {
            Ok(file) => file,
            Err(error)
                if !required
                    && error.kind() == std::io::ErrorKind::NotFound
                    && std::fs::symlink_metadata(path)
                        .is_err_and(|e| e.kind() == std::io::ErrorKind::NotFound) =>
            {
                return Ok(())
            }
            Err(error) => {
                return Err(format!(
                    "cannot read configuration {}: {}",
                    path.display(),
                    error.kind()
                ))
            }
        };
        let metadata = file
            .metadata()
            .map_err(|_| "cannot inspect configuration".to_owned())?;
        if !metadata.is_file() || metadata.len() > CONFIG_LIMIT {
            return Err("configuration must be a regular file no larger than 64 KiB".into());
        }
        let mut text = String::new();
        file.take(CONFIG_LIMIT + 1)
            .read_to_string(&mut text)
            .map_err(|_| "configuration is unreadable or not UTF-8".to_owned())?;
        if text.len() as u64 > CONFIG_LIMIT {
            return Err("configuration exceeds 64 KiB".into());
        }
        self.apply_config_text(&text)
    }

    fn defaults() -> Config {
        let systemd_mode = if euid_is_root() {
            SystemdMode::System
        } else {
            SystemdMode::User
        };
        let unit_dir = default_unit_dir(systemd_mode);
        let pinvou = std::env::var_os("PINVOU3_HOME")
            .filter(|s| !s.is_empty())
            .map(PathBuf::from)
            .unwrap_or_else(|| home().join(".pinvou3"));
        let (socket_path, state_dir) = if systemd_mode == SystemdMode::System {
            (
                PathBuf::from("/run/megacube/cmsd.sock"),
                PathBuf::from("/var/lib/megacube"),
            )
        } else {
            let state = std::env::var_os("XDG_STATE_HOME")
                .filter(|v| !v.is_empty())
                .map(PathBuf::from)
                .unwrap_or_else(|| home().join(".local/state"));
            let runtime = std::env::var_os("XDG_RUNTIME_DIR")
                .filter(|v| !v.is_empty())
                .map(PathBuf::from)
                .unwrap_or_else(|| state.clone());
            (runtime.join("megacube/cmsd.sock"), state.join("megacube"))
        };
        Config {
            socket_path,
            state_dir,
            gateway_listen: "127.0.0.1:8000".to_string(),
            systemd_mode,
            native_unit_namespace: "legacy".into(),
            cards_dir: pinvou.join("model-store/cards"),
            engine_root: home().join(".local/share/local-model-manager/engines/tachyon-serve"),
            unit_dir,
            pool_start: 18080,
            pool_end: 18099,
            proc_root: PathBuf::from("/proc"),
            meminfo_path: PathBuf::from("/proc/meminfo"),
            debug_available_mib: None,
            ready_timeout_secs: 1500,
            models_dir: pinvou.join("model-store/models"),
            staging_dir: pinvou.join("model-store/staging"),
            hf_endpoint: "https://hf-mirror.com".to_string(),
            docker_cli: "docker".to_string(),
            nvidia_smi_cli: "nvidia-smi".to_string(),
            store_socket_path: default_store_socket(),
            store_poll_ms: 2000,
            store_inventory_program: None,
            store_inventory_profile: None,
        }
    }

    /// Flat TOML only. Parse with the standard library crate so quoted #,
    /// escapes, duplicate keys and inline comments have actual TOML semantics.
    fn apply_config_text(&mut self, text: &str) -> Result<(), String> {
        let table: toml::Table = text.parse().map_err(|error: toml::de::Error| {
            let line = error.span().map(|span| {
                text.as_bytes()[..span.start.min(text.len())]
                    .iter()
                    .filter(|b| **b == b'\n')
                    .count()
                    + 1
            });
            // Parser diagnostics can quote credential-bearing source lines.
            format!(
                "invalid TOML configuration near line {}; source values omitted",
                line.unwrap_or(1)
            )
        })?;
        // Apply mode first, explicit unit_dir later, independent of key order.
        if let Some(value) = table.get("systemd_mode") {
            self.checked_kv(
                "systemd_mode",
                value.as_str().ok_or("systemd_mode must be a string")?,
            )?;
        }
        for (key, value) in table {
            let value = match value {
                toml::Value::String(value) if !Self::numeric_key(&key) => value,
                toml::Value::Integer(value) if Self::numeric_key(&key) && value >= 0 => value.to_string(),
                _ => return Err("configuration contains an unsupported key/type/table; use documented top-level keys".into()),
            };
            self.checked_kv(&key, &value)?;
        }
        Ok(())
    }

    fn numeric_key(key: &str) -> bool {
        matches!(
            key,
            "debug_available_mib"
                | "pool_start"
                | "pool_end"
                | "ready_timeout_secs"
                | "store_poll_ms"
        )
    }

    fn checked_kv(&mut self, key: &str, value: &str) -> Result<(), String> {
        match key {
            "socket_path"
            | "state_dir"
            | "gateway_listen"
            | "cards_dir"
            | "engine_root"
            | "unit_dir"
            | "proc_root"
            | "meminfo_path"
            | "models_dir"
            | "staging_dir"
            | "hf_endpoint"
            | "docker_cli"
            | "nvidia_smi_cli"
            | "store_socket_path"
            | "store_inventory_program"
            | "store_inventory_profile" => {}
            "systemd_mode" if matches!(value, "system" | "user") => {}
            "native_unit_namespace" if matches!(value, "legacy" | "state") => {}
            "require_native_profile_budget" | "require_store_selection" if value == "v1" => {}
            "pool_start" | "pool_end" if value.parse::<u16>().is_ok_and(|v| v > 0) => {}
            "ready_timeout_secs"
                if value.parse::<u64>().is_ok_and(|v| (1..=86400).contains(&v)) => {}
            "store_poll_ms"
                if value
                    .parse::<u64>()
                    .is_ok_and(|v| (50..=60000).contains(&v)) => {}
            "debug_available_mib" if value.parse::<u64>().is_ok() => {}
            "systemd_mode"
            | "native_unit_namespace"
            | "require_native_profile_budget"
            | "require_store_selection"
            | "pool_start"
            | "pool_end"
            | "ready_timeout_secs"
            | "store_poll_ms"
            | "debug_available_mib" => {
                return Err(format!(
                    "invalid configuration value for {key}; value omitted"
                ));
            }
            _ => return Err("unknown configuration key; use documented top-level keys".into()),
        }
        self.apply_kv(key, value);
        Ok(())
    }

    fn apply_env(&mut self) -> Result<(), String> {
        for (key, var) in [
            ("socket_path", "CMSD_SOCKET"),
            ("state_dir", "CMSD_STATE_DIR"),
            ("gateway_listen", "CMSD_GATEWAY_LISTEN"),
            ("systemd_mode", "CMSD_SYSTEMD_MODE"),
            ("cards_dir", "CMSD_CARDS_DIR"),
            ("engine_root", "CMSD_ENGINE_ROOT"),
            ("unit_dir", "CMSD_UNIT_DIR"),
            ("proc_root", "CMSD_PROC_ROOT"),
            ("meminfo_path", "CMSD_MEMINFO"),
            ("debug_available_mib", "CMSD_DEBUG_AVAILABLE_MIB"),
            ("models_dir", "CMSD_MODELS_DIR"),
            ("staging_dir", "CMSD_STAGING_DIR"),
            ("hf_endpoint", "CMSD_HF_ENDPOINT"),
            ("docker_cli", "CMSD_DOCKER"),
            ("nvidia_smi_cli", "CMSD_NVIDIA_SMI"),
            ("store_socket_path", "CMSD_STORE_SOCKET"),
            ("store_poll_ms", "CMSD_STORE_POLL_MS"),
        ] {
            match std::env::var(var) {
                Ok(val) => self
                    .checked_kv(key, &val)
                    .map_err(|_| format!("invalid environment variable {var}; value omitted"))?,
                Err(std::env::VarError::NotPresent) => {}
                Err(_) => return Err(format!("environment variable {var} must be UTF-8")),
            }
        }
        Ok(())
    }

    fn validate(&self) -> Result<(), String> {
        if self.store_inventory_program.is_some() != self.store_inventory_profile.is_some() {
            return Err("store inventory program and profile must be configured together".into());
        }
        for path in [&self.store_inventory_program, &self.store_inventory_profile]
            .into_iter()
            .flatten()
        {
            if !path.is_absolute() || path.as_os_str().as_bytes().contains(&0) {
                return Err("store inventory paths must be absolute without NUL".into());
            }
        }
        for (key, path) in [
            ("socket_path", &self.socket_path),
            ("state_dir", &self.state_dir),
            ("cards_dir", &self.cards_dir),
            ("engine_root", &self.engine_root),
            ("unit_dir", &self.unit_dir),
            ("models_dir", &self.models_dir),
            ("staging_dir", &self.staging_dir),
            ("proc_root", &self.proc_root),
            ("meminfo_path", &self.meminfo_path),
            ("store_socket_path", &self.store_socket_path),
        ] {
            if !path.is_absolute() || path.as_os_str().as_bytes().contains(&0) {
                return Err(format!("{key} must be an absolute path without NUL"));
            }
        }
        if self.socket_path.as_os_str().as_bytes().len() >= 108
            || self.store_socket_path.as_os_str().as_bytes().len() >= 108
        {
            return Err("Unix socket path must fit Linux sockaddr_un (at most 107 bytes)".into());
        }
        if self.socket_path == self.store_socket_path {
            return Err("cmsd and model store must use different socket paths".into());
        }
        if self.pool_start > self.pool_end {
            return Err("pool_start must not exceed pool_end".into());
        }
        if self.gateway_listen.parse::<std::net::SocketAddr>().is_err() {
            return Err("gateway_listen must be a literal IP:port (IPv6 in brackets)".into());
        }
        Ok(())
    }

    fn apply_kv(&mut self, key: &str, val: &str) {
        match key {
            "socket_path" => self.socket_path = PathBuf::from(val),
            "state_dir" => self.state_dir = PathBuf::from(val),
            "gateway_listen" => self.gateway_listen = val.to_string(),
            "systemd_mode" => {
                self.systemd_mode = match val {
                    "system" => SystemdMode::System,
                    _ => SystemdMode::User,
                };
                // 模式变了、unit_dir 还是旧默认时跟着换默认
                let default_for_new = default_unit_dir(self.systemd_mode);
                if self.unit_dir
                    == default_unit_dir(match self.systemd_mode {
                        SystemdMode::System => SystemdMode::User,
                        SystemdMode::User => SystemdMode::System,
                    })
                {
                    self.unit_dir = default_for_new;
                }
            }
            "native_unit_namespace" => self.native_unit_namespace = val.into(),
            // Compatibility requirement: this binary always honors profile budgets.
            // Older strict readers reject the key before opening runtime state.
            "require_native_profile_budget" | "require_store_selection" => {}
            "cards_dir" => self.cards_dir = PathBuf::from(val),
            "engine_root" => self.engine_root = PathBuf::from(val),
            "unit_dir" => self.unit_dir = PathBuf::from(val),
            "proc_root" => self.proc_root = PathBuf::from(val),
            "meminfo_path" => self.meminfo_path = PathBuf::from(val),
            "debug_available_mib" => self.debug_available_mib = val.parse().ok(),
            "pool_start" => self.pool_start = val.parse().unwrap_or(self.pool_start),
            "pool_end" => self.pool_end = val.parse().unwrap_or(self.pool_end),
            "ready_timeout_secs" => {
                self.ready_timeout_secs = val.parse().unwrap_or(self.ready_timeout_secs)
            }
            "models_dir" => self.models_dir = PathBuf::from(val),
            "staging_dir" => self.staging_dir = PathBuf::from(val),
            "hf_endpoint" => self.hf_endpoint = val.to_string(),
            "docker_cli" => self.docker_cli = val.to_string(),
            "nvidia_smi_cli" => self.nvidia_smi_cli = val.to_string(),
            "store_socket_path" => self.store_socket_path = PathBuf::from(val),
            "store_poll_ms" => self.store_poll_ms = val.parse().unwrap_or(self.store_poll_ms),
            "store_inventory_program" => self.store_inventory_program = Some(PathBuf::from(val)),
            "store_inventory_profile" => self.store_inventory_profile = Some(PathBuf::from(val)),
            _ => {}
        }
    }

    pub fn env_dir(&self) -> PathBuf {
        self.state_dir.join("env")
    }

    pub fn state_file(&self) -> PathBuf {
        self.state_dir.join("state.json")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_unit_namespace_is_explicit_and_legacy_configs_keep_their_names() {
        let mut cfg = Config::defaults();
        assert_eq!(cfg.native_unit_namespace, "legacy");
        cfg.apply_config_text("native_unit_namespace = 'state'\n")
            .unwrap();
        assert_eq!(cfg.native_unit_namespace, "state");
        assert!(cfg
            .apply_config_text("require_native_profile_budget = 'v1'\n")
            .is_ok());
        assert!(cfg
            .apply_config_text("require_native_profile_budget = 'unknown'\n")
            .is_err());
        assert!(cfg
            .apply_config_text("require_native_profile_budget = 1\n")
            .is_err());
        assert!(cfg
            .apply_config_text("native_unit_namespace = 'arbitrary'\n")
            .is_err());
        assert!(cfg
            .apply_config_text("native_unit_namespace = true\n")
            .is_err());
    }

    #[test]
    fn config_text_minimal_toml() {
        let mut cfg = Config::defaults();
        cfg.apply_config_text(
            "# 注释\nsocket_path = \"/tmp/x.sock\"\npool_start = 19000\nmodels_dir = \"/tmp/models\"\nhf_endpoint = \"https://example.com\" # actual TOML comment\n",
        ).unwrap();
        assert_eq!(cfg.socket_path, PathBuf::from("/tmp/x.sock"));
        assert_eq!(cfg.pool_start, 19000);
        assert_eq!(cfg.pool_end, 18099);
        assert_eq!(cfg.models_dir, PathBuf::from("/tmp/models"));
        assert_eq!(cfg.hf_endpoint, "https://example.com");
        // 未配置项保持品悟约定默认值
        assert!(cfg.staging_dir.ends_with(".pinvou3/model-store/staging"));
    }

    #[test]
    fn systemd_mode_switch_moves_default_unit_dir() {
        let mut cfg = Config::defaults();
        cfg.systemd_mode = SystemdMode::System;
        cfg.unit_dir = default_unit_dir(SystemdMode::System);
        cfg.apply_kv("systemd_mode", "user");
        assert_eq!(cfg.systemd_mode, SystemdMode::User);
        assert!(cfg.unit_dir.ends_with("systemd/user"));
    }

    #[test]
    fn toml_preserves_quoted_hash_and_escape_and_rejects_ambiguity_without_leaking_values() {
        let mut cfg = Config::defaults();
        cfg.apply_config_text("cards_dir = \"/tmp/models#not-comment\\twith-space\" # comment\n")
            .unwrap();
        assert_eq!(
            cfg.cards_dir,
            PathBuf::from("/tmp/models#not-comment\twith-space")
        );
        for input in [
            "state_dir = \"/tmp/a\"\nstate_dir = \"/tmp/b\"",
            "state_dr = \"/tmp/x\"",
            "[cmsd]\nstate_dir = \"/tmp/x\"",
            "pool_end = \"19099\"",
            "pool_end = 2.0",
            "pool_end = -1",
            "pool_end = 65536",
            "systemd_mode = \"users\"",
            "store_poll_ms = 0",
            "ready_timeout_secs = 0",
            "invalid credential_secret_canary",
        ] {
            let error = cfg.apply_config_text(input).unwrap_err();
            assert!(!error.contains("credential_secret_canary"));
        }
    }

    #[test]
    fn required_config_missing_corrupt_directory_fifo_and_oversized_fail_without_fallback() {
        let root = crate::docker::fixtures::tmpdir("config-failure");
        let path = root.join("config");
        let mut cfg = Config::defaults();
        assert!(cfg.read_config(&path, false).is_ok());
        assert!(cfg.read_config(&path, true).is_err());
        assert!(cfg.read_config(&root, false).is_err());
        std::fs::write(&path, [0xff]).unwrap();
        assert!(cfg.read_config(&path, false).is_err());
        std::fs::write(&path, vec![b' '; CONFIG_LIMIT as usize + 1]).unwrap();
        assert!(cfg.read_config(&path, true).is_err());
        let alias = root.join("alias");
        std::os::unix::fs::symlink(root.join("absent"), &alias).unwrap();
        assert!(cfg.read_config(&alias, false).is_err());
        let fifo = root.join("fifo");
        let c_path = std::ffi::CString::new(fifo.as_os_str().as_bytes()).unwrap();
        // SAFETY: valid NUL-terminated path to this test's private directory.
        assert_eq!(unsafe { libc::mkfifo(c_path.as_ptr(), 0o600) }, 0);
        assert!(cfg.read_config(&fifo, true).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn validation_requires_distinct_bounded_sockets_absolute_paths_and_literal_gateway() {
        let mut cfg = Config::defaults();
        assert!(cfg.validate().is_ok());
        cfg.pool_start = cfg.pool_end + 1;
        assert!(cfg.validate().is_err());
        cfg.pool_start = cfg.pool_end;
        cfg.socket_path = cfg.store_socket_path.clone();
        assert!(cfg.validate().is_err());
        cfg.socket_path = PathBuf::from("relative.sock");
        assert!(cfg.validate().is_err());
        cfg.socket_path = PathBuf::from(format!("/{}", "x".repeat(107)));
        assert!(cfg.validate().is_err());
        cfg.socket_path = PathBuf::from("/tmp/bounded-config.sock");
        cfg.gateway_listen = "localhost:8000".into();
        assert!(cfg.validate().is_err());
        cfg.gateway_listen = "[::1]:0".into();
        assert!(cfg.validate().is_ok());
    }
}
