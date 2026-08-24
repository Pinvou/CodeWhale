//! Windows 内置 Python MCP 的按需依赖安装。
//!
//! 基础 Python 仍随安装包提供；体积较大的第三方包由受信任的内嵌 manifest
//! 锁定为 wheel + SHA-256，在用户目录按需下载。wheel 缓存按内容 hash 共享，
//! site-packages 按依赖锁 hash 隔离，因此应用升级不会覆盖已安装依赖。

use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

use sha2::{Digest, Sha256};

use crate::platform::paths;

use super::store::AssetRef;
use super::types::{PythonWheel, ToolManifest, WindowsPythonDependencyLock};

const MAX_WHEEL_BYTES: u64 = 64 * 1024 * 1024;
const MAX_ENV_BYTES: u64 = 192 * 1024 * 1024;
const COMPLETE_MARKER: &str = ".pinvou-dependency-lock";

static PYTHON_DEPENDENCY_LOCK: Mutex<()> = Mutex::new(());

fn install_lock() -> MutexGuard<'static, ()> {
    PYTHON_DEPENDENCY_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

#[derive(Debug, Clone)]
pub(super) struct InstalledPythonDependencies {
    pub dir: PathBuf,
    pub asset: AssetRef,
}

pub(super) fn ensure_for_builtin_manifest(
    manifest: &ToolManifest,
) -> Result<Option<InstalledPythonDependencies>, String> {
    if manifest.pip_dependencies.is_empty() {
        return Ok(None);
    }
    let spec = manifest
        .windows_python_dependencies
        .as_ref()
        .ok_or_else(|| {
            format!(
                "内置 MCP '{}' 声明了 Python 依赖，但缺少 Windows 可验证依赖锁",
                manifest.id
            )
        })?;
    ensure_lock(spec).map(Some)
}

fn ensure_lock(spec: &WindowsPythonDependencyLock) -> Result<InstalledPythonDependencies, String> {
    validate_lock(spec)?;
    let _guard = install_lock();
    verify_python_target(&spec.target)?;

    let destination = paths::python_packages_dir(&spec.lock_id);
    if completed_lock_matches(&destination, &spec.lock_id)
        && probe_imports(&destination, &spec.imports).is_ok()
    {
        return Ok(installed(spec, destination));
    }

    let cache = paths::python_wheel_cache_dir();
    fs::create_dir_all(&cache).map_err(|e| format!("创建 Python wheel 缓存失败: {e}"))?;
    let parent = destination
        .parent()
        .ok_or_else(|| "Python 依赖目录缺少父目录".to_string())?;
    fs::create_dir_all(parent).map_err(|e| format!("创建 Python 依赖目录失败: {e}"))?;

    let staging = parent.join(format!(
        ".{}.installing-{}",
        spec.lock_id,
        std::process::id()
    ));
    let _ = fs::remove_dir_all(&staging);
    fs::create_dir_all(&staging).map_err(|e| format!("创建 Python 依赖暂存目录失败: {e}"))?;

    let result = (|| -> Result<(), String> {
        let client = dependency_client()?;
        let mut extracted = 0_u64;
        for wheel in &spec.wheels {
            let cached = ensure_wheel_cached(&client, wheel, &cache)?;
            extract_wheel(&cached, &staging, &mut extracted)?;
        }
        write_marker(&staging, &spec.lock_id)?;
        probe_imports(&staging, &spec.imports)?;
        Ok(())
    })();
    if let Err(error) = result {
        let _ = fs::remove_dir_all(&staging);
        return Err(error);
    }

    // lock_id 目录是不可变内容地址。已有目录若未通过 marker/import 检查，
    // 只可能是旧版半安装或损坏副本；先清掉再提升完整 staging。
    if destination.exists() {
        fs::remove_dir_all(&destination)
            .map_err(|e| format!("清理损坏的 Python 依赖目录失败: {e}"))?;
    }
    fs::rename(&staging, &destination).map_err(|e| {
        let _ = fs::remove_dir_all(&staging);
        format!("提交 Python 依赖目录失败: {e}")
    })?;
    Ok(installed(spec, destination))
}

fn installed(spec: &WindowsPythonDependencyLock, dir: PathBuf) -> InstalledPythonDependencies {
    InstalledPythonDependencies {
        dir,
        asset: AssetRef {
            kind: "python-packages".to_string(),
            name: spec.target.clone(),
            version: spec.lock_id.clone(),
            sha256: spec.lock_id.clone(),
        },
    }
}

