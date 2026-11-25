//! Persistent configuration, the durable RPC idempotency ledger, pending
//! revocation replay, and user-custom Relay settings.
//!
//! These helpers are stateless and touch only the private JSON files under the
//! pinvou3 home directory. They never lock `Inner`; the facade reads the result
//! and updates `Inner` under its own guard.

use std::collections::HashSet;
use std::fs::{File, OpenOptions};
use std::io::Read;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::{
    ActiveEndpoint, PendingRevocation, PendingRevocationLedger, RpcLedger, WebAccessConfig,
    WebAccessInfo, platform,
};
use crate::platform::paths;

/// Round-15 review: a recognizable prefix for queue-full errors — stop and
/// refresh are recovery paths, and this prefix downgrades that error class to
/// "log and continue" instead of wedging disable/rotate; conflicts with the
/// same key but a different secret stay fail-closed (design boundary recorded
/// in round-14). A string prefix rather than an enum: the module's error
/// surface is plain String, consistent with existing style; callers
/// discriminate with `starts_with`.
pub(super) const PENDING_REVOCATION_QUEUE_FULL_PREFIX: &str =
    "pending Web revocation queue is full";

pub(super) fn pairing_info(endpoint: &ActiveEndpoint) -> WebAccessInfo {
    WebAccessInfo {
        endpoint_id: endpoint.config.endpoint_id.clone(),
        url: endpoint.url.clone(),
        qr_data_url: crate::features::connectors::connector_cli::make_qr(&endpoint.url),
        status: endpoint.status,
        revocation_disposition: None,
    }
}

pub(super) fn fresh_config(allow_host_workspace: bool) -> WebAccessConfig {
    WebAccessConfig {
        relay_url: remote_relay_ws_url(),
        endpoint_id: format!("ep_{}", crate::features::remote_control::short_token(24)),
        access_token: crate::features::remote_control::short_token(48),
        desktop_secret: crate::features::remote_control::short_token(48),
        allow_host_workspace,
    }
}

/// Uniform ws(s) URL validation: length cap, `ws://` or `wss://` prefix, no
/// whitespace; the caller keeps the failure message verbatim.
fn validate_ws_url(url: &str, invalid_message: &str) -> Result<(), String> {
    if url.len() > 2_048
        || !(url.starts_with("ws://") || url.starts_with("wss://"))
        || url.chars().any(char::is_whitespace)
    {
        return Err(invalid_message.to_string());
    }
    Ok(())
}

pub(super) fn validate_config(config: &WebAccessConfig) -> Result<(), String> {
    validate_ws_url(&config.relay_url, "Web access relay_url is invalid")?;
    if config.endpoint_id.len() < 8
        || config.endpoint_id.len() > 128
        || !config
            .endpoint_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err("Web access endpoint_id is invalid".to_string());
    }
    if !(24..=1_024).contains(&config.access_token.len())
        || !(24..=1_024).contains(&config.desktop_secret.len())
    {
        return Err("Web access credentials are invalid".to_string());
    }
    Ok(())
}

pub(super) fn config_path() -> PathBuf {
    paths::pinvou3_home().join("web-access.json")
}

pub(super) fn process_lock_path() -> PathBuf {
    paths::pinvou3_home().join("web-access.lock")
}

pub(super) fn pending_revocations_path() -> PathBuf {
    paths::pinvou3_home().join("web-access-pending-revocations.json")
}

/// Acquire the process-ownership lock (single desktop owner for Web access).
///
/// Uses `fd_lock` (already in the dependency graph via codewhale-config)
/// instead of fs2; both wrap the same OS primitive (flock on Unix,
/// LockFileEx on Windows). On success the backing allocation is kept alive
/// for the guard's `'static` lifetime: the ownership lock lives until process
/// exit anyway, and dropping the guard (or the process) releases it. On the
/// contended path the allocation is reclaimed so a failed attempt does not
/// strand an unlocked fd for the rest of the process (fs2 closed its file
/// there as well).
pub(super) fn acquire_process_lock(
    path: &Path,
) -> Result<fd_lock::RwLockWriteGuard<'static, File>, String> {
    let parent = path
        .parent()
        .ok_or_else(|| format!("invalid Web access lock path: {}", path.display()))?;
    std::fs::create_dir_all(parent)
        .map_err(|error| format!("create {}: {error}", parent.display()))?;
    let mut options = OpenOptions::new();
    options.read(true).write(true).create(true);
    platform::configure_private_open_options(&mut options);
    let file = options
        .open(path)
        .map_err(|error| format!("open {}: {error}", path.display()))?;
    platform::enforce_private_permissions(&file, path)
        .map_err(|error| format!("set private permissions on {}: {error}", path.display()))?;
    // SAFETY: `lock` is an owned `Box::into_raw` allocation with no other
    // references. The `&mut` below lives only for the `try_write` call unless
    // a guard is returned; a returned guard carries it for `'static`, which
    // is intended — the process-ownership lock is held until process exit and
    // the allocation is deliberately never freed on that path.
    let lock = Box::into_raw(Box::new(fd_lock::RwLock::new(file)));
    // SAFETY: lock is the sole owning pointer handed out by Box::into_raw with no
    // other references; the &mut lives only for the try_write call, a successfully
    // returned guard carries that borrow (deliberately held until process exit,
    // with the allocation never freed), and the failure path reclaims it via
    // Box::from_raw below.
    let guard = match unsafe { &mut *lock }.try_write() {
        Ok(guard) => guard,
        Err(error) => {
            // SAFETY: `try_write` failed, so no guard borrows the lock and the
            // `&mut` above has expired. Re-taking the Box is sound; dropping
            // it closes the fd instead of stranding it for the process
            // lifetime.
            unsafe { drop(Box::from_raw(lock)) };
            return Err(format!(
                "Web access is already owned by another desktop process ({}): {error}",
                path.display()
            ));
        }
    };
    Ok(guard)
}

pub(super) fn rpc_ledger_path() -> PathBuf {
    paths::pinvou3_home().join("web-access-rpc-ledger.json")
}

pub(super) fn load_config() -> Result<Option<WebAccessConfig>, String> {
    load_config_from(&config_path())
}

