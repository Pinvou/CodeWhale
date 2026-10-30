//! Isolated native weight generations, verified preparation, explicit switching and cleanup.
use super::super::{
    card_update,
    version_store::{self, Version},
    weight_identity,
};
use super::*;
use sha2::{Digest, Sha256};
use std::fs;
use std::io::Read;
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

fn now() -> String {
    chrono::Utc::now().to_rfc3339()
}
fn fail(message: impl Into<String>) -> ModelManagerError {
    ModelManagerError::new(message)
}

fn runtime_path(rec: &db::StoreModelRecord) -> Result<PathBuf, ModelManagerError> {
    let id = rec.runtime_card_id.as_deref().unwrap_or(&rec.card_id);
    Ok(config::data_dir()
        .join("cards")
        .join(config::safe_path_component(id).map_err(fail)?))
}

fn owned_directory(root: &Path, parts: &[&str]) -> Result<PathBuf, ModelManagerError> {
    let mut path = root.canonicalize().map_err(|e| fail(e.to_string()))?;
    let uid = fs::metadata("/proc/self")
        .map_err(|e| fail(e.to_string()))?
        .uid();
    for part in parts {
        path.push(config::safe_path_component(part).map_err(fail)?);
        match fs::DirBuilder::new().mode(0o700).create(&path) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(e) => return Err(fail(e.to_string())),
        }
        let meta = path.symlink_metadata().map_err(|e| fail(e.to_string()))?;
        if !meta.is_dir() || meta.uid() != uid || meta.mode() & 0o022 != 0 {
            return Err(fail("版本目录归属或权限不安全，已中止。"));
        }
    }
    Ok(path)
}

/// Existing retry locations are immutable: do not follow a replaced ancestor,
/// create another default tree, or accept a missing/mismatched ownership marker.
fn existing_owned_directory(path: &Path) -> Result<bool, ModelManagerError> {
    let uid = fs::metadata("/proc/self")
        .map_err(|e| fail(e.to_string()))?
        .uid();
    // Creation checks .versions, the public card and the revision directory.
    // Retrying must recheck all three: a private leaf cannot protect its name
    // when another user can replace it through a writable parent.
    for directory in path.ancestors().take(3) {
        let meta = directory
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?;
        if !meta.is_dir() || meta.uid() != uid || meta.mode() & 0o022 != 0 {
            return Ok(false);
        }
    }
    Ok(path.canonicalize().map_err(|e| fail(e.to_string()))? == path)
}

/// Resolve existing prefixes without creating missing directories. Resolve each
/// prefix before `..`, so a symlink followed by `..` keeps filesystem semantics.
/// Only a positively missing component may remain lexical; unreadable paths,
/// dangling links and non-directories cannot prove that two resources are apart.
fn planned_directory(path: &Path) -> Result<PathBuf, ModelManagerError> {
    let absolute = if path.is_absolute() {
        path.to_path_buf()
    } else {
        std::env::current_dir()
            .map_err(|e| fail(e.to_string()))?
            .join(path)
    };
    let mut resolved = PathBuf::new();
    for component in absolute.components() {
        match component {
            std::path::Component::CurDir => {}
            std::path::Component::ParentDir => {
                resolved.pop();
            }
            _ => {
                resolved.push(component.as_os_str());
                match resolved.canonicalize() {
                    Ok(canonical) => {
                        if !fs::metadata(&canonical)
                            .map_err(|e| fail(e.to_string()))?
                            .is_dir()
                        {
                            return Err(fail("权重资源路径不是目录，已保留现有记录和文件。"));
                        }
                        resolved = canonical;
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                        match resolved.symlink_metadata() {
                            Err(missing) if missing.kind() == std::io::ErrorKind::NotFound => {}
                            _ => return Err(fail(error.to_string())),
                        }
                    }
                    Err(error) => return Err(fail(error.to_string())),
                }
            }
        }
    }
    Ok(resolved)
}

/// Path::exists treats permission errors and dangling links as absence. A
/// record-only cleanup needs a positive NotFound result without linked parents.
fn resource_presence(path: &Path) -> Result<bool, ModelManagerError> {
    let mut missing = false;
    for ancestor in path.ancestors() {
        match ancestor.symlink_metadata() {
            Ok(meta) if meta.file_type().is_symlink() => {
                return Err(fail("拒绝清理链接资源路径。"));
            }
            Ok(_) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => missing = true,
            Err(e) => return Err(fail(e.to_string())),
        }
    }
    Ok(!missing)
}

fn open_digest_file(path: &Path, expected: &fs::Metadata) -> Result<fs::File, ModelManagerError> {
    let file = fs::OpenOptions::new()
        .read(true)
        // A regular file found during traversal can be replaced before open.
        // Do not follow a substituted link or wait for a FIFO writer.
        .custom_flags(nix::libc::O_NOFOLLOW | nix::libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| fail(e.to_string()))?;
    let actual = file.metadata().map_err(|e| fail(e.to_string()))?;
    if !actual.is_file() || (actual.dev(), actual.ino()) != (expected.dev(), expected.ino()) {
        return Err(fail("校验权重不是原普通文件，请检查路径后重试。"));
    }
    Ok(file)
}

fn file_digest(
    path: &Path,
    control: &script_runner::RunControl,
    on_progress: &mut ProgressFn<'_>,
) -> Result<String, ModelManagerError> {
    if control.cancelled() {
        return Err(fail("已取消"));
    }
    let expected = path.symlink_metadata().map_err(|e| fail(e.to_string()))?;
    if !expected.is_file() {
        return Err(fail("校验权重不是普通文件，请检查路径后重试。"));
    }
    let mut file = open_digest_file(path, &expected)?;
    let before = file.metadata().map_err(|e| fail(e.to_string()))?;
    let mut hash = Sha256::new();
    let mut read_bytes = 0u64;
    let mut last_progress = Instant::now();
    let report = |bytes: u64, progress: &mut ProgressFn<'_>| {
        progress(&format!(
            "校验权重 {}：{} / {} 字节（{:.1}%）",
            path.display(),
            bytes,
            before.len(),
            if before.len() == 0 {
                100.0
            } else {
                bytes as f64 * 100.0 / before.len() as f64
            },
        ));
        // 任务卡进度条协议行（与脚本下载进度同约定）：前端 reduceLogChunk
        // 解析它驱动进度/速度，并把该行从可见日志中过滤——没有它，digest
        // 阶段进度条只能停留在不确定态，字节进度要展开日志才看得到。
        progress(&format!("LMM_PROGRESS_BYTES={}/{}", bytes, before.len()));
    };
    report(0, on_progress);
    let mut buffer = vec![0; 1024 * 1024];
    loop {
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        let n = file.read(&mut buffer).map_err(|e| fail(e.to_string()))?;
        if n == 0 {
            break;
        }
        hash.update(&buffer[..n]);
        read_bytes = read_bytes.saturating_add(n as u64);
        if last_progress.elapsed() >= Duration::from_secs(1) {
            report(read_bytes, on_progress);
            last_progress = Instant::now();
        }
    }
    let after = file.metadata().map_err(|e| fail(e.to_string()))?;
    if (before.len(), before.mtime(), before.mtime_nsec())
        != (after.len(), after.mtime(), after.mtime_nsec())
    {
        return Err(fail("校验期间权重发生变化，请重试。"));
    }
    let named = path.symlink_metadata().map_err(|e| fail(e.to_string()))?;
    if !named.is_file() || (named.dev(), named.ino()) != (before.dev(), before.ino()) {
        return Err(fail("校验期间权重路径发生变化，请重试。"));
    }
    report(read_bytes, on_progress);
    Ok(crate::platform::encoding::hex_lower(&hash.finalize()))
}

