use std::ffi::OsStr;
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;
use std::process::{Command, Output, Stdio};
use std::time::{Duration, Instant};

const MAX_CAPTURE_BYTES: u64 = 4 * 1024 * 1024;
const CAPTURE_TRUNCATED_MARKER: &[u8] = b"\n[pinvou: subprocess output truncated]\n";

pub(crate) struct HiddenCommand;

impl HiddenCommand {
    pub(crate) fn new<S: AsRef<OsStr>>(program: S) -> Command {
        let mut command = Command::new(program);
        hide_std_console(&mut command);
        command
    }
}

fn is_windows_command_script(executable: &Path) -> bool {
    executable
        .extension()
        .and_then(|value| value.to_str())
        .is_some_and(|extension| {
            extension.eq_ignore_ascii_case("cmd") || extension.eq_ignore_ascii_case("bat")
        })
}

fn external_command_for(executable: &Path, windows: bool) -> Command {
    let executable = crate::platform::os::external_application_path(executable);
    if windows && is_windows_command_script(&executable) {
        let mut command = HiddenCommand::new("cmd");
        command.args(["/D", "/S", "/C"]).arg(executable);
        command
    } else {
        HiddenCommand::new(executable)
    }
}

/// 构造隐藏窗口的外部 CLI 命令。Windows npm 生成的 `.cmd` / `.bat` shim
/// 必须经 `cmd /D /S /C`，否则探测、登录或启动 Agent 时会被当成原生可执行文件。
pub(crate) fn external_command(executable: &Path) -> Command {
    external_command_for(executable, crate::platform::capabilities::is_windows())
}

fn external_tokio_command_for(executable: &Path, windows: bool) -> tokio::process::Command {
    let executable = crate::platform::os::external_application_path(executable);
    if windows && is_windows_command_script(&executable) {
        let mut command = HiddenTokioCommand::new("cmd");
        command.args(["/D", "/S", "/C"]).arg(executable);
        command
    } else {
        HiddenTokioCommand::new(executable)
    }
}

/// `external_command` 的 Tokio 版本。
pub(crate) fn external_tokio_command(executable: &Path) -> tokio::process::Command {
    external_tokio_command_for(executable, crate::platform::capabilities::is_windows())
}

/// Capture a subprocess without pipe deadlocks and enforce a wall-clock timeout.
///
/// Note: if captured stdout/stderr exceeds `MAX_CAPTURE_BYTES`, the whole process
/// tree is terminated regardless of the kill-tree flag — a runaway writer is never
/// allowed to outlive the caller.
pub(crate) fn output_with_timeout(
    mut command: Command,
    timeout: Duration,
) -> Result<Output, String> {
    std_process_group_leader(&mut command);
    output_with_timeout_inner(command, timeout, false)
}

/// Capture a subprocess with a wall-clock timeout and terminate its process tree
/// on timeout. Use this for helpers that can launch privileged descendants: killing
/// only the wrapper can otherwise leave the real operation running with inherited
/// stdout/stderr pipes after the caller has reported a timeout.
pub(crate) fn output_with_timeout_and_kill_tree(
    mut command: Command,
    timeout: Duration,
) -> Result<Output, String> {
    std_process_group_leader(&mut command);
    output_with_timeout_inner(command, timeout, true)
}