pub(super) fn load_config_from(path: &Path) -> Result<Option<WebAccessConfig>, String> {
    let file = match File::open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(format!("read {}: {error}", path.display())),
    };
    // 读取即修复,且先于任何内容读取:损坏 + 放宽权限的组合也不能把一个
    // 世界可读的凭据文件留在盘上(备份恢复或外部工具可能把它改成组/全员
    // 可读)。修复必须挂在**第一个**句柄上、先于 `read_to_string`——非
    // UTF-8 的损坏内容会让读取腿先行报错返回,挂在读取之后的修复永远不会
    // 执行,放宽权限的损坏文件就一直裸留在盘上。与写入路径的 0600 基线
    // 保持一致;修复失败按读取错误上抛,同时免去第二次 open 的 TOCTOU
    // (两次打开之间路径可被换掉)。Windows/fallback 平台该调用是显式
    // no-op。
    platform::enforce_private_permissions(&file, path).map_err(|error| {
        // 修复自身失败(fchmod 需要 owner 权限,备份恢复可能留下他者属主的
        // 世界可读密文件)时,不能把它一直留在众所周知的路径上。普通 rename
        // 会保留放宽的 mode 位,隔离副本在新名字下依旧世界可读——改走
        // 0600 副本 + unlink 原件(隔离副本是本进程创建的,fchmod 在其上
        // 必然成功;对原件的权限来自父目录写位,不依赖文件属主)。目录占位
        // (EISDIR fixture)等非普通文件退回 rename 语义;隔离彻底失败
        // (目录只读/Windows 文件锁)则保持原错误——那与目录不可写同级,
        // 后续写入同样会失败。
        match crate::platform::filesystem::quarantine_secret_file(path) {
            Ok(quarantined) => eprintln!(
                "[web-access] could not restrict permissions on {} ({error}); \
                     quarantined to {} (copy tightened to 0600) and the next \
                     start creates a fresh 0600 file",
                path.display(),
                quarantined.display()
            ),
            Err(quarantine_error) => eprintln!(
                "[web-access] could not restrict permissions on {} ({error}) and the \
                     quarantine also failed ({quarantine_error}); the loose file \
                     stays in place",
                path.display()
            ),
        }
        format!("restrict {}: {error}", path.display())
    })?;
    let mut text = String::new();
    file.take((super::MAX_WEB_ACCESS_CONFIG_BYTES + 1) as u64)
        .read_to_string(&mut text)
        .map_err(|error| format!("read {}: {error}", path.display()))?;
    if text.len() > super::MAX_WEB_ACCESS_CONFIG_BYTES {
        return Err(format!(
            "Web access config is larger than {} KiB: {}",
            super::MAX_WEB_ACCESS_CONFIG_BYTES / 1024,
            path.display()
        ));
    }
    let config: WebAccessConfig = serde_json::from_str(&text)
        .map_err(|error| format!("parse {}: {error}", path.display()))?;
    validate_config(&config)?;
    Ok(Some(config))
}

pub(super) fn persist_config(config: &WebAccessConfig) -> Result<(), String> {
    validate_config(config)?;
    atomic_write_private_json(&config_path(), config, "Web access config")
}

pub(super) fn remove_config() -> Result<(), String> {
    remove_private_file(&config_path())
}

// ── 用户自定义 Relay 地址 ─────────────────────────────────────────────
// 设置页填写的域名/IP 规范化为「WebSocket 地址 + 页面基址」一对后持久化；
// 生效优先级：运行时 env 覆盖 > PINVOU_REMOTE_* env > 本设置 > 内置默认。

const RELAY_DEFAULT_BASE_PATH: &str = "/pinvou3/remote";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RelaySettings {
    pub relay_url: String,
    pub public_base_url: String,
    /// 内置端点 profile 选择（production / test）。缺省或未知时回退 production，
    /// 保证旧版本写下的 web-relay.json 仍可加载。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub profile: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct RelaySettingsInfo {
    pub relay_url: String,
    pub public_base_url: String,
    pub custom: bool,
    pub default_relay_url: String,
    pub default_public_base_url: String,
    /// 当前生效的内置 profile id（供 UI 显示"服务端点"）。
    pub profile: String,
}

/// 持久化的端点 profile 选择（`~/.pinvou3/web-relay.json` 的 `profile` 字段）。
pub(super) fn selected_endpoint_profile() -> Option<String> {
    load_relay_settings().and_then(|settings| settings.profile)
}

pub(super) fn relay_settings_path() -> PathBuf {
    paths::pinvou3_home().join("web-relay.json")
}

/// http(s) 页面基址统一校验，与 `validate_ws_url` 同一基线。
fn validate_http_base_url(url: &str, invalid_message: &str) -> Result<(), String> {
    if url.len() > 2_048
        || !(url.starts_with("http://") || url.starts_with("https://"))
        || url.chars().any(char::is_whitespace)
    {
        return Err(invalid_message.to_string());
    }
    Ok(())
}

pub(super) fn validate_relay_settings(settings: &RelaySettings) -> Result<(), String> {
    validate_ws_url(
        &settings.relay_url,
        "relay address does not normalize to a valid ws(s) URL",
    )?;
    validate_http_base_url(
        &settings.public_base_url,
        "relay address does not normalize to a valid http(s) base",
    )
}

/// 把用户填写的 Relay 地址规范化。接受裸域名/IP（默认按 TLS 处理成 wss/https）、
/// `ws(s)://`、`http(s)://` 前缀以及可选自定义路径；无 TLS 证书的环境需要显式
/// 写 `ws://` 或 `http://` 前缀，不做静默降级。
pub(super) fn normalize_relay_address(input: &str) -> Result<RelaySettings, String> {
    let trimmed = input.trim();
    if trimmed.is_empty() {
        return Err("relay address is empty".to_string());
    }
    if trimmed.chars().any(char::is_whitespace) {
        return Err("relay address must not contain whitespace".to_string());
    }
    if trimmed.len() > 1_024 {
        return Err("relay address is too long".to_string());
    }
    let (ws_scheme, http_scheme, rest) = if let Some(rest) = trimmed.strip_prefix("wss://") {
        ("wss", "https", rest)
    } else if let Some(rest) = trimmed.strip_prefix("ws://") {
        ("ws", "http", rest)
    } else if let Some(rest) = trimmed.strip_prefix("https://") {
        ("wss", "https", rest)
    } else if let Some(rest) = trimmed.strip_prefix("http://") {
        ("ws", "http", rest)
    } else if trimmed.contains("://") {
        return Err("relay address only supports ws/wss/http/https".to_string());
    } else {
        ("wss", "https", trimmed)
    };
    if rest.contains('?') || rest.contains('#') || rest.contains('@') {
        return Err("relay address must not contain userinfo, query or fragment".to_string());
    }
    let (host, path) = match rest.find('/') {
        Some(index) => (&rest[..index], rest[index..].trim_end_matches('/')),
        None => (rest, ""),
    };
    if host.is_empty() {
        return Err("relay address is missing a host".to_string());
    }
    // 路径规则：不填 → 内置 /pinvou3/remote；填了 → 尊重自定义（容忍粘贴完整
    // ws 地址时结尾多出的 /ws，避免「复制现有 relay_url 再保存」变成 /ws/ws）。
    let base_path = if path.is_empty() {
        RELAY_DEFAULT_BASE_PATH.to_string()
    } else {
        path.strip_suffix("/ws").unwrap_or(path).to_string()
    };
    let settings = RelaySettings {
        relay_url: format!("{ws_scheme}://{host}{base_path}/ws"),
        public_base_url: format!("{http_scheme}://{host}{base_path}"),
        profile: None,
    };
    validate_relay_settings(&settings)?;
    Ok(settings)
}

