use std::ffi::OsStr;
use std::io::Read as _;
use std::path::Path;
use std::process::{Child, Command, Output, Stdio};
use std::time::{Duration, Instant};

/// Hard ceiling for captured stdout/stderr per stream. A child (or a
/// descendant) writing past it gets its whole process tree terminated, so a
/// runaway writer can neither exhaust memory nor outlive the caller. This
/// is a memory bound, not a tunable: a consumer whose legitimate output can
/// outgrow it must not capture that output at all but redirect it to a file
/// it owns and read it from there (the checkpoint diff and change list go
/// through `git diff --output` for exactly this reason), because no bound
/// on bytes read back into memory can be made to cover an inventory whose
/// size the consumer's own admission rules do not bound.
const MAX_CAPTURE_BYTES: u64 = 4 * 1024 * 1024;
/// Appended by [`marked_capture_bytes`] when a stream was clipped to
/// [`MAX_CAPTURE_BYTES`], so a truncated capture is never mistaken for the
/// child's complete output. The success path carries it only for a clip;
/// a capture that could not be read back in full is an error there (see
/// [`deliver_captures`]) and degrades to the marker alone only inside
/// error-message detail.
const CAPTURE_TRUNCATED_MARKER: &[u8] = b"\n[pinvou: subprocess output truncated]\n";

/// Readback chunk for [`read_capture`]: large enough that any capture
/// past one block exercises the multi-chunk path the cursor pins rely on.
const CAPTURE_READ_CHUNK: usize = 64 * 1024;
/// The timeout anchor clause inside this module's fixed error prefix
/// (`{program} timed out after {n}s: {detail}`). The boundary consumers
/// that classify or cut these messages — connectors' `ProbeError::classify`,
/// the pkexec privileged-helper wrapper, and the 7z list precheck — must
/// match on this constant instead of a private literal, so a wording change
/// here fails their compile/refactor rather than silently unmatching at
/// runtime (the formats themselves are additionally pinned end-to-end by
/// the fixture tests on both sides).
pub(crate) const CAPTURE_TIMEOUT_ANCHOR: &str = " timed out after ";
/// The overflow anchor clause inside this module's fixed error prefix
/// (`{program} output exceeded {limit} bytes; ...`, where `limit` is the
/// call's capture bound). Same producer/consumer contract as
/// [`CAPTURE_TIMEOUT_ANCHOR`].
pub(crate) const CAPTURE_OVERFLOW_ANCHOR: &str = "output exceeded";
#[cfg(windows)]
const WINDOWS_TASKKILL_TIMEOUT: Duration = Duration::from_secs(2);

pub(crate) struct HiddenCommand;

impl HiddenCommand {
    pub(crate) fn new<S: AsRef<OsStr>>(program: S) -> Command {
        let mut command = Command::new(program);
        hide_std_console(&mut command);
        command
    }
}

/// Strips `GIT_*` override variables (redirection/config-injection classes)
/// injected by the host environment. `GIT_DIR`/`GIT_WORK_TREE`/
/// `GIT_INDEX_FILE`/`GIT_OBJECT_DIRECTORY` exported by the launching shell
/// redirect the module's internal git operations to unrelated repositories,
/// indexes, or object stores; `GIT_CONFIG*` injection overrides the target
/// repository's own configuration; `GIT_CEILING_DIRECTORIES` and friends alter
/// repository discovery — these variables really exist when the GUI is
/// launched from a development shell. Features that spawn git internally
/// (code_checkpoints shadow repositories, codex_acp workspace branch/diff
/// operations) must call this before spawning.
///
/// **Must remove keys one by one from a fixed key list; do not iterate
/// `env::vars_os()` first and delete matches**: iterating `environ` while other
/// threads run `setenv`/`remove_var` concurrently can miss keys (glibc environ
/// mutation is not thread-safe); parallel tests have occasionally lost
/// isolation entirely this way (the 2026-09-12 flaky family). Once
/// `GIT_CONFIG_COUNT` is removed, git ignores the `GIT_CONFIG_KEY_n`/
/// `GIT_CONFIG_VALUE_n` numbered pairs, so they need no enumeration.
///
/// Non-redirection variables such as `GIT_AUTHOR_*`/`GIT_COMMITTER_*`/
/// `GIT_SSH*` are kept: operations on the user's real worktree should follow
/// the behavior of the user's own git; code_checkpoints shadow repositories
/// need stronger isolation (identity and global config pinned too) and should
/// use [`strip_all_git_env`] instead.
pub(crate) fn strip_git_override_env(command: &mut Command) {
    for key in GIT_OVERRIDE_KEYS {
        command.env_remove(key);
    }
}

/// Shadow-repository hardened variant of [`strip_git_override_env`]: also
/// removes identity/date variables. The shadow repository's commit identity is
/// provided explicitly via `-c` by the caller; `GIT_AUTHOR_*` exported by the
/// host shell must not leak into snapshot commits.
pub(crate) fn strip_all_git_env(command: &mut Command) {
    for key in GIT_OVERRIDE_KEYS.iter().copied().chain(GIT_IDENTITY_KEYS) {
        command.env_remove(key);
    }
}

/// Once `GIT_CONFIG_COUNT` is removed, the `GIT_CONFIG_KEY_n`/
/// `GIT_CONFIG_VALUE_n` numbered pairs become ineffective, so the numbered
/// keys need no enumeration.
///
/// `pub` so the same-repo `pinvou-cli` workspace lanes can import it through
/// the `features::codex_acp` facade re-export instead of mirroring the list
/// (a CLI-side mirror had already drifted by three keys); the GUI's own
/// callers keep using it through the crate-private strip_git_override_env.
pub const GIT_OVERRIDE_KEYS: [&str; 25] = [
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    "GIT_COMMON_DIR",
    "GIT_GRAFT_FILE",
    "GIT_SHALLOW_FILE",
    "GIT_REPLACE_REF_BASE",
    "GIT_NAMESPACE",
    "GIT_CEILING_DIRECTORIES",
    "GIT_DISCOVERY_ACROSS_FILESYSTEM",
    // GIT_TEMPLATE_DIR would seed a fresh `git init` of the shadow repo with
    // hook-carrying templates (pre-auto-gc runs under `gc --auto`).
    "GIT_TEMPLATE_DIR",
    // The pathspec-magic trio, stripped together for one reason: the secret
    // purge's pathspecs are `:(icase)*.pem`-shaped, so anything that changes
    // how default magic is interpreted can silently turn the purge into a
    // no-op. GIT_LITERAL_PATHSPECS=1 disables magic outright (the `:(icase)`
    // prefix is then matched as literal text); GIT_NOGLOB_PATHSPECS=1 makes
    // the default no-glob, so `*.pem` matches only a file literally named
    // `*.pem`. GIT_ICASE_PATHSPECS only broadens matching — the safe
    // direction — but goes with its siblings so the purge's matching rules
    // come from the command line rather than from whatever the host exported.
    "GIT_LITERAL_PATHSPECS",
    "GIT_NOGLOB_PATHSPECS",
    "GIT_ICASE_PATHSPECS",
    // GIT_EXTERNAL_DIFF replaces the diff driver with an arbitrary command.
    // `--no-ext-diff` closes it at the one diff call site, but the isolation
    // primitive must not depend on every future call site remembering a flag.
    "GIT_EXTERNAL_DIFF",
    "GIT_CONFIG",
    "GIT_CONFIG_GLOBAL",
    "GIT_CONFIG_SYSTEM",
    "GIT_CONFIG_NOSYSTEM",
    "GIT_CONFIG_PARAMETERS",
    "GIT_CONFIG_COUNT",
    // GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n pairs are gated by
    // GIT_CONFIG_COUNT: removing COUNT disables the whole group. Key/value 0
    // is still removed to guard against extreme host injections that bypass
    // COUNT.
    "GIT_CONFIG_KEY_0",
    "GIT_CONFIG_VALUE_0",
];