fn output_with_timeout_inner(
    mut command: Command,
    timeout: Duration,
    kill_tree_on_timeout: bool,
) -> Result<Output, String> {
    let program = command.get_program().to_string_lossy().into_owned();
    // Regular files cannot fill a pipe and block the child. They also let us read a
    // completed parent's output without waiting for a descendant that inherited the
    // same handles to exit, so the wall-clock timeout remains effective.
    let mut stdout_file = tempfile::tempfile()
        .map_err(|error| format!("create {program} stdout capture failed: {error}"))?;
    let mut stderr_file = tempfile::tempfile()
        .map_err(|error| format!("create {program} stderr capture failed: {error}"))?;
    let stdout_target = stdout_file
        .try_clone()
        .map_err(|error| format!("clone {program} stdout capture failed: {error}"))?;
    let stderr_target = stderr_file
        .try_clone()
        .map_err(|error| format!("clone {program} stderr capture failed: {error}"))?;
    command
        .stdin(Stdio::null())
        .stdout(Stdio::from(stdout_target))
        .stderr(Stdio::from(stderr_target));
    let mut child = command
        .spawn()
        .map_err(|error| format!("spawn {program} failed: {error}"))?;

    let started = Instant::now();
    let status = loop {
        if capture_limit_exceeded(&stdout_file) || capture_limit_exceeded(&stderr_file) {
            let _ = kill_process_tree(child.id());
            let _ = child.kill();
            let _ = child.wait();
            let stdout = read_capture(&mut stdout_file);
            let stderr = read_capture(&mut stderr_file);
            let detail = subprocess_output_detail(&stdout, &stderr);
            return Err(format!(
                "{program} output exceeded {MAX_CAPTURE_BYTES} bytes; subprocess tree termination requested: {detail}"
            ));
        }
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) if started.elapsed() <= timeout => {
                std::thread::sleep(Duration::from_millis(50));
            }
            Ok(None) => {
                if kill_tree_on_timeout {
                    let _ = kill_process_tree(child.id());
                }
                let _ = child.kill();
                let _ = child.wait();
                let stdout = read_capture(&mut stdout_file);
                let stderr = read_capture(&mut stderr_file);
                let detail = subprocess_output_detail(&stdout, &stderr);
                return Err(format!(
                    "{program} timed out after {}s: {detail}",
                    timeout.as_secs()
                ));
            }
            Err(error) => {
                if kill_tree_on_timeout {
                    let _ = kill_process_tree(child.id());
                }
                let _ = child.kill();
                let _ = child.wait();
                return Err(format!("{program} wait error: {error}"));
            }
        }
    };

    // A fast writer can cross the limit and exit between the last size check
    // and `try_wait()`. Re-check after completion so overflow is never reported
    // as a silently truncated success.
    if capture_limit_exceeded(&stdout_file) || capture_limit_exceeded(&stderr_file) {
        let _ = kill_process_tree(child.id());
        let stdout = read_capture(&mut stdout_file);
        let stderr = read_capture(&mut stderr_file);
        let detail = subprocess_output_detail(&stdout, &stderr);
        return Err(format!(
            "{program} output exceeded {MAX_CAPTURE_BYTES} bytes; subprocess tree termination requested: {detail}"
        ));
    }

    Ok(Output {
        status,
        stdout: read_capture(&mut stdout_file),
        stderr: read_capture(&mut stderr_file),
    })
}

fn capture_limit_exceeded(file: &std::fs::File) -> bool {
    file.metadata()
        .is_ok_and(|metadata| metadata.len() > MAX_CAPTURE_BYTES)
}

fn read_capture(file: &mut std::fs::File) -> Vec<u8> {
    let mut buffer = Vec::new();
    if file.seek(SeekFrom::Start(0)).is_ok() {
        let _ = file.take(MAX_CAPTURE_BYTES + 1).read_to_end(&mut buffer);
    }
    if buffer.len() as u64 > MAX_CAPTURE_BYTES {
        buffer.truncate(MAX_CAPTURE_BYTES as usize);
        buffer.extend_from_slice(CAPTURE_TRUNCATED_MARKER);
    }
    buffer
}

fn subprocess_output_detail(stdout: &[u8], stderr: &[u8]) -> String {
    let stdout = String::from_utf8_lossy(stdout);
    let stderr = String::from_utf8_lossy(stderr);
    let stdout = stdout.trim();
    let stderr = stderr.trim();
    let detail = match (stdout.is_empty(), stderr.is_empty()) {
        (true, true) => "no subprocess output".to_string(),
        (true, false) => stderr.to_string(),
        (false, true) => stdout.to_string(),
        (false, false) => format!("stderr:\n{stderr}\nstdout:\n{stdout}"),
    };
    detail.chars().take(4000).collect()
}

