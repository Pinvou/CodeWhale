//! `/update` — check for and install a new CodeWhale release without leaving
//! the TUI.
//!
//! The updater itself is not reimplemented here. `codewhale update` (see
//! `crates/cli/src/update.rs`) already resolves the release, downloads the
//! platform-correct asset, verifies its SHA256, and atomically replaces the
//! running binary; this command finds that binary and runs it. Duplicating any
//! of that logic would give us two updaters to keep honest.
//!
//! Two deliberate limits:
//!
//! * **Package-managed installs get instructions, not an updater run.**
//!   Overwriting a binary Homebrew, npm, or cargo owns leaves the manager's
//!   metadata describing a version that is no longer on disk, and the next
//!   upgrade silently reverts the user.
//! * **We do not relaunch.** This codebase has no self-exec/relaunch pattern,
//!   and inventing one under a TUI holding the terminal is not a small change.
//!   Telling the user to restart is the honest slice; that is also the only
//!   possible answer on Windows, where the replaced image is the one running.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};

use codewhale_command_contract::handler::CommandHandler;
use codewhale_command_contract::metadata::{CommandInfo, RegisterCommand};
use codewhale_release::InstallMethod;

use crate::commands::CommandResult;

pub(in crate::commands) const COMMAND_INFO: CommandInfo = CommandInfo {
    name: "update",
    aliases: &["upgrade"],
    usage: "/update [check|install]",
    description_key: "cmd_update_description",
};

pub(in crate::commands) struct UpdateCmd;

impl RegisterCommand<CommandResult> for UpdateCmd {
    fn info() -> &'static CommandInfo {
        &COMMAND_INFO
    }

    fn handler() -> CommandHandler<CommandResult> {
        CommandHandler::Pure(update)
    }
}

/// What `/update` was asked to do.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum UpdateMode {
    /// Explain what an install would do, and ask the updater whether one is
    /// available. The default: `/update` never installs without being told to.
    Check,
    /// Actually run the updater.
    Install,
}

fn parse_mode(arg: Option<&str>) -> Result<UpdateMode, String> {
    match arg
        .map(str::trim)
        .unwrap_or("")
        .to_ascii_lowercase()
        .as_str()
    {
        "" | "check" | "status" => Ok(UpdateMode::Check),
        "install" | "now" | "apply" | "yes" => Ok(UpdateMode::Install),
        other => Err(format!(
            "Unknown /update argument {other:?}. Usage: {}",
            COMMAND_INFO.usage
        )),
    }
}

/// How this install can be updated, resolved before anything runs.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum UpdaterPlan {
    /// Run this `codewhale` binary's `update` subcommand.
    Run(PathBuf),
    /// A package manager owns the binary; print its command instead.
    Managed(InstallMethod),
    /// Self-update would be correct, but no `codewhale` CLI is reachable —
    /// e.g. a bare `codewhale-tui` build with no sibling CLI.
    NoUpdater { exe: PathBuf },
}

/// The primary binary name. `codew` carries the same CLI and update subcommand.
const UPDATER_BIN_STEM: &str = "codewhale";

/// Resolve the update path without touching the process environment, so the
/// decision is testable. `exists` answers whether a candidate path is a file.
pub(super) fn resolve_updater(
    exe: Option<&Path>,
    method: InstallMethod,
    exists: &dyn Fn(&Path) -> bool,
) -> UpdaterPlan {
    if !method.supports_self_update() {
        return UpdaterPlan::Managed(method);
    }
    // No resolvable executable path is not a reason to guess at one: report it
    // as "no updater here" and let the user run the CLI themselves.
    let Some(exe) = exe else {
        return UpdaterPlan::NoUpdater {
            exe: PathBuf::from(UPDATER_BIN_STEM),
        };
    };
    if matches!(
        exe.file_stem().and_then(|stem| stem.to_str()),
        Some(UPDATER_BIN_STEM | "codew")
    ) {
        return UpdaterPlan::Run(exe.to_path_buf());
    }
    // A `codewhale-tui` build has no `update` subcommand, but the CLI that
    // does is normally installed right next to it.
    if let Some(dir) = exe.parent() {
        let extension = exe.extension().and_then(|ext| ext.to_str());
        let mut sibling = dir.join(UPDATER_BIN_STEM);
        if let Some(extension) = extension {
            sibling.set_extension(extension);
        }
        if exists(&sibling) {
            return UpdaterPlan::Run(sibling);
        }
    }
    UpdaterPlan::NoUpdater {
        exe: exe.to_path_buf(),
    }
}