/// Published through the `features::codex_acp` facade for the same-repo
/// `pinvou-cli`'s commit lane: the CLI must strip the exact identity list the
/// GUI's commit lane strips, or an ambient `GIT_AUTHOR_*` could outrank the
/// `-c user.name=` the CLI passes (same drift-prevention as
/// `GIT_OVERRIDE_KEYS` above); behavior is unchanged.
pub const GIT_IDENTITY_KEYS: [&str; 6] = [
    "GIT_AUTHOR_NAME",
    "GIT_AUTHOR_EMAIL",
    "GIT_AUTHOR_DATE",
    "GIT_COMMITTER_NAME",
    "GIT_COMMITTER_EMAIL",
    "GIT_COMMITTER_DATE",
];

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
/// Shared with the pinvou-cli kill tree (`support.rs` Windows arm) through
/// the targeted `platform::external_command` re-export — the `process`
/// module itself stays crate-private. The point of sharing is *resolution
/// parity*, not hardening: `external_application_path` only normalizes
/// `\\?\`/UNC spellings and returns a bare name like `taskkill` verbatim,
/// so a bare-name caller resolves exactly as it does for the GUI's own
/// `kill_pid_tree` (app dir → CWD → System32 → PATH). A caller that needs a
/// planted-`taskkill.exe`-in-CWD defense must resolve
/// `%SystemRoot%\System32\taskkill.exe` itself — this helper deliberately
/// does not invent a stricter PATH policy the GUI does not have.
///
/// The Windows `.cmd`/`.bat` branch keeps the manual `cmd /D /S /C` wrap
/// (unlike the CLI's own `build_command`, which delegates to std's hardened
/// batch quoting): these shims NEED the cmd interpretation to run at all
/// — an npm `.cmd` spawned directly is treated as a non-executable image —
/// and every current caller passes a fixed, first-party-resolved path with
/// no attacker-shaped argument material, so the BatBadBut argument-quoting
/// class the CLI pin guards against has no vector here. If a caller ever
/// forwards external arguments through this helper, route that caller
/// through `build_command`'s std delegation instead of extending the wrap.
pub fn external_command(executable: &Path) -> Command {
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
/// stdout/stderr are captured into temporary files instead of pipes: a
/// completed child's output is returned in full even when a daemonized
/// descendant inherited the capture handles, and the wall-clock timeout
/// stays effective regardless of descendants (issue #536). Capture is
/// bounded: if either stream exceeds [`MAX_CAPTURE_BYTES`] while this call
/// is running, the whole process tree is terminated regardless of the
/// kill-tree flag, and the error carries the truncated output. The bound is
/// an in-call guarantee: a descendant that escapes the tree kill (e.g. it
/// called setsid) can keep writing to the unlinked capture file after this
/// call returns — the space is reclaimed when it exits, and this platform's
/// managed process-tree ownership (the Windows Job Object) closes that
/// residual gap. A capture that cannot be read back in full is an error,
/// never partial `Output`.
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
///
/// The capture bound documented on [`output_with_timeout`] applies here too.
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
    let limit = MAX_CAPTURE_BYTES;
    // Regular files cannot fill up and block the child the way a pipe can.
    // They also let a completed parent's output be read back without
    // waiting for a descendant that inherited the same handles to exit, so
    // the wall-clock timeout stays effective (issue #536).
    // Named captures are required for the readback below: every clone of
    // one capture handle — ours and every inherited stdout/stderr copy in
    // the child tree — shares a single file position (`dup`/`try_clone`
    // semantics on Unix, shared `CurrentByteOffset` on Windows), so the
    // launcher must read through a freshly opened handle with a position
    // of its own, which needs the file's name. A crash between spawn and
    // cleanup can leak the name; bounded, like the rest of the temp dir's
    // churn.
    let stdout_file = tempfile::NamedTempFile::new()
        .map_err(|error| format!("create {program} stdout capture failed: {error}"))?;
    let stderr_file = tempfile::NamedTempFile::new()
        .map_err(|error| format!("create {program} stderr capture failed: {error}"))?;
    let stdout_target = stdout_file
        .as_file()
        .try_clone()
        .map_err(|error| format!("clone {program} stdout capture failed: {error}"))?;
    let stderr_target = stderr_file
        .as_file()
        .try_clone()
        .map_err(|error| format!("clone {program} stderr capture failed: {error}"))?;
    command
        .stdin(Stdio::null())
        .stdout(Stdio::from(stdout_target))
        .stderr(Stdio::from(stderr_target));
    let (mut child, managed_tree) =
        spawn_managed_std(command).map_err(|error| format!("spawn {program} failed: {error}"))?;

    let started = Instant::now();
    let status = loop {
        // Check the capture bound before the exit poll so a runaway writer
        // is terminated mid-flight: with [`MAX_CAPTURE_BYTES`] crossed, the
        // whole tree is killed regardless of the kill-tree flag — an
        // overflowing writer never gets to outlive the caller. A
        // burst-then-exit writer can cross the cap only after it already
        // exited; the probe below sends that state down the same
        // managed-tree termination path as the post-exit re-check instead
        // of a doomed kill on a dead pid.
        if capture_limit_exceeded(stdout_file.as_file(), limit)
            || capture_limit_exceeded(stderr_file.as_file(), limit)
        {
            let kill_note = kill_note_suffix(managed_tree.terminate());
            let _ = child.kill();
            let reap_note = reap_note_suffix(reap_killed_child(&mut child, REAP_GRACE));
            let stdout = marked_capture_bytes(read_capture(&stdout_file, limit));
            let stderr = marked_capture_bytes(read_capture(&stderr_file, limit));
            let detail = subprocess_output_detail(&stdout, &stderr);
            return Err(format!(
                "{program} {CAPTURE_OVERFLOW_ANCHOR} {limit} bytes; subprocess tree termination requested: {detail}{reap_note}{kill_note}"
            ));
        }
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) if started.elapsed() <= timeout => {
                std::thread::sleep(Duration::from_millis(50));
            }
            Ok(None) => {
                let kill_note = if kill_tree_on_timeout {
                    kill_note_suffix(managed_tree.terminate())
                } else {
                    String::new()
                };
                let _ = child.kill();
                let reap_note = reap_note_suffix(reap_killed_child(&mut child, REAP_GRACE));
                let stdout = marked_capture_bytes(read_capture(&stdout_file, limit));
                let stderr = marked_capture_bytes(read_capture(&stderr_file, limit));
                let detail = subprocess_output_detail(&stdout, &stderr);
                return Err(format!(
                    "{program}{CAPTURE_TIMEOUT_ANCHOR}{}s: {detail}{reap_note}{kill_note}",
                    timeout.as_secs()
                ));
            }
            Err(error) => {
                let kill_note = if kill_tree_on_timeout {
                    kill_note_suffix(managed_tree.terminate())
                } else {
                    String::new()
                };
                let _ = child.kill();
                // 等待错误本身已是调用方的诊断信息,回收结果按既有惯例丢弃;
                // 有界回收保证这条路径同样不越过自身的时间预算。捕获输出
                // 已写入临时文件,残余孙进程无法再把调用方拖过截止时间。
                let _ = reap_killed_child(&mut child, REAP_GRACE);
                return Err(format!("{program} wait error: {error}{kill_note}"));
            }
        }
    };

    // A fast writer can cross the limit and exit between the last size check
    // and `try_wait()`. Re-check after completion so overflow is never reported
    // as a silently truncated success.
    if capture_limit_exceeded(stdout_file.as_file(), limit)
        || capture_limit_exceeded(stderr_file.as_file(), limit)
    {
        let kill_note = kill_note_suffix(managed_tree.terminate());
        let stdout = marked_capture_bytes(read_capture(&stdout_file, limit));
        let stderr = marked_capture_bytes(read_capture(&stderr_file, limit));
        let detail = subprocess_output_detail(&stdout, &stderr);
        return Err(format!(
            "{program} {CAPTURE_OVERFLOW_ANCHOR} {limit} bytes; subprocess tree termination requested: {detail}{kill_note}"
        ));
    }

    match deliver_captures(&stdout_file, &stderr_file) {
        CaptureDelivery::Output { stdout, stderr } => Ok(Output {
            status,
            stdout,
            stderr,
        }),
        CaptureDelivery::ReadbackFailed(error) => Err(format!(
            "{program} subprocess capture readback failed: {error}"
        )),
    }
}

/// How long to keep reaping a child after a termination request. A
/// successful `kill()` does not force a prompt exit: a process stuck in
/// uninterruptible kernel sleep (Unix D state) never observes SIGKILL, so
/// a blocking `wait()` could hang the caller past its own timeout budget.
/// Bounded reaping trades that unbounded hang for a bounded, reported
/// leak.
pub(crate) const REAP_GRACE: Duration = Duration::from_secs(2);

/// Outcome of [`reap_killed_child`].
pub(crate) enum Reap {
    /// Child exited and was reaped.
    Reaped,
    /// Grace elapsed without an observed exit; the child may linger.
    Abandoned,
    /// Waiting for the child errored; the reap is not established.
    Failed(std::io::Error),
}

/// Reap a killed child, bounded by `grace`. Polls `try_wait()` instead of a
/// blocking `wait()` so the caller keeps its timeout guarantee even when the
/// child cannot exit promptly, and returns the outcome instead of swallowing
/// wait errors. The child does not have to have accepted the kill for this to
/// stay bounded: at most `grace` is spent even on a still-live child.
pub(crate) fn reap_killed_child(child: &mut Child, grace: Duration) -> Reap {
    let deadline = Instant::now() + grace;
    loop {
        match child.try_wait() {
            Ok(Some(_)) => return Reap::Reaped,
            Ok(None) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Ok(None) => return Reap::Abandoned,
            Err(error) => return Reap::Failed(error),
        }
    }
}

/// Renders a reap outcome as an error-message suffix; an empty string when
/// the child was reaped cleanly.
fn reap_note_suffix(reap: Reap) -> String {
    match reap {
        Reap::Reaped => String::new(),
        Reap::Abandoned => {
            String::from("; termination requested but the child has not exited; it may linger")
        }
        Reap::Failed(error) => format!("; reaping the child failed: {error}"),
    }
}

/// Renders a failed tree-kill request as an error-message suffix; an empty
/// string when the request succeeded. A failed request must reach the
/// caller's error message: the termination note would otherwise claim a
/// tree termination that did not happen (the Windows taskkill helper can
/// overrun its own budget and give up).
fn kill_note_suffix(result: std::io::Result<()>) -> String {
    result
        .err()
        .map(|error| format!("; kill tree request failed: {error}"))
        .unwrap_or_default()
}

/// True once the capture file has grown past `limit`. The child's own file
/// offset is irrelevant: the file length is observed, so the check works
/// while the child is running and after it exited.
fn capture_limit_exceeded(file: &std::fs::File, limit: u64) -> bool {
    file.metadata().is_ok_and(|metadata| metadata.len() > limit)
}

/// Outcome of reading a capture file back.
#[derive(Debug)]
enum CaptureReadback {
    /// The whole capture, at most `limit` bytes.
    Complete(Vec<u8>),
    /// The capture grew past `limit`; these are its first `limit` bytes.
    Clipped(Vec<u8>),
    /// The capture could not be read back in full. Carried as the error,
    /// never as bytes: whatever was read is not the child's output, and a
    /// consumer parsing it as such would report partial content as the
    /// whole (an empty change list for a capture that failed to reopen).
    Failed(std::io::Error),
}

/// Read a capture file back, clipped to `limit`. The extra probe byte
/// distinguishes "exactly at the limit" from "clipped".
///
/// The readback opens a fresh handle instead of using the capture handle:
/// every clone of one capture handle — ours and every inherited
/// stdout/stderr copy in the child tree — shares a single file position
/// (`dup`/`try_clone` on Unix, duplicated handles on Windows), so reading
/// through it would move the writers' position: backward at the first
/// read, or forward to a mid-file frontier after a chunk lands. A
/// surviving descendant's next write would then overwrite captured or
/// still-unread bytes below the cap with every step succeeding and no
/// truncation marker. A freshly opened handle has a position of its own
/// on both platforms (new open file description / new file object), so
/// writers keep appending and the capture stays an append log (pinned by
/// [`read_capture_never_rewinds_the_shared_writer_cursor`] and
/// [`read_capture_writer_writes_append_across_readback_blocks`]).
fn read_capture(capture: &tempfile::NamedTempFile, limit: u64) -> CaptureReadback {
    let mut buffer = Vec::new();
    let mut chunk = [0u8; CAPTURE_READ_CHUNK];
    let mut readback = match std::fs::File::open(capture.path()) {
        Ok(readback) => readback,
        Err(error) => return CaptureReadback::Failed(error),
    };
    // One chunk past the limit is enough to classify the capture: anything
    // beyond it is clipped below anyway.
    while buffer.len() as u64 <= limit {
        match readback.read(&mut chunk) {
            Ok(0) => break,
            Ok(read) => buffer.extend_from_slice(&chunk[..read]),
            Err(error) => return CaptureReadback::Failed(error),
        }
    }
    if buffer.len() as u64 > limit {
        buffer.truncate(limit as usize);
        return CaptureReadback::Clipped(buffer);
    }
    CaptureReadback::Complete(buffer)
}

/// The capture as bytes for the error-message detail and the success-path
/// delivery: a clipped capture carries [`CAPTURE_TRUNCATED_MARKER`], and a
/// capture that could not be read back in full degrades to the marker alone
/// — on an error path the message is already the failure, so the detail
/// stays best-effort (the success path never gets here with a failed
/// readback, see [`deliver_captures`]).
fn marked_capture_bytes(readback: CaptureReadback) -> Vec<u8> {
    match readback {
        CaptureReadback::Complete(bytes) => bytes,
        CaptureReadback::Clipped(mut bytes) => {
            bytes.extend_from_slice(CAPTURE_TRUNCATED_MARKER);
            bytes
        }
        CaptureReadback::Failed(_) => CAPTURE_TRUNCATED_MARKER.to_vec(),
    }
}