/// 读取失败（不存在/损坏/非法）一律回落默认 relay：设置文件不是凭据，宁可
/// 降级也不把「启用 Web 访问」整个卡死；重新保存会原子覆盖坏文件。
pub(super) fn load_relay_settings() -> Option<RelaySettings> {
    let file = File::open(relay_settings_path()).ok()?;
    let mut text = String::new();
    file.take((super::MAX_WEB_ACCESS_CONFIG_BYTES + 1) as u64)
        .read_to_string(&mut text)
        .ok()?;
    if text.len() > super::MAX_WEB_ACCESS_CONFIG_BYTES {
        return None;
    }
    let settings: RelaySettings = serde_json::from_str(&text).ok()?;
    validate_relay_settings(&settings).ok()?;
    Some(settings)
}

pub(super) fn pending_revocation_key(pending: &PendingRevocation) -> String {
    format!("{}\n{}", pending.relay_url, pending.endpoint_id)
}

pub(super) fn validate_pending_revocation(pending: &PendingRevocation) -> Result<(), String> {
    validate_ws_url(
        &pending.relay_url,
        "pending Web revocation has an invalid relay_url",
    )?;
    if pending.endpoint_id.len() < 4
        || pending.endpoint_id.len() > 128
        || !pending
            .endpoint_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err("pending Web revocation has an invalid endpoint_id".to_string());
    }
    if pending.desktop_secret.len() < 24 || pending.desktop_secret.len() > 1_024 {
        return Err("pending Web revocation has an invalid desktop secret".to_string());
    }
    Ok(())
}

pub(super) fn validate_pending_revocation_ledger(
    ledger: &PendingRevocationLedger,
) -> Result<(), String> {
    if ledger.version != super::PENDING_REVOCATIONS_VERSION {
        return Err(format!(
            "unsupported pending Web revocation version {}",
            ledger.version
        ));
    }
    if ledger.entries.len() > super::MAX_PENDING_REVOCATIONS {
        return Err(format!(
            "pending Web revocation count exceeds {}",
            super::MAX_PENDING_REVOCATIONS
        ));
    }
    let mut keys = HashSet::new();
    for pending in &ledger.entries {
        validate_pending_revocation(pending)?;
        if !keys.insert(pending_revocation_key(pending)) {
            return Err(format!(
                "duplicate pending Web revocation for {}",
                pending.endpoint_id
            ));
        }
    }
    Ok(())
}

/// Dedicated to the replay startup path: a Corrupt original has already been
/// quarantined at read time and we continue with an empty ledger — the same
/// "recovery paths are not wedged by a bad file" convention as
/// queue/acknowledge, so enable/resume/rotate no longer hard-fail once over
/// a file that has already been cleaned up (the original threw the parse
/// error straight at the user; a retry would self-heal, but the root cause
/// no longer exists). Unavailable is still transient IO and keeps
/// propagating.
pub(super) fn load_pending_revocations_for_replay() -> Result<PendingRevocationLedger, String> {
    match load_pending_revocations_classified(&pending_revocations_path()) {
        RevocationLedgerLoad::Ledger(ledger) => Ok(ledger),
        RevocationLedgerLoad::Corrupt(error) => {
            eprintln!("[web-access] pending-revocation ledger is corrupt, starting fresh: {error}");
            Ok(PendingRevocationLedger::default())
        }
        RevocationLedgerLoad::Unavailable(error) => Err(error),
    }
}

/// Strict read (both Corrupt and Unavailable error): production paths all go
/// through the classified version or the replay wrapper; this function only
/// serves tests asserting directly against real on-disk files.
#[cfg(test)]
pub(super) fn load_pending_revocations_from(
    path: &Path,
) -> Result<PendingRevocationLedger, String> {
    match load_pending_revocations_classified(path) {
        RevocationLedgerLoad::Ledger(ledger) => Ok(ledger),
        RevocationLedgerLoad::Corrupt(error) | RevocationLedgerLoad::Unavailable(error) => {
            Err(error)
        }
    }
}

/// Three-way classification of pending-revocation ledger reads: Corrupt
/// means unparseable or oversize content (nothing salvageable; the original
/// was quarantined at read time as evidence) and retries are pointless; a
/// parseable ledger that fails whole-file validation is salvaged entry by
/// entry into a Ledger at read time and does not come through here.
/// Unavailable is transient IO where retrying helps. The queue/acknowledge
/// paths use it to decide "move the bad ledger aside and continue" vs
/// "propagate and wait for a retry" — disable/rotate are recovery paths and
/// must not be wedged permanently by one bad file.
enum RevocationLedgerLoad {
    Ledger(PendingRevocationLedger),
    Corrupt(String),
    Unavailable(String),
}

/// Names an endpoint id inside a `[web-access]` log line without opening a
/// log-forging surface: capacity paths name entries whose endpoint_id values
/// are not per-entry validated yet, so a planted/corrupt ledger could
/// otherwise carry control characters and ANSI sequences straight into the
/// desktop log. Clip each id to the same charset the validation uses,
/// marking lossily clipped entries corrupt. Shared by the salvage
/// truncation and the backlog cap retire path.
pub(super) fn clip_pending_endpoint_id_for_log(endpoint_id: &str) -> String {
    let clipped: String = endpoint_id
        .chars()
        .filter(|character| {
            character.is_ascii_alphanumeric() || *character == '-' || *character == '_'
        })
        .collect();
    if clipped == endpoint_id {
        clipped
    } else {
        format!("{clipped}~corrupt")
    }
}

/// Salvages, entry by entry, a parseable ledger that failed whole-file
/// validation: a version drift, an oversize entry, a corrupt entry, or a
/// duplicate key invalidates only its own entry, not the remaining valid
/// pending revocations — dropping everything would keep the old endpoint
/// joinable at the relay. Each entry goes through the same
/// `validate_pending_revocation` strictness as a normal load; the first of a
/// duplicate key wins; capacity caps the salvage.
fn salvage_pending_revocations(ledger: &PendingRevocationLedger) -> PendingRevocationLedger {
    let mut salvaged = PendingRevocationLedger::default();
    let mut keys = HashSet::new();
    for (index, pending) in ledger.entries.iter().enumerate() {
        if salvaged.entries.len() >= super::MAX_PENDING_REVOCATIONS {
            // Round-15 review NIT: a capacity truncation must report the
            // count explicitly — silently dropped entries mean the
            // corresponding endpoints stay joinable at the relay until
            // manual cleanup.
            // Round-16 review: beyond the count, name each one (which
            // credentials are still relay-side joinable is the input to
            // manual cleanup); the truncated set is at most the total pending
            // count and this path is already the exceptional branch, so the
            // per-entry log volume is acceptable.
            // Round-20 review (S10-2): the truncation runs before per-entry
            // validation, so the named ids go through the shared log-forging
            // clip helper (round-29 review: extracted so the backlog cap
            // retire path names ids with the same strictness).
            let dropped: Vec<String> = ledger
                .entries
                .iter()
                .skip(index)
                .map(|pending| clip_pending_endpoint_id_for_log(&pending.endpoint_id))
                .collect();
            eprintln!(
                "[web-access] salvage dropped {} pending revocations past the capacity of {} (endpoints stay joinable at the relay until revoked manually): {}",
                ledger.entries.len() - index,
                super::MAX_PENDING_REVOCATIONS,
                dropped.join(", ")
            );
            break;
        }
        if validate_pending_revocation(pending).is_err() {
            continue;
        }
        if !keys.insert(pending_revocation_key(pending)) {
            continue;
        }
        salvaged.entries.push_back(pending.clone());
    }
    salvaged
}

