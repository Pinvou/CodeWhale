//! 磁盘空间检查与统计工具。

use std::path::Path;

const GB: f64 = 1024.0 * 1024.0 * 1024.0;

pub struct SpaceCheck {
    pub ok: bool,
    pub free_bytes: u64,
    pub required_bytes: u64,
    pub reason: String,
}

pub fn disk_free_bytes(path: &Path) -> std::io::Result<u64> {
    std::fs::create_dir_all(path)?;
    let stat = nix::sys::statvfs::statvfs(path).map_err(std::io::Error::from)?;
    Ok(stat.blocks_available() as u64 * stat.fragment_size() as u64)
}

/// 下载/导入前的磁盘空间预检查：可用空间 >= 待下载文件大小 + 安全余量。
/// 余量的意义：Ollama `ollama create` 导入时会短暂同时持有暂存文件与新写入的
/// blob，仅比对"文件大小"本身不够，需要预留余量。
pub fn check_space(path: &Path, required_bytes: u64, min_free_margin_gb: f64) -> SpaceCheck {
    let free = disk_free_bytes(path).unwrap_or(0);
    let margin = (min_free_margin_gb * GB) as u64;
    let needed = required_bytes + margin;
    if free < needed {
        SpaceCheck {
            ok: false,
            free_bytes: free,
            required_bytes,
            reason: format!(
                "可用空间不足：需要 {}（含 {}GB 安全余量），当前可用 {}",
                format_bytes(needed as f64),
                format_general_float(min_free_margin_gb),
                format_bytes(free as f64),
            ),
        }
    } else {
        SpaceCheck {
            ok: true,
            free_bytes: free,
            required_bytes,
            reason: String::new(),
        }
    }
}

pub fn dir_size_bytes(path: &Path) -> u64 {
    if !path.exists() {
        return 0;
    }
    walk_dir_size(path)
}

fn walk_dir_size(path: &Path) -> u64 {
    let mut total = 0u64;
    let Ok(entries) = std::fs::read_dir(path) else {
        return 0;
    };
    for entry in entries.flatten() {
        let p = entry.path();
        if p.is_dir() {
            total += walk_dir_size(&p);
        } else if let Ok(meta) = entry.metadata() {
            total += meta.len();
        }
    }
    total
}

/// 对应 Python 的 `{:g}` 格式：整数值不带小数点（2.0 -> "2"），否则保留小数。
fn format_general_float(n: f64) -> String {
    if n.fract() == 0.0 {
        format!("{}", n as i64)
    } else {
        format!("{}", n)
    }
}

pub fn format_bytes(n: f64) -> String {
    let mut n = n;
    for unit in ["B", "KB", "MB", "GB", "TB"] {
        if n.abs() < 1024.0 {
            return if unit == "B" {
                format!("{}{}", n as i64, unit)
            } else {
                format!("{:.1}{}", n, unit)
            };
        }
        n /= 1024.0;
    }
    format!("{:.1}PB", n)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_bytes() {
        assert_eq!(format_bytes(500.0), "500B");
        assert_eq!(format_bytes(2048.0), "2.0KB");
        assert_eq!(format_bytes(1024.0 * 1024.0 * 3.0), "3.0MB");
    }
}
