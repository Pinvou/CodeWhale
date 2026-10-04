use std::ffi::OsStr;
use std::io;
use std::os::windows::ffi::OsStrExt;
use windows_sys::Win32::System::Com::{
    COINIT_APARTMENTTHREADED, COINIT_DISABLE_OLE1DDE, CoInitializeEx, CoUninitialize,
};
use windows_sys::Win32::UI::Shell::{
    SEE_MASK_FLAG_NO_UI, SEE_MASK_NOASYNC, SHELLEXECUTEINFOW, ShellExecuteExW,
};
use windows_sys::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;

fn target_wide(target: &OsStr) -> io::Result<Vec<u16>> {
    let mut wide: Vec<u16> = target.encode_wide().collect();
    if wide.is_empty() || wide.contains(&0) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid open target",
        ));
    }
    wide.push(0);
    Ok(wide)
}

fn shell_request(target: &[u16]) -> SHELLEXECUTEINFOW {
    SHELLEXECUTEINFOW {
        cbSize: std::mem::size_of::<SHELLEXECUTEINFOW>() as u32,
        fMask: SEE_MASK_NOASYNC | SEE_MASK_FLAG_NO_UI,
        lpVerb: windows_sys::w!("open"),
        // One complete URL, file or directory name; never interpreted by cmd.exe.
        lpFile: target.as_ptr(),
        nShow: SW_SHOWNORMAL,
        ..Default::default()
    }
}

struct ComApartment;

impl Drop for ComApartment {
    fn drop(&mut self) {
        // SAFETY: created only after successful initialization, on this same thread.
        unsafe { CoUninitialize() };
    }
}

pub(super) fn open_target(target: &OsStr) -> io::Result<()> {
    let wide = target_wide(target)?;
    // Shell extensions can require STA. Do not change the caller's COM apartment.
    std::thread::Builder::new()
        .name("pinvou-system-open".into())
        .spawn(move || {
            // SAFETY: a fresh thread has no COM apartment; no reserved pointer is supplied.
            let result = unsafe {
                CoInitializeEx(
                    std::ptr::null(),
                    (COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE) as u32,
                )
            };
            if result < 0 {
                return Err(io::Error::other(format!(
                    "COM initialization failed: {result:#x}"
                )));
            }
            let _apartment = ComApartment;
            let mut request = shell_request(&wide);
            // SAFETY: all pointers reference live NUL-terminated buffers until the
            // synchronous call returns. No process handle is requested or owned.
            if unsafe { ShellExecuteExW(&mut request) } == 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        })?
        .join()
        .map_err(|_| io::Error::other("system open worker failed"))?
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preserves_complete_oauth_url_as_one_shell_target() {
        let url = "https://example.invalid/authorize?client_id=desktop&state=s&redirect_uri=http%3A%2F%2F127.0.0.1%3A8080%2Fcallback&code_challenge=c";
        let wide = target_wide(OsStr::new(url)).unwrap();
        let request = shell_request(&wide);
        // SAFETY: lpFile points at the live wide buffer created immediately above.
        let actual = unsafe { std::slice::from_raw_parts(request.lpFile, wide.len()) };
        assert_eq!(
            String::from_utf16(&actual[..actual.len() - 1]).unwrap(),
            url
        );
        assert!(request.lpParameters.is_null());
        assert!(request.lpDirectory.is_null());
        assert_eq!(request.fMask, SEE_MASK_NOASYNC | SEE_MASK_FLAG_NO_UI);
    }

    #[test]
    fn preserves_unicode_file_and_directory_names_with_shell_metacharacters() {
        for target in [r"C:\任务 & 资料\测试^(一).txt", r"C:\任务 & 资料"] {
            let wide = target_wide(OsStr::new(target)).unwrap();
            assert_eq!(String::from_utf16(&wide[..wide.len() - 1]).unwrap(), target);
        }
    }

    #[test]
    fn rejects_empty_or_embedded_nul_before_system_launch() {
        for target in ["", "https://example.invalid/\0&other"] {
            assert_eq!(
                open_target(OsStr::new(target)).unwrap_err().kind(),
                io::ErrorKind::InvalidInput
            );
        }
    }

    #[test]
    fn actual_windows_shell_returns_error_for_missing_local_file_without_error_ui() {
        // A missing file under a unique task-owned name cannot launch a handler.
        let path = std::env::temp_dir()
            .join(format!("pinvou-open-missing-{}", uuid::Uuid::new_v4()))
            .join("missing.txt");
        let error = open_target(path.as_os_str()).unwrap_err();
        assert!(error.raw_os_error().is_some());
    }
}
