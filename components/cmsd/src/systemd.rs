//! systemd 后端：单元文件生成、启停、状态查询、日志尾部读取。
//!
//! D3：新安装按状态目录隔离模板；旧配置保留 `megacube-model@.service`，
//! 但不覆盖其他实例的模板。生产系统级（User= 设备主用户），
//! 开发环境用 user 级 + linger（语义相同，免 root）。
//! D4：Restart=on-failure + StartLimitBurst=3/300s 由单元文件落实，
//! cmsd 只观测结果（Result=start-limit-hit → degraded）。
//!
//! 启动参数与真机卡片 qwen38-27b-nvfp4-tachyon/enable.sh 的 2026-10-01
//! 生产配置一字不差（模板 ExecStart），每卡差异全部由 env 文件注入。

use crate::config::SystemdMode;
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::process::Command;

pub struct Systemd {
    pub mode: SystemdMode,
    pub unit_dir: PathBuf,
    /// env 文件目录（$STATE_DIR/env），单元模板里 EnvironmentFile 指向 %i.env。
    pub env_dir: PathBuf,
    /// 系统级单元的运行身份（D3：设备主用户）。user 模式忽略。
    pub run_user: String,
    cli: String,
    unit_prefix: String,
}

#[derive(Debug, Clone, Default)]
pub struct UnitState {
    pub active_state: String,
    pub sub_state: String,
    pub main_pid: u32,
    pub n_restarts: u64,
    pub result: String,
    /// unit 文件不存在（未生成/已清理）。
    pub not_found: bool,
    pub boot_enabled: Option<bool>,
    pub checked_at: u64,
}

impl UnitState {
    pub fn is_active(&self) -> bool {
        self.active_state == "active"
    }

    /// start-limit 熔断命中（systemd 放弃再拉起）。
    pub fn start_limit_hit(&self) -> bool {
        self.result == "start-limit-hit"
            || (self.active_state == "failed" && self.result == "start-limit")
    }
}

impl Systemd {
    pub fn new(
        mode: SystemdMode,
        unit_dir: PathBuf,
        env_dir: PathBuf,
        run_user: String,
    ) -> Systemd {
        Systemd {
            mode,
            unit_dir,
            env_dir,
            run_user,
            cli: "systemctl".into(),
            unit_prefix: "megacube-model".into(),
        }
    }

    pub fn with_state_namespace(mut self, scoped: bool) -> Self {
        if scoped {
            use std::os::unix::ffi::OsStrExt;
            let digest =
                ring::digest::digest(&ring::digest::SHA256, self.env_dir.as_os_str().as_bytes());
            let suffix: String = digest.as_ref()[..8]
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect();
            self.unit_prefix = format!("megacube-model-{suffix}");
        }
        self
    }

    /// Model processes belong to the configured execution account. System
    /// daemons run as root, while their model units use CMSD_RUN_USER.
    pub fn model_run_uid(&self) -> Result<u32, String> {
        if self.mode == SystemdMode::User {
            return Ok(unsafe { libc::geteuid() });
        }
        let name = std::ffi::CString::new(self.run_user.as_str())
            .map_err(|_| "model execution account is invalid")?;
        if name.as_bytes().is_empty() {
            return Err("model execution account is missing".into());
        }
        let mut record = std::mem::MaybeUninit::<libc::passwd>::uninit();
        let mut result = std::ptr::null_mut();
        let mut buffer = vec![0u8; 64 * 1024];
        // Reentrant NSS lookup; an unknown account or oversized result fails
        // closed instead of falling back to the privileged daemon's identity.
        let status = unsafe {
            if self.run_user.bytes().all(|byte| byte.is_ascii_digit()) {
                let uid = self
                    .run_user
                    .parse::<u32>()
                    .map_err(|_| "model execution UID is invalid")?;
                libc::getpwuid_r(
                    uid,
                    record.as_mut_ptr(),
                    buffer.as_mut_ptr().cast(),
                    buffer.len(),
                    &mut result,
                )
            } else {
                libc::getpwnam_r(
                    name.as_ptr(),
                    record.as_mut_ptr(),
                    buffer.as_mut_ptr().cast(),
                    buffer.len(),
                    &mut result,
                )
            }
        };
        if status != 0 || result.is_null() {
            return Err("model execution account could not be resolved".into());
        }
        Ok(unsafe { record.assume_init() }.pw_uid)
    }

    pub fn unit_name_for(&self, card: &str) -> String {
        format!("{}@{card}.service", self.unit_prefix)
    }

    pub fn card_from_unit(&self, unit: &str) -> Option<String> {
        unit.strip_prefix(&format!("{}@", self.unit_prefix))
            .and_then(|value| value.strip_suffix(".service"))
            .map(str::to_owned)
    }

    fn template_path(&self) -> PathBuf {
        self.unit_dir.join(format!("{}@.service", self.unit_prefix))
    }

