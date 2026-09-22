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

/// 递归累加目录体积，**不跟符号链接**。
///
/// # 为什么必须不跟
///
/// 这里原来用 `p.is_dir()` 判断要不要往下钻，而 `is_dir()` 是跟软链的——于是
/// 两件事都会出：
///
/// 1. **环**。模型目录里出现一条指回上层的目录软链（`weights/self -> weights`，
///    或者卡片脚本自己建的 `current -> <某个版本目录>`），遍历就绕不出来。
///    而这个函数跑在**卸载的前置测量**上（lifecycle.rs 在执行卸载脚本之前要把
///    整棵树量两遍），用户感知是"点了卸载没反应"，最后栈溢出崩掉整个应用。
/// 2. **虚增**。软链指向外部挂载点（`/mnt/nas/models/...` 这种）时，那边几十 GB
///    会被算进 `freed_bytes`，而卸载根本不会删到它。lifecycle.rs 开头那条
///    "报多了是骗人"的约束直接被破坏。
///
/// 做法跟同模块 `import_scan::walk` 对齐（那里已有 `symlink_loops_do_not_hang_the_scan`
/// 回归测试）：用 `entry.file_type()` 判断——它读的是 `lstat`，不跟软链；软链条目
/// 整条跳过，既不下钻也不计体积。
///
/// 刻意**不**另起一套 visited 集合：visited 要按 (dev, ino) 记，量一棵几十万文件的
/// 权重树等于在卸载前先攒一个几十 MB 的 HashSet，而"不跟软链"已经把环断干净了
/// ——硬链接做不出目录环，普通目录树本身也不成环。
fn walk_dir_size(path: &Path) -> u64 {
    let mut total = 0u64;
    let Ok(entries) = std::fs::read_dir(path) else {
        return 0;
    };
    for entry in entries.flatten() {
        // 拿不到 file_type 就跳过：宁可少算，也不要为了"算全"退回跟软链的 is_dir()。
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_symlink() {
            continue;
        }
        if file_type.is_dir() {
            total += walk_dir_size(&entry.path());
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

    /// 目录级软链成环时，测量必须照样返回——原来的 `is_dir()` 版本会在这里
    /// 无限递归到栈溢出，而这段代码跑在用户点"卸载"之后、脚本执行之前。
    #[test]
    fn symlink_loop_does_not_hang_the_measurement() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("model");
        std::fs::create_dir_all(root.join("sub")).unwrap();
        std::fs::write(root.join("sub").join("w.bin"), vec![0u8; 100]).unwrap();
        std::os::unix::fs::symlink(&root, root.join("self")).unwrap();
        assert_eq!(dir_size_bytes(&root), 100);
    }

    /// 指向目录外的软链不能算进体积：卸载删不掉它指的那份数据，
    /// 算进来就等于跟用户虚报释放了多少空间。
    #[test]
    fn symlinks_pointing_outside_are_not_counted() {
        let tmp = tempfile::tempdir().unwrap();
        let outside_dir = tmp.path().join("elsewhere");
        std::fs::create_dir_all(&outside_dir).unwrap();
        std::fs::write(outside_dir.join("big.bin"), vec![0u8; 4096]).unwrap();

        let root = tmp.path().join("model");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("w.bin"), vec![0u8; 10]).unwrap();
        // 一条指向外部文件的软链 + 一条指向外部目录的软链，两条都不该被计入。
        std::os::unix::fs::symlink(outside_dir.join("big.bin"), root.join("big-link.bin")).unwrap();
        std::os::unix::fs::symlink(&outside_dir, root.join("elsewhere-link")).unwrap();

        assert_eq!(dir_size_bytes(&root), 10);
    }
}
