//! Windows 内置 Python MCP 的按需依赖安装。
//!
//! 基础 Python 仍随安装包提供；体积较大的第三方包由受信任的内嵌 manifest
//! 锁定为 wheel + SHA-256，在用户目录按需下载。wheel 缓存按内容 hash 共享，
//! site-packages 按依赖锁 hash 隔离，因此应用升级不会覆盖已安装依赖。
//!
//! GC 策略（与 native_installer 一致，留待后续 PR）：崩溃残留的
//! `.{lock_id}.installing-{pid}` 暂存目录、`.{sha}.{pid}.part` 部分下载、以及
//! 依赖锁升级后遗留的旧 `python-packages/<lock_id>` 目录暂无回收路径。因目录
//! 按内容寻址且锁数量极少，滞留量有界（数十 MB 级），属有意取舍而非遗漏。

use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

use sha2::{Digest, Sha256};

use crate::platform::paths;

use super::store::AssetRef;
use super::types::{PythonWheel, ToolManifest, WindowsPythonDependencyLock};

const MAX_WHEEL_BYTES: u64 = 64 * 1024 * 1024;
const MAX_ENV_BYTES: u64 = 192 * 1024 * 1024;
const PYTHON_PROBE_TIMEOUT: Duration = Duration::from_secs(30);
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
    run_staged(&staging, || -> Result<(), String> {
        let client = dependency_client()?;
        let mut extracted = 0_u64;
        for wheel in &spec.wheels {
            let cached = ensure_wheel_cached(&client, wheel, &cache)?;
            extract_wheel(&cached, &staging, &mut extracted)?;
        }
        write_marker(&staging, &spec.lock_id)?;
        probe_imports(&staging, &spec.imports)?;
        Ok(())
    })?;

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
        validate_wheel_target(&wheel.filename, &spec.target)?;
    }
    let canonical = canonical_lock(spec);
    let computed = hex_lower(&Sha256::digest(canonical.as_bytes()));
    if computed != spec.lock_id {
        return Err(format!(
            "Windows Python 依赖锁校验失败(expected {}, got {computed})",
            spec.lock_id
        ));
    }
    Ok(())
}

fn run_staged<F>(staging: &Path, operation: F) -> Result<(), String>
where
    F: FnOnce() -> Result<(), String>,
{
    let _ = fs::remove_dir_all(staging);
    fs::create_dir_all(staging).map_err(|e| format!("创建 Python 依赖暂存目录失败: {e}"))?;
    match operation() {
        Ok(()) => Ok(()),
        Err(error) => {
            let _ = fs::remove_dir_all(staging);
            Err(error)
        }
    }
}

fn canonical_lock(spec: &WindowsPythonDependencyLock) -> String {
    std::iter::once(format!("target:{}", spec.target))
        .chain(
            spec.wheels
                .iter()
                .map(|wheel| format!("{}:{}", wheel.filename, wheel.sha256)),
        )
        .collect::<Vec<_>>()
        .join("\n")
}

fn parse_target(target: &str) -> Result<(&str, &str), String> {
    let (python, platform) = target
        .split_once('-')
        .ok_or_else(|| format!("无效的 Windows Python target: {target}"))?;
    if !python.starts_with("cp")
        || python.len() < 4
        || !python[2..].bytes().all(|byte| byte.is_ascii_digit())
        || platform.is_empty()
        || !platform
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_')
    {
        return Err(format!("无效的 Windows Python target: {target}"));
    }
    Ok((python, platform))
}

fn interpreter_version(tag: &str, prefix: &str) -> Option<(u32, u32)> {
    let digits = tag.strip_prefix(prefix)?;
    if digits.len() < 2 || !digits.bytes().all(|byte| byte.is_ascii_digit()) {
        return None;
    }
    let (major, minor) = digits.split_at(1);
    Some((major.parse().ok()?, minor.parse().ok()?))
}

fn compatible_pure_python_tag(tag: &str, target: (u32, u32)) -> bool {
    tag == format!("py{}", target.0)
        || interpreter_version(tag, "py")
            .is_some_and(|version| version.0 == target.0 && version.1 <= target.1)
}