/// 隔离损坏账本原件留证。返回隔离是否成功——调用方据此决定空抢救集是否
/// 可以按空账本语义移除路径(隔离彻底失败时原件是仅存的证据,不得随后被
/// 删除)。
fn quarantine_pending_ledger(path: &Path, data: &[u8]) -> bool {
    match crate::platform::filesystem::quarantine_corrupt_file(path) {
        Ok(quarantined) => {
            eprintln!(
                "[web-access] quarantined corrupt pending-revocation ledger to {}",
                quarantined.display()
            );
            true
        }
        Err(rename_error) => {
            // A failed rename (Windows AV/backup locks etc.) must not
            // silently allow the evidence to be overwritten/deleted in place
            // by a later fresh-start write. First copy the whole file (in the
            // oversize case the in-memory `data` holds only the truncated
            // prefix — the complete evidence is on disk); if the copy fails
            // too, fall back to the in-memory bytes; only if both fail
            // degrade to logging alone.
            match crate::platform::filesystem::quarantine_corrupt_file_copy(path) {
                Ok(quarantined) => eprintln!(
                    "[web-access] rename failed ({rename_error}); copied corrupt pending-revocation ledger to {}",
                    quarantined.display()
                ),
                Err(copy_error) => {
                    match crate::platform::filesystem::quarantine_corrupt_file_bytes(path, data) {
                        Ok(quarantined) => eprintln!(
                            "[web-access] rename failed ({rename_error}, copy failed: {copy_error}); \
                             wrote in-memory bytes of corrupt pending-revocation ledger to {}",
                            quarantined.display()
                        ),
                        Err(error) => {
                            eprintln!(
                                "[web-access] failed to quarantine corrupt pending-revocation ledger {}: {error}",
                                path.display()
                            );
                            return false;
                        }
                    }
                }
            }
            true
        }
    }
}

fn load_pending_revocations_classified(path: &Path) -> RevocationLedgerLoad {
    let file = match File::open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return RevocationLedgerLoad::Ledger(PendingRevocationLedger::default());
        }
        Err(error) => {
            return RevocationLedgerLoad::Unavailable(format!("read {}: {error}", path.display()));
        }
    };
    // 读取即修复,且先于任何内容读取(与 load_config_from 同形):该文件
    // 持有 desktop_secret,损坏 + 放宽权限的组合也不能把世界可读的密文件
    // 留在盘上。修复挂在**第一个**句柄上——读取腿的真实 I/O 错误
    // (EISDIR/EACCES 等)会先行返回,挂在读取之后的修复永远不会执行,
    // 放宽权限的损坏文件就一直裸留在盘上;同时免去第二次 open 的 TOCTOU
    // (两次打开之间路径可被换掉,修复的可能不是读到的那个 inode)。
    // (Windows/fallback 平台该调用是显式 no-op。)
    if let Err(error) = platform::enforce_private_permissions(&file, path) {
        // 修复自身失败时同样把放宽权限的密文件隔离挪离众所周知的路径
        // (与 load_config_from 同因同策):改走 0600 副本 + unlink 原件,
        // 隔离副本不再继承放宽的 mode 位;非普通文件退回 rename;隔离失败
        // 则保持 Unavailable 原错误。
        match crate::platform::filesystem::quarantine_secret_file(path) {
            Ok(quarantined) => eprintln!(
                "[web-access] could not restrict permissions on {} ({error}); \
                 quarantined to {} (copy tightened to 0600) and the next \
                 write creates a fresh 0600 ledger",
                path.display(),
                quarantined.display()
            ),
            Err(quarantine_error) => eprintln!(
                "[web-access] could not restrict permissions on {} ({error}) and the \
                     quarantine also failed ({quarantine_error}); the loose file \
                     stays in place",
                path.display()
            ),
        }
        return RevocationLedgerLoad::Unavailable(format!("restrict {}: {error}", path.display()));
    }
    // The same take()-bounded read as the config loader: truncate at the cap
    // first, then check — a corrupt/oversize file must not trigger an
    // unbounded allocation.
    let mut data = Vec::new();
    if let Err(error) = file
        .take((super::MAX_PENDING_REVOCATIONS_FILE_BYTES + 1) as u64)
        .read_to_end(&mut data)
    {
        return RevocationLedgerLoad::Unavailable(format!("read {}: {error}", path.display()));
    }
    if data.len() > super::MAX_PENDING_REVOCATIONS_FILE_BYTES {
        quarantine_pending_ledger(path, &data);
        return RevocationLedgerLoad::Corrupt(format!(
            "pending Web revocation file is larger than {} KiB: {}",
            super::MAX_PENDING_REVOCATIONS_FILE_BYTES / 1024,
            path.display()
        ));
    }
    let ledger = match serde_json::from_slice::<PendingRevocationLedger>(&data) {
        Ok(ledger) => ledger,
        Err(error) => {
            // Entirely unparseable: nothing is salvageable, so quarantine the
            // original as evidence and treat it as corrupt (queue/acknowledge
            // continue from an empty ledger and rewrite it with valid
            // content).
            quarantine_pending_ledger(path, &data);
            return RevocationLedgerLoad::Corrupt(format!("parse {}: {error}", path.display()));
        }
    };
    if let Err(error) = validate_pending_revocation_ledger(&ledger) {
        // Parseable but failing whole-file validation (version drift/oversize/
        // corrupt entry/duplicate key): salvage the still-valid entries one by
        // one and quarantine the original instead of dropping everything —
        // discarded pending revocations would keep the old endpoint joinable
        // at the relay.
        let salvaged = salvage_pending_revocations(&ledger);
        eprintln!(
            "[web-access] pending-revocation ledger invalid ({error}); salvaged {} of {} entries",
            salvaged.entries.len(),
            ledger.entries.len()
        );
        let quarantined = quarantine_pending_ledger(path, &data);
        // 抢救出的条目必须立刻落盘:进程崩溃在 worker ack 之前会丢掉这批
        // 待吊销记录(隔离副本只留证据,不会被自动重放),旧 endpoint 将在
        // relay 侧保持可加入——正是本分支要避免的结局。空抢救集按空账本
        // 语义移除已隔离的路径,避免下一次 load 重复走隔离;但隔离彻底失败
        // 时原件是仅存的证据,不得按空账本语义删除(保留原件并大声记录,
        // 下一轮 load 重走隔离)。落盘失败则降级为内存态:本进程内由回放的
        // 权威账本(`pending_revocation_backlog`)接住,确认针对它整体落盘
        // 即可重建盘上账本;进程在此之前重启则这批条目已不可恢复(与崩溃
        // 窗口同级残余)。
        if salvaged.entries.is_empty() && !quarantined {
            eprintln!(
                "[web-access] quarantine of the invalid pending-revocation ledger {} failed and \
                 nothing was salvaged; the original file is kept in place as evidence",
                path.display()
            );
            return RevocationLedgerLoad::Ledger(salvaged);
        }
        if let Err(persist_error) = persist_pending_revocations_to(path, &salvaged) {
            eprintln!(
                "[web-access] failed to persist salvaged pending revocations: {persist_error}"
            );
        }
        return RevocationLedgerLoad::Ledger(salvaged);
    }
    RevocationLedgerLoad::Ledger(ledger)
}

