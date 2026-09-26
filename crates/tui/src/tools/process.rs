//! Shared bounded child-process runner for the interpreted-script tools
//! (`js_execution`, `code_execution`, plugin scripts).
//!
//! One shape for "run a child under a wall-clock budget, capture its output,
//! and never let pipes or grandchildren outlive the call":
//!
//! * stdout/stderr are piped explicitly and drained concurrently with the
//!   wait — a child blocked on a full pipe buffer still exits, so a timeout
//!   is a real kill rather than a deadlock;
//! * the budget bounds the child only. Once it exits, the drains get a short
//!   grace to see EOF; if a grandchild that inherited the write ends keeps
//!   them open past the grace, the drains are aborted and the output read so
//!   far is returned with a note on stderr. On Unix, aborting drops our read
//!   ends and hands the grandchild an EPIPE on its next write; on Windows
//!   the aborted read lingers on a blocking-pool thread until the
//!   grandchild closes the pipe, so the grandchild is not signalled — the
//!   lingering read ends when the grandchild exits, which bounds the leak
//!   by the grandchild's own lifetime;
//! * on Unix each child is spawned in its own process group
//!   (`process_group(0)`), and the timeout SIGKILLs the whole group: the
//!   shell tools fork their trailing commands instead of exec'ing them, so
//!   killing only the direct child would orphan a grandchild (the observed
//!   leak was a live 60s sleep left behind by every timed-out gate run).
//!   The kill is followed by a synchronous reap, so there is no window
//!   where the tool has failed but the interpreter is still running, and
//!   the kill is directly assertable in tests. A grandchild that escaped
//!   the group (`setsid`, or the Windows kill which covers the child only)
//!   is bounded by the drain grace as before; the group is never targeted
//!   speculatively on the success paths, so an intentionally-detached
//!   background process that closed the pipes is left alone;
//! * the timeout kill can still harvest the pipes: every group writer is
//!   dead, EOF is immediate, and whatever the interpreter printed before
//!   the kill is returned with the output instead of being discarded.
//!
//! `kill_on_drop(true)` remains set as the cancel-path backstop: when the
//! whole tool future is dropped (turn interrupt), the owned child handle
//! drops with it and the child is killed. It kills the direct child only —
//! a process group cannot be signalled by a drop — so on that one path a
//! forked grandchild is still bounded by the drain grace/EPIPE as before.
//! The drain tasks need their own backstop, because dropping a `JoinHandle`
//! detaches rather than aborts — `DrainTasks` owns them and aborts in
//! `Drop`, so the cancel path releases the pipe read ends like every other
//! exit.

use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::Value;

use super::spec::ToolError;

/// Grace granted to the pipe drains after the child has exited. A child that
/// exits normally closes its own write ends, so EOF arrives immediately;
/// only a pipe inherited by a still-running grandchild can hold it, and that
/// must not hold the tool call past a short bound.
const CHILD_PIPE_DRAIN_GRACE: Duration = Duration::from_secs(2);

/// Appended to stderr when the post-exit drain grace expired. The captured
/// output may be truncated (a grandchild still holds the pipes), and a
/// caller that only surfaces stdout on success — the plugin tools parse
/// stdout as their result — must be able to detect the truncation instead of
/// reporting a silently cut output as a success.
pub(crate) const DRAIN_TRUNCATED_NOTE: &[u8] =
    b"[codewhale] output pipes did not close after the interpreter exited \
      (inherited by a still-running grandchild?); returning the output \
      captured before the drain grace expired\n";

/// True when `output` carries the drain-truncation note.
pub(crate) fn drain_truncated(output: &std::process::Output) -> bool {
    output
        .stderr
        .windows(DRAIN_TRUNCATED_NOTE.len())
        .any(|window| window == DRAIN_TRUNCATED_NOTE)
}

/// How a bounded run ended. `timed_out` carries the same `output` shape as
/// a completed run — the group was killed and reaped, so `status` is the
/// kill status (no exit code on Unix) and the buffers hold whatever the
/// pipes yielded before the drains were cut. Callers that surface partial
/// output on a timeout (the gate log) want this distinction; callers that
/// report only "it timed out" use [`run_bounded_child`].
pub(crate) struct BoundedOutcome {
    pub output: std::process::Output,
    /// The budget elapsed and the run was killed for it.
    pub timed_out: bool,
}

