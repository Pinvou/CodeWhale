//! Dispatch URLs and filesystem targets through their Windows associations.

use std::ffi::OsStr;
use std::os::windows::ffi::OsStrExt;

#[cfg(not(test))]
use windows_sys::Win32::UI::Shell::ShellExecuteW;
use windows_sys::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;

#[cfg(test)]
use tests::shell_execute as ShellExecuteW;

pub fn open_target(target: impl AsRef<OsStr>, label: &str) -> Result<(), String> {
    let mut target: Vec<u16> = target.as_ref().encode_wide().collect();
    // Do not let a NUL truncate the destination, or an empty target open an
    // unrelated location. Errors must not include credential-bearing URLs.
    if target.is_empty() || target.contains(&0) {
        return Err(format!("系统打开失败({label}): invalid target"));
    }
    target.push(0);
    // A null operation preserves the registered default verb (which need not
    // be "open"). Parameters stay null: the entire URL/path is one target,
    // never a command line interpreted by a shell.
    // SAFETY: The owned NUL-terminated string lives through the call. The
    // other pointer arguments are null. The return value is an opaque
    // success/error code, not an owned process handle.
    let result = unsafe {
        ShellExecuteW(
            std::ptr::null_mut(),
            std::ptr::null(),
            target.as_ptr(),
            std::ptr::null(),
            std::ptr::null(),
            SW_SHOWNORMAL,
        ) as isize
    };
    if result <= 32 {
        return Err(format!("系统打开失败({label}): ShellExecuteW ({result})"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use windows_sys::Win32::Foundation::{HINSTANCE, HWND};
    use windows_sys::Win32::UI::WindowsAndMessaging::SHOW_WINDOW_CMD;
    use windows_sys::core::PCWSTR;

    #[derive(Debug)]
    struct Call {
        owner_is_null: bool,
        operation: Option<Vec<u16>>,
        target: Option<Vec<u16>>,
        parameters: Option<Vec<u16>>,
        directory: Option<Vec<u16>>,
        show: SHOW_WINDOW_CMD,
    }

    #[derive(Default)]
    struct MockState {
        active: bool,
        result: isize,
        calls: Vec<Call>,
    }

    thread_local! {
        static MOCK: RefCell<MockState> = RefCell::new(MockState::default());
    }

    struct MockGuard(MockState);

    impl MockGuard {
        fn new(result: isize) -> Self {
            Self(MOCK.with(|state| {
                state.replace(MockState {
                    active: true,
                    result,
                    calls: Vec::new(),
                })
            }))
        }
    }

    impl Drop for MockGuard {
        fn drop(&mut self) {
            MOCK.with(|state| state.replace(std::mem::take(&mut self.0)));
        }
    }

    unsafe fn wide_value(value: PCWSTR) -> Option<Vec<u16>> {
        if value.is_null() {
            return None;
        }
        let mut units = Vec::new();
        loop {
            // SAFETY: The boundary receives the live NUL-terminated buffer
            // constructed by open_target (or a static operation literal).
            let unit = unsafe { *value.add(units.len()) };
            if unit == 0 {
                return Some(units);
            }
            units.push(unit);
        }
    }

    pub(super) unsafe fn shell_execute(
        owner: HWND,
        operation: PCWSTR,
        target: PCWSTR,
        parameters: PCWSTR,
        directory: PCWSTR,
        show: SHOW_WINDOW_CMD,
    ) -> HINSTANCE {
        let mocked = MOCK.with(|state| {
            let mut state = state.borrow_mut();
            if !state.active {
                return None;
            }
            // SAFETY: These are exactly the pointer arguments supplied by
            // open_target; their buffers remain live for this invocation.
            let call = unsafe {
                Call {
                    owner_is_null: owner.is_null(),
                    operation: wide_value(operation),
                    target: wide_value(target),
                    parameters: wide_value(parameters),
                    directory: wide_value(directory),
                    show,
                }
            };
            state.calls.push(call);
            Some(state.result as HINSTANCE)
        });
        if let Some(result) = mocked {
            return result;
        }
        // Other application tests retain the real OS behavior. Only a guard
        // on this test thread replaces the external native boundary.
        // SAFETY: All arguments are forwarded unchanged from open_target.
        unsafe {
            windows_sys::Win32::UI::Shell::ShellExecuteW(
                owner, operation, target, parameters, directory, show,
            )
        }
    }

    fn invoke(target: &OsStr, result: isize) -> (Result<(), String>, Vec<Call>) {
        let guard = MockGuard::new(result);
        let outcome = open_target(target, "test-label");
        let calls = MOCK.with(|state| std::mem::take(&mut state.borrow_mut().calls));
        drop(guard);
        (outcome, calls)
    }

    #[test]
    fn public_opener_preserves_default_association_and_entire_target() {
        for target in [
            "https://example.invalid/portal-session-handoff?locale=zh-Hans&next=a,b#ticket=synthetic-only",
            "https://example.invalid/?value=one&other=two|three^four%20five",
            r"C:\Some Folder\报告, [1].pdf",
            r"C:\Some Folder\资料",
        ] {
            let (outcome, calls) = invoke(OsStr::new(target), 33);
            outcome.unwrap();
            assert_eq!(
                calls.len(),
                1,
                "public entry must dispatch to the native API"
            );
            let call = &calls[0];
            assert!(call.owner_is_null);
            assert!(call.operation.is_none(), "use the system default verb");
            assert_eq!(call.target, Some(target.encode_utf16().collect()));
            assert!(call.parameters.is_none());
            assert!(call.directory.is_none());
            assert_eq!(call.show, SW_SHOWNORMAL);
        }
    }

    #[test]
    fn public_opener_reports_native_errors_without_disclosing_target() {
        let target = OsStr::new("https://example.invalid/#ticket=synthetic-private-value");
        for code in [0, 2, 5, 26, 27, 28, 29, 30, 31, 32] {
            let (outcome, calls) = invoke(target, code);
            let error = outcome.unwrap_err();
            assert_eq!(calls.len(), 1);
            assert!(error.contains("test-label"));
            assert!(error.contains(&format!("({code})")));
            assert!(!error.contains("example.invalid"));
            assert!(!error.contains("synthetic-private-value"));
        }
        assert!(invoke(target, 33).0.is_ok());
    }

    #[test]
    fn public_opener_rejects_empty_or_nul_targets_without_native_dispatch() {
        for target in ["", "https://example.invalid/\0another-target"] {
            let (outcome, calls) = invoke(OsStr::new(target), 33);
            let error = outcome.unwrap_err();
            assert!(calls.is_empty());
            assert!(error.contains("invalid target"));
            assert!(!error.contains("example.invalid"));
        }
    }
}