pub(super) fn persist_pending_revocations_to(
    path: &Path,
    ledger: &PendingRevocationLedger,
) -> Result<(), String> {
    validate_pending_revocation_ledger(ledger)?;
    if ledger.entries.is_empty() {
        remove_private_file(path)
    } else {
        atomic_write_private_json(path, ledger, "pending Web revocations")
    }
}

pub(super) fn queue_pending_revocation_for_removal(config: &WebAccessConfig) -> Result<(), String> {
    queue_pending_revocation_for_removal_at(
        &pending_revocations_path(),
        PendingRevocation::from(config),
    )
}

/// Record the revocation intent for a config that is being removed right now
/// (stop/rotate). A full queue must never block that lifecycle — once 32
/// permanently unreachable relays accumulated, the user could otherwise never
/// stop or rotate Web access again. When the queue is full the oldest entry is
/// dropped for the newest revocation, and the drop is permanent: the ledger no
/// longer holds that intent, so it can never be replayed. The dropped relay
/// keeps serving its (no longer referenced) token until external expiry or a
/// manual cleanup, while the removal the user asked for always succeeds.
pub(super) fn queue_pending_revocation_for_removal_at(
    path: &Path,
    pending: PendingRevocation,
) -> Result<(), String> {
    validate_pending_revocation(&pending)?;
    let mut ledger = match load_pending_revocations_classified(path) {
        RevocationLedgerLoad::Ledger(ledger) => ledger,
        RevocationLedgerLoad::Corrupt(error) => {
            // 停用/轮换是恢复路径:Corrupt 只剩完全不可解析/超限两类(可解
            // 析但未过验证的账本已在读取时逐条抢救并隔离原件),记录后从空
            // 账本继续;随后的持久化会用合法内容重写,旧 endpoint 的吊销
            // 不会被永久卡死。
            eprintln!("[web-access] pending-revocation ledger is corrupt, starting fresh: {error}");
            PendingRevocationLedger::default()
        }
        RevocationLedgerLoad::Unavailable(error) => return Err(error),
    };
    let key = pending_revocation_key(&pending);
    if let Some(existing) = ledger
        .entries
        .iter()
        .find(|entry| pending_revocation_key(entry) == key)
    {
        return if existing == &pending {
            Ok(())
        } else {
            Err(format!(
                "conflicting pending Web revocation credentials for {}",
                pending.endpoint_id
            ))
        };
    }
    if ledger.entries.len() >= super::MAX_PENDING_REVOCATIONS {
        ledger.entries.pop_front();
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "pending_revocation_queue_full",
            "evicted_oldest",
        );
    }
    ledger.entries.push_back(pending);
    persist_pending_revocations_to(path, &ledger)
}

pub(super) fn queue_pending_revocation_at(
    path: &Path,
    pending: PendingRevocation,
) -> Result<(), String> {
    validate_pending_revocation(&pending)?;
    let mut ledger = match load_pending_revocations_classified(path) {
        RevocationLedgerLoad::Ledger(ledger) => ledger,
        RevocationLedgerLoad::Corrupt(error) => {
            // Disable/rotate are recovery paths: by this point Corrupt is only
            // the wholly unparseable/oversize classes (a parseable but invalid
            // ledger was already salvaged entry by entry at read time with the
            // original quarantined), so log it and continue from an empty
            // ledger; the subsequent persist rewrites it with valid content
            // and the old endpoint's revocation is not wedged forever.
            eprintln!("[web-access] pending-revocation ledger is corrupt, starting fresh: {error}");
            PendingRevocationLedger::default()
        }
        RevocationLedgerLoad::Unavailable(error) => return Err(error),
    };
    let key = pending_revocation_key(&pending);
    if let Some(existing) = ledger
        .entries
        .iter()
        .find(|entry| pending_revocation_key(entry) == key)
    {
        return if existing == &pending {
            Ok(())
        } else {
            Err(format!(
                "conflicting pending Web revocation credentials for {}",
                pending.endpoint_id
            ))
        };
    }
    if ledger.entries.len() >= super::MAX_PENDING_REVOCATIONS {
        return Err(format!(
            "{PENDING_REVOCATION_QUEUE_FULL_PREFIX} ({})",
            super::MAX_PENDING_REVOCATIONS
        ));
    }
    ledger.entries.push_back(pending);
    persist_pending_revocations_to(path, &ledger)
}

/// 回放确认的落盘原语——把调用方持有的权威账本(进程内 backlog)整体写盘,
/// 取代「重读磁盘→删一条→重写」;语义见 manager `finish_pending_revocation`。
/// 空集移除文件(与 `_to` 一致)。
pub(super) fn persist_pending_revocations(ledger: &PendingRevocationLedger) -> Result<(), String> {
    persist_pending_revocations_to(&pending_revocations_path(), ledger)
}

pub(super) fn eligible_pending_revocations(
    ledger: &PendingRevocationLedger,
    current: Option<&WebAccessConfig>,
) -> Vec<PendingRevocation> {
    ledger
        .entries
        .iter()
        .filter(|pending| {
            current.is_none_or(|config| {
                pending.relay_url != config.relay_url
                    || pending.endpoint_id != config.endpoint_id
                    || pending.desktop_secret != config.desktop_secret
            })
        })
        .cloned()
        .collect()
}

pub(super) fn load_or_initialize_rpc_ledger(endpoint_id: &str) -> Result<RpcLedger, String> {
    load_or_initialize_rpc_ledger_at(&rpc_ledger_path(), endpoint_id)
}