fn validate_lock(spec: &WindowsPythonDependencyLock) -> Result<(), String> {
    if !is_sha256(&spec.lock_id) {
        return Err("Windows Python 依赖 lock_id 必须是小写 SHA-256".to_string());
    }
    if spec.target.trim().is_empty() || spec.imports.is_empty() || spec.wheels.is_empty() {
        return Err("Windows Python 依赖锁缺少 target/imports/wheels".to_string());
    }
    for import in &spec.imports {
        if !valid_import_name(import) {
            return Err(format!("不安全的 Python import 名称: {import}"));
        }
    }
    for wheel in &spec.wheels {
        validate_wheel(wheel)?;
    }
    let canonical = spec
        .wheels
        .iter()
        .map(|wheel| format!("{}:{}", wheel.filename, wheel.sha256))
        .collect::<Vec<_>>()
        .join("\n");
    let computed = hex_lower(&Sha256::digest(canonical.as_bytes()));
    if computed != spec.lock_id {
        return Err(format!(
            "Windows Python 依赖锁校验失败(expected {}, got {computed})",
            spec.lock_id
        ));
    }
    Ok(())
}

fn validate_wheel(wheel: &PythonWheel) -> Result<(), String> {
    if wheel.filename.contains('/')
        || wheel.filename.contains('\\')
        || !wheel.filename.ends_with(".whl")
        || !is_sha256(&wheel.sha256)
    {
        return Err(format!("无效的 Python wheel 声明: {}", wheel.filename));
    }
    let url = reqwest::Url::parse(&wheel.url)
        .map_err(|e| format!("{} 下载地址无效: {e}", wheel.filename))?;
    if url.scheme() != "https" || url.host_str() != Some("files.pythonhosted.org") {
        return Err(format!(
            "{} 仅允许从 files.pythonhosted.org 通过 HTTPS 下载",
            wheel.filename
        ));
    }
    Ok(())
}

fn valid_import_name(value: &str) -> bool {
    !value.is_empty()
        && value.split('.').all(|part| {
            let mut chars = part.chars();
            chars
                .next()
                .is_some_and(|c| c == '_' || c.is_ascii_alphabetic())
                && chars.all(|c| c == '_' || c.is_ascii_alphanumeric())
        })
}

fn is_sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn dependency_client() -> Result<reqwest::blocking::Client, String> {
    reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(15))
        .timeout(Duration::from_secs(180))
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            if attempt.previous().len() >= 10 || attempt.url().scheme() != "https" {
                attempt.stop()
            } else {
                attempt.follow()
            }
        }))
        .user_agent("Pinvou-Agent python-dependency-installer")
        .build()
        .map_err(|e| format!("创建 Python 依赖下载客户端失败: {e}"))
}

fn ensure_wheel_cached(
    client: &reqwest::blocking::Client,
    wheel: &PythonWheel,
    cache: &Path,
) -> Result<PathBuf, String> {
    let destination = cache.join(format!("{}.whl", wheel.sha256));
    if crate::platform::hashing::sha256_file(&destination)
        .is_ok_and(|actual| actual == wheel.sha256)
    {
        return Ok(destination);
    }

    let response = client
        .get(&wheel.url)
        .send()
        .and_then(reqwest::blocking::Response::error_for_status)
        .map_err(|e| format!("下载 {} 失败: {e}", wheel.filename))?;
    if response
        .content_length()
        .is_some_and(|length| length > MAX_WHEEL_BYTES)
    {
        return Err(format!("{} 超过 64 MiB 安全上限", wheel.filename));
    }

    let partial = cache.join(format!(".{}.{}.part", wheel.sha256, std::process::id()));
    let _ = fs::remove_file(&partial);
    let mut reader = response.take(MAX_WHEEL_BYTES + 1);
    let mut file = File::create(&partial).map_err(|e| format!("创建 wheel 暂存文件失败: {e}"))?;
    let copied = io::copy(&mut reader, &mut file).map_err(|e| format!("保存 wheel 失败: {e}"))?;
    file.sync_all()
        .map_err(|e| format!("同步 wheel 失败: {e}"))?;
    if copied > MAX_WHEEL_BYTES {
        let _ = fs::remove_file(&partial);
        return Err(format!("{} 超过 64 MiB 安全上限", wheel.filename));
    }
    let actual = crate::platform::hashing::sha256_file(&partial)
        .map_err(|e| format!("校验 {} 失败: {e}", wheel.filename))?;
    if actual != wheel.sha256 {
        let _ = fs::remove_file(&partial);
        return Err(format!(
            "{} 下载校验失败(expected {}, got {actual})",
            wheel.filename, wheel.sha256
        ));
    }
    if destination.exists() {
        fs::remove_file(&destination).map_err(|e| format!("替换 wheel 缓存失败: {e}"))?;
    }
    fs::rename(&partial, &destination).map_err(|e| format!("提交 wheel 缓存失败: {e}"))?;
    Ok(destination)
}

