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
//!   far is returned with a note on stderr. Aborting drops our read ends,
//!   which hands the grandchild an EPIPE on its next write;
//! * the timeout path kills the child explicitly and reaps it before
//!   returning, so there is no window where the tool has failed but the
//!   interpreter is still running, and the kill is directly assertable in
//!   tests.
//!
//! `kill_on_drop(true)` remains set as the cancel-path backstop: when the
//! whole tool future is dropped (turn interrupt), the owned child handle
//! drops with it and the child is killed.

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

/// Run a pre-configured command under `budget`, capturing stdout/stderr.
///
/// When `stdin_input` is `Some`, stdin is piped and the bytes are written by
/// a background task (the plugin tools feed the script its JSON input this
/// way); otherwise stdin is left exactly as the caller configured it. The
/// command must already carry its arguments, environment, and working
/// directory.
pub(crate) async fn run_bounded_child(
    cmd: &mut tokio::process::Command,
    stdin_input: Option<Vec<u8>>,
    budget: Duration,
    label: &str,
) -> Result<std::process::Output, ToolError> {
    // kill_on_drop is the cancel-path backstop: when the tool future is
    // dropped the owned child handle goes with it, which kills the child.
    // The timeout path below kills explicitly regardless, so the kill and
    // reap are synchronous and testable rather than best-effort.
    cmd.kill_on_drop(true);
    cmd.stdout(Stdio::piped());
    cmd.stderr(Stdio::piped());
    if stdin_input.is_some() {
        cmd.stdin(Stdio::piped());
    }

    let mut child = cmd
        .spawn()
        .map_err(|e| ToolError::execution_failed(format!("failed to spawn {label}: {e}")))?;

    let mut stdin_writer = match (child.stdin.take(), stdin_input) {
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
    let mut stdout_task = tokio::spawn(drain_pipe(stdout_pipe, Arc::clone(&stdout_buf)));
    let mut stderr_task = tokio::spawn(drain_pipe(stderr_pipe, Arc::clone(&stderr_buf)));

    let output = match tokio::time::timeout(budget, child.wait()).await {
        Ok(status) => {
            let status =
                status.map_err(|e| ToolError::execution_failed(format!("{label}: {e}")))?;
            // The child closed its write ends; EOF should already have
            // arrived. The grace only bounds the grandchild case, where the
            // write ends live on in an inherited copy and read_to_end would
            // otherwise wait for that process to exit.
            let drained = tokio::time::timeout(CHILD_PIPE_DRAIN_GRACE, async {
                let _ = tokio::join!(&mut stdout_task, &mut stderr_task);
                if let Some(writer) = stdin_writer.as_mut() {
                    let _ = writer.await;
                }
            })
            .await;
            if drained.is_err() {
                // Abort (don't join) the drain tasks: joining would wait for
                // EOF that only the grandchild can deliver. Aborting drops
                // our read ends — the grandchild sees EPIPE on its next
                // write — and the shared buffers keep what was captured.
                stdout_task.abort();
                stderr_task.abort();
                if let Some(writer) = &stdin_writer {
                    writer.abort();
                }
                let mut stderr = snapshot(&stderr_buf);
                if stderr.last() != Some(&b'\n') && !stderr.is_empty() {
                    stderr.push(b'\n');
                }
                stderr.extend_from_slice(DRAIN_TRUNCATED_NOTE);
                std::process::Output {
                    status,
                    stdout: snapshot(&stdout_buf),
                    stderr,
                }
            } else {
                std::process::Output {
                    status,
                    stdout: snapshot(&stdout_buf),
                    stderr: snapshot(&stderr_buf),
                }
            }
        }
        Err(_elapsed) => {
            let _ = child.kill().await;
            let _ = child.wait().await;
            // Abort (don't join) the drain tasks and the stdin writer: a
            // grandchild that inherited the pipes keeps the write ends open
            // after the child dies, so read_to_end would never see EOF and
            // joining here would hang the caller past the budget.
            stdout_task.abort();
            stderr_task.abort();
            if let Some(writer) = &stdin_writer {
                writer.abort();
            }
            return Err(ToolError::Timeout {
                seconds: budget.as_secs(),
            });
        }
    };

    Ok(output)
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
    use super::*;

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
}