/// Path-injected variant for tests; the initialization branches still persist
/// through the global `persist_rpc_ledger`, so tests only exercise the
/// read/validation paths against injected paths.
pub(super) fn load_or_initialize_rpc_ledger_at(
    path: &Path,
    endpoint_id: &str,
) -> Result<RpcLedger, String> {
    // take()-bounded read: a ledger of 512 tombstones is far below 1 MiB, so
    // an oversize file is necessarily corrupt and must not be read into
    // memory unbounded before parsing.
    let file = match File::open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            let ledger = RpcLedger::for_endpoint(endpoint_id);
            persist_rpc_ledger(&ledger)?;
            return Ok(ledger);
        }
        Err(error) => return Err(format!("read {}: {error}", path.display())),
    };
    // 首次句柄上的 0600 修复,与 config / revocations 两个载入器同形——该
    // 文件按 0600 写入,备份恢复可能留下放宽权限的副本,读取即修复且先于
    // 任何内容读取。内容敏感度低于 desktop_secret(endpoint_id + 请求指纹),
    // 修复失败按读取错误 fail-closed 上抛(与本载入器其余 IO 错误一致);
    // Windows/fallback 平台为显式 no-op。
    platform::enforce_private_permissions(&file, path)
        .map_err(|error| format!("restrict {}: {error}", path.display()))?;
    let mut text = String::new();
    file.take((super::MAX_RPC_LEDGER_FILE_BYTES + 1) as u64)
        .read_to_string(&mut text)
        .map_err(|error| format!("read {}: {error}", path.display()))?;
    if text.len() > super::MAX_RPC_LEDGER_FILE_BYTES {
        return Err(format!(
            "Web RPC ledger is larger than {} KiB: {}",
            super::MAX_RPC_LEDGER_FILE_BYTES / 1024,
            path.display()
        ));
    }
    let mut ledger: RpcLedger = serde_json::from_str(&text)
        .map_err(|error| format!("parse {}: {error}", path.display()))?;
    if ledger.endpoint_id != endpoint_id {
        let ledger = RpcLedger::for_endpoint(endpoint_id);
        persist_rpc_ledger(&ledger)?;
        return Ok(ledger);
    }
    if ledger.version != super::RPC_LEDGER_VERSION {
        return Err(format!(
            "unsupported Web RPC ledger version {} in {}",
            ledger.version,
            path.display()
        ));
    }
    let mut request_ids = HashSet::new();
    for entry in &ledger.entries {
        if entry.request_id.is_empty()
            || entry.request_id.len() > 256
            || entry.fingerprint.len() != 64
            || !entry
                .fingerprint
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(format!(
                "invalid Web RPC ledger entry in {}",
                path.display()
            ));
        }
        if !request_ids.insert(entry.request_id.as_str()) {
            return Err(format!(
                "duplicate Web RPC request id in {}: {}",
                path.display(),
                entry.request_id
            ));
        }
    }
    if ledger.entries.len() > super::RPC_CACHE_CAPACITY {
        let overflow = ledger.entries.len() - super::RPC_CACHE_CAPACITY;
        ledger.entries.drain(..overflow);
        persist_rpc_ledger(&ledger)?;
    }
    Ok(ledger)
}

pub(super) fn persist_rpc_ledger(ledger: &RpcLedger) -> Result<(), String> {
    if ledger.version != super::RPC_LEDGER_VERSION || ledger.endpoint_id.is_empty() {
        return Err("refusing to persist an invalid Web RPC ledger".to_string());
    }
    atomic_write_private_json(&rpc_ledger_path(), ledger, "Web RPC ledger")
}

pub(super) fn remove_rpc_ledger() -> Result<(), String> {
    remove_private_file(&rpc_ledger_path())
}

pub(super) fn atomic_write_private_json<T: Serialize>(
    path: &Path,
    value: &T,
    label: &str,
) -> Result<(), String> {
    let parent = path
        .parent()
        .ok_or_else(|| format!("invalid {label} path: {}", path.display()))?;
    std::fs::create_dir_all(parent)
        .map_err(|error| format!("create {}: {error}", parent.display()))?;
    let data =
        serde_json::to_vec_pretty(value).map_err(|error| format!("serialize {label}: {error}"))?;
    // Private temp file + fsync + atomic replace + failure cleanup are provided
    // by the shared platform filesystem helper.
    crate::platform::filesystem::atomic_write_private(path, &data)
        .map_err(|error| format!("write {}: {error}", path.display()))
}

pub(super) fn remove_private_file(path: &Path) -> Result<(), String> {
    match std::fs::remove_file(path) {
        Ok(()) => {
            if let Some(parent) = path.parent() {
                // Best-effort durability for the directory-entry deletion; on
                // platforms where a directory cannot be opened as a file the
                // sync is skipped.
                if let Ok(dir) = std::fs::File::open(parent) {
                    dir.sync_all()
                        .map_err(|error| format!("sync {}: {error}", parent.display()))?;
                }
            }
            Ok(())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(format!("remove {}: {error}", path.display())),
    }
}

/// Single entry point for env-var overrides: empty after trim or failing
/// validation always returns None (falling back to the stored value/built-in
/// default) without erroring — env is only an override mechanism, and an
/// invalid override must degrade rather than wedge Web access. A pure
/// function, convenient for tests without env vars.
fn normalized_env_url(
    raw: Option<String>,
    env_name: &str,
    validate: impl Fn(&str) -> Result<(), String>,
) -> Option<String> {
    let value = raw?;
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Err(error) = validate(trimmed) {
        // An operator's explicit override being discarded must leave a trace:
        // a silent fallback would make the operator believe they are talking
        // to their own relay while actually working on the stored value /
        // built-in default.
        eprintln!("[web-access] ignoring invalid {env_name} override, falling back: {error}");
        return None;
    }
    Some(trimmed.to_string())
}

pub(super) fn remote_public_base_url() -> String {
    if let Some(url) = normalized_env_url(
        std::env::var("PINVOU_REMOTE_PUBLIC_URL").ok(),
        "PINVOU_REMOTE_PUBLIC_URL",
        |url| validate_http_base_url(url, "PINVOU_REMOTE_PUBLIC_URL is invalid"),
    ) {
        return url;
    }
    if let Some(settings) = load_relay_settings() {
        return settings.public_base_url;
    }
    super::active_endpoint_profile().public_base_url.to_string()
}

pub(super) fn remote_relay_ws_url() -> String {
    // Same rule as the other two env overrides (empty after trim/invalid
    // always falls back, no error): an invalid override degrades to the
    // stored value/built-in default instead of wedging enable inside
    // validate_config.
    if let Some(url) = normalized_env_url(
        std::env::var("PINVOU_REMOTE_RELAY_WS_URL").ok(),
        "PINVOU_REMOTE_RELAY_WS_URL",
        |url| validate_ws_url(url, "PINVOU_REMOTE_RELAY_WS_URL is invalid"),
    ) {
        return url;
    }
    if let Some(settings) = load_relay_settings() {
        return settings.relay_url;
    }
    super::active_endpoint_profile().relay_ws_url.to_string()
}

/// A 端控制通道的 Relay 地址（E2E-02）：与 WebUI v2 的 desktop endpoint 共用
/// 同一份配置，并允许开发期用 `PINVOU_REMOTE_RUNTIME_RELAY_WS_URL` 临时覆盖
/// 本条连接的地址（不写回持久配置，避免把普通链路指向测试 Relay）。
pub(super) fn controller_relay_ws_url() -> String {
    if let Ok(url) = std::env::var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL") {
        if !url.trim().is_empty() {
            return url;
        }
    }
    remote_relay_ws_url()
}