/// How a completed child's two captures reach the caller; see
/// [`deliver_captures`].
enum CaptureDelivery {
    Output {
        stdout: Vec<u8>,
        stderr: Vec<u8>,
    },
    /// A capture could not be read back in full.
    ReadbackFailed(std::io::Error),
}

/// Success-path delivery of the two captures. A capture that cannot be read
/// back in full is a failure, never bytes: the caller parses `Output` as the
/// child's complete output, and a marker in place of content is dropped by
/// a line parser as noise (a list read from a capture that failed to reopen
/// would come back empty and be reported as "no changes"). A clipped
/// capture (a write landing between the post-exit re-check and the read) is
/// delivered marked, as the boundary consumers expect.
fn deliver_captures(
    stdout_file: &tempfile::NamedTempFile,
    stderr_file: &tempfile::NamedTempFile,
) -> CaptureDelivery {
    let stdout = read_capture(stdout_file, MAX_CAPTURE_BYTES);
    let stderr = read_capture(stderr_file, MAX_CAPTURE_BYTES);
    match (stdout, stderr) {
        (CaptureReadback::Failed(error), _) | (_, CaptureReadback::Failed(error)) => {
            CaptureDelivery::ReadbackFailed(error)
        }
        (stdout, stderr) => CaptureDelivery::Output {
            stdout: marked_capture_bytes(stdout),
            stderr: marked_capture_bytes(stderr),
        },
    }
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
/// 一起终止，不孤儿化（评审中危项）。Windows no-op（兼容 taskkill /T 仅尽力终止父链仍在的进程树）。
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

/// Kernel-owned membership for a subprocess tree created by this module.
///
/// On Windows the job intentionally does not use KILL_ON_JOB_CLOSE: successful
/// launchers may return while a descendant continues. Explicit termination is
/// reserved for timeout, output overflow or cancellation. On Unix the
/// existing process-group id remains the ownership token.
#[derive(Clone)]
pub(crate) struct ManagedProcessTree {
    #[cfg(windows)]
    job: std::sync::Arc<WindowsJobObject>,
    #[cfg(not(windows))]
    pid: u32,
}

impl ManagedProcessTree {
    pub(crate) fn terminate(&self) -> std::io::Result<()> {
        #[cfg(windows)]
        {
            self.job.terminate()
        }
        #[cfg(not(windows))]
        {
            kill_process_tree(self.pid)
        }
    }
}

fn spawn_managed_std(
    command: Command,
) -> std::io::Result<(std::process::Child, ManagedProcessTree)> {
    #[cfg(windows)]
    {
        spawn_managed_windows(command, WindowsJobObject::assign_and_resume)
    }
    #[cfg(not(windows))]
    {
        let mut command = command;
        std_process_group_leader(&mut command);
        let child = command.spawn()?;
        let pid = child.id();
        Ok((child, ManagedProcessTree { pid }))
    }
}

// The injected operation keeps the failure cleanup identical to production;
// tests exercise real suspended children and real job assignment, not a mock child.
#[cfg(windows)]
fn spawn_managed_windows(
    mut command: Command,
    activate: impl FnOnce(&WindowsJobObject, u32) -> std::io::Result<()>,
) -> std::io::Result<(std::process::Child, ManagedProcessTree)> {
    prepare_windows_managed_std_command(&mut command);
    let job = std::sync::Arc::new(WindowsJobObject::create()?);
    let mut child = command.spawn()?;
    if let Err(error) = activate(&job, child.id()) {
        let _ = job.terminate();
        let _ = child.kill();
        let reap_note = reap_note_suffix(reap_killed_child(&mut child, REAP_GRACE));
        return Err(std::io::Error::new(
            error.kind(),
            format!("{error}{reap_note}"),
        ));
    }
    Ok((child, ManagedProcessTree { job }))
}

/// Start an async subprocess in an owned tree before it can spawn descendants.
/// Failure to activate the suspended Windows child is fail-closed with bounded
/// reaping; normal completion does not implicitly kill surviving descendants.
pub(crate) async fn spawn_managed_tokio(
    mut command: tokio::process::Command,
) -> std::io::Result<(tokio::process::Child, ManagedProcessTree)> {
    command.kill_on_drop(true);
    #[cfg(windows)]
    {
        spawn_managed_windows_tokio(command, WindowsJobObject::assign_and_resume).await
    }
    #[cfg(not(windows))]
    {
        tokio_process_group_leader(&mut command);
        let child = command.spawn()?;
        let pid = child
            .id()
            .ok_or_else(|| std::io::Error::other("spawned child has no process id"))?;
        Ok((child, ManagedProcessTree { pid }))
    }
}

#[cfg(windows)]
async fn spawn_managed_windows_tokio(
    mut command: tokio::process::Command,
    activate: impl FnOnce(&WindowsJobObject, u32) -> std::io::Result<()>,
) -> std::io::Result<(tokio::process::Child, ManagedProcessTree)> {
    use windows_sys::Win32::System::Threading::{
        CREATE_NEW_PROCESS_GROUP, CREATE_NO_WINDOW, CREATE_SUSPENDED,
    };
    command.kill_on_drop(true);
    command.creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_SUSPENDED);
    let job = std::sync::Arc::new(WindowsJobObject::create()?);
    let mut child = command.spawn()?;
    let activation = child
        .id()
        .ok_or_else(|| std::io::Error::other("spawned child has no process id"))
        .and_then(|pid| activate(&job, pid));
    if let Err(error) = activation {
        let _ = job.terminate();
        let _ = child.start_kill();
        let reap = tokio::time::timeout(REAP_GRACE, child.wait()).await;
        let note = match reap {
            Ok(Ok(_)) => String::new(),
            Ok(Err(reap_error)) => format!("; child reap failed: {reap_error}"),
            Err(_) => "; child reap timed out".to_owned(),
        };
        return Err(std::io::Error::new(error.kind(), format!("{error}{note}")));
    }
    Ok((child, ManagedProcessTree { job }))
}

#[cfg(windows)]
struct WindowsJobObject {
    // Kernel handles are valid across threads; Arc retains this owner until
    // the last installer/canceller finishes. Drop closes the handle once.
    handle: usize,
}

#[cfg(windows)]
impl WindowsJobObject {
    fn create() -> std::io::Result<Self> {
        use windows_sys::Win32::System::JobObjects::CreateJobObjectW;

        // SAFETY: null security/name pointers request an unnamed job with
        // default security. The returned owned handle is closed by Drop.
        let handle = unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) };
        if handle.is_null() {
            return Err(std::io::Error::last_os_error());
        }
        Ok(Self {
            handle: handle as usize,
        })
    }

    fn raw(&self) -> windows_sys::Win32::Foundation::HANDLE {
        self.handle as windows_sys::Win32::Foundation::HANDLE
    }

    fn assign_and_resume(&self, pid: u32) -> std::io::Result<()> {
        self.assign(pid)?;
        resume_windows_process(pid)
    }

    fn assign(&self, pid: u32) -> std::io::Result<()> {
        use windows_sys::Win32::Foundation::CloseHandle;
        use windows_sys::Win32::System::JobObjects::AssignProcessToJobObject;
        use windows_sys::Win32::System::Threading::{
            OpenProcess, PROCESS_SET_QUOTA, PROCESS_TERMINATE,
        };

        // CREATE_SUSPENDED prevents the root from spawning before assignment.
        // SAFETY: OpenProcess has no pointer arguments; the returned handle is
        // closed exactly once below.
        let process = unsafe { OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid) };
        if process.is_null() {
            return Err(std::io::Error::last_os_error());
        }
        // SAFETY: both job and process handles are valid for this call.
        let assigned = unsafe { AssignProcessToJobObject(self.raw(), process) };
        let assign_error = (assigned == 0).then(std::io::Error::last_os_error);
        // SAFETY: close the owned process handle once assignment is complete.
        unsafe { CloseHandle(process) };
        if let Some(error) = assign_error {
            return Err(error);
        }
        Ok(())
    }

    fn terminate(&self) -> std::io::Result<()> {
        use windows_sys::Win32::System::JobObjects::TerminateJobObject;

        // SAFETY: self retains a valid job handle. Job membership is tracked
        // by the kernel, including descendants whose intermediary has exited.
        if unsafe { TerminateJobObject(self.raw(), 1) } == 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(())
        }
    }
}

#[cfg(windows)]
impl Drop for WindowsJobObject {
    fn drop(&mut self) {
        use windows_sys::Win32::Foundation::CloseHandle;

        // No KILL_ON_JOB_CLOSE flag is set: normal completion deliberately
        // leaves descendants alive. Explicit terminate() is the only kill.
        // SAFETY: this type exclusively owns the non-null handle.
        unsafe { CloseHandle(self.raw()) };
    }
}

#[cfg(windows)]
fn prepare_windows_managed_std_command(command: &mut Command) {
    use std::os::windows::process::CommandExt as _;
    use windows_sys::Win32::System::Threading::{CREATE_NEW_PROCESS_GROUP, CREATE_SUSPENDED};

    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_SUSPENDED);
}