/// 构造执行远程安装脚本的异步子进程命令：Windows 用 PowerShell
/// `irm <url> | iex`，其他平台用 `sh -c "curl -fsSL <url> | bash"`。
pub(crate) fn install_script_command(unix_url: &str, windows_url: &str) -> tokio::process::Command {
    if crate::platform::capabilities::is_windows() {
        let mut command = HiddenTokioCommand::new("powershell");
        command
            .args(["-NoProfile", "-NonInteractive", "-Command"])
            // 先下载校验内容再执行：claude.ai 等官方站点会对非浏览器客户端
            // 间歇返回 Cloudflare 验证页（HTML/JS），直接 iex 会变成莫名其妙的
            // 解析错误且 stderr 为空；校验到 HTML 就给出可操作的中文错误。
            // 注意不能匹配任意 `<` 开头：kimi 官方脚本第一行是 `<#`（PowerShell
            // 块注释），`^\s*<` 会把它误判成验证页导致 kimi 永远装不上（实测）。
            // 只匹配真实 HTML 文档特征（Cloudflare 页以 <!DOCTYPE html> 开头）。
            .arg(format!(
                "$s = irm {windows_url}; if ($s -match '^\\s*<(html|!doctype|head|body|script)') {{ throw '官方站点返回了验证页而非安装脚本（可能是网络拦截或频控），请稍后重试' }}; iex $s"
            ));
        // Windows 开发机常见 PATH 顺序：Git for Windows 的 usr/bin 排在 System32
        // 前面，官方安装脚本调 tar 会命中 MSYS tar——盘符路径（C:\...）被当成
        // 「远程主机:路径」语法而失败（实测报错 Cannot execute remote shell）。
        // 把 System32 提到 PATH 最前，保证脚本拿到 Windows 原生工具。
        let system32 = std::env::var_os("SystemRoot")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| std::path::PathBuf::from(r"C:\Windows"))
            .join("System32");
        let mut sanitized_path = system32.into_os_string();
        if let Some(existing) = std::env::var_os("PATH") {
            sanitized_path.push(";");
            sanitized_path.push(existing);
        }
        command.env("PATH", sanitized_path);
        command
    } else {
        let mut command = HiddenTokioCommand::new("sh");
        command
            .arg("-c")
            .arg(format!("curl -fsSL {unix_url} | bash"));
        // Unix 上把安装进程放进**独立进程组**（组长 pid = 子进程 pid）：取消时
        // 按组杀（kill -9 -pgid）才能真正终止 curl | bash 派生的子进程，否则
        // 子 shell 孤儿化继续安装（评审中危项）。tokio 的 process_group 是
        // inherent 方法，无需 import。
        #[cfg(unix)]
        {
            command.process_group(0);
        }
        command
    }
}

/// Unix 上把（tokio）命令设为独立进程组组长（组长 pid = 子进程 pid）：
/// 取消安装时按组杀（kill -9 -pgid）能连 curl | bash / npm 派生的子进程
/// 一起终止，不孤儿化（评审中危项）。Windows no-op（taskkill /T 已杀整树）。
pub(crate) fn tokio_process_group_leader(command: &mut tokio::process::Command) {
    #[cfg(unix)]
    {
        // tokio 的 process_group 是 inherent 方法，无需 import。
        command.process_group(0);
    }
    #[cfg(not(unix))]
    {
        let _ = command;
    }
}

/// `tokio_process_group_leader` 的 std 版本（spawn_blocking 场景，如 Homebrew）。
pub(crate) fn std_process_group_leader(command: &mut Command) {
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt as _;
        command.process_group(0);
    }
    #[cfg(not(unix))]
    {
        let _ = command;
    }
}