    /// A configuration edit or incomplete backup must not silently remap live native units.
    pub fn claim_namespace(
        &self,
        state: &std::path::Path,
        has_owned_native: bool,
    ) -> Result<(), String> {
        let marker = state.join("native-unit-namespace-v1");
        let expected = format!("cmsd-native-unit-namespace-v1\n{}\n", self.unit_prefix);
        match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(&marker)
        {
            Ok(mut file) => {
                let meta = file.metadata().map_err(|e| e.to_string())?;
                if !meta.is_file() || meta.len() > 1024 {
                    return Err("model_unit_namespace_corrupt".into());
                }
                let mut actual = String::new();
                Read::by_ref(&mut file)
                    .take(1025)
                    .read_to_string(&mut actual)
                    .map_err(|_| "model_unit_namespace_corrupt")?;
                if actual != expected {
                    return Err("model_unit_namespace_changed: restore matching configuration and full state backup".into());
                }
                Ok(())
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                if self.unit_prefix != "megacube-model" && has_owned_native {
                    return Err("model_unit_namespace_missing: existing native ownership needs matching namespace record".into());
                }
                std::fs::create_dir_all(state).map_err(|e| e.to_string())?;
                let temporary = state.join(format!(
                    ".native-unit-namespace-{}-{}.tmp",
                    std::process::id(),
                    std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_nanos()
                ));
                let mut file = std::fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(0o600)
                    .open(&temporary)
                    .map_err(|e| e.to_string())?;
                let result = (|| {
                    file.write_all(expected.as_bytes())
                        .and_then(|_| file.sync_all())
                        .map_err(|e| e.to_string())?;
                    std::fs::hard_link(&temporary, &marker).map_err(|e| e.to_string())?;
                    std::fs::File::open(state)
                        .and_then(|file| file.sync_all())
                        .map_err(|e| e.to_string())
                })();
                let _ = std::fs::remove_file(temporary);
                result
            }
            Err(_) => Err("model_unit_namespace_unreadable".into()),
        }
    }

    /// Read one existing template/control backup through its verified FD. Parent
    /// directory aliases and readable administrator-owned files retain their
    /// existing contract; the leaf must be a bounded ordinary file.
    fn template_bytes(path: &std::path::Path) -> std::io::Result<Vec<u8>> {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
            .open(path)?;
        Self::bounded_template_bytes(file)
    }

    /// Existing matching backups may be readable file aliases. Keep that
    /// contract while rejecting special files before reading their bytes.
    fn backup_bytes(path: &std::path::Path) -> std::io::Result<Vec<u8>> {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
            .open(path)?;
        Self::bounded_template_bytes(file)
    }

