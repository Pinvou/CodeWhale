//! Advanced shell execution with background process support and sandboxing.
//!
//! Provides:
//! - Synchronous command execution with timeout
//! - Background process execution
//! - Process output retrieval
//! - Process termination
//! - Sandbox support (macOS Seatbelt and opt-in Linux bubblewrap)
//! - Streaming output (future)

use anyhow::{Context, Result, anyhow};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::fs::File;
use std::io;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use uuid::Uuid;
use wait_timeout::ChildExt;

#[cfg(unix)]
use std::os::fd::FromRawFd;
#[cfg(unix)]
use std::os::unix::process::CommandExt;
#[cfg(windows)]
use std::os::windows::io::AsRawHandle;
#[cfg(windows)]
use std::os::windows::io::FromRawHandle;
#[cfg(windows)]
use windows::Win32::Foundation::{CloseHandle, HANDLE};
#[cfg(windows)]
use windows::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
    SetInformationJobObject, TerminateJobObject,
};
#[cfg(windows)]
use windows::core::PCWSTR;

#[cfg(not(target_env = "ohos"))]
use portable_pty::{CommandBuilder, PtySize, native_pty_system};

mod guidance;
mod output;

use super::shell_output::{summarize_output, truncate_with_meta};
use crate::child_env;
use crate::sandbox::{
    CommandSpec,
    ExecEnv,
    SandboxManager,
    SandboxPolicy as ExecutionSandboxPolicy, // Rename to avoid conflict with spec::SandboxPolicy
    SandboxType,
};
use crate::tools::resource_admission::{
    CommandExpense, HeavyCommandPermit, MemoryPressure, acquire_heavy_command_permit,
    infer_command_expense,
};
use crate::work_graph::{
    EvidenceKind, EvidenceRef, OperationIntent, OperationOwnerSnapshot, OwnerState,
    SharedWorkRuntime,
};
use crate::worker_profile::ShellPolicy;
use output::{
    BoundedOutputAccumulator, BoundedOutputSnapshot, RAW_STREAM_SETTLED_TAIL_BYTES,
    RawOutputBuffer, SharedRawOutput, new_shared_raw_output, tail_from_buffer, tail_text,
    take_delta_from_buffer,
};

const READONLY_ENV_MARKER: &str = "CODEWHALE_INTERNAL_READONLY_ARGV";

#[cfg(unix)]
static PENDING_PERSISTENT_PROCESS_GROUPS: std::sync::OnceLock<
    Mutex<std::collections::HashSet<u32>>,
> = std::sync::OnceLock::new();

#[cfg(unix)]
fn pending_persistent_process_groups() -> &'static Mutex<std::collections::HashSet<u32>> {
    PENDING_PERSISTENT_PROCESS_GROUPS.get_or_init(|| Mutex::new(std::collections::HashSet::new()))
}

#[cfg(unix)]
fn register_pending_persistent_process_group(process_group_id: u32) {
    let mut groups = pending_persistent_process_groups()
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    groups.insert(process_group_id);
}

#[cfg(unix)]
fn unregister_pending_persistent_process_group(process_group_id: u32) {
    let mut groups = pending_persistent_process_groups()
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    groups.remove(&process_group_id);
}

/// Kill services that were staged for ownership transfer but have not yet
/// been released. The process-wide signal path calls this immediately before
/// `process::exit`, where Rust destructors cannot run.
#[cfg(unix)]
pub(crate) fn abort_pending_persistent_process_groups_for_exit() {
    let groups = {
        let mut groups = pending_persistent_process_groups()
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        groups.drain().collect::<Vec<_>>()
    };
    for process_group_id in groups {
        if let Ok(process_group_id) = i32::try_from(process_group_id) {
            // SAFETY: the id was captured from a child spawned with
            // `process_group(0)`. A negative pid targets that child's process
            // group, never Codewhale's own group.
            unsafe {
                libc::kill(-process_group_id, libc::SIGKILL);
            }
        }
    }
}

fn validate_shell_working_dir(path: &Path, inherited_session_workspace: bool) -> Result<()> {
    let metadata = std::fs::metadata(path).with_context(|| {
        let source = if inherited_session_workspace {
            "saved session workspace"
        } else {
            "requested working directory"
        };
        format!(
            "{source} is unavailable: {}. Restore or remap that directory, resume/fork the session from an existing workspace, or pass an explicit `working_dir`/`cwd` to exec_shell",
            path.display()
        )
    })?;
    if !metadata.is_dir() {
        let source = if inherited_session_workspace {
            "saved session workspace"
        } else {
            "requested working directory"
        };
        return Err(anyhow!(
            "{source} is not a directory: {}. Resume/fork from an existing workspace or pass an explicit `working_dir`/`cwd`",
            path.display()
        ));
    }
    Ok(())
}

/// Status of a shell process.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub enum ShellStatus {
    Running,
    Completed,
    Failed,
    Killed,
    TimedOut,
}

/// Result from a shell command execution.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ShellResult {
    pub task_id: Option<String>,
    pub status: ShellStatus,
    /// Lossless process exit status. Windows exception/NTSTATUS values use
    /// the full unsigned 32-bit range, so an i32 would corrupt them.
    pub exit_code: Option<i64>,
    pub stdout: String,
    pub stderr: String,
    pub duration_ms: u64,
    /// Original stdout length in bytes.
    #[serde(default)]
    pub stdout_len: usize,
    /// Original stderr length in bytes.
    #[serde(default)]
    pub stderr_len: usize,
    /// Bytes omitted from stdout due to truncation.
    #[serde(default)]
    pub stdout_omitted: usize,
    /// Bytes omitted from stderr due to truncation.
    #[serde(default)]
    pub stderr_omitted: usize,
    /// Whether stdout was truncated.
    #[serde(default)]
    pub stdout_truncated: bool,
    /// Whether stderr was truncated.
    #[serde(default)]
    pub stderr_truncated: bool,
    /// Whether the command was executed in a sandbox.
    #[serde(default)]
    pub sandboxed: bool,
    /// Type of sandbox used (if any).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sandbox_type: Option<String>,
    /// Whether the command was blocked by sandbox restrictions.
    #[serde(default)]
    pub sandbox_denied: bool,
}

/// Compact, UI-oriented view of a tracked background shell job.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ShellJobSnapshot {
    pub id: String,
    pub job_id: String,
    pub command: String,
    pub cwd: PathBuf,
    pub status: ShellStatus,
    pub exit_code: Option<i64>,
    pub elapsed_ms: u64,
    pub stdout_tail: String,
    pub stderr_tail: String,
    pub stdout_len: usize,
    pub stderr_len: usize,
    pub stdin_available: bool,
    pub stale: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub elapsed_since_output_ms: Option<u64>,
    pub linked_task_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner_agent_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner_agent_name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin_tool_call_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin_turn_id: Option<String>,
    /// Immutable root session that launched the job. Empty legacy records are
    /// intentionally hidden from session-scoped completion drains.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub owner_session_id: String,
}

/// Once-only completion event for a tracked background shell job.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ShellCompletionEvent {
    pub task_id: String,
    pub command: String,
    pub status: ShellStatus,
    pub exit_code: Option<i64>,
    pub duration_ms: u64,
    pub stdout_tail: String,
    pub stderr_tail: String,
    #[serde(default)]
    pub stdout_len: usize,
    #[serde(default)]
    pub stderr_len: usize,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub evidence_ref: Option<String>,
    pub linked_task_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner_agent_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner_agent_name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin_tool_call_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin_turn_id: Option<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub owner_session_id: String,
}

/// Byte evidence captured alongside a bounded completion event. Exact unless
/// the stream exceeded the in-memory retention ceiling, in which case the
/// omission is declared per stream rather than presented as complete.
#[derive(Debug, Clone)]
pub(crate) struct ShellCompletionEvidence {
    pub event: ShellCompletionEvent,
    stdout: Vec<u8>,
    stderr: Vec<u8>,
    stdout_omitted: usize,
    stderr_omitted: usize,
}

impl ShellCompletionEvidence {
    /// Encode each stream losslessly. UTF-8 remains readable; arbitrary bytes
    /// use base64 so `retrieve_tool_result` can still recover exact output.
    pub(crate) fn artifact_bytes(&self) -> Vec<u8> {
        fn stream(bytes: &[u8], omitted: usize) -> serde_json::Value {
            let mut value = match std::str::from_utf8(bytes) {
                Ok(content) => serde_json::json!({
                    "encoding": "utf-8",
                    "byte_length": bytes.len(),
                    "content": content,
                }),
                Err(_) => serde_json::json!({
                    "encoding": "base64",
                    "byte_length": bytes.len(),
                    "content": base64::engine::general_purpose::STANDARD.encode(bytes),
                }),
            };
            // Additive and only present when something was actually dropped, so
            // the common case stays byte-identical to the v1 artifact readers
            // already parse.
            if omitted > 0
                && let Some(object) = value.as_object_mut()
            {
                object.insert("leading_bytes_omitted".into(), omitted.into());
                object.insert(
                    "total_byte_length".into(),
                    bytes.len().saturating_add(omitted).into(),
                );
            }
            value
        }

        serde_json::json!({
            "schema": "codewhale.shell_completion.evidence.v1",
            "task_id": self.event.task_id,
            "command": self.event.command,
            "status": format!("{:?}", self.event.status),
            "exit_code": self.event.exit_code,
            "duration_ms": self.event.duration_ms,
            "origin_tool_call_id": self.event.origin_tool_call_id,
            "origin_turn_id": self.event.origin_turn_id,
            "stdout": stream(&self.stdout, self.stdout_omitted),
            "stderr": stream(&self.stderr, self.stderr_omitted),
        })
        .to_string()
        .into_bytes()
    }
}

// Keep the two inline streams at a 2 KiB combined hard ceiling. The durable
// artifact carries the exact bytes beyond these diagnostic tails.
const SHELL_COMPLETION_TAIL_BYTES: usize = 1_024;

/// How long a finished shell record stays listed in `/jobs`.
const FINISHED_SHELL_MAX_AGE: Duration = Duration::from_secs(3600);
/// Ceiling on finished records kept for the jobs panel. A long automation run
/// makes hundreds of `Bash` calls per hour; the panel is only useful for the
/// recent ones (#5472).
const MAX_FINISHED_SHELL_RECORDS: usize = 128;
/// Ceiling on bytes still held across all finished records. Each settled record
/// releases down to a 64 KiB tail, so this only binds when many large outputs
/// finish inside the same window.
const MAX_FINISHED_SHELL_BYTES: usize = 8 * 1024 * 1024;

fn bounded_completion_tail(buffer: &SharedRawOutput, max_bytes: usize) -> (usize, String) {
    let (total, candidate) = tail_from_buffer(buffer, max_bytes);
    if candidate.len() <= max_bytes {
        return (total, candidate);
    }
    let content_budget = max_bytes.saturating_sub(3);
    let mut start = candidate.len().saturating_sub(content_budget);
    while start < candidate.len() && !candidate.is_char_boundary(start) {
        start += 1;
    }
    (total, format!("...{}", &candidate[start..]))
}

/// Optional owner attribution for background shell work.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ShellJobOwner {
    pub agent_id: String,
    pub agent_name: String,
}

/// Full output view used by `/jobs show <id>`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ShellJobDetail {
    pub snapshot: ShellJobSnapshot,
    pub stdout: String,
    pub stderr: String,
}

pub struct ShellDeltaResult {
    pub command: String,
    pub result: ShellResult,
    pub stdout_total_len: usize,
    pub stderr_total_len: usize,
}

enum ShellChild {
    Process(Child),
    #[cfg(not(target_env = "ohos"))]
    Pty(Box<dyn portable_pty::Child + Send>),
}
#[cfg(unix)]
impl ShellChild {
    fn process_id(&self) -> Option<u32> {
        match self {
            Self::Process(child) => Some(child.id()),
            #[cfg(not(target_env = "ohos"))]
            Self::Pty(_) => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ShellOwnership {
    Managed,
    PersistPending,
    Released,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PersistentServiceReceipt {
    pub task_id: String,
    pub pid: u32,
    pub process_group_id: u32,
    pub ownership: String,
}

#[cfg(unix)]
fn signal_child_process_group(child: &Child, signal: libc::c_int) -> std::io::Result<()> {
    let pgid = child.id() as libc::pid_t;
    if pgid <= 0 {
        return Ok(());
    }

    let result = unsafe { libc::kill(-pgid, signal) };
    if result == 0 {
        Ok(())
    } else {
        let err = std::io::Error::last_os_error();
        if err.raw_os_error() == Some(libc::ESRCH) {
            // The group is already gone (or never formed); nothing to signal.
            Ok(())
        } else {
            Err(err)
        }
    }
}

#[cfg(unix)]
fn kill_child_process_group(child: &mut Child) -> std::io::Result<()> {
    let pgid = child.id() as libc::pid_t;
    if pgid <= 0 {
        return child.kill();
    }

    signal_child_process_group(child, libc::SIGKILL).or_else(|_| child.kill())
}

/// Bounded wait for the direct child to exit. Returns true once the child was
/// reaped (or the wait errored), false when the grace elapsed first. Unlike
/// `Child::wait`, this can never wedge the caller behind a child stuck in
/// uninterruptible sleep.
#[cfg(unix)]
fn wait_child_bounded(child: &mut Child, grace: Duration) -> bool {
    let deadline = Instant::now() + grace;
    loop {
        match child.try_wait() {
            Ok(Some(_)) | Err(_) => return true,
            Ok(None) => {}
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Terminate a shell's whole process group with a bounded SIGTERM → SIGKILL
/// escalation (#52). The previous kill path SIGKILLed only the direct child
/// and then joined output-reader threads with no timeout, so the tool
/// returned whenever the command's descendants felt like exiting — observed
/// as a 120s foreground timeout returning after 300s. Every step here is
/// bounded: the tool returns at ~timeout + grace.
///
/// `Ok(true)` means the direct child's exit was observed (reaped, or the
/// wait errored). `Ok(false)` means the stop was sent but the direct child
/// was not reaped within the reap grace — it may still be alive (e.g.
/// uninterruptible sleep) and must keep counting as a potential writer
/// until a later observation confirms the exit.
#[cfg(unix)]
fn terminate_child_process_group(child: &mut Child) -> std::io::Result<bool> {
    escalate_child_process_group(child, KILL_TERM_GRACE, KILL_REAP_GRACE)
}

/// The escalation with injectable graces so tests can drive each expiry
/// without waiting out the production constants.
#[cfg(unix)]
fn escalate_child_process_group(
    child: &mut Child,
    term_grace: Duration,
    reap_grace: Duration,
) -> std::io::Result<bool> {
    // Cooperative stop first so shells and their children can run traps and
    // clean up; bounded so a SIGTERM-ignoring command cannot stall the caller.
    let _ = signal_child_process_group(child, libc::SIGTERM);
    if wait_child_bounded(child, term_grace) {
        // The leader exited on SIGTERM; descendants may linger, so SIGKILL
        // the rest of the group (ESRCH when it is already empty).
        kill_child_process_group(child)?;
        return Ok(true);
    }
    kill_child_process_group(child)?;
    Ok(wait_child_bounded(child, reap_grace))
}

/// Configure parent-death signaling so shell-spawned children are reaped when
/// the TUI dies abnormally (#421). On Linux this installs
/// `PR_SET_PDEATHSIG(SIGTERM)` via `pre_exec` — the kernel then sends SIGTERM
/// to the child the moment the parent process exits, even on SIGKILL of the
/// TUI. The cancellation path already SIGKILLs the whole process group, so
/// this only fires when the parent dies without running its drop / cleanup
/// code (panic during shutdown, OOM, hardware crash, etc.).
///
/// On macOS / Windows there's no kernel equivalent. The existing graceful
/// path (`kill_child_process_group` from the cancellation token) still
/// handles normal shutdown; abnormal exit can leak children — tracked as a
/// follow-up watchdog item per the original issue's acceptance criteria.
#[cfg(all(target_os = "linux", not(target_env = "ohos")))]
fn install_parent_death_signal(cmd: &mut Command) {
    use std::os::unix::process::CommandExt;
    // SAFETY: `pre_exec` runs in the child between fork and exec. The closure
    // only calls `libc::prctl` with stack-allocated constant arguments and
    // does not touch heap memory or the parent's locks. Both requirements
    // (async-signal-safe + no allocation in the post-fork window) are met.
    unsafe {
        cmd.pre_exec(|| {
            let result = libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM, 0, 0, 0);
            if result == -1 {
                // Surface the errno but do not abort the spawn — the child
                // will simply lose the parent-death cleanup safety net.
                Err(std::io::Error::last_os_error())
            } else {
                Ok(())
            }
        });
    }
}

/// Whether PowerShell refused to load the tool's temporary `-File` script
/// because of the machine's execution policy.
///
/// Only what the console formatter never translates is matched. The refusal
/// prose is localized; what survives is the error-record metadata:
///
/// * `unauthorizedaccess` - the `FullyQualifiedErrorId` of a script the
///   security manager refused (its label stays English under en-US, zh-CN, and
///   ja-JP);
/// * `parentcontainserrorrecordexception` or `pssecurityexception` - the
///   exception type of that refusal, for the host `-File` path and the
///   in-session `&` path respectively. .NET type names are never localized,
///   and a command that fails on its own reports its own exception type, so an
///   ordinary access-denied error inside the script cannot trigger a retry.
///
/// `about_execution_policies` (the help topic both refusals cite) stays English
/// too and covers a truncated metadata block. The `LinkID=135170` fwlink is
/// deliberately *not* matched: the formatter wraps it mid-token at its line
/// width, so it is absent from the captured text of a real refusal.
///
/// Two structural rules keep a *different* refusal out of the retry, because a
/// retry is only safe when nothing ran yet:
///
/// * a `<path>.ps1:<line>` location line rules every branch out: it means a
///   statement already ran, so the failure happened *inside* a script rather
///   than at the host's load boundary, and retrying would duplicate whatever
///   executed before it (a top-level load refusal prints no location line);
/// * the refusal has to name the exact `-File` path this invocation passed. A
///   command that spawns its own child (`powershell -File inner.ps1`, which
///   gets no dispatcher bypass) reports a top-level-shaped refusal with no
///   location line, yet the outer script did run the statements before that
///   call, so only a refusal of our own temporary script is retried.
fn powershell_execution_policy_rejection(stderr: &str, stdout: &str, refused_script: &str) -> bool {
    let mut haystack = String::with_capacity(stderr.len() + stdout.len() + 1);
    haystack.push_str(stderr);
    haystack.push('\n');
    haystack.push_str(stdout);
    let haystack = haystack.to_ascii_lowercase();
    // Windows paths are case-insensitive, and the localized message always
    // carries the path it refused.
    if !haystack.contains(&refused_script.to_ascii_lowercase()) {
        return false;
    }
    if haystack.match_indices(".ps1:").any(|(index, _)| {
        haystack[index + 5..]
            .chars()
            .next()
            .is_some_and(|c| c.is_ascii_digit())
    }) {
        return false;
    }
    (haystack.contains("unauthorizedaccess")
        && (haystack.contains("parentcontainserrorrecordexception")
            || haystack.contains("pssecurityexception")))
        || haystack.contains("about_execution_policies")
}

/// The temporary `-File` script this invocation handed to PowerShell, when the
/// spec is a temp-script launch at all.
fn powershell_temp_script(spec: &CommandSpec) -> Option<&str> {
    let mut args = spec.args.iter();
    while let Some(arg) = args.next() {
        if arg == "-File" {
            return args.next().map(String::as_str);
        }
    }
    None
}

/// Whether a finished PowerShell attempt is a refusal the inline retry may
/// repair: only a *failed* temp `-File` run qualifies. A completed run keeps
/// its result, and a `Killed` (the user cancelled) or `TimedOut` attempt must
/// never be re-run - that would defeat the stop or double the wall clock.
fn powershell_refusal_needs_inline_retry(spec: &CommandSpec, result: &ShellResult) -> bool {
    if result.status != ShellStatus::Failed {
        return false;
    }
    let Some(refused_script) = powershell_temp_script(spec) else {
        return false;
    };
    powershell_execution_policy_rejection(&result.stderr, &result.stdout, refused_script)
}

/// Keep the first attempt's result when the inline retry cannot even start.
///
/// Reaching the retry means the first attempt was already a refusal, so the
/// recovery path's own spawn failure is strictly less informative than the
/// refusal evidence it would otherwise replace: annotate that stderr and
/// return the first result.
fn keep_first_result_after_a_failed_retry(
    mut first: ShellResult,
    error: &anyhow::Error,
) -> ShellResult {
    if !first.stderr.is_empty() && !first.stderr.ends_with('\n') {
        first.stderr.push('\n');
    }
    first.stderr.push_str(&format!(
        "[codewhale] the inline -EncodedCommand retry could not start ({error}); \
         the temporary script was refused by the local execution policy\n"
    ));
    first
}

/// Attach `args` to a `std::process::Command`, honoring shell-quoting on
/// Windows.
///
/// Issue #1691: on Windows the shell command is invoked as
/// `cmd /C "chcp 65001 >NUL & <command>"`. Rust's `Command::arg` applies
/// MSVCRT (`CommandLineToArgvW`) escaping, turning the embedded `"` in a
/// quoted argument (e.g. `git commit -m "feat: complete sub-pages"`) into
/// `\"`. `cmd.exe` does NOT use MSVCRT parsing — it treats `\` literally and
/// `"` as a bare quote toggle — so the escaped payload is mis-tokenized and
/// `git` receives `feat:`, `complete`, `sub-pages"` as separate pathspecs
/// (the reported `pathspec 'sub-pages"' did not match` symptom). Passing the
/// `cmd /C` payload through `CommandExt::raw_arg` suppresses std's escaping so
/// the string reaches `cmd.exe` verbatim, exactly as a terminal would.
#[cfg(windows)]
fn push_shell_args(cmd: &mut Command, program: &str, args: &[String]) {
    use std::os::windows::process::CommandExt;
    // The `cmd /C <payload>` shape is the only place std's per-arg escaping
    // corrupts a quoted command. Pass `/C` and the payload raw so the quotes
    // survive; any other program keeps normal (correct) escaping. Match `cmd`
    // by file stem so a full path (`C:\Windows\System32\cmd.exe`) or `.exe`
    // suffix still triggers the raw-arg path.
    let is_cmd = std::path::Path::new(program)
        .file_stem()
        .and_then(|s| s.to_str())
        .map(|s| s.eq_ignore_ascii_case("cmd"))
        .unwrap_or(false);
    if is_cmd && args.len() == 2 && args[0].eq_ignore_ascii_case("/C") {
        cmd.raw_arg(&args[0]);
        cmd.raw_arg(&args[1]);
    } else {
        cmd.args(args);
    }
}

#[cfg(not(windows))]
fn push_shell_args(cmd: &mut Command, _program: &str, args: &[String]) {
    // Unix delegates tokenization entirely to `sh -c <command>`; the command
    // string is passed as a single argv entry and never split by us.
    cmd.args(args);
}

#[cfg(not(all(target_os = "linux", not(target_env = "ohos"))))]
fn install_parent_death_signal(_cmd: &mut Command) {
    // No kernel-level equivalent on macOS / Windows. The cooperative
    // cancellation + process_group SIGKILL path covers normal shutdown;
    // abnormal exit (panic without unwind, SIGKILL of the TUI) can still
    // leak children on those platforms — tracked as a follow-up.
}

#[cfg(windows)]
#[derive(Debug)]
struct WindowsJob {
    handle: HANDLE,
}

#[cfg(windows)]
// SAFETY: Windows job handles are process-wide kernel handles. Moving the
// wrapper between threads does not invalidate the handle, and access is
// externally synchronized by ShellManager's mutex.
unsafe impl Send for WindowsJob {}
#[cfg(windows)]
// SAFETY: The wrapper exposes only terminate/drop operations around a kernel
// handle; concurrent use is guarded by ShellManager.
unsafe impl Sync for WindowsJob {}

#[cfg(windows)]
impl WindowsJob {
    fn attach_to_child(child: &Child) -> std::io::Result<Self> {
        let handle = unsafe { CreateJobObjectW(None, PCWSTR::null()).map_err(windows_io_error)? };
        let job = Self { handle };

        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;

        unsafe {
            SetInformationJobObject(
                job.handle,
                JobObjectExtendedLimitInformation,
                &limits as *const _ as *const core::ffi::c_void,
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
            .map_err(windows_io_error)?;

            let process_handle = HANDLE(child.as_raw_handle());
            AssignProcessToJobObject(job.handle, process_handle).map_err(windows_io_error)?;
        }

        Ok(job)
    }

    fn terminate(&self) -> std::io::Result<()> {
        unsafe { TerminateJobObject(self.handle, 1).map_err(windows_io_error) }
    }
}

#[cfg(windows)]
impl Drop for WindowsJob {
    fn drop(&mut self) {
        unsafe {
            let _ = CloseHandle(self.handle);
        }
    }
}

#[cfg(windows)]
fn windows_io_error(error: windows::core::Error) -> std::io::Error {
    std::io::Error::other(error)
}

#[cfg(windows)]
fn terminate_windows_job(job: Option<&WindowsJob>, child: &mut Child) -> std::io::Result<()> {
    if let Some(job) = job {
        match job.terminate() {
            Ok(()) => return Ok(()),
            Err(error) => {
                tracing::warn!(
                    ?error,
                    "failed to terminate Windows job object; falling back to immediate child kill"
                );
            }
        }
    }
    child.kill()
}

#[cfg(windows)]
fn terminate_and_close_windows_job(windows_job: Option<WindowsJob>) {
    if let Some(job) = windows_job.as_ref()
        && let Err(err) = job.terminate()
    {
        tracing::warn!(
            ?err,
            "failed to terminate Windows shell job before closing job handle"
        );
    }
    drop(windows_job);
}

#[cfg(windows)]
fn terminate_child_and_close_windows_job(
    windows_job: Option<WindowsJob>,
    child: &mut Child,
) -> std::io::Result<()> {
    let result = terminate_windows_job(windows_job.as_ref(), child);
    drop(windows_job);
    result
}

#[cfg(windows)]
fn attach_windows_job(child: &Child, command: &str) -> Option<WindowsJob> {
    match WindowsJob::attach_to_child(child) {
        Ok(job) => Some(job),
        Err(error) => {
            tracing::warn!(
                ?error,
                command,
                "failed to attach Windows shell process to job object; descendant cleanup degraded"
            );
            None
        }
    }
}

#[cfg(windows)]
fn terminate_unregistered_process(child: &mut Child, job: Option<&WindowsJob>) {
    let _ = terminate_windows_job(job, child);
    let _ = child.wait();
}

#[cfg(not(windows))]
fn terminate_unregistered_process(child: &mut Child) {
    #[cfg(unix)]
    {
        let _ = kill_child_process_group(child);
        let _ = wait_child_bounded(child, KILL_REAP_GRACE);
    }
    #[cfg(not(unix))]
    {
        let _ = child.kill();
        let _ = child.wait();
    }
}

#[derive(Clone, Copy, Debug)]
struct ShellExitStatus {
    code: Option<i64>,
    success: bool,
}

impl ShellExitStatus {
    fn from_std(status: std::process::ExitStatus) -> Self {
        Self {
            code: status.code().map(std_exit_code_i64),
            success: status.success(),
        }
    }

    #[cfg(not(target_env = "ohos"))]
    fn from_pty(status: portable_pty::ExitStatus) -> Self {
        Self {
            code: Some(i64::from(status.exit_code())),
            success: status.success(),
        }
    }
}

#[cfg(windows)]
fn std_exit_code_i64(code: i32) -> i64 {
    // std exposes Windows DWORD process statuses through i32. Reinterpret
    // negative values as their original unsigned bit pattern so codes such
    // as 0xC0000005 survive JSON, persistence, and diagnostics unchanged.
    i64::from(code as u32)
}

#[cfg(not(windows))]
fn std_exit_code_i64(code: i32) -> i64 {
    i64::from(code)
}

impl ShellChild {
    fn try_wait(&mut self) -> std::io::Result<Option<ShellExitStatus>> {
        match self {
            ShellChild::Process(child) => child
                .try_wait()
                .map(|status| status.map(ShellExitStatus::from_std)),
            #[cfg(not(target_env = "ohos"))]
            ShellChild::Pty(child) => child
                .try_wait()
                .map(|status| status.map(ShellExitStatus::from_pty)),
        }
    }

    #[cfg(not(windows))]
    fn kill(&mut self) -> std::io::Result<()> {
        match self {
            #[cfg(unix)]
            ShellChild::Process(child) => kill_child_process_group(child),
            #[cfg(not(unix))]
            ShellChild::Process(child) => child.kill(),
            #[cfg(not(target_env = "ohos"))]
            ShellChild::Pty(child) => child.kill(),
        }
    }
}

enum StdinWriter {
    Pipe(ChildStdin),
    /// Unix PTY lane: a dup of the portable-pty master fd, so interact
    /// writes can run on the bounded worker thread exactly like pipe
    /// writes. The reader thread holds its own clone of the master fd for
    /// the task's whole lifetime, so this dup is NOT the last master-side
    /// handle: dropping it delivers nothing to the slave on its own. An
    /// explicit `close_stdin` therefore delivers the ^D bytes itself,
    /// through the same bounded worker as a payload write (see
    /// [`bounded_stdin_eof_delivery`]), and a delivery that fails or
    /// stalls claims no end-of-file.
    /// The cfg mirrors the only constructor (the unix arm inside the
    /// `not(target_env = "ohos")` PTY block — ohos returns from tty mode
    /// before PTY setup): keeping the variant off ohos avoids a
    /// never-constructed `dead_code` variant there, which `deny(warnings)`
    /// makes fatal, and every referencing site carries the same predicate.
    #[cfg(all(unix, not(target_env = "ohos")))]
    PtyFile(std::fs::File),
    #[cfg(all(not(target_env = "ohos"), not(unix)))]
    Pty(Box<dyn Write + Send>),
}

impl StdinWriter {
    fn write_all(&mut self, data: &[u8]) -> std::io::Result<()> {
        match self {
            StdinWriter::Pipe(stdin) => stdin.write_all(data),
            #[cfg(all(unix, not(target_env = "ohos")))]
            StdinWriter::PtyFile(file) => file.write_all(data),
            #[cfg(all(not(target_env = "ohos"), not(unix)))]
            StdinWriter::Pty(writer) => writer.write_all(data),
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        match self {
            StdinWriter::Pipe(stdin) => stdin.flush(),
            #[cfg(all(unix, not(target_env = "ohos")))]
            StdinWriter::PtyFile(file) => file.flush(),
            #[cfg(all(not(target_env = "ohos"), not(unix)))]
            StdinWriter::Pty(writer) => writer.flush(),
        }
    }
}

/// Blocks SIGPIPE for the guarded region on unix, restoring it on drop.
/// The process deliberately runs with SIGPIPE at SIG_DFL (`run_with_args`
/// re-arms it so delegated children see conventional pipe semantics), which
/// arms it for in-process threads too. A bounded stdin write that times out
/// deliberately leaves its worker blocked mid-write on the handle, and the
/// documented stall recovery — killing the task, or the stalled command
/// simply exiting later — closes the read end: the resumed write would
/// otherwise kill the whole host process with SIGPIPE instead of failing
/// with EPIPE like every handled write error.
///
/// Blocking alone is not sufficient: a SIGPIPE generated while blocked is
/// not discarded — it stays PENDING on the worker thread, and unblocking
/// delivers a pending unblocked signal before `pthread_sigmask` returns.
/// With SIGPIPE at SIG_DFL that would kill the process one instruction
/// after the "handled" EPIPE, exactly where the guard ends. The drop
/// therefore checks this thread's pending set and consumes a pending
/// SIGPIPE with `sigwait` BEFORE restoring the mask. A write-failure
/// SIGPIPE is thread-directed, so a pending SIGPIPE inside a guarded
/// region is one this region's write generated; `sigpending` also reports
/// process-directed signals, but nothing in this process raises SIGPIPE
/// that way, and consuming such a signal under SIG_DFL would only spare
/// the process. Test binaries run with std's SIG_IGN, under
/// which a generated SIGPIPE is discarded at generation and never becomes
/// pending — which is why the suite cannot catch a block-and-restore guard;
/// the consume path is pinned by re-arming SIG_DFL inside a helper process
/// (see `sigpipe_guard_survives_a_broken_pipe_restore`).
struct BlockedSigpipeGuard {
    #[cfg(unix)]
    previous: libc::sigset_t,
}

impl BlockedSigpipeGuard {
    fn new() -> Self {
        #[cfg(unix)]
        {
            // SAFETY: `zeroed` is valid for `sigset_t` (an array of
            // integer words) and both pointers below are local stack
            // values. A failed `pthread_sigmask` is not expected with
            // these local sets; if it did fail, the write would run with
            // the process default disposition (the pre-guard behavior)
            // and the drop's restore would install the zeroed `previous`
            // mask — an empty set, leaving this thread's signals unblocked.
            unsafe {
                let mut blocked: libc::sigset_t = std::mem::zeroed();
                let mut previous: libc::sigset_t = std::mem::zeroed();
                libc::sigemptyset(&mut blocked);
                libc::sigaddset(&mut blocked, libc::SIGPIPE);
                // Best-effort: if the mask cannot be set, the write runs
                // with the process default and the failure mode is simply
                // the pre-guard one.
                libc::pthread_sigmask(libc::SIG_BLOCK, &blocked, &mut previous);
                Self { previous }
            }
        }
        #[cfg(not(unix))]
        Self {}
    }
}

#[cfg(unix)]
impl Drop for BlockedSigpipeGuard {
    fn drop(&mut self) {
        unsafe {
            // A write that ended in EPIPE raised SIGPIPE while it was
            // blocked here. Consume it before the restore — otherwise the
            // unblock delivers the pending signal at the process
            // disposition (SIG_DFL in production) and the guard's own Drop
            // kills the process it exists to protect (see the struct doc).
            let mut pending: libc::sigset_t = std::mem::zeroed();
            libc::sigpending(&mut pending);
            if libc::sigismember(&pending, libc::SIGPIPE) == 1 {
                let mut blocked: libc::sigset_t = std::mem::zeroed();
                libc::sigemptyset(&mut blocked);
                libc::sigaddset(&mut blocked, libc::SIGPIPE);
                let mut delivered: i32 = 0;
                // SAFETY: the signal is pending on this thread, so the
                // wait returns immediately; the sets are local stack
                // values.
                libc::sigwait(&blocked, &mut delivered);
            }
            libc::pthread_sigmask(libc::SIG_SETMASK, &self.previous, std::ptr::null_mut());
        }
    }
}

#[cfg(not(unix))]
impl Drop for BlockedSigpipeGuard {
    fn drop(&mut self) {}
}

/// Outcome of the bounded end-of-file delivery behind an explicit
/// `close_stdin` on a unix terminal task: `Delivered` means the ^D bytes
/// landed, `Failed` means the write errored without landing (typically the
/// child was already gone and the master write failed), and `Stalled` means
/// the tty input queue was still full at the deadline.
#[cfg(all(unix, not(target_env = "ohos")))]
#[derive(Debug)]
enum BoundedEofDelivery {
    Delivered,
    Failed,
    Stalled,
}

/// Deliver `\n` + VEOF through the given master fd on the same bounded
/// worker a payload write uses ([`bounded_stdin_write_owned`]). The leading
/// newline flushes any pending partial line, because canonical mode only
/// turns VEOF into a read of length zero from an EMPTY line buffer — the
/// same two bytes the old ownership box injected on drop. In raw mode both
/// bytes are ordinary input, exactly like that box drop; the caller records
/// a delivery only when the write really completed, so the flag never
/// claims an end-of-file that did not land. The EOF byte is written
/// literally (`0x04`) rather than read from `c_cc[VEOF]`: portable-pty's
/// unix writer reads the live byte at drop and writes nothing at all when
/// VEOF is disabled (`_POSIX_VDISABLE`), so the two behaviors differ in
/// that state — this close sends the two bytes as ordinary input, exactly
/// like raw mode, and no end-of-file ever arrives. The `close_stdin`
/// schema discloses that; the old box delivered nothing in the same
/// state, so the close still reports honestly what it did.
#[cfg(all(unix, not(target_env = "ohos")))]
fn bounded_stdin_eof_delivery(file: &std::fs::File) -> BoundedEofDelivery {
    use std::os::fd::AsFd;
    let dup_fd = match file.as_fd().try_clone_to_owned() {
        Ok(fd) => fd,
        // No clone means no worker can attempt the delivery; report it as
        // a failed delivery — a clone failure is a process-fd problem, not
        // a queue-full condition, and the flag must stay down either way.
        Err(_) => return BoundedEofDelivery::Failed,
    };
    match bounded_stdin_write_owned(
        StdinWriter::Pipe(std::process::ChildStdin::from(dup_fd)),
        b"\n\x04".to_vec(),
    ) {
        // The clone is dropped on every arm: the dup this function was
        // given stays with the task, and only the write's outcome decides
        // what the close may claim.
        Err(_) => BoundedEofDelivery::Failed,
        Ok((_, Ok(Ok(())))) => BoundedEofDelivery::Delivered,
        Ok((_, Ok(Err(_)))) => BoundedEofDelivery::Failed,
        Ok((_, Err(std::sync::mpsc::RecvTimeoutError::Timeout))) => BoundedEofDelivery::Stalled,
        Ok((_, Err(std::sync::mpsc::RecvTimeoutError::Disconnected))) => BoundedEofDelivery::Failed,
    }
}

/// The single bounded stdin write every platform uses: the worker thread
/// takes the writer by value, writes and flushes the payload, and hands the
/// writer back through the channel. Unix reaches it with a clone of the
/// task's handle — pipes clone the `ChildStdin` fd and the PTY lane clones
/// the dup'd master fd (see [`BackgroundShell::write_stdin`] and
/// [`bounded_stdin_eof_delivery`]); Windows and ohos cannot clone an fd out
/// of their handle, so the handoff moves the real writer.
/// `Ok((Some(writer), Ok(io_result)))` returns the writer to the caller.
/// `Ok((None, Err(RecvTimeoutError::Timeout)))` means the deadline hit
/// first: the writer stays with the abandoned worker, the bytes still land
/// if the command resumes reading, and the caller marks the stall.
/// `Err(RecvTimeoutError::Disconnected)` means the worker died before
/// completing (a panic in the write path): the writer is gone, and the
/// caller must NOT mark a stall — a writer failure is not the command's
/// fault. An `Err` from the function itself means the worker thread could
/// not be spawned at all: nothing was written, and the writer is gone with
/// the failed closure (ownership was already handed over), so the caller
/// must report the task as having no writable stdin. A plain
/// `thread::spawn` panic here would unwind through the shell-manager lock
/// and poison it for the rest of the process.
type OwnedBoundedWriteOutcome = (
    Option<StdinWriter>,
    Result<std::io::Result<()>, std::sync::mpsc::RecvTimeoutError>,
);

fn bounded_stdin_write_owned(
    writer: StdinWriter,
    payload: Vec<u8>,
) -> anyhow::Result<OwnedBoundedWriteOutcome> {
    let (sender, receiver) = std::sync::mpsc::channel();
    let worker = std::thread::Builder::new()
        .name("stdin-bounded-write".to_string())
        .spawn(move || {
            let _sigpipe = BlockedSigpipeGuard::new();
            let mut writer = writer;
            let result = writer.write_all(&payload).and_then(|_| writer.flush());
            // The writer moves back out only on the send path; a send failure
            // (receiver gone) drops it, which is fine — the caller already
            // gave up on the handle at that point.
            let _ = sender.send((writer, result));
        });
    let receiver = match worker {
        Ok(_) => receiver,
        Err(error) => {
            return Err(anyhow!(error).context("Failed to spawn the bounded stdin writer thread"));
        }
    };
    match receiver.recv_timeout(STDIN_WRITE_DEADLINE) {
        Ok((writer, result)) => Ok((Some(writer), Ok(result))),
        Err(error) => Ok((None, Err(error))),
    }
}

fn spawn_reader_thread<R: Read + Send + 'static>(
    mut reader: R,
    buffer: SharedRawOutput,
) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let mut chunk = [0u8; 4096];
        loop {
            match reader.read(&mut chunk) {
                Ok(0) => break,
                Ok(n) => {
                    // `RawOutputBuffer::append` enforces the in-flight ceiling
                    // here, at the only writer, so a chatty command cannot grow
                    // the process without bound while it runs (#5472). It
                    // returns false once the stream has been abandoned, which
                    // is this thread's only exit when a descendant holds the
                    // pipe open and EOF never arrives.
                    let keep_reading = buffer
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .append(&chunk[..n]);
                    if !keep_reading {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
    })
}

fn spawn_bounded_reader_thread<R: Read + Send + 'static>(
    mut reader: R,
    output: Arc<Mutex<BoundedOutputAccumulator>>,
) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let mut chunk = [0u8; 4096];
        loop {
            match reader.read(&mut chunk) {
                Ok(0) => break,
                Ok(n) => {
                    let mut guard = output.lock().unwrap_or_else(|error| error.into_inner());
                    if let Err(error) = guard.append(&chunk[..n]) {
                        guard.record_error(&error);
                        return;
                    }
                }
                Err(error) => {
                    output
                        .lock()
                        .unwrap_or_else(|poison| poison.into_inner())
                        .record_error(&error);
                    return;
                }
            }
        }
        let mut guard = output.lock().unwrap_or_else(|error| error.into_inner());
        if let Err(error) = guard.finish() {
            guard.record_error(&error);
        }
    })
}

#[cfg(unix)]
fn shared_output_pipe() -> io::Result<(File, File, File)> {
    let mut descriptors = [0; 2];
    // SAFETY: `pipe` initializes both descriptors on success. Each descriptor
    // is immediately transferred into exactly one owned `File`.
    if unsafe { libc::pipe(descriptors.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: successful `pipe` returned two live, uniquely owned descriptors.
    let reader = unsafe { File::from_raw_fd(descriptors[0]) };
    let writer = unsafe { File::from_raw_fd(descriptors[1]) };
    let stderr_writer = writer.try_clone()?;
    Ok((reader, writer, stderr_writer))
}

#[cfg(windows)]
fn shared_output_pipe() -> io::Result<(File, File, File)> {
    let mut read_handle = std::ptr::null_mut();
    let mut write_handle = std::ptr::null_mut();
    // SAFETY: CreatePipe initializes both handles on success; ownership is
    // transferred to `File` immediately below.
    if unsafe {
        windows_sys::Win32::System::Pipes::CreatePipe(
            &mut read_handle,
            &mut write_handle,
            std::ptr::null(),
            0,
        )
    } == 0
    {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: successful CreatePipe returned two live, uniquely owned handles.
    let reader = unsafe { File::from_raw_handle(read_handle.cast()) };
    let writer = unsafe { File::from_raw_handle(write_handle.cast()) };
    let stderr_writer = writer.try_clone()?;
    Ok((reader, writer, stderr_writer))
}

const SYNC_READER_DRAIN_TIMEOUT: Duration = Duration::from_secs(5);
const STALE_NO_OUTPUT_AFTER: Duration = Duration::from_secs(60);

/// Grace between SIGTERM and SIGKILL on the shell kill path (timeout,
/// cancel, drop). Bounded so a SIGTERM-ignoring command is force-killed
/// instead of stalling the tool (#52).
#[cfg(unix)]
const KILL_TERM_GRACE: Duration = Duration::from_millis(500);
/// Bounded reap wait after SIGKILL; a child stuck in uninterruptible sleep
/// must not wedge the caller behind an unbounded `wait`.
#[cfg(unix)]
const KILL_REAP_GRACE: Duration = Duration::from_millis(1_000);
/// Bounded join for output-reader threads after the process group is killed.
/// A descendant that escaped the group (its own session/process group) keeps
/// its inherited pipe write-end open, so the reader cannot see EOF until that
/// descendant exits on its own — an unbounded join held the shell-manager
/// lock for minutes and overshot the tool timeout (#52).
const READER_JOIN_GRACE: Duration = Duration::from_millis(2_000);

fn spawn_sync_reader_thread<R: Read + Send + 'static>(
    mut reader: R,
) -> std::sync::mpsc::Receiver<Vec<u8>> {
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        // Bounded, unlike the `read_to_end` this replaces (#5472 finding 2).
        // `recv_sync_reader_output` gives up after 5 s, but the thread lives as
        // long as the pipe does — an interactive command that keeps printing
        // grew this Vec without limit, for a result nobody was still waiting
        // for. The tail is what the caller renders, so keep the tail.
        let mut buf = RawOutputBuffer::new();
        let mut chunk = [0u8; 4096];
        loop {
            match reader.read(&mut chunk) {
                Ok(0) => break,
                Ok(n) => {
                    if !buf.append(&chunk[..n]) {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
        tx.send(buf.retained().to_vec()).ok();
    });
    rx
}

fn recv_sync_reader_output(rx: &std::sync::mpsc::Receiver<Vec<u8>>) -> Vec<u8> {
    rx.recv_timeout(SYNC_READER_DRAIN_TIMEOUT)
        .unwrap_or_default()
}

/// A background shell process being tracked
pub struct BackgroundShell {
    pub id: String,
    pub command: String,
    pub working_dir: PathBuf,
    pub status: ShellStatus,
    pub exit_code: Option<i64>,
    pub started_at: Instant,
    /// When the job reached a terminal status. A finished job reports the
    /// duration it finished with; without this, `started_at.elapsed()` kept
    /// growing and `/jobs` showed "2m 07s" for a 12-second command (#5478).
    finished_at: Option<Instant>,
    last_output_at: Instant,
    last_observed_output_len: usize,
    pub sandbox_type: SandboxType,
    pub linked_task_id: Option<String>,
    pub owner_agent: Option<ShellJobOwner>,
    owner_session_id: String,
    origin_tool_call_id: Option<String>,
    origin_turn_id: Option<String>,
    ownership: ShellOwnership,
    stdout_buffer: SharedRawOutput,
    stderr_buffer: Option<SharedRawOutput>,
    /// Lowercase `bash` streams one combined process pipe through a bounded
    /// small-contract-compatible accumulator while persisting the complete output.
    bounded_output: Option<Arc<Mutex<BoundedOutputAccumulator>>>,
    heavy_permit: Option<HeavyCommandPermit>,
    stdout_cursor: usize,
    stderr_cursor: usize,
    completion_reported: bool,
    stdin: Option<StdinWriter>,
    /// Set when the child has really been handed end-of-file: either its
    /// stdin pipe was closed through `write_stdin`, or an explicit unix
    /// terminal close delivered the ^D bytes through the bounded worker.
    /// Stays false when the task never exposed a writable stdin (persistent
    /// services spawn with a null stdin, PTY lanes keep the slave open
    /// through the reader), when the handle was simply released after
    /// completion, and when a close delivery failed or stalled — so
    /// `interact` can state the true reason stdin is unavailable.
    stdin_closed_at_eof: bool,
    /// Set when a stdin write outlived [`STDIN_WRITE_DEADLINE`] because the
    /// command stopped consuming its pipe, or when a terminal close's own
    /// ^D delivery stalled the same way (same deadline, same refusal
    /// family). The abandoned bytes keep flowing from the background writer
    /// if the child resumes, so further writes are refused (two writers
    /// could interleave) instead of queued behind an unknown pipe state.
    stdin_write_stalled: bool,
    child: Option<ShellChild>,
    /// A stop was delivered but the direct child's exit was never observed:
    /// the bounded reap grace elapsed first (e.g. uninterruptible sleep), so
    /// the child may still be alive and writing. Claim coordination must keep
    /// treating the job as a potential writer until a later poll reaps the
    /// child; `collect_output` therefore keeps the handle instead of dropping
    /// it while this is set.
    stop_unconfirmed: bool,
    #[cfg(windows)]
    windows_job: Option<WindowsJob>,
    stdout_thread: Option<std::thread::JoinHandle<()>>,
    stderr_thread: Option<std::thread::JoinHandle<()>>,
    work_lifecycle: Option<ShellWorkLifecycle>,
    lifecycle_seq: u64,
    last_lifecycle_status: Option<ShellStatus>,
    last_lifecycle_bytes: usize,
}

#[derive(Clone)]
struct ShellWorkLifecycle {
    work: SharedWorkRuntime,
    session_id: String,
}

impl ShellWorkLifecycle {
    fn register(&self, id: &str, command: &str) -> Result<()> {
        self.work
            .register_operation(
                &self.session_id,
                OperationIntent::new(
                    format!("shell:{id}"),
                    format!("Shell · {command}"),
                    false,
                    "exec_shell",
                    id,
                ),
            )
            .map(|_| ())
            .map_err(anyhow::Error::msg)
    }

    fn observe(&self, id: &str, status: &ShellStatus, seq: u64, raw_bytes: usize) -> Result<()> {
        let owner_state = match status {
            ShellStatus::Running => OwnerState::Running,
            ShellStatus::Completed => OwnerState::Completed,
            ShellStatus::Failed | ShellStatus::TimedOut => OwnerState::Failed,
            ShellStatus::Killed => OwnerState::Cancelled,
        };
        let raw_bytes = u64::try_from(raw_bytes).unwrap_or(u64::MAX);
        let output = EvidenceRef::new(
            EvidenceKind::Receipt {
                owner: "shell".to_string(),
            },
            format!("shell:{id}:output"),
            Some(raw_bytes),
            false,
        )
        .map_err(|err| anyhow!(err.to_string()))?;
        self.work
            .reconcile_operation(
                &self.session_id,
                OperationOwnerSnapshot::new(
                    format!("shell:{id}"),
                    owner_state,
                    seq,
                    lifecycle_now_ms(),
                )
                .with_output(output),
            )
            .map(|_| ())
            .map_err(anyhow::Error::msg)
    }
}

struct ShellSpawnIntentGuard {
    lifecycle: Option<ShellWorkLifecycle>,
    id: String,
    armed: bool,
}

struct ShellSpawnContext {
    owner_agent: Option<ShellJobOwner>,
    owner_session_id: String,
    origin_tool_call_id: Option<String>,
    origin_turn_id: Option<String>,
    work_lifecycle: Option<ShellWorkLifecycle>,
}

impl ShellSpawnIntentGuard {
    fn new(lifecycle: Option<ShellWorkLifecycle>, id: &str, command: &str) -> Result<Self> {
        if let Some(lifecycle) = lifecycle.as_ref() {
            lifecycle.register(id, command)?;
        }
        Ok(Self {
            lifecycle,
            id: id.to_string(),
            armed: true,
        })
    }

    fn disarm(&mut self) {
        self.armed = false;
    }
}

impl Drop for ShellSpawnIntentGuard {
    fn drop(&mut self) {
        if self.armed
            && let Some(lifecycle) = self.lifecycle.as_ref()
            && let Err(err) = lifecycle.observe(&self.id, &ShellStatus::Failed, 1, 0)
        {
            tracing::warn!(shell_id = %self.id, error = %err, "failed to record shell spawn failure");
        }
    }
}

impl BackgroundShell {
    /// Wall time to report: elapsed while running, frozen once finished.
    fn wall_duration(&self) -> Duration {
        self.finished_at
            .unwrap_or_else(Instant::now)
            .saturating_duration_since(self.started_at)
    }

    fn wall_millis(&self) -> u64 {
        u64::try_from(self.wall_duration().as_millis()).unwrap_or(u64::MAX)
    }

    /// Stamp the finish instant the first time a terminal status is observed.
    /// Idempotent: a later poll must not restate when the job ended.
    fn mark_finished(&mut self) {
        if self.finished_at.is_none() && self.status != ShellStatus::Running {
            self.finished_at = Some(Instant::now());
        }
    }

    /// Check if the process has completed and update status
    fn poll(&mut self) -> bool {
        self.refresh_output_activity();
        if self.status != ShellStatus::Running {
            self.mark_finished();
            self.publish_lifecycle_best_effort();
            return true;
        }

        #[cfg(unix)]
        let pending_process_group = (self.ownership == ShellOwnership::PersistPending)
            .then(|| self.child.as_ref().and_then(ShellChild::process_id))
            .flatten();
        let completed = if let Some(ref mut child) = self.child {
            match child.try_wait() {
                Ok(Some(status)) => {
                    self.exit_code = status.code;
                    self.status = if status.success {
                        ShellStatus::Completed
                    } else {
                        ShellStatus::Failed
                    };
                    self.heavy_permit.take();
                    self.collect_output();
                    true
                }
                Ok(None) => false, // Still running
                Err(_) => {
                    self.status = ShellStatus::Failed;
                    self.heavy_permit.take();
                    self.collect_output();
                    true
                }
            }
        } else {
            true
        };
        #[cfg(unix)]
        if completed && let Some(process_group_id) = pending_process_group {
            unregister_pending_persistent_process_group(process_group_id);
        }
        self.mark_finished();
        self.publish_lifecycle_best_effort();
        completed
    }

    /// Reap an unconfirmed stop once the child has actually exited. Bounded:
    /// a single `try_wait`, so a still-uninterruptible child keeps its
    /// uncertainty and is re-asked on the next observation.
    fn confirm_stop_when_reaped(&mut self) {
        if !self.stop_unconfirmed {
            return;
        }
        let confirmed = match self.child.as_mut() {
            Some(ShellChild::Process(child)) => matches!(child.try_wait(), Ok(Some(_)) | Err(_)),
            // The PTY stop waits synchronously, and a dropped handle leaves
            // nothing observable to hold the frame for.
            _ => true,
        };
        if confirmed {
            self.stop_unconfirmed = false;
        }
    }

    /// Whether the job still occupies its write scope: running, or stopped
    /// with the direct child's exit unconfirmed. "Signal delivered" and
    /// "exit observed" are different states, and claim coordination may only
    /// hand the scope to a successor once the latter holds.
    fn holds_write_scope(&self) -> bool {
        self.status == ShellStatus::Running || self.stop_unconfirmed
    }

    fn publish_lifecycle(&mut self) -> Result<()> {
        let bytes = self.observed_output_len();
        if self.last_lifecycle_status.as_ref() == Some(&self.status)
            && self.last_lifecycle_bytes == bytes
        {
            return Ok(());
        }
        let next_seq = self.lifecycle_seq.saturating_add(1);
        if let Some(lifecycle) = self.work_lifecycle.as_ref() {
            lifecycle.observe(&self.id, &self.status, next_seq, bytes)?;
        }
        self.lifecycle_seq = next_seq;
        self.last_lifecycle_status = Some(self.status.clone());
        self.last_lifecycle_bytes = bytes;
        Ok(())
    }

    fn publish_lifecycle_best_effort(&mut self) {
        if let Err(err) = self.publish_lifecycle() {
            tracing::warn!(shell_id = %self.id, error = %err, "failed to reconcile shell lifecycle");
        }
    }

    fn refresh_output_activity(&mut self) {
        let observed_len = self.observed_output_len();
        if observed_len != self.last_observed_output_len {
            self.last_observed_output_len = observed_len;
            self.last_output_at = Instant::now();
        }
    }

    fn observed_output_len(&self) -> usize {
        if let Some(output) = self.bounded_output.as_ref() {
            return output
                .lock()
                .map(|output| output.total_bytes())
                .unwrap_or(0);
        }
        let stdout_len = self
            .stdout_buffer
            .lock()
            .map(|data| data.total_len())
            .unwrap_or(0);
        let stderr_len = self
            .stderr_buffer
            .as_ref()
            .and_then(|buffer| buffer.lock().ok().map(|data| data.total_len()))
            .unwrap_or(0);
        stdout_len.saturating_add(stderr_len)
    }

    /// Drop everything but a bounded tail of both raw streams.
    ///
    /// Called only once a job is terminal **and** its bytes have already been
    /// delivered — either returned as the foreground tool result, or written to
    /// its durable completion artifact. Before that the full bytes are a
    /// contract; after it they are pure residency for the up-to-1 h the
    /// finished record stays listed, which is what took the owner's host to
    /// 11 GB of swap (#5472 finding 1).
    fn release_delivered_output(&mut self) {
        if self.status == ShellStatus::Running {
            return;
        }
        for buffer in [Some(&self.stdout_buffer), self.stderr_buffer.as_ref()]
            .into_iter()
            .flatten()
        {
            buffer
                .lock()
                .unwrap_or_else(|poison| poison.into_inner())
                .release_to_tail(RAW_STREAM_SETTLED_TAIL_BYTES);
        }
    }

    /// Bytes still held in memory for this job, for the eviction accounting in
    /// [`ShellManager::cleanup`] and for tests that assert the bound.
    fn retained_output_bytes(&self) -> usize {
        let stdout = self
            .stdout_buffer
            .lock()
            .map(|data| data.retained().len())
            .unwrap_or(0);
        let stderr = self
            .stderr_buffer
            .as_ref()
            .and_then(|buffer| buffer.lock().ok().map(|data| data.retained().len()))
            .unwrap_or(0);
        stdout.saturating_add(stderr)
    }

    /// Collect output from the background threads
    fn collect_output(&mut self) {
        // Kill the whole process group before joining reader threads.
        // When the shell spawned persistent background jobs (e.g. `nohup curl`),
        // those subprocesses keep the pipe write-ends open after the shell exits.
        // Without this kill, the reader join would block until the descendant
        // exits, freezing the UI event loop that calls list_jobs() → poll() →
        // collect_output(). The joins themselves are additionally bounded
        // (READER_JOIN_GRACE) because a descendant in its own session/process
        // group escapes even the group kill (#52).
        #[cfg(unix)]
        if let Some(child) = self.child.as_mut() {
            match child {
                ShellChild::Process(proc) => {
                    let _ = kill_child_process_group(proc);
                }
                #[cfg(not(target_env = "ohos"))]
                ShellChild::Pty(_) => {}
            }
        }
        #[cfg(windows)]
        terminate_and_close_windows_job(self.windows_job.take());
        if let Some(handle) = self.stdout_thread.take() {
            finish_background_reader(handle, &self.status, Some(&self.stdout_buffer));
        }
        if let Some(handle) = self.stderr_thread.take() {
            finish_background_reader(handle, &self.status, self.stderr_buffer.as_ref());
        }
        self.stdin = None;
        if !self.stop_unconfirmed {
            self.child = None;
        }
    }

    /// Close the stdin pipe of this task, keeping a PTY writer attached —
    /// the foreground close: pipe readers get end-of-file at once, while a
    /// terminal command keeps the interact surface a session member can
    /// write to while it runs. Only an unstalled PIPE is claimed as
    /// end-of-file: after a stalled write the abandoned worker still holds
    /// a clone of the handle and the bytes have not finished landing, so
    /// the flag stays down and refusals report the stall instead — the
    /// same conjunct the `write_stdin` close applies.
    fn close_stdin_pipe(&mut self) {
        if matches!(self.stdin, Some(StdinWriter::Pipe(_))) {
            self.stdin_closed_at_eof = !self.stdin_write_stalled;
            self.stdin = None;
        }
    }

    fn write_stdin(&mut self, input: &str, close: bool) -> Result<()> {
        // Taken owned so the Windows/ohos fallback can move the writer into
        // its bounded worker and hand it back. Every unix exit restores the
        // slot first, keeping the stall/eof refusal ordering identical with
        // the pinned unix behavior; on the ownership-fallback cfgs the
        // Timeout, Disconnected, and spawn-failure arms deliberately leave
        // the slot empty — the writer is gone with the worker, and the stall
        // flag or the returned refusal reports that.
        // `mut` is real only on the ownership-fallback cfgs (they re-take and
        // reassign the slot); the unix fd-clone path only moves it, so the
        // same predicate that strips the fallback also silences `unused_mut`.
        #[cfg_attr(all(unix, not(target_env = "ohos")), allow(unused_mut))]
        let mut slot = self.stdin.take();
        if slot.is_some() {
            if !input.is_empty() {
                if self.stdin_write_stalled {
                    self.stdin = slot;
                    return Err(anyhow!(
                        "stdin writes to task {} are refused: an earlier write did not complete within {STDIN_WRITE_DEADLINE:?} because the command stopped consuming its stdin; the abandoned bytes still land if it resumes. Kill the task and start a new one instead of feeding it further.",
                        self.id
                    ));
                }
                // A blocking write under the shell-manager lock wedges the
                // executor thread and every other shell in the session when
                // the stdin handle's buffer is full and the command never
                // reads (the exact stall the inline-payload cap exists to
                // close on the run boundary — interact chunks and
                // small-buffer platforms hit the same wall one chunk later).
                // Every platform bounds the write now, by what its handle
                // offers: on unix the write is bounded for BOTH handle kinds
                // by fd clone — pipes clone the `ChildStdin` fd, and the PTY
                // lane clones the dup'd master fd it spawned with — a worker
                // thread owns the clone and finishes the payload in the
                // background (it still lands if the child resumes), the
                // caller learns about the stall at the deadline, and the
                // stall flag refuses any further write so two writers can
                // never interleave on one handle.
                // The same predicate as the `PtyFile` variant. ohos strips
                // this whole statement (the attribute gates the if-else), so
                // ohos takes the owned-writer fallback below — that
                // predicate is what keeps ohos compiling with a live write
                // and no dead binding, field, or methods under the workspace
                // deny(warnings).
                #[cfg(all(unix, not(target_env = "ohos")))]
                if let Some(writer) = slot.as_ref() {
                    // Exhaustive by construction: on this cfg `StdinWriter`
                    // has exactly `Pipe` and `PtyFile`, so a future third
                    // unix variant fails to COMPILE at the match below
                    // instead of silently falling into an unbounded blocking
                    // write under the session lock. The `else` arm only
                    // covers the `None` slot the take site already ruled
                    // out.
                    use std::os::fd::AsFd;
                    let dup_fd = match writer {
                        StdinWriter::Pipe(pipe) => pipe.as_fd().try_clone_to_owned(),
                        StdinWriter::PtyFile(file) => file.as_fd().try_clone_to_owned(),
                    };
                    let dup_fd = match dup_fd {
                        Ok(fd) => fd,
                        Err(error) => {
                            // The clone failed without consuming the
                            // handle: restore it so the task keeps its
                            // interact surface instead of later refusals
                            // misreporting a live task as "a finished
                            // command has released its stdin".
                            self.stdin = slot;
                            return Err(anyhow!(error).context(
                                "Failed to clone the stdin handle for the bounded write",
                            ));
                        }
                    };
                    // The bounded write is the shared helper (the same one
                    // the owned platforms and the ^D close delivery use):
                    // the clone rides it by value, so only the ORIGINAL
                    // slot decides what is restored below — a returned
                    // clone is dropped, never reattached.
                    match bounded_stdin_write_owned(
                        StdinWriter::Pipe(std::process::ChildStdin::from(dup_fd)),
                        input.as_bytes().to_vec(),
                    ) {
                        // The clone came back with a completed write: drop
                        // it and keep the original attached.
                        Ok((_, Ok(Ok(())))) => {}
                        Ok((_, Ok(Err(error)))) => {
                            // The worker completed: restore the ORIGINAL
                            // handle so a write error does not silently
                            // strip the task's interact surface (the base
                            // behavior kept the handle attached after a
                            // write error too).
                            self.stdin = slot;
                            return Err(anyhow!(error).context("Failed to write to stdin"));
                        }
                        // A channel close without a send means the worker
                        // died before completing (a panic in the write
                        // path): that is a writer failure, not a stall, so
                        // the task is not marked stalled — a healthy write
                        // after this must not be refused by the flag.
                        Ok((_, Err(std::sync::mpsc::RecvTimeoutError::Disconnected))) => {
                            self.stdin = slot;
                            return Err(anyhow!(stdin_writer_failed_refusal(&self.id)));
                        }
                        Ok((_, Err(std::sync::mpsc::RecvTimeoutError::Timeout))) => {
                            // The clone stays with the abandoned worker;
                            // the original handle is restored, and the
                            // stall flag refuses any further write so two
                            // writers can never interleave on one handle.
                            self.stdin_write_stalled = true;
                            self.stdin = slot;
                            return Err(anyhow!(stdin_stalled_write_refusal(&self.id)));
                        }
                        Err(error) => {
                            // The spawn failed before any byte was written
                            // and the worker holds nothing: restore the
                            // handle and fail like a plain write error. A
                            // bare `thread::spawn` panic here would unwind
                            // through the shell-manager mutex and poison it
                            // for the rest of the process.
                            self.stdin = slot;
                            return Err(anyhow!(error)
                                .context("Failed to spawn the bounded stdin writer thread"));
                        }
                    }
                } else {
                    // The `None` slot the take site already ruled out:
                    // no handle to write through, nothing to restore.
                }
                // Windows and ohos cannot clone an fd out of their handle
                // (Windows strips the statement above because `unix` is
                // false, ohos by the `not(target_env = "ohos")` half — the
                // portable-pty writer box exposes no cloneable handle at
                // all). They bound by ownership instead: the worker takes
                // the writer, finishes the payload, and hands the writer
                // back through the channel; a deadline timeout leaves the
                // writer with the abandoned worker (the bytes still land if
                // the command resumes), marks the stall, and the caller
                // returns with the session's shell lock released — cancel,
                // kill, and wait all stay live. This is the same wedge the
                // unix comment above describes, closed here too.
                #[cfg(any(target_env = "ohos", not(unix)))]
                {
                    let writer = slot.take().expect("slot held above");
                    let (returned, result) = bounded_stdin_write_owned(
                        writer,
                        input.as_bytes().to_vec(),
                    )
                    .map_err(|error| {
                        // The worker could not be spawned and the writer
                        // went with the failed closure: `slot` stays None,
                        // so the task honestly reports no writable stdin
                        // from here on.
                        anyhow!(error).context("Failed to spawn the bounded stdin writer thread")
                    })?;
                    match result {
                        Ok(Ok(())) => {
                            slot = returned;
                        }
                        Ok(Err(error)) => {
                            // The worker completed and handed the writer
                            // back: keep it attached so a write error does
                            // not silently strip the task's interact
                            // surface (same as the unix path above).
                            self.stdin = returned;
                            return Err(anyhow!(error).context("Failed to write to stdin"));
                        }
                        Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                            self.stdin_write_stalled = true;
                            // `slot` stays None: the abandoned worker owns
                            // the handle, so the close below cannot claim
                            // EOF and further writes hit the stall refusal.
                            return Err(anyhow!(stdin_stalled_write_refusal(&self.id)));
                        }
                        Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                            // The worker died before completing (a panic in
                            // the write path): that is a writer failure, not
                            // a stall — the task is not marked stalled so a
                            // healthy write after this is not refused, and
                            // the writer is gone with the worker (nothing
                            // to restore).
                            return Err(anyhow!(stdin_writer_failed_refusal(&self.id)));
                        }
                    }
                }
            }
            if close {
                // Only a dropped PIPE delivers end-of-file. An explicit
                // close on a unix PTY writer must DELIVER it instead: the
                // old ownership box injected `\n` + VEOF on drop (a `cat`
                // on a pty exited 0 the instant it dropped), and the dup
                // the terminal lane now holds delivers nothing on its own
                // because the reader holds the master — so write the same
                // ^D semantics through the dup, then drop it. The delivery
                // runs on the same kind of bounded worker as a payload
                // write: two bytes into a tty whose input queue is already
                // full (the earlier payload completed inside the deadline,
                // so the stall flag is still down) block just as forever
                // as a large payload would, and inline under the manager
                // lock they wedge every shell in the session.
                #[cfg(all(unix, not(target_env = "ohos")))]
                if let Some(StdinWriter::PtyFile(file)) = slot.as_ref() {
                    if !self.stdin_write_stalled {
                        match bounded_stdin_eof_delivery(file) {
                            BoundedEofDelivery::Delivered => {
                                self.stdin = None;
                                self.stdin_closed_at_eof = true;
                                return Ok(());
                            }
                            // The close consumed the handle as requested,
                            // but the bytes did not land (typically the
                            // child was already gone and the master write
                            // failed): keep the flag down so later refusals
                            // do not claim readers saw end-of-file. Ok is
                            // honest here — the release the caller asked
                            // for happened, and no delivery is possible
                            // once the child is gone.
                            BoundedEofDelivery::Failed => {
                                self.stdin = None;
                                return Ok(());
                            }
                            // Queue still full at the deadline: restore the
                            // handle under the stall flag and report the
                            // close as FAILED. The schema's close promises a
                            // delivered end-of-file, so a close that neither
                            // delivered nor released must not read as
                            // success — the same disposition the
                            // payload-timeout arm applies.
                            BoundedEofDelivery::Stalled => {
                                self.stdin_write_stalled = true;
                                self.stdin = slot;
                                return Err(anyhow!(stdin_stalled_close_refusal(&self.id)));
                            }
                        }
                    }
                    // A task whose last write stalled gets no delivery
                    // attempt at all: the queue that stalled the payload
                    // would stall the two bytes too, so the close is
                    // refused without waiting and without destroying the
                    // interact surface — the model is told the close was
                    // not applied instead of reading a success that
                    // delivered nothing.
                    self.stdin = slot;
                    return Err(anyhow!(stdin_stalled_close_not_applied_refusal(&self.id)));
                }
                // Windows cannot deliver end-of-file to a console child at
                // all (portable-pty's ConPTY writer has no EOF-on-drop —
                // that exists only in its unix writer), so an explicit
                // close here only releases the handle. Say so instead of
                // returning a silent success that the schema's "Close
                // stdin" would read as a delivered end-of-file.
                #[cfg(all(not(unix), not(target_env = "ohos")))]
                if matches!(slot, Some(StdinWriter::Pty(_))) {
                    self.stdin = None;
                    return Err(anyhow!(
                        "stdin close for task {} released the terminal's stdin handle, but this platform cannot deliver end-of-file to a console child; the command keeps running until it finishes or its timeout ends it.",
                        self.id
                    ));
                }
                // A task whose last write STALLED must not claim EOF: the
                // abandoned worker still holds a clone of the handle and
                // the bytes have not finished landing, so the flag stays
                // down and the tail refusal reports the stall instead.
                if let Some(StdinWriter::Pipe(_)) = slot {
                    self.stdin_closed_at_eof = !self.stdin_write_stalled;
                }
                self.stdin = None;
            } else {
                self.stdin = slot;
            }
            return Ok(());
        }

        if input.is_empty() && close {
            return Ok(());
        }

        if self.stdin_write_stalled {
            return Err(anyhow!(
                "stdin is not available for task {}: its last write stalled because the command stopped consuming stdin, and the abandoned writer may still hold the handle; kill it and start a new one.",
                self.id
            ));
        }

        if self.stdin_closed_at_eof {
            return Err(anyhow!(
                "stdin is not available for task {}: its stdin was closed, so stdin readers have seen end-of-file (a program that put its terminal into raw mode — or disabled or remapped VEOF — consumes those bytes as ordinary input instead); start input-driven work with background=true and feed it with action=\"interact\"",
                self.id
            ));
        }
        Err(anyhow!(
            "stdin is not available for task {}: no writable stdin handle is attached to it (a finished command has released its stdin, a persistent command never exposes one, a terminal command exposes one only while it runs and only until its stdin is closed, and a bounded stdin writer that failed to spawn or panicked takes its handle with it); start input-driven work with background=true and feed it with action=\"interact\"",
            self.id
        ))
    }

    fn full_output(&self) -> (String, String, usize, usize) {
        if let Some(snapshot) = self.bounded_output_snapshot(false).ok().flatten() {
            return (snapshot.content, String::new(), snapshot.total_bytes, 0);
        }
        let (stdout_bytes, stderr_bytes, stdout_omitted, stderr_omitted) =
            self.retained_output_bytes_with_omissions();
        // Report what the stream produced, not what is still held.
        let stdout_len = stdout_bytes.len().saturating_add(stdout_omitted);
        let stderr_len = stderr_bytes.len().saturating_add(stderr_omitted);

        (
            String::from_utf8_lossy(&stdout_bytes).to_string(),
            String::from_utf8_lossy(&stderr_bytes).to_string(),
            stdout_len,
            stderr_len,
        )
    }

    /// Retained bytes for both streams plus how many leading bytes the memory
    /// bound discarded. Callers that publish these bytes as evidence must
    /// declare the omission rather than presenting a clipped stream as exact.
    fn retained_output_bytes_with_omissions(&self) -> (Vec<u8>, Vec<u8>, usize, usize) {
        if let Some(snapshot) = self.bounded_output_snapshot(false).ok().flatten() {
            let omitted = snapshot.total_bytes.saturating_sub(snapshot.retained_bytes);
            return (snapshot.content.into_bytes(), Vec::new(), omitted, 0);
        }
        let (stdout_bytes, stdout_omitted) = self
            .stdout_buffer
            .lock()
            .map(|data| (data.retained().to_vec(), data.dropped()))
            .unwrap_or_default();
        let (stderr_bytes, stderr_omitted) = self
            .stderr_buffer
            .as_ref()
            .and_then(|buffer| {
                buffer
                    .lock()
                    .ok()
                    .map(|data| (data.retained().to_vec(), data.dropped()))
            })
            .unwrap_or_default();
        (stdout_bytes, stderr_bytes, stdout_omitted, stderr_omitted)
    }

    fn take_delta(&mut self) -> (String, String, usize, usize, usize, usize) {
        if let Some(snapshot) = self.bounded_output_snapshot(false).ok().flatten() {
            let changed = snapshot.total_bytes != self.stdout_cursor;
            self.stdout_cursor = snapshot.total_bytes;
            if changed {
                self.last_output_at = Instant::now();
                self.last_observed_output_len = snapshot.total_bytes;
                let delta_len = snapshot.content.len();
                return (
                    snapshot.content,
                    String::new(),
                    delta_len,
                    0,
                    snapshot.total_bytes,
                    0,
                );
            }
            return (String::new(), String::new(), 0, 0, snapshot.total_bytes, 0);
        }
        let (stdout_delta, stdout_total) =
            take_delta_from_buffer(&self.stdout_buffer, &mut self.stdout_cursor);
        let (stderr_delta, stderr_total) = if let Some(buffer) = self.stderr_buffer.as_ref() {
            take_delta_from_buffer(buffer, &mut self.stderr_cursor)
        } else {
            (Vec::new(), 0)
        };

        let stdout_delta_len = stdout_delta.len();
        let stderr_delta_len = stderr_delta.len();

        if stdout_delta_len > 0 || stderr_delta_len > 0 {
            self.last_output_at = Instant::now();
            self.last_observed_output_len = stdout_total.saturating_add(stderr_total);
        }

        (
            String::from_utf8_lossy(&stdout_delta).to_string(),
            String::from_utf8_lossy(&stderr_delta).to_string(),
            stdout_delta_len,
            stderr_delta_len,
            stdout_total,
            stderr_total,
        )
    }

    fn sandbox_denied(&self) -> bool {
        if matches!(self.status, ShellStatus::Running) {
            return false;
        }
        let (_, stderr_full, _, _) = self.full_output();
        SandboxManager::was_denied(
            self.sandbox_type,
            self.exit_code
                .and_then(|code| i32::try_from(code).ok())
                .unwrap_or(-1),
            &stderr_full,
        )
    }

    /// Kill the process
    fn kill(&mut self) -> Result<()> {
        #[cfg(unix)]
        if self.ownership == ShellOwnership::PersistPending
            && let Some(process_group_id) = self.child.as_ref().and_then(ShellChild::process_id)
        {
            unregister_pending_persistent_process_group(process_group_id);
        }
        if let Some(ref mut child) = self.child {
            match child {
                ShellChild::Process(proc) => {
                    #[cfg(windows)]
                    {
                        terminate_windows_job(self.windows_job.as_ref(), proc)
                            .context("Failed to kill process tree")?;
                        let _ = proc.wait();
                    }
                    #[cfg(all(not(windows), unix))]
                    {
                        // Bounded SIGTERM → SIGKILL escalation against the
                        // whole process group; returns within ~grace even if
                        // the command ignores SIGTERM (#52). A false return
                        // means the stop was sent but the direct child's exit
                        // is unconfirmed — record that so coordination keeps
                        // treating the job as a potential writer until a
                        // later poll observes the exit.
                        let reaped = terminate_child_process_group(proc)
                            .context("Failed to kill process")?;
                        self.stop_unconfirmed = !reaped;
                    }
                    #[cfg(all(not(windows), not(unix)))]
                    {
                        proc.kill().context("Failed to kill process")?;
                        let _ = proc.wait();
                    }
                }
                #[cfg(not(target_env = "ohos"))]
                ShellChild::Pty(child) => {
                    child.kill().context("Failed to kill process")?;
                    let _ = child.wait();
                }
            }
        }
        self.status = ShellStatus::Killed;
        self.mark_finished();
        self.heavy_permit.take();
        self.collect_output();
        self.publish_lifecycle_best_effort();
        Ok(())
    }

    /// Get a snapshot of the current state
    #[allow(dead_code)]
    pub fn snapshot(&self) -> Result<ShellResult> {
        let sandboxed = !matches!(self.sandbox_type, SandboxType::None);
        if let Some(snapshot) = self.bounded_output_snapshot(self.status != ShellStatus::Running)? {
            return Ok(ShellResult {
                task_id: Some(self.id.clone()),
                status: self.status.clone(),
                exit_code: self.exit_code,
                stdout: snapshot.content,
                stderr: String::new(),
                duration_ms: self.wall_millis(),
                stdout_len: snapshot.total_bytes,
                stderr_len: 0,
                stdout_omitted: snapshot.total_bytes.saturating_sub(snapshot.retained_bytes),
                stderr_omitted: 0,
                stdout_truncated: snapshot.truncated,
                stderr_truncated: false,
                sandboxed,
                sandbox_type: sandboxed.then(|| self.sandbox_type.to_string()),
                sandbox_denied: false,
            });
        }
        let (stdout_full, stderr_full, stdout_total, stderr_total) = self.full_output();
        let (stdout, stdout_meta) = truncate_with_meta(&stdout_full);
        let (stderr, stderr_meta) = truncate_with_meta(&stderr_full);
        // `truncate_with_meta` can only see the bytes still held. Fold in what
        // the in-memory bound dropped so a >16 MiB stream reports its real
        // length and its real omission instead of silently shrinking (#5472).
        let stdout_dropped = stdout_total.saturating_sub(stdout_meta.original_len);
        let stderr_dropped = stderr_total.saturating_sub(stderr_meta.original_len);
        Ok(ShellResult {
            task_id: Some(self.id.clone()),
            status: self.status.clone(),
            exit_code: self.exit_code,
            stdout,
            stderr,
            duration_ms: self.wall_millis(),
            stdout_len: stdout_total,
            stderr_len: stderr_total,
            stdout_omitted: stdout_meta.omitted.saturating_add(stdout_dropped),
            stderr_omitted: stderr_meta.omitted.saturating_add(stderr_dropped),
            stdout_truncated: stdout_meta.truncated || stdout_dropped > 0,
            stderr_truncated: stderr_meta.truncated || stderr_dropped > 0,
            sandboxed,
            sandbox_type: if sandboxed {
                Some(self.sandbox_type.to_string())
            } else {
                None
            },
            sandbox_denied: self.sandbox_denied(),
        })
    }

    fn bounded_output_snapshot(&self, finalize: bool) -> Result<Option<BoundedOutputSnapshot>> {
        self.bounded_output
            .as_ref()
            .map(|output| {
                output
                    .lock()
                    .unwrap_or_else(|error| error.into_inner())
                    .snapshot(finalize)
                    .map_err(anyhow::Error::from)
            })
            .transpose()
    }

    fn job_snapshot(&self) -> ShellJobSnapshot {
        // Use tail_from_buffer instead of full_output so we never clone the
        // entire accumulated stdout/stderr for display purposes.  full_output
        // is O(total_bytes_written), which caused the ShellManager mutex to be
        // held for an arbitrarily long time during list_jobs() calls from the
        // TUI event loop — freezing input handling on long automation runs.
        let (stdout_len, stdout_tail) =
            if let Some(snapshot) = self.bounded_output_snapshot(false).ok().flatten() {
                (snapshot.total_bytes, tail_text(&snapshot.content, 1_200))
            } else {
                tail_from_buffer(&self.stdout_buffer, 1200)
            };
        let (stderr_len, stderr_tail) = self
            .stderr_buffer
            .as_ref()
            .map(|buf| tail_from_buffer(buf, 1200))
            .unwrap_or((0, String::new()));
        let elapsed_since_output_ms = (self.status == ShellStatus::Running)
            .then(|| u64::try_from(self.last_output_at.elapsed().as_millis()).unwrap_or(u64::MAX));
        let stale = elapsed_since_output_ms.is_some_and(|elapsed| {
            elapsed >= u64::try_from(STALE_NO_OUTPUT_AFTER.as_millis()).unwrap_or(u64::MAX)
        });
        ShellJobSnapshot {
            id: self.id.clone(),
            job_id: self.id.clone(),
            command: self.command.clone(),
            cwd: self.working_dir.clone(),
            status: self.status.clone(),
            exit_code: self.exit_code,
            elapsed_ms: self.wall_millis(),
            stdout_tail,
            stderr_tail,
            stdout_len,
            stderr_len,
            stdin_available: self.stdin.is_some() && self.status == ShellStatus::Running,
            stale,
            elapsed_since_output_ms,
            linked_task_id: self.linked_task_id.clone(),
            owner_agent_id: self
                .owner_agent
                .as_ref()
                .map(|owner| owner.agent_id.clone()),
            owner_agent_name: self
                .owner_agent
                .as_ref()
                .map(|owner| owner.agent_name.clone()),
            origin_tool_call_id: self.origin_tool_call_id.clone(),
            origin_turn_id: self.origin_turn_id.clone(),
            owner_session_id: self.owner_session_id.clone(),
        }
    }

    fn completion_event(&self) -> ShellCompletionEvent {
        let snapshot = self.job_snapshot();
        let (stdout_len, stdout_tail) =
            if let Some(output) = self.bounded_output_snapshot(false).ok().flatten() {
                (
                    output.total_bytes,
                    tail_text(&output.content, SHELL_COMPLETION_TAIL_BYTES),
                )
            } else {
                bounded_completion_tail(&self.stdout_buffer, SHELL_COMPLETION_TAIL_BYTES)
            };
        let (stderr_len, stderr_tail) = self
            .stderr_buffer
            .as_ref()
            .map(|buffer| bounded_completion_tail(buffer, SHELL_COMPLETION_TAIL_BYTES))
            .unwrap_or((0, String::new()));
        ShellCompletionEvent {
            task_id: snapshot.id,
            command: snapshot.command,
            status: snapshot.status,
            exit_code: snapshot.exit_code,
            duration_ms: snapshot.elapsed_ms,
            stdout_tail,
            stderr_tail,
            stdout_len,
            stderr_len,
            evidence_ref: None,
            linked_task_id: snapshot.linked_task_id,
            owner_agent_id: snapshot.owner_agent_id,
            owner_agent_name: snapshot.owner_agent_name,
            origin_tool_call_id: snapshot.origin_tool_call_id,
            origin_turn_id: snapshot.origin_turn_id,
            owner_session_id: snapshot.owner_session_id,
        }
    }

    fn completion_evidence(&self) -> ShellCompletionEvidence {
        let event = self.completion_event();
        let (stdout, stderr, stdout_omitted, stderr_omitted) =
            self.retained_output_bytes_with_omissions();
        ShellCompletionEvidence {
            event,
            stdout,
            stderr,
            stdout_omitted,
            stderr_omitted,
        }
    }

    fn job_detail(&self) -> ShellJobDetail {
        let (stdout, stderr, _, _) = self.full_output();
        ShellJobDetail {
            snapshot: self.job_snapshot(),
            stdout,
            stderr,
        }
    }
}

fn finish_background_reader(
    handle: std::thread::JoinHandle<()>,
    status: &ShellStatus,
    buffer: Option<&SharedRawOutput>,
) {
    // A killed Windows process can leave a pipe reader blocked even after its
    // Job Object has been closed. Cancellation must return promptly instead of
    // waiting for that reader to observe EOF. Other terminal states still join
    // so their final output is collected before the shell is discarded.
    #[cfg(windows)]
    if *status == ShellStatus::Killed {
        drop(handle);
        return;
    }

    #[cfg(not(windows))]
    let _ = status;

    // Bounded join (#52): after the process group is killed the reader
    // normally sees EOF immediately, but a descendant that escaped the group
    // (its own session/process group) keeps its inherited pipe write-end
    // open, so the reader stays blocked until that descendant exits on its
    // own. Joining unboundedly froze the foreground shell — and, through the
    // shell-manager lock, every other shell — for minutes. On timeout the
    // join is handed to a helper thread and we return; the reader thread
    // still finishes on its own once the pipe finally closes.
    let (done_tx, done_rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = handle.join();
        let _ = done_tx.send(());
    });
    if done_rx.recv_timeout(READER_JOIN_GRACE).is_ok() {
        return;
    }
    // The reader is still blocked in `read()` on a pipe a descendant refuses to
    // close. Previously both it and the helper thread above stayed alive for the
    // life of the process, the reader still appending into a buffer nobody would
    // ever read (#5472 finding 2). Abandoning the stream releases what it holds
    // and gives the reader an exit on its next wakeup, which also lets the
    // helper's `join` return.
    if let Some(buffer) = buffer {
        buffer
            .lock()
            .unwrap_or_else(|poison| poison.into_inner())
            .abandon();
    }
}

impl Drop for BackgroundShell {
    fn drop(&mut self) {
        #[cfg(unix)]
        if self.ownership == ShellOwnership::PersistPending
            && let Some(process_group_id) = self.child.as_ref().and_then(ShellChild::process_id)
        {
            unregister_pending_persistent_process_group(process_group_id);
        }
        // A scope holder is destroyed only when the whole manager goes away
        // (or a future eviction path regresses): the unconfirmed child may
        // still be alive behind the retained handle, so give it the same
        // final bounded stop a Running job gets instead of dropping it
        // silently.
        if self.ownership != ShellOwnership::Released
            && (self.status == ShellStatus::Running || self.stop_unconfirmed)
            && let Some(ref mut child) = self.child
        {
            #[cfg(windows)]
            match child {
                ShellChild::Process(proc) => {
                    let _ = terminate_windows_job(self.windows_job.as_ref(), proc);
                }
                #[cfg(not(target_env = "ohos"))]
                ShellChild::Pty(child) => {
                    let _ = child.kill();
                }
            }
            #[cfg(all(not(windows), unix))]
            {
                let _ = child.kill();
                match child {
                    ShellChild::Process(proc) => {
                        let _ = wait_child_bounded(proc, KILL_REAP_GRACE);
                    }
                    #[cfg(not(target_env = "ohos"))]
                    ShellChild::Pty(child) => {
                        let _ = child.wait();
                    }
                }
            }
            #[cfg(all(not(windows), not(unix)))]
            {
                let _ = child.kill();
                let _ = child.wait();
            }
        }
    }
}

#[cfg(all(unix, not(target_env = "ohos")))]
pub(crate) fn inherited_interactive_terminal_refusal() -> Option<&'static str> {
    Some(
        "Inherited interactive terminal takeover is unavailable on Unix because foreground TTY \
         ownership cannot be transferred safely. Use Bash with `background: true, tty: true`, \
         then continue it with `action: \"interact\"` and the returned `task_id`; alternatively \
         use `terminal/run` and `terminal/send`, or launch the command in a new terminal. For \
         non-interactive work, omit `interactive: true`.",
    )
}

#[cfg(all(unix, target_env = "ohos"))]
pub(crate) fn inherited_interactive_terminal_refusal() -> Option<&'static str> {
    Some(
        "Inherited interactive terminal takeover is unavailable on Unix because foreground TTY \
         ownership cannot be transferred safely. Launch the command in a new terminal, or omit \
         `interactive: true` for non-interactive work.",
    )
}

#[cfg(not(unix))]
pub(crate) fn inherited_interactive_terminal_refusal() -> Option<&'static str> {
    None
}

/// Manages background shell processes with optional sandboxing.
pub struct ShellManager {
    processes: HashMap<String, BackgroundShell>,
    stale_jobs: HashMap<String, ShellJobSnapshot>,
    default_workspace: PathBuf,
    sandbox_manager: SandboxManager,
    sandbox_policy: ExecutionSandboxPolicy,
    foreground_background_requested: bool,
    /// Directory for lowercase-`bash` complete-output spill files
    /// (`None` = process temp dir). Overridable so tests can fault-inject a
    /// missing/unwritable spill location.
    output_spill_dir: Option<PathBuf>,
}

impl std::fmt::Debug for ShellManager {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ShellManager")
            .field("processes", &self.processes.len())
            .field("stale_jobs", &self.stale_jobs.len())
            .field("default_workspace", &self.default_workspace)
            .field("sandbox_policy", &self.sandbox_policy)
            .field(
                "foreground_background_requested",
                &self.foreground_background_requested,
            )
            .finish()
    }
}

impl ShellManager {
    fn require_session_owner(&self, task_id: &str, active_session_id: &str) -> Result<()> {
        let owned = self.processes.get(task_id).is_some_and(|shell| {
            !active_session_id.is_empty() && shell.owner_session_id == active_session_id
        }) || self.stale_jobs.get(task_id).is_some_and(|job| {
            !active_session_id.is_empty() && job.owner_session_id == active_session_id
        });
        if owned {
            Ok(())
        } else {
            // Do not disclose whether the id exists in another session.
            Err(anyhow!("Job {task_id} not found"))
        }
    }

    /// Create a new `ShellManager` with default (no sandbox) policy.
    pub fn new(workspace: PathBuf) -> Self {
        Self {
            processes: HashMap::new(),
            stale_jobs: HashMap::new(),
            default_workspace: workspace,
            sandbox_manager: SandboxManager::new(),
            sandbox_policy: ExecutionSandboxPolicy::default(),
            foreground_background_requested: false,
            output_spill_dir: None,
        }
    }

    /// Point lowercase-`bash` complete-output spill files at `dir` instead of
    /// the process temp dir. Tests use a nonexistent dir to simulate a full or
    /// broken temp volume. (unix-only: the regression test that uses it drives
    /// a POSIX shell loop.)
    #[cfg(all(test, unix))]
    pub(crate) fn set_output_spill_dir_for_test(&mut self, dir: Option<PathBuf>) {
        self.output_spill_dir = dir;
    }

    /// Insert a finished job without spawning. Count-bound tests would
    /// otherwise pay for 100+ live shells.
    #[cfg(test)]
    pub(crate) fn seed_finished_record_for_test(&mut self, id: impl Into<String>, age: Duration) {
        let now = Instant::now();
        let started_at = now.checked_sub(age).unwrap_or(now);
        let id = id.into();
        self.processes.insert(
            id.clone(),
            BackgroundShell {
                id,
                command: String::new(),
                working_dir: self.default_workspace.clone(),
                status: ShellStatus::Completed,
                exit_code: Some(0),
                started_at,
                finished_at: Some(now),
                last_output_at: now,
                last_observed_output_len: 0,
                sandbox_type: SandboxType::None,
                linked_task_id: None,
                owner_agent: None,
                owner_session_id: String::new(),
                origin_tool_call_id: None,
                origin_turn_id: None,
                ownership: ShellOwnership::Managed,
                stdout_buffer: new_shared_raw_output(),
                stderr_buffer: Some(new_shared_raw_output()),
                bounded_output: None,
                heavy_permit: None,
                stdout_cursor: 0,
                stderr_cursor: 0,
                completion_reported: false,
                stdin: None,
                stdin_closed_at_eof: false,
                stdin_write_stalled: false,
                child: None,
                stop_unconfirmed: false,
                #[cfg(windows)]
                windows_job: None,
                stdout_thread: None,
                stderr_thread: None,
                work_lifecycle: None,
                lifecycle_seq: 0,
                last_lifecycle_status: None,
                last_lifecycle_bytes: 0,
            },
        );
    }

    /// Insert a record that looks like a live job owned by an agent, without
    /// spawning. Reservation tests drive the job's lifetime by hand: flip the
    /// status to `Completed` to model the job exiting.
    #[cfg(test)]
    pub(crate) fn seed_running_shell_for_test(
        &mut self,
        id: impl Into<String>,
        owner_agent_id: &str,
    ) {
        let now = Instant::now();
        let id = id.into();
        self.processes.insert(
            id.clone(),
            BackgroundShell {
                id,
                command: String::new(),
                working_dir: self.default_workspace.clone(),
                status: ShellStatus::Running,
                exit_code: None,
                started_at: now,
                finished_at: None,
                last_output_at: now,
                last_observed_output_len: 0,
                sandbox_type: SandboxType::None,
                linked_task_id: None,
                owner_agent: Some(ShellJobOwner {
                    agent_id: owner_agent_id.to_string(),
                    agent_name: "implementer".to_string(),
                }),
                owner_session_id: String::new(),
                origin_tool_call_id: None,
                origin_turn_id: None,
                ownership: ShellOwnership::Managed,
                stdout_buffer: new_shared_raw_output(),
                stderr_buffer: Some(new_shared_raw_output()),
                bounded_output: None,
                heavy_permit: None,
                stdout_cursor: 0,
                stderr_cursor: 0,
                completion_reported: false,
                stdin: None,
                stdin_closed_at_eof: false,
                stdin_write_stalled: false,
                child: None,
                stop_unconfirmed: false,
                #[cfg(windows)]
                windows_job: None,
                stdout_thread: None,
                stderr_thread: None,
                work_lifecycle: None,
                lifecycle_seq: 0,
                last_lifecycle_status: None,
                last_lifecycle_bytes: 0,
            },
        );
    }

    /// Model a seeded job exiting: reservations consulting it must release.
    #[cfg(test)]
    pub(crate) fn finish_seeded_shell_for_test(&mut self, id: &str) {
        if let Some(shell) = self.processes.get_mut(id) {
            shell.status = ShellStatus::Completed;
            shell.exit_code = Some(0);
            shell.finished_at = Some(Instant::now());
        }
    }

    /// Model the state only an un-reaped stopped child can produce (see
    /// `stop_unconfirmed`): no portable test can spawn a child that survives
    /// SIGKILL, so tests set the flag — and the terminal status — the kill
    /// path would have recorded, and drive the real confirmation against a
    /// real child. Gated to the platforms whose tests consume it: a plain
    /// `#[cfg(test)]` helper would be dead code (denied) on Windows.
    #[cfg(all(test, unix))]
    pub(crate) fn mark_stop_unconfirmed_for_test(&mut self, id: &str) {
        if let Some(shell) = self.processes.get_mut(id) {
            shell.status = ShellStatus::Killed;
            shell.finished_at = Some(Instant::now());
            shell.stop_unconfirmed = true;
        }
    }

    /// The real direct child's pid, for tests that must end the job from
    /// outside the manager — the way an uninterruptible process ends: only
    /// the OS can.
    #[cfg(all(test, unix))]
    pub(crate) fn background_child_pid_for_test(&self, id: &str) -> Option<u32> {
        self.processes
            .get(id)
            .and_then(|shell| shell.child.as_ref())
            .and_then(ShellChild::process_id)
    }

    /// Test-only observation of the workspace selected by runtime rebuilds.
    #[cfg(test)]
    pub(crate) fn default_workspace(&self) -> &Path {
        &self.default_workspace
    }

    /// Enable or disable bubblewrap passthrough (#2184).
    ///
    /// When enabled and `/usr/bin/bwrap` is executable on Linux, exec_shell
    /// commands are routed through bubblewrap for filesystem isolation.
    pub fn set_prefer_bwrap(&mut self, prefer: bool) {
        self.sandbox_manager.set_prefer_bwrap(prefer);
    }

    /// Set user-configured bwrap mount extensions (#5410): extra read-only
    /// roots and writable device nodes such as `/dev/null`.
    pub fn set_bwrap_extensions(&mut self, extensions: crate::sandbox::BwrapMountExtensions) {
        self.sandbox_manager.set_bwrap_extensions(extensions);
    }

    /// Forward the opt-in sandbox read deny-list (S1, #5568) to the sandbox
    /// manager; `~` prefixes expand there.
    pub fn set_denied_read_subpaths(&mut self, paths: Vec<std::path::PathBuf>) {
        self.sandbox_manager.set_denied_read_subpaths(paths);
    }

    /// Return the OS sandbox wrapper this shell manager is configured and able
    /// to apply to commands.
    pub fn configured_sandbox_type(&self) -> Option<SandboxType> {
        self.sandbox_manager.configured_sandbox()
    }

    /// Request that the active foreground shell wait detach and leave its
    /// process running in the background job table.
    pub fn request_foreground_background(&mut self) {
        self.foreground_background_requested = true;
    }

    #[cfg(test)]
    pub(crate) fn foreground_background_requested_for_test(&self) -> bool {
        self.foreground_background_requested
    }

    fn clear_foreground_background_request(&mut self) {
        self.foreground_background_requested = false;
    }

    fn take_foreground_background_request(&mut self) -> bool {
        let requested = self.foreground_background_requested;
        self.foreground_background_requested = false;
        requested
    }

    /// Execute a shell command with stdin/TTY options plus an extra env-var map
    /// that is merged into the spawned process environment. Used by the
    /// `shell_env` hook injection path (#456).
    #[allow(clippy::too_many_arguments)]
    #[cfg(test)]
    pub fn execute_with_options_env(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        background: bool,
        stdin_data: Option<&str>,
        tty: bool,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
    ) -> Result<ShellResult> {
        self.execute_with_options_env_for_owner(
            command,
            working_dir,
            timeout_ms,
            background,
            stdin_data,
            tty,
            policy_override,
            extra_env,
            None,
        )
    }

    /// Launch a parent-owned job stamped to the immutable root session.
    #[allow(clippy::too_many_arguments)]
    pub fn execute_with_options_env_for_session(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        background: bool,
        stdin_data: Option<&str>,
        tty: bool,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
        owner_session_id: &str,
    ) -> Result<ShellResult> {
        self.execute_with_options_env_for_owner_and_work(
            command,
            working_dir,
            timeout_ms,
            background,
            stdin_data,
            tty,
            policy_override,
            extra_env,
            None,
            owner_session_id.to_string(),
            None,
            None,
            None,
            None,
            false,
            (1_000, 600_000),
        )
    }

    /// Same as `execute_with_options_env`, with optional background-job owner
    /// attribution for sub-agent launched jobs.
    #[allow(clippy::too_many_arguments)]
    #[cfg(test)]
    pub fn execute_with_options_env_for_owner(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        background: bool,
        stdin_data: Option<&str>,
        tty: bool,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
        owner_agent: Option<ShellJobOwner>,
    ) -> Result<ShellResult> {
        self.execute_with_options_env_for_owner_and_work(
            command,
            working_dir,
            timeout_ms,
            background,
            stdin_data,
            tty,
            policy_override,
            extra_env,
            owner_agent,
            String::new(),
            None,
            None,
            None,
            None,
            false,
            (1_000, 600_000),
        )
    }

    /// Host-facing owner-aware launch with an explicit immutable session owner.
    #[allow(clippy::too_many_arguments)]
    pub fn execute_with_options_env_for_owner_and_session(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        background: bool,
        stdin_data: Option<&str>,
        tty: bool,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
        owner_agent: Option<ShellJobOwner>,
        owner_session_id: &str,
    ) -> Result<ShellResult> {
        self.execute_with_options_env_for_owner_and_work(
            command,
            working_dir,
            timeout_ms,
            background,
            stdin_data,
            tty,
            policy_override,
            extra_env,
            owner_agent,
            owner_session_id.to_string(),
            None,
            None,
            None,
            None,
            false,
            (1_000, 600_000),
        )
    }

    /// Owner-aware execution with an optional Work Graph lifecycle sink.
    #[allow(clippy::too_many_arguments)]
    fn execute_with_options_env_for_owner_and_work(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        background: bool,
        stdin_data: Option<&str>,
        tty: bool,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
        owner_agent: Option<ShellJobOwner>,
        owner_session_id: String,
        origin_tool_call_id: Option<String>,
        origin_turn_id: Option<String>,
        work_lifecycle: Option<ShellWorkLifecycle>,
        readonly_roots: Option<&[PathBuf]>,
        persist_pending: bool,
        timeout_bounds_ms: (u64, u64),
    ) -> Result<ShellResult> {
        // Log execution via ShellDispatcher when SHELL_DISPATCHER_LOG is set.
        crate::shell_dispatcher::ShellDispatcher::log_exec(command);

        let work_dir = working_dir.map_or_else(|| self.default_workspace.clone(), PathBuf::from);
        validate_shell_working_dir(&work_dir, working_dir.is_none())?;

        let timeout_ms = timeout_ms.clamp(timeout_bounds_ms.0, timeout_bounds_ms.1);

        // Use override policy if provided, otherwise use the manager's policy
        let policy = policy_override.unwrap_or_else(|| self.sandbox_policy.clone());

        // Create command spec and prepare sandboxed environment
        let spec = if let Some(roots) = readonly_roots {
            if command.contains('|')
                || readonly_sequence_segments(command).is_some_and(|segments| segments.len() > 1)
                || agent_readonly_command_needs_shell_for_cd(command)
                || command.contains('*')
            {
                // An agent read-only compound — or any glob-bearing command:
                // a `*` the wildcard walker judged is only real on the shell
                // lane, which expands it the way the walker's per-match
                // judgment assumed, while the direct-argv lane would hand
                // the program the literal spelling and the expansion the
                // gate walked could never happen. Every sequence segment and
                // every pipeline stage was admitted by
                // `is_agent_readonly_shell_command` (no redirects,
                // expansions, or subshells — only `|` between validated
                // pipeline stages and `;`/`&&`/`||` between validated
                // segments), so a shell is needed to bind them; pipefail
                // additionally reports a failed stage where the shell speaks
                // it. The resolved shell must be able to run the compound as
                // written (cmd.exe cannot), checked before anything is bound.
                // Git stages receive the same in-process hardening flags the
                // direct-argv lane splices, because the shell would otherwise
                // run whatever diff driver or textconv the repository
                // configures. Single glob-free commands keep the shell-free
                // direct-argv lane below.
                let kind = crate::shell_dispatcher::global_dispatcher().kind();
                if let Some(reason) = readonly_compound_refused_by_shell_kind(kind, command) {
                    return Err(anyhow!("{reason}"));
                }
                let hardened = harden_readonly_shell_git_stages(command)?;
                let shell_bound = readonly_shell_bound_command(kind, &hardened);
                CommandSpec::shell(
                    &shell_bound,
                    work_dir.clone(),
                    Duration::from_millis(timeout_ms),
                )
            } else {
                let (program, args) = hardened_readonly_argv(command)?;
                let program = resolve_readonly_program(&program, roots)?;
                CommandSpec::program(
                    program
                        .to_str()
                        .ok_or_else(|| anyhow!("read-only executable path is not valid UTF-8"))?,
                    args,
                    work_dir.clone(),
                    Duration::from_millis(timeout_ms),
                )
            }
        } else {
            CommandSpec::shell(command, work_dir.clone(), Duration::from_millis(timeout_ms))
        };
        let spec = spec.with_policy(policy).with_env(extra_env);
        let exec_env = self.sandbox_manager.prepare(&spec);

        if background {
            let bounded_output = timeout_bounds_ms == (1, BASH_MAX_TIMEOUT_MS);
            self.spawn_background_sandboxed(
                command,
                &work_dir,
                &exec_env,
                None,
                stdin_data,
                tty,
                ShellSpawnContext {
                    owner_agent,
                    owner_session_id,
                    origin_tool_call_id,
                    origin_turn_id,
                    work_lifecycle,
                },
                persist_pending,
                bounded_output,
            )
        } else {
            if tty {
                return Err(anyhow!(
                    "TTY mode requires background execution (set background: true)."
                ));
            }
            let first = Self::execute_sync_sandboxed(
                command, &work_dir, timeout_ms, stdin_data, &exec_env,
            )?;
            // Only a failed temp `-File` run the host refused to load is
            // retried: a completed run, a cancellation, a timeout, and the
            // command's own output all keep the first result. The refusal runs
            // no statement, so the inline rerun cannot duplicate side effects.
            if !powershell_refusal_needs_inline_retry(&spec, &first) {
                return Ok(first);
            }
            self.retry_powershell_without_script_file(
                command, &work_dir, timeout_ms, stdin_data, &spec, first,
            )
        }
    }

    /// PowerShell's temporary `-File` script is subject to the machine's
    /// execution policy, so a locked-down machine refuses a script this tool
    /// wrote itself. PowerShell rejects the file before running a single
    /// statement, which makes one inline retry side-effect free: the identical
    /// payload is re-sent as `-EncodedCommand` (no file to refuse, no quoting
    /// surface, no BOM), which is also what survives a Group Policy
    /// `-ExecutionPolicy` override or an AppLocker script rule. Any other
    /// failure keeps the first result untouched.
    fn retry_powershell_without_script_file(
        &self,
        command: &str,
        work_dir: &std::path::Path,
        timeout_ms: u64,
        stdin_data: Option<&str>,
        spec: &CommandSpec,
        first: ShellResult,
    ) -> Result<ShellResult> {
        let Some(encoded) = CommandSpec::powershell_encoded_shell(
            command,
            work_dir.to_path_buf(),
            Duration::from_millis(timeout_ms),
        ) else {
            return Ok(first);
        };
        let encoded = encoded
            .with_policy(spec.sandbox_policy.clone())
            .with_env(spec.env.clone());
        let exec_env = self.sandbox_manager.prepare(&encoded);
        let mut retried = match Self::execute_sync_sandboxed(
            command, work_dir, timeout_ms, stdin_data, &exec_env,
        ) {
            Ok(retried) => retried,
            Err(error) => return Ok(keep_first_result_after_a_failed_retry(first, &error)),
        };
        if !retried.stderr.is_empty() && !retried.stderr.ends_with('\n') {
            retried.stderr.push('\n');
        }
        retried.stderr.push_str(
            "[codewhale] the local execution policy refused the temporary script; \
             the same command was re-run inline with -EncodedCommand\n",
        );
        retried.duration_ms = retried.duration_ms.saturating_add(first.duration_ms);
        Ok(retried)
    }

    /// Interactive variant that accepts extra env vars (#456 shell_env hook).
    pub fn execute_interactive_with_policy_env(
        &mut self,
        command: &str,
        working_dir: Option<&str>,
        timeout_ms: u64,
        policy_override: Option<ExecutionSandboxPolicy>,
        extra_env: HashMap<String, String>,
    ) -> Result<ShellResult> {
        crate::shell_dispatcher::ShellDispatcher::log_exec(command);

        // A new Unix process group that inherits the terminal is not its
        // foreground owner. Letting it read stdin triggers SIGTTIN; sharing
        // Codewhale's group instead would make cooked-mode Ctrl+C terminate
        // both parent and child. Until this path owns a complete POSIX job-
        // control lease, fail closed before spawning. Persistent PTY tools
        // already provide a safe interactive lane without taking over the
        // operator's live terminal.
        if let Some(message) = inherited_interactive_terminal_refusal() {
            return Err(anyhow!(message));
        }

        let work_dir = working_dir.map_or_else(|| self.default_workspace.clone(), PathBuf::from);
        validate_shell_working_dir(&work_dir, working_dir.is_none())?;

        let timeout_ms = timeout_ms.clamp(1000, 600_000);
        let policy = policy_override.unwrap_or_else(|| self.sandbox_policy.clone());

        let spec = CommandSpec::shell(command, work_dir.clone(), Duration::from_millis(timeout_ms))
            .with_policy(policy)
            .with_env(extra_env);
        let exec_env = self.sandbox_manager.prepare(&spec);

        Self::execute_interactive_sandboxed(command, &work_dir, timeout_ms, &exec_env)
    }

    /// Execute command synchronously with timeout (sandboxed).
    fn execute_sync_sandboxed(
        original_command: &str,
        working_dir: &std::path::Path,
        timeout_ms: u64,
        stdin_data: Option<&str>,
        exec_env: &ExecEnv,
    ) -> Result<ShellResult> {
        let started = Instant::now();
        let timeout = Duration::from_millis(timeout_ms);
        let sandbox_type = exec_env.sandbox_type;
        let sandboxed = exec_env.is_sandboxed();

        // Build the command from ExecEnv
        let program = exec_env.program();
        let args = exec_env.args();

        let mut cmd = Command::new(program);
        crate::utils::suppress_console_window(&mut cmd);
        push_shell_args(&mut cmd, program, args);
        cmd.current_dir(working_dir)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        #[cfg(unix)]
        {
            cmd.process_group(0);
        }
        install_parent_death_signal(&mut cmd);

        if stdin_data.is_some() {
            cmd.stdin(Stdio::piped());
        } else {
            // std defaults to `Stdio::inherit()`, which on a hosted (non-PTY)
            // run hands the child the parent's TTY or an arbitrary pipe it can
            // never see EOF on. This lane has no stdin payload and no operator:
            // a deterministic null stdin gives stdin-readers an immediate
            // end-of-file instead of a hang or a SIGTTIN stop.
            cmd.stdin(Stdio::null());
        }

        child_env::apply_to_command(&mut cmd, child_env::string_map_env(&exec_env.env));
        remove_readonly_redirect_env(&mut cmd, &exec_env.env);

        // Disable raw mode before spawn; restore only if raw mode was active
        // on entry (issue #1690).
        let raw_mode_was_enabled = crossterm::terminal::is_raw_mode_enabled().unwrap_or(false);
        if raw_mode_was_enabled {
            let _ = crossterm::terminal::disable_raw_mode();
        }
        struct SyncRawModeGuard {
            restore: bool,
        }
        impl Drop for SyncRawModeGuard {
            fn drop(&mut self) {
                if self.restore {
                    let _ = crossterm::terminal::enable_raw_mode();
                }
            }
        }
        let _guard = SyncRawModeGuard {
            restore: raw_mode_was_enabled,
        };

        let mut child = cmd
            .spawn()
            .with_context(|| format!("Failed to execute: {original_command}"))?;
        #[cfg(windows)]
        let windows_job = attach_windows_job(&child, original_command);

        if let Some(input) = stdin_data
            && let Some(mut stdin) = child.stdin.take()
        {
            // Same SIGPIPE discipline as the bounded stdin workers (see
            // [`BlockedSigpipeGuard`]): the child dying mid-write must
            // surface as a handled write error, not kill a process that
            // runs with SIGPIPE at its default disposition. Unlike the
            // tool surface this manager-API lane has neither
            // [`MAX_INLINE_STDIN_BYTES`] nor the bounded writer's
            // deadline — every production payload goes through the
            // background lane's bounded writer (see
            // [`STDIN_WRITE_DEADLINE`]), so a direct caller passing a
            // payload here must keep it within what the child reads.
            let _sigpipe = BlockedSigpipeGuard::new();
            stdin
                .write_all(input.as_bytes())
                .context("Failed to write to stdin")?;
            stdin.flush().ok();
        }

        let stdout_handle = child.stdout.take().context("Failed to capture stdout")?;
        let stderr_handle = child.stderr.take().context("Failed to capture stderr")?;

        // Spawn threads to read output. Use bounded receives below so a killed
        // or detached descendant that keeps pipe handles open cannot wedge the
        // foreground shell path while the global tool lock is held (#2571).
        let stdout_rx = spawn_sync_reader_thread(stdout_handle);
        let stderr_rx = spawn_sync_reader_thread(stderr_handle);

        // Wait with timeout
        if let Some(status) = child.wait_timeout(timeout)? {
            let status = ShellExitStatus::from_std(status);
            #[cfg(unix)]
            let _ = kill_child_process_group(&mut child);
            #[cfg(windows)]
            terminate_and_close_windows_job(windows_job);
            let stdout = recv_sync_reader_output(&stdout_rx);
            let stderr = recv_sync_reader_output(&stderr_rx);
            let stdout_str = String::from_utf8_lossy(&stdout).to_string();
            let stderr_str = String::from_utf8_lossy(&stderr).to_string();
            let exit_code = status
                .code
                .and_then(|code| i32::try_from(code).ok())
                .unwrap_or(-1);

            // Check if sandbox denied the operation
            let sandbox_denied = SandboxManager::was_denied(sandbox_type, exit_code, &stderr_str);
            let (stdout, stdout_meta) = truncate_with_meta(&stdout_str);
            let (stderr, stderr_meta) = truncate_with_meta(&stderr_str);

            Ok(ShellResult {
                task_id: None,
                status: if status.success {
                    ShellStatus::Completed
                } else {
                    ShellStatus::Failed
                },
                exit_code: status.code,
                stdout,
                stderr,
                duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
                stdout_len: stdout_meta.original_len,
                stderr_len: stderr_meta.original_len,
                stdout_omitted: stdout_meta.omitted,
                stderr_omitted: stderr_meta.omitted,
                stdout_truncated: stdout_meta.truncated,
                stderr_truncated: stderr_meta.truncated,
                sandboxed,
                sandbox_type: if sandboxed {
                    Some(sandbox_type.to_string())
                } else {
                    None
                },
                sandbox_denied,
            })
        } else {
            // Timeout - kill the process
            #[cfg(unix)]
            let _ = kill_child_process_group(&mut child);
            #[cfg(windows)]
            let _ = terminate_child_and_close_windows_job(windows_job, &mut child);
            #[cfg(all(not(unix), not(windows)))]
            let _ = child.kill();
            let status = child.wait().ok();
            let stdout = recv_sync_reader_output(&stdout_rx);
            let stderr = recv_sync_reader_output(&stderr_rx);
            let stdout_str = String::from_utf8_lossy(&stdout).to_string();
            let stderr_str = String::from_utf8_lossy(&stderr).to_string();
            let (stdout, stdout_meta) = truncate_with_meta(&stdout_str);
            let (stderr, stderr_meta) = truncate_with_meta(&stderr_str);

            Ok(ShellResult {
                task_id: None,
                status: ShellStatus::TimedOut,
                exit_code: status
                    .map(ShellExitStatus::from_std)
                    .and_then(|status| status.code),
                stdout,
                stderr,
                duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
                stdout_len: stdout_meta.original_len,
                stderr_len: stderr_meta.original_len,
                stdout_omitted: stdout_meta.omitted,
                stderr_omitted: stderr_meta.omitted,
                stdout_truncated: stdout_meta.truncated,
                stderr_truncated: stderr_meta.truncated,
                sandboxed,
                sandbox_type: if sandboxed {
                    Some(sandbox_type.to_string())
                } else {
                    None
                },
                sandbox_denied: false,
            })
        }
    }

    /// Execute command interactively with timeout (sandboxed).
    fn execute_interactive_sandboxed(
        original_command: &str,
        working_dir: &std::path::Path,
        timeout_ms: u64,
        exec_env: &ExecEnv,
    ) -> Result<ShellResult> {
        let started = Instant::now();
        let timeout = Duration::from_millis(timeout_ms);
        let sandbox_type = exec_env.sandbox_type;
        let sandboxed = exec_env.is_sandboxed();

        let program = exec_env.program();
        let args = exec_env.args();

        let mut cmd = Command::new(program);
        crate::utils::suppress_console_window(&mut cmd);
        push_shell_args(&mut cmd, program, args);
        cmd.current_dir(working_dir)
            .stdin(Stdio::inherit())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit());
        #[cfg(unix)]
        {
            cmd.process_group(0);
        }
        install_parent_death_signal(&mut cmd);

        // Disable raw mode before spawn; restore only if raw mode was active
        // on entry (issue #1690).
        let raw_mode_was_enabled = crossterm::terminal::is_raw_mode_enabled().unwrap_or(false);
        if raw_mode_was_enabled {
            let _ = crossterm::terminal::disable_raw_mode();
        }
        struct InteractiveRawModeGuard {
            restore: bool,
        }
        impl Drop for InteractiveRawModeGuard {
            fn drop(&mut self) {
                if self.restore {
                    let _ = crossterm::terminal::enable_raw_mode();
                }
            }
        }
        let _guard = InteractiveRawModeGuard {
            restore: raw_mode_was_enabled,
        };

        child_env::apply_to_command(&mut cmd, child_env::string_map_env(&exec_env.env));

        let mut child = cmd
            .spawn()
            .with_context(|| format!("Failed to execute: {original_command}"))?;
        #[cfg(windows)]
        let windows_job = attach_windows_job(&child, original_command);

        if let Some(status) = child.wait_timeout(timeout)? {
            let status = ShellExitStatus::from_std(status);
            #[cfg(windows)]
            terminate_and_close_windows_job(windows_job);
            Ok(ShellResult {
                task_id: None,
                status: if status.success {
                    ShellStatus::Completed
                } else {
                    ShellStatus::Failed
                },
                exit_code: status.code,
                stdout: String::new(),
                stderr: String::new(),
                duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
                stdout_len: 0,
                stderr_len: 0,
                stdout_omitted: 0,
                stderr_omitted: 0,
                stdout_truncated: false,
                stderr_truncated: false,
                sandboxed,
                sandbox_type: if sandboxed {
                    Some(sandbox_type.to_string())
                } else {
                    None
                },
                sandbox_denied: false,
            })
        } else {
            #[cfg(unix)]
            let _ = kill_child_process_group(&mut child);
            #[cfg(windows)]
            let _ = terminate_child_and_close_windows_job(windows_job, &mut child);
            #[cfg(all(not(unix), not(windows)))]
            let _ = child.kill();
            let status = child.wait().ok();

            Ok(ShellResult {
                task_id: None,
                status: ShellStatus::TimedOut,
                exit_code: status
                    .map(ShellExitStatus::from_std)
                    .and_then(|status| status.code),
                stdout: String::new(),
                stderr: String::new(),
                duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
                stdout_len: 0,
                stderr_len: 0,
                stdout_omitted: 0,
                stderr_omitted: 0,
                stdout_truncated: false,
                stderr_truncated: false,
                sandboxed,
                sandbox_type: if sandboxed {
                    Some(sandbox_type.to_string())
                } else {
                    None
                },
                sandbox_denied: false,
            })
        }
    }

    /// Spawn a background process (sandboxed).
    #[allow(clippy::too_many_arguments)]
    fn spawn_background_sandboxed(
        &mut self,
        original_command: &str,
        working_dir: &std::path::Path,
        exec_env: &ExecEnv,
        heavy_permit: Option<HeavyCommandPermit>,
        stdin_data: Option<&str>,
        tty: bool,
        spawn_context: ShellSpawnContext,
        persist_pending: bool,
        small_contract_mode: bool,
    ) -> Result<ShellResult> {
        let ShellSpawnContext {
            owner_agent,
            owner_session_id,
            origin_tool_call_id,
            origin_turn_id,
            work_lifecycle,
        } = spawn_context;
        let task_id = format!("shell_{}", &Uuid::new_v4().to_string()[..8]);
        let mut spawn_guard =
            ShellSpawnIntentGuard::new(work_lifecycle.clone(), &task_id, original_command)?;
        let started = Instant::now();
        let sandbox_type = exec_env.sandbox_type;
        let sandboxed = exec_env.is_sandboxed();

        // Build the command from ExecEnv
        let program = exec_env.program();
        let args = exec_env.args();

        #[cfg(target_env = "ohos")]
        if tty {
            return Err(anyhow!(
                "TTY shell mode is not supported on HarmonyOS/OpenHarmony yet."
            ));
        }

        let stdout_buffer = new_shared_raw_output();
        let stderr_buffer = if tty || persist_pending || small_contract_mode {
            None
        } else {
            Some(new_shared_raw_output())
        };
        // The spill file is best-effort: a full disk or exhausted descriptor
        // table must not make `echo ok` unrunnable (that is exactly how the
        // owner's session got wedged under swap exhaustion).
        let bounded_output = small_contract_mode.then(|| {
            Arc::new(Mutex::new(BoundedOutputAccumulator::new_in(
                self.output_spill_dir.as_deref(),
            )))
        });

        #[cfg(windows)]
        let mut windows_job = None;

        let (child, stdin, stdout_thread, stderr_thread) = if tty {
            #[cfg(target_env = "ohos")]
            unreachable!("OHOS TTY mode returns before PTY setup");

            #[cfg(not(target_env = "ohos"))]
            {
                let pty_system = native_pty_system();
                let pair = pty_system
                    .openpty(PtySize {
                        rows: 24,
                        cols: 80,
                        pixel_width: 0,
                        pixel_height: 0,
                    })
                    .context("Failed to open PTY")?;

                let mut cmd = CommandBuilder::new(program);
                for arg in args {
                    cmd.arg(arg);
                }
                cmd.cwd(working_dir);
                child_env::apply_to_pty_command(&mut cmd, child_env::string_map_env(&exec_env.env));

                let mut child = pair
                    .slave
                    .spawn_command(cmd)
                    .with_context(|| format!("Failed to spawn PTY command: {original_command}"))?;
                drop(pair.slave);

                let reader = match pair.master.try_clone_reader() {
                    Ok(reader) => reader,
                    Err(err) => {
                        let _ = child.kill();
                        let _ = child.wait();
                        return Err(err).context("Failed to clone PTY reader");
                    }
                };
                // The interact writer is acquired BEFORE the reader thread
                // spawns, so a writer-acquisition failure aborts with only
                // the child to clean up (kill + wait) instead of orphaning
                // an already-spawned reader thread. Nothing observes the
                // order: `pair.master` is alive across the whole block, so
                // the slave sees no end-of-file in either arrangement.
                //
                // Unix: the interact writer is a dup of the master fd, and
                // `take_writer` is NOT called — its box's Drop injects a
                // newline plus `VEOF` into the master (that is how "dropping
                // the writer sends EOF" is implemented there), which a
                // terminal command that never receives an interact would read
                // as an immediate end-of-file (probe-pinned: `cat` on a pty
                // exited 0 the instant the box was dropped). The dup keeps
                // a master-side description open — no EOF for the slave
                // while the reader's clone is open too. A plain drop of the
                // dup (the spawn-path close never touches this lane, and a
                // close whose delivery failed releases it) still shows the
                // child nothing;
                // an explicit `close_stdin` instead delivers the ^D bytes
                // itself through the bounded worker and claims end-of-file
                // only when that write really landed; the final master
                // handle closes at teardown, after the child can no longer
                // observe it.
                #[cfg(unix)]
                let stdin_writer = {
                    use std::os::fd::FromRawFd;
                    match pair.master.as_raw_fd() {
                        Some(fd) => {
                            let dup_fd = unsafe { libc::dup(fd) };
                            if dup_fd < 0 {
                                let _ = child.kill();
                                let _ = child.wait();
                                return Err(std::io::Error::last_os_error())
                                    .context("Failed to dup the PTY master for the stdin writer");
                            }
                            // SAFETY: `dup_fd` is a freshly duplicated fd owned
                            // by this call; `File::from_raw_fd` takes ownership.
                            Some(StdinWriter::PtyFile(unsafe {
                                std::fs::File::from_raw_fd(dup_fd)
                            }))
                        }
                        None => {
                            let _ = child.kill();
                            let _ = child.wait();
                            return Err(anyhow!(
                                "Failed to obtain the PTY master fd for the stdin writer"
                            ));
                        }
                    }
                };
                // Windows has no dup'd master fd to hand out (the
                // portable-pty writer box is not fd-exposed there), so the
                // lane keeps the box and its blocking write, per the
                // interact comment.
                #[cfg(not(unix))]
                let stdin_writer = Some(StdinWriter::Pty(match pair.master.take_writer() {
                    Ok(writer) => writer,
                    Err(err) => {
                        let _ = child.kill();
                        let _ = child.wait();
                        return Err(err).context("Failed to take PTY writer");
                    }
                }));
                let stdout_thread = Some(spawn_reader_thread(reader, Arc::clone(&stdout_buffer)));

                (ShellChild::Pty(child), stdin_writer, stdout_thread, None)
            }
        } else if persist_pending {
            let mut cmd = Command::new(program);
            crate::utils::suppress_console_window(&mut cmd);
            push_shell_args(&mut cmd, program, args);
            cmd.current_dir(working_dir)
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null());
            #[cfg(unix)]
            {
                cmd.process_group(0);
            }

            child_env::apply_to_command(&mut cmd, child_env::string_map_env(&exec_env.env));
            remove_readonly_redirect_env(&mut cmd, &exec_env.env);

            let child = cmd.spawn().with_context(|| {
                format!("Failed to spawn persistent service: {original_command}")
            })?;
            (ShellChild::Process(child), None, None, None)
        } else {
            let mut cmd = Command::new(program);
            crate::utils::suppress_console_window(&mut cmd);
            push_shell_args(&mut cmd, program, args);
            cmd.current_dir(working_dir).stdin(Stdio::piped());
            let combined_reader = if small_contract_mode {
                let (reader, stdout, stderr) =
                    shared_output_pipe().context("Failed to create combined shell output pipe")?;
                cmd.stdout(Stdio::from(stdout)).stderr(Stdio::from(stderr));
                Some(reader)
            } else {
                cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
                None
            };
            #[cfg(unix)]
            {
                cmd.process_group(0);
            }

            child_env::apply_to_command(&mut cmd, child_env::string_map_env(&exec_env.env));
            remove_readonly_redirect_env(&mut cmd, &exec_env.env);

            let mut child = cmd
                .spawn()
                .with_context(|| format!("Failed to spawn background: {original_command}"))?;
            #[cfg(windows)]
            {
                windows_job = attach_windows_job(&child, original_command);
            }

            let stdin_handle = child.stdin.take().map(StdinWriter::Pipe);

            let (stdout_thread, stderr_thread) =
                if let (Some(reader), Some(output)) = (combined_reader, bounded_output.as_ref()) {
                    (
                        Some(spawn_bounded_reader_thread(reader, Arc::clone(output))),
                        None,
                    )
                } else {
                    let stdout_handle = child.stdout.take().ok_or_else(|| {
                        #[cfg(windows)]
                        terminate_unregistered_process(&mut child, windows_job.as_ref());
                        #[cfg(not(windows))]
                        terminate_unregistered_process(&mut child);
                        anyhow!("Failed to capture stdout")
                    })?;
                    let stderr_handle = child.stderr.take().ok_or_else(|| {
                        #[cfg(windows)]
                        terminate_unregistered_process(&mut child, windows_job.as_ref());
                        #[cfg(not(windows))]
                        terminate_unregistered_process(&mut child);
                        anyhow!("Failed to capture stderr")
                    })?;
                    (
                        Some(spawn_reader_thread(
                            stdout_handle,
                            Arc::clone(&stdout_buffer),
                        )),
                        stderr_buffer
                            .as_ref()
                            .map(|buffer| spawn_reader_thread(stderr_handle, Arc::clone(buffer))),
                    )
                };

            (
                ShellChild::Process(child),
                stdin_handle,
                stdout_thread,
                stderr_thread,
            )
        };

        let mut bg_shell = BackgroundShell {
            id: task_id.clone(),
            command: original_command.to_string(),
            working_dir: working_dir.to_path_buf(),
            status: ShellStatus::Running,
            exit_code: None,
            started_at: started,
            finished_at: None,
            last_output_at: started,
            last_observed_output_len: 0,
            sandbox_type,
            linked_task_id: None,
            owner_agent,
            owner_session_id,
            origin_tool_call_id,
            origin_turn_id,
            ownership: if persist_pending {
                ShellOwnership::PersistPending
            } else {
                ShellOwnership::Managed
            },
            stdout_buffer,
            stderr_buffer,
            bounded_output,
            heavy_permit,
            stdout_cursor: 0,
            stderr_cursor: 0,
            completion_reported: false,
            stdin,
            stdin_closed_at_eof: false,
            stdin_write_stalled: false,
            child: Some(child),
            stop_unconfirmed: false,
            #[cfg(windows)]
            windows_job,
            stdout_thread,
            stderr_thread,
            work_lifecycle,
            lifecycle_seq: 0,
            last_lifecycle_status: None,
            last_lifecycle_bytes: 0,
        };

        #[cfg(unix)]
        if persist_pending {
            let process_group_id = bg_shell
                .child
                .as_ref()
                .and_then(ShellChild::process_id)
                .ok_or_else(|| anyhow!("Persistent service has no process group id"))?;
            register_pending_persistent_process_group(process_group_id);
        }

        if let Some(input) = stdin_data
            && let Err(err) = bg_shell.write_stdin(input, false)
        {
            // No task id exists yet — the record is only registered below —
            // so `write_stdin`'s "kill it and start a new one" recovery copy
            // has no target here. The spawn aborts instead: the child is
            // killed and the caller learns the run never started.
            let _ = bg_shell.kill();
            return Err(err.context(
                "the run never started: delivering the inline stdin payload failed, so the command was killed and NO task was created (the task id the error above mentions was never registered — do not try to cancel or kill it)",
            ));
        }

        if let Err(err) = bg_shell.publish_lifecycle() {
            let _ = bg_shell.kill();
            return Err(err);
        }

        self.processes.insert(task_id.clone(), bg_shell);
        spawn_guard.disarm();
        // Evict here, not only from `list_jobs()`: retention must not depend on
        // the user opening the jobs panel, and every spawn is exactly the moment
        // the previous calls' records became one call staler (#5472).
        self.cleanup(FINISHED_SHELL_MAX_AGE);

        Ok(ShellResult {
            task_id: Some(task_id),
            status: ShellStatus::Running,
            exit_code: None,
            stdout: String::new(),
            stderr: String::new(),
            duration_ms: 0,
            stdout_len: 0,
            stderr_len: 0,
            stdout_omitted: 0,
            stderr_omitted: 0,
            stdout_truncated: false,
            stderr_truncated: false,
            sandboxed,
            sandbox_type: if sandboxed {
                Some(sandbox_type.to_string())
            } else {
                None
            },
            sandbox_denied: false,
        })
    }

    /// Get output from a background process
    #[allow(dead_code)]
    pub fn get_output(
        &mut self,
        task_id: &str,
        block: bool,
        timeout_ms: u64,
    ) -> Result<ShellResult> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;

        if block && shell.status == ShellStatus::Running {
            let timeout = Duration::from_millis(timeout_ms.clamp(1000, 600_000));
            let deadline = Instant::now() + timeout;

            while shell.status == ShellStatus::Running && Instant::now() < deadline {
                if shell.poll() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(100));
            }

            // If still running after timeout
            if shell.status == ShellStatus::Running {
                return shell.snapshot();
            }
        } else {
            shell.poll();
        }

        shell.snapshot()
    }

    /// Poll a job and return only its status.
    ///
    /// The foreground wait loop ticks every 100 ms and discards the snapshot
    /// unless the job is terminal, but `get_output` → `snapshot` clones both
    /// raw buffers to build it. On a command printing 50 MB that was ~1.5 GB of
    /// allocate-and-drop churn per wait, all of it thrown away (#5472 finding 1,
    /// the transient term). Status is what the loop actually needs.
    fn poll_status(&mut self, task_id: &str) -> Result<ShellStatus> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;
        shell.poll();
        Ok(shell.status.clone())
    }

    /// Write data to stdin of a background process.
    pub fn write_stdin(&mut self, task_id: &str, input: &str, close: bool) -> Result<()> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;
        shell.write_stdin(input, close)?;
        Ok(())
    }

    /// Close a foreground command's stdin PIPE so pipe readers get their
    /// end-of-file at once, keeping any PTY writer attached: dropping the
    /// PTY writer is invisible to the child (the reader holds the master)
    /// but destroys the interact surface a session member can still use
    /// while a terminal command runs — the foreground terminal lane's
    /// contract. A missing record is a no-op, not an error: only eviction
    /// removes a record, and eviction drops the stdin handle with it, so
    /// the pipe cannot still be open — and the close runs between spawn
    /// and the first caller-visible step, where an error would abort a run
    /// whose result is already determined.
    fn close_stdin_pipe(&mut self, task_id: &str) {
        if let Some(shell) = self.processes.get_mut(task_id) {
            shell.close_stdin_pipe();
        }
    }

    pub fn write_stdin_for_session(
        &mut self,
        active_session_id: &str,
        task_id: &str,
        input: &str,
        close: bool,
    ) -> Result<()> {
        self.require_session_owner(task_id, active_session_id)?;
        self.write_stdin(task_id, input, close)
    }

    /// Get incremental output from a background process, consuming any new output.
    fn get_output_delta(
        &mut self,
        task_id: &str,
        wait: bool,
        timeout_ms: u64,
    ) -> Result<ShellDeltaResult> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;

        if wait && shell.status == ShellStatus::Running {
            let timeout = Duration::from_millis(timeout_ms.clamp(1000, 600_000));
            let deadline = Instant::now() + timeout;

            while shell.status == ShellStatus::Running && Instant::now() < deadline {
                if shell.poll() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(100));
            }
        } else {
            shell.poll();
        }

        let (
            stdout_delta,
            stderr_delta,
            stdout_delta_len,
            stderr_delta_len,
            stdout_total,
            stderr_total,
        ) = shell.take_delta();
        let (stdout, stdout_meta) = truncate_with_meta(&stdout_delta);
        let (stderr, stderr_meta) = truncate_with_meta(&stderr_delta);
        let sandboxed = !matches!(shell.sandbox_type, SandboxType::None);

        let command = shell.command.clone();
        let result = ShellResult {
            task_id: Some(shell.id.clone()),
            status: shell.status.clone(),
            exit_code: shell.exit_code,
            stdout,
            stderr,
            duration_ms: u64::try_from(shell.started_at.elapsed().as_millis()).unwrap_or(u64::MAX),
            stdout_len: stdout_meta.original_len.max(stdout_delta_len),
            stderr_len: stderr_meta.original_len.max(stderr_delta_len),
            stdout_omitted: stdout_meta.omitted,
            stderr_omitted: stderr_meta.omitted,
            stdout_truncated: stdout_meta.truncated,
            stderr_truncated: stderr_meta.truncated,
            sandboxed,
            sandbox_type: if sandboxed {
                Some(shell.sandbox_type.to_string())
            } else {
                None
            },
            sandbox_denied: shell.sandbox_denied(),
        };

        Ok(ShellDeltaResult {
            command,
            result,
            stdout_total_len: stdout_total,
            stderr_total_len: stderr_total,
        })
    }

    fn attach_heavy_permit(&mut self, task_id: &str, permit: HeavyCommandPermit) -> Result<()> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;
        shell.heavy_permit = Some(permit);
        Ok(())
    }

    /// Kill a running background process
    pub fn kill(&mut self, task_id: &str) -> Result<ShellResult> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;

        shell.kill()?;
        shell.snapshot()
    }

    pub fn kill_for_session(
        &mut self,
        active_session_id: &str,
        task_id: &str,
    ) -> Result<ShellResult> {
        self.require_session_owner(task_id, active_session_id)?;
        self.kill(task_id)
    }

    /// Kill every currently running background shell process.
    #[cfg(test)]
    pub fn kill_running(&mut self) -> Result<Vec<ShellResult>> {
        let ids = self
            .processes
            .iter()
            .filter(|(_, shell)| shell.status == ShellStatus::Running)
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();

        let mut results = Vec::with_capacity(ids.len());
        for id in ids {
            results.push(self.kill(&id)?);
        }
        Ok(results)
    }

    pub fn kill_running_for_session(
        &mut self,
        active_session_id: &str,
    ) -> Result<Vec<ShellResult>> {
        let ids = self
            .processes
            .iter()
            .filter(|(_, shell)| {
                shell.status == ShellStatus::Running
                    && !active_session_id.is_empty()
                    && shell.owner_session_id == active_session_id
            })
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();
        let mut results = Vec::with_capacity(ids.len());
        for id in ids {
            results.push(self.kill(&id)?);
        }
        Ok(results)
    }

    /// Transfer every still-running `persist:true` process out of Codewhale's
    /// ownership. This is called only by the real headless exec host after the
    /// enclosing turn has completed successfully.
    #[cfg(unix)]
    pub fn commit_persistent_services(&mut self) -> Result<Vec<PersistentServiceReceipt>> {
        let mut ids = self
            .processes
            .iter()
            .filter(|(_, shell)| shell.ownership == ShellOwnership::PersistPending)
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();
        ids.sort();

        for id in &ids {
            let shell = self
                .processes
                .get_mut(id)
                .ok_or_else(|| anyhow!("Persistent service {id} disappeared before commit"))?;
            shell.poll();
            if shell.status != ShellStatus::Running {
                return Err(anyhow!(
                    "Persistent service {id} exited before ownership transfer (status {:?}, exit code {:?})",
                    shell.status,
                    shell.exit_code
                ));
            }
            if shell
                .child
                .as_ref()
                .and_then(ShellChild::process_id)
                .is_none()
            {
                return Err(anyhow!(
                    "Persistent service {id} has no releasable process id"
                ));
            }
        }

        let mut receipts = Vec::with_capacity(ids.len());
        for id in ids {
            let mut shell = self
                .processes
                .remove(&id)
                .ok_or_else(|| anyhow!("Persistent service {id} disappeared during commit"))?;
            let pid = shell
                .child
                .as_ref()
                .and_then(ShellChild::process_id)
                .ok_or_else(|| anyhow!("Persistent service {id} lost its process id"))?;
            unregister_pending_persistent_process_group(pid);
            shell.ownership = ShellOwnership::Released;
            shell.stdin = None;
            shell.heavy_permit.take();
            shell.work_lifecycle = None;
            receipts.push(PersistentServiceReceipt {
                task_id: id,
                pid,
                process_group_id: pid,
                ownership: "external".to_string(),
            });
        }
        Ok(receipts)
    }

    /// Kill only services waiting for a successful exec ownership transfer.
    /// Ordinary background jobs retain their existing manager lifetime.
    pub fn abort_persistent_services(&mut self) {
        let ids = self
            .processes
            .iter()
            .filter(|(_, shell)| shell.ownership == ShellOwnership::PersistPending)
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();
        for id in ids {
            if let Err(error) = self.kill(&id) {
                tracing::warn!(shell_id = %id, %error, "failed to abort pending persistent service");
            }
        }
    }

    /// Poll a background process and return incremental output.
    #[cfg(test)]
    pub fn poll_delta(
        &mut self,
        task_id: &str,
        wait: bool,
        timeout_ms: u64,
    ) -> Result<ShellDeltaResult> {
        self.get_output_delta(task_id, wait, timeout_ms)
    }

    pub fn poll_delta_for_session(
        &mut self,
        active_session_id: &str,
        task_id: &str,
        wait: bool,
        timeout_ms: u64,
    ) -> Result<ShellDeltaResult> {
        self.require_session_owner(task_id, active_session_id)?;
        self.get_output_delta(task_id, wait, timeout_ms)
    }

    fn get_output_delta_for_session(
        &mut self,
        active_session_id: &str,
        task_id: &str,
        wait: bool,
        timeout_ms: u64,
    ) -> Result<ShellDeltaResult> {
        self.require_session_owner(task_id, active_session_id)?;
        self.get_output_delta(task_id, wait, timeout_ms)
    }

    /// Attach durable task context to a live shell job.
    pub fn tag_linked_task(&mut self, task_id: &str, linked_task_id: Option<String>) -> Result<()> {
        let shell = self
            .processes
            .get_mut(task_id)
            .ok_or_else(|| anyhow!("Task {task_id} not found"))?;
        shell.linked_task_id = linked_task_id;
        Ok(())
    }

    /// Inspect full output for a live or stale job.
    pub fn inspect_job(&mut self, task_id: &str) -> Result<ShellJobDetail> {
        if let Some(shell) = self.processes.get_mut(task_id) {
            shell.poll();
            return Ok(shell.job_detail());
        }
        if let Some(snapshot) = self.stale_jobs.get(task_id) {
            return Ok(ShellJobDetail {
                snapshot: snapshot.clone(),
                stdout: snapshot.stdout_tail.clone(),
                stderr: snapshot.stderr_tail.clone(),
            });
        }
        Err(anyhow!("Job {task_id} not found"))
    }

    pub fn inspect_job_for_session(
        &mut self,
        active_session_id: &str,
        task_id: &str,
    ) -> Result<ShellJobDetail> {
        self.require_session_owner(task_id, active_session_id)?;
        self.inspect_job(task_id)
    }

    /// List all live and known-stale background shell jobs for the TUI.
    pub fn list_jobs(&mut self) -> Vec<ShellJobSnapshot> {
        for shell in self.processes.values_mut() {
            shell.poll();
        }
        // Evict completed processes older than 1 hour to bound memory growth.
        self.cleanup(FINISHED_SHELL_MAX_AGE);

        let mut jobs = self
            .processes
            .values()
            .map(BackgroundShell::job_snapshot)
            .collect::<Vec<_>>();
        jobs.extend(self.stale_jobs.values().cloned());
        jobs.sort_by(|a, b| {
            job_status_rank(&a.status, a.stale)
                .cmp(&job_status_rank(&b.status, b.stale))
                .then_with(|| a.id.cmp(&b.id))
        });
        jobs
    }

    pub fn list_jobs_for_session(&mut self, active_session_id: &str) -> Vec<ShellJobSnapshot> {
        if active_session_id.is_empty() {
            return Vec::new();
        }
        self.list_jobs()
            .into_iter()
            .filter(|job| job.owner_session_id == active_session_id)
            .collect()
    }

    /// Whether a finished parent-owned job's completion is waiting to be
    /// claimed. Unlike
    /// [`Self::may_have_undelivered_completion`] this polls, so it reports
    /// readiness the moment the process exits; the engine's idle shell wake
    /// uses it to fire exactly when evidence exists.
    #[cfg(test)]
    pub(crate) fn has_finished_unreported_jobs(&mut self) -> bool {
        self.processes.values_mut().any(|shell| {
            shell.poll();
            shell.owner_agent.is_none()
                && shell.status != ShellStatus::Running
                && !shell.completion_reported
        })
    }

    pub(crate) fn has_finished_unreported_jobs_for_session(
        &mut self,
        active_session_id: &str,
    ) -> bool {
        !active_session_id.is_empty()
            && self.processes.values_mut().any(|shell| {
                shell.poll();
                shell.owner_session_id == active_session_id
                    && shell.owner_agent.is_none()
                    && shell.status != ShellStatus::Running
                    && !shell.completion_reported
            })
    }

    /// Drain once-only completion events together with lossless stream bytes.
    /// The engine publishes the bytes outside this manager's mutex and puts
    /// only the bounded event plus resulting handle into model context.
    #[cfg(test)]
    pub(crate) fn drain_finished_jobs_with_evidence(&mut self) -> Vec<ShellCompletionEvidence> {
        self.drain_finished_jobs_with_evidence_inner(None)
    }

    pub(crate) fn drain_finished_jobs_with_evidence_for_session(
        &mut self,
        active_session_id: &str,
    ) -> Vec<ShellCompletionEvidence> {
        self.drain_finished_jobs_with_evidence_inner(Some(active_session_id))
    }

    fn drain_finished_jobs_with_evidence_inner(
        &mut self,
        active_session_id: Option<&str>,
    ) -> Vec<ShellCompletionEvidence> {
        let mut completions = Vec::new();
        for shell in self.processes.values_mut() {
            shell.poll();
            let owned = active_session_id.is_none_or(|session_id| {
                !session_id.is_empty() && shell.owner_session_id == session_id
            });
            if owned && shell.status != ShellStatus::Running && !shell.completion_reported {
                shell.completion_reported = true;
                completions.push(shell.completion_evidence());
                // The bytes are now in the caller's hands (they become a durable
                // session artifact). Holding a second copy here for the rest of
                // the retention hour is what #5472 measured.
                shell.release_delivered_output();
            }
        }
        completions.sort_by(|a, b| a.event.task_id.cmp(&b.event.task_id));
        completions
    }

    /// A terminal foreground result is already returned as the tool result;
    /// do not emit it again through the background-completion channel.
    fn acknowledge_foreground_completion(&mut self, task_id: &str) {
        if let Some(shell) = self.processes.get_mut(task_id) {
            shell.completion_reported = true;
            // The caller already holds this job's `ShellResult`; the record only
            // stays listed so `/jobs` can show it. A 1,200-char tail is all any
            // remaining consumer reads, so the rest is released now instead of
            // at the 1 h `cleanup` (#5472 finding 1 — the dominant term: every
            // uppercase `Bash` call, foreground included, went through here).
            shell.release_delivered_output();
        }
    }

    /// Whether the next production turn may inject a parent-owned shell
    /// completion event.
    ///
    /// This deliberately does not poll processes or flip
    /// `completion_reported`: preview is read-only. A running job counts as
    /// pending because it can finish before production drains completions; in
    /// that race an exact request body cannot be proved without mutation.
    #[cfg(test)]
    pub fn may_have_undelivered_completion(&self) -> bool {
        self.processes
            .values()
            .any(|shell| shell.owner_agent.is_none() && !shell.completion_reported)
    }

    pub fn may_have_undelivered_completion_for_session(&self, active_session_id: &str) -> bool {
        !active_session_id.is_empty()
            && self.processes.values().any(|shell| {
                shell.owner_session_id == active_session_id
                    && shell.owner_agent.is_none()
                    && !shell.completion_reported
            })
    }

    /// Return agent owners whose tracked shell work is still running. The
    /// engine uses this to keep a worker's heartbeat alive while its only
    /// pending work is an explicitly tracked background shell task.
    #[cfg(test)]
    pub fn running_owner_agent_ids(&mut self) -> Vec<String> {
        self.running_owner_agent_ids_inner(None)
    }

    pub fn running_owner_agent_ids_for_session(&mut self, active_session_id: &str) -> Vec<String> {
        self.running_owner_agent_ids_inner(Some(active_session_id))
    }

    /// Task ids of `owner_agent_id`'s tracked jobs that still hold their
    /// write scope, in stable order. Coordination reservations validate an
    /// owner's in-flight unbounded work against this, so it polls first like
    /// every reader: a job that exited since the last look must not keep
    /// holding a frame — and one whose stop was sent but never confirmed
    /// must, until a poll observes the exit.
    pub fn running_task_ids_for_owner(&mut self, owner_agent_id: &str) -> Vec<String> {
        let mut ids = self
            .processes
            .values_mut()
            .filter_map(|shell| {
                shell.poll();
                shell.confirm_stop_when_reaped();
                (shell.holds_write_scope()
                    && shell
                        .owner_agent
                        .as_ref()
                        .is_some_and(|owner| owner.agent_id == owner_agent_id))
                .then(|| shell.id.clone())
            })
            .collect::<Vec<_>>();
        ids.sort();
        ids.dedup();
        ids
    }

    /// Whether any of `task_ids` still holds its write scope. Reservations
    /// stamp the ids they saw at dispatch and re-ask here, so an interrupted
    /// owner's job keeps its frame occupied exactly as long as the job
    /// lives — and a killed job whose exit was never confirmed keeps it
    /// until a poll observes the child is gone.
    pub(crate) fn any_running_job(&mut self, task_ids: &[String]) -> bool {
        task_ids.iter().any(|task_id| {
            self.processes.get_mut(task_id).is_some_and(|shell| {
                shell.poll();
                shell.confirm_stop_when_reaped();
                shell.holds_write_scope()
            })
        })
    }

    /// Whether `owner_agent_id` has any running job at all. The fallback for
    /// a reservation stamped before its job got an id (the dispatch that
    /// would have stamped it was aborted mid-call): hold the frame while any
    /// owned job runs rather than guess which one is unfinished.
    pub(crate) fn owner_has_running_job(&mut self, owner_agent_id: &str) -> bool {
        !self.running_task_ids_for_owner(owner_agent_id).is_empty()
    }

    fn running_owner_agent_ids_inner(&mut self, active_session_id: Option<&str>) -> Vec<String> {
        let mut owners = self
            .processes
            .values_mut()
            .filter_map(|shell| {
                shell.poll();
                (shell.status == ShellStatus::Running
                    && active_session_id.is_none_or(|session_id| {
                        !session_id.is_empty() && shell.owner_session_id == session_id
                    }))
                .then(|| {
                    shell
                        .owner_agent
                        .as_ref()
                        .map(|owner| owner.agent_id.clone())
                })
                .flatten()
            })
            .collect::<Vec<_>>();
        owners.sort();
        owners.dedup();
        owners
    }

    /// Remember a restart-stale job so the UI can show it instead of hiding it.
    #[allow(dead_code)]
    pub fn remember_stale_job(
        &mut self,
        id: impl Into<String>,
        command: impl Into<String>,
        cwd: PathBuf,
        linked_task_id: Option<String>,
    ) {
        let id = id.into();
        self.stale_jobs.insert(
            id.clone(),
            ShellJobSnapshot {
                id: id.clone(),
                job_id: id,
                command: command.into(),
                cwd,
                status: ShellStatus::Killed,
                exit_code: None,
                elapsed_ms: 0,
                stdout_tail: String::new(),
                stderr_tail: "Process is no longer attached to this TUI session.".to_string(),
                stdout_len: 0,
                stderr_len: 0,
                stdin_available: false,
                stale: true,
                elapsed_since_output_ms: None,
                linked_task_id,
                owner_agent_id: None,
                owner_agent_name: None,
                origin_tool_call_id: None,
                origin_turn_id: None,
                owner_session_id: String::new(),
            },
        );
    }

    /// Clean up completed processes older than the given duration, then enforce
    /// the count and byte ceilings on what is left.
    ///
    /// Age alone is not a bound: it only fired from `list_jobs()`, so a session
    /// that never opened the jobs panel evicted nothing, and 500 finished
    /// records inside one hour were all retained regardless of size (#5472).
    pub fn cleanup(&mut self, max_age: Duration) {
        self.processes.retain(|_, shell| {
            // A job whose stop was sent but whose exit is unconfirmed still
            // holds its write scope, so it survives cleanup exactly like a
            // Running job: reclaiming its record would drop the only child
            // handle and let a successor writer be admitted while the old
            // child may still be alive. The bounded confirm here lets an
            // already-dead child be reaped and reclaimed in this same pass.
            shell.confirm_stop_when_reaped();
            if shell.holds_write_scope() {
                true
            } else {
                shell.started_at.elapsed() < max_age
            }
        });
        self.enforce_finished_job_bounds();
    }

    /// Bytes still held across every tracked job. The retention bound in #5472
    /// is stated in these terms, so the tests assert on them directly.
    #[cfg(all(test, unix))]
    pub(crate) fn retained_output_bytes_total(&self) -> usize {
        self.processes
            .values()
            .map(BackgroundShell::retained_output_bytes)
            .fold(0, usize::saturating_add)
    }

    #[cfg(test)]
    pub(crate) fn tracked_job_count(&self) -> usize {
        self.processes.len()
    }

    /// Drop the oldest finished records once they exceed either ceiling.
    /// Running jobs — and jobs whose stop has not been confirmed yet, which
    /// still hold their write scope — are never evicted, since killing the
    /// handle would orphan a live or potentially-live process; a job whose
    /// completion has not been delivered yet is evicted last, since dropping
    /// it loses the only copy of its result.
    fn enforce_finished_job_bounds(&mut self) {
        let mut finished = self
            .processes
            .iter()
            .filter(|(_, shell)| !shell.holds_write_scope())
            .map(|(id, shell)| {
                (
                    id.clone(),
                    shell.completion_reported,
                    shell.started_at,
                    shell.retained_output_bytes(),
                )
            })
            .collect::<Vec<_>>();
        let total_bytes: usize = finished
            .iter()
            .map(|(_, _, _, bytes)| *bytes)
            .fold(0, usize::saturating_add);
        if finished.len() <= MAX_FINISHED_SHELL_RECORDS && total_bytes <= MAX_FINISHED_SHELL_BYTES {
            return;
        }
        // Undelivered completions last, then oldest first.
        finished.sort_by(|a, b| a.1.cmp(&b.1).reverse().then_with(|| a.2.cmp(&b.2)));
        let mut remaining_count = finished.len();
        let mut remaining_bytes = total_bytes;
        for (id, _, _, bytes) in finished {
            if remaining_count <= MAX_FINISHED_SHELL_RECORDS
                && remaining_bytes <= MAX_FINISHED_SHELL_BYTES
            {
                break;
            }
            self.processes.remove(&id);
            remaining_count -= 1;
            remaining_bytes = remaining_bytes.saturating_sub(bytes);
        }
    }
}

fn job_status_rank(status: &ShellStatus, stale: bool) -> u8 {
    if stale {
        return 4;
    }
    match status {
        ShellStatus::Running => 0,
        ShellStatus::Failed | ShellStatus::TimedOut => 1,
        ShellStatus::Killed => 2,
        ShellStatus::Completed => 3,
    }
}

/// Thread-safe wrapper for `ShellManager`
pub type SharedShellManager = Arc<Mutex<ShellManager>>;

/// Create a new shared shell manager with default sandbox policy.
pub fn new_shared_shell_manager(workspace: PathBuf) -> SharedShellManager {
    Arc::new(Mutex::new(ShellManager::new(workspace)))
}

// === ToolSpec Implementations ===

use crate::command_safety::{
    SafetyLevel, agent_readonly_command_needs_shell_for_cd, analyze_command,
    extract_primary_command, is_agent_readonly_shell_command, is_parallel_readonly_command,
    normalize_windows_command_paths, readonly_sequence_segments,
    readonly_sequence_segments_with_connectors,
};
use crate::execpolicy::{ExecPolicyDecision, load_default_policy};
use crate::features::Feature;
use crate::tools::cargo_failure_summary::summarize_cargo_failure;
use crate::tools::spec::{
    ApprovalRequirement, ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec,
    optional_bool, optional_str, optional_u64, required_str, type_mismatch,
};
use async_trait::async_trait;
use serde_json::json;

const FOREGROUND_TIMEOUT_RECOVERY_HINT: &str = "Foreground Bash is for bounded commands. \
The timed-out process was killed; rerun long work as Bash action=\"run\" background=true, \
then poll with Bash action=\"wait\" task_id=\"<id>\".";

const MACOS_PROVENANCE_HINT: &str = "Docker buildx failed to update its activity file due to a macOS \
com.apple.provenance restriction. Files created by Docker Desktop's signed process carry a \
kernel-enforced provenance tag that blocks writes from child processes (including the TUI \
shell sandbox). Workarounds: (1) run the Docker build from a regular terminal outside the \
TUI, or (2) disable BuildKit with DOCKER_BUILDKIT=0 (only works if your Dockerfiles do not \
use RUN --mount directives).";

/// Human-readable exit status for a shell result: the numeric code when the
/// process returned one, or "terminated by signal" when it did not (rather
/// than leaking `Some(127)` / `None` Debug output to the user).
fn exit_code_label(code: Option<i64>) -> String {
    match (code, exit_code_hex(code)) {
        (Some(code), Some(hex)) => format!("exit code {code} ({hex})"),
        (Some(code), None) => format!("exit code {code}"),
        (None, _) => "terminated by signal".to_string(),
    }
}

fn exit_code_hex(code: Option<i64>) -> Option<String> {
    code.filter(|code| *code > i64::from(i32::MAX) && *code <= i64::from(u32::MAX))
        .map(|code| format!("0x{code:08X}"))
}
const PYTHON_BUILD_DEPENDENCY_HINT: &str = "Python build dependency missing: setuptools is not \
available in the active environment. Install the declared build requirements first, for example \
`python -m pip install -U pip setuptools wheel build`, then rerun the build command.";

fn attach_cargo_failure_summary(
    metadata: &mut serde_json::Value,
    command: &str,
    result: &ShellResult,
) {
    if let Some(summary) = summarize_cargo_failure(
        command,
        &result.stdout,
        &result.stderr,
        result.exit_code.and_then(|code| i32::try_from(code).ok()),
    ) {
        metadata["cargo_failure_summary"] = summary.to_metadata_value();
    }
}

fn attach_python_build_dependency_hint(
    metadata: &mut serde_json::Value,
    hint: Option<&'static str>,
) {
    if let Some(hint) = hint {
        metadata["python_build_dependency_hint"] = json!({
            "kind": "missing_setuptools",
            "hint": hint,
            "recommended_first_step": "python -m pip install -U pip setuptools wheel build",
        });
    }
}

pub(crate) fn looks_like_macos_provenance_failure(result: &ShellResult) -> bool {
    if matches!(result.status, ShellStatus::Completed) && result.exit_code == Some(0) {
        return false;
    }
    let combined = format!("{}\n{}", result.stdout, result.stderr).to_ascii_lowercase();
    combined.contains("com.apple.provenance")
        || combined.contains("update builder last activity")
        || (combined.contains("buildx/activity") && combined.contains("operation not permitted"))
}

fn macos_provenance_hint(result: &ShellResult) -> Option<&'static str> {
    if looks_like_macos_provenance_failure(result) {
        Some(MACOS_PROVENANCE_HINT)
    } else {
        None
    }
}

fn python_build_dependency_hint(command: &str, result: &ShellResult) -> Option<&'static str> {
    if matches!(result.status, ShellStatus::Completed) && result.exit_code == Some(0) {
        return None;
    }

    let command = command.to_ascii_lowercase();
    let combined = format!("{}\n{}", result.stdout, result.stderr).to_ascii_lowercase();
    let mentions_missing_setuptools = [
        "no module named 'setuptools'",
        "no module named \"setuptools\"",
        "setuptools is not available",
        "cannot import 'setuptools",
        "cannot import \"setuptools",
        "missing dependencies",
    ]
    .iter()
    .any(|needle| combined.contains(needle))
        && combined.contains("setuptools");
    if !mentions_missing_setuptools {
        return None;
    }

    let pythonish_command = [
        "python",
        "pip",
        "pytest",
        "tox",
        "nox",
        "cython",
        "setup.py",
        "build_ext",
    ]
    .iter()
    .any(|needle| command.contains(needle));
    let pythonish_output = [
        "setup.py",
        "pyproject.toml",
        "build_meta",
        "build_ext",
        "pep 517",
        "cython",
    ]
    .iter()
    .any(|needle| combined.contains(needle));

    if pythonish_command || pythonish_output {
        Some(PYTHON_BUILD_DEPENDENCY_HINT)
    } else {
        None
    }
}

fn command_likely_needs_network(command: &str) -> bool {
    let normalized = command.to_ascii_lowercase();
    let Some(primary) = extract_primary_command(&normalized) else {
        return false;
    };
    let primary = primary.rsplit(['/', '\\']).next().unwrap_or(primary);

    match primary {
        "curl" | "wget" | "fetch" | "nc" | "netcat" | "ncat" | "ssh" | "scp" | "sftp" | "rsync"
        | "ftp" | "ping" | "traceroute" | "nslookup" | "dig" | "host" | "nmap" | "gh" | "hub" => {
            true
        }
        "git" => [
            " fetch",
            " pull",
            " clone",
            " ls-remote",
            " submodule",
            " push",
        ]
        .iter()
        .any(|needle| normalized.contains(needle)),
        "cargo" => [" install", " fetch", " update", " publish", " search"]
            .iter()
            .any(|needle| normalized.contains(needle)),
        "npm" | "pnpm" | "yarn" => [" install", " i", " add", " update", " publish"]
            .iter()
            .any(|needle| normalized.contains(needle)),
        "pip" | "pip3" | "uv" | "poetry" => [" install", " add", " sync", " update"]
            .iter()
            .any(|needle| normalized.contains(needle)),
        "brew" | "apt" | "apt-get" | "yum" | "dnf" | "pacman" => true,
        "go" => [" get", " install", " mod download"]
            .iter()
            .any(|needle| normalized.contains(needle)),
        _ => false,
    }
}

fn looks_like_network_blocked_failure(result: &ShellResult) -> bool {
    if matches!(result.status, ShellStatus::Completed | ShellStatus::Running)
        || result.exit_code == Some(0)
    {
        return false;
    }

    if result.stdout.trim() == "000" {
        return true;
    }
    if result.sandboxed && result.stdout.is_empty() && result.stderr.is_empty() {
        return true;
    }

    let output = format!("{}\n{}", result.stdout, result.stderr).to_ascii_lowercase();
    [
        "operation not permitted",
        "network is unreachable",
        "could not resolve host",
        "couldn't resolve host",
        "failed to resolve",
        "temporary failure in name resolution",
        "name or service not known",
        "nodename nor servname provided",
        "no address associated",
        "failed to connect",
        "couldn't connect",
        "connection timed out",
        "connection reset",
    ]
    .iter()
    .any(|pattern| output.contains(pattern))
}

fn shell_network_restricted_hint<'a>(
    context: &'a ToolContext,
    command: &str,
    result: &ShellResult,
) -> Option<&'a str> {
    let hint = context.shell_network_denied_hint.as_deref()?;
    let policy_blocks_network = context
        .elevated_sandbox_policy
        .as_ref()
        .is_some_and(|policy| !policy.has_network_access());
    if !policy_blocks_network || !command_likely_needs_network(command) {
        return None;
    }
    if result.sandbox_denied || looks_like_network_blocked_failure(result) {
        Some(hint)
    } else {
        None
    }
}

/// Coaching line when the execution sandbox denied a command and the
/// Plan-mode network hint did not already explain it. Most often a write under
/// a read-only posture: name the effective posture and the Ask-only retry shape
/// so other postures do not mistake it for autonomous authority.
fn shell_sandbox_denied_hint(context: &ToolContext, result: &ShellResult) -> Option<String> {
    if !result.sandbox_denied {
        return None;
    }
    let policy = context.elevated_sandbox_policy.as_ref()?;
    Some(format!(
        "The execution sandbox blocked this command. Effective sandbox posture: {}. [sandbox: Ask-only escalation — retry this exact command once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]",
        policy.posture_label()
    ))
}

fn shell_job_owner_from_context(context: &ToolContext) -> Option<ShellJobOwner> {
    let agent_id = context
        .owner_agent_id
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())?;
    let agent_name = context
        .owner_agent_name
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .unwrap_or(agent_id);
    Some(ShellJobOwner {
        agent_id: agent_id.to_string(),
        agent_name: agent_name.to_string(),
    })
}

fn shell_work_lifecycle_from_context(context: &ToolContext) -> Option<ShellWorkLifecycle> {
    context
        .runtime
        .work
        .as_ref()
        .map(|work| ShellWorkLifecycle {
            work: work.clone(),
            session_id: context.state_namespace.clone(),
        })
}

fn lifecycle_now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(i64::MAX)
}

fn attach_shell_owner_metadata(metadata: &mut serde_json::Value, context: &ToolContext) {
    let Some(owner) = shell_job_owner_from_context(context) else {
        return;
    };
    metadata["owner_agent_id"] = json!(owner.agent_id);
    metadata["owner_agent_name"] = json!(owner.agent_name);
}

/// Whether the resolved shell kind can bind an admitted compound at all.
/// `cmd.exe` treats `;` as an argument delimiter, not a command separator, so
/// a `;`-sequence could neither run nor fail as a sequence there; it expands
/// `%NAME%` from the environment before any program sees the word; and it
/// consumes a bare `^` as an escape character in the same pre-parse, so
/// `cat a^b` opens `ab` — a spelling the operand gate judged as a
/// nonexistent name it could skip (`%`'s twin is zsh's `=cmd` expansion,
/// which the gate refuses outright; `^`'s is the `=` family too). PowerShell
/// executes `;`-sequences, reads `%`/`^` literally, and `&&`/`||` parse on
/// PowerShell 7+; the Windows PowerShell 5.1 parse error is a safe
/// nothing-ran failure. Pure so the platform-dependent decision is
/// unit-testable on every host.
/// The read-only lane's shell-startup env walls: non-interactive zsh
/// sources `$ZDOTDIR/.zshenv`, where operator-set options such as
/// EXTENDED_GLOB could turn charset-admitted characters into live globs —
/// an EMPTY ZDOTDIR makes zsh fall back to `$HOME/.zshenv`, the exact file
/// this wall bypasses, so the override points at `/`; bash's GLOBIGNORE
/// could likewise widen what a pattern matches. Pure so the values pin at
/// the table (the child's live env is not observable through the admitted
/// grammar — env-reading commands refuse).
fn readonly_shell_startup_env_walls() -> [(&'static str, &'static str); 2] {
    [("ZDOTDIR", "/"), ("GLOBIGNORE", "")]
}

/// The fixed spawn-env overrides every admitted read-only child runs under,
/// beyond the computed GIT_* pin block: the startup-env walls (see
/// [`readonly_shell_startup_env_walls`]) plus the `CDPATH` clear (a hostile
/// CDPATH rewrites what a bare `cd sub` resolves to), the ripgrep config
/// clear (a hostile `RIPGREP_CONFIG_PATH` re-arms flags on an admitted
/// `rg`), and the `BASHOPTS`/`SHELLOPTS` clears — bash honors both at
/// startup from the process environment it inherits (`BASHOPTS=dotglob`
/// widens `*` over dotfiles the GLOBIGNORE wall keeps cold; `SHELLOPTS`
/// arms `set -o` spellings). The tool process's environment is operator
/// territory, the same reason the ZDOTDIR wall exists. Extracted so the
/// application site has one pinned source and a dropped call site turns
/// into dead code under `deny(warnings)`.
fn readonly_spawn_env_overrides() -> Vec<(&'static str, String)> {
    let mut overrides: Vec<(&'static str, String)> = readonly_shell_startup_env_walls()
        .iter()
        .map(|(key, value)| (*key, value.to_string()))
        .collect();
    overrides.push(("CDPATH", String::new()));
    overrides.push(("RIPGREP_CONFIG_PATH", String::new()));
    overrides.push(("BASHOPTS", String::new()));
    overrides.push(("SHELLOPTS", String::new()));
    overrides
}

fn readonly_compound_refused_by_shell_kind(
    kind: &crate::shell_dispatcher::ShellKind,
    command: &str,
) -> Option<&'static str> {
    // PowerShell argument mode splits a bare `,` into several arguments
    // (`cat a,b` binds two), so the gate would judge one word where the
    // shell passes many — planted outside symlinks spelled `a` and `b`
    // would both open unjudged. The refusal covers quoted commas too,
    // mirroring the cmd `%` wall's any-occurrence shape.
    if kind.is_powershell() && command.contains(',') {
        return Some(
            "[shell.readonly.sequence.unsupported_shell] PowerShell argument mode splits a bare `,` into several arguments, so a read-only probe containing one could run with operands the gate judged as a single word (quoted occurrences refuse too, fail-closed); run the probe without `,`.",
        );
    }
    if !matches!(kind, crate::shell_dispatcher::ShellKind::Cmd) {
        return None;
    }
    // cmd.exe has no comment syntax, but the tokenizer this gate shares with
    // the classifier (shlex) drops a `#`-word-start run to end-of-line as a
    // POSIX comment: `cat f # x | wc -l` is judged as the words `cat` and `f`
    // alone, while cmd.exe passes `#` and `x` as live argv words the gate
    // never judged — a planted outside symlink spelled `#` opens unjudged.
    // The POSIX and PowerShell lanes run LESS than the gate judged (they do
    // honor `#` comments), so this wall is cmd-only, and any occurrence
    // refuses rather than trying to re-derive shlex's word-start rule on raw
    // text — nothing in the admitted grammar legitimately carries `#`.
    // Quoted occurrences refuse too, mirroring the `%` wall's shape.
    if command.contains('#') {
        return Some(
            "[shell.readonly.sequence.unsupported_shell] cmd.exe has no comment syntax, so a read-only probe carrying `#` could run words the gate judged as a comment (quoted occurrences refuse too, fail-closed); run the probe without `#`.",
        );
    }
    // cmd.exe tokenizes `,` and `=` as argument delimiters when it passes
    // the line to a program (the same phase that splits spaces — `mkdir a,b`
    // creates two directories), so `cat a,b | wc -l` runs with argv words
    // `a` and `b` while the gate judged the single word `a,b` — planted
    // outside symlinks spelled `a` and `b` would both open unjudged. The
    // admitted grammar carries no `=`- or `,`-bearing option, so any
    // occurrence reaching a compound is an operand word the shell will
    // split; quoted occurrences refuse too, mirroring the `%` wall's
    // any-occurrence shape.
    if command.contains(',') || command.contains('=') {
        return Some(
            "[shell.readonly.sequence.unsupported_shell] cmd.exe splits `,` and `=` into separate argument words when it runs a command, so a read-only probe containing either could run with operands the gate judged as a single word (quoted occurrences refuse too, fail-closed); run the probe without `,` or `=`.",
        );
    }
    if command.contains('%') || command.contains('^') {
        return Some(
            "[shell.readonly.sequence.unsupported_shell] cmd.exe expands `%NAME%` and consumes `^` before the command runs, so a read-only probe containing either could run under a spelling the gate never judged; run one command at a time without `%` or `^`.",
        );
    }
    if readonly_sequence_segments(command).is_some_and(|segments| segments.len() > 1) {
        return Some(
            "[shell.readonly.sequence.unsupported_shell] multi-segment cmd compound probes refuse as a fail-closed per-shell decision: `;` is an argument delimiter there, so every `;`/`&&`/`||` sequence is refused for a cmd shell, while a single-stage `|` pipeline still runs once the raw-text walls above (`#`, `%`, `^`, `,`, `=`) pass; run the probes one at a time.",
        );
    }
    None
}

/// The shell-lane command text for one admitted read-only compound: the
/// `set -o pipefail` prefix is prepended only when a `|` is present (a real
/// pipeline, or harmlessly an `||` sequence, where the option is a no-op) AND
/// the resolved shell binary is known to speak it. The decision is keyed on
/// the binary, not the kind: `/bin/sh` is bash on macOS but dash on Debian
/// and Ubuntu, and dash (like busybox ash) exits the whole `-c` payload on
/// `set -o pipefail` — the prefix would turn every admitted pipeline into a
/// nothing-ran failure there. bash, zsh, and the ksh family honor it; every
/// other binary runs the compound unprefixed and reports the last stage's
/// status, which is reporting fidelity, not a boundary. cmd.exe mangles the
/// `;`-bearing fragment and PowerShell errors on the statement, so Windows
/// kinds never receive it. Pure for tests.
fn readonly_shell_bound_command(
    kind: &crate::shell_dispatcher::ShellKind,
    command: &str,
) -> String {
    if command.contains('|') && shell_binary_speaks_pipefail(kind) {
        format!("set -o pipefail; {command}")
    } else {
        command.to_string()
    }
}

/// Whether the shell binary behind `kind` accepts `set -o pipefail`: keyed on
/// the executable's file name, since `$SHELL` and `/bin/sh` can resolve to
/// anything.
fn shell_binary_speaks_pipefail(kind: &crate::shell_dispatcher::ShellKind) -> bool {
    if matches!(
        kind,
        crate::shell_dispatcher::ShellKind::Cmd
            | crate::shell_dispatcher::ShellKind::Pwsh
            | crate::shell_dispatcher::ShellKind::WindowsPowerShell
    ) {
        return false;
    }
    let binary = std::path::Path::new(kind.binary())
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or_default();
    matches!(binary, "bash" | "zsh" | "ksh" | "ksh93" | "mksh")
}

fn enforce_readonly_github_network_policy(
    command: &str,
    context: &ToolContext,
) -> Result<(), ToolError> {
    // Judge every pipeline stage of every sequence segment with the same
    // splits the classifier used: `pwd; gh repo view` and `gh repo view |
    // head` reach the network exactly like a bare `gh repo view`, so they
    // must consult the decider exactly like it. The shared predicate is
    // token-level per stage, not the whole-command parallel classifier: the
    // agent grammar admits `*` operands, and a stage like `gh pr list
    // --search '*'` reaches the network while a charset-gated predicate
    // would un-admit it and skip the policy.
    if !crate::command_safety::command_reaches_github_readonly(command) {
        return Ok(());
    }
    let Some(decider) = context.network_policy.as_ref() else {
        return Ok(());
    };

    use crate::network_policy::Decision;
    match decider.evaluate("api.github.com", "Bash") {
        Decision::Allow => Ok(()),
        Decision::Deny => Err(ToolError::permission_denied(
            "Read-only GitHub CLI access to 'api.github.com' is blocked by the active network policy."
                .to_string(),
        )),
        Decision::Prompt => Err(ToolError::permission_denied(
            "Read-only GitHub CLI access to 'api.github.com' requires network approval; allow that host in the parent session or network policy before dispatching the read-only probe lane."
                .to_string(),
        )),
    }
}

/// `exec_shell_input_is_parallel_readonly` with the agent-posture classifier:
/// same input-shape restrictions (run action only, no background/tty/stdin),
/// but commands are judged by [`is_agent_readonly_shell_command`] so
/// `ShellPolicy::ReadOnly` agents keep a usable inspection surface
/// (pipelines, globs, `git -C`, `find`, `sed -n`, `npm view`).
fn exec_shell_input_agent_readonly(input: &serde_json::Value) -> bool {
    if !exec_shell_input_is_parallel_readonly_shape(input) {
        return false;
    }
    let command = input
        .get("command")
        .and_then(serde_json::Value::as_str)
        .expect("shape check established a command string");
    is_agent_readonly_shell_command(command)
}

/// `exec_shell_input_agent_readonly` is also the gate-side predicate for the
/// subagent posture check (#5426): the catalog carve-out that admits
/// canonical `bash` to the read-only inspection roles (Scout/Reviewer/
/// Planner) and to a `test` child whose session grants the bounded probe
/// lane must judge the same call the
/// `BashTool::execute` `ShellPolicy::ReadOnly` branch will judge, so the
/// posture gate can admit a proven-readonly call without ever widening past
/// the execute-time refusal.
pub(crate) fn agent_readonly_bash_input(input: &serde_json::Value) -> bool {
    // Canonical lowercase `bash` advertises `timeout` in seconds, while the
    // internal executor contract uses `timeout_ms`. Normalize through the same
    // translator the concrete tool uses so posture, session approval, envelope,
    // and execute judge one input (#5595). Legacy/internal shapes fall back to
    // their existing direct classification. Input-contract note: the read-only
    // posture is narrower than the concrete tool — a legacy call carrying
    // `justification`/`sandbox_permissions` translates into a shape the
    // strict read-only gate refuses (those keys are Ask-mode concepts with no
    // read-only meaning), so such a call fails closed at the posture where
    // the concrete tool would have accepted the key. That disagreement is
    // deliberate: the lane must not inherit Ask-mode escalation surfaces.
    let translated = contract_bash_legacy_input(input).unwrap_or_else(|_| input.clone());
    exec_shell_input_agent_readonly(&translated)
}

fn exec_shell_input_is_parallel_readonly(input: &serde_json::Value) -> bool {
    if !exec_shell_input_is_parallel_readonly_shape(input) {
        return false;
    }
    let command = input
        .get("command")
        .and_then(serde_json::Value::as_str)
        .expect("shape check established a command string");
    is_parallel_readonly_command(command)
}

fn exec_shell_input_is_parallel_readonly_shape(input: &serde_json::Value) -> bool {
    let Some(fields) = input.as_object() else {
        return false;
    };
    if fields
        .keys()
        .any(|key| !matches!(key.as_str(), "action" | "command" | "cwd" | "timeout_ms"))
    {
        return false;
    }
    match input.get("action") {
        None | Some(serde_json::Value::Null) => {}
        Some(serde_json::Value::String(action)) if action == "run" => {}
        Some(_) => return false,
    }
    if ["background", "interactive", "tty", "combined_output"]
        .iter()
        .any(|key| {
            !matches!(
                input.get(*key),
                None | Some(serde_json::Value::Null | serde_json::Value::Bool(false))
            )
        })
    {
        return false;
    }
    if ["stdin", "input", "data"]
        .iter()
        .any(|key| input.get(*key).is_some())
    {
        return false;
    }
    if ["task_id", "id", "wait", "block", "close_stdin", "all"]
        .iter()
        .any(|key| input.get(*key).is_some())
    {
        return false;
    }

    input
        .get("command")
        .and_then(serde_json::Value::as_str)
        .is_some()
}

fn hardened_readonly_argv(command: &str) -> Result<(String, Vec<String>)> {
    let mut argv = shlex::split(&normalize_windows_command_paths(command))
        .ok_or_else(|| anyhow!("could not parse classifier-approved read command"))?;
    if argv.is_empty() {
        return Err(anyhow!("classifier-approved read command was empty"));
    }
    splice_git_read_hardening(&mut argv)?;
    let program = argv.remove(0);
    Ok((program, argv))
}

/// Splice the flags that keep a classifier-admitted Git read inside Git's own
/// process into `argv`, in place: repository or user configuration may name a
/// diff driver (`diff.<driver>.command`), a textconv filter, or a signature
/// helper, and `log`/`show`/`diff` would otherwise run it — as would `blame`,
/// which applies textconv filters by default. Returns whether anything was
/// spliced — the remaining admitted subcommands (`status`, `ls-files`, and
/// `grep`, whose textconv is opt-in and whose option table admits no
/// `--textconv`) consult no driver. `argv` that is not a Git command is left
/// untouched. One table for both execution lanes: the direct-argv lane
/// splices into the argv it execs, and the shell lane splices into every Git
/// stage of a compound before the shell sees it
/// ([`harden_readonly_shell_git_stages`]).
fn splice_git_read_hardening(argv: &mut Vec<String>) -> Result<bool> {
    if !argv.first().is_some_and(|program| program == "git") {
        return Ok(false);
    }
    // The agent read-only classifier admits `git -C <dir>` and
    // `git --no-pager` before the subcommand; keep the preamble but locate
    // the subcommand after it so the hardening flags splice in the right
    // place. `-C` targets were already workspace-checked by
    // `enforce_readonly_workspace_operands`.
    let subcommand_index = readonly_git_preamble_len(argv);
    let subcommand = argv
        .get(subcommand_index)
        .map(String::as_str)
        .ok_or_else(|| {
            anyhow!("classifier-approved Git read was missing its literal subcommand")
        })?;
    let at = subcommand_index + 1;
    match subcommand {
        "diff" => {
            argv.splice(
                at..at,
                ["--no-ext-diff".to_string(), "--no-textconv".to_string()],
            );
            Ok(true)
        }
        "log" | "show" => {
            argv.splice(
                at..at,
                [
                    "--no-ext-diff".to_string(),
                    "--no-textconv".to_string(),
                    "--no-show-signature".to_string(),
                ],
            );
            Ok(true)
        }
        "blame" => {
            // `git blame` demonstrably runs the repository's textconv
            // filter (pinned red/green by
            // `readonly_shell_lane_git_show_does_not_run_repository_textconv`),
            // so it keeps its `--no-textconv` splice.
            argv.splice(at..at, ["--no-textconv".to_string()]);
            Ok(true)
        }
        // Main's #123/#124 widening (rebase reconciliation): the read-only
        // forms of these spawn no configured helper — external
        // diff/textconv/signature verification belong to diff/log/show
        // (and blame, above) only — and their read-only SHAPE (rev-parse's
        // bounded flag table, branch's listing forms, worktree list's
        // flag-only arguments) is the classifier's contract; this hardener
        // only keeps the read inside git's own process.
        "status" | "ls-files" | "grep" | "rev-parse" | "branch" | "worktree" => Ok(false),
        _ => Err(anyhow!(
            "classifier-approved Git read did not keep its subcommand in argv[1]"
        )),
    }
}

/// The shell-lane text of an admitted read-only compound with every Git
/// `log`/`show`/`diff`/`blame` stage hardened like the direct-argv lane hardens a
/// bare one. A compound hands the shell the raw command text, so without
/// this `git show HEAD | cat` ran whatever `diff.<driver>.textconv` the
/// repository's own configuration named — arbitrary execution from an
/// admitted read — while bare `git show HEAD` was spliced and safe; the
/// `GIT_*` environment hardening pins global and system configuration, not
/// the repository's. Every stage is re-emitted from the argv the operand gate
/// already parsed (so the rewrite can never see a different token boundary
/// than the gate judged), spliced in place of the trimmed stage text, with the
/// separators between stages copied through byte for byte. A stage whose
/// tokens cannot be re-quoted portably refuses rather than running unhardened.
fn harden_readonly_shell_git_stages(command: &str) -> Result<String> {
    let Some(segments) = readonly_sequence_segments(command) else {
        return Err(anyhow!(
            "read-only compound contains a bare `&` and matches no admitted shape"
        ));
    };
    let base = command.as_ptr() as usize;
    let mut hardened = String::with_capacity(command.len() + 64);
    let mut copied_up_to = 0;
    for segment in segments {
        for stage in crate::command_safety::readonly_pipeline_stages(segment) {
            let trimmed = stage.trim();
            let Some(replacement) = hardened_readonly_git_stage(trimmed)? else {
                continue;
            };
            let start = trimmed.as_ptr() as usize - base;
            hardened.push_str(&command[copied_up_to..start]);
            hardened.push_str(&replacement);
            copied_up_to = start + trimmed.len();
        }
    }
    hardened.push_str(&command[copied_up_to..]);
    Ok(hardened)
}

/// One stage of [`harden_readonly_shell_git_stages`]: `Some(text)` for a Git
/// stage that received hardening flags, `None` for every other stage.
fn hardened_readonly_git_stage(stage: &str) -> Result<Option<String>> {
    let Some(mut argv) = shlex::split(&normalize_windows_command_paths(stage)) else {
        return Err(anyhow!(
            "could not parse classifier-approved read-only stage"
        ));
    };
    if !splice_git_read_hardening(&mut argv)? {
        return Ok(None);
    }
    let mut words = Vec::with_capacity(argv.len());
    for token in &argv {
        words.push(readonly_shell_word(token)?);
    }
    Ok(Some(words.join(" ")))
}

/// Re-emit one parsed token as shell text that every supported shell kind
/// reads back as the same single word: bare when it is made only of
/// characters that read back unchanged on sh and cmd.exe alike (`,` and `=`
/// stay bare for the POSIX shells, but both split cmd.exe argument words
/// and PowerShell argument mode splits a bare comma — so a `,`- or
/// `=`-bearing command refuses at the per-kind wall in
/// `readonly_compound_refused_by_shell_kind` before re-emission;
/// `*` deliberately stays bare — the operand gate judged its expansion and
/// the shell must still perform it; a `-C` value carrying one never gets
/// here, it refuses at the root resolver; `%` and `^` do not qualify, since
/// cmd.exe expands `%NAME%` in bare words and consumes a bare `^` as its
/// escape),
/// wrapped in double quotes when it merely carries whitespace, single
/// quotes, or those cmd.exe-sensitive characters, and refused when it holds a
/// character whose quoting rules differ between sh, cmd.exe, and PowerShell.
/// The charset gate already refuses `$`, backticks, and newlines; the
/// remaining refusals are rare spellings of a probe that runs fine as a
/// single command on the direct-argv lane.
fn readonly_shell_word(token: &str) -> Result<String> {
    let bare = !token.is_empty()
        && token.chars().all(|ch| {
            ch.is_ascii_alphanumeric()
                || matches!(
                    ch,
                    '_' | '-' | '.' | '/' | ':' | '=' | ',' | '+' | '@' | '*'
                )
        });
    if bare {
        return Ok(token.to_string());
    }
    if token
        .chars()
        .any(|ch| matches!(ch, '"' | '\\' | '$' | '`' | '\n' | '\r' | '!'))
    {
        return Err(anyhow!(
            "[shell.readonly.git.unquotable_stage] Read-only Git stage argument {token:?} cannot be re-quoted portably for the shell lane; run the Git read as a single command instead."
        ));
    }
    Ok(format!("\"{token}\""))
}

/// Canonical boundary set for the read-only shell: every declared
/// workspace root (primary first), canonicalized. Attached roots carry the
/// same trust weight as the primary — they are agent-writable, so a program
/// or PATH entry under any of them is untrusted, while operands under them
/// were already admitted by the roots-aware `resolve_path`. Roots that do not
/// resolve on disk cannot contain anything and drop out: fail-closed for
/// operand admission, and a non-existent root can shadow neither a PATH entry
/// nor an executable.
fn canonical_readonly_roots(roots: &[PathBuf]) -> std::io::Result<Vec<PathBuf>> {
    let Some((primary, extra)) = roots.split_first() else {
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "read-only boundary requires at least the primary root",
        ));
    };
    let mut canonical = vec![primary.canonicalize()?];
    // Round-24 P3 perf: the Vec `contains` made the dedup O(N²) in the
    // root-set size — a set keeps the order-preserving first-wins dedup
    // linear. Behavior identical (first canonical spelling wins).
    let mut seen: std::collections::HashSet<PathBuf> = canonical.iter().cloned().collect();
    for root in extra {
        if let Ok(resolved) = root.canonicalize()
            && seen.insert(resolved.clone())
        {
            canonical.push(resolved);
        }
    }
    Ok(canonical)
}

/// Upper bound on distinct git content-filter names one command may ask the
/// child env to neutralize; beyond it the gate refuses instead of pinning a
/// partial set.
const MAX_READONLY_GIT_FILTER_PINS: usize = 8;

/// Length bound for one discovered filter name: a name past it refuses at
/// the unspellable-name wall like any other spelling the child env block
/// cannot pin faithfully.
const MAX_DISCOVERED_FILTER_NAME_BYTES: usize = 64;

/// Upper bound on distinct repositories one command's filter discovery may
/// scan, counting every registered submodule a scanned repository declares;
/// beyond it the gate refuses instead of pinning a partial set.
const MAX_READONLY_GIT_REPOS_SCANNED: usize = 32;

/// Wall-clock bound on one command's whole filter discovery, mirroring the
/// glob walk's expansion budget. The per-repo count (32) and the per-child
/// watchdog (10 s) bound the scans individually but not together: a hostile
/// workspace chaining 32 slow-but-not-failing repositories would otherwise
/// buy minutes of pre-spawn host work per command (measured: one FIFO-index
/// repository lands its own refusal at the watchdog; a benign 33-repo
/// workspace scans in ~100 ms, so the aggregate never fires on real work).
const MAX_READONLY_DISCOVERY_WALL_CLOCK: std::time::Duration = std::time::Duration::from_secs(10);

/// Read bound for one repository config file during filter discovery.
const READONLY_GIT_CONFIG_SCAN_CAP: usize = 256 * 1024;

/// Read bound for one short git metadata file (a `.git` link or a
/// `commondir` pointer): git consumes the whole value (an unbounded run of
/// trailing `\r`/`\n` bytes stripped, the first NUL ending it), and a real
/// value is a path that fits far below this cap; a file at the cap names a
/// target git itself could not open either.
const READONLY_GIT_META_SCAN_CAP: usize = 4 * 1024;

/// Wall-clock bound on one pre-spawn discovery git child. The child opens
/// repository files itself (`.git/index`, `HEAD`, refs): a FIFO at any of
/// them blocks its own `open` forever, and this runs before spawn where no
/// watchdog bounds anything — the bounded-reader wall covers only the files
/// this process opens. The watchdog kills the child at the bound, the read
/// loop then sees end-of-file, and the command refuses.
const READONLY_DISCOVERY_CHILD_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// The watchdog's poll tick. The quantum is latency every admitted git
/// command pays — the reaping main thread joins this thread, which only
/// wakes on the next tick — so it stays far below the fork cost it wraps,
/// and the value is pinned (`readonly_discovery_watchdog_poll_quantum`) so
/// the latency claim cannot regress silently. The kill time itself is
/// quantum-insensitive (the sleep-then-check loop lands the kill at the
/// first tick at-or-past the deadline), which is why the bounded-child pin
/// asserts an elapsed window around the bound while this const is pinned
/// for the latency.
const READONLY_DISCOVERY_WATCHDOG_POLL: std::time::Duration = std::time::Duration::from_millis(2);

/// Read bound for one `git ls-files --stage` listing during filter
/// discovery: a repository's index enumerates every tracked path, so the
/// honest bound is generous — and one byte over it refuses rather than
/// trusting a truncated submodule set.
const READONLY_GIT_INDEX_SCAN_CAP: usize = 16 * 1024 * 1024;

/// Oldest git release whose config the child env block can pin exactly the
/// way this lane needs. 2.31.0 introduced the `GIT_CONFIG_KEY_n` /
/// `GIT_CONFIG_VALUE_n` / `GIT_CONFIG_COUNT` feed, but the block also turns
/// the operator's global/system config off with `GIT_CONFIG_GLOBAL` /
/// `GIT_CONFIG_NOSYSTEM`, and `GIT_CONFIG_GLOBAL` arrived only in 2.32.0 —
/// on 2.31.x the feed pins would take while the operator's global config
/// still loaded, so a repo `.gitattributes` could reference a filter
/// defined only there that the repository-only config scan would never
/// discover. On an older git the environment block silently does not exist
/// — the fixed fsmonitor/hooksPath pins AND the discovered filter pins
/// alike — and for `status`/`ls-files`/`grep` those pins are the only
/// wall, so the lane version-checks the host git for every command with a
/// read-only git stage instead of admitting under pins that would not
/// take.
const MIN_GIT_ENV_CONFIG_VERSION: (u64, u64) = (2, 32);

/// Total-bytes bound on the path-text directory-identity keys a non-unix
/// glob walk may store (unix keys are fixed-size `(dev, ino, remaining)`
/// triples); without it, deep wide trees at the node cap could transiently
/// hold hundreds of megabytes of key text.
#[cfg(not(unix))]
const MAX_READONLY_GLOB_ENTERED_KEY_BYTES: usize = 8 * 1024 * 1024;

/// One declared git content filter and the conversion keys it arms. Each
/// armed key gets its own env pin — and only the armed keys: pinning
/// `process` for a repo that declared none would *introduce* a protocol
/// filter (git prefers it over clean/smudge) and turn a working read into a
/// protocol failure.
#[derive(Debug)]
struct ReadonlyGitFilterPin {
    name: String,
    clean: bool,
    smudge: bool,
    process: bool,
}

impl ReadonlyGitFilterPin {
    fn new(name: String) -> Self {
        Self {
            name,
            clean: false,
            smudge: false,
            process: false,
        }
    }

    fn arms_a_conversion(&self) -> bool {
        self.clean || self.smudge || self.process
    }
}

/// Discover the git content filters the repositories a command's git stages
/// run under, so the child env can pin each armed conversion key to an inert
/// helper. A `.gitattributes` filter assignment only executes when the
/// repository's own config defines `filter.<name>.clean`/`smudge`/`process`
/// — an attribute naming an undefined filter is inert, git skips the
/// conversion — and repository config is the one configuration source the
/// read-only env block does not already pin away. Scanning the config of
/// every repository a stage can reach is therefore complete: every returned
/// key is pinned with `filter.<name>.<key>`, which is command-scoped config
/// and outranks the repository's own value, turning a declared filter into
/// a byte-for-byte pass-through (`cat`) or — for the protocol (`process`)
/// form — into a helper whose handshake fails loudly, so a protocol filter
/// degrades its repository's conversions to errors rather than ever running
/// its configured command. Anything the scan cannot confidently enumerate
/// refuses the command instead of pinning a partial set: config `include`
/// directives, the worktree-config extension, a `core.worktree` redirect,
/// unspellable filter names, a `core.worktree` declaration that does not
/// resolve back to the worktree the repository was discovered in, and more
/// names or repositories than the scan bounds. Non-UTF-8 config bytes
/// decode lossily like any `str` conversion; a name carrying the
/// replacement marker then refuses at the unspellable-name wall, so the
/// decode can never silently mask a driver.
///
/// The enumeration covers more than the stage's own repository: git forks
/// its own `git status`/`git diff` child into any index gitlink whose path
/// holds a repository, and that child converts content under the
/// submodule's repository config — probe-pinned, it fires even when
/// `.gitmodules` is gone from the worktree (the file is only a label on
/// git's fork; the index's gitlink is the criterion) and even when the
/// superproject reports a clean tree, and it inherits these env pins
/// intact — so every repository the scanned repo's index registers as a
/// gitlink is scanned too, through one hardened helper (`run_discovery_git`)
/// that execs git with the same environment the read-only child runs under.
/// Filter names are matched byte-exactly the way git matches an attribute
/// value against a config subsection (`Mix` and `mix` are two drivers), and
/// the config parser mirrors git's own: a leading UTF-8 BOM is skipped, one
/// trailing `\r` per line (CRLF) is stripped, `;`/`#` start comments outside
/// double quotes anywhere in a line (the quote state carrying across
/// continuation lines), keys on a section-header line parse, a line can
/// re-target the section with further `[...]` headers before its key
/// (`[a] [b] k = v` parses `b.k`), headers parse in all four git shapes
/// (`[base]`, the dotted `[base.subsection]` with a case-folded subsection,
/// the quoted `[base "Sub"]` byte-exact, and the combined `[filter.x "y"]`
/// = subsection `x.y`), and a value continues over the next physical lines
/// only when the comment-stripped text ends in an odd number of
/// backslashes. When a command reaches a git stage at all, the host git is
/// version-checked: the env-config mechanism the whole block rides needs
/// git >= 2.32 (the feed pins need 2.31, and turning the operator's global
/// config off needs 2.32's `GIT_CONFIG_GLOBAL`), and on an older git it
/// would silently not exist or not take.
///
/// The walk mirrors the operand gate's `cd` fold so a git stage that runs
/// inside a nested repository (`cd sub; git status`) scans the repository it
/// will actually run under. Run after the operand gate passed, so every `cd`
/// is one canonicalizable relative directory joined by `;` or the sequence
/// start.
///
/// One precedence fact is load-bearing here: command-line `-c key=value`
/// outranks every `GIT_CONFIG_*` environment pin, so the classifier's
/// refusal of `-c`-bearing git stages (the git grammar consumes only
/// `--no-pager`/`-C` before the subcommand) is the wall that keeps this
/// whole block honest. Any future admission of a git global option must
/// re-derive that refusal first.
fn discover_readonly_git_filter_pins(
    command: &str,
    effective_cwd: &std::path::Path,
    roots: &[PathBuf],
) -> Result<Vec<ReadonlyGitFilterPin>, ToolError> {
    // No git stage, nothing to scan: keep the no-repository dispatch at
    // zero filesystem reads.
    let has_git_stage =
        readonly_sequence_segments_with_connectors(command).is_some_and(|segments| {
            segments.iter().any(|(segment, _)| {
                crate::command_safety::readonly_pipeline_stages(segment)
                    .iter()
                    .any(|stage| {
                        readonly_segment_argv(stage)
                            .map(|argv| argv.first().is_some_and(|program| program == "git"))
                            .unwrap_or(false)
                    })
            })
        });
    if !has_git_stage {
        return Ok(Vec::new());
    }
    let mut pins: Vec<ReadonlyGitFilterPin> = Vec::new();
    let mut scanned: std::collections::HashSet<PathBuf> = std::collections::HashSet::new();
    let mut repos_remaining = MAX_READONLY_GIT_REPOS_SCANNED;
    let discovery_deadline = std::time::Instant::now() + MAX_READONLY_DISCOVERY_WALL_CLOCK;
    let mut live_cwd = effective_cwd.to_path_buf();
    // The raw PATH the discovery git's sanitized environment is built from,
    // read once per pass so every exec in the walk sees the same search set.
    let env_path = std::env::var_os("PATH");
    let Some(segments) = readonly_sequence_segments_with_connectors(command) else {
        return Ok(Vec::new());
    };
    for (segment, connector) in segments {
        let stages = crate::command_safety::readonly_pipeline_stages(segment);
        for stage in &stages {
            let Ok(argv) = readonly_segment_argv(stage) else {
                continue;
            };
            if !argv.first().is_some_and(|program| program == "git") {
                continue;
            }
            // The stage root through the same helper the operand gate used,
            // so the repository scanned is exactly the one git runs under —
            // a chained `-C A -C escape` resolves cumulatively, not against
            // the flat cwd.
            let stage_root = readonly_git_stage_root(&argv, &live_cwd, roots)
                .map_err(|reason| {
                    ToolError::permission_denied(format!(
                        "[shell.readonly.git.filter_config] A git stage's `-C` preamble could not be resolved inside the workspace: {reason}"
                    ))
                })?;
            scan_readonly_repository_at(
                &stage_root,
                &mut pins,
                &mut scanned,
                &mut repos_remaining,
                roots,
                env_path.as_deref(),
                discovery_deadline,
            )?;
        }
        if stages.len() > 1 {
            continue;
        }
        let Ok(argv) = readonly_segment_argv(segment) else {
            continue;
        };
        match argv.first().map(String::as_str) {
            // A `cd` its connector may not run refuses at the gate, so every
            // `cd` that reaches this walk always runs; fold exactly where the
            // gate folds.
            Some("cd")
                if !matches!(
                    connector,
                    crate::command_safety::ReadonlySequenceConnector::AndThen
                        | crate::command_safety::ReadonlySequenceConnector::OrElse
                ) =>
            {
                // The silent skip on a failed canonicalize is a guard on a
                // gate-guaranteed state: the operand gate refused any `cd`
                // destination it could not canonicalize (and prove
                // enterable) before this walk runs, so only the tiny
                // gate->discovery race disclosed at the seam can land here,
                // and keeping the old cwd matches the shell's failed-`cd`
                // continuation under `;`.
                if let Some(target) = argv.get(1)
                    && let Ok(resolved) = live_cwd.join(target).canonicalize()
                {
                    live_cwd = resolved;
                }
            }
            _ => {}
        }
    }
    if pins.len() > MAX_READONLY_GIT_FILTER_PINS {
        return Err(ToolError::permission_denied(format!(
            "[shell.readonly.git.filter_config] The repositories this command's git stages run in declare more than {MAX_READONLY_GIT_FILTER_PINS} content filters, more than the read-only lane can pin inert; use a workspace without filter declarations for repository evidence."
        )));
    }
    // The env block writes the fixed core.fsmonitor/core.hooksPath/
    // log.showSignature pins for EVERY read-only git stage, unconditionally,
    // so the version gate fires for every command with a git stage — not
    // only when discovery armed filter pins: on an old git none of those
    // pins would take.
    ensure_git_supports_env_config_pins(roots)?;
    Ok(pins)
}

/// The env-config mechanism the whole read-only git env block rides needs
/// git >= 2.32 (feed pins 2.31, `GIT_CONFIG_GLOBAL` 2.32), and for
/// `status`/`ls-files`/`grep` those pins are the only
/// wall — on an older git the environment block silently does not exist and
/// the repository's filters would run under an admitted read. The block
/// emits `GIT_CONFIG_COUNT` for every read-only git stage (the fixed
/// fsmonitor/hooksPath/log.showSignature pins are unconditional), so the
/// host git is checked for every command with a git stage, before any child
/// exists: a refusal names the version gap instead of admitting under
/// protection that would not take. The probe itself runs the way the
/// read-only child runs — cleared environment, sanitized PATH, git
/// configuration pinned off, and the same argv config pins as every other
/// discovery invocation — so the binary it measures is the trusted host
/// git, never an agent-planted `git` shim from a workspace PATH entry, and
/// the scanned repository's `core.fsmonitor` cannot fire inside the probe
/// either. The result is memoized per sanitized PATH (the version cannot
/// change under a running process), so the gate costs one fork per process,
/// not one per command.
fn ensure_git_supports_env_config_pins(roots: &[PathBuf]) -> Result<(), ToolError> {
    let path = std::env::var_os("PATH");
    ensure_git_supports_env_config_pins_with_path(roots, path.as_deref())
}

/// [`ensure_git_supports_env_config_pins`] with the PATH the probe resolves
/// git through supplied explicitly; the seam exists so tests can plant a
/// PATH without mutating the live process environment.
fn ensure_git_supports_env_config_pins_with_path(
    roots: &[PathBuf],
    path: Option<&std::ffi::OsStr>,
) -> Result<(), ToolError> {
    // The decision is a pure function of the sanitized PATH (same PATH ->
    // same resolved binary -> same version), and git's version cannot change
    // under a running process, so one memoized probe per distinct sanitized
    // PATH removes the per-command fork/exec this gate used to pay. Both
    // deterministic outcomes are cached — an old-git host refuses with one
    // probe, not one per command — while a transient spawn failure is
    // re-probed on the next command rather than cached as permanent.
    let sanitized_path = path
        .and_then(|path| readonly_sanitized_path_from(roots, path))
        .map(|value| value.to_string_lossy().into_owned());
    {
        let memo = GIT_VERSION_PROBE
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match memo.as_ref() {
            Some((cached_path, decision)) if *cached_path == sanitized_path => {
                return match decision {
                    Ok(()) => Ok(()),
                    Err(reason) => Err(version_gate_refusal(reason.clone())),
                };
            }
            _ => {}
        }
    }
    let mut command = std::process::Command::new("git");
    arm_discovery_git_config_pins(&mut command);
    command.arg("--version");
    apply_discovery_git_env(&mut command, roots, path);
    let output = command
        .output()
        .map_err(|error| {
            ToolError::permission_denied(format!(
                "[shell.readonly.git.filter_config] A read-only git stage on this command needs this host's git version-checked before the stage runs, and the version probe could not start ({error}); run repository evidence on a git >= {}.{} host.",
                MIN_GIT_ENV_CONFIG_VERSION.0, MIN_GIT_ENV_CONFIG_VERSION.1
            ))
        })?;
    let version = String::from_utf8_lossy(&output.stdout);
    let decision = if git_env_config_supported(&version) {
        Ok(())
    } else {
        Err(format!(
            "this host's git ({}) predates the {}.{} env-config mechanism the read-only git pins ride — repository configuration (content filters, fsmonitor, hooks) would run under an admitted read; upgrade git for repository evidence.",
            version.trim(),
            MIN_GIT_ENV_CONFIG_VERSION.0,
            MIN_GIT_ENV_CONFIG_VERSION.1
        ))
    };
    let mut memo = GIT_VERSION_PROBE
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    *memo = Some((sanitized_path, decision.clone()));
    decision.map_err(version_gate_refusal)
}

fn version_gate_refusal(reason: String) -> ToolError {
    ToolError::permission_denied(format!("[shell.readonly.git.filter_config] {reason}"))
}

/// One memoized version-gate decision, keyed on the sanitized PATH the
/// probe resolved git through.
type GitVersionProbeDecision = Result<(), String>;
static GIT_VERSION_PROBE: std::sync::Mutex<Option<(Option<String>, GitVersionProbeDecision)>> =
    std::sync::Mutex::new(None);

/// Whether a `git --version` output declares a release that honors
/// `GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`/`GIT_CONFIG_COUNT`
/// (>= [`MIN_GIT_ENV_CONFIG_VERSION`]). Unparseable output refuses: the pins
/// must be proven to exist, not assumed.
fn git_env_config_supported(version_output: &str) -> bool {
    let Some((_, rest)) = version_output.split_once("version") else {
        return false;
    };
    let numerics: String = rest
        .chars()
        .skip_while(|ch| !ch.is_ascii_digit())
        .take_while(|ch| ch.is_ascii_digit() || *ch == '.')
        .collect();
    let mut components = numerics.split('.');
    let (Some(Ok(major)), Some(Ok(minor))) = (
        components.next().map(str::parse),
        components.next().map(str::parse),
    ) else {
        return false;
    };
    (major, minor) >= MIN_GIT_ENV_CONFIG_VERSION
}

/// Where a git stage's `--no-pager`/`-C <dir>` preamble ends: the same walk
/// the classifier admits and [`splice_git_read_hardening`] uses to locate
/// the subcommand. The operand gate slices the operands off after it, so
/// the `-C` values themselves are judged only by [`readonly_git_stage_root`]
/// — never twice, and never under the wrong root.
fn readonly_git_preamble_len(argv: &[String]) -> usize {
    let mut index = 1;
    while let Some(flag) = argv.get(index) {
        match flag.as_str() {
            "--no-pager" => index += 1,
            "-C" => index += 2,
            _ => break,
        }
    }
    index
}

/// The directory one git stage's operands resolve against and the repository
/// it runs under: git applies each `-C <dir>` cumulatively from the stage's
/// working directory, so the last one wins (`git -C sub status` runs against
/// `sub`'s repository even though the shell never `cd`'d; `cd sub; git -C ..
/// status` runs against the parent's). This is the ONE walk for both
/// consumers — the operand gate's per-stage operand judgment and the
/// discovery pass's repository scan — so the two can never diverge: each
/// `-C` takes the next token (absent → refuse), resolves relative values
/// against the accumulated root, and must canonicalize to a directory
/// inside the workspace before the root updates. A chain of literal
/// operands like `git -C A -C escape` therefore behaves exactly like the
/// flat `git A/escape` it executes as. A `-C` value carrying a wildcard
/// or a leading `=` refuses instead: the shell expands such spellings
/// against the stage's working directory (glob, or zsh's EQUALS lookup)
/// before git ever sees the word, while this resolver judges the literal
/// spelling — the expansion never passes through the plain-operand walker
/// because `-C` values are sliced off before it runs, so refusal is the
/// only wall that matches where git actually chdir's. Failure names the
/// reason; callers attach their own refusal tag.
fn readonly_git_stage_root(
    stage_argv: &[String],
    base: &std::path::Path,
    roots: &[PathBuf],
) -> Result<std::path::PathBuf, String> {
    let mut dir = base.to_path_buf();
    let mut index = 1;
    while let Some(flag) = stage_argv.get(index) {
        match flag.as_str() {
            "--no-pager" => index += 1,
            "-C" => {
                let Some(target) = stage_argv.get(index + 1) else {
                    return Err(
                        "the git stage carries a `-C` without its directory operand".to_string()
                    );
                };
                // The shell expands a leading `~` against $HOME before git
                // ever sees the word, while this resolver joins spellings
                // against the stage's base directory — the same
                // gate-vs-shell divergence the plain-operand and `cd` rules
                // refuse `~` for, so a `-C` value gets the same wall.
                if target.starts_with('~') {
                    return Err(format!(
                        "read-only git `-C` operand {target:?} carries a tilde the shell would expand outside the judged location"
                    ));
                }
                // The same divergence, glob flavor: a `-C` value the shell
                // expands (any wildcard in the word; `*` is the only one
                // the charset admits) binds git to whichever expansion
                // entry sorts first, while this resolver canonicalizes the
                // literal spelling. A planted literal directory satisfies
                // the gate while the expansion binds elsewhere — proven
                // end to end with the chained shape `git -C x -C *`, whose
                // planted `*` lives inside `x/`, outside the expansion
                // directory, so the leftover entries land in git's
                // subcommand and rev slots and a hostile repo's HEAD
                // renders. Refuse any wildcard outright, like the `cd` and
                // gh-stage rules do.
                if target.contains('*') {
                    return Err(format!(
                        "read-only git `-C` operand {target:?} carries a wildcard the shell would expand outside the judged location"
                    ));
                }
                // zsh's EQUALS expansion is the non-glob member of the same
                // family: `git -C =pwd status` makes zsh substitute the
                // resolved path of the `pwd` binary while this resolver
                // judges a literal `=pwd` spelling (the plain-operand gate
                // refuses `=`-prefixed operands for exactly this reason,
                // but never sees `-C` values).
                if target.starts_with('=') {
                    return Err(format!(
                        "read-only git `-C` operand {target:?} starts with `=`, which zsh expands to a command's path outside the judged location"
                    ));
                }
                let joined = if std::path::Path::new(target).is_absolute() {
                    std::path::PathBuf::from(target)
                } else {
                    dir.join(target)
                };
                let resolved = joined.canonicalize().map_err(|error| {
                    format!("read-only git `-C` operand {target:?} could not be resolved inside the workspace: {error}")
                })?;
                if !resolved.is_dir() {
                    return Err(format!(
                        "read-only git `-C` operand {target:?} does not resolve to a directory inside the workspace"
                    ));
                }
                if !roots.iter().any(|root| resolved.starts_with(root)) {
                    return Err(format!(
                        "read-only git `-C` operand {target:?} resolves outside the workspace"
                    ));
                }
                // Prove the directory enterable the way the `cd` fold does:
                // canonicalize and is_dir need search permission only on the
                // ancestors, while git's `chdir` needs it on the destination
                // itself — a mode-000 directory passes the checks above and
                // would then fail the child's `chdir`, leaving the gate's
                // judgment on a state the shell never reached.
                resolved.join(".").metadata().map_err(|error| {
                    format!(
                        "read-only git `-C` operand {target:?} resolves to a directory the shell cannot enter: {error}"
                    )
                })?;
                dir = resolved;
                index += 2;
            }
            _ => break,
        }
    }
    Ok(dir)
}

/// The discovery git's environment, shared by every git invocation this lane
/// makes (the submodule enumeration and the version probe alike — note the
/// enumeration child of a scanned repository may run before the version
/// gate refuses an old git, so this arming applies to both): a cleared
/// environment plus the same sanitized PATH the
/// read-only child runs under — entries under any declared workspace root
/// are dropped by the exact construction the child env block uses
/// (`readonly_sanitized_path`), so an agent-planted `git` shim is never the
/// binary discovery execs — git configuration pinned off, prompts disabled,
/// and a locale-stable C locale. `path` is the raw PATH value the sanitizing
/// construction consumes; the production callers supply the live process
/// PATH read once at discovery entry, and tests supply their own.
fn apply_discovery_git_env(
    command: &mut std::process::Command,
    roots: &[PathBuf],
    path: Option<&std::ffi::OsStr>,
) {
    command.env_clear();
    if let Some(path) = path
        .and_then(|path| readonly_sanitized_path_from(roots, path))
        .map(|value| value.to_string_lossy().into_owned())
    {
        command.env("PATH", path);
    }
    let null_device = if cfg!(windows) { "NUL" } else { "/dev/null" };
    command.env("GIT_CONFIG_NOSYSTEM", "1");
    command.env("GIT_CONFIG_GLOBAL", null_device);
    command.env("GIT_TERMINAL_PROMPT", "0");
    command.env("LC_ALL", "C");
}

/// The fixed configuration pins every discovery git invocation carries as
/// argv `-c` entries — the same three the admitted child gets from its env
/// block. Only system/global configuration is pinned off for these children;
/// repo-local `.git/config` stays fully live, and on git 2.43 a repo-local
/// `core.fsmonitor` command executes inside a plain `git ls-files --stage -z`
/// (probe-pinned) — attacker-chosen command execution in the scanned
/// repository, under the tool process's own authority, before anything is
/// admitted. argv `-c` entries outrank every config file and work on every
/// git, unlike the `GIT_CONFIG_COUNT` mechanism the admitted child rides
/// (which needs >= 2.32), so these pins do not wait on the version gate.
fn arm_discovery_git_config_pins(command: &mut std::process::Command) {
    let null_device = if cfg!(windows) { "NUL" } else { "/dev/null" };
    for (key, value) in [
        ("core.fsmonitor", "false".to_string()),
        ("core.hooksPath", null_device.to_string()),
        ("log.showSignature", "false".to_string()),
    ] {
        command.arg("-c").arg(format!("{key}={value}"));
    }
}

/// Read one git metadata file the way discovery must. The size cap is
/// checked on the OPENED handle's metadata — no TOCTOU between a stat and
/// the read — anything that is not a regular file refuses before it is
/// read, and on unix the open itself is non-blocking: a FIFO at a
/// discovered path (`.git/config`, a `commondir` pointer) reports size 0
/// to `stat`, so a plain size-checked `fs::read` never sees it coming and
/// blocks in `open` until a writer appears — inside the pre-spawn
/// discovery, where no watchdog bounds anything. `Ok(None)` is the
/// caller's signal that the path does not exist.
fn read_bounded_regular_file(
    path: &std::path::Path,
    cap: usize,
) -> Result<Option<Vec<u8>>, String> {
    let mut options = std::fs::File::options();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.custom_flags(libc::O_NONBLOCK);
    }
    let file = match options.open(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(format!(
                "{} could not be opened for discovery: {error}",
                path.display()
            ));
        }
    };
    let metadata = file.metadata().map_err(|error| {
        format!(
            "{} could not be stat'd for discovery: {error}",
            path.display()
        )
    })?;
    if !metadata.is_file() {
        return Err(format!(
            "{} is not a regular file; discovery reads only regular files",
            path.display()
        ));
    }
    if metadata.len() > cap as u64 {
        return Err(format!(
            "{} is larger than the {cap}-byte discovery bound",
            path.display()
        ));
    }
    // Regular files ignore O_NONBLOCK, and take() bounds the read itself so
    // a file that grows past the cap between the fstat and the read cannot
    // buy more than one byte over the cap of transient allocation. The
    // one-byte margin also makes that growth detectable: without the check
    // below, a config that grew past the cap would parse as a complete text
    // with its tail missing — the one member of the pre-spawn race family
    // whose failure direction is a silent admission rather than a refusal.
    let mut bytes = Vec::new();
    file.take((cap + 1) as u64)
        .read_to_end(&mut bytes)
        .map_err(|error| format!("{} could not be read: {error}", path.display()))?;
    if bytes.len() > cap {
        return Err(format!(
            "{} grew past the {cap}-byte discovery bound before it could be read",
            path.display()
        ));
    }
    Ok(Some(bytes))
}

/// Run one read-only discovery `git` invocation and return its stdout.
/// stdout is read under `stdout_cap` — one byte more refuses rather than
/// trusting a truncated listing — and any spawn failure, non-zero status, or
/// cap overflow refuses with the reason: a listing this lane cannot fully
/// read is a submodule set it cannot prove. `path` is the raw PATH the
/// hardened environment sanitizes, supplied by the caller.
fn run_discovery_git(
    dir: &std::path::Path,
    args: &[&str],
    stdout_cap: usize,
    roots: &[PathBuf],
    path: Option<&std::ffi::OsStr>,
    timeout: std::time::Duration,
) -> Result<Vec<u8>, String> {
    let mut command = std::process::Command::new("git");
    arm_discovery_git_config_pins(&mut command);
    command
        .current_dir(dir)
        .args(args)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null());
    apply_discovery_git_env(&mut command, roots, path);
    let mut child = command
        .spawn()
        .map_err(|error| format!("git could not be started for discovery: {error}"))?;
    let stdout = child.stdout.take();
    // The child opens repository files itself (`.git/index`, `HEAD`, refs):
    // a FIFO at any of them blocks its own `open` forever, and nothing else
    // bounds this pre-spawn region. A watchdog thread holds the child behind
    // a lock, kills it at the bound, and lets the read loop below see
    // end-of-file; `timed_out` distinguishes the bound's refusal from git's
    // own failure. The stdout pipe is taken before the child is shared, so
    // the lock is never held across a blocking read.
    let child = std::sync::Arc::new(std::sync::Mutex::new(child));
    let timed_out = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let watchdog = {
        let child = std::sync::Arc::clone(&child);
        let timed_out = std::sync::Arc::clone(&timed_out);
        std::thread::spawn(move || {
            let deadline = std::time::Instant::now() + timeout;
            loop {
                // The poll quantum is latency every admitted git command
                // pays: the main thread reaps as soon as the read loop sees
                // end-of-file and then joins this thread, which only wakes
                // on the next tick. Keep the quantum far below the fork cost
                // it wraps. A child wedged in uninterruptible sleep (D-state)
                // survives SIGKILL and is bounded only by the blocking pool;
                // killable opens — the FIFO family the bounds exist for —
                // die here.
                std::thread::sleep(READONLY_DISCOVERY_WATCHDOG_POLL);
                let Ok(mut guard) = child.try_lock() else {
                    continue;
                };
                if guard.try_wait().map_or(true, |exited| exited.is_some()) {
                    return;
                }
                if std::time::Instant::now() >= deadline {
                    let _ = guard.kill();
                    timed_out.store(true, std::sync::atomic::Ordering::Release);
                    return;
                }
            }
        })
    };
    let read_result = match stdout {
        Some(mut pipe) => {
            let mut bytes = Vec::new();
            let mut chunk = [0u8; 64 * 1024];
            loop {
                match pipe.read(&mut chunk) {
                    Ok(0) => break Ok(bytes),
                    Ok(read) => {
                        bytes.extend_from_slice(&chunk[..read]);
                        if bytes.len() > stdout_cap {
                            break Err(format!(
                                "the git listing is larger than the {stdout_cap}-byte discovery bound"
                            ));
                        }
                    }
                    Err(error) => break Err(format!("the git listing could not be read: {error}")),
                }
            }
        }
        None => Err("git discovery produced no readable stdout".to_string()),
    };
    // Reap in every path, including a refusal: no zombie is left behind.
    // The reap POLLS the child instead of blocking in `wait()` under the
    // lock: a child that closed its stdout but lingers would otherwise hold
    // the mutex forever and the watchdog — which only `try_lock`s — could
    // never deliver its deadline kill after end-of-file. Each iteration
    // holds the lock just long enough to `try_wait`, so the watchdog slips
    // in between polls.
    let status: std::io::Result<std::process::ExitStatus> = loop {
        {
            let mut guard = child.lock().map_err(|_| {
                "the discovery child's state was poisoned by a panicked watchdog".to_string()
            })?;
            match guard.try_wait() {
                Ok(Some(status)) => break Ok(status),
                Ok(None) => {}
                Err(error) => break Err(error),
            }
        }
        if timed_out.load(std::sync::atomic::Ordering::Acquire) {
            // The watchdog fired and killed the child; surface the kill as
            // an exit with a blocking reap (a SIGKILLed process that can
            // exit at all cannot linger), then report the bound below.
            let mut guard = child.lock().map_err(|_| {
                "the discovery child's state was poisoned by a panicked watchdog".to_string()
            })?;
            break guard.wait();
        }
        std::thread::sleep(READONLY_DISCOVERY_WATCHDOG_POLL);
    };
    let _ = watchdog.join();
    if timed_out.load(std::sync::atomic::Ordering::Acquire) {
        return Err(format!(
            "the discovery listing did not complete within {timeout:?}; the repository holds a file git blocks on"
        ));
    }
    match read_result {
        Ok(bytes) => {
            let status = status
                .map_err(|error| format!("git discovery did not report a status: {error}"))?;
            if !status.success() {
                return Err(format!("git exited with {status}"));
            }
            Ok(bytes)
        }
        Err(reason) => Err(reason),
    }
}

/// The gitlink paths one repository's index records (`git ls-files --stage`,
/// the listing git consults when it forks its dirty-check child; mode
/// `160000` is a gitlink, and plain blobs and directories never fork). Every
/// path is lossy-decoded — git reads bytes — and a path carrying a
/// replacement marker or any spelling this lane cannot resolve refuses via
/// [`validated_git_metadata_path`], because a silently skipped submodule is
/// a repository whose filters would run unpinned.
fn readonly_gitlink_paths(
    worktree_dir: &std::path::Path,
    roots: &[PathBuf],
    path: Option<&std::ffi::OsStr>,
) -> Result<Vec<String>, String> {
    let bytes = run_discovery_git(
        worktree_dir,
        &["ls-files", "--stage", "-z"],
        READONLY_GIT_INDEX_SCAN_CAP,
        roots,
        path,
        READONLY_DISCOVERY_CHILD_TIMEOUT,
    )?;
    let mut paths = Vec::new();
    for record in bytes.split(|byte| *byte == 0) {
        if record.is_empty() {
            // The final record's terminator.
            continue;
        }
        let Some(tab) = record.iter().position(|byte| *byte == b'\t') else {
            return Err(
                "an index record carries no path after its mode/sha/stage header".to_string(),
            );
        };
        let mode = String::from_utf8_lossy(&record[..tab]);
        if mode.split(' ').next() != Some("160000") {
            continue;
        }
        let path = String::from_utf8_lossy(&record[tab + 1..]).into_owned();
        validated_git_metadata_path(&path)?;
        paths.push(path);
    }
    Ok(paths)
}

/// One path this lane resolves out of git metadata (an index gitlink record;
/// the same rules the `.gitmodules` reader applied, now applied to the index
/// listing that actually decides git's fork). Lexical escapes (absolute
/// spellings, `..`), undecodable replacement markers, and — on Windows,
/// where `\` is separator syntax — quoted or backslash spellings refuse: a
/// silently skipped submodule is a repository whose filters would run
/// unpinned. On unix `"` and `\` are ordinary filename bytes consumed
/// in-process (join, canonicalize — never re-quoted), so a legitimate
/// submodule carrying one is scanned like git runs it; a path whose
/// COMPONENT is a symlink to an outside repository is also scanned, not
/// refused — git forks into the repo the symlink resolves to, so its
/// filters must be pinned there.
fn validated_git_metadata_path(path: &str) -> Result<(), String> {
    let windows_unspellable = cfg!(windows) && (path.contains('"') || path.contains('\\'));
    // The echo is bounded like every config-derived refusal text: a crafted
    // index gitlink path is bounded only by the 16 MiB listing cap, the
    // refusal reaches the model verbatim, and an unbounded echo would turn
    // the refusal itself into the read-back channel the round-11 caps
    // closed for filter names and `core.worktree` values.
    let echo = readonly_refusal_echo(path);
    if path.is_empty() || windows_unspellable || path.contains(char::REPLACEMENT_CHARACTER) {
        return Err(format!(
            "git metadata declares a path {echo:?} whose spelling this lane does not resolve"
        ));
    }
    if path.starts_with('/') || path.split('/').any(|component| component == "..") || path == "." {
        return Err(format!(
            "git metadata declares a path {echo:?} outside the worktree this lane bounds"
        ));
    }
    Ok(())
}

/// Scan the repository at (or above) `cwd` once per command, accumulating
/// its armed filter declarations into `pins`, then scan every submodule its
/// index registers — git forks its own `git status`/`git diff` child into
/// any index gitlink whose path holds a repository, and that child converts
/// content under the submodule's repository config, so a filter declared
/// there runs under an admitted superproject read and must be pinned the
/// same way.
fn scan_readonly_repository_at(
    cwd: &std::path::Path,
    pins: &mut Vec<ReadonlyGitFilterPin>,
    scanned: &mut std::collections::HashSet<PathBuf>,
    repos_remaining: &mut usize,
    roots: &[PathBuf],
    env_path: Option<&std::ffi::OsStr>,
    deadline: std::time::Instant,
) -> Result<(), ToolError> {
    let refuse = |reason: String| {
        ToolError::permission_denied(format!(
            "[shell.readonly.git.filter_config] Read-only git stages run in a repository whose configuration this lane cannot safely neutralize: {reason}"
        ))
    };
    let Some((gitdir, worktree_dir)) = readonly_git_dir_for(cwd).map_err(refuse)? else {
        return Ok(());
    };
    let key = gitdir.canonicalize().unwrap_or_else(|_| gitdir.clone());
    if !scanned.insert(key) {
        return Ok(());
    }
    if *repos_remaining == 0 {
        return Err(refuse(
            "this command's git stages reach more repositories than the lane scans".to_string(),
        ));
    }
    // Checked after the dedup insert so only repos that would actually be
    // scanned pay the check, and before any of this repo's children run.
    if std::time::Instant::now() >= deadline {
        return Err(refuse(format!(
            "this command's git-stage discovery outlasted its {MAX_READONLY_DISCOVERY_WALL_CLOCK:?} aggregate wall-clock budget, so the workspace chains more repositories than the lane can scan in time; narrow the command's git stages"
        )));
    }
    *repos_remaining -= 1;
    let names = readonly_git_config_filter_names(&gitdir, &worktree_dir).map_err(refuse)?;
    for name in names {
        // Byte-exact merge: git matches an attribute value against the
        // config subsection byte for byte, so two repositories declaring
        // case-variant spellings declare two drivers, and folding one onto
        // the other would pin a spelling git never looks up while leaving
        // the live one armed.
        if let Some(pinned) = pins.iter_mut().find(|pinned| pinned.name == name.name) {
            pinned.clean |= name.clean;
            pinned.smudge |= name.smudge;
            pinned.process |= name.process;
        } else {
            pins.push(name);
        }
    }
    // Enumeration is index-driven: git forks its own dirty-check child into
    // ANY index gitlink whose path holds a repository, and `.gitmodules` is
    // only a label on that fork (probe-pinned: with `.gitmodules` deleted
    // from the worktree but the gitlink still in the index, git still forks
    // and the submodule's filter still fires), so the registered submodule
    // set comes from the index itself. Fail-closed cost, disclosed at this
    // call site: the `git ls-files --stage -z` child runs once for every
    // scanned repository — even one whose index holds zero gitlinks, an
    // admitted git command pays this fork twice (its own repository plus
    // the scan) — and an index listing past the 16 MiB scan cap refuses
    // every git evidence command rather than trusting a truncated set.
    for gitlink in readonly_gitlink_paths(&worktree_dir, roots, env_path).map_err(refuse)? {
        scan_readonly_repository_at(
            &worktree_dir.join(std::path::Path::new(&gitlink)),
            pins,
            scanned,
            repos_remaining,
            roots,
            env_path,
            deadline,
        )?;
    }
    Ok(())
}

/// Whether `gitdir` is a git directory by git's own `is_git_directory`
/// predicate (setup.c) — the check its discovery loop consults for every
/// `.git` DIRECTORY it meets: a `HEAD` passing `validate_headref` (path.c)
/// plus an executable-accessible `objects` and `refs` under the common
/// directory the `commondir` pointer resolves to. The helpers below mirror
/// those functions check for check, because either divergence leaks: a
/// directory this predicate rejects while git's accepts makes the walk-up
/// skip a repository the child runs (its config unpinned), and a directory
/// it accepts while git's rejects makes the walk-up stop while git
/// continues to an ancestor whose filters then run unpinned. Both sides are
/// probe-pinned against git 2.43 (an empty or HEAD-only directory
/// continues; a dangling-HEAD symlink runs).
fn readonly_dir_is_git_repository(gitdir: &std::path::Path) -> bool {
    if !readonly_headref_validates(&gitdir.join("HEAD")) {
        return false;
    }
    let Some(common) = readonly_gitdir_common_dir(gitdir) else {
        return false;
    };
    readonly_path_executable_accessible(&common.join("objects"))
        && readonly_path_executable_accessible(&common.join("refs"))
}

/// `validate_headref` (path.c): a `HEAD` symlink whose target starts with
/// `refs/` validates without consulting the target's existence (probe-
/// pinned: a HEAD symlink to a never-created ref runs the repository), and
/// anything else must be a regular file whose first 255 bytes are either
/// `ref:` plus a {SP, TAB, LF, CR} skip plus a `refs/`-prefixed name
/// (probe-pinned against git 2.43: `\n` skips, while `\v` and `\f` REJECT
/// — the skip set is narrower than both C `isspace` and Rust's
/// `is_ascii_whitespace`, so `ref:\frefs/x` is a spelling git's own
/// discovery refuses and walks past), or
/// a full 40-hex-character object name for a detached HEAD. Everything else
/// — missing, unreadable, FIFO, a directory, a symlink pointing outside
/// `refs/`, an unparsable spelling — fails. A FIFO HEAD blocks git's own
/// open until the discovery watchdog ends the child, so declining it here
/// is fail-safe.
fn readonly_headref_validates(head: &std::path::Path) -> bool {
    let Ok(metadata) = std::fs::symlink_metadata(head) else {
        return false;
    };
    if metadata.file_type().is_symlink() {
        let Ok(target) = std::fs::read_link(head) else {
            return false;
        };
        #[cfg(unix)]
        {
            use std::os::unix::ffi::OsStrExt as _;
            return target.as_os_str().as_bytes().starts_with(b"refs/");
        }
        #[cfg(not(unix))]
        {
            return target
                .as_os_str()
                .to_str()
                .is_some_and(|target| target.starts_with("refs/"));
        }
    }
    // validate_headref reads the first 255 bytes of the HEAD it opened, of
    // any file size — the prefix checks below must run on those bytes for a
    // HEAD of any size too, or a crafted size could flip a spelling git
    // accepts into one the walk-up skips.
    let Some(bytes) = readonly_regular_file_prefix(head, 255) else {
        return false;
    };
    if bytes.starts_with(b"ref:") {
        let mut rest = &bytes[4..];
        // The skip set is exactly {SP, TAB, LF, CR} — probe-pinned against
        // git 2.43 (see the fn doc). Rust's `is_ascii_whitespace` would
        // additionally accept `\f`, admitting a decoy gitdir git's own
        // discovery rejects and walks past — a hostile ancestor then runs
        // unpinned under an admitted read.
        while matches!(rest.first(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            rest = &rest[1..];
        }
        if rest.starts_with(b"refs/") {
            return true;
        }
    }
    bytes.len() >= 40 && bytes[..40].iter().all(|byte| byte.is_ascii_hexdigit())
}

/// The first `limit` bytes of the regular file at `path`, or `None` when
/// the spelling is not a readable regular file. Unlike
/// [`read_bounded_regular_file`] there is no pre-read size bound: the
/// callers below must see the same bytes regardless of file size (the
/// `take` bounds the read instead), and the non-blocking open plus the
/// regular-file check on the opened handle keep a FIFO from blocking the
/// pre-spawn walk.
fn readonly_regular_file_prefix(path: &std::path::Path, limit: usize) -> Option<Vec<u8>> {
    let mut options = std::fs::File::options();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.custom_flags(libc::O_NONBLOCK);
    }
    let file = options.open(path).ok()?;
    if !file.metadata().ok()?.is_file() {
        return None;
    }
    let mut bytes = Vec::new();
    file.take(limit as u64).read_to_end(&mut bytes).ok()?;
    Some(bytes)
}

/// The byte-exact mirror of git's `get_common_dir_noenv`/gitfile pointer
/// read (setup.c; `GIT_COMMON_DIR` is not in this lane's discovery
/// environment), shared by the two `commondir` consumers — the walk-up
/// predicate and the config scan: relative values lean against `base`, the
/// whole value strips its trailing `\r`/`\n` run exactly like the `.git`
/// link value (probe-pinned), and the first NUL byte ends it — git consumes
/// the value as a C string, so `…/.git\0junk` resolves `…/.git` and runs
/// THAT repository's filters (probe-pinned end to end: a marker filter
/// armed in the resolved repository fired under a plain `git status` while
/// the whole-value spelling canonicalized to nothing). Paths are bytes on
/// unix, so the truncated value is turned into an `OsStr` path directly: a
/// pointer naming a repository through a non-UTF-8 byte path is one git
/// runs (probe-pinned, filter fired) and this lane must scan that same
/// repository, not decline it. Windows paths are UTF-16 and a non-UTF-8
/// value cannot be mirrored faithfully there, so the lane refuses —
/// fail-closed: the predicate turns that refusal into a decline, where the
/// child's own git fatals on the same value, and the scan refuses the
/// command; the `.git` gitfile reader keeps its own stricter UTF-8 decode
/// wall (refusing there is fail-closed, never walk-past). No pointer means
/// the gitdir is its own common directory; a pointer whose stat fails
/// spells "no shared directory" to git too. git DIES on an unreadable or
/// empty pointer rather than continuing its walk — this lane declines
/// instead, which walks up while the child dies at git's own fatal, and a
/// FIFO pointer blocks git's own read until the discovery watchdog ends
/// the child: fail-safe both ways.
fn readonly_git_pointer_target(bytes: &[u8], base: &std::path::Path) -> Result<PathBuf, String> {
    let mut stripped = bytes;
    while stripped.ends_with(b"\n") || stripped.ends_with(b"\r") {
        stripped = &stripped[..stripped.len() - 1];
    }
    if let Some(nul) = stripped.iter().position(|&byte| byte == 0) {
        stripped = &stripped[..nul];
    }
    if stripped.is_empty() {
        return Err("names no directory".to_string());
    }
    #[cfg(unix)]
    {
        use std::os::unix::ffi::OsStrExt as _;
        let value = std::path::Path::new(std::ffi::OsStr::from_bytes(stripped));
        Ok(if value.is_absolute() {
            value.to_path_buf()
        } else {
            base.join(value)
        })
    }
    #[cfg(not(unix))]
    {
        let Ok(value) = std::str::from_utf8(stripped) else {
            return Err("holds bytes this lane cannot decode into a Windows path".to_string());
        };
        Ok(if std::path::Path::new(value).is_absolute() {
            PathBuf::from(value)
        } else {
            base.join(value)
        })
    }
}

fn readonly_gitdir_common_dir(gitdir: &std::path::Path) -> Option<PathBuf> {
    let pointer = gitdir.join("commondir");
    let Ok(metadata) = std::fs::metadata(&pointer) else {
        return Some(gitdir.to_path_buf());
    };
    if !metadata.is_file() {
        return None;
    }
    let bytes = readonly_regular_file_prefix(&pointer, READONLY_GIT_META_SCAN_CAP)?;
    let resolved = readonly_git_pointer_target(&bytes, gitdir).ok()?;
    resolved.canonicalize().ok()
}

/// git's `access(path, X_OK)` for the `objects`/`refs` checks — the
/// faithful syscall, so a directory the walk-up stops on is accepted or
/// rejected exactly as the child's discovery accepts or rejects it.
#[cfg(unix)]
fn readonly_path_executable_accessible(path: &std::path::Path) -> bool {
    use std::os::unix::ffi::OsStrExt as _;
    let Ok(spelling) = std::ffi::CString::new(path.as_os_str().as_bytes()) else {
        return false;
    };
    // SAFETY: `spelling` outlives the call and `access` writes nothing it
    // was given.
    unsafe { libc::access(spelling.as_ptr(), libc::X_OK) == 0 }
}

/// Windows: git's mingw `access(path, X_OK)` resolves directories by
/// existence, and `objects`/`refs` are directories in every layout git
/// accepts there — existence is the faithful mirror, not an approximation.
#[cfg(not(unix))]
fn readonly_path_executable_accessible(path: &std::path::Path) -> bool {
    path.exists()
}

/// The git directory git itself would use from `cwd` — paired with the
/// directory the `.git` entry was found in, the repository's worktree root.
/// The walk mirrors `setup_git_directory_gently_1`'s per-level order, keyed
/// on the FOLLOWED kind of the `.git` entry (git's discovery stats through
/// a symlink): a `.git` DIRECTORY is a repository only when
/// [`readonly_dir_is_git_repository`] says so, and a directory failing that
/// predicate is SKIPPED in favor of the walk-up (probe-pinned: `git status`
/// beneath an empty `sub/.git` resolves the ancestor and runs ITS filters)
/// — stopping on any directory would scan a repository git never runs while
/// the child ran one this lane never scanned. A `.git` SYMLINK is followed
/// by git (probe-pinned against git 2.43: a link whose target is a plain
/// `gitdir:` file IS applied — `git status` in a directory whose `.git` is
/// a symlink to a one-line gitfile resolved the target repository and
/// executed ITS `filter.<name>.clean` — which the previous
/// symlink-decides-arm walked past with zero pins), so a link to a
/// directory goes through the same predicate and a link to a plain file
/// goes through the same gitfile reader as a direct one. A dangling or
/// vanished link is neither: git's stat fails and the walk continues.
/// Divergences that remain, each fail-safe because the child's
/// own git fatals or the discovery watchdog refuses: the bare check (git
/// also accepts the directory ITSELF as a bare repository — HEAD, objects,
/// and refs directly in `dir` — which converts no worktree content, so no
/// filter can fire there and the lane does not scan it), filesystem
/// boundaries (the child env clears GIT_DISCOVERY_ACROSS_FILESYSTEM, so the
/// child may stop where the walk continues; the extra repositories it
/// refuses are scanned, never run), and safe.directory (a repository git
/// rejects for dubious ownership fatals in the child after the scan pinned
/// it, failing the command honestly).
///
/// A `.git` regular file — direct, or reached through a symlink — parses as
/// the worktree/submodule `gitdir:` link.
/// git parses it byte-wise: the literal lowercase prefix `gitdir:` plus
/// EXACTLY ONE space, then the whole remaining file with an unbounded run
/// of trailing `\r`/`\n` bytes stripped (any order and count — probe-pinned
/// against git 2.43: `\n\n`, `\r\r`, and `\n\r` endings all resolve the
/// fully stripped target) and every other byte kept, interior newlines
/// included. The value is a C string to git, so the first NUL byte ends it
/// (probe-pinned: `gitdir: <target>\0junk` resolves `<target>` and runs
/// THAT repository). Every other spelling is the `invalid gitfile format`
/// git itself fatals on. A value that is empty, or carries leading or
/// trailing space/tab, is one this lane refuses rather than resolves: git
/// would resolve it against the link's directory and run a repository
/// found at the padded path (probe-pinned), so the refusal is the
/// fail-closed side of a mirror this lane declines to follow for a link
/// class no legitimate checkout writes. The resolved target must pass the
/// same [`readonly_dir_is_git_repository`] predicate — `read_gitfile`
/// validates it the same way and fatals on a non-repository, and admitting
/// a link whose scan finds nothing would let the child die at that fatal
/// instead. Anything the lane cannot decode or read refuses the same way.
fn readonly_git_dir_for(cwd: &std::path::Path) -> Result<Option<(PathBuf, PathBuf)>, String> {
    let mut dir = cwd.to_path_buf();
    loop {
        let candidate = dir.join(".git");
        match std::fs::symlink_metadata(&candidate) {
            Ok(_) if candidate.is_dir() => {
                if readonly_dir_is_git_repository(&candidate) {
                    return Ok(Some((candidate, dir)));
                }
            }
            Ok(_) if candidate.is_file() => {
                // Bound before reading: a crafted link file of any size would
                // otherwise buy a full-size transient allocation per scan. git
                // parses this file byte-wise and consumes the whole value;
                // anything the lane cannot decode is a repository git would
                // still run (its filters included), so it refuses instead of
                // reporting "no repository".
                let Some(bytes) = read_bounded_regular_file(&candidate, READONLY_GIT_META_SCAN_CAP)
                    .map_err(|error| format!("the .git link at {error}"))?
                else {
                    // `is_file` saw the link; if it vanished between that check
                    // and the read, refusing beats guessing at a moving tree.
                    return Err(format!(
                        "the .git link at {} vanished before it could be read",
                        candidate.display()
                    ));
                };
                // git consumes the WHOLE file, not one line, and strips an
                // unbounded run of trailing `\r`/`\n` bytes in any order —
                // probe-pinned: a value ending in `\n\n`, `\r\r`, or `\n\r`
                // resolves the fully stripped target and runs THAT repository,
                // where a one-strip model resolves a decoy path one byte longer
                // and scans the wrong (or no) repository while git runs the
                // hostile one. Every remaining byte is the target path,
                // interior newlines included: a link whose value carries an
                // interior newline resolves a gitdir literally named with that
                // newline and runs THAT repository's content filters, so a
                // first-line cut would scan the decoy the first line names
                // while git ran a hostile repo this lane never saw. The whole
                // value is therefore what this lane resolves — the byte-exact
                // mirror, so the scan reaches the same repository git runs
                // (pinned end to end by the multi-line-gitdir-link filter
                // test).
                let text = std::str::from_utf8(&bytes).map_err(|_| {
                    format!(
                        "the .git link at {} holds bytes this lane does not decode",
                        candidate.display()
                    )
                })?;
                let stripped = text.trim_end_matches(['\n', '\r']);
                // The value is a C string to git: the first NUL byte ends it
                // (probe-pinned — `gitdir: <target>\0junk` resolves `<target>`
                // and runs THAT repository's filters), so bytes past the first
                // NUL are bytes git never sees.
                let stripped = match stripped.find('\0') {
                    Some(nul) => &stripped[..nul],
                    None => stripped,
                };
                // git requires the literal lowercase prefix plus exactly one
                // space; a missing, doubled, or tab-replaced space fatals
                // there (`invalid gitfile format`). Trailing space or tab rides
                // inside the value as an ordinary path byte — git resolves the
                // padded spelling against the link's directory and runs a
                // repository found at the padded path (probe-pinned) — and this
                // lane refuses that class instead of following it, the
                // fail-closed side of a mirror no legitimate checkout needs.
                let Some(target) = stripped.strip_prefix("gitdir: ") else {
                    return Err(format!(
                        "the .git link at {} is not a spelling git accepts (invalid gitfile format)",
                        candidate.display()
                    ));
                };
                if target.is_empty()
                    || target.starts_with([' ', '\t'])
                    || target.ends_with([' ', '\t'])
                {
                    return Err(format!(
                        "the .git link at {} names a padded or empty target this lane refuses",
                        candidate.display()
                    ));
                }
                let gitdir = if std::path::Path::new(target).is_absolute() {
                    PathBuf::from(target)
                } else {
                    dir.join(target)
                };
                // `read_gitfile` validates the target it resolved with the same
                // `is_git_directory` predicate the discovery loop applies to
                // `.git` directories, and fatals on a non-repository; this is
                // that same predicate, so a link naming a non-repository refuses
                // here instead of admitting a read whose discovery scans
                // nothing and whose child dies at git's own `not a git
                // repository` fatal — while a target git accepts (a
                // dangling-HEAD symlink validates) is scanned instead of
                // refused.
                if !readonly_dir_is_git_repository(&gitdir) {
                    return Err(format!(
                        "the .git link at {} names {} which is not a git directory (git itself fatals there)",
                        candidate.display(),
                        gitdir.display()
                    ));
                }
                return Ok(Some((gitdir, dir)));
            }
            // A dangling `.git` symlink (and any other entry that is neither
            // a followed directory nor a followed file — FIFO, socket,
            // device) takes git's NOT_A_FILE branch, whose
            // `is_git_directory` then fails for lack of a usable HEAD — the
            // walk continues, like the stat failure below it.
            Ok(_) => {}
            // A stat that fails (missing, an unreadable ancestor, a symlink
            // loop) is git's STAT_FAILED: the walk continues.
            Err(_) => {}
        }
        let Some(parent) = dir.parent() else {
            return Ok(None);
        };
        dir = parent.to_path_buf();
    }
}

/// Filter declarations armed by one git directory's config files: its own
/// `config` plus the shared config a linked worktree points at through
/// `commondir`. Both the pointer files and the config are read bounded on
/// the opened handle — regular files only, non-blocking on unix, size-capped
/// — and anything else refuses instead of scanning a partial set; non-UTF-8
/// bytes decode lossily and a name carrying the replacement marker refuses
/// at the name wall. `core.worktree` is validated only for a
/// main git directory: git resolves a linked worktree from its gitdir
/// back-pointer and IGNORES `core.worktree` even when the shared config
/// declares it (probe-pinned: the toplevel is the linked worktree while the
/// filter in the shared config still fires), and the shared config already
/// reaches this scan through the `commondir` chain.
/// Echo a config-derived spelling inside a refusal message without letting
/// a crafted value buy unbounded error text: a filter name or a
/// `core.worktree` value can be file-sized, the refusal reaches the model
/// verbatim, and discovery reads can follow symlinks — so the echoed
/// spelling is also kept well short of an outside-file read-back channel.
fn readonly_refusal_echo(value: &str) -> &str {
    const MAX_REFUSAL_ECHO_BYTES: usize = 200;
    if value.len() <= MAX_REFUSAL_ECHO_BYTES {
        return value;
    }
    let mut cut = MAX_REFUSAL_ECHO_BYTES;
    while !value.is_char_boundary(cut) {
        cut -= 1;
    }
    &value[..cut]
}

fn readonly_git_config_filter_names(
    gitdir: &std::path::Path,
    worktree_dir: &std::path::Path,
) -> Result<Vec<ReadonlyGitFilterPin>, String> {
    let mut config_paths = vec![gitdir.join("config")];
    let commondir_path = gitdir.join("commondir");
    // A git directory carrying a `commondir` pointer is a linked worktree;
    // its configs skip the `core.worktree` validation git itself skips.
    let mut linked_worktree = false;
    match std::fs::metadata(&commondir_path) {
        // A git directory without a commondir pointer shares nothing.
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => {
            return Err(format!(
                "the commondir pointer at {} could not be read: {error}",
                commondir_path.display()
            ));
        }
        Ok(_metadata) => {
            let Some(bytes) =
                read_bounded_regular_file(&commondir_path, READONLY_GIT_META_SCAN_CAP)
                    .map_err(|error| format!("the commondir pointer at {error}"))?
            else {
                return Err(format!(
                    "the commondir pointer at {} vanished before it could be read",
                    commondir_path.display()
                ));
            };
            linked_worktree = true;
            // Byte-exact like the `.git` link, through the shared pointer
            // reader: git strips an unbounded run of trailing `\r`/`\n`
            // bytes (any order and count — probe-pinned: `\n\n`, `\r\r`,
            // and `\n\r` endings all resolve the fully stripped shared
            // directory), ends the value at the first NUL (git consumes it
            // as a C string — a NUL-bearing pointer resolves and runs the
            // prefix target, probe-pinned with a firing filter), and
            // resolves the rest as raw byte paths, interior newlines and
            // non-UTF-8 path bytes included — so this lane scans exactly
            // the shared directory git reads. git
            // resolves the value it consumed (a dangling spelling fatals
            // there before anything runs, while a padded one resolves a
            // path carrying the padding and runs a directory found there —
            // probe-pinned — which this mirror follows just as exactly), so
            // a value this lane cannot resolve refuses instead of silently
            // scanning a config git never opens. An empty pointer names no
            // shared directory in git either.
            let common_dir = readonly_git_pointer_target(&bytes, gitdir).map_err(|error| {
                format!(
                    "the commondir pointer at {} {error}",
                    commondir_path.display()
                )
            })?;
            // git resolves the value it consumed — a dangling spelling
            // fatals there before anything runs — so a value this lane
            // cannot resolve refuses instead of silently scanning a config
            // git never opens.
            let common_dir = common_dir.canonicalize().map_err(|error| {
                format!(
                    "the commondir pointer at {} names a shared directory git could not resolve ({error})",
                    commondir_path.display()
                )
            })?;
            config_paths.push(common_dir.join("config"));
        }
    }
    let mut names = Vec::new();
    for path in config_paths {
        // Bounded on the opened handle: a crafted config of any size would
        // otherwise buy a full-size transient allocation per scanned
        // repository, and a FIFO at a config path would otherwise block the
        // pre-spawn scan in `open` until a writer appears.
        let Some(bytes) = read_bounded_regular_file(&path, READONLY_GIT_CONFIG_SCAN_CAP)
            .map_err(|error| format!("filter-discovery config {error}"))?
        else {
            // A repository without a config file there declares nothing.
            continue;
        };
        let text = String::from_utf8_lossy(&bytes);
        // One parse per config text serves both consumers — `core.worktree`
        // validation and the filter-name accumulation each re-parsed the
        // same text before.
        let settings = parse_git_config_settings(&text)?;
        if !linked_worktree {
            validated_core_worktree(&settings, gitdir, worktree_dir)?;
        }
        names.extend(armed_git_filter_names(&settings)?);
    }
    Ok(names)
}

/// One key setting parsed from git config text, with the section it
/// appeared under. Values are raw — git's quote/escape processing is not
/// applied — so consumers needing a value spell their own fail-closed rules.
/// `continued` marks a value that spans more than one physical line: this
/// parser keeps only the first line's text, while git joins the continuation
/// (dropping each `\`+newline and decoding escapes), so a consumer that
/// admits on the raw value would judge a path git never applies.
struct ParsedGitConfigSetting {
    base: String,
    subsection: Option<String>,
    key: String,
    value: Option<String>,
    continued: bool,
}

/// A physical git config line continues its value onto the next one iff it
/// ends in an odd number of backslashes — each pair is an escaped literal
/// backslash, so an even count ends the value (probe-pinned against git's
/// own parser). The input is the COMMENT-STRIPPED text of the line
/// ([`strip_git_config_comment`]): a comment ends the logical line, so a
/// backslash inside a comment never continues it, and a trailing `\r` (git
/// strips one per line) must already be gone.
fn git_config_line_continues(raw: &str) -> bool {
    let trimmed = raw.trim_end();
    let mut trailing = 0usize;
    for ch in trimmed.chars().rev() {
        if ch == '\\' {
            trailing += 1;
        } else {
            break;
        }
    }
    trailing % 2 == 1
}

/// Cut one physical config line at its first unquoted comment marker,
/// mirroring git: `#`/`;` start a comment anywhere outside double quotes —
/// in a value or after a header's `]` — and the rest of the line is dropped
/// with the logical line ending there (a backslash inside a comment never
/// continues it). A `"` toggles quote state unless an odd run of preceding
/// backslashes escapes it, and `in_quotes` carries that state in from the
/// caller (continuation lines keep the opening line's state) and back out.
/// Returns the comment-stripped text, the quote state at the cut (or at end
/// of line when no comment was found), and whether a comment was found.
fn strip_git_config_comment(line: &str, mut in_quotes: bool) -> (&str, bool, bool) {
    let mut backslash_run = 0usize;
    for (index, ch) in line.char_indices() {
        if ch == '\\' {
            backslash_run += 1;
            continue;
        }
        let escaped = backslash_run % 2 == 1;
        backslash_run = 0;
        if ch == '"' && !escaped {
            in_quotes = !in_quotes;
        } else if (ch == '#' || ch == ';') && !in_quotes {
            return (&line[..index], in_quotes, true);
        }
    }
    (line, in_quotes, false)
}

/// The byte index of the `]` closing a section header on one line, found
/// with a quote-aware scan: a `]` inside a double-quoted subsection name is
/// literal (git accepts it; this scanner later refuses such names
/// fail-closed, but must find the real close rather than mis-parse).
fn git_config_header_close(line: &str) -> Option<usize> {
    let mut in_quotes = false;
    let mut backslash_run = 0usize;
    for (index, ch) in line.char_indices() {
        if ch == '\\' {
            backslash_run += 1;
            continue;
        }
        let escaped = backslash_run % 2 == 1;
        backslash_run = 0;
        if ch == '"' && !escaped {
            in_quotes = !in_quotes;
        } else if ch == ']' && !in_quotes {
            return Some(index);
        }
    }
    None
}

/// Split a header's base text into the section and optional dotted
/// subsection, both lowercased the way git folds them. The split lands on
/// the FIRST dot, so `[base.sub]` is section `base` and subsection `sub`,
/// `[FILTER.EV]` arms driver `ev` (the subsection is case-folded), and
/// `[filter.a.b]` is section `filter` with subsection `a.b`.
fn split_git_header_base(base_text: &str) -> (String, Option<String>) {
    let base_text = base_text.trim();
    match base_text.split_once('.') {
        Some((base, dotted)) => (
            base.trim().to_ascii_lowercase(),
            Some(dotted.trim().to_ascii_lowercase()),
        ),
        None => (base_text.to_ascii_lowercase(), None),
    }
}

/// Whether a quoted header name closes early: the caller strips the suffix
/// `"` expecting it to be the closer, so that holds only while the
/// escape-aware scan never toggles out of the opening quote. An interior
/// unescaped quote toggles out early and leaves the suffix to re-open
/// instead of close — that shape (and an escaped `\"`, which the name
/// charset refuses downstream) is not a spelling this scanner resolves, so
/// it refuses instead of mis-parsing the header.
fn git_quoted_name_unterminated(name: &str) -> bool {
    // The opening quote was consumed by the caller.
    let mut in_quotes = true;
    let mut backslash_run = 0usize;
    for ch in name.chars() {
        if ch == '\\' {
            backslash_run += 1;
            continue;
        }
        let escaped = backslash_run % 2 == 1;
        backslash_run = 0;
        if ch == '"' && !escaped {
            in_quotes = !in_quotes;
        }
    }
    // Still inside at the end: every quote char the name carried was
    // escaped, and the stripped suffix closes the opening. Toggled out:
    // the suffix re-opens, so the name never closed.
    !in_quotes
}

/// Parse one `[...]` section header's text (between `[` and the unquoted
/// `]`) into its base section and optional subsection, mirroring git's four
/// shapes: `[base]`, the dotted `[base.subsection]` (split at the FIRST
/// dot, the subsection case-folded), the quoted `[base "Sub"]` (the name
/// byte-exact), and the combined `[filter.x "y"]` (section `filter`,
/// subsection `x.y` with the dotted part lowercased and the quoted name
/// appended). Base and dotted parts are lowercased the way git folds them.
fn parse_git_config_header(header: &str) -> Result<(String, Option<String>), String> {
    let Some(quote) = header.find('"') else {
        let (base, dotted) = split_git_header_base(header);
        return Ok((base, dotted));
    };
    let Some(name) = header[quote + 1..].strip_suffix('"') else {
        return Err("a filter subsection is not terminated".to_string());
    };
    if git_quoted_name_unterminated(name) {
        return Err("a filter subsection is not terminated".to_string());
    }
    let (base, dotted) = split_git_header_base(&header[..quote]);
    let subsection = match dotted {
        Some(dotted) => Some(format!("{dotted}.{name}")),
        None => Some(name.to_string()),
    };
    Ok((base, subsection))
}

/// Parse every key setting of one git config text, mirroring the parts of
/// git's own parser this lane depends on: a leading UTF-8 BOM is skipped
/// (git skips it too; a mid-file BOM makes git fatal, so no setting can
/// hide behind one), one trailing `\r` per physical line (CRLF) is
/// stripped, `#`/`;` start comments outside double quotes anywhere in a
/// line with the quote state carrying across continuation lines, keys on a
/// section-header line parse (a same-line `[filter "x"] clean = v` arms a
/// filter git runs while a header-only skip would hide it — probe-pinned),
/// a line re-targets the section at every `[` after a header's `]`
/// (`[a] [b] k = v` parses `b.k`), headers parse in all four git shapes
/// ([`parse_git_config_header`]), `include`/`includeIf` headers refuse (an
/// included file can declare filters outside this scan), and a value
/// continues over the next physical lines only while the comment-stripped
/// lines are continuations. Returns a refusal reason for anything that
/// could hide a setting from the caller's scan. The mirror is
/// one-directional by design: shapes git itself fatals on (a mid-file BOM,
/// `= value` with no key, an unterminated quote) parse through here and can
/// only over-pin a conversion git never arms — the enumeration child dies
/// at git's bad-config fatal and the command refuses.
fn parse_git_config_settings(config_text: &str) -> Result<Vec<ParsedGitConfigSetting>, String> {
    let config_text = config_text.strip_prefix('\u{feff}').unwrap_or(config_text);
    let mut settings: Vec<ParsedGitConfigSetting> = Vec::new();
    // (section, subsection) of the last `[...]` header; `None` before one.
    let mut section: Option<(String, Option<String>)> = None;
    let mut continuation = false;
    // Index into `settings` of the setting whose value the current logical
    // line's continuation extends, if any (`None` for a pre-section key
    // line, whose swallowed continuation lines belong to no setting).
    let mut continuing_setting: Option<usize> = None;
    // Quote state carried across the continuation lines of the current
    // logical line; every fresh logical line starts unquoted.
    let mut in_quotes = false;
    for raw_line in config_text.lines() {
        // git strips ONE trailing `\r`, so a CRLF file parses like an LF one.
        let raw = raw_line.strip_suffix('\r').unwrap_or(raw_line);
        let (text, quotes_after, comment_found) = strip_git_config_comment(raw, in_quotes);
        if continuation {
            // Continuation text is value, not key, and must never be read
            // as one. The value keeps going while lines keep ending in an
            // unescaped backslash; an unquoted comment ends the logical
            // line there (a backslash inside a comment never continues
            // it), and the quote state carried from the opening line
            // decides whether that line's `#`/`;` even is a comment.
            if comment_found {
                continuation = false;
                in_quotes = false;
            } else {
                continuation = git_config_line_continues(text);
                in_quotes = if continuation { quotes_after } else { false };
            }
            // The captured value is only this logical line's first physical
            // line; mark the setting it continues so value-dependent
            // consumers can refuse instead of judging a prefix of the
            // string git decodes. A pre-section key line has no setting to
            // mark (git swallows those lines too).
            if let Some(index) = continuing_setting {
                settings[index].continued = true;
            }
            continue;
        }
        in_quotes = false;
        let line = text.trim();
        if line.is_empty() {
            // A whole-line comment or a blank line; comments end at the
            // newline, so nothing can dangle here.
            continue;
        }
        let key_text = if line.starts_with('[') {
            let mut rest = line;
            loop {
                // The closing `]` is found with a quote-aware scan (a `]`
                // inside a quoted subsection name is literal), then git's
                // same-line re-targeting loops: another `[` after a
                // header's `]` starts a new section immediately.
                let Some(close) = git_config_header_close(rest) else {
                    return Err("a section header is not terminated".to_string());
                };
                let (base, subsection) = parse_git_config_header(&rest[1..close])?;
                // An `include`d file can declare filters outside this scan.
                if base == "include" || base == "includeif" {
                    return Err("the repository config carries `include` directives".to_string());
                }
                section = Some((base, subsection));
                rest = rest[close + 1..].trim_start();
                if !rest.starts_with('[') {
                    break;
                }
            }
            if rest.is_empty() {
                if comment_found {
                    // A comment ends the logical line; nothing dangles.
                    continue;
                }
                if git_config_line_continues(text) {
                    return Err("a section header line ends in a dangling continuation".to_string());
                }
                continue;
            }
            rest
        } else {
            line
        };
        // The value may continue over the next physical lines (an odd
        // trailing-backslash count on the header line continues a same-line
        // key's value too). A comment already ended the logical line, and
        // the quote state carries into continuations only when the line
        // actually continues. This must run before the no-section skip: a
        // pre-section key line ending in an odd backslash makes git swallow
        // the NEXT line as value text — a following section header becomes
        // part of the value and arms nothing (probe-pinned) — so this
        // parser must carry the continuation state into that line too,
        // exactly like git.
        continuation = !comment_found && git_config_line_continues(text);
        in_quotes = if continuation { quotes_after } else { false };
        let Some((base, subsection)) = &section else {
            // A key before any header is not a setting; git keeps it as a
            // section-less entry that no section-scoped lookup reaches.
            continue;
        };
        let (key, value) = match key_text.split_once('=') {
            Some((key, value)) => (
                key.trim().to_ascii_lowercase(),
                Some(value.trim().to_string()),
            ),
            None => (key_text.trim().to_ascii_lowercase(), None),
        };
        if base == "extensions" && key == "worktreeconfig" {
            return Err(
                "the repository enables the worktree-config extension, which adds a config file this scan does not read".to_string(),
            );
        }
        continuing_setting = Some(settings.len());
        settings.push(ParsedGitConfigSetting {
            base: base.clone(),
            subsection: subsection.clone(),
            key,
            value,
            continued: continuation,
        });
    }
    Ok(settings)
}

/// Filter declarations armed by one config text. Only `filter.<name>` keys
/// that execute a conversion arm a pin — `required` alone never does — and
/// names are matched byte-exactly, the way git matches an attribute value
/// against a config subsection: case-variant subsections are two drivers,
/// and case-folding one onto the other would pin a spelling git never looks
/// up while leaving the live one armed.
fn armed_git_filter_names(
    settings: &[ParsedGitConfigSetting],
) -> Result<Vec<ReadonlyGitFilterPin>, String> {
    let mut names: Vec<ReadonlyGitFilterPin> = Vec::new();
    for setting in settings {
        if setting.base != "filter" {
            continue;
        }
        let armed_key = match setting.key.as_str() {
            "clean" => Some(0),
            "smudge" => Some(1),
            "process" => Some(2),
            // `required` alone never executes a conversion command: with no
            // clean/smudge/process defined, git's own conversion failure is
            // the wall, so there is nothing to pin.
            _ => None,
        };
        let Some(armed_key) = armed_key else {
            continue;
        };
        // A `[filter]` section without a subsection builds two-part keys
        // (`filter.clean`), which no `filter.<driver>.<key>` driver lookup
        // ever reads — inert, nothing to pin.
        let Some(name) = &setting.subsection else {
            continue;
        };
        let pinned = if let Some(pinned) = names.iter_mut().find(|pinned| &pinned.name == name) {
            pinned
        } else {
            if names.len() >= MAX_READONLY_GIT_FILTER_PINS {
                return Err(format!(
                    "the config declares more than {MAX_READONLY_GIT_FILTER_PINS} distinct content filters, more than the read-only lane can pin inert"
                ));
            }
            names.push(ReadonlyGitFilterPin::new(name.clone()));
            names.last_mut().expect("just pushed")
        };
        match armed_key {
            0 => pinned.clean = true,
            1 => pinned.smudge = true,
            _ => pinned.process = true,
        }
    }
    for pin in &names {
        if pin.name.is_empty()
            || pin.name.len() > MAX_DISCOVERED_FILTER_NAME_BYTES
            || !pin
                .name
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '.' | '_' | '-'))
        {
            return Err(format!(
                "the repository declares a filter named {:?}, whose spelling this lane does not pin",
                readonly_refusal_echo(&pin.name)
            ));
        }
    }
    names.retain(ReadonlyGitFilterPin::arms_a_conversion);
    Ok(names)
}

/// Validate a `core.worktree` declaration against the worktree the
/// repository was discovered in. An absorbed submodule legitimately points
/// its `core.worktree` back at its own checkout (`worktree = ../../../sub`
/// relative to the absorbed gitdir), so a declaration that resolves to
/// exactly the discovered worktree admits; anything else — a redirect
/// outside the workspace or merely elsewhere in it — refuses, because an
/// admitted read would list and display files outside the tree this lane
/// scanned and bounded. Only the key git itself applies matches here: a
/// subsection-carrying spelling (`[core "sub"] worktree = …`, which git
/// lists as the inert key `core.sub.worktree`) never relocates anything and
/// must not refuse the command. git applies the LAST value of a duplicated
/// `core.worktree`, so the last declaration is the one validated — taking
/// the first would admit a worktree a later duplicate relocates.
fn validated_core_worktree(
    settings: &[ParsedGitConfigSetting],
    gitdir: &std::path::Path,
    worktree_dir: &std::path::Path,
) -> Result<(), String> {
    let Some(setting) = settings.iter().rev().find(|setting| {
        setting.base == "core" && setting.key == "worktree" && setting.subsection.is_none()
    }) else {
        return Ok(());
    };
    // git applies the continuation-joined, escape-decoded value; this
    // parser captured only the first physical line. A hostile config can
    // drive the two apart — a raw value that canonicalizes back to the
    // discovered worktree while the decoded value relocates git elsewhere
    // (probe-pinned with a `w\`-continuation fixture) — so a continued
    // value refuses instead of validating a prefix of git's string.
    if setting.continued {
        return Err(format!(
            "the repository config declares `core.worktree` with a continued value ({:?}); this scan reads the first physical line only while git joins the continuation, so the declared worktree cannot be validated against the tree this lane bounds",
            readonly_refusal_echo(setting.value.as_deref().unwrap_or(""))
        ));
    }
    // git also escape-decodes quoted values (`\n` `\t` `\b` `\\` `\"` become
    // control bytes, probe-pinned against git 2.43: a config value spelled
    // `"w\nx"` chdir's to a name containing a real newline) and strips the
    // quotes themselves, while this parser captured the raw spelling
    // byte-for-byte. Any `\` or `"` in the raw value means the string git
    // applies diverges from the string validated here — a raw spelling that
    // canonicalizes back to the discovered worktree through a decoy link can
    // decode (or unquote) to an outside relocation — so such a value refuses
    // instead of validating a prefix of git's string. An unquoted,
    // backslash-free single line keeps admitting: there, the raw value IS
    // the string git applies (git keeps backslashes literal outside quotes).
    let declared = setting.value.clone().unwrap_or_default();
    if declared.contains('\\') || declared.contains('"') {
        return Err(format!(
            "the repository config declares `core.worktree` with a quoted or backslash-escaped value ({:?}); git escape-decodes quoted values and strips their quotes, so the raw spelling this scan validated is not the string git would apply, and the declared worktree cannot be validated against the tree this lane bounds",
            readonly_refusal_echo(&declared)
        ));
    }
    // git resolves a relative `core.worktree` against the git directory.
    let joined = if std::path::Path::new(&declared).is_absolute() {
        PathBuf::from(&declared)
    } else {
        gitdir.join(&declared)
    };
    let declared_worktree = joined.canonicalize().map_err(|error| {
        format!(
            "the repository config declares `core.worktree` {:?}, which does not resolve ({error}); refusing a relocated worktree",
            readonly_refusal_echo(&declared)
        )
    })?;
    let discovered = worktree_dir
        .canonicalize()
        .map_err(|error| format!("the repository worktree could not be resolved: {error}"))?;
    if declared_worktree != discovered {
        return Err(format!(
            "the repository config declares `core.worktree` {:?}, which relocates the worktree away from the tree this lane bounds",
            readonly_refusal_echo(&declared)
        ));
    }
    Ok(())
}

fn enforce_readonly_workspace_operands(
    command: &str,
    roots: &[PathBuf],
    effective_cwd: &std::path::Path,
) -> Result<(), ToolError> {
    let roots = canonical_readonly_roots(roots).map_err(|error| {
        ToolError::execution_failed(format!(
            "Could not resolve the read-only workspace before shell dispatch: {error}"
        ))
    })?;
    let effective_cwd = effective_cwd.canonicalize().map_err(|error| {
        ToolError::permission_denied(format!(
            "Could not prove the read-only shell working directory stays in the workspace: {error}"
        ))
    })?;
    if !roots.iter().any(|root| effective_cwd.starts_with(root)) {
        return Err(ToolError::permission_denied(
            "[shell.readonly.cwd.outside_workspace] Read-only shell working directory resolves outside the workspace.",
        ));
    }

    // Compound sequences are judged segment by segment, with exactly the
    // split the admitting classifier used, so an operand can never hide in a
    // later `;`/`&&`/`||` segment behind a clean first command (a whole-
    // command token parse glues the separator onto the operand spelling and
    // both mis-resolves and mis-reports it). A bare `&` never classifies, so
    // a `None` split reaching this gate fails closed instead of checking
    // nothing. The shell changes its live working directory at each admitted
    // `cd`, so the segments fold the same drift left-to-right: after an
    // admitted `cd` the base cwd for every later segment becomes the
    // canonicalized destination, and relative operands resolve exactly where
    // the shell will resolve them — a symlink that exists only under the
    // `cd` destination can otherwise slip past the dispatch-cwd check while
    // the shell still dereferences it. The fold only mirrors the shell while
    // the `cd` actually runs, which the connector carries: a `cd` joined by
    // `&&`/`||` may never execute at all, and the segment fn refuses it
    // before it could fold.
    let Some(segments) = readonly_sequence_segments_with_connectors(command) else {
        return Err(ToolError::invalid_input(
            "Read-only command contains a bare `&` and matches no admitted shape.".to_string(),
        ));
    };
    // One glob budget for the whole command: the caps bound the host-side
    // walk, and a per-operand budget would let a command multiply it by
    // however many `*` operands fit in the command text.
    let mut glob_budget = ReadonlyGlobBudget::default();
    let mut live_cwd = effective_cwd;
    for (segment, connector) in segments {
        live_cwd = enforce_readonly_segment_operands(
            segment,
            connector,
            &roots,
            &live_cwd,
            &mut glob_budget,
        )?;
    }
    Ok(())
}

/// The gh exemption skips every operand judgment for a gh stage, on the
/// rationale that high-level gh reads consume no workspace path operands.
/// They do, however, accept `HOST/OWNER/REPO` operands the classifier
/// host-checks by their literal spelling, and those operands are strings
/// gh consumes itself — not workspace paths — so there is no workspace walk
/// that could bound a wildcard's expansion host-side. The exemption
/// therefore holds only for literal operands: ANY wildcard token after the
/// program word — quoted or bare — refuses.
fn refuse_gh_stage_glob_operands(argv: &[String]) -> Result<(), ToolError> {
    if let Some(token) = argv.iter().skip(1).find(|token| token.contains('*')) {
        return Err(ToolError::permission_denied(format!(
            "[shell.readonly.operand.gh_glob] Read-only gh stages are judged by their literal operands, and {token:?} carries a wildcard; the lane refuses any wildcard token on a gh stage — quoted or bare — because a gh stage's operand scope cannot be bounded host-side. Spell the operand literally or drop it."
        )));
    }
    Ok(())
}

/// Operand containment for one sequence segment of a read-only command.
///
/// A segment with a pipeline is judged stage by stage, because every stage
/// runs in its own subshell against the same working directory: a `gh`
/// stage exempts only itself (high-level gh reads consume no local path
/// operands; their host is pinned and evaluated separately by the
/// network-policy guard), and a `cd` stage is refused outright — it persists
/// nowhere, so there is no destination to fold and nothing a later stage
/// could safely be resolved against. Judging the whole segment by its first
/// program would let `gh … | cat /etc/passwd` or `cd sub | cat /etc/passwd`
/// skip every operand after the `|`.
///
/// An unpiped segment that is `cd <one relative path>` has its operand
/// resolved against the workspace like the shell working directory itself.
/// Returns the working directory the next segment runs under: unchanged for
/// every other segment, the canonicalized `cd` destination for an admitted
/// `cd`.
fn enforce_readonly_segment_operands(
    segment: &str,
    connector: crate::command_safety::ReadonlySequenceConnector,
    roots: &[PathBuf],
    effective_cwd: &std::path::Path,
    glob_budget: &mut ReadonlyGlobBudget,
) -> Result<std::path::PathBuf, ToolError> {
    let stages = crate::command_safety::readonly_pipeline_stages(segment);
    if stages.len() > 1 {
        for stage in stages {
            let argv = readonly_segment_argv(stage)?;
            match argv.first().map(String::as_str) {
                Some("cd") => {
                    return Err(ToolError::permission_denied(format!(
                        "[shell.readonly.cwd.pipeline_stage] Read-only `cd` is admitted only as a whole sequence segment, never as a pipeline stage; refused {stage:?}."
                    )));
                }
                Some("gh") => refuse_gh_stage_glob_operands(&argv)?,
                Some("git") => {
                    // git's `-C` preamble accumulates: the stage's operands
                    // resolve against the directory the stage actually runs
                    // under, through the same helper the discovery pass
                    // walks, so a chain of literal operands like
                    // `git -C A -C escape` is judged exactly like the flat
                    // `git A/escape` it executes as. The `-C` values are
                    // judged inside the helper — a value the shell would
                    // expand (wildcard, tilde, zsh `=`) refuses there,
                    // because these operands are sliced off before the
                    // plain-operand walker could expansion-judge them.
                    let stage_root =
                        readonly_git_stage_root(&argv, effective_cwd, roots).map_err(|reason| {
                            ToolError::permission_denied(format!(
                                "[shell.readonly.operand.outside_workspace] {reason}"
                            ))
                        })?;
                    enforce_readonly_argv_operands(
                        &argv[readonly_git_preamble_len(&argv)..],
                        roots,
                        &stage_root,
                        glob_budget,
                        // The stage root is where git resolves the words, but
                        // the SHELL expanded any wildcard against its own cwd
                        // before git saw them — judge both bases (see the
                        // walker's doc).
                        Some(effective_cwd),
                    )?;
                }
                // An empty stage (`a | | b`) refuses too: the classifier
                // rejects it, and this gate's doc promises every stage is
                // judged — an empty argv would otherwise pass silently.
                None => {
                    return Err(ToolError::invalid_input(format!(
                        "Read-only pipeline carries an empty stage; refused {segment:?}."
                    )));
                }
                _ => {
                    enforce_readonly_argv_operands(&argv, roots, effective_cwd, glob_budget, None)?
                }
            }
        }
        return Ok(effective_cwd.to_path_buf());
    }
    let argv = readonly_segment_argv(segment)?;
    if argv.is_empty() {
        return Err(ToolError::invalid_input(format!(
            "Read-only sequence carries an empty segment; refused {segment:?}."
        )));
    }
    if argv.first().is_some_and(|program| program == "gh") {
        refuse_gh_stage_glob_operands(&argv)?;
        return Ok(effective_cwd.to_path_buf());
    }
    if argv.first().is_some_and(|program| program == "cd") {
        // A `cd` after `&&`/`||` runs only when the predecessor's exit status
        // allows it — a runtime fact no pre-spawn gate can evaluate — while
        // the fold below is unconditional. `cat nope && cd sub; cat link`
        // would leave the shell in the original directory (where a planted
        // root symlink awaits) while the gate judged `link` under `sub/` and
        // skipped it at the exists() miss. `;` and the sequence start run
        // their `cd` whatever came before, so only those fold.
        if matches!(
            connector,
            crate::command_safety::ReadonlySequenceConnector::AndThen
                | crate::command_safety::ReadonlySequenceConnector::OrElse
        ) {
            return Err(ToolError::permission_denied(
                "[shell.readonly.cwd.conditional] Read-only `cd` joined by `&&`/`||` may never run at all, so the gate refuses to fold later operands onto its destination; put the `cd` first or join the sequence with `;`."
                    .to_string(),
            ));
        }
        // `cd` is admitted only as `cd <one relative path>`. Resolve the
        // operand against the workspace exactly like the shell's own working
        // directory and reuse the same refusal tag, so a destination outside
        // the workspace (absolute, or a relative symlink pointing out) fails
        // with the boundary the model already knows. The classifier's shape
        // rule (`readonly_cd_operand_admitted`) is then applied here again so
        // the seam never folds onto a spelling the shell reinterprets —
        // `cd =ls` on zsh expands to `/bin/ls`, the shell's `cd` fails, and a
        // `;`/`||` sequence continues in the original directory while a fold
        // onto a literal `=ls` directory would have judged every later
        // operand somewhere the shell never went.
        let target = argv.get(1).map(String::as_str).unwrap_or_default();
        if argv.len() != 2 {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.cwd.shape] Read-only `cd` takes exactly one relative directory operand; refused {:?}.",
                argv[1..].join(" ")
            )));
        }
        let joined = effective_cwd.join(target);
        let resolved = joined.canonicalize().map_err(|error| {
            ToolError::permission_denied(format!(
                "[shell.readonly.cwd.outside_workspace] Could not prove read-only `cd` operand {target:?} stays inside the workspace because it could not be resolved: {error}"
            ))
        })?;
        if !roots.iter().any(|root| resolved.starts_with(root)) {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.cwd.outside_workspace] Read-only `cd` operand {target:?} resolves outside the workspace."
            )));
        }
        // Judged after containment so an outside destination keeps the
        // boundary message; what remains are inside-resolving spellings the
        // shell would still reinterpret.
        if !crate::command_safety::readonly_cd_operand_admitted(target) {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.cwd.shape] Read-only `cd` takes exactly one relative directory operand without `..`, `~`, `=`, `\\`, a `+`/`-` prefix, or a wildcard; refused {target:?}."
            )));
        }
        // The fold below is only exact while the shell's `cd` succeeds. A
        // destination that exists but is not a directory (`cd notes.txt`)
        // canonicalizes fine, yet the shell's `cd` fails on it and a `;`/`||`
        // sequence continues in the ORIGINAL directory — while the gate would
        // resolve every later operand under a path nothing exists beneath,
        // skipping each one, and `cd notes.txt; cat link` would read a
        // planted outside symlink at the root unjudged. Refuse before the
        // fold can lie.
        if !resolved.is_dir() {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.cwd.not_a_directory] Read-only `cd` operand {target:?} is not a directory; the sequence would continue in the original working directory while the gate judged it under {target:?}."
            )));
        }
        // A directory the process cannot enter fails the shell's `cd` just as
        // deterministically (`chdir` needs search permission on the
        // destination itself, while `canonicalize`/`is_dir` need it only on
        // the ancestors), so a mode-000 or foreign-owned directory would lie
        // to the fold the same way. Resolving `<dir>/.` forces a lookup
        // inside the directory — the exact permission `chdir` needs — without
        // listing it, so a directory that is searchable but unreadable stays
        // admitted exactly as the shell admits it. The remaining gap is the
        // race between this probe and the shell's `cd`, which no pre-spawn
        // check can close.
        if std::fs::metadata(resolved.join(".")).is_err() {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.cwd.unsearchable] Read-only `cd` operand {target:?} is a directory the shell could not enter; the sequence would continue in the original working directory while the gate judged it under {target:?}."
            )));
        }
        return Ok(resolved);
    }
    if argv.first().is_some_and(|program| program == "git") {
        // Same accumulated-root judgment as a piped git stage: a chained
        // `-C` re-targets the directory every operand of the stage resolves
        // against, and the helper judges the `-C` values themselves.
        let stage_root =
            readonly_git_stage_root(&argv, effective_cwd, roots).map_err(|reason| {
                ToolError::permission_denied(format!(
                    "[shell.readonly.operand.outside_workspace] {reason}"
                ))
            })?;
        enforce_readonly_argv_operands(
            &argv[readonly_git_preamble_len(&argv)..],
            roots,
            &stage_root,
            glob_budget,
            // Both expansion bases, exactly like the piped git stage.
            Some(effective_cwd),
        )?;
        return Ok(effective_cwd.to_path_buf());
    }
    enforce_readonly_argv_operands(&argv, roots, effective_cwd, glob_budget, None)?;
    Ok(effective_cwd.to_path_buf())
}

fn readonly_segment_argv(segment: &str) -> Result<Vec<String>, ToolError> {
    shlex::split(&normalize_windows_command_paths(segment)).ok_or_else(|| {
        ToolError::invalid_input("Could not parse read-only command arguments".to_string())
    })
}

/// Judge every operand of one parsed command against the workspace roots,
/// resolving relative spellings against `effective_cwd`.
///
/// `shell_expansion_cwd` is `Some` only for a git stage, whose `effective_cwd`
/// is the accumulated `-C` stage root: the shell pathname-expands a wildcard
/// operand against ITS OWN working directory before git ever sees the words,
/// so a glob must be walked at both bases. The stage-root walk judges where
/// git resolves the word (its pathspec base); the shell-cwd walk judges what
/// the shell actually splices into the command text — a planted dash-named
/// entry the stage-root walk never sees (`git -C .r diff … *` over a hidden
/// clean `.r/` while `--textconv` sits at the workspace root) would re-arm a
/// hardening flag the splice removed, and git is last-flag-wins. Every match
/// of either walk is still judged by its resolved location, and both share
/// the one command glob budget.
fn enforce_readonly_argv_operands(
    argv: &[String],
    roots: &[PathBuf],
    effective_cwd: &std::path::Path,
    glob_budget: &mut ReadonlyGlobBudget,
    shell_expansion_cwd: Option<&std::path::Path>,
) -> Result<(), ToolError> {
    // The classifier's option rows deliberately admit everything after a
    // `--` separator as a plain operand (`options_match_allowlist` flips its
    // option context off). The gate must mirror that flip: past `--` a
    // `-`-prefixed token is a file name the program opens verbatim, so
    // judging only the suffix after `=` would check `b` while the shell
    // hands the program `-a=b` — a planted symlink spelled `-a=b` would be
    // opened under a name the gate never judged.
    let mut past_option_terminator = false;
    for token in argv.iter().skip(1) {
        if !past_option_terminator && token == "--" {
            past_option_terminator = true;
            continue;
        }
        // Past `--` a `-`-prefixed token is a plain file name (the
        // classifier deliberately admits everything after the terminator),
        // so the option wall must not fire there: `cat -- -a/b` reads the
        // workspace file named `-a/b`, and the whole-word judgment below
        // already judges post-`--` tokens by resolved location.
        if !past_option_terminator
            && token.starts_with('-')
            && (token.contains('/') || token.contains('\\'))
        {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.option.attached_path] Read-only shell options may not carry attached paths; refused {token:?}. Use the bounded File read/search actions for project evidence."
            )));
        }
        // zsh's default EQUALS option resolves an operand spelled `=cmd` to
        // that command's absolute path, so `cat =ls | wc -l` reads /bin/ls on
        // the shell lane while a path check would see a relative value that
        // does not exist and skip it. Refuse the spelling outright; a real
        // file named `=ls` is read through the bounded File actions instead.
        // The refusal is unconditional — zsh applies the expansion to the
        // word wherever it appears, `--` included.
        if token.starts_with('=') {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.shape] Read-only shell operands may not start with `=` (shell command-path expansion); refused {token:?}. Use the bounded File read/search actions for project evidence."
            )));
        }
        // Only an option carries its path after an `=` (`--file=notes.md`).
        // A bare operand is the whole word: the shell opens — and pathname-
        // expands — `x=y` as one name, so judging only `y` would read a
        // symlink spelled `x=y` under a name that does not exist, and walk
        // the glob `a*=b*` as `b*`, which matches nothing while the shell
        // matched `ax=b.out`. The spelling is judged exactly as parsed, never
        // trimmed: whitespace survives into a token only through quoting, so
        // `cat ' x '` names a file called ` x ` and a trimmed judgement
        // would look up `x` instead, find nothing, and skip the real name.
        let value = if !past_option_terminator && token.starts_with('-') {
            token
                .split_once('=')
                .map_or(token.as_str(), |(_, value)| value)
        } else {
            token.as_str()
        };
        // Windows `Path::canonicalize` returns verbatim device paths
        // (`\\?\C:\...`), which `Path::is_absolute` does not recognize; without
        // stripping the prefix the operand falls into the shape refusal and
        // every canonical absolute read is denied on Windows. The stripped
        // form resolves to the same location, so location-based judgement is
        // unchanged. Note the `\\.\` branch strips only the three-byte
        // `\\.`: every device-namespace spelling (`\\.\PhysicalDrive0`,
        // `\\.\pipe\name`) and verbatim-UNC form keeps a leading `\`, which
        // the shape gate below refuses fail-closed — the strip only ever
        // normalizes drive-letter remainders. On unix the strip stays off: a
        // `\\?\`-prefixed word there is a plain (odd) POSIX file name, and
        // stripping it would judge a different name than the program opens
        // (`cat '\\.\secret'` read a planted outside symlink as `secret`);
        // the shape gate below fails such spellings closed.
        let value = if cfg!(windows) {
            value
                .strip_prefix(r"\\?\")
                .or_else(|| value.strip_prefix(r"\\.\"))
                .unwrap_or(value)
        } else {
            value
        };
        if value.is_empty() || value == "-" {
            continue;
        }
        let candidate = std::path::Path::new(value);
        let bytes = value.as_bytes();
        let windows_prefixed =
            (bytes.len() >= 2 && bytes[0].is_ascii_alphabetic() && bytes[1] == b':')
                || value.starts_with("\\\\")
                || candidate
                    .components()
                    .any(|component| matches!(component, std::path::Component::Prefix(_)));

        // #5595: an absolute operand is safe by resolved location, not by
        // spelling. This is the canonical `git -C /absolute/workspace log`
        // case. Requiring canonicalization also keeps nonexistent output-like
        // targets fail-closed, while symlinks are judged by their destination.
        if candidate.is_absolute() {
            let resolved = candidate.canonicalize().map_err(|error| {
                ToolError::permission_denied(format!(
                    "[shell.readonly.operand.unresolved] Could not prove absolute read-only operand {value:?} stays inside the workspace because it could not be resolved: {error}"
                ))
            })?;
            if !roots.iter().any(|root| resolved.starts_with(root)) {
                return Err(ToolError::permission_denied(format!(
                    "[shell.readonly.operand.outside_workspace] Read-only shell operand {value:?} resolves outside the workspace. Use the bounded File read/search actions for project evidence."
                )));
            }
            // A glob-bearing absolute operand cannot stop here: the shell
            // pathname-expands an absolute word exactly like a relative one,
            // so containment of the LITERAL spelling (this canonicalize
            // proved a planted `x*`-named entry, not the expansion) proves
            // nothing about what the expansion hands the program — every
            // match the shell synthesizes still needs judging. Walk the
            // glob-bearing suffix from the literal's canonical parent: the
            // match judge applies the same resolved-location,
            // dash-named-wall, and shared-budget rules a relative operand's
            // matches get. The literal prefix above the first glob component
            // is already validated by this canonicalize (and resolves any
            // symlinked directory chain), so only the suffix is walked.
            if value.contains('*') {
                let components: Vec<&str> = value
                    .split('/')
                    .filter(|component| !component.is_empty() && *component != ".")
                    .collect();
                let Some(first_glob) = components
                    .iter()
                    .position(|component| component.contains('*'))
                else {
                    // `value.contains('*')` but no `/`-component carries it:
                    // the `*` rides a separator spelling the component filter
                    // dropped, so there is no expansion surface to judge.
                    continue;
                };
                let suffix = components[first_glob..].join("/");
                // The walk base is the directory that CONTAINS the first
                // glob component: the canonical literal, popped once per
                // suffix component (canonicalize proved each literal prefix
                // segment and resolved its symlink chain; the suffix is the
                // only part the shell still expands).
                let mut walk_base = resolved.as_path();
                for _ in 0..components.len() - first_glob {
                    walk_base = walk_base
                        .parent()
                        .ok_or_else(|| {
                            ToolError::permission_denied(format!(
                                "[shell.readonly.operand.shape] Read-only shell operand {value:?} has no parent directory to walk; refused."
                            ))
                        })?;
                }
                enforce_readonly_glob_operand(&suffix, roots, walk_base, glob_budget)?;
            }
            continue;
        }

        if value.starts_with('~')
            || value.contains('\\')
            || windows_prefixed
            || candidate.has_root()
            || candidate
                .components()
                .any(|component| matches!(component, std::path::Component::ParentDir))
        {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.shape] Read-only shell operands must stay inside the workspace; refused {value:?}. Use the bounded File read/search actions for project evidence."
            )));
        }

        // The shell pathname-expands the whole word, not the part after the
        // `=`: `--exclude-from=zz*` matches a planted `--exclude-from=zzq`
        // while a walk of `zz*` alone matches nothing. Judge the full
        // spelling too whenever an option carries a wildcard anywhere; the
        // union over-approximates every shell's expansion.
        if token.starts_with('-') && token.as_str() != value && token.contains('*') {
            enforce_readonly_glob_operand(token, roots, effective_cwd, glob_budget)?;
            if let Some(shell_cwd) = shell_expansion_cwd.filter(|base| *base != effective_cwd) {
                enforce_readonly_glob_operand(token, roots, shell_cwd, glob_budget)?;
            }
        }
        if value.contains('*') {
            // The seam is option-table-blind: it cannot tell a quoted value
            // the shell passes literally (`grep -e 'pat*'`) from a bare
            // word the shell expands, so a `*`-bearing value is walked as a
            // glob even when quoting suppressed the expansion. That
            // over-approximation is the cost of judging without the
            // option tables' context; it fails safe (an incidental match
            // outside the workspace refuses a command the shell would have
            // run inertly) and shares the one command glob budget.
            enforce_readonly_glob_operand(value, roots, effective_cwd, glob_budget)?;
            if let Some(shell_cwd) = shell_expansion_cwd.filter(|base| *base != effective_cwd) {
                enforce_readonly_glob_operand(value, roots, shell_cwd, glob_budget)?;
            }
            continue;
        }

        let joined = effective_cwd.join(candidate);
        if joined.exists() {
            let resolved = joined.canonicalize().map_err(|error| {
                ToolError::permission_denied(format!(
                    "[shell.readonly.operand.unresolved] Could not prove read-only operand {value:?} stays in the workspace: {error}"
                ))
            })?;
            if !roots.iter().any(|root| resolved.starts_with(root)) {
                return Err(ToolError::permission_denied(format!(
                    "[shell.readonly.operand.outside_workspace] Read-only shell operand {value:?} resolves outside the workspace. Use the bounded File read/search actions for project evidence."
                )));
            }
        }
    }
    Ok(())
}

/// Containment for a relative operand that carries a glob. The classifier's
/// charset admits `*` as the only wildcard (`?`, `[`, and `{` are refused
/// outright), and the shell expands it against the live working directory —
/// including entries that are symlinks to files outside the workspace, which
/// the literal check above never sees because the spelling `*` exists on no
/// disk. Walk the pattern the way the shell does (component by component,
/// `*` matching within one name, hidden entries only when the component asks
/// for them, symlinked directories followed) and judge every match by its
/// resolved location the moment it is produced. No match is inert: the shell
/// either errors (zsh `nomatch`) or hands the literal spelling to a program
/// that finds no file.
///
/// sh/dash expand a dot-glob (`.*`) to include `.` and `..` (modern bash and
/// zsh do not), and `read_dir` never yields them, so those two candidates are
/// synthesized at every level a dot-component matches and judged like any
/// other match.
///
/// The walk is bounded because it runs in the host before any child exists to
/// time out: a symlinked directory pointing at an ancestor makes a deep
/// pattern re-enter the same directory at every level, so re-entries,
/// visited nodes, and judged matches are all capped and exceeding any of
/// them refuses (fail closed, like the rest of the gate). Re-entry dedup is
/// keyed on the directory identity plus the *remaining component count*: a
/// re-entry at the same depth is a repeated expansion of the same suffix
/// (safe to skip — within one operand's walk the count uniquely identifies
/// the suffix `components[n-count..]`), while every distinct remaining
/// depth is still explored, so no match the shell would produce goes
/// unjudged — and the
/// remaining-depth component is load-bearing, not an optimization: without
/// it, a cycle whose expansion deepens (a self-link whose match carries more
/// pattern) stops exploring while the shell keeps expanding, and a leaf past
/// the cycle goes unjudged. The node and
/// match counters live in the command-wide [`ReadonlyGlobBudget`], so a
/// command cannot buy the caps once per `*` operand; the re-entry set is per
/// operand, because its keys include that operand's remaining pattern.
///
/// Like the `cd` fold, the judged-vs-run decision carries a residual no
/// pre-spawn check can close: a workspace writer can swap a judged-inside
/// symlink for an outside one after the walk and before the child opens its
/// expansion. The child opens strictly after the walk completes, and the
/// threat model is hostile workspace content, so this is the same declared
/// race the fold documents at its own probe. A hardlink to an outside file
/// is likewise invisible to any path-based gate (the resolved location is
/// genuinely inside the workspace) and is disclosed as out of scope at
/// `docs/CONFIGURATION.md` ("does not stop a hardlink to a denied file").
///
/// Design note: a simpler contract — admit a glob only in the final path
/// component, refuse `*` anywhere else — would need about half of this
/// walker (no dedup, no node budget, no dot synthesis above the leaf). Full
/// shell parity was kept so `cat */Cargo.toml`-shaped probes stay admitted;
/// the price is the cycle handling above, which the measured 10 s blowup on
/// an ancestor-pointing symlink made mandatory rather than optional.
fn enforce_readonly_glob_operand(
    value: &str,
    roots: &[PathBuf],
    effective_cwd: &std::path::Path,
    glob_budget: &mut ReadonlyGlobBudget,
) -> Result<(), ToolError> {
    let components: Vec<&str> = value
        .split('/')
        .filter(|component| !component.is_empty() && *component != ".")
        .collect();
    // zsh expands a `**/` (or `***/`) component recursively to every depth
    // by default, while this walker — like bash without `globstar`, sh, and
    // dash — consumes one directory level per component. A recursive
    // expansion would produce matches the walker never judged, so the
    // spelling is refused outright wherever more pattern follows it; a
    // trailing `**` is a plain `*` on every supported shell and stays
    // admitted.
    if components
        .iter()
        .take(components.len().saturating_sub(1))
        .any(|component| component.len() >= 2 && component.chars().all(|ch| ch == '*'))
    {
        return Err(ToolError::permission_denied(format!(
            "[shell.readonly.operand.shape] Read-only shell globs may not recurse (`**/`); refused {value:?}. Name the directory levels explicitly, or use the bounded File search actions."
        )));
    }
    // The walk recurses once per component (every `.*` level also enters the
    // synthesized `..`), so the component count bounds the stack; real probes
    // name a handful of levels.
    if components.len() > MAX_READONLY_GLOB_COMPONENTS {
        return Err(ToolError::permission_denied(format!(
            "[shell.readonly.operand.glob_too_broad] Read-only shell glob {value:?} names more than {MAX_READONLY_GLOB_COMPONENTS} path components; name fewer levels or use the bounded File search actions."
        )));
    }
    #[cfg_attr(not(unix), allow(unused_mut))]
    let mut walk = ReadonlyGlobWalk {
        roots,
        operand: value,
        budget: glob_budget,
        #[cfg(unix)]
        entered: std::collections::HashSet::new(),
        #[cfg(not(unix))]
        entered_paths: std::collections::HashSet::new(),
    };
    collect_readonly_glob_matches(effective_cwd, &components, &mut walk)
}

/// Upper bound on `/`-separated components in one glob operand, see
/// [`enforce_readonly_glob_operand`].
const MAX_READONLY_GLOB_COMPONENTS: usize = 32;

/// Hard bounds for the glob expansion of one whole command, shared by every
/// `*` operand in every segment and stage. 4096 matches covers any real
/// reconnaissance probe; 50 000 visited nodes bounds patterns that descend
/// widely but match narrowly (a deep `*/*/…/zz` over a symlink cycle would
/// otherwise explore exponentially without producing a single match); 500 000
/// examined directory entries bounds the work that produces neither — a
/// pattern that matches nothing enters no directory and judges no match, yet
/// still reads every entry of the directory it scans, so without this third
/// counter 8000 `zz*` operands (a 32 KB command) over a 2000-entry directory
/// read 16 million entries for free. Shared rather than per operand because
/// the walk is host work done before any child exists to time out: with a
/// fresh budget per operand, a ~1 KB command of 500 `*` tokens over a
/// 2000-entry directory bought ~25 s of admitted host CPU.
const MAX_READONLY_GLOB_MATCHES: usize = 4096;
const MAX_READONLY_GLOB_NODES: usize = 50_000;
const MAX_READONLY_GLOB_ENTRIES: usize = 500_000;

/// Wall-clock bound on one command's whole glob expansion. The match, node,
/// and entry caps bound the walk's work but not its wall time: on a cold
/// cache or a network-backed workspace, scanning up to a cap can take orders
/// of magnitude longer than the same walk locally, and no child process
/// exists yet for a watchdog to end.
const READONLY_GLOB_WALK_DEADLINE: Duration = Duration::from_secs(10);

/// Match, node, and examined-entry counters for one command's glob
/// expansion, see [`MAX_READONLY_GLOB_MATCHES`].
struct ReadonlyGlobBudget {
    matches: usize,
    nodes: usize,
    entries: usize,
    deadline: Instant,
    /// Total bytes of the path-text dedup keys on hosts without a portable
    /// directory identity, where each entered directory stores its full path
    /// (`(dev, ino)` keys on unix are fixed-size and need no bound).
    #[cfg(not(unix))]
    entered_key_bytes: usize,
}

impl Default for ReadonlyGlobBudget {
    fn default() -> Self {
        Self {
            matches: 0,
            nodes: 0,
            entries: 0,
            deadline: Instant::now() + READONLY_GLOB_WALK_DEADLINE,
            #[cfg(not(unix))]
            entered_key_bytes: 0,
        }
    }
}

#[cfg(test)]
impl ReadonlyGlobBudget {
    /// A budget whose walk budget is already spent, for pinning the
    /// deadline refusal without waiting on real time.
    fn expired() -> Self {
        Self {
            matches: 0,
            nodes: 0,
            entries: 0,
            deadline: Instant::now() - Duration::from_secs(1),
            #[cfg(not(unix))]
            entered_key_bytes: 0,
        }
    }
}

struct ReadonlyGlobWalk<'a> {
    roots: &'a [PathBuf],
    operand: &'a str,
    budget: &'a mut ReadonlyGlobBudget,
    /// `(device, inode, remaining-pattern-length)` of every directory entered,
    /// so a symlink cycle re-enters each `(directory, suffix)` pair once.
    #[cfg(unix)]
    entered: std::collections::HashSet<(u64, u64, usize)>,
    /// Path-text stand-in for the unix identity key (no portable dev/ino).
    #[cfg(not(unix))]
    entered_paths: std::collections::HashSet<(String, usize)>,
}

impl ReadonlyGlobWalk<'_> {
    /// The caps bound the walk's work; this bounds its wall time *between*
    /// operations, so a slow or network-backed filesystem cannot stretch one
    /// expansion past any watchdog (no child exists yet to time out). One
    /// blocking filesystem call itself can still exceed the deadline; the
    /// walk runs on the blocking pool, so the executor is not stalled.
    fn within_deadline(&self) -> Result<(), ToolError> {
        if Instant::now() >= self.budget.deadline {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.glob_deadline] Read-only glob operand {operand:?} outlasted this command's {READONLY_GLOB_WALK_DEADLINE:?} expansion budget — the pattern scans more of the workspace than the caps alone reveal on this filesystem. Narrow the pattern or use fewer glob operands.",
                operand = self.operand
            )));
        }
        Ok(())
    }

    /// Judge one final match by its resolved location, counting it against
    /// the match cap. Every branch returns on the first failure, so the walk
    /// stops at the first match that cannot be proven inside the workspace.
    fn judge(&mut self, matched: &std::path::Path) -> Result<(), ToolError> {
        self.within_deadline()?;
        self.budget.matches += 1;
        if self.budget.matches > MAX_READONLY_GLOB_MATCHES {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.glob_too_broad] Read-only glob operand {operand:?} takes this command's glob expansion past {MAX_READONLY_GLOB_MATCHES} matches in total; narrow the pattern or use fewer glob operands. Use the bounded File read/search actions for project evidence.",
                operand = self.operand
            )));
        }
        // The shell expands `*` inside the already-hardened command text, so
        // an entry whose name reads as a flag lands in an option slot the
        // gate never judged: `git diff --no-ext-diff --no-textconv *` runs
        // with a planted `--ext-diff` file re-arming the external diff the
        // splice removed (git's last-flag-wins; the attribute driver behind
        // `diff.<name>.command` is exactly what the env pins do not cover),
        // and `--show-signature` re-arms `gpg.program` the same way. gh
        // stages never reach this walker (their globs refuse outright), so
        // the wall is unconditional: a file legitimately named like a flag
        // is readable through the bounded File actions.
        if matched
            .file_name()
            .is_some_and(|name| name.to_string_lossy().starts_with('-'))
        {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.glob_dash_named] Read-only glob operand {operand:?} expands to {matched:?}, whose name reads as an option to the program, so the shell's expansion could re-arm a wall the gate removed; refused. Use the bounded File read/search actions for project evidence.",
                operand = self.operand,
                matched = matched.display(),
            )));
        }
        let resolved = matched.canonicalize().map_err(|error| {
            ToolError::permission_denied(format!(
                "[shell.readonly.operand.unresolved] Could not prove glob operand {operand:?} match {matched:?} stays in the workspace: {error}",
                operand = self.operand
            ))
        })?;
        if !self.roots.iter().any(|root| resolved.starts_with(root)) {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.outside_workspace] Read-only shell glob operand {operand:?} expands to {matched:?}, which resolves outside the workspace. Use the bounded File read/search actions for project evidence.",
                operand = self.operand
            )));
        }
        Ok(())
    }

    /// Count one directory descent against the node budget before recursing,
    /// deduplicated by directory identity and remaining pattern length: `Ok`
    /// carries `true` when this `(directory, suffix)` pair is new and the
    /// recursion should proceed. The identity is the *followed* metadata (the
    /// directory a symlink lands on), so sibling self-links collapse onto the
    /// same keys. Non-unix hosts have no portable `(dev, ino)`; the path text
    /// stands in (junction cycles are then bounded by the node budget alone).
    fn enter(&mut self, dir: &std::path::Path, remaining: usize) -> Result<bool, ToolError> {
        self.within_deadline()?;
        self.budget.nodes += 1;
        if self.budget.nodes > MAX_READONLY_GLOB_NODES {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.glob_too_broad] Read-only glob operand {operand:?} takes this command's glob expansion past {MAX_READONLY_GLOB_NODES} directories without resolving; narrow the pattern or use fewer glob operands. Use the bounded File read/search actions for project evidence.",
                operand = self.operand
            )));
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt;
            if let Ok(metadata) = dir.metadata() {
                let key = (metadata.dev(), metadata.ino(), remaining);
                return Ok(self.entered.insert(key));
            }
            // Unresolvable directory: proceed; the walk's own read_dir misses.
            Ok(true)
        }
        #[cfg(not(unix))]
        {
            let key = dir.to_string_lossy().into_owned();
            self.budget.entered_key_bytes = self.budget.entered_key_bytes.saturating_add(key.len());
            if self.budget.entered_key_bytes > MAX_READONLY_GLOB_ENTERED_KEY_BYTES {
                return Err(ToolError::permission_denied(format!(
                    "[shell.readonly.operand.glob_too_broad] Read-only glob operand {operand:?} takes this command's glob expansion past {MAX_READONLY_GLOB_ENTERED_KEY_BYTES} bytes of directory-identity keys; narrow the pattern or use fewer glob operands. Use the bounded File read/search actions for project evidence.",
                    operand = self.operand
                )));
            }
            Ok(self.entered_paths.insert((key, remaining)))
        }
    }

    /// Count one directory entry examined against the entry budget. This is
    /// the counter that bounds a pattern matching nothing: it neither enters
    /// a directory nor judges a match, yet scans every entry it is compared
    /// against.
    fn examine(&mut self) -> Result<(), ToolError> {
        self.within_deadline()?;
        self.budget.entries += 1;
        if self.budget.entries > MAX_READONLY_GLOB_ENTRIES {
            return Err(ToolError::permission_denied(format!(
                "[shell.readonly.operand.glob_too_broad] Read-only glob operand {operand:?} takes this command's glob expansion past {MAX_READONLY_GLOB_ENTRIES} examined directory entries; narrow the pattern or use fewer glob operands. Use the bounded File read/search actions for project evidence.",
                operand = self.operand
            )));
        }
        Ok(())
    }
}

fn collect_readonly_glob_matches(
    base: &std::path::Path,
    components: &[&str],
    walk: &mut ReadonlyGlobWalk<'_>,
) -> Result<(), ToolError> {
    let Some((component, rest)) = components.split_first() else {
        walk.judge(base)?;
        return Ok(());
    };
    if !component.contains('*') {
        let next = base.join(component);
        if rest.is_empty() {
            // `symlink_metadata` so a dangling symlink still counts as a
            // match the shell would hand to the program.
            if next.symlink_metadata().is_ok() {
                walk.judge(&next)?;
            }
        } else if next.is_dir() && walk.enter(&next, rest.len())? {
            collect_readonly_glob_matches(&next, rest, walk)?;
        }
        return Ok(());
    }
    // sh/dash expand a dot-glob to `.` and `..` (modern bash and zsh do
    // not), and `read_dir` never yields them, so synthesize the two
    // candidates the laxest admitted shell would produce and judge them
    // exactly like a directory entry. Both candidates are TEXTUAL paths
    // under `base`: `base.join("..")`, never `base.parent()`. The kernel
    // resolves the shell's expanded `..` physically — through a followed
    // directory symlink, `sub/..` lands on the target's parent, not on
    // `sub`'s lexical parent (probe-pinned: dash's existence check for
    // `sub/..*/x` opens `sub/../x` and reads the file) — and `judge` and
    // `enter` resolve a textual spelling through the same canonicalize the
    // kernel applies, so the lexical parent would judge a path the shell
    // never resolves while the physical one crosses outside unjudged.
    if component.starts_with('.') {
        for (name, candidate) in [
            (".", Some(base.to_path_buf())),
            ("..", Some(base.join(".."))),
        ] {
            let Some(candidate) = candidate else {
                continue;
            };
            if !readonly_star_match(component, name) {
                continue;
            }
            if rest.is_empty() {
                walk.judge(&candidate)?;
            } else if candidate.is_dir() && walk.enter(&candidate, rest.len())? {
                collect_readonly_glob_matches(&candidate, rest, walk)?;
            }
        }
    }
    let Ok(entries) = std::fs::read_dir(base) else {
        return Ok(());
    };
    for entry in entries.flatten() {
        walk.examine()?;
        let name = entry.file_name();
        // A name that is not UTF-8 cannot be matched textually; count it as a
        // match so its location is still judged (fail closed).
        let matched = name.to_str().is_none_or(|name| {
            (!name.starts_with('.') || component.starts_with('.'))
                && readonly_star_match(component, name)
        });
        if !matched {
            continue;
        }
        let next = base.join(&name);
        if rest.is_empty() {
            // Every matching entry is judged, even where a trailing-slash
            // pattern (`x*/`) means the shells keep only directories: a
            // matching outside-symlink FILE then refuses a command the
            // shells would run harmlessly — the safe over-approximation,
            // like the dangling-match one below the fold.
            walk.judge(&next)?;
        } else if next.is_dir() && walk.enter(&next, rest.len())? {
            collect_readonly_glob_matches(&next, rest, walk)?;
        }
    }
    Ok(())
}

/// Match one path component against a pattern whose only wildcard is `*`
/// (any run of characters, including none).
fn readonly_star_match(pattern: &str, name: &str) -> bool {
    let mut pieces = pattern.split('*');
    let first = pieces.next().unwrap_or("");
    let Some(mut rest) = name.strip_prefix(first) else {
        return false;
    };
    let pieces: Vec<&str> = pieces.collect();
    let Some((last, middle)) = pieces.split_last() else {
        return rest.is_empty();
    };
    for piece in middle {
        match rest.find(piece) {
            Some(offset) => rest = &rest[offset + piece.len()..],
            None => return false,
        }
    }
    rest.ends_with(last)
}

fn readonly_sanitized_path_from(
    roots: &[PathBuf],
    path: &std::ffi::OsStr,
) -> Option<std::ffi::OsString> {
    let roots = canonical_readonly_roots(roots).ok()?;
    let safe = std::env::split_paths(path).filter_map(|entry| {
        if !entry.is_absolute() {
            return None;
        }
        let resolved = entry.canonicalize().ok()?;
        (!roots.iter().any(|root| resolved.starts_with(root))).then_some(resolved)
    });
    std::env::join_paths(safe).ok()
}

fn readonly_sanitized_path(roots: &[PathBuf]) -> Option<String> {
    let path = std::env::var_os("PATH")?;
    readonly_sanitized_path_from(roots, &path).map(|value| value.to_string_lossy().into_owned())
}

fn resolve_readonly_program(program: &str, roots: &[PathBuf]) -> Result<PathBuf> {
    let path = std::env::var_os("PATH")
        .ok_or_else(|| anyhow!("no executable search path is configured"))?;
    resolve_readonly_program_from_path(program, roots, &path)
}

fn resolve_readonly_program_from_path(
    program: &str,
    roots: &[PathBuf],
    path: &std::ffi::OsStr,
) -> Result<PathBuf> {
    let roots = canonical_readonly_roots(roots)?;
    if std::path::Path::new(program).components().count() != 1 {
        return Err(anyhow!(
            "read-only command must name a bare allowlisted executable"
        ));
    }
    let safe_path = readonly_sanitized_path_from(&roots, path).ok_or_else(|| {
        anyhow!("no trusted executable search path remains outside the workspace")
    })?;
    let names = if cfg!(windows) {
        vec![format!("{program}.exe"), format!("{program}.com")]
    } else {
        vec![program.to_string()]
    };
    for directory in std::env::split_paths(&safe_path) {
        for name in &names {
            let candidate = directory.join(name);
            if !candidate.is_file() {
                continue;
            }
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt as _;
                if candidate.metadata()?.permissions().mode() & 0o111 == 0 {
                    continue;
                }
            }
            let resolved = candidate.canonicalize()?;
            if resolved.is_absolute() && !roots.iter().any(|root| resolved.starts_with(root)) {
                return Ok(resolved);
            }
        }
    }
    Err(anyhow!(
        "allowlisted read-only executable {program:?} was not found at a canonical path outside the workspace"
    ))
}

fn remove_readonly_redirect_env(cmd: &mut Command, env: &HashMap<String, String>) {
    if env.get(READONLY_ENV_MARKER).map(String::as_str) != Some("1") {
        return;
    }
    cmd.env_remove(READONLY_ENV_MARKER);
    let removals = cmd
        .get_envs()
        .filter_map(|(key, _)| {
            let upper = key.to_string_lossy().to_ascii_uppercase();
            let guarded = upper.starts_with("GIT_")
                || upper.starts_with("GH_")
                || upper.starts_with("GITHUB_");
            let safe = matches!(
                upper.as_str(),
                "GIT_OPTIONAL_LOCKS"
                    | "GIT_NO_LAZY_FETCH"
                    | "GIT_PAGER"
                    | "GIT_CONFIG_NOSYSTEM"
                    | "GIT_CONFIG_GLOBAL"
                    | "GIT_CONFIG_PARAMETERS"
                    | "GIT_EXTERNAL_DIFF"
                    | "GIT_ATTR_NOSYSTEM"
                    | "GIT_CONFIG_COUNT"
                    | "GH_PAGER"
                    | "GH_PROMPT_DISABLED"
                    | "GH_NO_UPDATE_NOTIFIER"
                    | "GH_HOST"
                    | "GH_REPO"
            ) || upper.starts_with("GIT_CONFIG_KEY_")
                || upper.starts_with("GIT_CONFIG_VALUE_");
            (guarded && !safe).then(|| key.to_os_string())
        })
        .collect::<Vec<_>>();
    for key in removals {
        cmd.env_remove(key);
    }
}

fn exec_shell_input_starts_detached(input: &serde_json::Value) -> bool {
    input
        .get("command")
        .and_then(serde_json::Value::as_str)
        .is_some()
        && input
            .get("interactive")
            .and_then(serde_json::Value::as_bool)
            != Some(true)
        && (input.get("background").and_then(serde_json::Value::as_bool) == Some(true)
            || input.get("tty").and_then(serde_json::Value::as_bool) == Some(true))
}

fn persistent_services_enabled_for(context: &ToolContext) -> bool {
    #[cfg(unix)]
    {
        context.persist_services_enabled
            && context.owner_agent_id.is_none()
            && context.tool_authority.is_none()
            && context.sandbox_backend.is_none()
            && matches!(context.shell_policy, ShellPolicy::Full)
            && matches!(
                context.elevated_sandbox_policy,
                Some(ExecutionSandboxPolicy::DangerFullAccess)
            )
    }
    #[cfg(not(unix))]
    {
        let _ = context;
        false
    }
}

#[allow(clippy::too_many_arguments)]
async fn execute_foreground_via_background(
    context: &ToolContext,
    command: &str,
    heavy_permit: Option<HeavyCommandPermit>,
    working_dir: Option<String>,
    timeout_ms: Option<u64>,
    stdin_data: Option<&str>,
    tty: bool,
    policy_override: Option<ExecutionSandboxPolicy>,
    extra_env: HashMap<String, String>,
    direct_argv: bool,
    timeout_bounds_ms: (u64, u64),
) -> Result<ShellResult> {
    let timeout_ms =
        timeout_ms.map(|timeout| timeout.clamp(timeout_bounds_ms.0, timeout_bounds_ms.1));
    let spawn_timeout_ms = timeout_ms.unwrap_or(timeout_bounds_ms.1);
    // The read-only launch boundary is the full declared root set (primary
    // first), so program resolution and PATH sanitization distrust every
    // agent-writable root, not just the primary.
    let readonly_roots = direct_argv.then(|| {
        codewhale_core::normalize_workspace_roots(&context.workspace, &context.workspace_roots)
    });
    let spawned = {
        let mut manager = context
            .shell_manager
            .lock()
            .map_err(|_| anyhow!("shell manager lock poisoned"))?;
        manager.clear_foreground_background_request();
        let owner = shell_job_owner_from_context(context);
        let lifecycle = shell_work_lifecycle_from_context(context);
        manager.execute_with_options_env_for_owner_and_work(
            command,
            working_dir.as_deref(),
            spawn_timeout_ms,
            true,
            stdin_data,
            tty,
            policy_override,
            extra_env,
            owner,
            context.state_namespace.clone(),
            context.origin_tool_call_id.clone(),
            context.origin_turn_id.clone(),
            lifecycle,
            readonly_roots.as_deref(),
            false,
            timeout_bounds_ms,
        )?
    };
    let task_id = spawned
        .task_id
        .ok_or_else(|| anyhow!("foreground shell did not return a process id"))?;
    // The stdin write end must close BEFORE any fallible step between spawn
    // and close: a future step that can fail here (the heavy-permit attach
    // below) would otherwise return early and leak an open write end,
    // wedging stdin-reading commands until the foreground watchdog killed
    // them. The task-id extraction above is the one step between spawn and
    // close, and `spawn_background_sandboxed` always returns an id on `Ok`,
    // so the close reaches the shell layer without a fallible step in
    // between (the manager-level `close_stdin_pipe` wrapper treats an
    // already-evicted record as success — eviction drops the stdin
    // handle with the record, so the invariant that matters holds: no
    // later failure can leave the pipe open).
    //
    // Foreground has no operator at the keyboard, so whatever the command
    // will ever read from stdin must be bounded now. With stdin data the
    // payload was delivered at spawn and closing the write end turns the
    // pipe into a normal end-of-file; with no data, closing immediately
    // delivers that EOF at once. Leaving the write end open wedged any
    // stdin-reading command (`cat`, a bare `$SHELL`) on a pipe that never
    // EOFs until the foreground watchdog killed it. This is the foreground
    // stdin contract the tool description states: a command detached to the
    // background later (`request_foreground_background`) keeps running, but
    // its stdin stays at EOF — `interact` reports the closed stdin and points
    // at background=true, which is the lane for input-driven work. The close
    // is deliberately pipe-only: a terminal (`combined_output`) foreground
    // command's PTY writer must stay attached, because dropping it would
    // destroy the interact surface a session member can still write to
    // while the terminal command runs (the child sees no difference — the
    // reader holds the master — and the terminal lane's blocking is bounded
    // by the foreground watchdog, not by a never-EOFing pipe). The
    // manager-side close is idempotent-safe after natural completion and on
    // lanes where the handle is absent (persistent).
    {
        let mut manager = context
            .shell_manager
            .lock()
            .map_err(|_| anyhow!("shell manager lock poisoned"))?;
        manager.close_stdin_pipe(&task_id);
    }
    if let Some(permit) = heavy_permit {
        let mut manager = context
            .shell_manager
            .lock()
            .map_err(|_| anyhow!("shell manager lock poisoned"))?;
        manager.attach_heavy_permit(&task_id, permit)?;
    }

    let deadline = timeout_ms.map(|timeout| Instant::now() + Duration::from_millis(timeout));
    // Adaptive poll cadence: fast commands (the common case — grep, wc, echo)
    // finish in single-digit milliseconds, and a fixed 100ms tick made every
    // foreground call pay that full quantum before completion was noticed.
    // Start fine-grained and back off to the 100ms cap for long-running work.
    let mut poll_tick_ms: u64 = FOREGROUND_POLL_INITIAL_MS;
    loop {
        if context
            .cancel_token
            .as_ref()
            .is_some_and(|token| token.is_cancelled())
        {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| anyhow!("shell manager lock poisoned"))?;
            let result = manager.kill(&task_id);
            if result.is_ok() {
                manager.acknowledge_foreground_completion(&task_id);
            }
            return result;
        }

        // Poll status only. The snapshot — and the buffer clones behind it — is
        // built once, when there is actually a result to return (#5472). Both
        // happen under one lock acquisition so the record cannot be evicted
        // between observing that it finished and reading its result.
        let finished = {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| anyhow!("shell manager lock poisoned"))?;
            if manager.take_foreground_background_request() {
                return manager.get_output(&task_id, false, 0);
            }
            if manager.poll_status(&task_id)? == ShellStatus::Running {
                None
            } else {
                let snapshot = manager.get_output(&task_id, false, 0)?;
                // Ordering matters: the snapshot is taken before the
                // acknowledgement releases the retained bytes.
                manager.acknowledge_foreground_completion(&task_id);
                Some(snapshot)
            }
        };

        if let Some(snapshot) = finished {
            return Ok(snapshot);
        }

        if deadline.is_some_and(|deadline| Instant::now() >= deadline) {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| anyhow!("shell manager lock poisoned"))?;
            let mut result = manager.kill(&task_id)?;
            manager.acknowledge_foreground_completion(&task_id);
            result.status = ShellStatus::TimedOut;
            return Ok(result);
        }

        tokio::time::sleep(Duration::from_millis(poll_tick_ms)).await;
        poll_tick_ms = (poll_tick_ms * 2).min(FOREGROUND_POLL_MAX_MS);
    }
}

const BASH_MAX_TIMEOUT_MS: u64 = i32::MAX as u64;

/// Upper bound for one inline `stdin` payload (counted in UTF-8 bytes).
/// Every payload the tool surface accepts is delivered through the bounded
/// stdin writer (see [`STDIN_WRITE_DEADLINE`]), so a command that stops
/// reading surfaces as a deadline stall rather than a wedged executor; the
/// cap additionally keeps oversized payloads from ever reaching the
/// writer, and on Linux's default 64 KiB pipe buffer a cap-sized payload
/// completes into an empty pipe without a reader, so the common
/// payload-then-close foreground shape delivers without stalling there.
/// Platforms with smaller default pipe buffers lean on the deadline
/// instead. Anything larger is refused with a pointer at background
/// `interact` streaming instead of hanging the session.
const MAX_INLINE_STDIN_BYTES: usize = 64 * 1024;

/// Wall-clock bound on one stdin write. At the bound an `interact` write is
/// abandoned to its background worker (the bytes still land if the child
/// resumes reading), the task's stdin is marked stalled, and the manager
/// lock is released — so each stalled write costs one bounded wait under
/// the lock, and n stalled tasks serialize into n such waits. A stalled
/// SPAWN payload is different: no task id exists yet, so the spawn aborts
/// and the child is killed instead of running task-less. The abandoned
/// worker is SIGPIPE-blocked (see [`BlockedSigpipeGuard`]): when the task
/// is later killed — the documented stall recovery — the worker observes
/// EPIPE instead of taking the process down.
const STDIN_WRITE_DEADLINE: std::time::Duration = std::time::Duration::from_secs(5);

/// One shared copy per stdin refusal shape so the unix fd-clone path and
/// the owned-writer path cannot drift apart — the pins assert substrings
/// of these. They are functions rather than consts so the deadline
/// interpolation stays a real format capture.
fn stdin_stalled_write_refusal(task_id: &str) -> String {
    format!(
        "the write to task {task_id} did not complete within {STDIN_WRITE_DEADLINE:?}: the command stopped consuming its stdin, so its input buffer filled and the rest of the payload was abandoned to a background writer (it still lands if the command resumes reading). Further stdin writes to this task are refused; kill it and start a new one."
    )
}

fn stdin_writer_failed_refusal(task_id: &str) -> String {
    format!(
        "the stdin writer for task {task_id} failed: the background write ended without completing or reporting (the writer task panicked); the payload's delivery is unknown. Kill the task and start a new one."
    )
}

/// Serves only the unix PTY close arm (the Windows/ohos close arm has its
/// own inline copy and never takes the unix delivery path). Same
/// predicate as the `PtyFile` variant, so `deny(warnings)`' fatal
/// `dead_code` cannot fire on platforms where the arm is compiled out.
#[cfg(all(unix, not(target_env = "ohos")))]
fn stdin_stalled_close_refusal(task_id: &str) -> String {
    format!(
        "the stdin close for task {task_id} did not complete within {STDIN_WRITE_DEADLINE:?}: the terminal's input queue stayed full because the command stopped consuming input, so the end-of-file bytes were not delivered; the close was not applied and the interact surface stays attached. Further stdin writes to this task are refused; kill it and start a new one."
    )
}

/// See [`stdin_stalled_close_refusal`]: unix-PTY-only, same cfg predicate.
#[cfg(all(unix, not(target_env = "ohos")))]
fn stdin_stalled_close_not_applied_refusal(task_id: &str) -> String {
    format!(
        "the stdin close for task {task_id} was not applied: its last stdin write stalled because the command stopped consuming input, so the end-of-file was never delivered and no close can deliver it now; the interact surface stays attached and further stdin writes are refused; kill it and start a new one."
    )
}

/// Initial cadence for foreground-via-background completion polling. Fast
/// commands dominate real agent traffic; detection latency on `true`-class
/// commands drops from ~100ms to ~10ms, while long commands reach the
/// 100ms cap within one doubling step.
const FOREGROUND_POLL_INITIAL_MS: u64 = 10;
/// Poll-cadence ceiling; matches the previous fixed tick so long-running
/// command overhead is unchanged.
const FOREGROUND_POLL_MAX_MS: u64 = 100;

/// Default foreground lifetime for a contract-`bash` `action=run` that names
/// no `timeout_ms`. Matches the value the tool's own input schema advertises;
/// before this existed the omitted case fell through to
/// `BASH_MAX_TIMEOUT_MS`.
const CONTRACT_BASH_FOREGROUND_DEFAULT_TIMEOUT_MS: u64 = 120_000;

/// Resolve the lifetime for one `bash` run.
///
/// A foreground contract-`bash` run that names no timeout used to inherit
/// `BASH_MAX_TIMEOUT_MS` (~24.8 days), so a command that blocked on an
/// interactive prompt or a hung network call pinned the turn indefinitely —
/// the tool row just counted seconds while the model waited. The tool's own
/// schema already promises `action=run 120000`, and its description already
/// says foreground is for bounded commands, so honor that: an omitted
/// timeout takes the advertised default, which lets
/// `FOREGROUND_TIMEOUT_RECOVERY_HINT` kill the process and tell the model to
/// rerun with `background=true`.
///
/// An explicit `timeout_ms` is still honored up to the full contract ceiling,
/// and background and interactive runs keep their own lifetimes: their
/// processes are meant to outlive the call, so bounding them here would kill
/// long-lived jobs the model deliberately detached.
fn contract_bash_timeout_ms(
    optional_timeout: bool,
    requested_ms: Option<u64>,
    background: bool,
    interactive: bool,
) -> Option<u64> {
    if optional_timeout && requested_ms.is_none() && !background && !interactive {
        return Some(CONTRACT_BASH_FOREGROUND_DEFAULT_TIMEOUT_MS);
    }
    requested_ms
}

fn contract_bash_error_status(result: &ShellResult, timeout_ms: Option<u64>) -> String {
    match result.status {
        ShellStatus::TimedOut => {
            let millis = timeout_ms.unwrap_or(BASH_MAX_TIMEOUT_MS);
            let seconds = if millis.is_multiple_of(1_000) {
                (millis / 1_000).to_string()
            } else {
                format!("{}", millis as f64 / 1_000.0)
            };
            format!("Command timed out after {seconds} seconds")
        }
        ShellStatus::Killed => "Command aborted".to_string(),
        ShellStatus::Failed | ShellStatus::Completed | ShellStatus::Running => format!(
            "Command exited with code {}",
            result.exit_code.unwrap_or(-1)
        ),
    }
}

fn finish_contract_bash_result(
    result: ShellResult,
    timeout_ms: Option<u64>,
    context: &ToolContext,
) -> Result<ToolResult, ToolError> {
    let sandbox_denied_hint = shell_sandbox_denied_hint(context, &result);
    let mut output = result.stdout.clone();
    output.push_str(&result.stderr);
    if let Some(hint) = sandbox_denied_hint {
        output = if output.is_empty() {
            hint
        } else {
            format!("{hint}\n\n{output}")
        };
    }
    let metadata = json!({
        "evidence_routing": "inline", "exit_code": result.exit_code,
        "status": format!("{:?}", result.status), "duration_ms": result.duration_ms,
        "sandboxed": result.sandboxed, "sandbox_type": result.sandbox_type,
        "task_id": result.task_id, "backgrounded": result.status == ShellStatus::Running,
    });
    if result.status == ShellStatus::Running {
        let task_id = result.task_id.as_deref().unwrap_or("unknown");
        let partial = (!output.is_empty()).then(|| format!("\n\nOutput so far:\n{output}"));
        return Ok(ToolResult::success(format!(
            "Foreground shell wait moved to /jobs: {task_id}{}\n\nThe command is still running; completion will appear as a runtime event.",
            partial.as_deref().unwrap_or_default()
        )).with_metadata(metadata));
    }
    if result.status != ShellStatus::Completed {
        let status = contract_bash_error_status(&result, timeout_ms);
        return Err(ToolError::execution_failed(if output.is_empty() {
            status
        } else {
            format!("{output}\n\n{status}")
        }));
    }

    Ok(ToolResult::success(if output.is_empty() {
        "(no output)".to_string()
    } else {
        output
    })
    .with_metadata(metadata))
}

/// Small foreground-only shell surface shown to new model turns.
pub struct LowercaseBashTool;

#[async_trait]
impl ToolSpec for LowercaseBashTool {
    fn name(&self) -> &'static str {
        "bash"
    }

    fn description(&self) -> &'static str {
        "Execute a shell command in the workspace and return stdout and stderr. The command runs in the detected platform shell — `$SHELL` on Unix (any shell it names, falling back to `/bin/sh`); on Windows, bash when the session provides one, otherwise PowerShell (pwsh 7 or Windows PowerShell 5.1) or cmd.exe. Windows PowerShell 5.1 lacks `&&`; prefer forms the detected shell supports. In read-only shell sessions a single approved command without a `*` runs directly as argv instead — pipelines (`a | b`), `;`/`&&`/`||` sequences, commands with a `cd` segment, and single commands carrying a `*` glob run through the detected shell. This foreground surface runs without a terminal stdin: interactive programs and a bare `$SHELL` receive end-of-file and exit. With an external sandbox backend configured this tool is unavailable (use the backend-specific shell tool instead). Output keeps the last 2000 lines or 50KB. An optional timeout is expressed in seconds; when omitted the command is killed after 120 seconds, so pass an explicit timeout for work expected to take longer. In Ask, after a sandbox denial, retry the exact command once with sandbox_permissions (the narrowest wider mode that suffices) and a one-sentence justification; the approval prompt asks the user."
    }

    fn input_schema(&self) -> serde_json::Value {
        json!({
            "type": "object",
            "properties": {
                "command": { "type": "string", "description": "Shell command to execute." },
                "timeout": { "type": "number", "description": "Optional timeout in seconds; when omitted the command is killed after 120 seconds." },
                "sandbox_permissions": {
                    "type": "string",
                    "enum": ["workspace-write", "danger-full-access"],
                    "description": "The wider sandbox mode this exact command needs. Use only as a one-shot retry after a sandbox denial; requires justification and user approval in Ask."
                },
                "justification": {
                    "type": "string",
                    "description": "Required with sandbox_permissions: one sentence explaining why this exact command needs wider access."
                }
            },
            "required": ["command"],
            "additionalProperties": false
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        BashTool::contract_delegate().capabilities()
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Required
    }

    fn approval_requirement_for(&self, input: &serde_json::Value) -> ApprovalRequirement {
        let translated = contract_bash_legacy_input(input).unwrap_or_else(|_| input.clone());
        BashTool::contract_delegate().approval_requirement_for(&translated)
    }

    fn is_read_only_for(&self, input: &serde_json::Value) -> bool {
        contract_bash_legacy_input(input)
            .is_ok_and(|translated| BashTool::contract_delegate().is_read_only_for(&translated))
    }

    fn supports_parallel_for(&self, input: &serde_json::Value) -> bool {
        self.is_read_only_for(input)
    }

    async fn execute(
        &self,
        input: serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        let translated = contract_bash_legacy_input(&input)?;
        BashTool::contract_delegate()
            .execute(translated, context)
            .await
    }
}

fn contract_bash_legacy_input(input: &serde_json::Value) -> Result<serde_json::Value, ToolError> {
    let object = input
        .as_object()
        .ok_or_else(|| ToolError::invalid_input("bash input must be an object"))?;
    let unexpected = object
        .keys()
        .filter(|key| {
            !matches!(
                key.as_str(),
                "command" | "timeout" | "sandbox_permissions" | "justification"
            )
        })
        .cloned()
        .collect::<Vec<_>>();
    if !unexpected.is_empty() {
        return Err(ToolError::invalid_input(format!(
            "unexpected bash parameter(s): {}",
            unexpected.join(", ")
        )));
    }
    let command = required_str(input, "command")?;
    let mut translated = json!({"command": command});
    if let Some(timeout) = input.get("timeout") {
        let seconds = timeout.as_f64().ok_or_else(|| {
            ToolError::invalid_input("Invalid timeout: expected a finite number of seconds")
        })?;
        if !seconds.is_finite() || seconds <= 0.0 {
            return Err(ToolError::invalid_input(
                "Invalid timeout: expected a positive finite number of seconds",
            ));
        }
        let millis = seconds * 1000.0;
        if millis > BASH_MAX_TIMEOUT_MS as f64 {
            return Err(ToolError::invalid_input(format!(
                "Invalid timeout: maximum is {} seconds",
                BASH_MAX_TIMEOUT_MS as f64 / 1000.0
            )));
        }
        translated["timeout_ms"] = json!((millis as u64).max(1));
    }
    for field in ["sandbox_permissions", "justification"] {
        if let Some(value) = input.get(field) {
            translated[field] = value.clone();
        }
    }
    Ok(translated)
}

/// Compatibility shell tool retained for saved v0.9.x transcripts and the
/// background/session control surface. It is hidden from new model catalogs.
pub struct BashTool {
    name: &'static str,
    forced_action: Option<&'static str>,
    read_only: bool,
    optional_timeout: bool,
}

pub(crate) fn readonly_bash_input_schema() -> serde_json::Value {
    json!({
        "type": "object",
        "properties": {
            "action": { "type": "string", "enum": ["run"] },
            "command": { "type": "string", "description": "A classifier-approved read command" },
            "cwd": { "type": "string", "description": "Workspace-relative working directory" },
            "timeout_ms": { "type": "integer", "description": "Timeout in milliseconds (1000-600000)" }
        },
        "required": ["command"],
        "additionalProperties": false
    })
}

impl BashTool {
    pub const fn new(name: &'static str) -> Self {
        Self {
            name,
            forced_action: None,
            read_only: false,
            optional_timeout: false,
        }
    }

    pub const fn read_only(name: &'static str) -> Self {
        Self {
            name,
            forced_action: None,
            read_only: true,
            optional_timeout: false,
        }
    }

    pub const fn alias(name: &'static str, action: &'static str) -> Self {
        Self {
            name,
            forced_action: Some(action),
            read_only: false,
            optional_timeout: false,
        }
    }

    const fn contract_delegate() -> Self {
        Self {
            name: "bash",
            forced_action: Some("run"),
            read_only: false,
            optional_timeout: true,
        }
    }
}

#[async_trait]
impl ToolSpec for BashTool {
    fn name(&self) -> &'static str {
        self.name
    }

    fn model_visible(&self) -> bool {
        false
    }

    fn description(&self) -> &'static str {
        if self.read_only {
            "Inspect the workspace with the bounded read-only command subset. Single commands without a `*` run directly as argv; pipelines (`a | b`), `;`/`&&`/`||` sequences, commands with a `cd` segment, and single commands carrying a `*` glob run through the detected shell instead — the gate expands the glob and judges every match by its resolved location, and shapes it cannot mirror that way (a recursive `**/`, a glob on a `gh` stage) refuse (pipelines additionally get a `set -o pipefail` prefix where the detected binary speaks it — bash, zsh, and the ksh family — never on cmd.exe or PowerShell; every multi-segment compound and `%`/`^`/`,`/`=`-bearing command is refused on a cmd.exe host, and `,`-bearing commands on a PowerShell host; a standalone `cd` affects only that one command). Only action=run plus command, cwd, and timeout_ms are accepted."
        } else {
            guidance::description()
        }
    }

    fn input_schema(&self) -> serde_json::Value {
        if self.read_only {
            return readonly_bash_input_schema();
        }
        json!({
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["run", "wait", "interact", "cancel"],
                    "description": "Action to perform (default: run)"
                },
                "command": {
                    "type": "string",
                    "description": guidance::runtime_command_guidance()
                },
                "timeout_ms": {
                    "type": "integer",
                    "description": "Timeout in milliseconds. The default depends on the action: action=run 120000 (the standalone Bash tool caps it at 600000), action=wait 30000, action=interact 1000. A foreground action=run that omits this is bounded by that default and killed with a background-rerun hint; pass an explicit value for longer foreground work, or background=true. For action=wait, `timeout_secs` (seconds) and `timeout` (milliseconds) are accepted aliases. Background=true tasks are not bounded by it — stop them with action=cancel."
                },
                "background": {
                    "type": "boolean",
                    "description": "Temporary background; killed at session exit. Surviving headless services need background:true,persist:true. It is not killed at timeout_ms; plan to poll it with action=wait or stop it with action=cancel."
                },
                "interactive": {
                    "type": "boolean",
                    "description": "Run interactively with terminal IO (default: false)"
                },
                "stdin": {
                    "type": "string",
                    "maxLength": 65536,
                    "description": "Stdin data to send (action=run: before waiting; action=interact: to the background task). Also accepted as `input` or `data` — send only one. Inline payloads are capped at 64 KiB, counted as UTF-8 bytes (maxLength mirrors the same number): a write to a command that is not reading ends as a bounded deadline-stall refusal instead of blocking the session; for bigger payloads run the command in the background and feed it through `action=interact` in chunks."
                },
                "input": {
                    "type": "string",
                    "maxLength": 65536,
                    "description": "Alias for `stdin`."
                },
                "data": {
                    "type": "string",
                    "maxLength": 65536,
                    "description": "Alias for `stdin`."
                },
                "cwd": {
                    "type": "string",
                    "description": "Optional working directory for the command"
                },
                "tty": {
                    "type": "boolean",
                    "description": "Allocate a pseudo-terminal for interactive programs (implies background)"
                },
                "combined_output": {
                    "type": "boolean",
                    "description": "Capture stdout and stderr as one chronological PTY stream (default false)"
                },
                "task_id": {
                    "type": "string",
                    "description": "Task ID for action=wait/interact/cancel. Also accepted as `id`."
                },
                "task_ids": {
                    "type": "array",
                    "items": { "type": "string" },
                    "description": "For action=wait: wait on several background task ids at once (alternative to `task_id`)."
                },
                "until": {
                    "type": "string",
                    "enum": ["any", "all"],
                    "description": "For action=wait with `task_ids`: return as soon as any task is terminal (`any`) or when every task is terminal (`all`, default)."
                },
                "id": {
                    "type": "string",
                    "description": "Alias for `task_id`."
                },
                "wait": {
                    "type": "boolean",
                    "description": "For action=wait, block until the task completes or timeout elapses (default: true). Pass false for a nonblocking snapshot; `block` is an accepted alias."
                },
                "close_stdin": {
                    "type": "boolean",
                    "description": "Close stdin after sending (action=interact); on a terminal task the close delivers end-of-file, sent as a literal newline followed by the EOF byte — a program that put its terminal into raw mode, or disabled or remapped VEOF, receives those bytes as ordinary input instead, and on Windows the close only releases the terminal handle (a console child has no end-of-file to deliver)"
                },
                "all": {
                    "type": "boolean",
                    "description": "Cancel all running background tasks (action=cancel)"
                },
                "persist": {
                    "type": "boolean",
                    "description": "Keep this background service running after a successful headless exec (default: false). Requires background:true and explicit danger-full-access. Run the service itself in the foreground; do not use nohup or a trailing `&`."
                },
                "sandbox_permissions": {
                    "type": "string",
                    "enum": ["workspace-write", "danger-full-access"],
                    "description": "The wider sandbox mode this exact command needs. Use only as a one-shot retry after a sandbox denial; requires justification and user approval in Ask."
                },
                "justification": {
                    "type": "string",
                    "description": "Required with sandbox_permissions: one sentence explaining why this exact command needs wider access."
                }
            },
            // The schema used to declare nothing required at all, so
            // `Bash{}` was schema-valid for the tool that runs shell
            // commands. What is required is per-action and cannot be spelled
            // as a flat `required` list: `run` needs `command`,
            // `wait`/`interact`/`cancel` need `task_id` (or its `id` alias),
            // and `cancel` needs `all` instead when cancelling everything.
            // A root `anyOf` of `required` groups is how this repo already
            // spells that (`finance`, `apply_patch`), and `schema_sanitize`
            // knows the shape: providers that reject root composition get the
            // groups merged and the constraint restated as a description note
            // (`root_composition_constraint_note`). The cost is that
            // `strict_schema_supported` rejects a root `anyOf`, so `Bash`
            // opts out of DeepSeek strict mode — as `finance` already does on
            // the same default agent surface, which turns strict mode off for
            // the whole tool set regardless.
            "anyOf": [
                { "required": ["command"] },
                { "required": ["task_id"] },
                { "required": ["id"] },
                { "required": ["all"] }
            ]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![
            ToolCapability::ExecutesCode,
            ToolCapability::Sandboxable,
            ToolCapability::RequiresApproval,
        ]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Required
    }

    fn approval_requirement_for(&self, input: &serde_json::Value) -> ApprovalRequirement {
        if exec_shell_input_is_parallel_readonly(input) {
            ApprovalRequirement::Auto
        } else {
            self.approval_requirement()
        }
    }

    fn is_read_only_for(&self, input: &serde_json::Value) -> bool {
        exec_shell_input_is_parallel_readonly(input)
    }

    fn supports_parallel_for(&self, input: &serde_json::Value) -> bool {
        exec_shell_input_is_parallel_readonly(input)
    }

    fn starts_detached_for(&self, input: &serde_json::Value) -> bool {
        exec_shell_input_starts_detached(input)
    }

    async fn execute(
        &self,
        input: serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        // `and_then(as_str).unwrap_or("run")` treated *any* non-string
        // `action` as absent and fell through to the branch that runs
        // arbitrary code: `Bash{action: 3, command: "…"}` executed the
        // command. Every sibling family refuses a non-string action
        // (`canonical_action::required_action`), and `Bash` cannot be the
        // lenient one. `optional_str` is the type-strictness lane's extractor:
        // absent or `null` takes the documented `run` default, anything else
        // is a `type_mismatch` naming the field and the type it needed.
        let action = match self.forced_action {
            Some(forced) => forced,
            None => optional_str(&input, "action")?.unwrap_or("run"),
        };
        // The task actions drive pre-existing manager tasks, which the
        // read-only shape gate below never sees. A read-only shell holds no
        // task surface of its own — the shape gate refuses `background`, so
        // it can never have created one — and a task addressed here could
        // belong to another agent in the session, so the policy is checked
        // before the dispatch rather than after it.
        if action != "run" && matches!(context.shell_policy, ShellPolicy::ReadOnly) {
            return Err(ToolError::permission_denied(
                "[shell.readonly.action] A read-only shell holds no background task surface; only `run` with a bounded inspection command is available.",
            ));
        }
        match action {
            "wait" => return self.execute_wait(&input, context).await,
            "interact" => return self.execute_interact(&input, context).await,
            "cancel" => return self.execute_cancel(&input, context).await,
            "run" => {}
            // Bash was the only action wrapper whose catch-all fell through to
            // its most dangerous branch: `{"action":"kill", "command":…}` ran
            // the command instead of cancelling, and a mis-cased "Cancel" did
            // the same. Every sibling (`File`, `Git`, `Web`, `Run`) already
            // refuses an unknown action; the tool that executes arbitrary code
            // should not be the lenient one.
            other => {
                return Err(ToolError::invalid_input(format!(
                    "Unknown Bash action \"{other}\"; nothing was run. Pass one of: run, wait, interact, cancel."
                )));
            }
        }
        let command = required_str(&input, "command")?;
        match context.shell_policy {
            ShellPolicy::None => {
                return Ok(ToolResult::error(
                    "Shell tools are disabled by the active permission profile.",
                ));
            }
            ShellPolicy::ReadOnly if !exec_shell_input_agent_readonly(&input) => {
                return Ok(ToolResult::error(
                    "Shell command blocked by read-only shell policy. Use a non-mutating, non-background inspection command, or switch to Work mode (`/mode work`) for write-capable shell work.",
                ));
            }
            ShellPolicy::ReadOnly | ShellPolicy::Full => {}
        }
        enforce_readonly_github_network_policy(command, context)?;
        let requested_timeout_ms = if self.optional_timeout {
            input
                .get("timeout_ms")
                .map(|value| {
                    value
                        .as_u64()
                        .ok_or_else(|| type_mismatch("timeout_ms", value, "a positive integer"))
                })
                .transpose()?
        } else {
            Some(optional_u64(&input, "timeout_ms", 120_000)?.min(600_000))
        };
        let background = optional_bool(&input, "background", false)?;
        let interactive = optional_bool(&input, "interactive", false)?;
        let combined_output = optional_bool(&input, "combined_output", false)?;
        let tty = optional_bool(&input, "tty", false)? || (combined_output && background);
        let timeout_ms = contract_bash_timeout_ms(
            self.optional_timeout,
            requested_timeout_ms,
            background,
            interactive,
        );
        let timeout_value_ms = timeout_ms.unwrap_or(BASH_MAX_TIMEOUT_MS);
        // Strict types (2026-08-04 review): a non-string here used to be
        // silently dropped — the command then ran with NO stdin and reported
        // success, the exact silent-drop failure the alias hardening closed
        // for misspelled names. A wrong type is an error, never a no-op.
        let stdin_data = match first_present_field(&input, &["stdin", "input", "data"]) {
            None => None,
            Some((name, value)) => {
                let payload = value
                    .as_str()
                    .ok_or_else(|| type_mismatch(name, value, "a string"))?
                    .to_string();
                // The tool surface's inline cap keeps an oversized payload
                // from ever reaching the writer; the spawn delivers the
                // payload through the bounded stdin writer, so a command
                // that never reads surfaces as the writer's 5-second
                // deadline stall refusal rather than a wedged executor. On
                // Linux the cap equals the default pipe buffer, so a
                // cap-sized write into an empty pipe completes without a
                // reader there; smaller default buffers lean on the same
                // deadline on the bounded lanes. Bigger payloads belong on
                // the background lane, fed through `action=interact` in
                // chunks.
                if payload.len() > MAX_INLINE_STDIN_BYTES {
                    return Ok(ToolResult::error(format!(
                        "The inline stdin payload is {} bytes; the shell tool accepts at most {MAX_INLINE_STDIN_BYTES} inline — run the command with background=true and feed it through action=interact in chunks.",
                        payload.len()
                    )));
                }
                Some(payload)
            }
        };

        if interactive && background {
            return Ok(ToolResult::error(
                "Interactive commands cannot run in background mode.",
            ));
        }
        if interactive && (tty || combined_output) {
            return Ok(ToolResult::error(
                "Interactive mode cannot be combined with TTY or combined_output sessions.",
            ));
        }
        if interactive && stdin_data.is_some() {
            return Ok(ToolResult::error(
                "Interactive mode cannot be combined with stdin data.",
            ));
        }

        let persist = optional_bool(&input, "persist", false)?;
        if persist {
            if !background {
                return Err(ToolError::invalid_input(
                    "persist:true requires background:true; a persisted service must be started as a background task.",
                ));
            }
            if interactive || tty {
                return Err(ToolError::invalid_input(
                    "persist:true cannot be combined with interactive or TTY modes.",
                ));
            }
            if stdin_data.is_some() {
                return Err(ToolError::invalid_input(
                    "persist:true spawns the service with null stdio; stdin data is not accepted.",
                ));
            }
            if !persistent_services_enabled_for(context) {
                return Err(ToolError::not_available(
                    "persistent background services (persist:true) are only available on Unix in the real headless `codewhale exec` host under an explicit danger-full-access / full shell authority. They are rejected in interactive sessions, desktop/app-server hosts, Fleet/sub-agents, restricted or external sandboxes, and TTY/interactive/stdin modes.",
                ));
            }
        }

        let background = background || tty;

        let mut execpolicy_decision: Option<ExecPolicyDecision> = None;
        if context.features.enabled(Feature::ExecPolicy)
            && let Some(policy) = load_default_policy()
                .map_err(|e| ToolError::execution_failed(format!("execpolicy load failed: {e}")))?
        {
            let decision = policy.evaluate(command);
            execpolicy_decision = Some(decision.clone());
            if let ExecPolicyDecision::Deny(reason) = decision {
                return Ok(ToolResult {
                    content: format!("BLOCKED: {reason}"),
                    success: false,
                    metadata: Some(json!({
                        "execpolicy": {
                            "decision": "deny",
                            "reason": reason,
                        }
                    })),
                });
            }
        }

        // Safety analysis (always run for metadata, but only block when not in YOLO mode)
        let safety = analyze_command(command);
        if !context.auto_approve {
            match safety.level {
                SafetyLevel::Dangerous => {
                    let reasons = safety.reasons.join("; ");
                    let suggestions = if safety.suggestions.is_empty() {
                        String::new()
                    } else {
                        format!("\nSuggestions: {}", safety.suggestions.join("; "))
                    };
                    return Ok(ToolResult {
                        content: format!(
                            "BLOCKED: This command was blocked for safety reasons.\n\nReasons: {reasons}{suggestions}\n\nNote: allow_shell=true exposes shell tools, but it does not disable built-in shell safety validation."
                        ),
                        success: false,
                        metadata: Some(json!({
                            "safety_level": "dangerous",
                            "blocked": true,
                            "reasons": safety.reasons,
                            "suggestions": safety.suggestions,
                        })),
                    });
                }
                SafetyLevel::RequiresApproval | SafetyLevel::Safe | SafetyLevel::WorkspaceSafe => {
                    // Proceed normally
                }
            }
        }

        let policy_override = context.elevated_sandbox_policy.clone();
        // Strict types: a non-string cwd used to silently run the command in
        // the workspace default instead of erroring (2026-08-04 review).
        let working_dir = match first_present_field(&input, &["cwd", "working_dir"])
            .map(|(name, value)| {
                value
                    .as_str()
                    .ok_or_else(|| type_mismatch(name, value, "a string"))
            })
            .transpose()?
        {
            Some(dir) => {
                // Validate cwd against workspace boundary (same as file tools)
                let resolved = context.resolve_path(dir)?;
                Some(resolved.to_string_lossy().to_string())
            }
            // Default to the tool context's workspace (which reflects the
            // child agent's worktree when `worktree: true` was used), not the
            // shared ShellManager's parent-workspace default_workspace.
            None => Some(context.workspace.display().to_string()),
        };
        let read_only_shell = matches!(context.shell_policy, ShellPolicy::ReadOnly);
        // The read-only boundary is the full declared root set, not just the
        // primary: attached roots are agent-writable (untrusted for programs
        // and PATH) yet legitimate operand/cwd targets.
        let readonly_roots = read_only_shell.then(|| {
            codewhale_core::normalize_workspace_roots(&context.workspace, &context.workspace_roots)
        });
        let mut readonly_git_filter_pins = Vec::new();
        if let Some(roots) = readonly_roots.as_deref() {
            let effective_cwd = working_dir
                .as_deref()
                .map(std::path::Path::new)
                .unwrap_or(&context.workspace);
            let cwd_spelling = effective_cwd.to_path_buf();
            // The gate walks the filesystem (glob expansion) in the host
            // before any child exists to time out, so the whole pre-spawn
            // block — the canonicalizations included: a realpath walk over a
            // cold-cache or network workspace stalls an executor thread —
            // runs on the blocking pool, and its cost stays bounded by the
            // walker's match/node caps. The root set and the cwd are
            // canonicalized ONCE, here, for both consumers. The gate
            // re-canonicalizes (idempotently), but
            // discovery consumed these RAW spellings before: a declared
            // root spelled through a symlink (macOS `/tmp` →
            // `/private/tmp`, an attached root alias) made discovery's
            // `-C` root-containment compare canonicalized targets against
            // non-canonical roots and refuse every `-C`-bearing git read,
            // and a divergent symlink inside the cwd spelling sent git's
            // physical `.git` walk-up somewhere the lane's lexical one did
            // not look. Both layers must judge the same spellings.
            let command = command.to_string();
            let roots = roots.to_vec();
            readonly_git_filter_pins = tokio::task::spawn_blocking(move || {
                let roots = canonical_readonly_roots(&roots).map_err(|error| {
                    ToolError::execution_failed(format!(
                        "Could not resolve the read-only workspace before shell dispatch: {error}"
                    ))
                })?;
                let effective_cwd = cwd_spelling.canonicalize().map_err(|error| {
                    ToolError::permission_denied(format!(
                        "Could not prove the read-only shell working directory stays in the workspace: {error}"
                    ))
                })?;
                enforce_readonly_workspace_operands(&command, &roots, &effective_cwd)?;
                discover_readonly_git_filter_pins(&command, &effective_cwd, &roots)
            })
            .await
            .map_err(|error| {
                ToolError::execution_failed(format!(
                    "the read-only operand check could not be scheduled: {error}"
                ))
            })??;
        }

        // #456 — collect env from any configured `shell_env` hooks. Runs
        // synchronously, captures stdout, parses `KEY=VAL` lines, audit-logs
        // the keys (never the values). Empty / no-op when no hook is
        // configured.
        let mut extra_env = if read_only_shell {
            // shell_env hooks are arbitrary operator-configured processes.
            // They cannot run inside the evidence-only execution boundary.
            HashMap::new()
        } else if let Some(hook_executor) = &context.runtime.hook_executor {
            let hook_ctx = crate::hooks::HookContext::new()
                .with_tool_name("exec_shell")
                .with_tool_args(&input);
            hook_executor.collect_shell_env(&hook_ctx)
        } else {
            std::collections::HashMap::new()
        };
        if read_only_shell {
            let null_device = if cfg!(windows) { "NUL" } else { "/dev/null" };
            let inert_git_helper = if cfg!(windows) {
                "cmd.exe /d /c exit 1"
            } else {
                "/usr/bin/false"
            };
            // Read-only Bash is intentionally a small inspection surface. Git
            // can otherwise invoke operator/repository configured helpers
            // while performing nominal reads (a pager or fsmonitor), and it
            // may opportunistically refresh the index. These environment
            // overrides make those reads non-interactive and suppress the
            // optional mutation/extension seams; the command classifier and
            // machine authority gate remain authoritative as well.
            extra_env.insert("GIT_OPTIONAL_LOCKS".to_string(), "0".to_string());
            extra_env.insert("GIT_NO_LAZY_FETCH".to_string(), "1".to_string());
            extra_env.insert("GIT_PAGER".to_string(), String::new());
            extra_env.insert("GH_PAGER".to_string(), String::new());
            extra_env.insert("GH_PROMPT_DISABLED".to_string(), "1".to_string());
            extra_env.insert("GH_NO_UPDATE_NOTIFIER".to_string(), "1".to_string());
            // The classifier rejects explicit GHES repo/URL targets. Pin the
            // implicit environment side too, so inherited GH_HOST/GH_REPO
            // cannot redirect the call after api.github.com was approved.
            extra_env.insert("GH_HOST".to_string(), "github.com".to_string());
            extra_env.insert("GH_REPO".to_string(), String::new());
            extra_env.insert("PAGER".to_string(), String::new());
            extra_env.insert("ENV".to_string(), String::new());
            extra_env.insert("BASH_ENV".to_string(), String::new());
            // Non-interactive zsh sources `$ZDOTDIR/.zshenv`, where
            // operator-set options such as EXTENDED_GLOB could turn
            // charset-admitted characters into live globs. An EMPTY ZDOTDIR
            // makes zsh fall back to `$HOME/.zshenv` — the exact file this
            // pin exists to bypass — so the override points at `/`, a
            // directory outside agent control; bash's GLOBIGNORE could
            // likewise widen what a pattern matches. The child's live env
            // is not observable through the admitted grammar (env-reading
            // commands refuse), so the values pin at the table.
            for (key, value) in readonly_spawn_env_overrides() {
                extra_env.insert(key.to_string(), value);
            }
            // Ignore user/system Git configuration and replace any repository
            // external diff helper with a fixed inert executable. Repository
            // config and attributes are attacker-controlled evidence inputs;
            // a nominal `git diff/log/show` must not turn them into programs.
            // Note the GIT_CONFIG_COUNT mechanism needs git >= 2.31 and the
            // GIT_CONFIG_GLOBAL pin below needs 2.32: on an older git this
            // block silently does not exist (or leaves the global config
            // live). The fixed pins
            // below are written for EVERY read-only git stage, so discovery
            // version-checks the host git for every command with a git
            // stage (`ensure_git_supports_env_config_pins`); the argv splice
            // is the other wall for the subcommands it covers (status/
            // ls-files/grep are covered by nothing but this block).
            extra_env.insert("GIT_CONFIG_NOSYSTEM".to_string(), "1".to_string());
            extra_env.insert("GIT_CONFIG_GLOBAL".to_string(), null_device.to_string());
            extra_env.insert("GIT_CONFIG_PARAMETERS".to_string(), String::new());
            extra_env.insert(
                "GIT_EXTERNAL_DIFF".to_string(),
                inert_git_helper.to_string(),
            );
            extra_env.insert("GIT_ATTR_NOSYSTEM".to_string(), "1".to_string());
            if let Some(path) = readonly_sanitized_path(readonly_roots.as_deref().unwrap_or(&[])) {
                extra_env.insert("PATH".to_string(), path);
            }
            // A `.gitattributes` filter assignment executes only when the
            // repository config defines the filter, so every conversion key
            // discovered for this command's repositories is pinned to an
            // inert helper here: `cat` for the worktree-side clean/smudge
            // conversions an admitted read performs, and the inert helper
            // for the protocol (`process`) form, whose handshake then fails
            // loudly instead of running its configured command. These are
            // command-scoped git config, which outranks the repository's own
            // values. An empty `process` override does take on current git
            // (probe-pinned: it silently disarms the filter), so a `cat`-
            // style empty value would work there — the helper is the honest
            // wall because it fails loudly on every git, including builds
            // that ignore empty overrides — and only keys the repository
            // itself declared are pinned, since introducing a `process` key
            // git would otherwise not see breaks a working clean/smudge
            // filter. One precedence fact bounds all of it: command-line
            // `-c key=value` outranks these pins, and the classifier's
            // refusal of `-c`-bearing git stages is what keeps them
            // authoritative (see `discover_readonly_git_filter_pins`).
            let mut git_config_count = 3usize;
            for pin in &readonly_git_filter_pins {
                for (armed, key, value) in [
                    (pin.clean, "clean", "cat".to_string()),
                    (pin.smudge, "smudge", "cat".to_string()),
                    (pin.process, "process", inert_git_helper.to_string()),
                ] {
                    if !armed {
                        continue;
                    }
                    extra_env.insert(
                        format!("GIT_CONFIG_KEY_{git_config_count}"),
                        format!("filter.{}.{}", pin.name, key),
                    );
                    extra_env.insert(format!("GIT_CONFIG_VALUE_{git_config_count}"), value);
                    git_config_count += 1;
                }
            }
            extra_env.insert("GIT_CONFIG_COUNT".to_string(), git_config_count.to_string());
            extra_env.insert("GIT_CONFIG_KEY_0".to_string(), "core.fsmonitor".to_string());
            extra_env.insert("GIT_CONFIG_VALUE_0".to_string(), "false".to_string());
            extra_env.insert("GIT_CONFIG_KEY_1".to_string(), "core.hooksPath".to_string());
            extra_env.insert("GIT_CONFIG_VALUE_1".to_string(), null_device.to_string());
            extra_env.insert(
                "GIT_CONFIG_KEY_2".to_string(),
                "log.showSignature".to_string(),
            );
            extra_env.insert("GIT_CONFIG_VALUE_2".to_string(), "false".to_string());
            extra_env.insert(READONLY_ENV_MARKER.to_string(), "1".to_string());
        }

        let command_expense = infer_command_expense(command);
        let heavy_permit = acquire_heavy_command_permit(command, context.cancel_token.as_ref())
            .await
            .map_err(|error| ToolError::execution_failed(error.to_string()))?;
        let admission_wait_ms = heavy_permit
            .as_ref()
            .map(|permit| u64::try_from(permit.queued_for().as_millis()).unwrap_or(u64::MAX));
        let admission_limit = heavy_permit.as_ref().map(HeavyCommandPermit::limit);
        let admission_memory = heavy_permit
            .as_ref()
            .map(HeavyCommandPermit::memory_pressure);

        // Route through external sandbox backend when configured.
        if let Some(backend) = &context.sandbox_backend {
            if self.optional_timeout {
                return Err(ToolError::not_available(
                    "bash is unavailable with this external sandbox backend because it cannot preserve combined streaming output and timeout semantics. Use the native sandbox or search for the backend-specific shell tool.",
                ));
            }
            if matches!(context.shell_policy, ShellPolicy::ReadOnly) {
                return Err(ToolError::permission_denied(
                    "Read-only shell cannot use an external sandbox backend because that interface accepts a raw command string rather than the classifier-approved argv. Use File read/search, or rerun without the external sandbox backend.",
                ));
            }
            if interactive {
                return Ok(ToolResult::error(
                    "Interactive mode is not supported with external sandbox backends.",
                ));
            }
            if background {
                return Ok(ToolResult::error(
                    "Background mode is not supported with external sandbox backends.",
                ));
            }
            if tty {
                return Ok(ToolResult::error(
                    "TTY mode is not supported with external sandbox backends.",
                ));
            }

            let started = std::time::Instant::now();
            let backend_result = backend.exec(command, &extra_env).await;

            let result = match backend_result {
                Ok(output) => {
                    let (stdout, stdout_meta) = truncate_with_meta(&output.stdout);
                    let (stderr, stderr_meta) = truncate_with_meta(&output.stderr);
                    ShellResult {
                        task_id: None,
                        status: if output.exit_code == 0 {
                            ShellStatus::Completed
                        } else {
                            ShellStatus::Failed
                        },
                        exit_code: Some(i64::from(output.exit_code)),
                        stdout,
                        stderr,
                        duration_ms: u64::try_from(started.elapsed().as_millis())
                            .unwrap_or(u64::MAX),
                        stdout_len: stdout_meta.original_len,
                        stderr_len: stderr_meta.original_len,
                        stdout_omitted: stdout_meta.omitted,
                        stderr_omitted: stderr_meta.omitted,
                        stdout_truncated: stdout_meta.truncated,
                        stderr_truncated: stderr_meta.truncated,
                        sandboxed: true,
                        sandbox_type: Some("opensandbox".to_string()),
                        sandbox_denied: false,
                    }
                }
                Err(e) => {
                    return Ok(ToolResult::error(format!("Sandbox backend error: {e}")));
                }
            };

            // Build result (reuse the existing output rendering below).
            let stdout_summary = summarize_output(&result.stdout);
            let stderr_summary = summarize_output(&result.stderr);
            let summary = if !stderr_summary.is_empty() {
                stderr_summary.clone()
            } else {
                stdout_summary.clone()
            };
            let python_dependency_hint = python_build_dependency_hint(command, &result);
            let mut output = if result.stdout.is_empty() && result.stderr.is_empty() {
                "(no output)".to_string()
            } else if result.stderr.is_empty() {
                result.stdout.clone()
            } else {
                format!("{}\n\nSTDERR:\n{}", result.stdout, result.stderr)
            };
            if let Some(hint) = python_dependency_hint {
                output = format!("{hint}\n\n{output}");
            }

            let mut metadata = json!({
                "exit_code": result.exit_code,
                "exit_code_hex": exit_code_hex(result.exit_code),
                "status": format!("{:?}", result.status),
                "duration_ms": result.duration_ms,
                "sandboxed": true,
                "sandbox_type": "opensandbox",
                "sandbox_denied": false,
                "task_id": result.task_id,
                "stdout_len": result.stdout_len,
                "stderr_len": result.stderr_len,
                "stdout_truncated": result.stdout_truncated,
                "stderr_truncated": result.stderr_truncated,
                "stdout_omitted": result.stdout_omitted,
                "stderr_omitted": result.stderr_omitted,
                "summary": summary,
                "stdout_summary": stdout_summary,
                "stderr_summary": stderr_summary,
                "safety_level": format!("{:?}", safety.level),
                "interactive": false,
                "canceled": false,
                "sandbox_backend": "opensandbox",
                "expense_class": match command_expense {
                    CommandExpense::Heavy => "heavy",
                    CommandExpense::Normal => "normal",
                },
                "resource_admission_wait_ms": admission_wait_ms,
                "resource_admission_limit": admission_limit,
            });
            attach_shell_owner_metadata(&mut metadata, context);
            attach_cargo_failure_summary(&mut metadata, command, &result);
            attach_python_build_dependency_hint(&mut metadata, python_dependency_hint);

            return Ok(ToolResult {
                content: output,
                success: result.status == ShellStatus::Completed,
                metadata: Some(metadata),
            });
        }

        let mut lifecycle_warning = None;
        let result = if interactive {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            let work_lifecycle = shell_work_lifecycle_from_context(context);
            let task_id = format!("shell_{}", &Uuid::new_v4().to_string()[..8]);
            let mut spawn_guard =
                ShellSpawnIntentGuard::new(work_lifecycle.clone(), &task_id, command)
                    .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            let result = manager.execute_interactive_with_policy_env(
                command,
                working_dir.as_deref(),
                timeout_value_ms,
                policy_override,
                extra_env,
            );
            match result {
                Ok(result) => {
                    // The process result is authoritative once execution has
                    // completed. Disarm before observing it so a graph-write
                    // failure cannot relabel a successful command as Failed.
                    spawn_guard.disarm();
                    if let Some(lifecycle) = work_lifecycle.as_ref() {
                        let raw_bytes = result.stdout_len.saturating_add(result.stderr_len);
                        if let Err(err) = lifecycle.observe(&task_id, &result.status, 1, raw_bytes)
                        {
                            tracing::warn!(shell_id = %task_id, error = %err, "interactive shell completed but Work lifecycle reconciliation failed");
                            lifecycle_warning = Some(err.to_string());
                        }
                    }
                    Ok(result)
                }
                Err(err) => Err(err),
            }
        } else if background {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            let result = manager.execute_with_options_env_for_owner_and_work(
                command,
                working_dir.as_deref(),
                timeout_value_ms,
                true,
                stdin_data.as_deref(),
                tty,
                policy_override,
                extra_env,
                shell_job_owner_from_context(context),
                context.state_namespace.clone(),
                context.origin_tool_call_id.clone(),
                context.origin_turn_id.clone(),
                shell_work_lifecycle_from_context(context),
                None,
                persist,
                (1_000, 600_000),
            );
            if let (Ok(result), Some(permit)) = (&result, heavy_permit)
                && let Some(task_id) = result.task_id.as_deref()
            {
                manager
                    .attach_heavy_permit(task_id, permit)
                    .map_err(|error| ToolError::execution_failed(error.to_string()))?;
            }
            result
        } else {
            execute_foreground_via_background(
                context,
                command,
                heavy_permit,
                working_dir,
                timeout_ms,
                stdin_data.as_deref(),
                combined_output,
                policy_override,
                extra_env,
                matches!(context.shell_policy, ShellPolicy::ReadOnly),
                if self.optional_timeout {
                    (1, BASH_MAX_TIMEOUT_MS)
                } else {
                    (1_000, 600_000)
                },
            )
            .await
        };

        match result {
            Ok(result) => {
                let backgrounded_foreground =
                    !background && !interactive && result.status == ShellStatus::Running;
                if (background || backgrounded_foreground)
                    && let (Some(shell_id), Some(task_id)) = (
                        result.task_id.as_deref(),
                        context.runtime.active_task_id.clone(),
                    )
                    && let Ok(mut manager) = context.shell_manager.lock()
                {
                    let _ = manager.tag_linked_task(shell_id, Some(task_id));
                }

                let was_cancelled = context
                    .cancel_token
                    .as_ref()
                    .is_some_and(|token| token.is_cancelled());
                if self.optional_timeout {
                    return finish_contract_bash_result(result, timeout_ms, context);
                }
                let task_id_str = result.task_id.clone().unwrap_or_default();
                let stdout_summary = summarize_output(&result.stdout);
                let stderr_summary = summarize_output(&result.stderr);
                let summary = if !stderr_summary.is_empty() {
                    stderr_summary.clone()
                } else {
                    stdout_summary.clone()
                };
                let network_restricted_hint =
                    shell_network_restricted_hint(context, command, &result).map(str::to_string);
                let sandbox_denied_hint = if network_restricted_hint.is_none() {
                    shell_sandbox_denied_hint(context, &result)
                } else {
                    None
                };
                let provenance_hint = macos_provenance_hint(&result);
                let python_dependency_hint = python_build_dependency_hint(command, &result);
                let mut output = if interactive {
                    format!(
                        "Interactive command completed (exit code: {:?})",
                        result.exit_code
                    )
                } else if result.status == ShellStatus::Completed {
                    if result.stdout.is_empty() && result.stderr.is_empty() {
                        "(no output)".to_string()
                    } else if result.stderr.is_empty() {
                        result.stdout.clone()
                    } else {
                        format!("{}\n\nSTDERR:\n{}", result.stdout, result.stderr)
                    }
                } else if persist && result.status == ShellStatus::Running {
                    format!(
                        "Persistent service staged: {task_id_str}. Probe readiness with a separate command. Codewhale will transfer ownership only if this exec finishes successfully."
                    )
                } else if result.status == ShellStatus::Running {
                    let completion_contract = if context.owner_agent_id.is_some() {
                        "completion stays in task/status and is not injected into the parent model."
                    } else {
                        "completion is delivered to the model as an internal runtime event and shown in task/status state."
                    };
                    if backgrounded_foreground {
                        format!(
                            "Foreground shell wait moved to /jobs: {task_id_str}\n\nReturns immediately; {completion_contract} Keep working; call Bash action=\"wait\" task_id=\"{task_id_str}\" at a true dependency to block until completion or timeout."
                        )
                    } else {
                        format!(
                            "Background task started: {task_id_str}\n\nReturns immediately; {completion_contract} Codewhale terminates this task when the session exits. If a service must survive a successful headless exec, start it with background=true and persist=true. Keep working; call Bash action=\"wait\" task_id=\"{task_id_str}\" at a true dependency to block until completion or timeout."
                        )
                    }
                } else if result.status == ShellStatus::Killed && was_cancelled {
                    format!(
                        "Command canceled; process killed.\n\nSTDOUT:\n{}\n\nSTDERR:\n{}",
                        result.stdout, result.stderr
                    )
                } else if result.status == ShellStatus::TimedOut {
                    format!(
                        "Command timed out after {timeout_value_ms}ms; process killed.\n\n{FOREGROUND_TIMEOUT_RECOVERY_HINT}\n\nSTDOUT:\n{}\n\nSTDERR:\n{}",
                        result.stdout, result.stderr
                    )
                } else {
                    format!(
                        "Command failed ({})\n\nSTDOUT:\n{}\n\nSTDERR:\n{}",
                        exit_code_label(result.exit_code),
                        result.stdout,
                        result.stderr
                    )
                };
                if let Some(hint) = network_restricted_hint.as_deref() {
                    output = format!("{hint}\n\n{output}");
                }
                if let Some(hint) = sandbox_denied_hint.as_deref() {
                    output = format!("{hint}\n\n{output}");
                }
                if let Some(hint) = provenance_hint {
                    output = format!("{hint}\n\n{output}");
                }
                if let Some(hint) = python_dependency_hint {
                    output = format!("{hint}\n\n{output}");
                }

                let mut metadata = json!({
                    "exit_code": result.exit_code,
                    "exit_code_hex": exit_code_hex(result.exit_code),
                    "status": format!("{:?}", result.status),
                    "duration_ms": result.duration_ms,
                    "sandboxed": result.sandboxed,
                    "sandbox_type": result.sandbox_type,
                    "sandbox_denied": result.sandbox_denied,
                    "task_id": result.task_id,
                    "stdout_len": result.stdout_len,
                    "stderr_len": result.stderr_len,
                    "stdout_truncated": result.stdout_truncated,
                    "stderr_truncated": result.stderr_truncated,
                    "stdout_omitted": result.stdout_omitted,
                    "stderr_omitted": result.stderr_omitted,
                    "lifecycle_warning": lifecycle_warning,
                    "expense_class": match command_expense {
                        CommandExpense::Heavy => "heavy",
                        CommandExpense::Normal => "normal",
                    },
                    "resource_admission_wait_ms": admission_wait_ms,
                    "resource_admission_limit": admission_limit,
                    "resource_admission_memory": match admission_memory {
                        Some(MemoryPressure::Critical) => "critical",
                        Some(MemoryPressure::Constrained) => "constrained",
                        Some(MemoryPressure::Nominal) | None => "nominal",
                        Some(MemoryPressure::Unknown) => "unknown",
                    },
                    "summary": summary,
                    "stdout_summary": stdout_summary,
                    "stderr_summary": stderr_summary,
                    "safety_level": format!("{:?}", safety.level),
                    "interactive": interactive,
                    "combined_output": combined_output,
                    "canceled": was_cancelled,
                    "execpolicy": execpolicy_decision.as_ref().map(|decision| match decision {
                        ExecPolicyDecision::Allow => json!({
                            "decision": "allow",
                        }),
                        ExecPolicyDecision::Deny(reason) => json!({
                            "decision": "deny",
                            "reason": reason,
                        }),
                        ExecPolicyDecision::AskUser(reason) => json!({
                            "decision": "ask_user",
                            "reason": reason,
                        }),
                    }),
                });
                metadata["backgrounded"] = json!(background || backgrounded_foreground);
                if persist {
                    metadata["persist_requested"] = json!(true);
                    metadata["ownership"] = json!("managed_pending_exec_success");
                    metadata["background_policy"] = json!("pending_ownership_transfer");
                    metadata["auto_resume_on_completion"] = json!(false);
                    metadata["completion_surface"] = json!("headless_exec_release_receipt");
                } else if background || backgrounded_foreground {
                    let child_owned = context.owner_agent_id.is_some();
                    metadata["auto_resume_on_completion"] = json!(!child_owned);
                    metadata["completion_surface"] = if child_owned {
                        json!("task_status_and_explicit_wait")
                    } else {
                        json!("runtime_event_and_task_status")
                    };
                    metadata["background_policy"] = json!("nonblocking");
                }
                if result.status == ShellStatus::TimedOut && !background && !interactive {
                    metadata["foreground_timeout_recovery"] = json!({
                        "process_killed": true,
                        "hint": FOREGROUND_TIMEOUT_RECOVERY_HINT,
                        "recommended_tools": ["Bash", "task_shell_start", "task_shell_wait"],
                        "rerun_as": {"tool": "Bash", "action": "run", "background": true},
                        "poll_with": [
                            {"tool": "Bash", "action": "wait"},
                            {"tool": "task_shell_wait"}
                        ]
                    });
                }
                if let Some(hint) = network_restricted_hint {
                    metadata["sandbox_network_restricted"] = json!(true);
                    metadata["sandbox_network_denied_hint"] = json!(hint);
                }
                if let Some(hint) = sandbox_denied_hint {
                    metadata["sandbox_denied_hint"] = json!(hint);
                }
                if provenance_hint.is_some() {
                    metadata["macos_provenance_restricted"] = json!(true);
                }
                attach_shell_owner_metadata(&mut metadata, context);
                attach_cargo_failure_summary(&mut metadata, command, &result);
                attach_python_build_dependency_hint(&mut metadata, python_dependency_hint);

                Ok(ToolResult {
                    content: output,
                    success: result.status == ShellStatus::Completed
                        || result.status == ShellStatus::Running,
                    metadata: Some(metadata),
                })
            }
            Err(e) => Ok(ToolResult::error(shell_execution_failed_message(&e))),
        }
    }
}

/// Render a spawn/stream failure for the model and the user: the full cause
/// chain (an anyhow context alone hides the `ENOSPC`/`EMFILE` underneath) plus,
/// when the innermost error looks like host resource exhaustion, what to do
/// about it. The shell tool keeps no state from a failed spawn, so retrying is
/// always safe.
fn shell_execution_failed_message(error: &anyhow::Error) -> String {
    let hint = error
        .chain()
        .filter_map(|cause| cause.downcast_ref::<io::Error>())
        .find_map(output::resource_exhaustion_hint);
    match hint {
        Some(hint) => format!(
            "Shell execution failed: {error:#}. Likely host resource exhaustion — {hint}. The shell tool itself is still usable; the next call starts fresh."
        ),
        None => format!("Shell execution failed: {error:#}"),
    }
}

/// Maximum deliberate dependency-barrier wait accepted by `exec_shell_wait`.
pub(crate) const EXEC_SHELL_WAIT_MAX_TIMEOUT_MS: u64 = 600_000;

impl BashTool {
    async fn execute_wait(
        &self,
        input: &serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        // Multi-task wait (#5549): task_ids + until=any|all. The validated
        // single-task path stays unchanged below when task_ids is absent.
        if let Some(task_ids) = input.get("task_ids") {
            let ids = task_ids
                .as_array()
                .ok_or_else(|| type_mismatch("task_ids", task_ids, "an array of strings"))?;
            let ids = ids
                .iter()
                .map(|value| {
                    value
                        .as_str()
                        .ok_or_else(|| type_mismatch("task_ids", value, "strings"))
                        .map(str::to_string)
                })
                .collect::<Result<Vec<_>, _>>()?;
            return self.execute_wait_many(input, context, &ids).await;
        }
        let task_id = required_task_id(input)?;
        let wait = match first_present_field(input, &["wait", "block"]) {
            None => true,
            Some((name, value)) => value
                .as_bool()
                .ok_or_else(|| type_mismatch(name, value, "a boolean"))?,
        };
        let timeout_ms = wait_timeout_ms(input)?;

        let (delta, wait_canceled) = if wait {
            wait_for_shell_delta_cancellable(context, task_id, timeout_ms).await?
        } else {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            let delta = manager
                .get_output_delta_for_session(&context.state_namespace, task_id, false, timeout_ms)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            (delta, false)
        };

        let status = delta.result.status.clone();
        let mut result = build_shell_delta_tool_result(delta, context);
        if let Some(metadata) = result.metadata.as_mut()
            && let Some(object) = metadata.as_object_mut()
        {
            object.insert("wait_timeout_ms".to_string(), json!(timeout_ms));
        }
        if wait_canceled {
            if matches!(status, ShellStatus::Running) {
                result.content = format!(
                    "Wait canceled; background shell task {task_id} is still running.\n\n{}",
                    result.content
                );
            }
            if let Some(metadata) = result.metadata.as_mut()
                && let Some(object) = metadata.as_object_mut()
            {
                object.insert("wait_canceled".to_string(), json!(true));
            }
        }

        Ok(result)
    }

    /// Wait on several background tasks at once (#5549).
    ///
    /// `until` selects the completion condition: `any` returns as soon as one
    /// task is terminal, `all` waits for every one. Unknown task ids fail the
    /// call (same contract as the single-task path); a timeout reports the
    /// still-running ids so the caller can cancel or wait again.
    async fn execute_wait_many(
        &self,
        input: &serde_json::Value,
        context: &ToolContext,
        task_ids: &[String],
    ) -> Result<ToolResult, ToolError> {
        let until = match input.get("until").and_then(serde_json::Value::as_str) {
            None | Some("all") => "all",
            Some("any") => "any",
            Some(other) => {
                return Err(ToolError::invalid_input(format!(
                    "until must be \"any\" or \"all\", got {other}"
                )));
            }
        };
        let wait = match first_present_field(input, &["wait", "block"]) {
            None => true,
            Some((name, value)) => value
                .as_bool()
                .ok_or_else(|| type_mismatch(name, value, "a boolean"))?,
        };
        let timeout_ms = wait_timeout_ms(input)?;
        let deadline = std::time::Instant::now() + Duration::from_millis(timeout_ms);
        let mut timed_out = false;
        let mut wait_canceled = false;

        let snapshot =
            |manager: &mut ShellManager| -> Result<Vec<(String, ShellStatus)>, ToolError> {
                task_ids
                    .iter()
                    .map(|id| {
                        let detail = manager
                            .inspect_job(id)
                            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
                        Ok((id.clone(), detail.snapshot.status))
                    })
                    .collect()
            };

        let mut poll_tick_ms: u64 = FOREGROUND_POLL_INITIAL_MS;
        let statuses = loop {
            let current = {
                let mut manager = context
                    .shell_manager
                    .lock()
                    .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
                snapshot(&mut manager)?
            };
            if context
                .cancel_token
                .as_ref()
                .is_some_and(|token| token.is_cancelled())
            {
                wait_canceled = true;
                break current;
            }
            let running = current
                .iter()
                .filter(|(_, status)| *status == ShellStatus::Running)
                .count();
            let terminal = current.len().saturating_sub(running);
            let satisfied = if until == "any" {
                terminal >= 1
            } else {
                running == 0
            };
            if !wait || satisfied {
                break current;
            }
            if std::time::Instant::now() >= deadline {
                timed_out = true;
                break current;
            }
            tokio::time::sleep(Duration::from_millis(poll_tick_ms)).await;
            poll_tick_ms = (poll_tick_ms * 2).min(FOREGROUND_POLL_MAX_MS);
        };

        let running_after = statuses
            .iter()
            .filter(|(_, status)| *status == ShellStatus::Running)
            .count();
        let settled = statuses.len().saturating_sub(running_after);
        let lines: Vec<String> = statuses
            .iter()
            .map(|(id, status)| format!("{id}: {status:?}"))
            .collect();
        let still_running: Vec<&str> = statuses
            .iter()
            .filter(|(_, status)| *status == ShellStatus::Running)
            .map(|(id, _)| id.as_str())
            .collect();
        let summary = if timed_out {
            format!(
                "wait timed out after {}ms; still running: {}",
                timeout_ms,
                if still_running.is_empty() {
                    "none".to_string()
                } else {
                    still_running.join(", ")
                }
            )
        } else if wait_canceled {
            format!(
                "wait canceled; still running: {}",
                if still_running.is_empty() {
                    "none".to_string()
                } else {
                    still_running.join(", ")
                }
            )
        } else {
            format!(
                "{settled} of {} background command{} settled; remaining running: {}",
                statuses.len(),
                if statuses.len() == 1 { "" } else { "s" },
                if still_running.is_empty() {
                    "none".to_string()
                } else {
                    still_running.join(", ")
                }
            )
        };
        let content = format!("{summary}\n{}\n", lines.join("\n"));
        let mut metadata = serde_json::Map::new();
        metadata.insert(
            "statuses".to_string(),
            serde_json::Value::Object(
                statuses
                    .iter()
                    .map(|(id, status)| (id.clone(), serde_json::json!(format!("{status:?}"))))
                    .collect::<serde_json::Map<_, _>>(),
            ),
        );
        metadata.insert("wait_timeout_ms".to_string(), json!(timeout_ms));
        metadata.insert("until".to_string(), json!(until));
        metadata.insert("timed_out".to_string(), json!(timed_out));
        if wait_canceled {
            metadata.insert("wait_canceled".to_string(), json!(true));
        }
        Ok(ToolResult {
            content: content.trim().to_string(),
            success: true,
            metadata: Some(serde_json::Value::Object(metadata)),
        })
    }

    async fn execute_interact(
        &self,
        input: &serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        let task_id = required_task_id(input)?;
        let close_stdin = optional_bool(input, "close_stdin", false)?;
        let timeout_ms = optional_u64(input, "timeout_ms", 1_000)?;
        // Same strict-type contract as `run` (2026-08-04): a non-string here
        // was silently dropped, so an `interact` call reported success while
        // writing nothing to the child's stdin. Alias order also matches
        // `run` now — `stdin` first — so the same payload reaches the same
        // place whichever spelling the model uses.
        let interaction_input = match first_present_field(input, &["stdin", "input", "data"]) {
            None => "",
            Some((name, value)) => {
                let payload = value
                    .as_str()
                    .ok_or_else(|| type_mismatch(name, value, "a string"))?;
                // The same bound the inline spawn payload carries: one
                // oversized `interact` payload would push megabytes through
                // a bounded write for no benefit — feed ≥64 KiB of input as
                // several interact calls (or generate it in the command).
                // The deadline below is what actually bounds the stall; this
                // keeps each write bounded too.
                if payload.len() > MAX_INLINE_STDIN_BYTES {
                    return Ok(ToolResult::error(format!(
                        "The interact stdin payload is {} bytes; the shell tool accepts at most {MAX_INLINE_STDIN_BYTES} per interact call — split the feed across interact calls.",
                        payload.len()
                    )));
                }
                payload
            }
        };

        {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            if !interaction_input.is_empty() || close_stdin {
                manager
                    .write_stdin_for_session(
                        &context.state_namespace,
                        task_id,
                        interaction_input,
                        close_stdin,
                    )
                    .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            }
        }

        let mut elapsed = 0u64;
        loop {
            if context
                .cancel_token
                .as_ref()
                .is_some_and(|token| token.is_cancelled())
            {
                let mut manager = context
                    .shell_manager
                    .lock()
                    .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
                let delta = manager
                    .get_output_delta_for_session(&context.state_namespace, task_id, false, 0)
                    .map_err(|err| ToolError::execution_failed(err.to_string()))?;
                let mut result = build_shell_delta_tool_result(delta, context);
                if let Some(metadata) = result.metadata.as_mut()
                    && let Some(object) = metadata.as_object_mut()
                {
                    object.insert("wait_canceled".to_string(), json!(true));
                }
                return Ok(result);
            }

            let delta = {
                let mut manager = context
                    .shell_manager
                    .lock()
                    .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
                manager
                    .get_output_delta_for_session(&context.state_namespace, task_id, false, 0)
                    .map_err(|err| ToolError::execution_failed(err.to_string()))?
            };

            if !delta.result.stdout.is_empty()
                || !delta.result.stderr.is_empty()
                || delta.result.status != ShellStatus::Running
                || elapsed >= timeout_ms
            {
                return Ok(build_shell_delta_tool_result(delta, context));
            }

            tokio::time::sleep(Duration::from_millis(50)).await;
            elapsed = elapsed.saturating_add(50);
        }
    }

    async fn execute_cancel(
        &self,
        input: &serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        let cancel_all = optional_bool(input, "all", false)?;
        let mut manager = context
            .shell_manager
            .lock()
            .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;

        if cancel_all {
            let results = manager
                .kill_running_for_session(&context.state_namespace)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            if results.is_empty() {
                return Ok(ToolResult {
                    content: "No running background commands.".to_string(),
                    success: true,
                    metadata: Some(json!({
                        "status": "Noop",
                        "canceled": 0,
                        "task_ids": [],
                    })),
                });
            }

            let task_ids = results
                .iter()
                .filter_map(|result| result.task_id.clone())
                .collect::<Vec<_>>();
            return Ok(ToolResult {
                content: format!(
                    "Canceled {} background command{}: {}",
                    task_ids.len(),
                    if task_ids.len() == 1 { "" } else { "s" },
                    task_ids.join(", ")
                ),
                success: true,
                metadata: Some(json!({
                    "status": "Killed",
                    "canceled": task_ids.len(),
                    "task_ids": task_ids,
                })),
            });
        }

        let task_id = required_task_id(input)?;
        let result = manager
            .kill_for_session(&context.state_namespace, task_id)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        let task_id = result
            .task_id
            .clone()
            .unwrap_or_else(|| task_id.to_string());
        Ok(ToolResult {
            content: format!("Canceled background command: {task_id}"),
            success: true,
            metadata: Some(json!({
                "status": format!("{:?}", result.status),
                "task_id": task_id,
                "exit_code": result.exit_code,
                "duration_ms": result.duration_ms,
            })),
        })
    }
}

fn required_task_id(input: &serde_json::Value) -> Result<&str, ToolError> {
    // A present-but-non-string task_id is a type error, not a missing field:
    // "missing required field" sends the model's retry in the wrong
    // direction when it already supplied `task_id: 42` (2026-08-04 review).
    match first_present_field(input, &["task_id", "id"]) {
        None => Err(ToolError::missing_field("task_id")),
        Some((name, value)) => value
            .as_str()
            .ok_or_else(|| type_mismatch(name, value, "a string")),
    }
}

/// First PRESENT value among aliased spellings of one field. `null` counts
/// as absent, matching the `is_absent` rule the shared typed helpers use.
fn first_present_field<'a>(
    input: &'a serde_json::Value,
    names: &[&'static str],
) -> Option<(&'static str, &'a serde_json::Value)> {
    names.iter().find_map(|name| match input.get(*name) {
        None | Some(serde_json::Value::Null) => None,
        Some(value) => Some((*name, value)),
    })
}

/// Effective `action=wait` timeout in milliseconds. `timeout_ms` is
/// canonical; `timeout_secs` (seconds) and bare `timeout` (milliseconds) are
/// honored so a habit formed on other wait tools gets the duration it asked
/// for instead of silently falling back to the 30 s default.
fn wait_timeout_ms(input: &serde_json::Value) -> Result<u64, ToolError> {
    match first_present_field(input, &["timeout_ms", "timeout_secs", "timeout"]) {
        None => Ok(30_000),
        Some(("timeout_secs", value)) => {
            let secs = value
                .as_u64()
                .ok_or_else(|| type_mismatch("timeout_secs", value, "an integer"))?;
            Ok(secs.saturating_mul(1_000))
        }
        Some((name, value)) => value
            .as_u64()
            .ok_or_else(|| type_mismatch(name, value, "an integer")),
    }
}

fn build_shell_delta_tool_result(delta: ShellDeltaResult, context: &ToolContext) -> ToolResult {
    let result = delta.result;
    let network_restricted_hint =
        shell_network_restricted_hint(context, &delta.command, &result).map(str::to_string);
    let sandbox_denied_hint = if network_restricted_hint.is_none() {
        shell_sandbox_denied_hint(context, &result)
    } else {
        None
    };
    let provenance_hint = macos_provenance_hint(&result);
    let python_dependency_hint = python_build_dependency_hint(&delta.command, &result);
    let stdout_summary = summarize_output(&result.stdout);
    let stderr_summary = summarize_output(&result.stderr);
    let summary = if !stderr_summary.is_empty() {
        stderr_summary.clone()
    } else {
        stdout_summary.clone()
    };

    let mut output = if result.stdout.is_empty() && result.stderr.is_empty() {
        match result.status {
            ShellStatus::Running => "Background task running (no new output).".to_string(),
            ShellStatus::Completed => "(no new output)".to_string(),
            ShellStatus::Failed => {
                format!("Command failed ({})", exit_code_label(result.exit_code))
            }
            ShellStatus::TimedOut => "Command timed out (no new output).".to_string(),
            ShellStatus::Killed => "Command killed (no new output).".to_string(),
        }
    } else if result.stderr.is_empty() {
        result.stdout.clone()
    } else {
        format!("{}\n\nSTDERR:\n{}", result.stdout, result.stderr)
    };
    // The model cannot see metadata, so surface the real elapsed time in the
    // visible content. Without it every wait result looks identical whether
    // the task just started or has been running for minutes, which biases the
    // model into busy-polling short waits and misjudging long ones.
    output = format!("{}\n\n{output}", wait_timing_line(&result));

    if let Some(hint) = network_restricted_hint.as_deref() {
        output = format!("{hint}\n\n{output}");
    }
    if let Some(hint) = sandbox_denied_hint.as_deref() {
        output = format!("{hint}\n\n{output}");
    }
    if let Some(hint) = provenance_hint {
        output = format!("{hint}\n\n{output}");
    }
    if let Some(hint) = python_dependency_hint {
        output = format!("{hint}\n\n{output}");
    }

    let mut metadata = json!({
        "exit_code": result.exit_code,
        "exit_code_hex": exit_code_hex(result.exit_code),
        "status": format!("{:?}", result.status),
        "duration_ms": result.duration_ms,
        "sandboxed": result.sandboxed,
        "sandbox_type": result.sandbox_type,
        "sandbox_denied": result.sandbox_denied,
        "task_id": result.task_id,
        "stdout_len": result.stdout_len,
        "stderr_len": result.stderr_len,
        "stdout_truncated": result.stdout_truncated,
        "stderr_truncated": result.stderr_truncated,
        "stdout_omitted": result.stdout_omitted,
        "stderr_omitted": result.stderr_omitted,
        "stdout_total_len": delta.stdout_total_len,
        "stderr_total_len": delta.stderr_total_len,
        "summary": summary,
        "stdout_summary": stdout_summary,
        "stderr_summary": stderr_summary,
        "command": delta.command,
        "stream_delta": true,
    });
    attach_shell_owner_metadata(&mut metadata, context);
    attach_cargo_failure_summary(&mut metadata, &delta.command, &result);
    attach_python_build_dependency_hint(&mut metadata, python_dependency_hint);

    let mut tool_result = ToolResult {
        content: output,
        success: matches!(result.status, ShellStatus::Completed | ShellStatus::Running),
        metadata: Some(metadata),
    };
    if let Some(hint) = network_restricted_hint
        && let Some(metadata) = tool_result.metadata.as_mut()
        && let Some(object) = metadata.as_object_mut()
    {
        object.insert("sandbox_network_restricted".to_string(), json!(true));
        object.insert("sandbox_network_denied_hint".to_string(), json!(hint));
    }
    if let Some(hint) = sandbox_denied_hint
        && let Some(metadata) = tool_result.metadata.as_mut()
        && let Some(object) = metadata.as_object_mut()
    {
        object.insert("sandbox_denied_hint".to_string(), json!(hint));
    }
    if provenance_hint.is_some()
        && let Some(metadata) = tool_result.metadata.as_mut()
        && let Some(object) = metadata.as_object_mut()
    {
        object.insert("macos_provenance_restricted".to_string(), json!(true));
    }
    tool_result
}

/// Human-readable elapsed time for a shell task ("450 ms", "12.3 s", "2m5s").
fn format_elapsed_ms(ms: u64) -> String {
    if ms < 1_000 {
        format!("{ms} ms")
    } else if ms < 60_000 {
        let secs = ms as f64 / 1_000.0;
        format!("{secs} s")
    } else {
        let total_secs = ms / 1_000;
        format!("{}m{}s", total_secs / 60, total_secs % 60)
    }
}

/// One-line status + elapsed summary for wait/delta results, placed at the top
/// of the visible content so the model can judge how long it actually waited.
fn wait_timing_line(result: &ShellResult) -> String {
    let status_phrase = match result.status {
        ShellStatus::Running => "still running",
        ShellStatus::Completed => "completed",
        ShellStatus::Failed => "failed",
        ShellStatus::Killed => "killed",
        ShellStatus::TimedOut => "timed out",
    };
    let elapsed = format_elapsed_ms(result.duration_ms);
    match result.task_id.as_deref() {
        Some(task_id) => format!("Task {task_id} {status_phrase} after {elapsed}."),
        None => format!("Task {status_phrase} after {elapsed}."),
    }
}

async fn wait_for_shell_delta_cancellable(
    context: &ToolContext,
    task_id: &str,
    timeout_ms: u64,
) -> Result<(ShellDeltaResult, bool), ToolError> {
    let timeout_ms = timeout_ms.clamp(1000, EXEC_SHELL_WAIT_MAX_TIMEOUT_MS);
    let deadline = Instant::now() + Duration::from_millis(timeout_ms);
    let mut stdout_accum = String::new();
    let mut stderr_accum = String::new();

    let mut poll_tick_ms: u64 = FOREGROUND_POLL_INITIAL_MS;
    let (command, result, stdout_total_len, stderr_total_len) = loop {
        if context
            .cancel_token
            .as_ref()
            .is_some_and(|token| token.is_cancelled())
        {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            let delta = manager
                .get_output_delta_for_session(&context.state_namespace, task_id, false, 0)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            append_shell_delta_output(&mut stdout_accum, &mut stderr_accum, &delta.result);
            return Ok((
                shell_delta_with_accumulated_output(
                    delta.command,
                    delta.result,
                    &stdout_accum,
                    &stderr_accum,
                    delta.stdout_total_len,
                    delta.stderr_total_len,
                ),
                true,
            ));
        }

        let delta = {
            let mut manager = context
                .shell_manager
                .lock()
                .map_err(|_| ToolError::execution_failed("shell manager lock poisoned"))?;
            manager
                .get_output_delta_for_session(&context.state_namespace, task_id, false, 0)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?
        };

        let stdout_total_len = delta.stdout_total_len;
        let stderr_total_len = delta.stderr_total_len;
        let command = delta.command.clone();
        append_shell_delta_output(&mut stdout_accum, &mut stderr_accum, &delta.result);

        let status = delta.result.status.clone();
        if status != ShellStatus::Running || Instant::now() >= deadline {
            break (command, delta.result, stdout_total_len, stderr_total_len);
        }

        tokio::time::sleep(Duration::from_millis(poll_tick_ms)).await;
        poll_tick_ms = (poll_tick_ms * 2).min(FOREGROUND_POLL_MAX_MS);
    };

    Ok((
        shell_delta_with_accumulated_output(
            command,
            result,
            &stdout_accum,
            &stderr_accum,
            stdout_total_len,
            stderr_total_len,
        ),
        false,
    ))
}

fn append_shell_delta_output(
    stdout_accum: &mut String,
    stderr_accum: &mut String,
    result: &ShellResult,
) {
    if !result.stdout.is_empty() {
        stdout_accum.push_str(&result.stdout);
    }
    if !result.stderr.is_empty() {
        stderr_accum.push_str(&result.stderr);
    }
}

fn shell_delta_with_accumulated_output(
    command: String,
    mut result: ShellResult,
    stdout_accum: &str,
    stderr_accum: &str,
    stdout_total_len: usize,
    stderr_total_len: usize,
) -> ShellDeltaResult {
    let (stdout, stdout_meta) = truncate_with_meta(stdout_accum);
    let (stderr, stderr_meta) = truncate_with_meta(stderr_accum);
    result.stdout = stdout;
    result.stderr = stderr;
    result.stdout_len = stdout_meta.original_len;
    result.stderr_len = stderr_meta.original_len;
    result.stdout_omitted = stdout_meta.omitted;
    result.stderr_omitted = stderr_meta.omitted;
    result.stdout_truncated = stdout_meta.truncated;
    result.stderr_truncated = stderr_meta.truncated;

    ShellDeltaResult {
        command,
        result,
        stdout_total_len,
        stderr_total_len,
    }
}

/// Tool for appending notes to a notes file.
pub struct NoteTool;

#[async_trait]
impl ToolSpec for NoteTool {
    fn name(&self) -> &'static str {
        "note"
    }

    fn description(&self) -> &'static str {
        "Append a note to the agent notes file for persistent context across sessions."
    }

    fn input_schema(&self) -> serde_json::Value {
        json!({
            "type": "object",
            "properties": {
                "content": {
                    "type": "string",
                    "description": "The note content to append"
                }
            },
            "required": ["content"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::WritesFiles]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Auto // Notes are low-risk
    }

    async fn execute(
        &self,
        input: serde_json::Value,
        context: &ToolContext,
    ) -> Result<ToolResult, ToolError> {
        let note_content = required_str(&input, "content")?;

        // Ensure parent directory exists
        if let Some(parent) = context.notes_path.parent() {
            std::fs::create_dir_all(parent).map_err(|e| {
                ToolError::execution_failed(format!("Failed to create notes directory: {e}"))
            })?;
        }

        // Append to notes file
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&context.notes_path)
            .map_err(|e| ToolError::execution_failed(format!("Failed to open notes file: {e}")))?;

        writeln!(file, "\n---\n{note_content}")
            .map_err(|e| ToolError::execution_failed(format!("Failed to write note: {e}")))?;

        Ok(ToolResult::success(format!(
            "Note appended to {}",
            context.notes_path.display()
        )))
    }
}

#[cfg(test)]
mod tests;