/// 校验 wheel 文件名末尾的 PEP 425 `python-abi-platform` 三元组。
/// 兼容集合与 `packaging.tags` 的 CPython supported tags 对齐：目标解释器及不高于
/// 目标版本的 `py3x-none-{platform,any}`、目标 `cp3x-none-{platform,any}`，以及
/// CPython 3.2 起不高于目标版本的同平台稳定 ABI（abi3）。
fn validate_wheel_target(filename: &str, target: &str) -> Result<(), String> {
    let (target_python, target_platform) = parse_target(target)?;
    let stem = filename
        .strip_suffix(".whl")
        .ok_or_else(|| format!("无效的 wheel 文件名: {filename}"))?;
    let mut tags = stem.rsplitn(4, '-');
    let platforms = tags.next().unwrap_or_default();
    let abis = tags.next().unwrap_or_default();
    let pythons = tags.next().unwrap_or_default();
    if tags.next().is_none() || pythons.is_empty() || abis.is_empty() || platforms.is_empty() {
        return Err(format!("wheel 缺少 PEP 425 标签: {filename}"));
    }
    let target_version = interpreter_version(target_python, "cp")
        .ok_or_else(|| format!("无效的 Windows Python target: {target}"))?;
    let valid_tag = |tag: &str| {
        !tag.is_empty()
            && tag
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_')
    };
    let python_tags = pythons.split('.').collect::<Vec<_>>();
    let abi_tags = abis.split('.').collect::<Vec<_>>();
    let platform_tags = platforms.split('.').collect::<Vec<_>>();
    if python_tags
        .iter()
        .chain(abi_tags.iter())
        .chain(platform_tags.iter())
        .any(|tag| !valid_tag(tag))
    {
        return Err(format!("wheel 含非法 PEP 425 标签: {filename}"));
    }
    for python in &python_tags {
        for abi in &abi_tags {
            for platform in &platform_tags {
                let pure_python = *abi == "none"
                    && (compatible_pure_python_tag(python, target_version)
                        || *python == target_python);
                let exact_cpython = *python == target_python && *abi == target_python;
                let stable_abi = *abi == "abi3"
                    && interpreter_version(python, "cp").is_some_and(|version| {
                        version.0 == target_version.0
                            && version >= (3, 2)
                            && version <= target_version
                    });
                let compatible = if *platform == target_platform {
                    exact_cpython || stable_abi || pure_python
                } else if *platform == "any" {
                    pure_python
                } else {
                    false
                };
                if compatible {
                    return Ok(());
                }
            }
        }
    }
    Err(format!(
        "wheel {filename} 的 PEP 425 标签与 target {target} 不兼容"
    ))
}

fn validate_wheel(wheel: &PythonWheel) -> Result<(), String> {
    // 文件名字符集收紧到 wheel 文件名实际可用集合（PEP 427 转义后仅含字母、
    // 数字与 `._+-`）：既挡路径分隔符，也杜绝 canonical lock（`filename:sha256`
    // 按行拼接）因 filename 含 `:`/换行而产生拼接歧义。
    let valid_filename = wheel.filename.ends_with(".whl")
        && wheel
            .filename
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'+' | b'-'));
    if !valid_filename || !is_sha256(&wheel.sha256) {
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
        // 锁中 URL 已固定到可信主机；禁止重定向，避免响应把请求带到未审计主机。
        .redirect(reqwest::redirect::Policy::none())
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
    if destination.exists() {
        fs::remove_file(&destination).map_err(|e| format!("清理损坏的 wheel 缓存失败: {e}"))?;
    }

    let response = client
        .get(&wheel.url)
        .send()
        .map_err(|e| format!("下载 {} 失败: {e}", wheel.filename))?;
    if !response.status().is_success() {
        return Err(format!(
            "下载 {} 失败: HTTP {}（不允许重定向）",
            wheel.filename,
            response.status()
        ));
    }
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
    extract_wheel_with_limit(wheel_path, destination, extracted_total, MAX_ENV_BYTES)
}