/// Development/manual-test override for the active connection only. Unlike
/// `PINVOU_REMOTE_RELAY_WS_URL`, this value is never written to the persistent
/// Web access config, so testing a local relay cannot strand the normal link.
/// An invalid override (whitespace, non-ws(s), over-long, empty after trim)
/// is ignored and falls back to the stored value.
pub(super) fn apply_runtime_relay_override(mut config: WebAccessConfig) -> WebAccessConfig {
    if let Some(relay_url) = normalized_env_url(
        std::env::var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL").ok(),
        "PINVOU_REMOTE_RUNTIME_RELAY_WS_URL",
        |url| validate_ws_url(url, "PINVOU_REMOTE_RUNTIME_RELAY_WS_URL is invalid"),
    ) {
        config.relay_url = relay_url;
    }
    config
}

/// A 端控制通道地址的来源标记，只用于有界排障日志。
///
/// 2026-09-20 实测：开发期遗留的用户级环境变量把 A 端指向测试 Relay，而 B 端连接码
/// 注册在生产 Relay，现象是「连接码无效或已过期」，但端上没有任何痕迹说明地址被
/// 覆盖。这里把来源显式记下来（`env` / `settings` / `profile`），不含任何凭据。
pub(super) fn controller_relay_url_source() -> &'static str {
    if std::env::var("PINVOU_REMOTE_RUNTIME_RELAY_WS_URL")
        .map(|url| !url.trim().is_empty())
        .unwrap_or(false)
    {
        return "runtime_env";
    }
    if std::env::var("PINVOU_REMOTE_RELAY_WS_URL")
        .map(|url| !url.trim().is_empty())
        .unwrap_or(false)
    {
        return "env";
    }
    if load_relay_settings().is_some() {
        return "settings";
    }
    "profile"
}

/// 只保留端点的路径部分：排障需要区分 `/pinvou3/remote/ws` 与
/// `/pinvou3/remote-test/ws`，但查询串里可能带凭据，绝不能落盘。
pub(super) fn relay_url_path(url: &str) -> String {
    let after_scheme = url.split_once("://").map(|(_, rest)| rest).unwrap_or("");
    let path = match after_scheme.find('/') {
        Some(index) => &after_scheme[index..],
        None => "",
    };
    path.split(['?', '#']).next().unwrap_or("").to_string()
}

/// A 端连接前写进排障日志的一行端点标记：内置 profile + 来源 + 路径。
pub(super) fn controller_endpoint_log_detail() -> String {
    format!(
        "profile:{}/source:{}/path:{}",
        super::active_endpoint_profile().id,
        controller_relay_url_source(),
        relay_url_path(&controller_relay_ws_url())
    )
}

pub(super) fn public_url(config: &WebAccessConfig) -> String {
    let mut url = format!(
        "{}/#endpoint={}&token={}",
        remote_public_base_url().trim_end_matches('/'),
        percent_encode(&config.endpoint_id),
        percent_encode(&config.access_token),
    );
    if config.relay_url != super::DEFAULT_RELAY_WS_URL {
        url.push_str("&relay=");
        url.push_str(&percent_encode(&config.relay_url));
    }
    url
}

pub(super) fn percent_encode(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            encoded.push(byte as char);
        } else {
            use std::fmt::Write as _;
            let _ = write!(encoded, "%{byte:02X}");
        }
    }
    encoded
}

/// Fresh opaque epoch token for the stream journal. Used by `StreamState` in the
/// parent module, so it stays `pub(super)`.
pub(super) fn new_stream_epoch() -> String {
    format!("epoch_{}", crate::features::remote_control::short_token(24))
}

#[cfg(test)]
mod tests {
    use super::relay_url_path;

    #[test]
    fn relay_url_path_keeps_only_the_path_and_drops_credentials() {
        assert_eq!(
            relay_url_path("wss://pinvou.com/pinvou3/remote/ws"),
            "/pinvou3/remote/ws"
        );
        assert_eq!(
            relay_url_path("wss://pinvou.com/pinvou3/remote-test/ws?token=hunter2"),
            "/pinvou3/remote-test/ws"
        );
        assert_eq!(
            relay_url_path("ws://127.0.0.1:8787/pinvou3/remote/ws"),
            "/pinvou3/remote/ws"
        );
        assert_eq!(relay_url_path("wss://pinvou.com"), "");
        assert_eq!(relay_url_path("not-a-url"), "");
    }
}

#[cfg(test)]
mod env_override_tests {
    use super::*;