/// Run a pre-configured command under `budget`, reporting whether it was
/// killed for exceeding the budget. Semantics are [`run_bounded_child`]'s;
/// see that function for the argument contract.
pub(crate) async fn run_bounded_child_observed(
    cmd: &mut tokio::process::Command,
    stdin_input: Option<Vec<u8>>,
    budget: Duration,
    label: &str,
) -> Result<BoundedOutcome, ToolError> {
    // kill_on_drop is the cancel-path backstop: when the tool future is
    // dropped the owned child handle goes with it, which kills the child.
    // The timeout path below kills the group explicitly and reaps, so the
    // kill is synchronous and testable rather than best-effort.
    cmd.kill_on_drop(true);
    // A private group per child, so a timeout kill can take the whole
    // tree out (shells fork their trailing commands instead of exec'ing
    // them) and stray terminal signals can't reach a tool interpreter.
    #[cfg(unix)]
    cmd.process_group(0);
    cmd.stdout(Stdio::piped());
    cmd.stderr(Stdio::piped());
    if stdin_input.is_some() {
        cmd.stdin(Stdio::piped());
    }

    let mut child = cmd
        .spawn()
        .map_err(|e| ToolError::execution_failed(format!("failed to spawn {label}: {e}")))?;

    let stdin_writer = match (child.stdin.take(), stdin_input) {
        (Some(mut stdin), Some(input_bytes)) => {
            use tokio::io::AsyncWriteExt as _;
            Some(tokio::spawn(async move {
                if stdin.write_all(&input_bytes).await.is_ok() {
                    let _ = stdin.shutdown().await;
                }
            }))
        }
        _ => None,
    };

    let stdout_pipe = child.stdout.take();
    let stderr_pipe = child.stderr.take();
    // Drain concurrently with the wait: a child blocked on a full pipe
    // buffer would otherwise never exit, turning every timeout into a
    // guaranteed kill. The buffers are shared so a grace expiry below can
    // still return what was captured instead of losing it with the task.
    let stdout_buf = Arc::new(Mutex::new(Vec::new()));
    let stderr_buf = Arc::new(Mutex::new(Vec::new()));
    let mut drains = DrainTasks {
        stdout: tokio::spawn(drain_pipe(stdout_pipe, Arc::clone(&stdout_buf))),
        stderr: tokio::spawn(drain_pipe(stderr_pipe, Arc::clone(&stderr_buf))),
        stdin: stdin_writer,
    };

    let (status, timed_out) = match tokio::time::timeout(budget, child.wait()).await {
        Ok(status) => {
            let status = match status {
                Ok(status) => status,
                Err(e) => {
                    // The one exit that isn't a timeout or a clean exit.
                    // `drains` aborts on drop, so this needs no explicit
                    // call; returning is enough.
                    return Err(ToolError::execution_failed(format!("{label}: {e}")));
                }
            };
            (status, false)
        }
        Err(_elapsed) => {
            // The budget elapsed. Kill the whole process group, not just
            // the child: the shell tools fork their trailing commands
            // instead of exec'ing them, so a child-only kill orphans every
            // grandchild. Then reap, and let the pipes deliver the partial
            // output instead of discarding it.
            kill_the_run(&mut child).await;
            let status = match child.wait().await {
                Ok(status) => status,
                Err(e) => {
                    return Err(ToolError::execution_failed(format!("{label}: {e}")));
                }
            };
            (status, true)
        }
    };
    let output = collect_pipes_after_exit(status, &mut drains, &stdout_buf, &stderr_buf).await;
    Ok(BoundedOutcome { output, timed_out })
}

/// Run a pre-configured command under `budget`, capturing stdout/stderr.
///
/// When `stdin_input` is `Some`, stdin is piped and the bytes are written by
/// a background task (the plugin tools feed the script its JSON input this
/// way); otherwise stdin is left exactly as the caller configured it. The
/// command must already carry its arguments, environment, and working
/// directory.
///
/// A run that exceeds the budget is killed (see the module doc) and
/// reported as [`ToolError::Timeout`]; its partial output is dropped.
/// Callers that want the partial output report via
/// [`run_bounded_child_observed`] instead.
pub(crate) async fn run_bounded_child(
    cmd: &mut tokio::process::Command,
    stdin_input: Option<Vec<u8>>,
    budget: Duration,
    label: &str,
) -> Result<std::process::Output, ToolError> {
    let observed = run_bounded_child_observed(cmd, stdin_input, budget, label).await?;
    if observed.timed_out {
        return Err(ToolError::Timeout {
            seconds: budget.as_secs(),
        });
    }
    Ok(observed.output)
}