#[cfg(windows)]
fn resume_windows_process(pid: u32) -> std::io::Result<()> {
    use windows_sys::Win32::Foundation::{CloseHandle, ERROR_NO_MORE_FILES, GetLastError};
    use windows_sys::Win32::System::Diagnostics::ToolHelp::{
        CreateToolhelp32Snapshot, TH32CS_SNAPTHREAD, THREADENTRY32, Thread32First, Thread32Next,
    };
    use windows_sys::Win32::System::Threading::{OpenThread, ResumeThread, THREAD_SUSPEND_RESUME};

    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        // SAFETY: the snapshot handle is checked and closed exactly once.
        let snapshot = unsafe { CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0) };
        if snapshot == windows_sys::Win32::Foundation::INVALID_HANDLE_VALUE {
            return Err(std::io::Error::last_os_error());
        }
        // SAFETY: THREADENTRY32 contains only integer fields; zero is valid.
        // dwSize is initialized before passing the record to the ToolHelp API.
        let mut entry: THREADENTRY32 = unsafe { std::mem::zeroed() };
        entry.dwSize = std::mem::size_of::<THREADENTRY32>() as u32;
        // SAFETY: entry is correctly sized and valid while snapshot is open.
        let mut has_entry = unsafe { Thread32First(snapshot, &mut entry) } != 0;
        while has_entry {
            if entry.th32OwnerProcessID == pid {
                // SAFETY: OpenThread has no pointer arguments; handle is
                // closed below after the single ResumeThread call.
                let thread = unsafe { OpenThread(THREAD_SUSPEND_RESUME, 0, entry.th32ThreadID) };
                if thread.is_null() {
                    let error = std::io::Error::last_os_error();
                    // SAFETY: close the owned snapshot before returning.
                    unsafe { CloseHandle(snapshot) };
                    return Err(error);
                }
                // SAFETY: thread grants THREAD_SUSPEND_RESUME.
                let previous = unsafe { ResumeThread(thread) };
                let result = if previous == u32::MAX {
                    Err(std::io::Error::last_os_error())
                } else {
                    Ok(())
                };
                // Preserve ResumeThread's error before any subsequent Win32 call.
                // SAFETY: close the owned thread handle once.
                unsafe { CloseHandle(thread) };
                // Resume immediately lets the process create and retire
                // threads. Do not continue enumerating a now-live process.
                // SAFETY: close the owned snapshot before returning.
                unsafe { CloseHandle(snapshot) };
                return result;
            }
            // SAFETY: snapshot and initialized entry remain valid.
            has_entry = unsafe { Thread32Next(snapshot, &mut entry) } != 0;
        }
        let terminal_error = if !has_entry {
            // SAFETY: read before CloseHandle.
            let error = unsafe { GetLastError() };
            (error != ERROR_NO_MORE_FILES).then(|| std::io::Error::from_raw_os_error(error as i32))
        } else {
            None
        };
        // SAFETY: close the owned snapshot handle once.
        unsafe { CloseHandle(snapshot) };
        if let Some(error) = terminal_error {
            return Err(error);
        }
        if Instant::now() >= deadline {
            return Err(std::io::Error::new(
                std::io::ErrorKind::TimedOut,
                format!("suspended process {pid} exposed no resumable thread"),
            ));
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Compatibility termination for a process not created through a managed
/// spawn helper. Windows pins the root identity and runs bounded `taskkill`,
/// but this best-effort snapshot cannot prove coverage of double-fork races.
/// Unix terminates the independently configured process group directly.
/// The synchronous output helpers retain their own private Job Object owner.
pub(crate) fn kill_process_tree(pid: u32) -> std::io::Result<()> {
    if crate::platform::capabilities::is_windows() {
        #[cfg(windows)]
        {
            // This compatibility path is only for a process not created with
            // ManagedProcessTree. Holding the root handle pins its identity,
            // but taskkill's user-space snapshot cannot close double-fork
            // races. Safety-critical callers must retain and terminate the
            // managed job returned by spawn_managed_std instead.
            let Some(_root_identity) = WindowsProcessHandle::open(pid)? else {
                return Ok(());
            };
            let mut taskkill = external_command(Path::new("taskkill"));
            taskkill.args(["/PID", &pid.to_string(), "/T", "/F"]);
            let status = run_termination_command_with_timeout(taskkill, WINDOWS_TASKKILL_TIMEOUT)?;
            return status
                .success()
                .then_some(())
                .ok_or_else(|| std::io::Error::other(format!("taskkill exited with {status}")));
        }
        #[cfg(not(windows))]
        unreachable!("runtime Windows capability cannot be true in a non-Windows build");
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

#[cfg(windows)]
fn run_termination_command_with_timeout(
    mut command: Command,
    timeout: Duration,
) -> std::io::Result<std::process::ExitStatus> {
    command
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    let mut child = command.spawn()?;
    let started = Instant::now();
    loop {
        if let Some(status) = child.try_wait()? {
            return Ok(status);
        }
        if started.elapsed() >= timeout {
            // `Child::kill` maps to TerminateProcess on Windows and does not
            // wait for the helper. Do not call wait() here: the whole point of
            // this boundary is that a wedged taskkill can never extend the
            // caller's wall-clock timeout without bound.
            let kill_error = child.kill().err();
            return Err(std::io::Error::new(
                std::io::ErrorKind::TimedOut,
                match kill_error {
                    Some(error) => format!(
                        "process-tree termination helper timed out after {}ms and could not be stopped: {error}",
                        timeout.as_millis()
                    ),
                    None => format!(
                        "process-tree termination helper timed out after {}ms",
                        timeout.as_millis()
                    ),
                },
            ));
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

#[cfg(windows)]
struct WindowsProcessHandle {
    #[cfg(test)]
    pid: u32,
    handle: windows_sys::Win32::Foundation::HANDLE,
}

#[cfg(windows)]
impl WindowsProcessHandle {
    fn open(pid: u32) -> std::io::Result<Option<Self>> {
        use windows_sys::Win32::Foundation::{ERROR_INVALID_PARAMETER, GetLastError};
        use windows_sys::Win32::System::Threading::{
            OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION, PROCESS_SYNCHRONIZE,
        };

        if pid <= 1 {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                format!("refusing process-tree termination for pid {pid}"),
            ));
        }
        // Holding this handle keeps the process object (and therefore its PID)
        // alive even after exit. The compatibility taskkill path and tests can
        // therefore refer to one stable root identity rather than a reused PID.
        // SAFETY: OpenProcess has no pointer arguments and the owned handle is
        // closed exactly once by Drop.
        let handle = unsafe {
            OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_SYNCHRONIZE,
                0,
                pid,
            )
        };
        if handle.is_null() {
            // SAFETY: GetLastError is read immediately after OpenProcess.
            let error = unsafe { GetLastError() };
            return if error == ERROR_INVALID_PARAMETER {
                Ok(None)
            } else {
                Err(std::io::Error::from_raw_os_error(error as i32))
            };
        }
        Ok(Some(Self {
            #[cfg(test)]
            pid,
            handle,
        }))
    }
}

#[cfg(windows)]
impl Drop for WindowsProcessHandle {
    fn drop(&mut self) {
        use windows_sys::Win32::Foundation::CloseHandle;

        // SAFETY: this type exclusively owns the non-null handle.
        unsafe { CloseHandle(self.handle) };
    }
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
pub(crate) mod tests {
    use super::*;
    use std::io::{Seek as _, Write as _};

    /// reap_killed_child collects an exited child promptly instead of
    /// waiting out the grace deadline. Soft-skips when no `sleep` binary
    /// is on PATH (bare Windows hosts; windows-latest CI bash steps have
    /// Git Bash's `sleep.exe` on PATH, so the test runs for real there).
    #[test]
    fn reap_killed_child_reaps_exited_child() {
        if Command::new("sleep").arg("0").status().is_err() {
            eprintln!("skipping: no `sleep` binary on this platform");
            return;
        }
        let mut child = Command::new("sleep")
            .arg("0")
            .spawn()
            .expect("spawn sleep 0");
        let started = Instant::now();
        assert!(
            matches!(reap_killed_child(&mut child, REAP_GRACE), Reap::Reaped),
            "an exited child must be reaped successfully"
        );
        assert!(
            started.elapsed() < REAP_GRACE,
            "reaping an exited child must not wait out the grace deadline"
        );
    }

    /// The Abandoned branch is induced deterministically by a zero grace
    /// budget on a live child: no SIGKILL-surviving process is needed. Only
    /// the Failed branch is not inducible in-process (std caches the exit
    /// status, so a collected child can never report a wait error).
    #[test]
    fn reap_killed_child_abandons_a_live_child_on_zero_grace() {
        if Command::new("sleep").arg("0").status().is_err() {
            eprintln!("skipping: no `sleep` binary on this platform");
            return;
        }
        let mut child = Command::new("sleep")
            .arg("30")
            .spawn()
            .expect("spawn sleep 30");
        assert!(
            matches!(
                reap_killed_child(&mut child, Duration::ZERO),
                Reap::Abandoned
            ),
            "a live child with a zero grace budget must be abandoned"
        );
        // Clean up so the test leaves no zombie or stray sleeper behind.
        let _ = child.kill();
        assert!(
            matches!(reap_killed_child(&mut child, REAP_GRACE), Reap::Reaped),
            "cleanup kill must reap the child"
        );
    }

    const OUTPUT_FIXTURE_ENV: &str = "PINVOU_PROCESS_OUTPUT_FIXTURE";
    const OUTPUT_FIXTURE_MARKER_ENV: &str = "PINVOU_PROCESS_OUTPUT_FIXTURE_MARKER";
    const OUTPUT_FIXTURE_TEST: &str = "platform::process::tests::command_output_fixture";
    const DESCENDANT_STARTUP_HANDSHAKE: Duration = Duration::from_secs(20);
    const DESCENDANT_TIMEOUT_AFTER_STARTUP: Duration = Duration::from_secs(3);
    static PROCESS_FIXTURE_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn lock_process_fixture() -> std::sync::MutexGuard<'static, ()> {
        PROCESS_FIXTURE_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    fn output_fixture_command(mode: &str) -> Command {
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args(["--exact", OUTPUT_FIXTURE_TEST, "--nocapture"])
            .env(OUTPUT_FIXTURE_ENV, mode);
        command
    }

    /// Test-only native identity probes for consumer lifecycle regressions.
    /// All Windows details stay in the platform adapter, not in ACP tests.
    pub(crate) struct BoundFixtureProcess {
        #[cfg(windows)]
        identity: WindowsProcessHandle,
    }

    impl BoundFixtureProcess {
        pub(crate) fn supported() -> bool {
            cfg!(windows)
        }

        pub(crate) fn bind(pid: u32) -> std::io::Result<Option<Self>> {
            #[cfg(windows)]
            {
                Ok(WindowsProcessHandle::open(pid)?.map(|identity| Self { identity }))
            }
            #[cfg(not(windows))]
            {
                let _ = pid;
                Ok(None)
            }
        }

        pub(crate) fn exited(&self, timeout: Duration) -> bool {
            #[cfg(windows)]
            {
                use windows_sys::Win32::Foundation::WAIT_OBJECT_0;
                use windows_sys::Win32::System::Threading::WaitForSingleObject;
                // SAFETY: the bound identity owns the live handle for the whole probe.
                unsafe {
                    WaitForSingleObject(
                        self.identity.handle,
                        timeout.as_millis().min(u32::MAX as u128) as u32,
                    ) == WAIT_OBJECT_0
                }
            }
            #[cfg(not(windows))]
            {
                let _ = timeout;
                false
            }
        }

        pub(crate) fn terminate(&self) -> std::io::Result<()> {
            #[cfg(windows)]
            {
                terminate_windows_fixture_process(&self.identity)
            }
            #[cfg(not(windows))]
            {
                Err(std::io::Error::new(
                    std::io::ErrorKind::Unsupported,
                    "native fixture cleanup is only supported on Windows",
                ))
            }
        }
    }

    #[cfg(windows)]
    fn terminate_windows_fixture_process(process: &WindowsProcessHandle) -> std::io::Result<()> {
        use windows_sys::Win32::System::Threading::{
            OpenProcess, PROCESS_TERMINATE, TerminateProcess,
        };

        // The bound identity pins this fixture's PID while we acquire the
        // additional cleanup right. Do not depend on the bounded, best-effort
        // taskkill compatibility path to clean up a deliberately surviving child.
        // SAFETY: OpenProcess has no pointer arguments; the identity remains owned.
        let handle = unsafe { OpenProcess(PROCESS_TERMINATE, 0, process.pid) };
        if handle.is_null() {
            return Err(std::io::Error::last_os_error());
        }
        let cleanup = WindowsProcessHandle {
            pid: process.pid,
            handle,
        };
        // SAFETY: cleanup owns a handle with PROCESS_TERMINATE, closed once by Drop.
        if unsafe { TerminateProcess(cleanup.handle, 1) } == 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(())
    }

    pub(crate) fn consumer_orphan_fixture(mode: &str, marker: &Path) -> tokio::process::Command {
        let mut command = output_fixture_command(mode);
        command
            .env(OUTPUT_FIXTURE_MARKER_ENV, marker)
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        tokio::process::Command::from(command)
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

    /// Spawns a long-lived descendant copy with inherited capture handles
    /// and waits for its running marker. Shared by the retention
    /// ("descendant") and tree-kill ("overflow_tree") modes and by the
    /// writing-descendant launcher fixture.
    fn spawn_descendant_and_wait_for_running_marker(mode: &str) {
        let mut descendant = output_fixture_command(mode);
        descendant.stdout(Stdio::inherit()).stderr(Stdio::inherit());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt as _;

            // Give the successful descendant an independent console
            // lifecycle so this fixture models a real helper that outlives
            // its completed launcher while retaining the launcher's capture
            // handles.
            const DETACHED_PROCESS: u32 = 0x0000_0008;
            const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
            descendant.creation_flags(DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP);
        }
        let _child = descendant.spawn().unwrap();
        assert!(wait_for_fixture_marker(
            "running",
            DESCENDANT_STARTUP_HANDSHAKE
        ));
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
                spawn_descendant_and_wait_for_running_marker("descendant_success");
                println!("descendant-started");
            }
            Ok("writer_descendant") => {
                // Like `descendant_success`, but it WRITES to the inherited
                // capture handles for its whole lifetime, concurrent with
                // the launcher's own output and the parent's readback.
                if let Some(pid) = fixture_marker("pid") {
                    std::fs::write(pid, std::process::id().to_string()).unwrap();
                }
                if let Some(running) = fixture_marker("running") {
                    std::fs::write(running, b"running").unwrap();
                }
                let deadline = Instant::now() + Duration::from_secs(5);
                while Instant::now() < deadline {
                    let mut stdout = std::io::stdout().lock();
                    let _ = stdout.write_all(b"late-write\n");
                    let _ = stdout.flush();
                    drop(stdout);
                    std::thread::sleep(Duration::from_millis(100));
                }
                if let Some(finished) = fixture_marker("finished") {
                    std::fs::write(finished, b"finished").unwrap();
                }
            }
            Ok("writer_launcher") => {
                spawn_descendant_and_wait_for_running_marker("writer_descendant");
                println!("parent-output");
            }
            Ok("overflow_tree") => {
                // Overflow while a live descendant holds the inherited
                // capture handles: the whole group must die with the
                // launcher, so the tree-kill tests can pin actual
                // descendant death instead of just the error text. The
                // descendant records its pid in the pid marker before
                // raising the running marker.
                spawn_descendant_and_wait_for_running_marker("descendant_success");
                let payload = vec![b'x'; 64 * 1024];
                loop {
                    std::io::stdout().write_all(&payload).unwrap();
                }
            }
            Ok("descendant_wait") | Ok("overflow_descendant") => {
                #[cfg(windows)]
                {
                    // The intermediary exits after creating the long-lived
                    // grandchild. A later ToolHelp-only walk cannot recover
                    // G -> vanished C -> root, while Job Object membership
                    // remains authoritative.
                    let status = output_fixture_command("descendant_intermediary")
                        .stdout(Stdio::inherit())
                        .stderr(Stdio::inherit())
                        .spawn()
                        .unwrap()
                        .wait()
                        .unwrap();
                    assert!(status.success());
                    if let Some(intermediary_exited) = fixture_marker("intermediary-exited") {
                        std::fs::write(intermediary_exited, b"exited").unwrap();
                    }
                    assert!(wait_for_fixture_marker(
                        "identity-bound",
                        DESCENDANT_STARTUP_HANDSHAKE
                    ));
                    if std::env::var(OUTPUT_FIXTURE_ENV).as_deref() == Ok("overflow_descendant") {
                        let payload = vec![b'x'; 64 * 1024];
                        loop {
                            std::io::stdout().write_all(&payload).unwrap();
                        }
                    }
                    std::thread::sleep(Duration::from_secs(60));
                }
                #[cfg(not(windows))]
                {
                    let mut child = output_fixture_command("descendant_timeout")
                        .stdout(Stdio::inherit())
                        .stderr(Stdio::inherit())
                        .spawn()
                        .unwrap();
                    assert!(wait_for_fixture_marker(
                        "running",
                        DESCENDANT_STARTUP_HANDSHAKE
                    ));
                    child.wait().unwrap();
                }
            }
            #[cfg(windows)]
            Ok("descendant_intermediary") => {
                let _grandchild = output_fixture_command("descendant_timeout")
                    .stdout(Stdio::inherit())
                    .stderr(Stdio::inherit())
                    .spawn()
                    .unwrap();
                assert!(wait_for_fixture_marker(
                    "running",
                    DESCENDANT_STARTUP_HANDSHAKE
                ));
            }
            #[cfg(windows)]
            Ok("nested_job_parent") => {
                let output = output_with_timeout_and_kill_tree(
                    output_fixture_command("flood"),
                    Duration::from_secs(10),
                )
                .expect("spawn managed child under an existing parent job");
                assert!(output.status.success());
            }
            #[cfg(windows)]
            Ok("root_timeout") => {
                let _child = output_fixture_command("descendant_timeout")
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .spawn()
                    .unwrap();
                assert!(wait_for_fixture_marker(
                    "running",
                    DESCENDANT_STARTUP_HANDSHAKE
                ));
                std::thread::sleep(Duration::from_secs(60));
            }
            Ok("consumer_success_root") => {
                let _descendant = output_fixture_command("consumer_success_descendant")
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .spawn()
                    .unwrap();
                assert!(wait_for_fixture_marker(
                    "running",
                    DESCENDANT_STARTUP_HANDSHAKE
                ));
            }
            Ok("consumer_success_descendant") => {
                std::fs::write(
                    fixture_marker("pid").unwrap(),
                    std::process::id().to_string(),
                )
                .unwrap();
                std::fs::write(fixture_marker("running").unwrap(), b"running").unwrap();
                assert!(wait_for_fixture_marker(
                    "identity-bound",
                    DESCENDANT_STARTUP_HANDSHAKE
                ));
                // The consumer pins identity before asserting normal survival;
                // its explicit cleanup normally ends this bounded fallback.
                std::thread::sleep(Duration::from_secs(60));
            }
            Ok("descendant_success") => {
                if let Some(pid) = fixture_marker("pid") {
                    std::fs::write(pid, std::process::id().to_string()).unwrap();
                }
                if let Some(running) = fixture_marker("running") {
                    std::fs::write(running, b"running").unwrap();
                }
                std::thread::sleep(Duration::from_secs(5));
                if let Some(finished) = fixture_marker("finished") {
                    std::fs::write(finished, b"finished").unwrap();
                }
            }
            Ok("descendant_timeout") => {
                if let Some(pid) = fixture_marker("pid") {
                    std::fs::write(pid, std::process::id().to_string()).unwrap();
                }
                if let Some(running) = fixture_marker("running") {
                    std::fs::write(running, b"running").unwrap();
                }
                // Stay alive beyond the launcher's full cold-start allowance
                // so the outer timeout always exercises process-tree cleanup.
                #[cfg(windows)]
                std::thread::sleep(Duration::from_secs(60));
                #[cfg(not(windows))]
                std::thread::sleep(
                    DESCENDANT_STARTUP_HANDSHAKE + DESCENDANT_TIMEOUT_AFTER_STARTUP * 2,
                );
                if let Some(finished) = fixture_marker("finished") {
                    std::fs::write(finished, b"finished").unwrap();
                }
            }
            _ => {}
        }
    }

    #[test]
    fn git_override_keys_cover_redirection_and_config_injection_without_identity() {
        // Redirection and config-injection keys must be on the strip list.
        for key in [
            "GIT_DIR",
            "GIT_WORK_TREE",
            "GIT_INDEX_FILE",
            "GIT_OBJECT_DIRECTORY",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES",
            "GIT_COMMON_DIR",
            "GIT_GRAFT_FILE",
            "GIT_SHALLOW_FILE",
            "GIT_REPLACE_REF_BASE",
            "GIT_NAMESPACE",
            "GIT_CEILING_DIRECTORIES",
            "GIT_DISCOVERY_ACROSS_FILESYSTEM",
            "GIT_CONFIG",
            "GIT_CONFIG_GLOBAL",
            "GIT_CONFIG_NOSYSTEM",
            "GIT_CONFIG_COUNT",
            "GIT_CONFIG_PARAMETERS",
            // Named explicitly so removing one goes red here. The sibling
            // test iterates GIT_OVERRIDE_KEYS itself and is therefore
            // tautological; only the array length would otherwise object, and
            // it objects to every edit equally.
            "GIT_TEMPLATE_DIR",
            "GIT_LITERAL_PATHSPECS",
            "GIT_NOGLOB_PATHSPECS",
            "GIT_ICASE_PATHSPECS",
            "GIT_EXTERNAL_DIFF",
        ] {
            assert!(
                GIT_OVERRIDE_KEYS.contains(&key),
                "override keys must cover {key}"
            );
        }
        // Identity, transport, and terminal-behavior variables are not on the
        // subset list: operations on the user's real worktree follow the
        // behavior of the user's own git (the shadow-repository hardened list
        // covers identity separately).
        for key in [
            "GIT_AUTHOR_NAME",
            "GIT_AUTHOR_EMAIL",
            "GIT_COMMITTER_DATE",
            "GIT_SSH_COMMAND",
            "GIT_ASKPASS",
            "GIT_TERMINAL_PROMPT",
            "GIT_EDITOR",
            "GIT_PAGER",
            "GIT_TRACE",
            "HOME",
            "GITHUB_TOKEN",
        ] {
            assert!(
                !GIT_OVERRIDE_KEYS.contains(&key),
                "override keys must not contain {key}"
            );
        }
        // Shadow-repository hardened list = subset list + identity keys, and
        // the identity keys must actually take effect in the hardened list.
        let strengthened: std::collections::BTreeSet<&str> = GIT_OVERRIDE_KEYS
            .iter()
            .copied()
            .chain(GIT_IDENTITY_KEYS)
            .collect();
        for key in GIT_IDENTITY_KEYS {
            assert!(strengthened.contains(key), "full strip must cover {key}");
        }
        assert!(!GIT_OVERRIDE_KEYS.contains(&"GIT_AUTHOR_NAME"));
    }

    /// The strip helpers must translate the key lists into explicit
    /// `env_remove` entries on the Command (observable via `get_envs`); the
    /// code_checkpoints test only spot-checks representatives, so the full
    /// list coverage lives here, next to the lists themselves.
    #[test]
    fn strip_git_env_helpers_remove_every_listed_key() {
        let removed_entries = |command: &std::process::Command| -> Vec<std::ffi::OsString> {
            command
                .get_envs()
                .filter_map(|(name, value)| value.is_none().then(|| name.to_os_string()))
                .collect()
        };

        let mut hardened = std::process::Command::new("git");
        strip_all_git_env(&mut hardened);
        let removed = removed_entries(&hardened);
        for key in GIT_OVERRIDE_KEYS.iter().copied().chain(GIT_IDENTITY_KEYS) {
            assert!(
                removed.iter().any(|entry| entry == key),
                "strip_all_git_env must env_remove {key}"
            );
        }

        let mut soft = std::process::Command::new("git");
        strip_git_override_env(&mut soft);
        let removed = removed_entries(&soft);
        for key in GIT_OVERRIDE_KEYS {
            assert!(
                removed.iter().any(|entry| entry == key),
                "strip_git_override_env must env_remove {key}"
            );
        }
        for key in GIT_IDENTITY_KEYS {
            assert!(
                !soft
                    .get_envs()
                    .any(|(name, _)| name == std::ffi::OsStr::new(key)),
                "strip_git_override_env must not touch identity key {key}"
            );
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

    #[cfg(windows)]
    #[test]
    fn termination_helper_timeout_is_bounded() {
        let _fixture_lock = lock_process_fixture();
        let mut wedged_helper = HiddenCommand::new("ping");
        wedged_helper.args(["-t", "127.0.0.1"]);
        let started = Instant::now();

        let error = run_termination_command_with_timeout(wedged_helper, Duration::from_millis(250))
            .expect_err("a wedged termination helper should hit its deadline");

        assert_eq!(error.kind(), std::io::ErrorKind::TimedOut);
        assert!(
            started.elapsed() < Duration::from_secs(3),
            "a wedged taskkill helper exceeded its fixed deadline"
        );
    }

    #[cfg(windows)]
    fn assert_windows_process_exited(process: &WindowsProcessHandle, timeout: Duration) {
        use windows_sys::Win32::Foundation::WAIT_OBJECT_0;
        use windows_sys::Win32::System::Threading::{GetExitCodeProcess, WaitForSingleObject};

        let timeout_ms = u32::try_from(timeout.as_millis()).unwrap_or(u32::MAX);
        // SAFETY: the test owns a valid process handle for both calls.
        assert_eq!(
            unsafe { WaitForSingleObject(process.handle, timeout_ms) },
            WAIT_OBJECT_0,
            "process {} did not signal",
            process.pid
        );
        let mut exit_code = 259_u32; // STILL_ACTIVE
        // SAFETY: exit_code is writable and the process handle remains valid.
        assert_ne!(
            unsafe { GetExitCodeProcess(process.handle, &mut exit_code) },
            0,
            "GetExitCodeProcess failed for {}: {}",
            process.pid,
            std::io::Error::last_os_error()
        );
        assert_ne!(exit_code, 259, "process {} is still active", process.pid);
    }

    #[cfg(windows)]
    fn wait_for_path(path: &Path, timeout: Duration) -> bool {
        let started = Instant::now();
        while !path.exists() && started.elapsed() < timeout {
            std::thread::sleep(Duration::from_millis(25));
        }
        path.exists()
    }

    #[cfg(windows)]
    #[test]
    fn async_managed_spawn_failures_reap_suspended_children_without_running() {
        let _fixture_lock = lock_process_fixture();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        for assign_first in [false, true] {
            let temporary = tempfile::tempdir().unwrap();
            let marker = temporary.path().join("async-never-resumed");
            let mut command = output_fixture_command("descendant_success");
            command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
            let mut identity = None;
            let started = Instant::now();
            let result = runtime.block_on(spawn_managed_windows_tokio(
                tokio::process::Command::from(command),
                |job, pid| {
                    identity = WindowsProcessHandle::open(pid)?;
                    if assign_first {
                        job.assign(pid)?;
                    }
                    Err(std::io::Error::new(
                        std::io::ErrorKind::PermissionDenied,
                        "injected activation failure",
                    ))
                },
            ));
            assert!(result.is_err());
            assert_windows_process_exited(&identity.unwrap(), Duration::from_secs(3));
            assert!(!marker.with_extension("running").exists());
            assert!(started.elapsed() < Duration::from_secs(5));
        }
    }

    #[cfg(windows)]
    #[test]
    fn async_managed_cancel_kills_orphaned_descendants() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("async-orphan");
        let pid_file = marker.with_extension("pid");
        let bound = marker.with_extension("identity-bound");
        let exited = marker.with_extension("intermediary-exited");
        let mut command = output_fixture_command("descendant_wait");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let waiter = std::thread::spawn(move || {
            assert!(wait_for_path(&pid_file, DESCENDANT_STARTUP_HANDSHAKE));
            let pid = std::fs::read_to_string(pid_file)
                .unwrap()
                .trim()
                .parse()
                .unwrap();
            let identity = WindowsProcessHandle::open(pid).unwrap().unwrap();
            std::fs::write(&bound, b"bound").unwrap();
            assert_windows_process_exited(&identity, Duration::from_secs(35));
        });
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            let (mut child, tree) = spawn_managed_tokio(tokio::process::Command::from(command))
                .await
                .unwrap();
            assert!(wait_for_path(&exited, DESCENDANT_STARTUP_HANDSHAKE));
            assert!(wait_for_path(
                &marker.with_extension("identity-bound"),
                DESCENDANT_STARTUP_HANDSHAKE
            ));
            let canceller = tree.clone();
            drop(tree);
            canceller.terminate().unwrap();
            let status = tokio::time::timeout(Duration::from_secs(5), child.wait())
                .await
                .unwrap()
                .unwrap();
            assert!(!status.success());
        });
        waiter.join().unwrap();
        assert!(!marker.with_extension("finished").exists());
    }

    #[test]
    fn timeout_kills_managed_process_tree() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("descendant-marker");
        let running = marker.with_extension("running");
        #[cfg(windows)]
        let descendant_pid = marker.with_extension("pid");
        #[cfg(windows)]
        let identity_bound = marker.with_extension("identity-bound");
        #[cfg(windows)]
        let intermediary_exited = marker.with_extension("intermediary-exited");
        let finished = marker.with_extension("finished");
        let mut command = output_fixture_command("descendant_wait");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        #[cfg(windows)]
        let identity_waiter = std::thread::spawn(move || {
            assert!(
                wait_for_path(&descendant_pid, DESCENDANT_STARTUP_HANDSHAKE),
                "descendant pid was not published before timeout"
            );
            let pid = std::fs::read_to_string(&descendant_pid)
                .expect("read timeout descendant pid")
                .trim()
                .parse::<u32>()
                .expect("parse timeout descendant pid");
            let identity = WindowsProcessHandle::open(pid)
                .expect("open timeout descendant identity")
                .expect("timeout descendant exited before identity binding");
            std::fs::write(&identity_bound, b"bound").expect("publish bound identity marker");
            assert_windows_process_exited(&identity, Duration::from_secs(35));
        });
        // Starting two nested copies of the large test binary can exceed a
        // short timeout on a cold Windows machine. The fixture has an explicit
        // startup handshake; give that setup its full budget, then exercise a
        // real timeout while the confirmed descendant remains alive.
        let timeout = DESCENDANT_STARTUP_HANDSHAKE + DESCENDANT_TIMEOUT_AFTER_STARTUP;
        let error = output_with_timeout_and_kill_tree(command, timeout).unwrap_err();

        assert!(error.contains("timed out after"));
        assert!(running.exists(), "descendant did not start before timeout");
        #[cfg(windows)]
        assert!(
            intermediary_exited.exists(),
            "short-lived intermediary had not exited before job termination"
        );
        #[cfg(windows)]
        identity_waiter.join().expect("identity waiter panicked");
        #[cfg(not(windows))]
        std::thread::sleep(Duration::from_secs(6));
        assert!(
            !finished.exists(),
            "descendant survived process-tree timeout"
        );
    }

    #[cfg(windows)]
    #[test]
    fn output_overflow_kills_orphaned_managed_descendant() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("overflow-descendant");
        let pid_file = marker.with_extension("pid");
        let identity_bound = marker.with_extension("identity-bound");
        let intermediary_exited = marker.with_extension("intermediary-exited");
        let mut command = output_fixture_command("overflow_descendant");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let identity_waiter = std::thread::spawn(move || {
            assert!(wait_for_path(&pid_file, DESCENDANT_STARTUP_HANDSHAKE));
            let pid = std::fs::read_to_string(&pid_file)
                .unwrap()
                .trim()
                .parse()
                .unwrap();
            let identity = WindowsProcessHandle::open(pid).unwrap().unwrap();
            std::fs::write(identity_bound, b"bound").unwrap();
            assert_windows_process_exited(&identity, Duration::from_secs(35));
        });
        let result = output_with_timeout_and_kill_tree(command, Duration::from_secs(30));
        identity_waiter.join().unwrap();
        assert!(intermediary_exited.exists());
        assert!(result.unwrap_err().contains("output exceeded"));
    }

    #[cfg(windows)]
    #[test]
    fn ordinary_timeout_does_not_terminate_managed_descendants() {
        use windows_sys::Win32::Foundation::WAIT_TIMEOUT;
        use windows_sys::Win32::System::Threading::WaitForSingleObject;
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("root-only-timeout");
        let pid_file = marker.with_extension("pid");
        let mut command = output_fixture_command("root_timeout");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let (root_finished, root_result) = std::sync::mpsc::channel();
        let identity_waiter = std::thread::spawn(move || {
            assert!(wait_for_path(&pid_file, DESCENDANT_STARTUP_HANDSHAKE));
            let pid = std::fs::read_to_string(pid_file)
                .unwrap()
                .trim()
                .parse()
                .unwrap();
            let identity = WindowsProcessHandle::open(pid).unwrap().unwrap();
            root_result.recv_timeout(Duration::from_secs(35)).unwrap();
            // SAFETY: identity pins the exact descendant; zero wait only probes liveness.
            let survived = unsafe { WaitForSingleObject(identity.handle, 0) } == WAIT_TIMEOUT;
            terminate_windows_fixture_process(&identity)
                .expect("terminate the deliberately surviving timeout fixture");
            assert_windows_process_exited(&identity, Duration::from_secs(3));
            survived
        });
        let result = output_with_timeout(
            command,
            DESCENDANT_STARTUP_HANDSHAKE + DESCENDANT_TIMEOUT_AFTER_STARTUP,
        );
        root_finished.send(()).unwrap();
        let survived = identity_waiter.join().unwrap();
        assert!(result.unwrap_err().contains("timed out after"));
        assert!(
            survived,
            "ordinary timeout unexpectedly killed the descendant"
        );
    }

    #[cfg(windows)]
    #[test]
    fn managed_spawn_failures_reap_suspended_children_without_running_them() {
        let _fixture_lock = lock_process_fixture();
        for assign_first in [false, true] {
            let temporary = tempfile::tempdir().unwrap();
            let marker = temporary.path().join("never-resumed");
            let mut command = output_fixture_command("descendant_success");
            command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
            let mut identity = None;
            let started = Instant::now();
            let result = spawn_managed_windows(command, |job, pid| {
                identity = WindowsProcessHandle::open(pid)?;
                if assign_first {
                    job.assign(pid)?;
                }
                Err(std::io::Error::new(
                    std::io::ErrorKind::PermissionDenied,
                    if assign_first {
                        "injected resume failure"
                    } else {
                        "injected assign failure"
                    },
                ))
            });
            assert!(result.is_err());
            assert_windows_process_exited(&identity.unwrap(), Duration::from_secs(3));
            assert!(!marker.with_extension("running").exists());
            assert!(started.elapsed() < Duration::from_secs(5));
        }
    }

    #[cfg(windows)]
    #[test]
    fn managed_spawn_succeeds_under_an_existing_parent_job() {
        let _fixture_lock = lock_process_fixture();
        let output = output_with_timeout_and_kill_tree(
            output_fixture_command("nested_job_parent"),
            Duration::from_secs(20),
        )
        .unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
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

    /// A large but bounded burst of output must be captured completely:
    /// regular-file capture cannot fill up and stall the writer the way a
    /// full pipe buffer would, so a chatty child neither deadlocks nor
    /// loses output. Guarded by the fixture lock because it re-execs the
    /// test binary.
    #[test]
    fn output_capture_does_not_deadlock_when_buffers_are_large() {
        let _fixture_lock = lock_process_fixture();
        // The budget must absorb a cold re-exec of the test binary
        // (multi-second on a loaded/AV-scanned Windows runner); on success
        // paths the child exits on its own, so a larger ceiling costs
        // nothing. 90s because the engine's single budget also covers
        // fixture startup — under cargo-lock contention a 30s budget was
        // observed firing a *correct* timeout before a slow-starting child
        // ever reached its payload (round-3 review M3).
        let output = output_with_timeout_and_kill_tree(
            output_fixture_command("flood"),
            Duration::from_secs(90),
        )
        .unwrap();

        assert!(output.status.success());
        assert!(output.stdout.len() >= 256 * 1024);
        assert!(output.stderr.len() >= 256 * 1024);
    }

    /// A child that keeps writing past the capture limit must be stopped:
    /// the poll loop notices the crossed bound mid-flight and terminates
    /// the whole tree, reporting an overflow error instead of a truncated
    /// success. The fixture holds a live descendant while overflowing, so
    /// the descendant-death assertion below pins an actual tree kill, not
    /// just the error text.
    #[test]
    fn output_capture_limit_terminates_process_tree() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("tree-overflow-marker");
        let mut command = output_fixture_command("overflow_tree");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        // 90s: the single budget covers the launcher's own re-exec, the
        // descendant's up-to-20s startup handshake and the flood (M3).
        let error =
            output_with_timeout_and_kill_tree(command, Duration::from_secs(90)).unwrap_err();

        assert!(error.contains("output exceeded"), "{error}");
        assert_descendant_fixture_died(&marker);
    }

    /// Pins the load-bearing wiring for "the tree is terminated on overflow
    /// regardless of the kill-tree flag": the plain variant must also make
    /// its child a process-group leader, or the unconditional group kill
    /// degenerates into a no-op (the leader's pid names no group) and the
    /// descendant survives. Removing the `std_process_group_leader` call in
    /// `output_with_timeout` fails the descendant-death assertion below.
    #[test]
    fn plain_output_limit_also_terminates_the_process_tree() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("plain-overflow-marker");
        let mut command = output_fixture_command("overflow_tree");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        let error = output_with_timeout(command, Duration::from_secs(90)).unwrap_err();

        assert!(error.contains("output exceeded"), "{error}");
        assert_descendant_fixture_died(&marker);
    }

    /// Asserts the overflow_tree fixture's descendant actually died. The
    /// descendant records its pid before raising the running marker, so the
    /// pid file is stable once the fixture started. The window must stay
    /// below the descendant's 5s natural lifetime, so a surviving
    /// descendant cannot pass the assertion by exiting on its own. Unix-only
    /// because std has no portable liveness probe; on Windows the in-flight
    /// tree kill rides on the managed Job Object and its outcome is reported
    /// through the error message (real descendant-death pinning there is
    /// covered by the job-based fixtures above).
    #[cfg(unix)]
    fn assert_descendant_fixture_died(marker: &std::path::Path) {
        let pid: u32 = std::fs::read_to_string(marker.with_extension("pid"))
            .expect("descendant pid marker")
            .trim()
            .parse()
            .expect("descendant pid");
        // SAFETY: libc::kill with sig=0 only probes liveness; no memory is touched.
        let alive = |pid: u32| unsafe { libc::kill(pid as i32, 0) == 0 };
        let started = Instant::now();
        while alive(pid) && started.elapsed() < Duration::from_secs(3) {
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(
            !alive(pid),
            "descendant {pid} survived the overflow tree kill"
        );
    }

    /// Windows counterpart: the overflow error above is the pin here; see
    /// the unix variant for why descendant death is not assertable.
    #[cfg(not(unix))]
    fn assert_descendant_fixture_died(_marker: &std::path::Path) {}

    /// A writer that crosses the limit and exits cleanly must still be
    /// errored, never given a silently truncated success. The in-flight
    /// check catches a crossing seen before the exit poll; the post-exit
    /// re-check covers a write landing inside the final iteration, after
    /// the size check but before `try_wait` reports the exit. This test
    /// pins the caller-visible contract, not which check fires.
    #[test]
    fn completed_process_cannot_bypass_output_capture_limit() {
        let _fixture_lock = lock_process_fixture();
        let error = output_with_timeout_and_kill_tree(
            output_fixture_command("overflow_exit"),
            Duration::from_secs(90),
        )
        .unwrap_err();

        assert!(error.contains("output exceeded"), "{error}");
        // Unix: the termination claim must be present on both sub-paths —
        // the in-flight check kills a live-or-reaped root via the pgid
        // (ESRCH is success), and the post-exit re-check kills the same
        // way. This is the behavioral pin that the kill-honesty fix keeps
        // reporting the request (round-1 M2; nothing asserted it before
        // round-3 M4).
        #[cfg(unix)]
        assert!(
            error.contains("subprocess tree termination requested"),
            "{error}"
        );
        // Windows: the managed Job Object keeps orphaned descendants
        // terminable even after the root was reaped, so the overflow paths
        // always request termination through it — the claim in the message
        // stays honest on both platforms (the job-based fixtures above pin
        // the actual descendant death).
    }

    /// The core output-retention regression: a launcher that exits on time
    /// while a live descendant still holds the inherited capture handles
    /// must have its own output returned IN FULL (the retired pipe handoff
    /// reported empty/partial here), and the call must not wait out the
    /// descendant. Timing is measured from the descendant's own running
    /// marker so test-binary startup time cannot masquerade as waiting for
    /// the capture handles to close.
    #[test]
    fn completed_parent_does_not_wait_for_descendant_capture_handles() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("descendant-marker");
        let running = marker.with_extension("running");
        let finished = marker.with_extension("finished");
        let mut command = output_fixture_command("descendant");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        // Starting another copy of the large test binary can take several
        // seconds on a cold Windows machine. Measure from the descendant's
        // own running marker so startup time cannot masquerade as waiting
        // for the inherited capture handles to close.
        let (marker_sender, marker_receiver) = std::sync::mpsc::sync_channel(1);
        let marker_waiter = std::thread::spawn(move || {
            // The launcher's own handshake starts after ITS startup, so this
            // waiter's deadline must be strictly larger or a slow start
            // lands in the gap: the launcher succeeds, but the channel
            // already holds a miss and the expect below fails spuriously.
            let deadline = DESCENDANT_STARTUP_HANDSHAKE + Duration::from_secs(15);
            let started = Instant::now();
            while !running.exists() && started.elapsed() < deadline {
                std::thread::sleep(Duration::from_millis(25));
            }
            let _ = marker_sender.send(running.exists().then(Instant::now));
        });
        // The outer launcher and the descendant are separate copies of the
        // large test binary. Give their explicit startup handshake enough
        // room (M3: 90s — the single budget covers both re-execs and the
        // 20s handshake), while the assertion below still measures the real
        // contract strictly from the descendant's running marker.
        let output = output_with_timeout_and_kill_tree(command, Duration::from_secs(90)).unwrap();
        // Upper-bounds the waiter thread's remaining wait (its deadline is
        // launcher handshake + 15s, and the call itself gives the launcher
        // 90s), so the recv cannot expire before the thread reports.
        let marker_seen_at = marker_receiver
            .recv_timeout(Duration::from_secs(20))
            .unwrap()
            .expect("descendant did not start before launcher exit");
        marker_waiter.join().unwrap();

        assert!(
            output.status.success(),
            "fixture failed with {:?}: stdout={} stderr={}",
            output.status.code(),
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(String::from_utf8_lossy(&output.stdout).contains("descendant-started"));
        assert!(marker_seen_at.elapsed() < Duration::from_secs(3));
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

    /// The stronger retention variant: the descendant WRITES to the
    /// inherited capture handles for its whole lifetime, concurrent with
    /// the launcher's own line and the parent's readback. Under a reader
    /// that rewinds the shared cursor, such a write lands at the rewound
    /// position — overwriting the captured prefix below the cap, where no
    /// truncation marker would fire (the native Windows probe behind this
    /// observed b'lete-response' from b'complete-response'). Positional
    /// reads keep every writer appending, so lines from both producers
    /// must come back intact. The corruption itself is a race, so this
    /// test pins the caller-visible contract; the deterministic pin for
    /// the mechanism is
    /// [`read_capture_never_rewinds_the_shared_writer_cursor`].
    #[test]
    fn writing_descendant_cannot_corrupt_the_completed_parents_capture() {
        let _fixture_lock = lock_process_fixture();
        let temporary = tempfile::tempdir().unwrap();
        let marker = temporary.path().join("writer-marker");
        let mut command = output_fixture_command("writer_launcher");
        command.env(OUTPUT_FIXTURE_MARKER_ENV, &marker);
        // M3 budget: the launcher re-exec plus the descendant's up-to-20s
        // startup handshake all run inside the engine's single budget.
        let output = output_with_timeout_and_kill_tree(command, Duration::from_secs(90)).unwrap();

        assert!(
            output.status.success(),
            "launcher fixture failed with {:?}: stdout={}",
            output.status.code(),
            String::from_utf8_lossy(&output.stdout)
        );
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(
            stdout.contains("parent-output"),
            "the launcher's own line must come back intact: {stdout}"
        );
        assert!(
            stdout.contains("late-write"),
            "the writing descendant's lines must be captured: {stdout}"
        );
        // Hygiene: the writer descendant exits on its own after ~5s.
        let finished = marker.with_extension("finished");
        let started = Instant::now();
        while !finished.exists() && started.elapsed() < Duration::from_secs(8) {
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(
            finished.exists(),
            "writer descendant did not exit naturally"
        );
        std::thread::sleep(Duration::from_millis(100));
    }

    /// The deterministic regression pin for cursor-independent capture
    /// reads: the launcher's readback must leave the shared writer cursor
    /// exactly where it was. The cursor is parked mid-file and the payload
    /// spans several readback chunks so any movement is observable — with
    /// the cursor parked at EOF and a single-chunk payload, a read's final
    /// frontier coincides with EOF and the movement would hide (the blind
    /// spot of the previous Windows strictness, which asserted only
    /// "no rewind"). The child tree's inherited handles write at the
    /// shared cursor, so a backward rewind turns their next write into an
    /// overwrite of the captured prefix, and a mid-file frontier into an
    /// overwrite of the still-unread tail — below the cap, with no marker
    /// (reverting `read_capture` to a read through the shared handle
    /// turns this test red on both platforms).
    #[test]
    fn read_capture_never_rewinds_the_shared_writer_cursor() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        let payload = capture_read_payload(3);
        file.write_all(&payload).unwrap();
        file.seek(std::io::SeekFrom::Start(4)).unwrap();

        let captured = complete_capture(read_capture(&file, MAX_CAPTURE_BYTES));

        assert_eq!(captured, payload);
        assert_eq!(
            file.stream_position().unwrap(),
            4,
            "read_capture moved the shared writer cursor; the child tree \
             writes at that cursor and would overwrite captured or unread \
             bytes"
        );
    }

    /// Multi-block read/write interleaving pin, in the scheduling-stable
    /// form production admits: the writer cursor sits at the append
    /// frontier while a readback spanning several chunks completes, then a
    /// write lands and the next readback must return the earlier payload
    /// and the late write fully intact. Corruption DURING a readback is
    /// inherently a race, so that side is covered by the mechanism pin
    /// above plus the writing-descendant fixture
    /// ([`writing_descendant_cannot_corrupt_the_completed_parents_capture`]).
    #[test]
    fn read_capture_writer_writes_append_across_readback_blocks() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        let payload = capture_read_payload(2);
        file.write_all(&payload).unwrap();
        file.seek(std::io::SeekFrom::End(0)).unwrap();

        assert_eq!(
            complete_capture(read_capture(&file, MAX_CAPTURE_BYTES)),
            payload
        );
        assert_eq!(
            file.stream_position().unwrap(),
            payload.len() as u64,
            "the readback must leave the writer cursor at the append frontier"
        );

        file.write_all(b"late-write").unwrap();
        let mut expected = payload.clone();
        expected.extend_from_slice(b"late-write");
        assert_eq!(
            complete_capture(read_capture(&file, MAX_CAPTURE_BYTES)),
            expected
        );
    }

    /// `blocks` chunks of [`CAPTURE_READ_CHUNK`] bytes with a distinct
    /// byte per block, so per-block corruption is visible in the result.
    fn capture_read_payload(blocks: usize) -> Vec<u8> {
        (0..blocks)
            .flat_map(|block| vec![b'A' + block as u8; CAPTURE_READ_CHUNK])
            .collect()
    }

    fn complete_capture(readback: CaptureReadback) -> Vec<u8> {
        match readback {
            CaptureReadback::Complete(bytes) => bytes,
            CaptureReadback::Clipped(_) => panic!("capture unexpectedly clipped"),
            CaptureReadback::Failed(error) => panic!("capture readback failed: {error}"),
        }
    }

    /// reap_note_suffix is the counterpart of kill_note_suffix: empty when
    /// the child was reaped cleanly, an explicit lingering note on
    /// abandon, and a named failure clause otherwise. Pure function, so
    /// every branch runs on every platform.
    #[test]
    fn reap_note_suffix_covers_every_reap_outcome() {
        assert_eq!(reap_note_suffix(Reap::Reaped), "");
        let abandoned = reap_note_suffix(Reap::Abandoned);
        assert!(
            abandoned.starts_with("; termination requested"),
            "the note must read as an appended clause, got: {abandoned}"
        );
        assert!(
            abandoned.contains("may linger"),
            "the note must disclose the lingering child, got: {abandoned}"
        );
        let failed = reap_note_suffix(Reap::Failed(std::io::Error::other("wait interrupted")));
        assert!(
            failed.starts_with("; reaping the child failed: "),
            "the note must read as an appended failure clause, got: {failed}"
        );
        assert!(
            failed.contains("wait interrupted"),
            "the note must carry the underlying error, got: {failed}"
        );
    }

    /// read_capture must clip at the limit with the truncation marker, and
    /// treat exactly-at-limit as complete — the boundary the overflow
    /// checks and the "never mistaken for complete output" contract lean
    /// on.
    #[test]
    fn read_capture_clips_at_the_limit_and_marks_truncation() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        let payload = vec![b'a'; (MAX_CAPTURE_BYTES as usize) + 128];
        file.write_all(&payload).unwrap();
        let clipped = marked_capture_bytes(read_capture(&file, MAX_CAPTURE_BYTES));
        assert_eq!(
            clipped.len(),
            MAX_CAPTURE_BYTES as usize + CAPTURE_TRUNCATED_MARKER.len()
        );
        assert!(
            clipped.ends_with(CAPTURE_TRUNCATED_MARKER),
            "a clipped capture must carry the truncation marker"
        );
        assert!(
            clipped.starts_with(&payload[..MAX_CAPTURE_BYTES as usize]),
            "the clipped capture must keep the first MAX_CAPTURE_BYTES bytes"
        );

        let mut file = tempfile::NamedTempFile::new().unwrap();
        file.write_all(&vec![b'b'; MAX_CAPTURE_BYTES as usize])
            .unwrap();
        let exact = marked_capture_bytes(read_capture(&file, MAX_CAPTURE_BYTES));
        assert_eq!(exact.len(), MAX_CAPTURE_BYTES as usize);
        assert!(
            !exact.ends_with(CAPTURE_TRUNCATED_MARKER),
            "exactly-at-limit output is complete and must not be marked"
        );
    }

    /// A capture that cannot be reopened must read back as a failure, never
    /// as marker bytes that pass for the child's output: this is the
    /// reopen-failure surface (the capture file vanishing before the
    /// readback), and the delivery contract below pins what it becomes on
    /// the success path. Deleting by path works on both platforms —
    /// Windows marks the still-open file delete-pending, which makes any
    /// later open-by-name fail exactly as this contract needs.
    #[test]
    fn read_capture_reports_a_failed_readback_for_a_vanished_capture() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        file.write_all(b"payload").unwrap();
        std::fs::remove_file(file.path()).unwrap();

        match read_capture(&file, MAX_CAPTURE_BYTES) {
            CaptureReadback::Failed(_) => {}
            other => panic!("a vanished capture must read back as Failed, got {other:?}"),
        }
    }

    /// The false-success elimination, at the delivery level: with one
    /// capture vanished, the completed child's output must be a
    /// ReadbackFailed, never `Output` whose stdout is the truncation
    /// marker — `git_ok` checks only the exit status and a line parser
    /// drops the marker, so marker bytes here would surface as a
    /// successful empty result.
    #[test]
    fn read_capture_delivery_fails_instead_of_passing_marker_bytes_as_output() {
        let stderr_file = tempfile::NamedTempFile::new().unwrap();
        let mut stdout_file = tempfile::NamedTempFile::new().unwrap();
        stdout_file.write_all(b"record line\n").unwrap();
        std::fs::remove_file(stdout_file.path()).unwrap();

        match deliver_captures(&stdout_file, &stderr_file) {
            CaptureDelivery::ReadbackFailed(_) => {}
            CaptureDelivery::Output { stdout, .. } => panic!(
                "a vanished capture must not deliver as output, got {} bytes",
                stdout.len()
            ),
        }
    }

    /// The delivery keeps the documented marker semantics: a clipped
    /// capture is delivered marked, exactly-at-limit delivered in full and
    /// unmarked.
    #[test]
    fn read_capture_delivery_marks_a_clipped_capture() {
        let stderr_file = tempfile::NamedTempFile::new().unwrap();

        let mut stdout_file = tempfile::NamedTempFile::new().unwrap();
        stdout_file
            .write_all(&vec![b'a'; MAX_CAPTURE_BYTES as usize + 128])
            .unwrap();
        let CaptureDelivery::Output { stdout, stderr } =
            deliver_captures(&stdout_file, &stderr_file)
        else {
            panic!("a clipped capture must deliver as output");
        };
        assert_eq!(stderr, b"");
        assert_eq!(
            stdout.len(),
            MAX_CAPTURE_BYTES as usize + CAPTURE_TRUNCATED_MARKER.len()
        );
        assert!(stdout.ends_with(CAPTURE_TRUNCATED_MARKER));

        let mut stdout_file = tempfile::NamedTempFile::new().unwrap();
        stdout_file
            .write_all(&vec![b'b'; MAX_CAPTURE_BYTES as usize])
            .unwrap();
        let CaptureDelivery::Output { stdout, stderr } =
            deliver_captures(&stdout_file, &stderr_file)
        else {
            panic!("exactly-at-limit output must deliver in full");
        };
        assert_eq!(stdout.len(), MAX_CAPTURE_BYTES as usize);
        assert!(!stdout.ends_with(CAPTURE_TRUNCATED_MARKER));
        assert_eq!(stderr, b"");
    }

    /// kill_note_suffix is the platform-independent half of the tree-kill
    /// error folding: empty on success, and a failure that names the request
    /// (the capture-limit paths otherwise claim a termination that did not
    /// happen). Pure function, so both branches run on every platform —
    /// unlike the runtime-Windows taskkill branch it annotates.
    #[test]
    fn kill_note_suffix_is_empty_on_success_and_names_the_failure() {
        assert_eq!(kill_note_suffix(Ok(())), "");
        let note = kill_note_suffix(Err(std::io::Error::other("taskkill budget overrun")));
        assert!(
            note.starts_with("; kill tree request failed: "),
            "the note must read as an appended failure clause, got: {note}"
        );
        assert!(
            note.contains("taskkill budget overrun"),
            "the note must carry the underlying error, got: {note}"
        );
    }
}