    fn bounded_template_bytes(file: std::fs::File) -> std::io::Result<Vec<u8>> {
        const LIMIT: u64 = 64 * 1024;
        let metadata = file.metadata()?;
        if !metadata.is_file() || metadata.len() > LIMIT {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "model template or backup must be an ordinary file under 64 KiB",
            ));
        }
        let mut bytes = Vec::new();
        file.take(LIMIT + 1).read_to_end(&mut bytes)?;
        if bytes.len() as u64 > LIMIT {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "model template or backup exceeds 64 KiB",
            ));
        }
        Ok(bytes)
    }

    pub fn assert_template_owner(&self) -> Result<(), String> {
        let existing = Self::template_bytes(&self.template_path()).map_err(|_| {
            "model_unit_namespace_unverified: model template missing or unreadable".to_owned()
        })?;
        if existing != self.template_content().as_bytes()
            && existing != self.previous_template_content().as_bytes()
        {
            return Err("model_unit_namespace_conflict: model template belongs to another state/configuration; preserved".into());
        }
        Ok(())
    }

    #[cfg(test)]
    pub fn with_test_cli(mut self, path: &std::path::Path) -> Self {
        self.cli = path.to_string_lossy().into_owned();
        self
    }

    fn cmd(&self, args: &[&str]) -> Command {
        let mut c = Command::new(&self.cli);
        if self.mode == SystemdMode::User {
            c.arg("--user");
        }
        c.args(args);
        c
    }

    fn run(&self, args: &[&str]) -> Result<String, String> {
        if args.first().is_some_and(|value| {
            ["start", "stop", "enable", "disable", "reset-failed"].contains(value)
        }) && args
            .get(1)
            .is_some_and(|unit| self.card_from_unit(unit).is_some())
        {
            self.assert_template_owner()?;
        }
        let mutation = args.first().is_some_and(|v| ["start", "stop"].contains(v));
        let out = crate::process::output(
            &mut self.cmd(args),
            std::time::Duration::from_secs(if mutation { 150 } else { 10 }),
        )
        .map_err(|e| format!("systemctl 调用失败：{e}"))?;
        if out.status.success() {
            Ok(String::from_utf8_lossy(&out.stdout).to_string())
        } else {
            Err(format!(
                "systemctl {} 失败（rc={:?}）：{}",
                args.join(" "),
                out.status.code(),
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
    }

    pub fn daemon_reload(&self) -> Result<(), String> {
        self.run(&["daemon-reload"]).map(|_| ())
    }

    pub fn start(&self, unit: &str) -> Result<(), String> {
        self.run(&["start", unit]).map(|_| ())
    }

    pub fn stop(&self, unit: &str) -> Result<(), String> {
        self.run(&["stop", unit]).map(|_| ())
    }

    pub fn disable(&self, unit: &str) -> Result<(), String> {
        self.run(&["disable", unit]).map(|_| ())
    }

    /// 清除 start-limit 计数与 failed 态（degraded 恢复的唯一入口）。
    pub fn reset_failed(&self, unit: &str) -> Result<(), String> {
        self.run(&["reset-failed", unit]).map(|_| ())
    }

    pub fn show(&self, unit: &str) -> Result<UnitState, String> {
        let out = crate::process::output(
            &mut self.cmd(&[
                "show",
                unit,
                "--property=ActiveState,SubState,MainPID,NRestarts,Result,LoadState,UnitFileState",
            ]),
            std::time::Duration::from_secs(10),
        )
        .map_err(|e| format!("systemctl show 调用失败：{e}"))?;
        if !out.status.success()
            && !String::from_utf8_lossy(&out.stdout).contains("LoadState=not-found")
        {
            return Err(format!(
                "systemctl show 失败：{}",
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        let text = String::from_utf8_lossy(&out.stdout).to_string();
        let props: BTreeMap<String, String> = text
            .lines()
            .filter_map(|l| l.split_once('='))
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        let get = |k: &str| props.get(k).cloned().unwrap_or_default();
        let load_state = get("LoadState");
        if load_state.is_empty()
            || !props.contains_key("ActiveState")
            || get("MainPID").parse::<u32>().is_err()
        {
            return Err("systemctl show 返回不完整，无法核实单元状态".into());
        }
        Ok(UnitState {
            active_state: get("ActiveState"),
            sub_state: get("SubState"),
            main_pid: get("MainPID").parse().unwrap_or(0),
            n_restarts: get("NRestarts").parse().unwrap_or(0),
            result: get("Result"),
            not_found: load_state == "not-found",
            boot_enabled: match get("UnitFileState").as_str() {
                "enabled" | "enabled-runtime" => Some(true),
                "disabled" | "linked" | "linked-runtime" | "static" | "indirect" | "not-found"
                | "masked" | "masked-runtime" => Some(false),
                _ => None,
            },
            checked_at: crate::timeutil::now_epoch(),
        })
    }

    /// 只列出本状态目录命名空间的实例（含 dead/failed），返回单元名。
    pub fn list_model_units(&self) -> Result<Vec<String>, String> {
        let pattern = format!("{}@*", self.unit_prefix);
        let out = self.run(&[
            "list-units",
            &pattern,
            "--all",
            "--plain",
            "--no-legend",
            "--no-page",
        ])?;
        Ok(out
            .lines()
            .filter_map(|l| l.split_whitespace().next())
            .filter(|s| self.card_from_unit(s).is_some())
            .map(|s| s.to_string())
            .collect())
    }

    pub fn enabled_checked(&self, unit: &str) -> Result<bool, String> {
        let out = crate::process::output(
            &mut self.cmd(&["is-enabled", unit]),
            std::time::Duration::from_secs(10),
        )
        .map_err(|e| e.to_string())?;
        let value = String::from_utf8_lossy(&out.stdout);
        match value.trim() {
            "enabled" | "enabled-runtime" => Ok(true),
            "linked" | "linked-runtime" | "disabled" | "static" | "indirect" | "not-found"
            | "masked" | "masked-runtime" => Ok(false),
            _ => Err(format!(
                "不能核实开机登记：{}",
                String::from_utf8_lossy(&out.stderr).trim()
            )),
        }
    }

    /// Only the explicitly adopted instance receives this drop-in. Updating
    /// the shared template must not alter an observation-only unit's policy.
    pub fn restart_is_disabled(&self, unit: &str) -> Result<bool, String> {
        if unit.contains('/') || !unit.ends_with(".service") {
            return Err("invalid model unit".into());
        }
        Ok(self
            .run(&["show", unit, "--property=Restart", "--value"])?
            .trim()
            == "no")
    }

    pub fn fence_recovery(&self, unit: &str) -> Result<(), String> {
        if self.card_from_unit(unit).is_none() || unit.contains('/') {
            return Err("invalid model unit".into());
        }
        self.assert_template_owner()?;
        let directory = self.unit_dir.join(format!("{unit}.d"));
        if recovery_dropin(&directory)? {
            self.daemon_reload()?;
        }
        if self.enabled_checked(unit)? {
            self.disable(unit)?;
        }
        let actual = self.run(&["show", unit, "--property=Restart", "--value"])?;
        if actual.trim() != "no" {
            return Err("无法核实Restart=no，拒绝绕过预算恢复".into());
        }
        Ok(())
    }

    /// start-limit 熔断判定：systemd 255 上 Result 仍是 exit-code（V4 实测），
    /// 可靠信号是 journal 里的 "Start request repeated too quickly"（或老版本
    /// 的 Result=start-limit-hit）。只在观察到 failed 时调用（代价可控）。
    pub fn start_limit_hit(&self, unit: &str, us: &UnitState) -> bool {
        if us.start_limit_hit() {
            return true;
        }
        if us.active_state != "failed" {
            return false;
        }
        self.journal_tail(unit, 5)
            .map(|t| t.contains("Start request repeated too quickly"))
            .unwrap_or(false)
    }

    /// 生成/更新模板单元（幂等：内容相同不重写，避免 daemon-reload 风暴）。
    pub fn write_template(&self) -> std::io::Result<bool> {
        std::fs::create_dir_all(&self.unit_dir)?;
        let path = self.template_path();
        let content = self.template_content();
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let temporary = self.unit_dir.join(format!(
            ".{}-{}-{stamp}.tmp",
            self.unit_prefix,
            std::process::id()
        ));
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&temporary)?;
        let result = (|| {
            file.write_all(content.as_bytes())?;
            file.sync_all()?;
            match std::fs::hard_link(&temporary, &path) {
                Ok(()) => {
                    std::fs::File::open(&self.unit_dir)?.sync_all()?;
                    Ok(true)
                }
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                    self.assert_template_owner().map_err(|message| {
                        std::io::Error::new(std::io::ErrorKind::PermissionDenied, message)
                    })?;
                    // Only the complete known previous template for this exact
                    // account/state path is eligible. Keep its original bytes.
                    let previous = self.previous_template_content();
                    if Self::template_bytes(&path)? == previous.as_bytes() {
                        let backup = path.with_extension("service.cmsd-before-profile-v1");
                        match std::fs::hard_link(&path, &backup) {
                            Ok(()) => {}
                            Err(e)
                                if e.kind() == std::io::ErrorKind::AlreadyExists
                                    && Self::backup_bytes(&backup)? == previous.as_bytes() => {}
                            Err(e) => return Err(e),
                        }
                        if Self::template_bytes(&path)? != previous.as_bytes() {
                            return Err(std::io::Error::other(
                                "model template changed during upgrade",
                            ));
                        }
                        std::fs::rename(&temporary, &path)?;
                        std::fs::File::open(&self.unit_dir)?.sync_all()?;
                        return Ok(true);
                    }
                    Ok(false)
                }
                Err(error) => Err(error),
            }
        })();
        let _ = std::fs::remove_file(temporary);
        result
    }

    fn template_content(&self) -> String {
        let mut s = String::new();
        s.push_str("[Unit]\n");
        s.push_str("Description=MegaCube model engine (card %i)\n");
        s.push_str(
            "# D4 自愈限频：5 分钟内失败 3 次熔断，cmsd 观测 Result=start-limit-hit 转 degraded\n",
        );
        s.push_str("StartLimitIntervalSec=300\n");
        s.push_str("StartLimitBurst=3\n");
        s.push_str("\n[Service]\n");
        s.push_str("Type=simple\n");
        if self.mode == SystemdMode::System {
            s.push_str(&format!("User={}\n", self.run_user));
        }
        s.push_str(&format!(
            "EnvironmentFile={}\n",
            self.env_dir.join("%i.env").display()
        ));
        s.push_str("# 启动参数骨架沿用真机卡片 enable.sh 的生产配置；每卡字面量旗标\n");
        s.push_str("# （--spec/--kv-dtype 等，模型相关）经 EXTRA_FLAGS 注入（卡片 enable.sh\n");
        s.push_str(
            "# 提取或 state_dir/serve-flags/<card>.txt 覆盖）。slots ≥ 2×并发（卡片铁律）。\n",
        );
        s.push_str("# argv[0] 必须是字面量绝对路径（systemd 不在程序名位置展开变量），\n");
        s.push_str(
            "# 故经 /bin/sh 启动；exec 保证引擎即主进程，信号直送。$$ 转义后由 shell 展开，\n",
        );
        s.push_str(
            "# EXTRA_FLAGS 刻意不加引号：shell 分词成多个参数（内容来自卡片/管理员，可信）。\n",
        );
        s.push_str("ExecStart=/bin/sh -c 'exec \"$$ENGINE_DIR/tachyon-serve\" \"$$MODEL_DIR/$$ARTIFACT\" --host \"$$HOST\" --port \"$$PORT\" --model-id \"$$MODEL_ID\" --max-context \"$$MAX_CONTEXT\" --max-concurrency \"$$MAX_CONCURRENCY\" --device-state-slots \"$${DEVICE_STATE_SLOTS:-$$SLOTS}\" --host-state-slots \"$${HOST_STATE_SLOTS:-$$SLOTS}\" $$EXTRA_FLAGS'\n");
        s.push_str("Restart=on-failure\n");
        s.push_str("RestartSec=5\n");
        s.push_str("# 权重 22GB，停止留给引擎充分退出时间\n");
        s.push_str("TimeoutStopSec=120\n");
        s.push_str("\n[Install]\n");
        match self.mode {
            SystemdMode::System => s.push_str("WantedBy=multi-user.target\n"),
            SystemdMode::User => s.push_str("WantedBy=default.target\n"),
        }
        s
    }

    fn previous_template_content(&self) -> String {
        self.template_content()
            .replace("$${DEVICE_STATE_SLOTS:-$$SLOTS}", "$$SLOTS")
            .replace("$${HOST_STATE_SLOTS:-$$SLOTS}", "$$SLOTS")
    }

    /// 引擎日志尾部（journald）。user 模式读用户 journal。
    pub fn journal_tail(&self, unit: &str, lines: u32) -> Result<String, String> {
        let mut c = Command::new("journalctl");
        if self.mode == SystemdMode::User {
            c.arg("--user");
        }
        c.args([
            "-u",
            unit,
            "-n",
            &lines.to_string(),
            "--no-pager",
            "--output=short",
        ]);
        let out = crate::process::output(&mut c, std::time::Duration::from_secs(10))
            .map_err(|e| format!("journalctl 调用失败：{e}"))?;
        Ok(String::from_utf8_lossy(&out.stdout).to_string())
    }

    /// 最近一行引擎日志（任务心跳用）。
    pub fn journal_last_line(&self, unit: &str) -> Option<String> {
        self.journal_tail(unit, 1).ok().and_then(|t| {
            t.lines()
                .rfind(|l| !l.trim().is_empty() && !l.starts_with("--"))
                .map(|l| l.chars().take(200).collect())
        })
    }
}

/// The callers have already proved the exact owned native unit. This helper
/// only validates/publishes its recovery drop-in; backend actions stay outside.
/// Both existing fence paths use these complete known Restart=no contents.
pub(crate) fn recovery_dropin(directory: &std::path::Path) -> Result<bool, String> {
    if !crate::private_storage::control_directory(directory)? {
        crate::private_storage::directory(directory, false)?;
    }
    let path = directory.join("90-cmsd-recovery.conf");
    const MANAGED: &[u8] =
        b"# cmsd owns budgeted recovery for this adopted instance\n[Service]\nRestart=no\n";
    const ADOPTED: &[u8] = b"# store runtime adopted by cmsd\n[Service]\nRestart=no\n";
    match crate::private_storage::read_control(&path, 64 * 1024)? {
        Some(bytes) if bytes == MANAGED || bytes == ADOPTED => Ok(false),
        Some(_) => Err(
            "model_recovery_dropin_changed: preserve and inspect the existing control record"
                .into(),
        ),
        None => {
            // A unique create-new temporary plus no-replace publication cannot
            // follow a stale or linked fixed temporary file.
            crate::private_storage::publish(&path, MANAGED, false)?;
            Ok(true)
        }
    }
}

/// 每卡 env 文件内容（EnvironmentFile 不支持变量展开，LD_LIBRARY_PATH 直接算好写入）。
/// 参数主体是卡片 .deploy-env；port 可能是分配器另发的（撞端口时）；
/// extra_flags 是卡片生产旗标（enable.sh 提取或覆盖文件）。
pub fn env_file_content(
    deploy: &crate::cards::DeployEnv,
    port: u16,
    max_context: u32,
    extra_flags: &str,
) -> String {
    // 卡片铁律：slots ≥ 2×并发；默认档 16（验证档：并发 8 → slots 16）
    let slots = (2 * deploy.max_concurrency).max(16);
    let mut s = String::new();
    s.push_str(&format!("ENGINE_DIR={}\n", deploy.engine_dir));
    s.push_str(&format!("LD_LIBRARY_PATH={}/lib\n", deploy.engine_dir));
    s.push_str(&format!("MODEL_DIR={}\n", deploy.model_dir));
    s.push_str(&format!("ARTIFACT={}\n", deploy.artifact));
    s.push_str(&format!("HOST={}\n", deploy.host));
    s.push_str(&format!("PORT={port}\n"));
    s.push_str(&format!("MODEL_ID={}\n", deploy.model_id));
    s.push_str(&format!("MAX_CONTEXT={max_context}\n"));
    s.push_str(&format!("MAX_CONCURRENCY={}\n", deploy.max_concurrency));
    s.push_str(&format!("SLOTS={slots}\n"));
    s.push_str(&format!("EXTRA_FLAGS={extra_flags}\n"));
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    fn recovery_fixture(tag: &str) -> (PathBuf, Systemd, PathBuf) {
        use std::os::unix::fs::PermissionsExt;
        let root = crate::docker::fixtures::tmpdir(tag);
        let cli = root.join("systemctl");
        std::fs::write(&cli, format!("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '{}'\ncase \"$*\" in *is-enabled*) echo disabled; exit 1;; *--property=Restart*) echo no;; *daemon-reload*) exit 0;; *) exit 7;; esac\n",root.join("calls").display())).unwrap();
        std::fs::set_permissions(&cli, std::fs::Permissions::from_mode(0o700)).unwrap();
        let sd = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("env"),
            "test".into(),
        )
        .with_test_cli(&cli);
        sd.write_template().unwrap();
        let directory = sd
            .unit_dir
            .join(format!("{}.d", sd.unit_name_for("card-a")));
        std::fs::create_dir(&directory).unwrap();
        std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o755)).unwrap();
        (root, sd, directory)
    }

    #[test]
    fn recovery_fence_keeps_valid_readable_dropins_and_never_follows_fixed_temporary_links() {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        let (root, sd, directory) = recovery_fixture("recovery-dropin-normal");
        let path = directory.join("90-cmsd-recovery.conf");
        let outside = root.join("outside");
        std::fs::write(&outside, b"preserve").unwrap();
        let old_temporary = directory.join(".90-cmsd-recovery.tmp");
        std::os::unix::fs::symlink(&outside, &old_temporary).unwrap();
        sd.fence_recovery(&sd.unit_name_for("card-a")).unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"preserve");
        assert!(std::fs::symlink_metadata(&old_temporary)
            .unwrap()
            .file_type()
            .is_symlink());
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        let inode = std::fs::metadata(&path).unwrap().ino();
        std::fs::write(root.join("calls"), b"").unwrap();
        sd.fence_recovery(&sd.unit_name_for("card-a")).unwrap();
        assert_eq!(std::fs::metadata(&path).unwrap().ino(), inode);
        assert_eq!(std::fs::metadata(&path).unwrap().mode() & 0o777, 0o644);
        assert!(!std::fs::read_to_string(root.join("calls"))
            .unwrap()
            .contains("daemon-reload"));
        let adopted = b"# store runtime adopted by cmsd\n[Service]\nRestart=no\n";
        std::fs::write(&path, adopted).unwrap();
        sd.fence_recovery(&sd.unit_name_for("card-a")).unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), adopted);
        assert_eq!(std::fs::metadata(&path).unwrap().mode() & 0o777, 0o644);
        assert!(!std::fs::read_to_string(root.join("calls"))
            .unwrap()
            .contains("daemon-reload"));
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn recovery_fence_refuses_unknown_dropins_before_backend_commands_or_rewrites() {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        for kind in [
            "dangling",
            "link",
            "fifo",
            "changed",
            "oversized",
            "directory",
            "writable",
        ] {
            let (root, sd, directory) = recovery_fixture(&format!("recovery-dropin-{kind}"));
            let path = directory.join("90-cmsd-recovery.conf");
            let outside = root.join("outside");
            std::fs::write(&outside, b"preserve").unwrap();
            match kind {
                "dangling" => std::os::unix::fs::symlink(root.join("missing"), &path).unwrap(),
                "link" => std::os::unix::fs::symlink(&outside, &path).unwrap(),
                "fifo" => {
                    use std::os::unix::ffi::OsStrExt;
                    let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                }
                "oversized" => std::fs::write(&path, vec![b'x'; 64 * 1024 + 1]).unwrap(),
                "directory" => std::fs::create_dir(&path).unwrap(),
                "writable" => {
                    std::fs::write(&path, b"preserve").unwrap();
                    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666))
                        .unwrap();
                }
                _ => std::fs::write(&path, b"external policy; preserve").unwrap(),
            }
            let inode = std::fs::symlink_metadata(&path).unwrap().ino();
            let started = std::time::Instant::now();
            assert!(
                sd.fence_recovery(&sd.unit_name_for("card-a")).is_err(),
                "{kind}"
            );
            assert!(
                started.elapsed() < std::time::Duration::from_secs(1),
                "{kind}"
            );
            assert_eq!(std::fs::symlink_metadata(&path).unwrap().ino(), inode);
            assert_eq!(std::fs::read(&outside).unwrap(), b"preserve");
            assert!(!root.join("calls").exists(), "{kind}");
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn recovery_fence_refuses_a_linked_dropin_directory() {
        let (root, sd, directory) = recovery_fixture("recovery-dropin-directory-link");
        std::fs::remove_dir(&directory).unwrap();
        let outside = root.join("outside");
        std::fs::create_dir(&outside).unwrap();
        std::os::unix::fs::symlink(&outside, &directory).unwrap();
        assert!(sd.fence_recovery(&sd.unit_name_for("card-a")).is_err());
        assert!(std::fs::read_dir(outside).unwrap().next().is_none());
        assert!(!root.join("calls").exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn model_uid_uses_the_execution_account_and_fails_closed() {
        let root = crate::docker::fixtures::tmpdir("model-run-uid");
        let mut backend = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("env"),
            "unknown-account".into(),
        );
        assert_eq!(backend.model_run_uid().unwrap(), unsafe { libc::geteuid() });
        backend.mode = SystemdMode::System;
        backend.run_user = "root".into();
        assert_eq!(backend.model_run_uid().unwrap(), 0);
        backend.run_user = unsafe { libc::geteuid() }.to_string();
        assert_eq!(backend.model_run_uid().unwrap(), unsafe { libc::geteuid() });
        for account in [
            "",
            "cmsd-nonexistent-execution-account",
            "4294967296",
            "bad\0account",
        ] {
            backend.run_user = account.into();
            assert!(backend.model_run_uid().is_err(), "{account:?}");
        }
    }

    #[test]
    fn independent_state_namespaces_never_rewrite_the_shared_legacy_template() {
        let root = crate::docker::fixtures::tmpdir("systemd-state-scope");
        let units = root.join("units");
        std::fs::create_dir_all(&units).unwrap();
        let legacy = units.join("megacube-model@.service");
        std::fs::write(&legacy, b"foreign template must remain").unwrap();
        let make = |state: &str| {
            Systemd::new(
                SystemdMode::User,
                units.clone(),
                root.join(state).join("env"),
                "test".into(),
            )
            .with_state_namespace(true)
        };
        let a = make("a");
        let b = make("b");
        assert_ne!(a.unit_name_for("same-card"), b.unit_name_for("same-card"));
        assert_eq!(
            a.unit_name_for("same-card"),
            make("a").unit_name_for("same-card")
        );
        assert!(a.write_template().unwrap());
        assert!(b.write_template().unwrap());
        assert!(!a.write_template().unwrap());
        assert_eq!(
            std::fs::read(legacy).unwrap(),
            b"foreign template must remain"
        );
        assert!(a.card_from_unit(&b.unit_name_for("same-card")).is_none());
        assert_eq!(
            a.card_from_unit(&a.unit_name_for("same-card")).as_deref(),
            Some("same-card")
        );
        assert!(a.assert_template_owner().is_ok());
        assert!(b.assert_template_owner().is_ok());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn known_previous_template_upgrades_only_with_the_same_state_and_keeps_backup() {
        let root = crate::docker::fixtures::tmpdir("systemd-template-upgrade");
        let units = root.join("units");
        std::fs::create_dir_all(&units).unwrap();
        let sd = Systemd::new(
            SystemdMode::User,
            units.clone(),
            root.join("state/env"),
            "test".into(),
        )
        .with_state_namespace(true);
        let old = sd.previous_template_content();
        std::fs::write(sd.template_path(), &old).unwrap();
        assert!(sd.assert_template_owner().is_ok());
        assert!(sd.write_template().unwrap());
        assert_eq!(
            std::fs::read_to_string(sd.template_path()).unwrap(),
            sd.template_content()
        );
        assert_eq!(
            std::fs::read_to_string(
                sd.template_path()
                    .with_extension("service.cmsd-before-profile-v1")
            )
            .unwrap(),
            old
        );
        assert!(!sd.write_template().unwrap());
        std::fs::write(sd.template_path(), old.replace("state/env", "another/env")).unwrap();
        assert!(sd.write_template().is_err());
    }

    #[test]
    fn template_upgrade_keeps_matching_readable_backup_aliases_and_directory_aliases() {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        for kind in ["ordinary", "link", "hardlink"] {
            let root = crate::docker::fixtures::tmpdir(&format!(
                "systemd-template-backup-existing-{kind}"
            ));
            let units = root.join("units");
            std::fs::create_dir(&units).unwrap();
            let alias = root.join("alias");
            std::os::unix::fs::symlink(&units, &alias).unwrap();
            let sd = Systemd::new(
                SystemdMode::User,
                alias,
                root.join("state/env"),
                "test".into(),
            );
            let old = sd.previous_template_content();
            std::fs::write(sd.template_path(), &old).unwrap();
            let backup = sd
                .template_path()
                .with_extension("service.cmsd-before-profile-v1");
            let outside = root.join("outside");
            std::fs::write(&outside, &old).unwrap();
            std::fs::set_permissions(&outside, std::fs::Permissions::from_mode(0o444)).unwrap();
            match kind {
                "link" => std::os::unix::fs::symlink(&outside, &backup).unwrap(),
                "hardlink" => std::fs::hard_link(sd.template_path(), &backup).unwrap(),
                _ => {
                    std::fs::write(&backup, &old).unwrap();
                    std::fs::set_permissions(&backup, std::fs::Permissions::from_mode(0o644))
                        .unwrap();
                }
            }
            let metadata = std::fs::symlink_metadata(&backup).unwrap();
            assert!(sd.write_template().unwrap(), "{kind}");
            assert_eq!(std::fs::read(&backup).unwrap(), old.as_bytes(), "{kind}");
            assert_eq!(
                std::fs::symlink_metadata(&backup).unwrap().ino(),
                metadata.ino(),
                "{kind}"
            );
            assert_eq!(
                std::fs::symlink_metadata(&backup).unwrap().mode(),
                metadata.mode(),
                "{kind}"
            );
            assert_eq!(std::fs::read(&outside).unwrap(), old.as_bytes(), "{kind}");
            assert!(!sd.write_template().unwrap(), "{kind}");
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn unknown_template_backups_are_bounded_and_preserved_before_upgrade() {
        use std::os::unix::ffi::OsStrExt;
        use std::os::unix::fs::MetadataExt;
        for kind in ["fifo", "dangling", "different", "oversized", "directory"] {
            let root = crate::docker::fixtures::tmpdir(&format!("systemd-template-backup-{kind}"));
            let sd = Systemd::new(
                SystemdMode::User,
                root.join("units"),
                root.join("state/env"),
                "test".into(),
            );
            std::fs::create_dir(&sd.unit_dir).unwrap();
            let old = sd.previous_template_content();
            let template = sd.template_path();
            std::fs::write(&template, &old).unwrap();
            let backup = template.with_extension("service.cmsd-before-profile-v1");
            let outside = root.join("outside");
            std::fs::write(&outside, &old).unwrap();
            match kind {
                "fifo" => {
                    let name = std::ffi::CString::new(backup.as_os_str().as_bytes()).unwrap();
                    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                }
                "dangling" => std::os::unix::fs::symlink(root.join("absent"), &backup).unwrap(),
                "different" => std::fs::write(&backup, b"foreign backup retained").unwrap(),
                "oversized" => std::fs::write(&backup, vec![b'x'; 64 * 1024 + 1]).unwrap(),
                _ => std::fs::create_dir(&backup).unwrap(),
            }
            let template_inode = std::fs::symlink_metadata(&template).unwrap().ino();
            let backup_metadata = std::fs::symlink_metadata(&backup).unwrap();
            let (sender, receiver) = std::sync::mpsc::channel();
            std::thread::spawn(move || sender.send(sd.write_template()).unwrap());
            assert!(
                receiver
                    .recv_timeout(std::time::Duration::from_secs(1))
                    .expect("template backup reads must be bounded")
                    .is_err(),
                "{kind}"
            );
            assert_eq!(std::fs::read(&template).unwrap(), old.as_bytes(), "{kind}");
            assert_eq!(
                std::fs::symlink_metadata(&template).unwrap().ino(),
                template_inode,
                "{kind}"
            );
            let after = std::fs::symlink_metadata(&backup).unwrap();
            assert_eq!(after.ino(), backup_metadata.ino(), "{kind}");
            assert_eq!(after.mode(), backup_metadata.mode(), "{kind}");
            assert_eq!(std::fs::read(&outside).unwrap(), old.as_bytes(), "{kind}");
            std::fs::remove_dir_all(root).unwrap();
        }
    }

    #[test]
    fn legacy_template_conflict_is_fail_closed_without_writing_either_state() {
        let root = crate::docker::fixtures::tmpdir("systemd-legacy-scope");
        let a = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("a/env"),
            "test".into(),
        );
        let b = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("b/env"),
            "test".into(),
        );
        assert!(a.write_template().unwrap());
        let before = std::fs::read(a.template_path()).unwrap();
        assert!(b.write_template().is_err());
        assert!(b.assert_template_owner().is_err());
        assert_eq!(std::fs::read(a.template_path()).unwrap(), before);
        assert!(!root.join("b").exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn linked_template_is_rejected_and_its_target_is_preserved() {
        let root = crate::docker::fixtures::tmpdir("systemd-link-scope");
        let sd = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("env"),
            "test".into(),
        );
        std::fs::create_dir_all(&sd.unit_dir).unwrap();
        let external = root.join("external");
        std::fs::write(&external, b"preserve").unwrap();
        std::os::unix::fs::symlink(&external, sd.template_path()).unwrap();
        assert!(sd.write_template().is_err());
        assert_eq!(std::fs::read(external).unwrap(), b"preserve");
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn namespace_record_blocks_unplanned_migration_and_incomplete_native_backup() {
        let root = crate::docker::fixtures::tmpdir("systemd-scope-record");
        let legacy = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("state/env"),
            "test".into(),
        );
        let scoped = Systemd::new(
            SystemdMode::User,
            root.join("units"),
            root.join("state/env"),
            "test".into(),
        )
        .with_state_namespace(true);
        assert!(scoped.claim_namespace(&root.join("state"), true).is_err());
        assert!(!root.join("state/native-unit-namespace-v1").exists());
        scoped.claim_namespace(&root.join("state"), false).unwrap();
        let before = std::fs::read(root.join("state/native-unit-namespace-v1")).unwrap();
        scoped.claim_namespace(&root.join("state"), true).unwrap();
        assert!(legacy.claim_namespace(&root.join("state"), true).is_err());
        assert_eq!(
            std::fs::read(root.join("state/native-unit-namespace-v1")).unwrap(),
            before
        );
        std::fs::write(root.join("state/native-unit-namespace-v1"), b"corrupt").unwrap();
        assert!(scoped.claim_namespace(&root.join("state"), false).is_err());
        assert_eq!(
            std::fs::read(root.join("state/native-unit-namespace-v1")).unwrap(),
            b"corrupt"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
    use crate::cards::DeployEnv;

    fn deploy(concurrency: u32) -> DeployEnv {
        DeployEnv {
            port: 18080,
            host: "127.0.0.1".to_string(),
            model_dir: "/models/Ornith".to_string(),
            engine_dir: "/engines/0f8a4a7c".to_string(),
            artifact: "ornith.tachyon".to_string(),
            model_id: "Ornith-1.5-35B-A3B-NVFP4".to_string(),
            max_concurrency: concurrency,
        }
    }

    #[test]
    fn env_file_follows_card_rules() {
        let s = env_file_content(
            &deploy(1),
            18080,
            32768,
            "--kv-dtype fp8 --spec mtp --draft-tokens 5",
        );
        assert!(s.contains("PORT=18080\n"));
        assert!(s.contains("SLOTS=16\n")); // max(2*1, 16)
        assert!(s.contains("LD_LIBRARY_PATH=/engines/0f8a4a7c/lib\n"));
        assert!(s.contains("EXTRA_FLAGS=--kv-dtype fp8 --spec mtp --draft-tokens 5\n"));
        let s8 = env_file_content(&deploy(12), 18080, 32768, "");
        assert!(s8.contains("SLOTS=24\n")); // 2*12 > 16
    }

    #[test]
    fn template_has_rate_limit_and_restart() {
        let sd = Systemd::new(
            SystemdMode::User,
            PathBuf::from("/tmp/u"),
            PathBuf::from("/tmp/env"),
            "test".to_string(),
        );
        let t = sd.template_content();
        assert!(t.contains("StartLimitBurst=3"));
        assert!(t.contains("StartLimitIntervalSec=300"));
        assert!(t.contains("Restart=on-failure"));
        assert!(t.contains("EnvironmentFile=/tmp/env/%i.env"));
        assert!(t.contains("WantedBy=default.target"));
        assert!(!t.contains("User="));
        // 骨架旗标 + EXTRA_FLAGS 注入点（模型相关旗标不在模板里）
        assert!(t.contains("ExecStart=/bin/sh -c 'exec \"$$ENGINE_DIR/tachyon-serve\""));
        let exec_line = t.lines().find(|l| l.starts_with("ExecStart=")).unwrap();
        assert!(exec_line.ends_with("$$EXTRA_FLAGS'"), "{exec_line}");
        for flag in [
            "--host \"$$HOST\"",
            "--port \"$$PORT\"",
            "--model-id \"$$MODEL_ID\"",
            "--max-context \"$$MAX_CONTEXT\"",
            "--device-state-slots \"$${DEVICE_STATE_SLOTS:-$$SLOTS}\"",
        ] {
            assert!(t.contains(flag), "模板缺 {flag}");
        }
    }

    #[test]
    fn system_template_has_user() {
        let sd = Systemd::new(
            SystemdMode::System,
            PathBuf::from("/etc/systemd/system"),
            PathBuf::from("/var/lib/megacube/env"),
            "test".to_string(),
        );
        let t = sd.template_content();
        assert!(t.contains("User=test"));
        assert!(t.contains("WantedBy=multi-user.target"));
    }

    #[test]
    fn unit_state_helpers() {
        let u = UnitState {
            active_state: "failed".to_string(),
            result: "start-limit-hit".to_string(),
            ..UnitState::default()
        };
        assert!(u.start_limit_hit());
        assert!(!u.is_active());
    }
}