/// Instructions for an install this command must not update in place.
pub(super) fn managed_install_message(method: InstallMethod) -> String {
    format!(
        "Codewhale was installed with {label}, which owns this binary.\n\
         {migration_help}\n\n\
         To keep this secondary {label} installation, run `{command}` in a shell.\n\
         /update will not replace a binary owned by {label}; restart Codewhale after changing installs.",
        label = method.label(),
        migration_help = codewhale_release::install::GITHUB_MIGRATION_HELP,
        command = method.update_command(),
    )
}

/// Instructions when self-update is right but no updater binary is reachable.
pub(super) fn no_updater_message(exe: &Path) -> String {
    format!(
        "No `{UPDATER_BIN_STEM}` updater was found for this install ({exe}).\n\
         The updater ships in the `{UPDATER_BIN_STEM}` CLI. Install or locate it, run \
         `{UPDATER_BIN_STEM} update` in a shell, then restart Codewhale.",
        exe = exe.display(),
    )
}

/// What an install would do, stated before it is done.
fn install_preamble(updater: &Path) -> String {
    format!(
        "`/update install` will run `{updater} update`: it resolves the latest release, \
         downloads the binary for this platform, verifies its SHA256 checksum, and atomically \
         replaces {updater}.\n\
         It does not restart Codewhale — you will need to do that yourself once it finishes. \
         The UI is paused while the updater runs.",
        updater = updater.display(),
    )
}

fn update(arg: Option<&str>) -> CommandResult {
    let mode = match parse_mode(arg) {
        Ok(mode) => mode,
        Err(message) => return CommandResult::error(message),
    };

    let exe = std::env::current_exe().ok();
    let method = match exe.as_deref() {
        Some(path) => InstallMethod::detect(path),
        None => InstallMethod::Binary,
    };

    match resolve_updater(exe.as_deref(), method, &|path: &Path| path.is_file()) {
        UpdaterPlan::Managed(method) => CommandResult::message(managed_install_message(method)),
        UpdaterPlan::NoUpdater { exe } => CommandResult::message(no_updater_message(&exe)),
        UpdaterPlan::Run(updater) => run_updater(&updater, mode),
    }
}

/// Total budget for the updater subprocess. Resolving, downloading, and
/// checksum-verifying a release is a minutes-scale operation, but it must
/// not wedge `/update` forever on a stalled link.
const UPDATER_TOTAL_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(15 * 60);

/// Once the updater has exited, how long its pipe drains get to see EOF
/// before the run proceeds without them. Normally EOF is already there —
/// the child closed its write ends by exiting — so the grace only bounds a
/// writer that outlived the child: a grandchild that inherited the pipe
/// ends. Same shape as the bounded tool runner's drain grace
/// (`tools/process.rs`); without a bound, an unbounded `join()` could hold
/// `/update` long past [`UPDATER_TOTAL_TIMEOUT`].
const UPDATER_DRAIN_GRACE: std::time::Duration = std::time::Duration::from_secs(2);

/// Capture ceiling per drained stream, mirroring the shared bounded runner
/// (`tools/process.rs`): a chatty or stuck error-printing updater must not
/// grow the drain buffers without bound across a multi-minute run. Output
/// past the cap is dropped with an explicit note appended to the transcript
/// snapshot, so the truncation stays observable.
const UPDATER_DRAIN_CAPTURE_CAP: usize = 16 * 1024 * 1024;

