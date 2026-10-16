//! unix 通用（Linux + macOS）：注册表权限收敛。
//!
//! 外部模型记录含 base_url/weights_path，目录/文件只留 owner 权限，同机其他
//! 用户不该读到。这套实现是纯 `std::os::unix`，macOS 本来就编得了——此前被
//! `cfg(not(target_os = "linux"))` 的空桩整体吞掉，等于在能加固的平台上静默
//! 放弃加固（第四轮评审 P2：`PINVOU3_MEGACUBE_DEVICE=1` 在 macOS 强开能力位
//! 后登记的注册表会按 umask 落成 0644）。现在按 `cfg(unix)` 复用同一份实现；
/// 非 unix 平台才走 unsupported 的显式桩。
use std::path::Path;

/// 注册表目录只留 owner 权限（0700）。
///
/// chmod 失败不阻断主流程（注册表落在共享挂载等怪文件系统上时写入仍受文件级
/// 0600 保护），但要留诊断痕迹——静默吞掉的话"目录为何组可读"就无从查起
/// （第七轮评审 P3）。
pub(crate) fn harden_dir_owner_only(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    if let Err(error) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)) {
        eprintln!(
            "[pinvou3-app] 注册表目录权限收敛失败（{}）：{error}",
            path.display()
        );
    }
}

/// 注册表记录文件只留 owner 权限（0600）。登记/移除走
/// `platform::atomic_write_private`（open 时即 0600）；这里服务读取路径的**重
/// 收敛**——doctor 安装模板教 AI 自写注册表文件，chmod 被跳过的存量文件在每次
/// 列表扫描时被尽力拉回 0600（第七轮评审 P3）。失败同样只留诊断痕迹。
pub(crate) fn harden_file_owner_only(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    if let Err(error) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
        eprintln!(
            "[pinvou3-app] 注册表文件权限收敛失败（{}）：{error}",
            path.display()
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn harden_strips_group_and_other_bits() {
        let dir = std::env::temp_dir().join(format!("pinvou-owner-only-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755)).unwrap();
        harden_dir_owner_only(&dir);
        let mode = std::fs::metadata(&dir).unwrap().permissions().mode() & 0o777;
        assert_eq!(
            mode & 0o077,
            0,
            "group/other bits must be stripped, got {mode:o}"
        );
        std::fs::remove_dir(&dir).ok();
    }

    #[test]
    fn harden_file_strips_group_and_other_bits() {
        // 读取路径对存量注册表文件的重收敛（第七轮评审 P3）：AI 自写文件跳过
        // chmod 时，扫描要能把它拉回 0600。
        let dir = std::env::temp_dir().join(format!("pinvou-owner-file-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        let file = dir.join("record.json");
        std::fs::write(&file, "{}").expect("write file");
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o644)).unwrap();
        harden_file_owner_only(&file);
        let mode = std::fs::metadata(&file).unwrap().permissions().mode() & 0o777;
        assert_eq!(
            mode & 0o077,
            0,
            "group/other bits must be stripped, got {mode:o}"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