/// Kill the run the budget expired on. On Unix this SIGKILLs the child's
/// process group — the child is spawned with `process_group(0)`, so
/// `-pgid` reaches it together with everything it forked that stayed in
/// the group, and never our own group. When there is no group to signal
/// (Windows, or the group is already gone) this kills the direct child,
/// and a pipe-inheriting grandchild is then bounded by the drain grace.
async fn kill_the_run(child: &mut tokio::process::Child) {
    #[cfg(unix)]
    {
        let pgid = libc::pid_t::try_from(child.id().unwrap_or(0)).unwrap_or(0);
        if pgid > 0 {
            // SAFETY: a negative pid targets the process group led by the
            // child; the pgid came from `process_group(0)` at spawn, so this
            // group cannot be Codewhale's own or the terminal's.
            let killed = unsafe { libc::kill(-pgid, libc::SIGKILL) };
            if killed == 0 {
                return;
            }
            // ESRCH: no live process in the group, so the child is dead
            // too (a live child would be in the group). Nothing to kill.
            // Any other failure (e.g. EPERM racing a setuid exec) falls
            // through to the child-only kill as the safe fallback.
            if std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH) {
                return;
            }
        }
    }
    let _ = child.kill().await;
}

/// Collect the pipes once the child is gone. The child closed its write
/// ends on a clean exit, so EOF should already have arrived; the grace
/// only bounds a writer that outlived the child (a grandchild that
/// inherited the pipe ends, or on the timeout path one that escaped the
/// process group). If the grace expires, the readers are aborted —
/// dropping our read ends hands an escaped grandchild EPIPE on its next
/// write — the shared buffers keep what was captured, and stderr gains
/// the truncation note.
async fn collect_pipes_after_exit(
    status: std::process::ExitStatus,
    drains: &mut DrainTasks,
    stdout_buf: &Arc<Mutex<Vec<u8>>>,
    stderr_buf: &Arc<Mutex<Vec<u8>>>,
) -> std::process::Output {
    let drained = tokio::time::timeout(CHILD_PIPE_DRAIN_GRACE, drains.join()).await;
    if drained.is_err() {
        // Abort (don't join) the drain tasks: joining would wait for EOF
        // that only a still-running writer can deliver. Explicit rather
        // than left to the guard so the abort is ordered before the
        // buffer snapshots below.
        drains.abort();
        let stdout = snapshot(stdout_buf);
        let mut stderr = snapshot(stderr_buf);
        if stderr.last() != Some(&b'\n') && !stderr.is_empty() {
            stderr.push(b'\n');
        }
        stderr.extend_from_slice(DRAIN_TRUNCATED_NOTE);
        std::process::Output {
            status,
            stdout,
            stderr,
        }
    } else {
        std::process::Output {
            status,
            stdout: snapshot(stdout_buf),
            stderr: snapshot(stderr_buf),
        }
    }
}

/// Owns the drain tasks and the stdin writer so that *every* exit aborts
/// them — including the one no function call can cover. Dropping a
/// [`tokio::task::JoinHandle`] detaches its task rather than aborting it, so
/// when the tool future is dropped on a turn interrupt the readers would
/// otherwise keep the pipe read ends open for as long as a pipe-inheriting
/// grandchild holds the write ends, which is exactly the leak
/// [`run_bounded_child`]'s module doc describes. A `Drop` impl is the only
/// construction that covers the cancel path; the explicit [`Self::abort`]
/// exists so the grace-expiry exit can order the abort before it snapshots
/// the buffers.
struct DrainTasks {
    stdout: tokio::task::JoinHandle<()>,
    stderr: tokio::task::JoinHandle<()>,
    stdin: Option<tokio::task::JoinHandle<()>>,
}

impl DrainTasks {
    fn abort(&self) {
        self.stdout.abort();
        self.stderr.abort();
        if let Some(writer) = self.stdin.as_ref() {
            writer.abort();
        }
    }

    /// Wait for both readers to see EOF, then for the stdin writer. Only
    /// safe under a timeout: EOF may be owed by a grandchild that outlives
    /// the child.
    async fn join(&mut self) {
        let _ = tokio::join!(&mut self.stdout, &mut self.stderr);
        if let Some(writer) = self.stdin.as_mut() {
            let _ = writer.await;
        }
    }
}

