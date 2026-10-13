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
pub(crate) fn harden_dir_owner_only(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700));
}

/// 注册表文件只留 owner 权限（0600），rename 落盘前调用。
pub(crate) fn set_file_owner_only(file: &std::fs::File) {
    use std::os::unix::fs::PermissionsExt;
    let _ = file.set_permissions(std::fs::Permissions::from_mode(0o600));
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
}
