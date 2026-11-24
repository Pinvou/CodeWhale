//! 连接器 CLI 的跨功能低层原语：编译期 lock 表访问、可执行文件命名、文件 SHA-256。
//!
//! lock 表（`resources/platforms/<os>/<arch>/bundle/connectors/connectors.lock.json`）
//! 同时被两类功能消费：`features/connectors`（native_installer 下载安装）与
//! `features/marketplace`（store 首启导入时校验存量 CLI 二进制）。按「跨功能复用的
//! 低层能力进全局 platform」的边界从这里供数；`connectors::platform` 的 `lock_json` /
//! `executable_name` 保留为委托，既有调用方零改动。

use serde::Deserialize;

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
const LOCK_JSON: &str =
    include_str!("../../resources/platforms/linux/aarch64/bundle/connectors/connectors.lock.json");
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
const LOCK_JSON: &str =
    include_str!("../../resources/platforms/linux/x86_64/bundle/connectors/connectors.lock.json");
#[cfg(all(target_os = "macos", target_arch = "aarch64"))]
const LOCK_JSON: &str =
    include_str!("../../resources/platforms/macos/aarch64/bundle/connectors/connectors.lock.json");
#[cfg(all(target_os = "macos", target_arch = "x86_64"))]
const LOCK_JSON: &str =
    include_str!("../../resources/platforms/macos/x86_64/bundle/connectors/connectors.lock.json");
#[cfg(all(target_os = "windows", target_arch = "x86_64"))]
const LOCK_JSON: &str =
    include_str!("../../resources/platforms/windows/x86_64/bundle/connectors/connectors.lock.json");
#[cfg(not(any(
    all(target_os = "linux", target_arch = "aarch64"),
    all(target_os = "linux", target_arch = "x86_64"),
    all(target_os = "macos", target_arch = "aarch64"),
    all(target_os = "macos", target_arch = "x86_64"),
    all(target_os = "windows", target_arch = "x86_64"),
)))]
const LOCK_JSON: &str = "";

pub fn lock_json() -> &'static str {
    LOCK_JSON
}

pub fn executable_name(name: &str) -> String {
    if cfg!(windows) {
        format!("{name}.exe")
    } else {
        name.to_string()
    }
}