fn run_updater(updater: &Path, mode: UpdateMode) -> CommandResult {
    #[cfg(unix)]
    use std::os::unix::process::CommandExt as _;
    use std::time::Instant;

    let mut command = Command::new(updater);
    command.arg("update");
    if mode == UpdateMode::Check {
        command.arg("--check");
    }
    // A private process group per updater run, so the timeout kill below can
    // take the whole tree out: the updater forks helpers instead of always
    // exec'ing them, and a child-only kill orphans every grandchild. Same
    // pattern as the bounded tool runner (`tools/process.rs`).
    #[cfg(unix)]
    command.process_group(0);

    let mut child = match command
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
    {
        Ok(child) => child,
        Err(err) => {
            return CommandResult::error(format!(
                "Failed to run `{} update`: {err}\nRun it in a shell instead, then restart Codewhale.",
                updater.display()
            ));
        }
    };

    // Drain both pipes into shared buffers on helper threads while polling
    // for exit. Draining after exit alone could deadlock: a child blocked
    // writing into a full pipe never exits, and `.output()` would wait
    // forever. The buffers are shared so a drain that misses its deadline
    // leaves behind what it captured instead of losing it with the thread.
    let stdout_pipe = child.stdout.take();
    let stderr_pipe = child.stderr.take();
    let stdout_buf = Arc::new(Mutex::new(Vec::new()));
    let stderr_buf = Arc::new(Mutex::new(Vec::new()));
    let (drain_done_tx, drain_done_rx) = mpsc::channel();
    spawn_drain(stdout_pipe, Arc::clone(&stdout_buf), drain_done_tx.clone());
    spawn_drain(stderr_pipe, Arc::clone(&stderr_buf), drain_done_tx.clone());
    drop(drain_done_tx);

    let deadline = Instant::now() + UPDATER_TOTAL_TIMEOUT;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) => {
                if Instant::now() >= deadline {
                    // Kill the whole tree, not just the direct child (see
                    // `process_group(0)` above), then reap so nothing keeps
                    // the pipes open on purpose.
                    kill_updater_tree(&mut child);
                    let _ = child.wait();
                    break None;
                }
                std::thread::sleep(std::time::Duration::from_millis(200));
            }
            Err(err) => {
                // The drains are abandoned here; each ends on its own when
                // its pipe closes.
                return CommandResult::error(format!(
                    "Failed to run `{} update`: {err}\nRun it in a shell instead, then restart Codewhale.",
                    updater.display()
                ));
            }
        }
    };

    // The updater is gone; give the drains a short grace to see EOF, then
    // proceed with whatever they captured. Without this bound a grandchild
    // holding the pipe write ends would wedge an unbounded `join()` here,
    // long past the budget above.
    let (stdout, stderr) = collect_drained(
        &drain_done_rx,
        &stdout_buf,
        &stderr_buf,
        UPDATER_DRAIN_GRACE,
    );

    let Some(status) = status else {
        return CommandResult::error(timeout_message(updater, &stdout, &stderr));
    };

    let transcript = updater_transcript(&stdout, &stderr);
    if !status.success() {
        return CommandResult::error(format!(
            "`{updater} update` failed.\n{transcript}",
            updater = updater.display()
        ));
    }

    match mode {
        UpdateMode::Check => {
            CommandResult::message(format!("{}\n\n{transcript}", install_preamble(updater)))
        }
        UpdateMode::Install => CommandResult::message(format!(
            "{transcript}\n\nRestart Codewhale to run the updated binary."
        )),
    }
}

/// Read one piped stream into the shared buffer until EOF, then report
/// completion. Chunked reads into the shared buffer (rather than a private
/// `read_to_end`) keep whatever arrived so far visible to the caller even
/// if this thread is later abandoned past its deadline.
fn spawn_drain<R: std::io::Read + Send + 'static>(
    pipe: Option<R>,
    buffer: Arc<Mutex<Vec<u8>>>,
    done: mpsc::Sender<()>,
) {
    std::thread::spawn(move || {
        if let Some(mut pipe) = pipe {
            let mut chunk = [0u8; 8192];
            let mut capped = false;
            loop {
                match pipe.read(&mut chunk) {
                    Ok(0) => break,
                    Ok(n) => {
                        let Ok(mut guard) = buffer.lock() else { break };
                        if guard.len() < UPDATER_DRAIN_CAPTURE_CAP {
                            let room = UPDATER_DRAIN_CAPTURE_CAP - guard.len();
                            guard.extend_from_slice(&chunk[..n.min(room)]);
                        } else if !capped {
                            // Cap reached: note the truncation once, drop the
                            // rest. The transcript renderer quotes at most a
                            // few KB, so the note survives to the user.
                            const CAP_NOTE: &str = "\n…[output truncated at the capture limit]";
                            guard.extend_from_slice(CAP_NOTE.as_bytes());
                            capped = true;
                        }
                    }
                    Err(_) => break,
                }
            }
        }
        // The receiver may already be gone (its deadline expired); that is
        // fine, the thread's work is done either way.
        let _ = done.send(());
    });
}

