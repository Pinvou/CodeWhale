// Only the capability projection is a host seam. Ledger file operations and
// private anchored directories use the actual product OS implementation.
pub(crate) fn desktop_control_supported() -> bool {
    cfg!(target_os = "windows")
}

#[cfg(test)]
#[path = "../../../src/platform/filesystem.rs"]
pub(crate) mod filesystem;