    #[test]
    fn salvage_truncation_log_clips_dropped_endpoint_ids_to_the_validated_charset() {
        // Round-20 review (S10-2) source pin: the capacity truncation runs
        // before per-entry validation, so named entries' endpoint_id values
        // are unvalidated — naming them directly would carry planted control
        // characters/ANSI sequences into the desktop log (log forging). The
        // shared clip helper (round-29 review: extracted so the backlog cap
        // retire path names ids with the same strictness) must keep the
        // charset clipping and the `~corrupt` marking; reverting it to raw
        // naming turns red.
        let source = include_str!("persistence.rs");
        let helper_start = source
            .find("fn clip_pending_endpoint_id_for_log(")
            .expect("the shared log-clip helper must exist");
        let rest = &source[helper_start..];
        let terminator = rest
            .find("\npub(super) fn ")
            .into_iter()
            .chain(rest.find("\nfn ").into_iter())
            .min()
            .expect("the clip helper body must be terminated by the next fn");
        let helper_body = crate::platform::test_support::comment_filtered(&rest[..terminator]);
        assert!(
            helper_body.contains("filter(|character| {"),
            "dropped endpoint ids must be clipped to the validated charset before they are named in the log"
        );
        assert!(
            helper_body.contains("~corrupt"),
            "lossy-clipped ids must be marked corrupt instead of passing \
             through unvalidated"
        );
        // The truncation log line must name ids through the helper, not raw
        // clones: binding the salvage window to the helper call keeps "what
        // gets logged is the clipped output" true across the extraction
        // (the round-27 m2 concern, restated for the shared-helper shape).
        let start = source
            .find("salvage dropped {} pending revocations")
            .expect("the truncation log line must exist");
        // The window's lower bound must land on a char boundary (this file
        // carries multibyte CJK comment text; a bare minus 900 would slice
        // into a multibyte char — a slice panic rather than an assertion
        // failure, the round-17 lesson).
        let mut window_start = start.saturating_sub(900);
        while window_start > 0 && !source.is_char_boundary(window_start) {
            window_start -= 1;
        }
        let window = crate::platform::test_support::comment_filtered(&source[window_start..start]);
        assert!(
            window.contains("clip_pending_endpoint_id_for_log("),
            "the salvage truncation must name dropped ids through the shared \
             clip helper; naming raw clones revives the log-forging surface"
        );
        // Round-27 review m2: the window previously stopped at the format-
        // string needle — "the clipping code exists" was pinned, but "what
        // gets logged is exactly the clipped output" was not: a mutation
        // swapping the eprintln! argument from `dropped.join(", ")` back to
        // naming the unclipped `ledger.entries` compiles and stays green
        // (the log-forging surface revives). The window extends past the log
        // line, pinning that the argument is the clipped vector itself.
        let mut window_end = (start + 800).min(source.len());
        while window_end < source.len() && !source.is_char_boundary(window_end) {
            window_end -= 1;
        }
        let logged = crate::platform::test_support::comment_filtered(&source[start..window_end]);
        assert!(
            logged.contains("dropped.join(\", \")"),
            "the log must name the clipped dropped vector, not the raw \
             unvalidated ledger entries"
        );
    }

    #[test]
    fn env_url_overrides_are_trimmed_and_fail_safe() {
        // Overrides take effect only when they pass the same validation as
        // the settings file; invalid/empty values always fall back (None)
        // without propagating — env overrides degrade gracefully rather than
        // wedging Web access.
        let http = |url: &str| validate_http_base_url(url, "invalid");
        assert_eq!(
            normalized_env_url(
                Some("  https://relay.example.test/pinvou3/remote \n".into()),
                "PINVOU_REMOTE_TEST_URL",
                http
            ),
            Some("https://relay.example.test/pinvou3/remote".into())
        );
        assert_eq!(
            normalized_env_url(
                Some("ftp://not-http".into()),
                "PINVOU_REMOTE_TEST_URL",
                http
            ),
            None
        );
        assert_eq!(
            normalized_env_url(
                Some("https://has space".into()),
                "PINVOU_REMOTE_TEST_URL",
                http
            ),
            None
        );
        assert_eq!(
            normalized_env_url(
                Some(format!("https://{}", "a".repeat(2_049))),
                "PINVOU_REMOTE_TEST_URL",
                http
            ),
            None,
            "the length cap matches the relay settings validation"
        );
        assert_eq!(
            normalized_env_url(Some("   ".into()), "PINVOU_REMOTE_TEST_URL", http),
            None
        );
        assert_eq!(
            normalized_env_url(None, "PINVOU_REMOTE_TEST_URL", http),
            None
        );

        let ws = |url: &str| validate_ws_url(url, "invalid");
        assert_eq!(
            normalized_env_url(
                Some(" ws://127.0.0.1:8787/ws ".into()),
                "PINVOU_REMOTE_TEST_URL",
                ws
            ),
            Some("ws://127.0.0.1:8787/ws".into())
        );
        assert_eq!(
            normalized_env_url(Some("http://not-ws".into()), "PINVOU_REMOTE_TEST_URL", ws),
            None
        );
        assert_eq!(
            normalized_env_url(Some("".into()), "PINVOU_REMOTE_TEST_URL", ws),
            None
        );
    }

    /// Round-6 review: all three env-override read points must route through
    /// `normalized_env_url` (trim + validation + invalid fallback); falling
    /// back to a bare `std::env::var` would let invalid overrides take effect
    /// silently. Helper-level tests (above) cannot see the wiring, so per
    /// this module's convention the three call sites' bodies are pinned with
    /// source contracts.
    #[test]
    fn env_override_readers_route_through_the_normalized_helper() {
        let source = include_str!("persistence.rs");
        for (fn_name, env_name) in [
            (
                "fn remote_public_base_url() -> String",
                "PINVOU_REMOTE_PUBLIC_URL",
            ),
            (
                "fn remote_relay_ws_url() -> String",
                "PINVOU_REMOTE_RELAY_WS_URL",
            ),
            (
                "fn apply_runtime_relay_override(mut config: WebAccessConfig)",
                "PINVOU_REMOTE_RUNTIME_RELAY_WS_URL",
            ),
        ] {
            let start = source
                .find(fn_name)
                .unwrap_or_else(|| panic!("{fn_name} must exist"));
            let body = &source[start..];
            let body = &body[..body
                .find("\npub(super) fn ")
                .or_else(|| body.find("\nfn "))
                .unwrap_or_else(|| panic!("{fn_name} body must be terminated by the next fn"))];
            assert!(
                body.contains("normalized_env_url("),
                "{fn_name} must route its env override through normalized_env_url"
            );
            assert!(body.contains(env_name), "{fn_name} must read {env_name}");
        }
    }

    /// Round-9 review minor: both loaders' repair-failure legs must go
    /// through `quarantine_secret_file` (0600 copy + unlink of the original)
    /// rather than the plain `quarantine_corrupt_file` — rename preserves the
    /// relaxed mode bits, leaving a world-readable secret owned by someone
    /// else just as exposed under its quarantined name. Triggering this
    /// branch requires foreign ownership (CI cannot drive it; same class as
    /// round-8's review-pinned residual), so per this module's convention the
    /// two call sites are pinned with source contracts; the helper's own
    /// 0600/unlink semantics are covered by executing tests in the
    /// filesystem module.
    #[test]
    fn repair_failure_legs_quarantine_secret_files() {
        let source = include_str!("persistence.rs");
        for fn_name in [
            "pub(super) fn load_config_from(path: &Path) -> Result<Option<WebAccessConfig>, String> {",
            "fn load_pending_revocations_classified(path: &Path) -> RevocationLedgerLoad {",
        ] {
            let start = source
                .find(fn_name)
                .unwrap_or_else(|| panic!("{fn_name} must exist"));
            let body = &source[start..];
            let body = &body[..body
                .find("\npub(super) fn ")
                .or_else(|| body.find("\nfn "))
                .unwrap_or_else(|| panic!("{fn_name} body must be terminated by the next fn"))];
            assert!(
                body.contains("quarantine_secret_file(path)"),
                "{fn_name} must quarantine a failed-permission repair via quarantine_secret_file"
            );
            assert!(
                !body.contains("quarantine_corrupt_file(path)"),
                "{fn_name} must not keep the loose-mode rename on its repair-failure leg"
            );
        }
    }
}