/// Wait for both pipe drains to finish within `budget`, then snapshot the
/// shared buffers. A drain that misses the deadline is abandoned: its
/// partial output stays in the buffer, and the thread ends when the pipe's
/// write ends close (the group kill closed them on the timeout path; on the
/// success paths a pipe-holding grandchild closes them when it exits).
fn collect_drained(
    done: &mpsc::Receiver<()>,
    stdout_buf: &Mutex<Vec<u8>>,
    stderr_buf: &Mutex<Vec<u8>>,
    budget: std::time::Duration,
) -> (Vec<u8>, Vec<u8>) {
    let started = std::time::Instant::now();
    for _ in 0..2 {
        let remaining = budget.saturating_sub(started.elapsed());
        match done.recv_timeout(remaining) {
            Ok(()) | Err(mpsc::RecvTimeoutError::Disconnected) => {}
            Err(mpsc::RecvTimeoutError::Timeout) => break,
        }
    }
    let stdout = stdout_buf.lock().map(|buf| buf.clone()).unwrap_or_default();
    let stderr = stderr_buf.lock().map(|buf| buf.clone()).unwrap_or_default();
    (stdout, stderr)
}

/// Kill the updater the budget expired on. On Unix this SIGKILLs the child's
/// process group — the child is spawned with `process_group(0)`, so `-pgid`
/// reaches it together with everything it forked that stayed in the group,
/// and never our own group. When there is no group to signal (Windows, or
/// the group is already gone) this kills the direct child; on Windows a
/// pipe-inheriting grandchild therefore survives the kill and is bounded by
/// the drain grace instead. Same shape as `tools/process.rs`'s
/// `kill_the_run`; the caller reaps with `child.wait()`.
fn kill_updater_tree(child: &mut std::process::Child) {
    #[cfg(unix)]
    {
        let pgid = libc::pid_t::try_from(child.id()).unwrap_or(0);
        if pgid > 0 {
            // SAFETY: a negative pid targets the process group led by the
            // child; the pgid came from `process_group(0)` at spawn, so this
            // group cannot be our own or the terminal's.
            let killed = unsafe { libc::kill(-pgid, libc::SIGKILL) };
            if killed == 0 {
                return;
            }
            // ESRCH usually means the group is already gone with the child
            // dead (the child led its own group and cannot have left it, so
            // an escaped live member is not expected here). Fall through to
            // the child-only kill on any kill error anyway — a no-op on a
            // dead child, the last chance to stop an escaped one. Any other
            // failure (e.g. EPERM racing a setuid exec) takes the same
            // fallback.
        }
    }
    let _ = child.kill();
}

/// The timeout error, keeping whatever transcript the drains had already
/// collected — for a hung updater that output is the diagnostic. The output
/// goes through the same merge/bound shape as every other transcript here
/// (`updater_transcript`), and is omitted entirely when both pipes came up
/// empty rather than showing the "(the updater printed nothing)" placeholder.
fn timeout_message(updater: &Path, stdout: &[u8], stderr: &[u8]) -> String {
    let mut message = format!(
        "`{updater} update` did not finish within {} minutes and was stopped. \
         The update may be incomplete; run `{updater} update` in a shell to retry, \
         then restart Codewhale.",
        UPDATER_TOTAL_TIMEOUT.as_secs() / 60,
        updater = updater.display(),
    );
    let has_output = !String::from_utf8_lossy(stdout).trim().is_empty()
        || !String::from_utf8_lossy(stderr).trim().is_empty();
    if has_output {
        message.push_str("\n\nOutput captured before the timeout:\n");
        message.push_str(&updater_transcript(stdout, stderr));
    }
    message
}