/// lock 表钉住的单个 CLI 制品信息（版本 + 二进制 SHA-256）。
/// 只暴露校验存量二进制所需的两个字段；下载地址等仍由 native_installer 私有schema持有。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConnectorArtifactPin {
    pub version: String,
    pub binary_sha256: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ConnectorLock {
    schema_version: u32,
    platform: String,
    artifacts: Vec<Artifact>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Artifact {
    name: String,
    version: String,
    binary_sha256: String,
}

/// 按名取 lock 表钉住的制品信息。lock 缺失 / schema 版本不符 / 平台不匹配 /
/// 无此条目时返回 None（调用方按「无法校验」处理，不当作校验通过）。
pub fn artifact_pin(name: &str) -> Option<ConnectorArtifactPin> {
    load_pins()?
        .into_iter()
        .find(|(n, _)| n == name)
        .map(|(_, pin)| pin)
}

/// lock 表全部制品的 pin（PATH 注入等需要枚举全部 CLI 的场景）。
/// 与 [`artifact_pin`] 同一校验口径；lock 不可用时返回空 vec。
pub fn all_artifact_pins() -> Vec<(String, ConnectorArtifactPin)> {
    load_pins().unwrap_or_default()
}

fn load_pins() -> Option<Vec<(String, ConnectorArtifactPin)>> {
    let lock: ConnectorLock = serde_json::from_str(lock_json()).ok()?;
    if lock.schema_version != 1 {
        return None;
    }
    let expected =
        super::paths::connector_platform_dir(std::env::consts::OS, std::env::consts::ARCH)?;
    if lock.platform != expected {
        return None;
    }
    Some(
        lock.artifacts
            .iter()
            .map(|artifact| {
                (
                    artifact.name.clone(),
                    ConnectorArtifactPin {
                        version: artifact.version.clone(),
                        binary_sha256: artifact.binary_sha256.clone(),
                    },
                )
            })
            .collect(),
    )
}

/// 按 lock 表解析 CLI 二进制的「当前应然路径」：
/// `~/.pinvou3/assets/cli/<name>/<version>/<exe>`。版本化布局路径的**唯一解析
/// 入口**——spawn、存量校验、PATH 注入全部从这里取，不散落拼路径。
/// 无 lock 条目（如走 npm 的 tmeet）或平台不支持 → None（调用方回退旧布局/shim）。
pub fn locked_cli_path(name: &str) -> Option<std::path::PathBuf> {
    let pin = artifact_pin(name)?;
    Some(super::paths::assets_cli_dir(name, &pin.version).join(executable_name(name)))
}

/// Search paths we can establish for a native shell execution. With the stock
/// ShellEnv hook, the installed launcher must win both when login PATH was
/// imported and when the hook failed and the child inherited the app PATH.
#[derive(Default)]
pub(crate) enum ConnectorShellPath {
    Inherited,
    #[cfg(unix)]
    Managed(ManagedConnectorPaths),
    #[default]
    ExplicitOnly,
}

#[cfg(unix)]
pub(crate) type ManagedConnectorPaths =
    std::collections::BTreeMap<&'static str, Vec<std::path::PathBuf>>;

pub(crate) fn registered_telemetry_cli(
    key: &str,
    program: &str,
    cwd: &std::path::Path,
    shell_path: &ConnectorShellPath,
    _command: &str,
) -> bool {
    use std::path::Path;

    let installed = match key {
        "eip" | "zhidao" => {
            let Some(name) = Path::new(program).file_name() else {
                return false;
            };
            let dir = super::paths::bundle_skills_dir().join(key).join("bin");
            let exact = dir.join(name);
            Some(if exact.is_file() {
                exact
            } else {
                dir.join(executable_name(&name.to_string_lossy()))
            })
        }
        "tmeet" => Some(tmeet_launcher_path(&super::os::connector_cli_command(
            "tmeet", "tmeet",
        ))),
        "feishu" | "wecom" | "dingtalk" => {
            let bin = match key {
                "feishu" => "lark-cli",
                "wecom" => "wecom-cli",
                _ => "dws",
            };
            locked_cli_path(bin)
                .filter(|path| path.is_file())
                .or_else(|| {
                    super::paths::managed_connector_bin_dir()
                        .map(|dir| dir.join(executable_name(bin)))
                })
        }
        _ => None,
    };
    let Some(installed) = installed else {
        return false;
    };
    if matches_installed_connector_launcher(program, cwd, &installed, None) {
        return true;
    }
    if matches!(shell_path, ConnectorShellPath::ExplicitOnly) {
        return false;
    }
    let inherited = std::env::var_os("PATH");
    if !matches_installed_connector_launcher(program, cwd, &installed, inherited.as_deref()) {
        return false;
    }
    match shell_path {
        ConnectorShellPath::Inherited => true,
        #[cfg(unix)]
        ConnectorShellPath::Managed(paths) => {
            // Keep the scope identical to the stock ShellEnv branch.
            if _command.contains(['\n', '\r', '|', '&', ';', '<', '>', '`'])
                || _command.contains("$(")
            {
                return false;
            }
            let Some(dirs) = paths.get(program) else {
                return false;
            };
            let Ok(path) = std::env::join_paths(dirs) else {
                return false;
            };
            matches_installed_connector_launcher(program, cwd, &installed, Some(&path))
        }
        ConnectorShellPath::ExplicitOnly => false,
    }
}

#[cfg(unix)]
pub(crate) fn managed_connector_shell_paths() -> ManagedConnectorPaths {
    use std::path::{Path, PathBuf};

    let mut paths = ManagedConnectorPaths::new();
    for binary in ["lark-cli", "wecom-cli", "dws"] {
        let versioned =
            locked_cli_path(binary).and_then(|file| file.parent().map(Path::to_path_buf));
        let legacy = super::paths::managed_connector_bin_dir();
        paths.insert(binary, versioned.into_iter().chain(legacy).collect());
    }
    for (key, aliases) in [
        ("eip", ["eip", "eip-cli", "eip-cli-aarch64"]),
        ("zhidao", ["zhidao", "zhidao-cli", "zhidao-cli-aarch64"]),
    ] {
        let dir = super::paths::bundle_skills_dir().join(key).join("bin");
        for binary in aliases {
            paths.insert(binary, vec![dir.clone()]);
        }
    }
    let tmeet = tmeet_launcher_path(&super::os::connector_cli_command("tmeet", "tmeet"));
    let tmeet_dir = tmeet
        .is_absolute()
        .then(|| tmeet.parent().map(Path::to_path_buf))
        .flatten()
        .or_else(|| {
            std::env::var_os("NPM_CONFIG_PREFIX")
                .or_else(|| std::env::var_os("npm_config_prefix"))
                .map(|prefix| PathBuf::from(prefix).join("bin"))
        })
        .or_else(|| {
            std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".npm-global/bin"))
        });
    paths.insert("tmeet", tmeet_dir.into_iter().collect());
    // A delimiter or a newline would not round-trip through the PATH line.
    paths.retain(|_, dirs| {
        !dirs.is_empty()
            && dirs.iter().all(|dir| {
                dir.is_absolute()
                    && dir
                        .to_str()
                        .is_some_and(|dir| !dir.contains([':', '\n', '\r', '\0']))
            })
    });
    paths
}