fn extract_wheel(
    wheel_path: &Path,
    destination: &Path,
    extracted_total: &mut u64,
) -> Result<(), String> {
    let file = File::open(wheel_path).map_err(|e| format!("打开 wheel 失败: {e}"))?;
    let mut archive = zip::ZipArchive::new(file).map_err(|e| format!("读取 wheel 失败: {e}"))?;
    for index in 0..archive.len() {
        let entry = archive
            .by_index(index)
            .map_err(|e| format!("读取 wheel 条目 #{index} 失败: {e}"))?;
        let Some(relative) = entry.enclosed_name() else {
            return Err("wheel 含不安全路径（穿越），拒绝安装".to_string());
        };
        if entry
            .unix_mode()
            .is_some_and(|mode| mode & 0o170000 == 0o120000)
        {
            return Err("wheel 含符号链接，拒绝安装".to_string());
        }
        let output = destination.join(relative);
        if entry.is_dir() {
            fs::create_dir_all(&output).map_err(|e| format!("创建 wheel 目录失败: {e}"))?;
            continue;
        }
        let parent = output
            .parent()
            .ok_or_else(|| "wheel 条目缺少父目录".to_string())?;
        fs::create_dir_all(parent).map_err(|e| format!("创建 wheel 目录失败: {e}"))?;
        let remaining = MAX_ENV_BYTES.saturating_sub(*extracted_total);
        let mut output_file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&output)
            .map_err(|e| format!("wheel 文件冲突 {}: {e}", output.display()))?;
        let copied = io::copy(&mut entry.take(remaining + 1), &mut output_file)
            .map_err(|e| format!("解压 wheel 条目失败: {e}"))?;
        if copied > remaining {
            return Err("Python 依赖解压后超过 192 MiB 安全上限".to_string());
        }
        *extracted_total += copied;
    }
    Ok(())
}

fn write_marker(directory: &Path, lock_id: &str) -> Result<(), String> {
    let path = directory.join(COMPLETE_MARKER);
    let mut file = File::create(path).map_err(|e| format!("写依赖锁标记失败: {e}"))?;
    file.write_all(lock_id.as_bytes())
        .and_then(|()| file.sync_all())
        .map_err(|e| format!("同步依赖锁标记失败: {e}"))
}

fn completed_lock_matches(directory: &Path, lock_id: &str) -> bool {
    fs::read_to_string(directory.join(COMPLETE_MARKER)).is_ok_and(|value| value == lock_id)
}

fn verify_python_target(expected: &str) -> Result<(), String> {
    let script = "import platform,sys; m=platform.machine().lower(); p='win_amd64' if m in ('amd64','x86_64') else 'win_'+m; a=f'cp{sys.version_info.major}{sys.version_info.minor}-{p}'; raise SystemExit(0 if a==sys.argv[1] else 17)";
    let output = std::process::Command::new(paths::python_command())
        .args(["-I", "-c", script, expected])
        .output()
        .map_err(|e| format!("无法运行内置 Python: {e}"))?;
    if output.status.success() {
        Ok(())
    } else {
        Err(format!(
            "内置 Python 与依赖目标 {expected} 不兼容，请更新 MCP 依赖锁"
        ))
    }
}

fn probe_imports(directory: &Path, imports: &[String]) -> Result<(), String> {
    let script = "import importlib,sys; sys.path.insert(0,sys.argv[1]); [importlib.import_module(n) for n in sys.argv[2:]]";
    let output = std::process::Command::new(paths::python_command())
        .args(["-I", "-B", "-c", script])
        .arg(directory)
        .args(imports)
        .output()
        .map_err(|e| format!("无法运行内置 Python 依赖验证: {e}"))?;
    if output.status.success() {
        return Ok(());
    }
    let detail = String::from_utf8_lossy(&output.stderr);
    Err(format!(
        "Python 依赖导入验证失败（{}）：{}",
        imports.join(", "),
        detail.lines().last().unwrap_or("未知错误")
    ))
}

fn hex_lower(bytes: &[u8]) -> String {
    crate::platform::encoding::hex_lower(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dependency_lock_rejects_hash_drift() {
        let spec = WindowsPythonDependencyLock {
            target: "cp313-win_amd64".into(),
            lock_id: "0".repeat(64),
            imports: vec!["docx".into()],
            wheels: vec![PythonWheel {
                filename: "demo-1.0-py3-none-any.whl".into(),
                url: "https://files.pythonhosted.org/packages/demo.whl".into(),
                sha256: "1".repeat(64),
            }],
        };
        assert!(validate_lock(&spec).unwrap_err().contains("依赖锁校验失败"));
    }

    #[test]
    fn dependency_lock_rejects_untrusted_host_and_import_code() {
        let wheel = PythonWheel {
            filename: "demo.whl".into(),
            url: "https://example.com/demo.whl".into(),
            sha256: "a".repeat(64),
        };
        assert!(validate_wheel(&wheel).is_err());
        assert!(!valid_import_name("docx;import os"));
        assert!(valid_import_name("lxml.etree"));
    }

    #[test]
    fn builtin_document_dependency_locks_are_self_consistent() {
        for manifest_json in [
            include_str!("../../../../resources/mcp-servers/gongwen/manifest.json"),
            include_str!("../../../../resources/mcp-servers/pptx/manifest.json"),
        ] {
            let manifest: ToolManifest = serde_json::from_str(manifest_json).unwrap();
            let lock = manifest.windows_python_dependencies.as_ref().unwrap();
            validate_lock(lock).unwrap_or_else(|error| panic!("{}: {error}", manifest.id));
        }
    }
}