/// Merge the updater's streams into one readable block, bounded so a pathological
/// run cannot flood the transcript.
fn updater_transcript(stdout: &[u8], stderr: &[u8]) -> String {
    const MAX_CHARS: usize = 4_000;
    let mut merged = String::new();
    for stream in [stdout, stderr] {
        let text = String::from_utf8_lossy(stream);
        let text = text.trim();
        if text.is_empty() {
            continue;
        }
        if !merged.is_empty() {
            merged.push('\n');
        }
        merged.push_str(text);
    }
    if merged.is_empty() {
        return "(the updater printed nothing)".to_string();
    }
    if merged.chars().count() > MAX_CHARS {
        let kept: String = merged.chars().take(MAX_CHARS).collect();
        return format!("{kept}\n… output truncated.");
    }
    merged
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_and_explicit_modes_parse() {
        assert_eq!(parse_mode(None), Ok(UpdateMode::Check));
        assert_eq!(parse_mode(Some("  ")), Ok(UpdateMode::Check));
        assert_eq!(parse_mode(Some("Check")), Ok(UpdateMode::Check));
        assert_eq!(parse_mode(Some("install")), Ok(UpdateMode::Install));
        assert_eq!(parse_mode(Some("NOW")), Ok(UpdateMode::Install));
        assert!(parse_mode(Some("--force")).is_err());
    }

    #[test]
    fn a_package_managed_install_gets_its_managers_command_not_an_updater_run() {
        for method in [
            InstallMethod::Npm,
            InstallMethod::Homebrew,
            InstallMethod::Cargo,
            InstallMethod::Omarchy,
        ] {
            let plan = resolve_updater(
                Some(Path::new("/opt/whatever/codewhale")),
                method,
                &|_: &Path| true,
            );
            assert_eq!(plan, UpdaterPlan::Managed(method), "{method:?}");

            let message = managed_install_message(method);
            assert!(message.contains(codewhale_release::install::GITHUB_MIGRATION_HELP));
            assert!(message.contains("mktemp -d"));
            assert!(
                message.contains(method.update_command()),
                "{method:?} message must name its own update command: {message}"
            );
            assert!(
                message.contains("restart Codewhale"),
                "{method:?} message must tell the user to restart: {message}"
            );
        }
    }

    #[test]
    fn a_tui_only_build_without_a_sibling_cli_falls_back_to_instructions() {
        let exe = Path::new("/usr/local/bin/codewhale-tui");
        let plan = resolve_updater(Some(exe), InstallMethod::Binary, &|_: &Path| false);
        assert_eq!(
            plan,
            UpdaterPlan::NoUpdater {
                exe: exe.to_path_buf()
            }
        );

        let message = no_updater_message(exe);
        assert!(message.contains("codewhale update"), "{message}");
        assert!(message.contains("restart Codewhale"), "{message}");
        assert!(message.contains("codewhale-tui"), "{message}");
    }

    #[test]
    fn a_tui_build_next_to_the_cli_runs_the_sibling_updater() {
        let plan = resolve_updater(
            Some(Path::new("/usr/local/bin/codewhale-tui")),
            InstallMethod::Binary,
            &|path: &Path| path == Path::new("/usr/local/bin/codewhale"),
        );
        assert_eq!(
            plan,
            UpdaterPlan::Run(PathBuf::from("/usr/local/bin/codewhale"))
        );
    }

    #[test]
    fn a_cli_install_runs_itself_including_the_windows_extension() {
        assert_eq!(
            resolve_updater(
                Some(Path::new("/usr/local/bin/codewhale")),
                InstallMethod::Binary,
                &|_: &Path| false
            ),
            UpdaterPlan::Run(PathBuf::from("/usr/local/bin/codewhale"))
        );
        // Forward slashes so the case is meaningful on the host running the
        // test: `\` is a plain filename character to a Unix `Path`, which
        // would make this assert about nothing.
        assert_eq!(
            resolve_updater(
                Some(Path::new("C:/tools/codewhale.exe")),
                InstallMethod::Binary,
                &|_: &Path| false
            ),
            UpdaterPlan::Run(PathBuf::from("C:/tools/codewhale.exe"))
        );
    }

    #[test]
    fn standalone_codew_uses_its_own_updater_without_a_companion() {
        for exe in ["/usr/local/bin/codew", "C:/tools/codew.exe"] {
            let exe = Path::new(exe);
            assert_eq!(
                resolve_updater(Some(exe), InstallMethod::Binary, &|_| {
                    panic!("the full codew CLI must not look for another installation")
                }),
                UpdaterPlan::Run(exe.to_path_buf())
            );
        }
    }

    #[test]
    fn a_windows_tui_build_finds_the_sibling_cli_with_its_extension() {
        assert_eq!(
            resolve_updater(
                Some(Path::new("C:/tools/codewhale-tui.exe")),
                InstallMethod::Binary,
                &|path: &Path| path == Path::new("C:/tools/codewhale.exe"),
            ),
            UpdaterPlan::Run(PathBuf::from("C:/tools/codewhale.exe"))
        );
    }

    #[test]
    fn an_unresolvable_executable_reports_no_updater_instead_of_guessing() {
        assert_eq!(
            resolve_updater(None, InstallMethod::Binary, &|_: &Path| true),
            UpdaterPlan::NoUpdater {
                exe: PathBuf::from("codewhale")
            }
        );
    }

    #[test]
    fn the_check_preamble_states_what_install_would_do() {
        let preamble = install_preamble(Path::new("/usr/local/bin/codewhale"));
        assert!(
            preamble.contains("/usr/local/bin/codewhale update"),
            "{preamble}"
        );
        assert!(preamble.contains("SHA256"), "{preamble}");
        assert!(preamble.contains("does not restart"), "{preamble}");
    }

    #[test]
    fn updater_output_is_merged_and_bounded() {
        assert_eq!(updater_transcript(b" out ", b""), "out");
        assert_eq!(updater_transcript(b"out", b"err"), "out\nerr");
        assert_eq!(
            updater_transcript(b"", b"  "),
            "(the updater printed nothing)"
        );

        let flood = "x".repeat(9_000);
        let bounded = updater_transcript(flood.as_bytes(), b"");
        assert!(bounded.ends_with("… output truncated."));
        assert!(bounded.chars().count() < flood.chars().count());
    }

    #[test]
    fn handler_is_pure_and_argument_only() {
        assert!(matches!(UpdateCmd::handler(), CommandHandler::Pure(_)));
        assert_eq!(UpdateCmd::info().description_key, "cmd_update_description");
        assert_eq!(UpdateCmd::info().aliases, &["upgrade"]);
    }

    #[test]
    fn the_timeout_error_keeps_the_partial_transcript() {
        let message = timeout_message(
            Path::new("/usr/local/bin/codewhale"),
            b"resolving the latest release",
            b"",
        );
        assert!(
            message.contains("did not finish within 15 minutes and was stopped"),
            "{message}"
        );
        assert!(
            message.contains("The update may be incomplete"),
            "{message}"
        );
        assert!(message.contains("in a shell to retry"), "{message}");
        assert!(message.contains("restart Codewhale"), "{message}");
        assert!(
            message.contains("Output captured before the timeout"),
            "{message}"
        );
        assert!(
            message.contains("resolving the latest release"),
            "{message}"
        );

        // Whitespace-only output counts as nothing: the partial section and
        // the success-path "(the updater printed nothing)" placeholder must
        // both stay out of the timeout message.
        let silent = timeout_message(Path::new("/usr/local/bin/codewhale"), b"  \n", b"");
        assert!(!silent.contains("Output captured"), "{silent}");
        assert!(!silent.contains("printed nothing"), "{silent}");
    }

    #[test]
    fn a_drain_that_misses_its_deadline_leaves_the_partial_output() {
        // The sender stays alive and never signals: the drain never finishes,
        // which is exactly the wedged-`join()` case this bound exists for.
        let (tx, rx) = mpsc::channel();
        let started = std::time::Instant::now();
        let (stdout, stderr) = collect_drained(
            &rx,
            &Mutex::new(b"partial stdout".to_vec()),
            &Mutex::new(Vec::new()),
            std::time::Duration::from_millis(100),
        );
        assert_eq!(stdout, b"partial stdout");
        assert!(stderr.is_empty());
        assert!(
            started.elapsed() < std::time::Duration::from_secs(5),
            "the deadline must cut the wait instead of blocking on the missing drain"
        );
        drop(tx);
    }

    #[test]
    fn completed_drains_hand_back_their_buffers() {
        let (tx, rx) = mpsc::channel();
        tx.send(()).expect("signal the stdout drain");
        tx.send(()).expect("signal the stderr drain");
        drop(tx);
        let (stdout, stderr) = collect_drained(
            &rx,
            &Mutex::new(b"out".to_vec()),
            &Mutex::new(b"err".to_vec()),
            std::time::Duration::from_secs(2),
        );
        assert_eq!(stdout, b"out");
        assert_eq!(stderr, b"err");
    }

    #[test]
    fn a_drain_reads_its_pipe_into_the_shared_buffer_then_signals() {
        let (tx, rx) = mpsc::channel();
        let buffer = Arc::new(Mutex::new(Vec::new()));
        spawn_drain(
            Some(std::io::Cursor::new(b"pipe bytes".to_vec())),
            Arc::clone(&buffer),
            tx,
        );
        assert_eq!(rx.recv_timeout(std::time::Duration::from_secs(2)), Ok(()));
        assert_eq!(*buffer.lock().expect("unpoisoned"), b"pipe bytes");
    }

    #[cfg(unix)]
    #[test]
    fn the_timeout_kill_takes_the_updater_tree_not_just_the_child() {
        use std::os::unix::process::CommandExt as _;

        // `sh` forks its command instead of exec'ing it, so a child-only
        // kill would leave the backgrounded sleep behind — the same leak
        // the tool runner's group kill exists for.
        let mut child = Command::new("sh")
            .arg("-c")
            .arg("sleep 60 & wait")
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .process_group(0)
            .spawn()
            .expect("spawn a fake updater tree");
        std::thread::sleep(std::time::Duration::from_millis(300));
        let pgid = libc::pid_t::try_from(child.id()).expect("pgid");

        kill_updater_tree(&mut child);
        child.wait().expect("reap the child");
        std::thread::sleep(std::time::Duration::from_millis(300));

        // The group existence probe succeeds only while a member is alive;
        // after the group kill it must report there is no such group.
        let group_alive = unsafe { libc::kill(-pgid, 0) } == 0;
        assert!(
            !group_alive,
            "something in the updater process group outlived the timeout kill"
        );
    }

    #[cfg(unix)]
    #[test]
    fn the_timeout_path_harvests_partial_output_through_the_group_kill() {
        use std::os::unix::process::CommandExt as _;
        use std::process::Stdio;

        // The fake updater prints, then hangs; the group kill must leave the
        // pipes readable so the drain can hand back what was printed before
        // the timeout instead of discarding it.
        let (tx, rx) = mpsc::channel();
        let stdout_buf = Arc::new(Mutex::new(Vec::new()));
        let mut child = Command::new("sh")
            .arg("-c")
            .arg("echo before-hang; sleep 60")
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .process_group(0)
            .spawn()
            .expect("spawn a hanging fake updater");
        spawn_drain(child.stdout.take(), Arc::clone(&stdout_buf), tx);
        std::thread::sleep(std::time::Duration::from_millis(300));

        kill_updater_tree(&mut child);
        child.wait().expect("reap the child");
        let (stdout, stderr) = collect_drained(
            &rx,
            &stdout_buf,
            &Mutex::new(Vec::new()),
            std::time::Duration::from_secs(2),
        );

        assert!(stderr.is_empty());
        assert!(
            String::from_utf8_lossy(&stdout).contains("before-hang"),
            "output printed before the timeout must survive the kill: {stdout:?}"
        );
    }
}