pub(crate) fn tmeet_launcher_path(command: &std::process::Command) -> std::path::PathBuf {
    // The bundled Node route for TMeet wraps the app-resolved CLI shim as its
    // first argument. On other routes the shim itself is the executable.
    command
        .get_args()
        .next()
        .unwrap_or_else(|| command.get_program())
        .into()
}

pub(crate) fn matches_installed_connector_launcher(
    program: &str,
    cwd: &std::path::Path,
    installed: &std::path::Path,
    path: Option<&std::ffi::OsStr>,
) -> bool {
    use std::path::Path;

    if !installed.is_absolute() || !installed.is_file() {
        return false;
    }
    let normalize = |value: &Path| super::os::platform_compat_path(&value.to_string_lossy());
    let installed = normalize(installed);
    let matches = |value: &Path| {
        let value = normalize(value);
        if cfg!(windows) {
            value
                .components()
                .map(|part| part.as_os_str().to_string_lossy().to_ascii_lowercase())
                .eq(installed
                    .components()
                    .map(|part| part.as_os_str().to_string_lossy().to_ascii_lowercase()))
        } else {
            value == installed
        }
    };
    let requested = Path::new(program);
    if requested.is_absolute() || program.contains(['/', '\\']) {
        // Explicit paths point at a single executable. Merely having the
        // official CLI basename somewhere outside the installed path is insufficient.
        return matches(&cwd.join(requested));
    }

    let suffixes: &[&str] = if cfg!(windows) && requested.extension().is_none() {
        &["", ".com", ".exe", ".bat", ".cmd", ".ps1"]
    } else {
        &[""]
    };
    let directory_match = |directory: &Path| {
        let mut found = suffixes
            .iter()
            .map(|suffix| directory.join(format!("{program}{suffix}")))
            .filter(|filename| filename.is_file());
        let first = found.next()?;
        // Multiple plausible launchers in one directory are ambiguous.
        Some(matches(&first) && found.all(|filename| matches(&filename)))
    };
    if directory_match(cwd) == Some(false) {
        return false;
    }
    path.into_iter()
        .flat_map(std::env::split_paths)
        .find_map(|directory| directory_match(&cwd.join(directory)))
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 路径解析单点：lock 表内的 CLI 解析到版本化资产目录；tmeet（npm 安装，
    /// 无 lock 条目）与未知名 → None（调用方回退旧布局/shim）。
    #[test]
    fn locked_cli_path_resolves_versioned_layout() {
        let Some(path) = locked_cli_path("lark-cli") else {
            return; // 当前平台无 lock（不支持的架构），无从断言
        };
        let pin = artifact_pin("lark-cli").unwrap();
        let expected = super::super::paths::assets_cli_dir("lark-cli", &pin.version)
            .join(executable_name("lark-cli"));
        assert_eq!(path, expected);
        assert!(
            path.to_string_lossy().contains("assets"),
            "应在版本化资产库下: {}",
            path.display()
        );
        assert!(locked_cli_path("tmeet").is_none(), "tmeet 无 lock 条目");
        assert!(locked_cli_path("no-such-cli").is_none());
        assert!(all_artifact_pins().len() >= 3, "lock 表应含全部制品");
    }
}
