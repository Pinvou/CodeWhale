//! A replaced weight path must not turn a checksum into an uninterruptible open.
use super::*;

fn fifo(path: &Path) {
    let path = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
    // SAFETY: the owned temporary path is NUL terminated and not shared.
    assert_eq!(unsafe { nix::libc::mkfifo(path.as_ptr(), 0o600) }, 0);
}

#[test]
fn weight_digest_rejects_open_races_without_blocking() {
    const CHILD: &str = "PINVOU_TEST_WEIGHT_DIGEST_OPEN_CHILD";
    if std::env::var_os(CHILD).as_deref() == Some(std::ffi::OsStr::new("1")) {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("weight");
        fifo(&path);
        assert!(file_digest(&path, &Default::default(), &mut |_| {}).is_err());
        let cancelled = script_runner::RunControl {
            cancel: Some(std::sync::Arc::new(std::sync::atomic::AtomicBool::new(
                true,
            ))),
            prompt: None,
        };
        assert!(
            file_digest(&path, &cancelled, &mut |_| {})
                .unwrap_err()
                .to_string()
                .contains("已取消")
        );
        assert!(file_digest(temp.path(), &Default::default(), &mut |_| {}).is_err());
        fs::remove_file(&path).unwrap();
        for replacement in ["fifo", "same-inode-link", "different-file"] {
            let kept = temp.path().join(format!("kept-{replacement}"));
            fs::write(&path, b"original bytes").unwrap();
            let expected = path.symlink_metadata().unwrap();
            fs::rename(&path, &kept).unwrap();
            match replacement {
                "fifo" => fifo(&path),
                "same-inode-link" => std::os::unix::fs::symlink(&kept, &path).unwrap(),
                _ => fs::write(&path, b"original bytes").unwrap(),
            }
            assert!(
                open_digest_file(&path, &expected).is_err(),
                "the real production opener accepted {replacement}"
            );
            assert_eq!(fs::read(&kept).unwrap(), b"original bytes");
            fs::remove_file(&path).unwrap();
        }
        return;
    }

    // Removing O_NONBLOCK must fail within a bound rather than hanging the
    // entire suite. The child invokes the same production open helper.
    let qualified = concat!(
        module_path!(),
        "::weight_digest_rejects_open_races_without_blocking"
    );
    let exact = qualified.split_once("::").unwrap().1;
    let mut child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["--exact", exact, "--nocapture", "--test-threads=1"])
        .env(CHILD, "1")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if child.try_wait().unwrap().is_some() {
            break;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let output = child.wait_with_output().unwrap();
            panic!(
                "weight opener blocked: {}",
                String::from_utf8_lossy(&output.stderr)
            );
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    let output = child.wait_with_output().unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed"));
}

#[test]
fn weight_digest_rejects_a_path_replaced_during_progress_and_preserves_both_files() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("weight");
    let kept = temp.path().join("kept");
    fs::write(&path, b"original bytes").unwrap();
    let mut replaced = false;
    let result = file_digest(&path, &Default::default(), &mut |line| {
        if !replaced && line.starts_with("LMM_PROGRESS_BYTES=0/") {
            fs::rename(&path, &kept).unwrap();
            fs::write(&path, b"new named bytes").unwrap();
            replaced = true;
        }
    });
    assert!(replaced);
    assert!(result.unwrap_err().to_string().contains("权重路径发生变化"));
    assert_eq!(fs::read(&kept).unwrap(), b"original bytes");
    assert_eq!(fs::read(&path).unwrap(), b"new named bytes");
}
