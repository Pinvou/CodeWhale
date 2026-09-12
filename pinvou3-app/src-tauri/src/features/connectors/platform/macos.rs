//! macOS 下的 H3C 内部 CLI 二进制定位。
//!
//! 二进制命名沿用 Linux 侧约定（x86_64 为 `<tool>-cli`、aarch64 加 `-aarch64`
//! 后缀），公共包装脚本 `bin/eip`、`bin/zhidao` 按 `uname -m` 选择同一套名字。
//! 与 linux.rs 同样的兜底：补可执行位 + 校验 Mach-O 架构，防止把别的平台的
//! 二进制（ELF/.exe）误打进 darwin bundle。

use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};

pub(crate) fn eip_bin_path() -> Result<PathBuf, String> {
    let bin_dir = crate::platform::paths::bundle_skills_dir()
        .join("eip")
        .join("bin");
    let name = if std::env::consts::ARCH == "aarch64" {
        "eip-cli-aarch64"
    } else {
        "eip-cli"
    };
    let path = bin_dir.join(name);
    if !path.is_file() {
        return Err(format!(
            "eip-cli 未找到: {}（需先把 EIP 技能二进制打包进 bundle）",
            path.display()
        ));
    }
    prepare_macos_cli(&path, "eip-cli")?;
    Ok(path)
}

pub(crate) fn zhidao_bin_path() -> Result<PathBuf, String> {
    let name = if std::env::consts::ARCH == "aarch64" {
        "zhidao-cli-aarch64"
    } else {
        "zhidao-cli"
    };
    let path = crate::platform::paths::bundle_skills_dir()
        .join("zhidao")
        .join("bin")
        .join(name);
    if !path.is_file() {
        return Err(format!(
            "zhidao CLI 未找到: {}（需先把知道技能二进制打包进 bundle）",
            path.display()
        ));
    }
    prepare_macos_cli(&path, "zhidao-cli")?;
    Ok(path)
}

fn prepare_macos_cli(path: &Path, label: &str) -> Result<(), String> {
    validate_macos_cli_arch(path, label)?;
    if let Ok(metadata) = std::fs::metadata(path) {
        let mode = metadata.permissions().mode();
        if mode & 0o111 == 0 {
            let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755));
        }
    }
    Ok(())
}

/// 只校验 64 位瘦 Mach-O 的 cputype；fat/universal 与非 Mach-O（脚本等）不拦截，
/// 与 linux.rs 对非 ELF 早返回的口径一致。
fn validate_macos_cli_arch(path: &Path, label: &str) -> Result<(), String> {
    let Ok(bytes) = std::fs::read(path) else {
        return Ok(());
    };
    if bytes.len() < 8 || bytes[0..4] != *b"\xCF\xFA\xED\xFE" {
        return Ok(());
    }
    let actual = match u32::from_le_bytes([bytes[4], bytes[5], bytes[6], bytes[7]]) {
        0x0100_000C => "aarch64",
        0x0100_0007 => "x86_64",
        _ => return Ok(()),
    };
    let expected = std::env::consts::ARCH;
    if actual == expected {
        return Ok(());
    }
    Err(format!(
        "{label} architecture mismatch: packaged binary is {actual}, but this macOS device is {expected}. Please package a matching {expected} binary at {}.",
        path.display()
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bin_paths_follow_arch_suffix_convention() {
        // 资源目录未必随测试释放（bundle_skills_dir 取决于 PINVOU3_HOME），这里
        // 只锁命名约定：aarch64 主机期望 -aarch64 后缀，x86_64 期望裸名。
        let expected = if std::env::consts::ARCH == "aarch64" {
            "eip-cli-aarch64"
        } else {
            "eip-cli"
        };
        let err = eip_bin_path().err().unwrap_or_default();
        if !err.is_empty() {
            assert!(
                err.contains(expected),
                "错误提示应包含本架构期望的二进制名 {expected}: {err}"
            );
        }
    }

    #[test]
    fn arch_validation_rejects_foreign_macho() {
        let tmp = std::env::temp_dir().join(format!("pinvou3-macho-{}", std::process::id()));
        std::fs::create_dir_all(&tmp).unwrap();
        let path = tmp.join("cli");
        // MH_MAGIC_64 + cputype 0x0100000C (arm64)
        let mut bytes = b"\xCF\xFA\xED\xFE".to_vec();
        bytes.extend_from_slice(&0x0100_000Cu32.to_le_bytes());
        std::fs::write(&path, &bytes).unwrap();
        if std::env::consts::ARCH == "x86_64" {
            assert!(validate_macos_cli_arch(&path, "eip-cli").is_err());
        } else {
            assert!(validate_macos_cli_arch(&path, "eip-cli").is_ok());
        }
        // 非 Mach-O 内容（脚本）不拦截
        std::fs::write(&path, b"#!/bin/sh\n").unwrap();
        assert!(validate_macos_cli_arch(&path, "eip-cli").is_ok());
        let _ = std::fs::remove_dir_all(&tmp);
    }
}
