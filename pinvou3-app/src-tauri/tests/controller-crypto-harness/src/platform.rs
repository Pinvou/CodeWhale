// Only the capability projection is a host seam. Ledger file operations and
// private anchored directories use the actual product OS implementation.
pub(crate) fn desktop_control_supported() -> bool {
    cfg!(target_os = "windows")
}

#[cfg(test)]
#[path = "../../../src/platform/filesystem.rs"]
pub(crate) mod filesystem;

pub(crate) use crate::features::remote_control::HarnessWorkspaceIdentity as WorkspaceIdentity;
pub(crate) use crate::features::remote_control::{test_workspace_identity, workspace_identity};
#[path = "../../../src/platform/encoding.rs"]
pub(crate) mod encoding;

// Host seam used only when actual Windows filesystem tests invoke PowerShell:
// convert canonical Windows prefixes to shell paths, not a production OS-policy test.
pub(crate) mod os {
    pub(crate) fn platform_compat_path(value: &str) -> std::path::PathBuf {
        if let Some(rest) = value.strip_prefix(r"\\?\UNC\") {
            return std::path::PathBuf::from(format!(r"\\{rest}"));
        }
        std::path::PathBuf::from(value.strip_prefix(r"\\?\").unwrap_or(value))
    }
}