/// 按 pid 杀进程树：Windows 用 taskkill 杀整棵树（脚本会再起子 shell，单杀
/// 父进程会留下继续运行的子进程）；其他平台按进程组杀（负 pid）——安装进程
/// 以 `process_group(0)` 独立成组，组杀连 curl | bash / npm 派生的子进程一起
/// 终止，不孤儿化（评审中危项）。Unix 直接调用 kill(2)，不得 spawn 外部 kill
/// 可执行文件；pid <= 1 或超出 i32 正数范围时拒绝执行，避免触发 kill(0/-1)
/// 的进程组或用户级特殊语义。ESRCH 表示目标已退出，按成功处理。
pub(crate) fn kill_process_tree(pid: u32) -> std::io::Result<()> {
    if crate::platform::capabilities::is_windows() {
        external_command(Path::new("taskkill"))
            .args(["/PID", &pid.to_string(), "/T", "/F"])
            .output()?;
        return Ok(());
    }
    #[cfg(unix)]
    {
        let Some(group) = i32::try_from(pid).ok().filter(|group| *group > 1) else {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                format!("refusing user-wide process group kill for pid {pid}"),
            ));
        };
        // SAFETY: libc::kill is a direct kill(2) wrapper; no memory is touched.
        let sent = unsafe { libc::kill(-group, libc::SIGKILL) };
        if sent != 0 {
            let error = std::io::Error::last_os_error();
            if error.raw_os_error() == Some(libc::ESRCH) {
                return Ok(());
            }
            return Err(error);
        }
    }
    #[cfg(not(unix))]
    {
        let _ = pid;
    }
    Ok(())
}

pub(crate) struct HiddenTokioCommand;

impl HiddenTokioCommand {
    #[allow(clippy::new_ret_no_self)]
    pub(crate) fn new<S: AsRef<OsStr>>(program: S) -> tokio::process::Command {
        let mut command = tokio::process::Command::new(program);
        hide_tokio_console(&mut command);
        command
    }
}

#[cfg(target_os = "windows")]
pub(crate) fn hide_std_console(command: &mut Command) {
    use std::os::windows::process::CommandExt;

    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(target_os = "windows"))]
pub(crate) fn hide_std_console(_command: &mut Command) {}