fn single_weight(rec: &db::StoreModelRecord) -> Result<(String, String), ModelManagerError> {
    let blob = rec
        .assets_content
        .get(weight_identity::MANIFEST)
        .ok_or_else(|| fail("此卡片缺少独立的权重清单，暂不支持版本化升级。"))?;
    let files = weight_identity::verified(blob, &blob.sha256).map_err(fail)?;
    if files.len() != 1 {
        return Err(fail("此版本暂不支持多文件权重的隔离升级；当前部署已保留。"));
    }
    files
        .into_iter()
        .next()
        .ok_or_else(|| fail("此卡片缺少可确认的单文件权重。"))
}

fn locate_weight(root: &Path, name: &str) -> Result<Option<PathBuf>, ModelManagerError> {
    let canonical = root.canonicalize().map_err(|e| fail(e.to_string()))?;
    let mut stack = vec![(root.to_path_buf(), 0)];
    let mut found = Vec::new();
    let mut visited = 0;
    let mut unresolved_link = false;
    while let Some((dir, depth)) = stack.pop() {
        if depth > 5 || visited > 10000 {
            return Err(fail("权重目录过于复杂，无法可靠确认文件归属。"));
        }
        for entry in fs::read_dir(dir).map_err(|e| fail(e.to_string()))? {
            let entry = entry.map_err(|e| fail(e.to_string()))?;
            visited += 1;
            let kind = entry.file_type().map_err(|e| fail(e.to_string()))?;
            if kind.is_dir() {
                stack.push((entry.path(), depth + 1));
            } else if kind.is_symlink() {
                if entry.path().ends_with(name) {
                    return Err(fail("拒绝使用链接权重文件。"));
                }
                unresolved_link = true;
            } else if kind.is_file() && entry.path().ends_with(name) {
                let path = entry
                    .path()
                    .canonicalize()
                    .map_err(|e| fail(e.to_string()))?;
                if path.starts_with(&canonical) {
                    found.push(path);
                }
            }
        }
    }
    if found.len() > 1 {
        return Err(fail("无法唯一确认隔离目录中的权重文件，未切换部署。"));
    }
    if found.is_empty() && unresolved_link {
        return Err(fail("权重目录包含未解析的链接，无法确认文件已不存在。"));
    }
    Ok(found.pop())
}

fn find_weight(root: &Path, name: &str) -> Result<PathBuf, ModelManagerError> {
    locate_weight(root, name)?.ok_or_else(|| fail("无法唯一确认隔离目录中的权重文件，未切换部署。"))
}

fn declared_resource_dir(rec: &db::StoreModelRecord) -> Result<Option<PathBuf>, ModelManagerError> {
    let path = runtime_path(rec)?.join(".deploy-env");
    match fs::read_to_string(path) {
        Ok(env) => Ok(env.lines().find_map(|line| {
            line.strip_prefix("MODEL_DIR=\"")
                .and_then(|value| value.strip_suffix('"'))
                .map(PathBuf::from)
        })),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(fail(error.to_string())),
    }
}