impl Drop for DrainTasks {
    fn drop(&mut self) {
        self.abort();
    }
}

async fn drain_pipe(pipe: Option<impl tokio::io::AsyncRead + Unpin>, buf: Arc<Mutex<Vec<u8>>>) {
    let Some(mut pipe) = pipe else {
        return;
    };
    use tokio::io::AsyncReadExt;
    let mut chunk = [0u8; 8192];
    loop {
        match pipe.read(&mut chunk).await {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                if let Ok(mut buf) = buf.lock() {
                    buf.extend_from_slice(&chunk[..n]);
                }
            }
        }
    }
}

fn snapshot(buf: &Arc<Mutex<Vec<u8>>>) -> Vec<u8> {
    buf.lock().map(|buf| buf.clone()).unwrap_or_default()
}

/// Convenience wrapper mirroring the previous per-tool helpers: serialize
/// `input` and feed it to the child on stdin.
pub(crate) fn stdin_json(input: &Value) -> Result<Vec<u8>, ToolError> {
    serde_json::to_vec(input)
        .map_err(|e| ToolError::invalid_input(format!("failed to serialize input: {e}")))
}

#[cfg(test)]
mod tests {
    #[cfg(unix)]
    use super::*;

    #[tokio::test]
    async fn dropping_the_drain_guard_aborts_the_readers() {
        // The cancel path (turn interrupt) drops the whole tool future
        // rather than taking any exit inside `run_bounded_child`, and
        // dropping a `JoinHandle` only detaches its task. Without the `Drop`
        // impl the readers would keep the pipe read ends open for as long as
        // a pipe-inheriting grandchild holds the write ends, so pin that a
        // dropped guard really does abort them.
        use std::sync::Arc;
        use std::time::Duration;

        let held = Arc::new(());
        let make = |held: Arc<()>| {
            tokio::spawn(async move {
                let _held = held;
                // Never completes on its own; only an abort ends this.
                std::future::pending::<()>().await;
            })
        };
        let guard = super::DrainTasks {
            stdout: make(Arc::clone(&held)),
            stderr: make(Arc::clone(&held)),
            stdin: Some(make(Arc::clone(&held))),
        };
        tokio::task::yield_now().await;
        assert_eq!(
            Arc::strong_count(&held),
            4,
            "all three readers must be live before the drop"
        );

        drop(guard);
        // Aborted tasks release their captured state once the runtime
        // reaps them.
        for _ in 0..100 {
            if Arc::strong_count(&held) == 1 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(
            Arc::strong_count(&held),
            1,
            "dropping the guard must abort every reader, not detach it"
        );
    }

    #[cfg(unix)]
    fn shell_command(script: &str) -> tokio::process::Command {
        let mut cmd = tokio::process::Command::new("sh");
        cmd.arg("-c").arg(script);
        cmd
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn bounded_child_returns_promptly_when_a_grandchild_holds_the_pipes() {
        // The direct child exits immediately; the backgrounded sleep
        // inherits both pipes and holds them for 30s. The call must still
        // return promptly with the output captured before the grace, not
        // wait for the grandchild.
        let started = std::time::Instant::now();
        let mut cmd = shell_command("echo grandchild-holds-pipes; sleep 30 &");
        let output = run_bounded_child(&mut cmd, None, Duration::from_secs(600), "sh")
            .await
            .expect("child must succeed");
        let elapsed = started.elapsed();
        assert!(output.status.success());
        assert!(
            String::from_utf8_lossy(&output.stdout).contains("grandchild-holds-pipes"),
            "output captured before the grace must survive: {:?}",
            output.stdout
        );
        assert!(
            String::from_utf8_lossy(&output.stderr)
                .contains("output pipes did not close after the interpreter exited"),
            "the truncation note must explain the early return: {:?}",
            output.stderr
        );
        assert!(
            elapsed < Duration::from_secs(15),
            "a pipe-holding grandchild must not hold the call past the grace; took {elapsed:?}"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn bounded_child_still_times_out_and_kills_a_hung_child() {
        let mut cmd = shell_command("echo started; sleep 60");
        let err = run_bounded_child(&mut cmd, None, Duration::from_secs(2), "sh")
            .await
            .expect_err("a 60s sleep must hit the budget");
        assert!(matches!(err, ToolError::Timeout { seconds: 2 }));
    }

    #[cfg(unix)]
    fn pid_is_gone(pid: libc::pid_t) -> bool {
        // kill(pid, 0) stays 0 for a zombie, and a group-killed grandchild
        // is reparented to init when its parent dies first; retry briefly
        // so reaping races don't flake the assertion.
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
        loop {
            if unsafe { libc::kill(pid, 0) } != 0
                && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
            {
                return true;
            }
            if std::time::Instant::now() >= deadline {
                return false;
            }
            std::thread::sleep(Duration::from_millis(25));
        }
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_timeout_kill_takes_out_the_whole_process_group_not_just_the_child() {
        // The observed leak: a timed-out run killed only the direct child,
        // so `sh -c "...; sleep 60 & ..."` orphaned the backgrounded sleep
        // for its full minute. Pin that the grandchild itself is dead once
        // the call reports the timeout — the whole point of process_group.
        let tmp = tempfile::tempdir().expect("tempdir");
        let child_pid_file = tmp.path().join("child.pid");
        let grandchild_pid_file = tmp.path().join("grandchild.pid");
        let script = format!(
            "echo $$ > {}; sleep 30 & echo $! > {}; sleep 30",
            child_pid_file.display(),
            grandchild_pid_file.display(),
        );
        let mut cmd = shell_command(&script);
        let err = run_bounded_child(&mut cmd, None, Duration::from_secs(2), "sh")
            .await
            .expect_err("the 30s sleeps must hit the budget");
        assert!(matches!(err, ToolError::Timeout { seconds: 2 }));

        let child: libc::pid_t = std::fs::read_to_string(&child_pid_file)
            .expect("child pid")
            .trim()
            .parse()
            .expect("child pid integer");
        let grandchild: libc::pid_t = std::fs::read_to_string(&grandchild_pid_file)
            .expect("grandchild pid")
            .trim()
            .parse()
            .expect("grandchild pid integer");
        assert!(
            pid_is_gone(child),
            "child {child} survived the timeout kill"
        );
        assert!(
            pid_is_gone(grandchild),
            "grandchild {grandchild} outlived the group kill: the original leak"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_completed_run_harvests_partial_output_through_the_group_kill() {
        // The child prints, then hangs; the runner kills the group and a
        // synchronous reap must leave the pipes readable. The marker proves
        // the kill did not discard the interpreter's earlier output.
        let mut cmd = shell_command("echo before-hang; sleep 30");
        let outcome = run_bounded_child_observed(&mut cmd, None, Duration::from_secs(2), "sh")
            .await
            .expect("observed run");
        assert!(outcome.timed_out, "the 30s sleep must trip the budget");
        assert!(
            String::from_utf8_lossy(&outcome.output.stdout).contains("before-hang"),
            "output printed before the timeout must survive the kill: {:?}",
            outcome.output.stdout
        );
        assert!(
            outcome.output.status.code().is_none(),
            "a SIGKILLed child has no exit code on Unix"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_detached_success_run_leaves_a_background_process_alive() {
        // The group is only signalled on the timeout path; a run that
        // exits cleanly must not take an intentionally-detached `&`
        // process with it. Pin that the group is never signalled
        // speculatively.
        let tmp = tempfile::tempdir().expect("tempdir");
        let detached_pid_file = tmp.path().join("detached.pid");
        // $! is the backgrounded sleep; $$ would be the shell, which
        // legitimately exits with the run.
        let script = format!("sleep 30 & echo $! > {}", detached_pid_file.display());
        let mut cmd = shell_command(&script);
        let outcome = run_bounded_child_observed(&mut cmd, None, Duration::from_secs(20), "sh")
            .await
            .expect("observed run");
        assert!(!outcome.timed_out);
        assert!(outcome.output.status.success());

        let detached: libc::pid_t = std::fs::read_to_string(&detached_pid_file)
            .expect("detached pid")
            .trim()
            .parse()
            .expect("detached pid integer");
        let alive = unsafe { libc::kill(detached, 0) } == 0;
        assert!(
            alive,
            "the backgrounded sleep {detached} died without a group signal"
        );
        unsafe {
            libc::kill(detached, libc::SIGKILL);
        }
        assert!(pid_is_gone(detached), "cleanup");
    }
}