#[cfg(target_os = "windows")]
pub(crate) fn hide_tokio_console(command: &mut tokio::process::Command) {
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(target_os = "windows"))]
pub(crate) fn hide_tokio_console(_command: &mut tokio::process::Command) {}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write as _;

    const OUTPUT_FIXTURE_ENV: &str = "PINVOU_PROCESS_OUTPUT_FIXTURE";
    const OUTPUT_FIXTURE_MARKER_ENV: &str = "PINVOU_PROCESS_OUTPUT_FIXTURE_MARKER";
    const OUTPUT_FIXTURE_TEST: &str = "platform::process::tests::command_output_fixture";

    fn output_fixture_command(mode: &str) -> Command {
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args(["--exact", OUTPUT_FIXTURE_TEST, "--nocapture"])
            .env(OUTPUT_FIXTURE_ENV, mode);
        command
    }

    fn fixture_marker(extension: &str) -> Option<std::path::PathBuf> {
        std::env::var_os(OUTPUT_FIXTURE_MARKER_ENV)
            .map(std::path::PathBuf::from)
            .map(|path| path.with_extension(extension))
    }

    fn wait_for_fixture_marker(extension: &str, timeout: Duration) -> bool {
        let Some(marker) = fixture_marker(extension) else {
            return false;
        };
        let started = Instant::now();
        while !marker.exists() && started.elapsed() < timeout {
            std::thread::sleep(Duration::from_millis(25));
        }
        marker.exists()
    }

    #[test]
    fn command_output_fixture() {
        match std::env::var(OUTPUT_FIXTURE_ENV).as_deref() {
            Ok("flood") => {
                let payload = vec![b'x'; 256 * 1024];
                std::io::stdout().write_all(&payload).unwrap();
                std::io::stderr().write_all(&payload).unwrap();
            }
            Ok("overflow") => {
                let payload = vec![b'x'; 64 * 1024];
                loop {
                    std::io::stdout().write_all(&payload).unwrap();
                    std::io::stderr().write_all(&payload).unwrap();
                }
            }
            Ok("overflow_exit") => {
                let payload = vec![b'x'; MAX_CAPTURE_BYTES as usize + 64 * 1024];
                std::io::stdout().write_all(&payload).unwrap();
                std::io::stderr().write_all(&payload).unwrap();
            }
            Ok("descendant") => {
                let _child = output_fixture_command("descendant_success")
                    .stdout(Stdio::inherit())
                    .stderr(Stdio::inherit())
                    .spawn()
                    .unwrap();
                assert!(wait_for_fixture_marker("running", Duration::from_secs(3)));
                println!("descendant-started");
            }
            Ok("descendant_wait") => {
                let mut child = output_fixture_command("descendant_timeout")
                    .stdout(Stdio::inherit())
                    .stderr(Stdio::inherit())
                    .spawn()
                    .unwrap();
                assert!(wait_for_fixture_marker("running", Duration::from_secs(3)));
                child.wait().unwrap();
            }
            Ok("descendant_success") | Ok("descendant_timeout") => {
                if let Some(running) = fixture_marker("running") {
                    std::fs::write(running, b"running").unwrap();
                }
                std::thread::sleep(Duration::from_secs(5));
                if let Some(finished) = fixture_marker("finished") {
                    std::fs::write(finished, b"finished").unwrap();
                }
            }
            _ => {}
        }
    }

    #[test]
    fn windows_command_shims_use_command_interpreter() {
        let command = external_command_for(Path::new(r"C:\Users\u\npm\kimi.cmd"), true);
        assert_eq!(command.get_program(), "cmd");
        assert_eq!(
            command
                .get_args()
                .map(|value| value.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["/D", "/S", "/C", r"C:\Users\u\npm\kimi.cmd"]
        );

        let command = external_tokio_command_for(Path::new(r"C:\Users\u\npm\claude.cmd"), true);
        assert_eq!(command.as_std().get_program(), "cmd");
        assert_eq!(
            command
                .as_std()
                .get_args()
                .map(|value| value.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["/D", "/S", "/C", r"C:\Users\u\npm\claude.cmd"]
        );
    }

    #[test]
    fn native_executables_do_not_use_command_interpreter() {
        let command = external_command_for(Path::new(r"C:\tools\kimi.exe"), true);
        assert_eq!(command.get_program(), r"C:\tools\kimi.exe");
        assert!(command.get_args().next().is_none());
    }

    #[test]
    fn timeout_kills_self_contained_process_tree() {
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("descendant-marker");
        let running = marker.with_extension("running");
        let finished = marker.with_extension("finished");
        let mut command = output_fixture_command("descendant_wait");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let started = Instant::now();
        let error = output_with_timeout_and_kill_tree(command, Duration::from_secs(3)).unwrap_err();

        assert!(error.contains("timed out after"));
        assert!(started.elapsed() < Duration::from_secs(5));
        assert!(running.exists(), "descendant did not start before timeout");
        std::thread::sleep(Duration::from_secs(6));
        assert!(
            !finished.exists(),
            "descendant survived process-tree timeout"
        );
    }

    /// Regression test for Unix group termination: the implementation must
    /// call kill(2) directly even when a fake `kill` executable is first on
    /// PATH, and it must terminate both the group leader and its descendant.
    #[cfg(unix)]
    #[test]
    fn kill_process_tree_terminates_group_without_spawning_external_kill() {
        use std::sync::Mutex;

        static PATH_LOCK: Mutex<()> = Mutex::new(());
        let _path_lock = PATH_LOCK.lock().unwrap_or_else(|p| p.into_inner());

        let work = std::env::temp_dir().join(format!(
            "pinvou3-kill-process-tree-{}-{}",
            std::process::id(),
            crate::platform::paths::tests::unique_suffix()
        ));
        std::fs::create_dir_all(&work).expect("create test work dir");
        let marker = work.join("external-kill-invoked");
        let sleep_pid_file = work.join("sleep-pid");
        let fake_kill_bin = work.join("kill");
        std::fs::write(
            &fake_kill_bin,
            format!("#!/bin/sh\necho \"$@\" >> {}\nexit 42\n", marker.display()),
        )
        .expect("write fake kill");
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&fake_kill_bin, std::fs::Permissions::from_mode(0o755))
            .expect("chmod fake kill");

        let previous_path = std::env::var_os("PATH");
        let mut split_path = previous_path
            .as_ref()
            .map(|value| std::env::split_paths(value).collect::<Vec<_>>())
            .unwrap_or_default();
        split_path.insert(0, work.clone());
        // SAFETY: this test serializes its PATH mutation on PATH_LOCK.
        unsafe { std::env::set_var("PATH", std::env::join_paths(&split_path).unwrap()) };

        let mut command = Command::new("sh");
        command.args([
            "-c",
            &format!("sleep 30 & echo $! > {}; wait", sleep_pid_file.display()),
        ]);
        std_process_group_leader(&mut command);
        let mut child = command.spawn().expect("spawn sh group leader");
        let sh_pid = child.id();

        let mut sleep_pid = None;
        for _ in 0..40 {
            if let Ok(text) = std::fs::read_to_string(&sleep_pid_file) {
                if let Ok(value) = text.trim().parse::<u32>() {
                    sleep_pid = Some(value);
                    break;
                }
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        let sleep_pid = sleep_pid.expect("descendant must report its pid");

        let kill_result = kill_process_tree(sh_pid);
        let _ = child.wait();

        // SAFETY: kill(pid, 0) only probes liveness.
        let alive = |pid: u32| unsafe { libc::kill(pid as i32, 0) == 0 };
        let mut both_dead = !alive(sh_pid) && !alive(sleep_pid);
        for _ in 0..100 {
            if both_dead {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
            both_dead = !alive(sh_pid) && !alive(sleep_pid);
        }
        // SAFETY: ensure no fixture process survives a failing assertion.
        let _ = unsafe { libc::kill(sleep_pid as i32, libc::SIGKILL) };

        // SAFETY: holding PATH_LOCK; restore the saved value.
        unsafe {
            match previous_path {
                Some(value) => std::env::set_var("PATH", value),
                None => std::env::remove_var("PATH"),
            }
        }
        let marker_content = std::fs::read_to_string(&marker).ok();

        assert!(kill_result.is_ok(), "group kill failed: {kill_result:?}");
        assert!(
            both_dead,
            "group leader {sh_pid} and descendant {sleep_pid} must both die"
        );
        assert!(
            marker_content.is_none(),
            "an external kill was spawned: {marker_content:?}"
        );

        let _ = std::fs::remove_dir_all(&work);
    }

    #[test]
    fn output_capture_does_not_deadlock_when_buffers_are_large() {
        let output = output_with_timeout_and_kill_tree(
            output_fixture_command("flood"),
            Duration::from_secs(5),
        )
        .unwrap();

        assert!(output.status.success());
        assert!(output.stdout.len() >= 256 * 1024);
        assert!(output.stderr.len() >= 256 * 1024);
    }

    #[test]
    fn output_capture_limit_terminates_process_tree() {
        let started = Instant::now();
        let error = output_with_timeout_and_kill_tree(
            output_fixture_command("overflow"),
            Duration::from_secs(5),
        )
        .unwrap_err();

        assert!(error.contains("output exceeded"), "{error}");
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn completed_process_cannot_bypass_output_capture_limit() {
        let error = output_with_timeout_and_kill_tree(
            output_fixture_command("overflow_exit"),
            Duration::from_secs(5),
        )
        .unwrap_err();

        assert!(error.contains("output exceeded"), "{error}");
    }

    #[test]
    fn completed_parent_does_not_wait_for_descendant_capture_handles() {
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("descendant-marker");
        let finished = marker.with_extension("finished");
        let mut command = output_fixture_command("descendant");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let started = Instant::now();
        let output = output_with_timeout_and_kill_tree(command, Duration::from_secs(5)).unwrap();

        assert!(output.status.success());
        assert!(String::from_utf8_lossy(&output.stdout).contains("descendant-started"));
        assert!(started.elapsed() < Duration::from_secs(3));
        let wait_started = Instant::now();
        while !finished.exists() && wait_started.elapsed() < Duration::from_secs(7) {
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(
            finished.exists(),
            "descendant fixture did not exit naturally"
        );
        std::thread::sleep(Duration::from_millis(100));
    }
}