pub(super) fn legacy_resource_dir(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Option<PathBuf> {
    let allowed = rec
        .models_root
        .as_deref()
        .map(Path::new)
        .unwrap_or(configured_root)
        .canonicalize()
        .ok()?;
    if let Some(value) = declared_resource_dir(rec).ok().flatten() {
        if let Ok(path) = value.canonicalize() {
            if path != allowed && path.starts_with(&allowed) {
                return Some(path);
            }
        }
    }
    rec.runtime_card_id
        .as_ref()
        .and_then(|_| rec.models_root.as_ref())
        .and_then(|root| PathBuf::from(root).canonicalize().ok())
}

pub(super) fn managed_storage(rec: &db::StoreModelRecord) -> Result<bool, ModelManagerError> {
    Ok(
        version_store::get(&rec.card_id, &version_store::active_id(rec))
            .map_err(fail)?
            .is_some_and(|v| v.managed),
    )
}

pub(super) fn ensure_not_deleting(rec: &db::StoreModelRecord) -> Result<(), ModelManagerError> {
    if version_store::get(&rec.card_id, &version_store::active_id(rec))
        .map_err(fail)?
        .is_some_and(|v| v.state == "deleting")
    {
        return Err(fail("当前版本的彻底删除尚未完成，请重试删除模型。"));
    }
    Ok(())
}

fn valid_resource_marker(
    root: &Path,
    expected: &str,
    deleting: bool,
) -> Result<bool, ModelManagerError> {
    let marker = root.join(".pinvou-version.json");
    Ok(match marker.symlink_metadata() {
        Ok(meta) => {
            meta.is_file()
                && meta.nlink() == 1
                && fs::read_to_string(marker).ok().as_deref() == Some(expected)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound && deleting => fs::read_dir(root)
            .map_err(|e| fail(e.to_string()))?
            .next()
            .is_none(),
        _ => false,
    })
}

/// A snapshot's saved path must still name the private tree that staging
/// registered. Check it before canonicalization can hide a replaced ancestor.
fn managed_version_root(
    version: &Version,
    card_id: &str,
    version_id: &str,
) -> Result<PathBuf, ModelManagerError> {
    let root = PathBuf::from(
        version
            .models_root
            .as_deref()
            .ok_or_else(|| fail("版本缺少存储目录记录"))?,
    );
    let expected = serde_json::json!({"card_id":card_id,"version_id":version_id}).to_string();
    if version.resource_dir != version.models_root
        || version.record.as_ref().is_none_or(|r| {
            r.models_root != version.models_root
                || r.runtime_card_id.as_deref() != Some(version.runtime_card_id.as_str())
        })
        || !existing_owned_directory(&root)?
        || !valid_resource_marker(&root, &expected, false)?
    {
        return Err(fail("版本目录归属或权限不符，未切换部署。"));
    }
    Ok(root)
}

/// Downloaded legacy cards have no deploy-env yet. Their missing runtime path
/// does not prove that an otherwise matching shared weight is unreferenced.
fn unresolved_weight_reference(other: &db::StoreModelRecord, owner: &db::StoreModelRecord) -> bool {
    if owner
        .source_repo_id
        .as_deref()
        .is_some_and(|repo| !repo.is_empty())
        && other.source_repo_id == owner.source_repo_id
        && (other.source_filename == owner.source_filename
            || other.source_filename.as_deref().is_none_or(str::is_empty)
            || owner.source_filename.as_deref().is_none_or(str::is_empty))
    {
        return true;
    }
    let inventory = |rec: &db::StoreModelRecord| {
        let blob = rec.assets_content.get(weight_identity::MANIFEST)?;
        weight_identity::verified(blob, &blob.sha256).ok()
    };
    match (inventory(other), inventory(owner)) {
        (Some(other), Some(owner)) => owner
            .iter()
            .any(|(name, hash)| other.get(name) == Some(hash)),
        _ => false,
    }
}

/// Check references before scripts can remove deployment state, then repeat
/// after the service stops before deleting any host-managed files.
pub(super) fn current_purge_preflight(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Result<Option<(Version, PathBuf, PathBuf)>, ModelManagerError> {
    let id = version_store::active_id(rec);
    let version = version_store::get(&rec.card_id, &id).map_err(fail)?;
    let managed = version.as_ref().is_some_and(|v| v.managed);
    let root = if managed {
        Some(PathBuf::from(
            rec.models_root
                .as_ref()
                .ok_or_else(|| fail("缺少版本存储目录"))?,
        ))
    } else {
        legacy_resource_dir(rec, configured_root)
    };
    let runtime = runtime_path(rec)?;
    let possible_legacy_reference = |other: &db::StoreModelRecord| {
        rec.source_repo_id
            .as_deref()
            .is_some_and(|repo| !repo.is_empty())
            && rec.source_repo_id == other.source_repo_id
            || unresolved_weight_reference(rec, other)
    };
    let overlaps = |other: Option<&Path>, other_record: Option<&db::StoreModelRecord>| match (
        root.as_deref(),
        other,
    ) {
        (Some(root), Some(other)) => root.starts_with(other) || other.starts_with(root),
        _ => !managed && other_record.is_some_and(possible_legacy_reference),
    };
    let other_versions = version_store::list(None).map_err(fail)?;
    if other_versions.iter().any(|v| {
        !(v.card_id == rec.card_id && v.version_id == id)
            && (config::data_dir().join("cards").join(&v.runtime_card_id) == runtime
                || overlaps(v.resource_dir.as_deref().map(Path::new), v.record.as_ref()))
    }) {
        return Err(fail("此版本的存储仍被引用，已保留资源和记录。"));
    }
    for other in db::list_store_models()
        .map_err(|e| fail(e.to_string()))?
        .iter()
        .filter(|r| r.card_id != rec.card_id)
    {
        if runtime_path(other).ok().as_ref() == Some(&runtime)
            || overlaps(
                legacy_resource_dir(other, configured_root).as_deref(),
                Some(other),
            )
        {
            return Err(fail("此版本的存储仍被其他模型引用，已保留资源和记录。"));
        }
    }
    if !managed {
        return Ok(None);
    }
    let version = version.ok_or_else(|| fail("当前托管版本缺少归属记录，已保留资源。"))?;
    let root = root.ok_or_else(|| fail("无法确认当前托管版本的权重目录，已保留资源。"))?;
    if resource_presence(&runtime)?
        && !runtime
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?
            .is_dir()
    {
        return Err(fail("运行目录归属异常，已保留资源和记录。"));
    }
    if resource_presence(&root)? {
        let expected = serde_json::json!({"card_id":rec.card_id,"version_id":id}).to_string();
        if !root
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?
            .is_dir()
            || !valid_resource_marker(&root, &expected, version.state == "deleting")?
        {
            return Err(fail("版本目录归属不符，已保留资源和记录。"));
        }
    }
    Ok(Some((version, root, runtime)))
}

/// Called after the card has removed its service with weight deletion disabled.
pub(super) fn purge_current_storage(
    rec: &db::StoreModelRecord,
    configured_root: &Path,
) -> Result<(), ModelManagerError> {
    let Some((mut version, root, runtime)) = current_purge_preflight(rec, configured_root)? else {
        return Ok(());
    };
    ensure_unused(&runtime, Some(&root))?;
    version.state = "deleting".into();
    version_store::put(&version).map_err(fail)?;
    if resource_presence(&root)? {
        remove_managed_resource(&root)?;
    }
    if resource_presence(&runtime)? {
        fs::remove_dir_all(runtime).map_err(|e| fail(e.to_string()))?;
    }
    Ok(())
}

/// Lexically resolves `.` and `..` components. Inputs here are absolute
/// (procfs cwd results and configured roots), so the result stays absolute.
fn normalize_absolute(path: &Path) -> PathBuf {
    let mut parts: Vec<&std::ffi::OsStr> = Vec::new();
    for component in path.components() {
        match component {
            std::path::Component::ParentDir => {
                parts.pop();
            }
            std::path::Component::CurDir => {}
            std::path::Component::Normal(part) => parts.push(part),
            _ => {}
        }
    }
    let mut normalized = PathBuf::from("/");
    normalized.extend(parts);
    normalized
}

/// Resolved spelling of a configured root: procfs reports fully resolved cwds
/// and argv paths may arrive through symlinked parents, so both spellings are
/// compared; when the root is gone the lexical form still matches plain text.
fn canonical_or_lexical(path: &Path) -> PathBuf {
    fs::canonicalize(path).unwrap_or_else(|_| normalize_absolute(path))
}

/// Extracts the filesystem path carried by one argv element, if any. Covers
/// the forms real launchers produce: the path itself, `--flag=path`, a
/// single-letter short option with an attached value, and `KEY=value`
/// assignments passed through argv (e.g. `env LD_PRELOAD=…`). Plain flags and
/// unrelated text yield None.
fn argv_path_candidate(arg: &str) -> Option<&str> {
    let candidate = if let Some(rest) = arg.strip_prefix("--") {
        rest.split_once('=').map(|(_, value)| value)?
    } else if let Some(rest) = arg.strip_prefix('-') {
        let mut chars = rest.chars();
        let flag = chars.next()?;
        if !flag.is_ascii_alphabetic() {
            return None;
        }
        let value = chars.as_str();
        value.strip_prefix('=').unwrap_or(value)
    } else if let Some((key, value)) = arg.split_once('=') {
        let env_like = !key.is_empty()
            && key
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_');
        if env_like { value } else { arg }
    } else {
        arg
    };
    // Only path-like spellings can reference the resource: bare words
    // (`--type=zygote`, `--log=auto`, rewritten process titles) are
    // configuration values, and resolving them as cwd-relative paths would
    // let any same-uid process block cleanup.
    (!candidate.is_empty() && candidate.contains('/')).then_some(candidate)
}

/// Passwd home of `uid`, used to resolve engine-side `~` expansion. Engines
/// reach the resource through their own user's home, which is the shared uid
/// here; an unknown home keeps tilde candidates conservative.
fn passwd_home(uid: u32) -> Option<PathBuf> {
    // SAFETY: libc::passwd is a plain C record of integers and raw pointers;
    // zeroed gives null pointers, the expected input state for getpwuid_r.
    let mut passwd: libc::passwd = unsafe { std::mem::zeroed() };
    let mut buffer = vec![0u8; 4096];
    let mut result: *mut libc::passwd = std::ptr::null_mut();
    // SAFETY: getpwuid_r writes only into the supplied record, buffer and
    // pointer slots, all of which outlive the call.
    let status = unsafe {
        libc::getpwuid_r(
            uid,
            &mut passwd,
            buffer.as_mut_ptr().cast(),
            buffer.len(),
            &mut result,
        )
    };
    if status != 0 || result.is_null() {
        return None;
    }
    // SAFETY: on success glibc points `result` at `passwd`, whose `pw_dir` is
    // NUL-terminated within `buffer`.
    let home = unsafe { std::ffi::CStr::from_ptr(passwd.pw_dir) }
        .to_str()
        .ok()?;
    (!home.is_empty()).then(|| PathBuf::from(home))
}

/// Whether one argv path candidate may reach `resource`. Candidates that
/// cannot be ruled out count as references: a blocked cleanup only keeps
/// resources and records, while a missed reference deletes weights a process
/// still opens.
fn argv_candidate_may_reference(
    candidate: &str,
    cwd: Option<&Path>,
    proc_entry: &Path,
    resource: &Path,
    home: Option<&Path>,
) -> bool {
    let path = if let Some(rest) = candidate.strip_prefix('~') {
        // Engines expand `~` and `~/…` against their user's home; a `~user/…`
        // form or an unknown home cannot be confirmed and stays conservative.
        match (home, rest.strip_prefix('/')) {
            (Some(home), Some(rest)) => Some(home.join(rest)),
            (Some(home), None) if rest.is_empty() => Some(home.to_path_buf()),
            _ => None,
        }
    } else if candidate.starts_with('/') {
        Some(PathBuf::from(candidate))
    } else {
        cwd.map(|cwd| cwd.join(candidate))
    };
    let Some(path) = path else {
        return true;
    };
    if normalize_absolute(&path).starts_with(resource) {
        return true;
    }
    // Resolve symlinks (and, for relative candidates, the kernel-resolved
    // procfs cwd base) so they compare equal to the canonical resource root.
    let resolved = if candidate.starts_with('/') || candidate.starts_with('~') {
        fs::canonicalize(&path)
    } else {
        fs::canonicalize(proc_entry.join("cwd").join(candidate))
    };
    resolved
        .map(|resolved| resolved.starts_with(resource))
        .unwrap_or(false)
}

const PROCESS_CMDLINE_LIMIT: usize = 8 * 1024 * 1024;

#[derive(Debug, PartialEq, Eq)]
enum CmdlineReadError {
    Io,
    TooLarge,
    TimedOut,
}

/// Read complete arguments before treating a process as unrelated. One extra
/// byte distinguishes an exactly-at-limit command line from a truncated one;
/// it never grows the retained buffer past the cap. The caller's budget spans
/// every process, including this read and the subsequent argument parsing.
fn read_process_cmdline(
    mut reader: impl Read,
    mut budget_exhausted: impl FnMut() -> bool,
) -> Result<Vec<u8>, CmdlineReadError> {
    let mut bytes = Vec::new();
    let mut buffer = [0; 16 * 1024];
    loop {
        if budget_exhausted() {
            return Err(CmdlineReadError::TimedOut);
        }
        let remaining = PROCESS_CMDLINE_LIMIT - bytes.len();
        let requested = remaining.clamp(1, buffer.len());
        let result = reader.read(&mut buffer[..requested]);
        if budget_exhausted() {
            return Err(CmdlineReadError::TimedOut);
        }
        let read = match result {
            Ok(read) => read,
            Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => return Err(CmdlineReadError::Io),
        };
        if read == 0 {
            return Ok(bytes);
        }
        if remaining == 0 {
            return Err(CmdlineReadError::TooLarge);
        }
        bytes.extend_from_slice(&buffer[..read]);
    }
}

fn ensure_unused(runtime: &Path, resource: Option<&Path>) -> Result<(), ModelManagerError> {
    // Read only; neither PID files nor discovered processes authorize signals.
    let uid = fs::metadata("/proc/self")
        .map_err(|e| fail(e.to_string()))?
        .uid();
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    let home = passwd_home(uid);
    let runtime = canonical_or_lexical(runtime);
    let resource = resource.map(canonical_or_lexical);
    for entry in fs::read_dir("/proc").map_err(|e| fail(e.to_string()))? {
        if std::time::Instant::now() > deadline {
            return Err(fail("进程检查超时，未删除或切换任何资源。"));
        }
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        if entry.file_name().to_string_lossy().parse::<u32>().is_err() {
            continue;
        }
        if entry.metadata().map(|m| m.uid() != uid).unwrap_or(true) {
            continue;
        }
        let cwd = fs::read_link(entry.path().join("cwd")).ok();
        if let Some(resolved_cwd) = cwd.as_deref().map(normalize_absolute) {
            if resolved_cwd.starts_with(&runtime)
                || resource
                    .as_ref()
                    .is_some_and(|r| resolved_cwd.starts_with(r))
            {
                return Err(fail("此版本仍有运行进程，请先停用。"));
            }
        }
        let Some(resource) = resource.as_deref() else {
            continue;
        };
        let Ok(file) = fs::File::open(entry.path().join("cmdline")) else {
            continue;
        };
        let bytes = match read_process_cmdline(file, || std::time::Instant::now() > deadline) {
            Ok(bytes) => bytes,
            // The process may exit between discovery, opening and reading.
            Err(CmdlineReadError::Io) => continue,
            Err(CmdlineReadError::TimedOut) => {
                return Err(fail("进程检查超时，未删除或切换任何资源。"));
            }
            Err(CmdlineReadError::TooLarge) => {
                return Err(fail(
                    "进程命令行超过 8MiB 检查上限，无法排除资源引用，未删除或切换任何资源。",
                ));
            }
        };
        for arg in bytes.split(|b| *b == 0).skip(1) {
            if std::time::Instant::now() > deadline {
                return Err(fail("进程检查超时，未删除或切换任何资源。"));
            }
            // Lossy decoding keeps the resource-root prefix comparable even
            // when a trailing filename carries non-UTF-8 bytes.
            let text = String::from_utf8_lossy(arg);
            let Some(candidate) = argv_path_candidate(&text) else {
                continue;
            };
            if argv_candidate_may_reference(
                candidate,
                cwd.as_deref(),
                &entry.path(),
                resource,
                home.as_deref(),
            ) {
                return Err(fail("此版本的权重仍被运行进程使用，未删除或切换资源。"));
            }
        }
    }
    if std::time::Instant::now() > deadline {
        return Err(fail("进程检查超时，未删除或切换任何资源。"));
    }
    Ok(())
}

impl ModelManager {
    pub(super) fn remote_weight_evidence(
        &self,
        rec: &db::StoreModelRecord,
        card: &StoreCardDetail,
        control: Option<&script_runner::RunControl>,
    ) -> Result<Option<db::ScriptBlob>, ModelManagerError> {
        let Some(url) = card.assets.get(weight_identity::MANIFEST) else {
            return Ok(None);
        };
        let hash = card
            .asset_sha256
            .as_ref()
            .and_then(|h| h.get(weight_identity::MANIFEST))
            .ok_or_else(|| fail("卡片权重清单缺少摘要，已保留原版本。"))?;
        if let Some(blob) = rec.assets_content.get(weight_identity::MANIFEST) {
            if weight_identity::verified(blob, hash).is_ok() {
                return Ok(Some(blob.clone()));
            }
        }
        if control.is_some_and(|c| c.cancelled()) {
            return Err(fail("已取消"));
        }
        let content = script_runner::fetch_script_content(
            url,
            Some(hash),
            script_runner::FetchPolicy {
                may_run_as_root: true,
                allow_local_path: false,
            },
        )?;
        Ok(Some(db::ScriptBlob {
            sha256: script_runner::sha256_hex(&content),
            content,
        }))
    }

    pub fn versions(&self, card_id: &str) -> Result<serde_json::Value, ModelManagerError> {
        let current = self.get_record(card_id)?;
        version_store::recover_interrupted(card_id).map_err(fail)?;
        let mut versions = version_store::list(Some(card_id)).map_err(fail)?;
        let id = version_store::active_id(&current);
        let deleting = versions
            .iter()
            .any(|v| v.version_id == id && v.state == "deleting");
        versions.retain(|v| v.version_id != id);
        versions.insert(
            0,
            Version {
                card_id: card_id.into(),
                version_id: id.clone(),
                state: if deleting { "deleting" } else { "current" }.into(),
                models_root: current.models_root.clone(),
                runtime_card_id: current
                    .runtime_card_id
                    .clone()
                    .unwrap_or_else(|| card_id.into()),
                resource_dir: legacy_resource_dir(&current, &self.settings.models_path())
                    .map(|p| p.to_string_lossy().into_owned()),
                managed: managed_storage(&current)?,
                created_at: current.downloaded_at.clone(),
                record: Some(current),
                error: None,
                planned_bytes: 0,
            },
        );
        Ok(serde_json::json!(versions.iter().map(|v|serde_json::json!({
            "version_id":v.version_id,"state":v.state,"created_at":v.created_at,"error":v.error,
            "engine_version":v.record.as_ref().and_then(|r|r.engine_version.clone()),
            "filename":v.record.as_ref().and_then(|r|r.source_filename.clone()),
            "size_bytes":v.models_root.as_ref().filter(|_|v.managed).or(v.resource_dir.as_ref()).map(|p|super::super::disk::dir_size_bytes(Path::new(p))).unwrap_or_else(||v.record.as_ref().map(|r|r.size_bytes.max(0) as u64).unwrap_or(0)),
            "can_switch":(["ready","history"].contains(&v.state.as_str())),
            "can_delete":v.version_id != id && v.state!="current" && v.state!="downloading",
            "can_retry":v.state=="failed",
        })).collect::<Vec<_>>()))
    }

    /// The service retains the public card's operation lock for this entire run.
    pub fn stage_version(
        &self,
        card_id: &str,
        revision: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::wait_resource_operation_lock(
            false,
            || control.cancelled(),
            || on_progress("等待其他模型资源操作完成，可取消…"),
        )
        .map_err(fail)?;
        let current = self.get_record(card_id)?;
        ensure_not_deleting(&current)?;
        let card = self.store.get_card(card_id)?;
        card_update::validate_inventory(
            &card.scripts,
            card.script_sha256.as_ref().unwrap_or(&BTreeMap::new()),
            &card.assets,
            card.asset_sha256.as_ref().unwrap_or(&BTreeMap::new()),
        )
        .map_err(fail)?;
        if card_update::revision(&card_update::remote_manifest(&card)) != revision {
            return Err(fail("线上卡片已再次更新，请重新检查。"));
        }
        let evidence = self.remote_weight_evidence(&current, &card, Some(&control))?;
        let assessment = card_update::compatibility(&current, &card, evidence.as_ref());
        if !assessment.can_stage {
            return Err(fail("此卡片不支持当前版本的隔离升级，现有部署已保留。"));
        }
        if legacy_resource_dir(&current, Path::new(&self.settings.models_dir)).is_none() {
            return Err(fail(
                "无法确认旧版部署的权重目录，请先修复旧版部署，确保可回退后再升级。",
            ));
        }
        let pkg = self.fetch_and_store_scripts(&card, Some(&control))?;
        self.stage_verified(&current, &card, revision, pkg, on_progress, control)
    }

    /// Called under the quota admission lock, with the production caller also
    /// retaining its shared resource lock. Only this exact retry owns its row;
    /// the same card's active and other generations remain protected references.
    fn ensure_stage_target_disjoint(
        &self,
        target: &Path,
        card_id: &str,
        revision: &str,
    ) -> Result<(), ModelManagerError> {
        // Settings::models_path creates its directory even when only reading
        // the path. Admission must not use it before disjointness is proven.
        let configured_root = Path::new(&self.settings.models_dir);
        let target = planned_directory(target)?;
        let check = |resource: &Path| -> Result<(), ModelManagerError> {
            // Deletion also protects the exact registered spelling. A linked
            // resource may resolve elsewhere yet still make this candidate
            // uncleanable under that unchanged conservative protection.
            if target.starts_with(resource) || resource.starts_with(&target) {
                return Err(fail(
                    "新版下载目录与已登记权重目录重叠，已保留现有记录和文件。新建版本请改用独立目录。",
                ));
            }
            let resource = planned_directory(resource)?;
            if target.starts_with(&resource) || resource.starts_with(&target) {
                return Err(fail(
                    "新版下载目录与已登记权重目录重叠，已保留现有记录和文件。新建版本请改用独立目录。",
                ));
            }
            Ok(())
        };
        for active in db::list_store_models().map_err(|e| fail(e.to_string()))? {
            if managed_storage(&active)? {
                // Managed generations own their whole root, including files
                // beside the .deploy-env weight subdirectory.
                let root = active
                    .models_root
                    .as_deref()
                    .ok_or_else(|| fail("已登记隔离版本缺少权重目录，未创建或登记新版。"))?;
                check(Path::new(root))?;
            } else if let Some(root) = legacy_resource_dir(&active, configured_root) {
                // A legacy models_root may be shared; protect its actual
                // deployment resource, allowing normal .versions siblings.
                check(&root)?;
            } else if let Some(root) = active
                .runtime_card_id
                .as_ref()
                .and(active.models_root.as_deref())
            {
                // Restored legacy rows also carry runtime_card_id. If their
                // fixed root reappears, the existing resolver may fall back to
                // that whole root even without a resolvable deployment marker.
                check(Path::new(root))?;
            } else if let Some(declared) = declared_resource_dir(&active)? {
                // A missing legacy directory is still an explicit reference:
                // creating our candidate could make it resolvable again and
                // cause the unchanged deletion guard to retain the candidate.
                let allowed = planned_directory(
                    active
                        .models_root
                        .as_deref()
                        .map(Path::new)
                        .unwrap_or(configured_root),
                )?;
                let root = planned_directory(&declared)?;
                if root != allowed && root.starts_with(&allowed) {
                    check(&root)?;
                }
            }
        }
        for version in version_store::list(None).map_err(fail)? {
            if version.card_id == card_id && version.version_id == revision {
                continue;
            }
            let root = version
                .resource_dir
                .as_deref()
                .ok_or_else(|| fail("已登记历史版本缺少权重目录，未创建或登记新版。"))?;
            check(Path::new(root))?;
        }
        Ok(())
    }

    fn stage_verified(
        &self,
        current: &db::StoreModelRecord,
        card: &StoreCardDetail,
        revision: &str,
        pkg: CardPackage,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let card_id = current.card_id.as_str();
        let blob = pkg
            .assets
            .get(weight_identity::MANIFEST)
            .ok_or_else(|| fail("缺少权重校验依据"))?;
        let expected = card
            .asset_sha256
            .as_ref()
            .and_then(|h| h.get(weight_identity::MANIFEST))
            .ok_or_else(|| fail("缺少权重摘要"))?;
        let files = weight_identity::verified(blob, expected).map_err(fail)?;
        if files.len() != 1 {
            return Err(fail("暂不支持多文件隔离升级"));
        }
        let mut download_lease = version_store::acquire_download_lease(
            card_id,
            || control.cancelled(),
            || on_progress("等待上次下载脚本退出，完整存储预留保持不变，可取消…"),
        )
        .map_err(fail)?;
        let reservation = version_store::wait_quota_reservation_lock(
            || control.cancelled(),
            || on_progress("等待其他新版下载登记存储配额，可取消…"),
        )
        .map_err(fail)?;
        let existing = version_store::get(card_id, revision).map_err(fail)?;
        if let Some(v) = &existing {
            if ["ready", "current", "history"].contains(&v.state.as_str()) {
                return Ok(serde_json::json!({"version_id":revision}));
            }
            if !["failed", "downloading"].contains(&v.state.as_str()) {
                return Err(fail("此版本正在清理，请先完成删除后重试。"));
            }
        }
        // Exclude only this candidate's old reservation, retaining its actual
        // files. Refuse unreadable retained trees before creating a new one.
        let retained = if self.settings.storage_quota_gb.is_some() {
            version_store::recover_interrupted_for_quota().map_err(fail)?;
            version_store::retained_bytes_without_reservation(card_id, revision).map_err(fail)?
        } else {
            0
        };
        let safe = config::safe_path_component(card_id).map_err(fail)?;
        // Full revision IDs are validated before any path construction.
        if revision.len() != 64 || !revision.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(fail("无效版本标识"));
        }
        let storage_id = format!("{safe}--{}", &revision[..32]);
        let marker = serde_json::json!({"card_id":card_id,"version_id":revision}).to_string();
        let configured_root = Path::new(&self.settings.models_dir);
        let (models_root, required, quota_growth) = if let Some(v) = &existing {
            let root = PathBuf::from(
                v.models_root
                    .as_deref()
                    .ok_or_else(|| fail("失败版本缺少权重目录记录"))?,
            );
            let expected = serde_json::json!({"card_id":card_id,"version_id":revision}).to_string();
            if !v.managed
                || v.runtime_card_id != storage_id
                || v.resource_dir.as_deref() != v.models_root.as_deref()
                || !root.ends_with(Path::new(".versions").join(&safe).join(revision))
                || !existing_owned_directory(&root)?
                || !valid_resource_marker(&root, &expected, false)?
            {
                return Err(fail("失败版本目录归属不符，已保留原记录和资源。"));
            }
            self.ensure_stage_target_disjoint(&root, card_id, revision)?;
            // Scripts own their output layout, including HF cache directories.
            // Existing bytes remain charged, but neither a matching basename
            // nor an etag proves those bytes will be reused by this attempt.
            // Reserve a complete additional copy; the original script may still
            // resume and retain any required configuration side effects.
            (root, card.summary.size_bytes, card.summary.size_bytes)
        } else {
            let target =
                planned_directory(&configured_root.join(".versions").join(&safe).join(revision))?;
            // Even the space check creates directories. Reject overlaps before
            // it, ownership markers, runtime directories or version registration.
            self.ensure_stage_target_disjoint(&target, card_id, revision)?;
            // Preserve the normal download path's ability to create a newly
            // configured target root before owned_directory canonicalizes it.
            let initial_space = self.check_can_download_increment_with_retained(
                card.summary.size_bytes,
                card.summary.size_bytes,
                configured_root,
                retained,
            );
            if !initial_space.ok {
                return Err(fail(initial_space.reason));
            }
            let root = owned_directory(configured_root, &[".versions", &safe, revision])?;
            if root != target {
                return Err(fail("下载目录在检查后发生变化，未登记或下载新版。"));
            }
            (root, card.summary.size_bytes, card.summary.size_bytes)
        };
        let actual = version_store::quota_dir_size_bytes(&models_root).map_err(fail)?;
        let marker_bytes = if models_root.join(".pinvou-version.json").exists() {
            0
        } else {
            marker.len() as u64
        };
        let planned_bytes = actual
            .saturating_add(quota_growth)
            .saturating_add(marker_bytes);
        i64::try_from(planned_bytes).map_err(|e| fail(e.to_string()))?;
        let additional = if existing.is_some() {
            quota_growth.saturating_add(marker_bytes)
        } else {
            planned_bytes
        };
        let space = self.check_can_download_increment_with_retained(
            required,
            additional,
            &models_root,
            retained,
        );
        if !space.ok {
            return Err(fail(space.reason));
        }
        let cards = config::data_dir().join("cards");
        fs::create_dir_all(&cards).map_err(|e| fail(e.to_string()))?;
        owned_directory(&cards, &[&storage_id])?;
        ensure_unused(
            &config::data_dir().join("cards").join(&storage_id),
            Some(&models_root),
        )?;
        let marker_path = models_root.join(".pinvou-version.json");
        match fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&marker_path)
        {
            Ok(mut file) => {
                use std::io::Write;
                file.write_all(marker.as_bytes())
                    .map_err(|e| fail(e.to_string()))?;
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                let meta = marker_path
                    .symlink_metadata()
                    .map_err(|e| fail(e.to_string()))?;
                if !meta.is_file()
                    || meta.nlink() != 1
                    || fs::read_to_string(&marker_path).ok().as_deref() != Some(&marker)
                {
                    return Err(fail("版本目录归属不符"));
                }
            }
            Err(e) => return Err(fail(e.to_string())),
        }
        let mut version = Version {
            card_id: card_id.into(),
            version_id: revision.into(),
            state: "downloading".into(),
            record: None,
            models_root: Some(models_root.to_string_lossy().into_owned()),
            runtime_card_id: storage_id.clone(),
            resource_dir: Some(models_root.to_string_lossy().into_owned()),
            managed: true,
            created_at: existing
                .as_ref()
                .map(|v| v.created_at.clone())
                .unwrap_or_else(now),
            error: None,
            planned_bytes,
        };
        version_store::put(&version).map_err(fail)?;
        drop(reservation);
        let result = (|| {
            if control.cancelled() {
                return Err(fail("已取消"));
            }
            on_progress("下载并校验新版卡片；当前部署与旧版权重保持可用。");
            let source = ScriptsSource::Local {
                blobs: pkg.scripts.clone(),
                assets: pkg.assets.clone(),
                profiles: card.profiles.clone(),
            };
            let mut env = self.env_for_card(&card, &current.model_name);
            env.insert(
                "LMM_MODELS_DIR".into(),
                models_root.to_string_lossy().into_owned(),
            );
            env.insert("LMM_STORAGE_CARD_ID".into(), storage_id.clone());
            let probe = self.engine_probe(&source, card_id, &env);
            self.install_engine(&source, &env, on_progress, control.clone(), probe.need_root)?;
            db::mark_engine_installed(
                &card.summary.engine_name,
                card.summary.engine_version.as_deref(),
                None,
                source.script_hash("install_engine").as_deref(),
            )
            .map_err(|e| fail(e.to_string()))?;
            self.run_card_script_with_retry(
                &source,
                &env,
                on_progress,
                control.clone(),
                &mut download_lease,
            )?;
            on_progress("独立校验新版权重 SHA-256；通过后才允许切换。");
            for (name, hash) in &files {
                let path = find_weight(&models_root, name)?;
                if file_digest(&path, &control, on_progress)? != *hash {
                    return Err(fail("新版权重完整性校验失败，当前部署未改变。"));
                }
            }
            let mut rec = (*current).clone();
            rec.card_name = card.summary.name.clone();
            rec.org = card.summary.org.clone();
            rec.description = card.summary.description.clone();
            rec.engine_name = card.summary.engine_name.clone();
            rec.engine_version = card.summary.engine_version.clone();
            rec.size_bytes =
                i64::try_from(card.summary.size_bytes).map_err(|e| fail(e.to_string()))?;
            rec.quantization = card.summary.quantization.clone();
            rec.source_repo_id = card.source_repo_id.clone();
            rec.source_filename = card.source_filename.clone();
            rec.deploy_topology = card.summary.deploy_topology;
            rec.profiles = card.profiles.clone();
            rec.active_profile_id = current
                .active_profile_id
                .as_ref()
                .filter(|id| rec.profiles.iter().any(|profile| &profile.id == *id))
                .cloned();
            rec.scripts = card
                .scripts
                .iter()
                .map(|(k, v)| (k.clone(), Some(v.clone())))
                .collect();
            rec.scripts_content = pkg.scripts;
            rec.assets_content = pkg.assets;
            rec.scripts_fetched_at = Some(now());
            rec.status = "downloaded".into();
            rec.running = false;
            rec.downloaded_at = now();
            rec.deployed_at = None;
            rec.endpoint_url = None;
            rec.endpoint_model = None;
            rec.container_name = None;
            rec.deployed_script_sha256 = None;
            rec.card_update_pending = false;
            rec.models_root = version.models_root.clone();
            rec.runtime_card_id = Some(storage_id);
            rec.active_version_id = Some(revision.into());
            version.record = Some(rec);
            version.state = "ready".into();
            version.planned_bytes = 0;
            version.error = None;
            Ok(())
        })();
        // Retain the fresh parent OFD across DB finalization. The barrier
        // covers local download descendants retaining that fd; external daemon
        // work still depends on the card script's synchronous completion contract.
        download_lease.confirm_stopped().map_err(fail)?;
        let _quota = version_store::wait_quota_reservation_lock(
            || control.cancelled(),
            || on_progress("等待提交新版下载状态，可取消…"),
        )
        .map_err(fail)?;
        if let Err(error) = result {
            version.state = "failed".into();
            version.planned_bytes = 0;
            version.error = Some(error.to_string());
            version_store::put(&version).map_err(fail)?;
            return Err(error);
        }
        version_store::put(&version).map_err(fail)?;
        on_progress("新版已验证，当前部署未切换；停用当前模型后，可在历史版本中切换。");
        Ok(serde_json::json!({"version_id":revision}))
    }

    pub fn switch_version(
        &self,
        card_id: &str,
        id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::wait_resource_operation_lock(
            true,
            || control.cancelled(),
            || on_progress("等待其他模型资源操作完成，可取消…"),
        )
        .map_err(fail)?;
        let current = self.get_record(card_id)?;
        ensure_not_deleting(&current)?;
        if current.running {
            return Err(fail("请先停用当前模型，再切换版本。"));
        }
        let managed = managed_storage(&current)?;
        let resource = if managed {
            current.models_root.as_ref().map(PathBuf::from)
        } else {
            legacy_resource_dir(&current, &self.settings.models_path())
        };
        let resource = resource.ok_or_else(|| {
            fail("无法确认当前版本的权重目录，不能保证回退；请先修复当前部署，未切换版本。")
        })?;
        ensure_unused(&runtime_path(&current)?, Some(&resource))?;
        let mut target = version_store::get(card_id, id)
            .map_err(fail)?
            .ok_or_else(|| fail("目标版本不存在"))?;
        if !["ready", "history"].contains(&target.state.as_str()) {
            return Err(fail("目标版本尚未验证完成"));
        }
        let managed_root = if target.managed {
            Some(managed_version_root(&target, card_id, id)?)
        } else {
            None
        };
        if target.state == "ready" {
            // A new upgrade promises a usable rollback. A history target is
            // also a recovery route and remains usable if the current file broke.
            if managed {
                let active_id = version_store::active_id(&current);
                let active = version_store::get(card_id, &active_id)
                    .map_err(fail)?
                    .ok_or_else(|| fail("当前托管版本缺少归属记录，未切换部署。"))?;
                if managed_version_root(&active, card_id, &active_id)? != resource
                    || current.runtime_card_id.as_deref() != Some(active.runtime_card_id.as_str())
                {
                    return Err(fail("当前版本目录归属不符，不能保证回退；未切换部署。"));
                }
            }
            on_progress("校验当前版本权重，确认升级后仍可回退。");
            let (name, hash) = single_weight(&current)?;
            let old_file = find_weight(&resource, &name).map_err(|_| {
                fail("当前权重缺失，不能保证升级后回退；请先修复当前部署，未切换版本。")
            })?;
            if file_digest(&old_file, &control, on_progress)? != hash {
                return Err(fail(
                    "当前权重校验失败，不能保证升级后回退；请先修复当前部署，未切换版本。",
                ));
            }
        }
        let is_upgrade = target.state == "ready";
        let rec = target
            .record
            .as_mut()
            .ok_or_else(|| fail("目标版本缺少部署记录"))?;
        for blob in rec
            .scripts_content
            .values()
            .chain(rec.assets_content.values())
        {
            if !script_runner::sha256_hex(&blob.content).eq_ignore_ascii_case(&blob.sha256) {
                return Err(fail("目标版本的本地卡片校验失败，未切换部署。"));
            }
        }
        let (name, hash) = single_weight(rec)?;
        let root = managed_root
            .or_else(|| legacy_resource_dir(rec, &self.settings.models_path()))
            .ok_or_else(|| fail("无法确认目标权重目录"))?;
        ensure_unused(&runtime_path(rec)?, Some(&root))?;
        on_progress("校验目标版本权重；当前卡片记录保持不变。");
        if file_digest(&find_weight(&root, &name)?, &control, on_progress)? != hash {
            return Err(fail("目标权重校验失败，未切换版本。"));
        }
        let source = self.local_scripts_source(rec)?;
        // A ready version has never been deployed: retain the user's latest
        // supported choice, including versions staged by older app releases.
        // A history version instead owns its saved deployment configuration.
        let saved_profile = if is_upgrade {
            current
                .active_profile_id
                .as_deref()
                .filter(|id| source.profiles().iter().any(|profile| profile.id == *id))
                .or(rec.active_profile_id.as_deref())
        } else {
            rec.active_profile_id.as_deref()
        };
        let selected_profile =
            super::super::deploy_profiles::selected_id(card_id, source.profiles(), saved_profile)
                .map(str::to_owned);
        let mut env = self.env_for_record(rec);
        if let Some(profile) = &selected_profile {
            env.insert("LMM_DEPLOY_PROFILE".into(), profile.clone());
        }
        let probe = self.engine_probe(&source, card_id, &env);
        if !probe.installed {
            let lines =
                self.install_engine(&source, &env, on_progress, control.clone(), probe.need_root)?;
            db::mark_engine_installed(
                &rec.engine_name,
                rec.engine_version.as_deref(),
                extract_marker(&lines, "LMM_ENGINE_IMAGE=").as_deref(),
                source.script_hash("install_engine").as_deref(),
            )
            .map_err(|e| fail(format!("记录引擎安装状态失败：{e}")))?;
        }
        self.run_card_script(&source, "deploy", &env, on_progress, control.clone(), false)?;
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        rec.status = "deployed".into();
        rec.deployed_at = Some(now());
        rec.card_update_pending = false;
        rec.running = false;
        rec.deployed_script_sha256 = source.script_hash("deploy");
        rec.active_profile_id = selected_profile;
        let mut old = Version {
            card_id: card_id.into(),
            version_id: version_store::active_id(&current),
            state: "history".into(),
            models_root: current.models_root.clone(),
            runtime_card_id: current
                .runtime_card_id
                .clone()
                .unwrap_or_else(|| card_id.into()),
            resource_dir: Some(resource.to_string_lossy().into_owned()),
            managed,
            created_at: current.downloaded_at.clone(),
            record: Some(current.clone()),
            error: None,
            planned_bytes: 0,
        };
        old.models_root = Some(
            current
                .models_root
                .clone()
                .unwrap_or_else(|| self.settings.models_path().to_string_lossy().into_owned()),
        );
        if let Some(record) = old.record.as_mut() {
            record.models_root = old.models_root.clone();
        }
        version_store::activate(&current, &old, &target).map_err(fail)?;
        on_progress("版本已切换并部署；旧版已归档，可以回退或清理。请启用模型。");
        Ok(serde_json::json!({"switched":true}))
    }

    pub fn delete_version(
        &self,
        card_id: &str,
        id: &str,
        on_progress: &mut ProgressFn<'_>,
        control: script_runner::RunControl,
    ) -> Result<serde_json::Value, ModelManagerError> {
        let _resources = version_store::wait_resource_operation_lock(
            true,
            || control.cancelled(),
            || on_progress("等待其他模型资源操作完成，可取消…"),
        )
        .map_err(fail)?;
        let current = self.get_record(card_id)?;
        if version_store::active_id(&current) == id {
            return Err(fail("当前版本不能从历史版本中删除。"));
        }
        let Some(mut version) = version_store::get(card_id, id).map_err(fail)? else {
            return Ok(
                serde_json::json!({"freed_bytes":0,"remaining_paths":[],"already_removed":true}),
            );
        };
        if ["current", "downloading"].contains(&version.state.as_str()) {
            return Err(fail("此版本正在使用，不能清理。"));
        }
        let runtime = config::data_dir()
            .join("cards")
            .join(config::safe_path_component(&version.runtime_card_id).map_err(fail)?);
        let resource = version
            .resource_dir
            .as_ref()
            .map(PathBuf::from)
            .ok_or_else(|| fail("无法确认旧版权重归属，未删除资源。"))?;
        ensure_unused(&runtime, Some(&resource))?;
        let others = version_store::list(None).map_err(fail)?;
        let active = db::list_store_models().map_err(|e| fail(e.to_string()))?;
        let shared_runtime = active
            .iter()
            .any(|r| runtime_path(r).ok().as_ref() == Some(&runtime))
            || others.iter().any(|v| {
                !(v.card_id == card_id && v.version_id == id)
                    && v.runtime_card_id == version.runtime_card_id
            });
        let shared_resource = active.iter().any(|other| {
            if let Some(path) = legacy_resource_dir(other, &self.settings.models_path()) {
                resource.starts_with(&path) || path.starts_with(&resource)
            } else {
                !version.managed
                    && version
                        .record
                        .as_ref()
                        .is_some_and(|owner| unresolved_weight_reference(other, owner))
            }
        }) || others
            .iter()
            .filter(|v| !(v.card_id == card_id && v.version_id == id))
            .filter_map(|v| v.resource_dir.as_ref())
            .map(PathBuf::from)
            .any(|other| resource.starts_with(&other) || other.starts_with(&resource));
        if shared_resource || shared_runtime {
            return Err(fail(
                "此版本的权重或运行目录仍被其他版本引用，未删除任何资源。",
            ));
        }
        if resource_presence(&runtime)?
            && !runtime
                .symlink_metadata()
                .map_err(|e| fail(e.to_string()))?
                .is_dir()
        {
            return Err(fail("运行目录归属异常，未删除资源。"));
        }
        let mut freed = 0u64;
        let mut legacy_file = None;
        let mut remaining = Vec::new();
        let resource_present = resource_presence(&resource)?;
        if resource_present {
            if !resource
                .symlink_metadata()
                .map_err(|e| fail(e.to_string()))?
                .is_dir()
            {
                return Err(fail("拒绝清理链接或非目录资源。"));
            }
            if version.managed {
                let expected = serde_json::json!({"card_id":card_id,"version_id":id}).to_string();
                // A crash between removing the last marker and the empty root
                // may leave only that directory. Never accept unknown contents.
                if !valid_resource_marker(&resource, &expected, version.state == "deleting")? {
                    return Err(fail("版本目录归属校验失败，未删除资源。"));
                }
                freed = allocated_exclusive(&resource)?;
            } else if let Some(rec) = version.record.as_ref() {
                let (name, hash) = single_weight(rec)?;
                // A complete bounded search also proves that externally removed
                // weights are absent. Ambiguous, unreadable or linked files still
                // fail before any record or unrelated resource can be removed.
                let file = locate_weight(&resource, &name)?;
                if let Some(file) = file {
                    let meta = file.symlink_metadata().map_err(|e| fail(e.to_string()))?;
                    if !meta.is_file() {
                        return Err(fail("旧版权重不是普通文件，未删除资源。"));
                    }
                    if !file
                        .canonicalize()
                        .map_err(|e| fail(e.to_string()))?
                        .starts_with(resource.canonicalize().map_err(|e| fail(e.to_string()))?)
                    {
                        return Err(fail("旧版权重路径越界"));
                    }
                    if file_digest(&file, &control, on_progress)? != hash {
                        return Err(fail(
                            "旧版权重已变化，未删除资源。请确认没有其他模型共享后手动移走或删除该权重，再重试清理；也可使用“只移除记录”保留全部文件并移除本卡及历史记录。",
                        ));
                    }
                    if meta.nlink() == 1 {
                        freed = meta.blocks() * 512;
                    }
                    legacy_file = Some(file);
                }
                remaining.push(resource.to_string_lossy().into_owned());
            } else {
                return Err(fail("旧版缺少文件清单，未删除资源。"));
            }
        }
        if control.cancelled() {
            return Err(fail("已取消"));
        }
        // Commit point: after this marker cleanup is resumable, never reported as
        // a rollbackable history item whose weights might already be gone.
        version.state = "deleting".into();
        version_store::put(&version).map_err(fail)?;
        if version.managed && resource_presence(&resource)? {
            remove_managed_resource(&resource)?;
        }
        if let Some(file) = legacy_file {
            fs::remove_file(file).map_err(|e| fail(e.to_string()))?;
        }
        if resource_presence(&runtime)? {
            fs::remove_dir_all(&runtime).map_err(|e| fail(e.to_string()))?;
        }
        version_store::remove(card_id, id).map_err(fail)?;
        on_progress("选中的历史版本已清理，共享引擎保留。统计只计入实际删除的独占文件块。");
        Ok(serde_json::json!({"freed_bytes":freed,"remaining_paths":remaining}))
    }
}

/// Preserve the ownership marker through any partial recursive deletion. Only
/// remove it once the tree is empty so permission/IO failures remain retryable.
fn remove_managed_resource(root: &Path) -> Result<(), ModelManagerError> {
    let marker = root.join(".pinvou-version.json");
    for entry in fs::read_dir(root).map_err(|e| fail(e.to_string()))? {
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        if entry.path() == marker {
            continue;
        }
        if entry.file_type().map_err(|e| fail(e.to_string()))?.is_dir() {
            fs::remove_dir_all(entry.path()).map_err(|e| fail(e.to_string()))?;
        } else {
            fs::remove_file(entry.path()).map_err(|e| fail(e.to_string()))?;
        }
    }
    match fs::remove_file(marker) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(fail(e.to_string())),
    }
    fs::remove_dir(root).map_err(|e| fail(e.to_string()))
}

/// The caller supplies the same disjoint candidate roots before and after
/// managed-current deletion. Unknown occupancy fails instead of becoming zero;
/// only a positively absent path contributes no remaining allocated blocks.
pub(super) fn allocated_exclusive_paths(paths: &[PathBuf]) -> Result<u64, ModelManagerError> {
    paths.iter().try_fold(0u64, |total, path| {
        if resource_presence(path)? {
            total
                .checked_add(allocated_exclusive(path)?)
                .ok_or_else(|| fail("独占文件块统计溢出，无法确认释放量。"))
        } else {
            Ok(total)
        }
    })
}

fn allocated_exclusive(path: &Path) -> Result<u64, ModelManagerError> {
    let mut total = 0;
    for entry in fs::read_dir(path).map_err(|e| fail(e.to_string()))? {
        let entry = entry.map_err(|e| fail(e.to_string()))?;
        let meta = entry
            .path()
            .symlink_metadata()
            .map_err(|e| fail(e.to_string()))?;
        if meta.is_dir() {
            total += allocated_exclusive(&entry.path())?;
        } else if meta.is_file() && meta.nlink() == 1 {
            total += meta.blocks() * 512;
        }
    }
    Ok(total)
}

#[cfg(test)]
mod tests;
