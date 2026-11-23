//! Dispatch URLs and filesystem targets through their Windows associations.

use std::ffi::OsStr;
use std::os::windows::ffi::OsStrExt;

use windows_sys::Win32::UI::Shell::ShellExecuteW;
use windows_sys::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;

pub fn open_target(target: impl AsRef<OsStr>, label: &str) -> Result<(), String> {
    open_target_with(target.as_ref(), label, |target| {
        // SAFETY: Both strings are NUL-terminated and live through the call.
        // No owner, parameters or working directory is passed. The return
        // value is an opaque success/error code, not an owned process handle.
        unsafe {
            ShellExecuteW(
                std::ptr::null_mut(),
                windows_sys::w!("open"),
                target.as_ptr(),
                std::ptr::null(),
                std::ptr::null(),
                SW_SHOWNORMAL,
            ) as isize
        }
    })
}

fn open_target_with(
    target: &OsStr,
    label: &str,
    execute: impl FnOnce(&[u16]) -> isize,
) -> Result<(), String> {
    let mut target: Vec<u16> = target.encode_wide().collect();
    // Do not let a NUL truncate the selected destination, or an empty target
    // open an unrelated default location. Never include the URL in errors:
    // account handoff URLs can carry one-time credentials in their fragment.
    if target.is_empty() || target.contains(&0) {
        return Err(format!("系统打开失败({label}): invalid target"));
    }
    target.push(0);
    // Explorer can start successfully yet open a folder instead of handling
    // the URL. ShellExecute uses the registered protocol/file association,
    // with no command shell to interpret query or fragment metacharacters.
    let result = execute(&target);
    if result <= 32 {
        return Err(format!("系统打开失败({label}): ShellExecuteW ({result})"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preserves_whole_url_and_unicode_filesystem_targets() {
        for target in [
            "https://example.invalid/portal-session-handoff?locale=zh-Hans&next=a,b#ticket=synthetic-only",
            "https://example.invalid/?value=one&other=two|three^four%20five",
            r"C:\Some Folder\报告, [1].pdf",
            r"C:\Some Folder\资料",
        ] {
            let mut called = false;
            open_target_with(OsStr::new(target), "test", |received| {
                called = true;
                assert_eq!(received.last(), Some(&0));
                assert_eq!(
                    String::from_utf16(&received[..received.len() - 1]).unwrap(),
                    target
                );
                33
            })
            .unwrap();
            assert!(called);
        }
    }

    #[test]
    fn reports_native_association_errors_without_disclosing_target() {
        let target = OsStr::new("https://example.invalid/#ticket=synthetic-private-value");
        for code in [0, 2, 5, 26, 27, 28, 29, 30, 31, 32] {
            let error = open_target_with(target, "test-label", |_| code).unwrap_err();
            assert!(error.contains("test-label"));
            assert!(error.contains(&format!("({code})")));
            assert!(!error.contains("example.invalid"));
            assert!(!error.contains("synthetic-private-value"));
        }
        assert!(open_target_with(target, "test", |_| 33).is_ok());
    }

    #[test]
    fn rejects_empty_or_nul_targets_before_native_dispatch() {
        for target in ["", "https://example.invalid/\0another-target"] {
            let error = open_target_with(OsStr::new(target), "test", |_| {
                panic!("invalid target must not reach the native opener")
            })
            .unwrap_err();
            assert!(error.contains("invalid target"));
            assert!(!error.contains("example.invalid"));
        }
    }
}