fn extract_wheel_with_limit(
    wheel_path: &Path,
    destination: &Path,
    extracted_total: &mut u64,
    max_env_bytes: u64,
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
        let remaining = max_env_bytes.saturating_sub(*extracted_total);
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

fn run_command_with_timeout(
    command: Command,
    operation: &str,
    timeout: Duration,
) -> Result<Output, String> {
    crate::platform::process::output_with_timeout_and_kill_tree(command, timeout)
        .map_err(|error| format!("{operation}失败: {error}"))
}

fn verify_python_target(expected: &str) -> Result<(), String> {
    let script = "import platform,sys; m=platform.machine().lower(); p='win_amd64' if m in ('amd64','x86_64') else 'win_'+m; a=f'cp{sys.version_info.major}{sys.version_info.minor}-{p}'; raise SystemExit(0 if a==sys.argv[1] else 17)";
    let mut command = Command::new(paths::python_command());
    command.args(["-I", "-c", script, expected]);
    let output = run_command_with_timeout(command, "内置 Python ABI 验证", PYTHON_PROBE_TIMEOUT)?;
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
    let mut command = Command::new(paths::python_command());
    command
        .args(["-I", "-B", "-c", script])
        .arg(directory)
        .args(imports);
    let output =
        run_command_with_timeout(command, "内置 Python 依赖导入验证", PYTHON_PROBE_TIMEOUT)?;
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
    use std::net::TcpListener;

    fn write_test_wheel(path: &Path, entries: &[(&str, &[u8], Option<u32>)]) {
        let file = File::create(path).unwrap();
        let mut writer = zip::ZipWriter::new(file);
        for (name, content, mode) in entries {
            let mut options = zip::write::SimpleFileOptions::default();
            if let Some(mode) = mode {
                options = options.unix_permissions(*mode);
            }
            writer.start_file(*name, options).unwrap();
            writer.write_all(content).unwrap();
        }
        writer.finish().unwrap();
    }

    fn one_response_server(status: &str, headers: &str, body: &[u8]) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let status = status.to_string();
        let headers = headers.to_string();
        let body = body.to_vec();
        std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 1024];
            let _ = stream.read(&mut request);
            write!(
                stream,
                "HTTP/1.1 {status}\r\nContent-Length: {}\r\n{headers}\r\n",
                body.len()
            )
            .unwrap();
            stream.write_all(&body).unwrap();
        });
        format!("http://{address}/demo.whl")
    }

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
    fn wheel_filename_charset_blocks_canonical_lock_ambiguity() {
        // canonical lock 以 `filename:sha256` 按行拼接；filename 若允许 `:` 或
        // 换行，可构造两个不同 wheel 列表拼出相同 canonical 串（共享 lock_id
        // 安装目录）。字符集白名单同时封死路径分隔符与拼接歧义。
        for bad in [
            "demo:1.0-py3-none-any.whl",
            "demo\n-1.0-py3-none-any.whl",
            "demo/evil-1.0-py3-none-any.whl",
            "demo\\evil-1.0-py3-none-any.whl",
            "../demo-1.0-py3-none-any.whl",
        ] {
            let wheel = PythonWheel {
                filename: bad.into(),
                url: "https://files.pythonhosted.org/packages/demo.whl".into(),
                sha256: "a".repeat(64),
            };
            assert!(
                validate_wheel(&wheel).is_err(),
                "{bad:?} should be rejected"
            );
        }
        let good = PythonWheel {
            filename: "demo-1.0+local-py3-none-any.whl".into(),
            url: "https://files.pythonhosted.org/packages/demo.whl".into(),
            sha256: "a".repeat(64),
        };
        assert!(validate_wheel(&good).is_ok());
    }

    #[test]
    fn cp314_target_rejects_cp313_non_stable_wheel() {
        let error =
            validate_wheel_target("lxml-6.1.2-cp313-cp313-win_amd64.whl", "cp314-win_amd64")
                .unwrap_err();
        assert!(error.contains("不兼容"));
        assert!(
            validate_wheel_target("python_docx-1.2.0-py3-none-any.whl", "cp314-win_amd64").is_ok()
        );
    }

    #[test]
    fn wheel_target_checks_compatible_tag_combinations() {
        let target = "cp313-win_amd64";
        for compatible in [
            "demo-1-cp313-cp313-win_amd64.whl",
            "demo-1-cp313-abi3-win_amd64.whl",
            "demo-1-cp312-abi3-win_amd64.whl",
            "demo-1-cp32-abi3-win_amd64.whl",
            "demo-1-cp313-none-any.whl",
            "demo-1-py313-none-any.whl",
            "demo-1-py312-none-any.whl",
            "demo-1-py313-none-win_amd64.whl",
            "demo-1-py3-none-any.whl",
        ] {
            validate_wheel_target(compatible, target)
                .unwrap_or_else(|error| panic!("{compatible}: {error}"));
        }
        for incompatible in [
            "demo-1-cp27-abi3-win_amd64.whl",
            "demo-1-cp31-abi3-win_amd64.whl",
            "demo-1-cp314-abi3-win_amd64.whl",
            "demo-1-py314-none-any.whl",
            "demo-1-cp313-cp313-win_arm64.whl",
            "demo-1-CP313-cp313-win_amd64.whl",
            "demo-1-cp313-cp313-any.whl",
            "demo-1-cp313-abi3-any.whl",
        ] {
            assert!(
                validate_wheel_target(incompatible, target).is_err(),
                "{incompatible} should be rejected"
            );
        }
    }

    #[test]
    fn dependency_lock_hash_binds_target() {
        let mut spec = WindowsPythonDependencyLock {
            target: "cp313-win_amd64".into(),
            lock_id: String::new(),
            imports: vec!["demo".into()],
            wheels: vec![PythonWheel {
                filename: "demo-1.0-py3-none-any.whl".into(),
                url: "https://files.pythonhosted.org/packages/demo.whl".into(),
                sha256: "1".repeat(64),
            }],
        };
        spec.lock_id = hex_lower(&Sha256::digest(canonical_lock(&spec).as_bytes()));
        validate_lock(&spec).unwrap();
        spec.target = "cp314-win_amd64".into();
        assert!(validate_lock(&spec).is_err());
    }

    #[test]
    fn wheel_extraction_rejects_traversal_symlink_limit_and_collision() {
        let temp = tempfile::tempdir().unwrap();
        let destination = temp.path().join("site");
        fs::create_dir_all(&destination).unwrap();

        let traversal = temp.path().join("traversal.whl");
        write_test_wheel(&traversal, &[("../escape.py", b"x", None)]);
        assert!(extract_wheel(&traversal, &destination, &mut 0).is_err());
        assert!(!temp.path().join("escape.py").exists());

        let symlink = temp.path().join("symlink.whl");
        let file = File::create(&symlink).unwrap();
        let mut writer = zip::ZipWriter::new(file);
        writer
            .add_symlink(
                "link.py",
                "target.py",
                zip::write::SimpleFileOptions::default(),
            )
            .unwrap();
        writer.finish().unwrap();
        assert!(extract_wheel(&symlink, &destination, &mut 0).is_err());

        let oversized = temp.path().join("oversized.whl");
        write_test_wheel(&oversized, &[("large.bin", b"12345", None)]);
        assert!(extract_wheel_with_limit(&oversized, &destination, &mut 0, 4).is_err());

        let first = temp.path().join("first.whl");
        let second = temp.path().join("second.whl");
        write_test_wheel(&first, &[("same.py", b"first", None)]);
        write_test_wheel(&second, &[("same.py", b"second", None)]);
        let collision_destination = temp.path().join("collision");
        fs::create_dir_all(&collision_destination).unwrap();
        let mut total = 0;
        extract_wheel(&first, &collision_destination, &mut total).unwrap();
        assert!(extract_wheel(&second, &collision_destination, &mut total).is_err());
    }

    #[test]
    fn failed_probe_path_cleans_staging() {
        let temp = tempfile::tempdir().unwrap();
        let staging = temp.path().join("staging");
        let error = run_staged(&staging, || {
            fs::write(staging.join("partial.py"), b"partial").unwrap();
            Err("probe failed".into())
        })
        .unwrap_err();
        assert_eq!(error, "probe failed");
        assert!(!staging.exists());
    }

    #[test]
    fn downloader_rejects_redirect_and_removes_bad_cache() {
        let temp = tempfile::tempdir().unwrap();
        let client = dependency_client().unwrap();
        let redirect = PythonWheel {
            filename: "demo-1.0-py3-none-any.whl".into(),
            url: one_response_server(
                "302 Found",
                "Location: https://example.com/evil.whl\r\n",
                b"",
            ),
            sha256: "a".repeat(64),
        };
        let error = ensure_wheel_cached(&client, &redirect, temp.path()).unwrap_err();
        assert!(error.contains("HTTP 302"));

        let bad_cache = temp.path().join(format!("{}.whl", "b".repeat(64)));
        fs::write(&bad_cache, b"corrupt").unwrap();
        let unavailable = PythonWheel {
            filename: "demo-1.0-py3-none-any.whl".into(),
            url: one_response_server("500 Internal Server Error", "", b""),
            sha256: "b".repeat(64),
        };
        assert!(ensure_wheel_cached(&client, &unavailable, temp.path()).is_err());
        assert!(!bad_cache.exists());
    }

    #[test]
    fn builtin_python_dependency_locks_are_self_consistent() {
        let mut checked = 0;
        for package in super::super::mcp_catalog::MCP_PACKAGES {
            let manifest: ToolManifest = serde_json::from_str(package.manifest_json).unwrap();
            // 与运行期 ensure_for_builtin_manifest 的 fail-closed 对齐：声明了
            // pip 依赖的内置 MCP 必须随附 Windows 依赖锁，防漏配在构建期被拦截。
            assert!(
                manifest.pip_dependencies.is_empty()
                    || manifest.windows_python_dependencies.is_some(),
                "{} 声明了 pip_dependencies 但缺少 windows_python_dependencies 锁",
                manifest.id
            );
            if manifest.windows_python_dependencies.is_none() {
                continue;
            }
            let lock = manifest.windows_python_dependencies.as_ref().unwrap();
            validate_lock(lock).unwrap_or_else(|error| panic!("{}: {error}", manifest.id));
            checked += 1;
        }
        assert!(checked > 0, "内置目录至少应声明一个 Windows Python 依赖锁");
    }
}
