//! Command construction: every `gh` and `git` process this tool runs.
//!
//! Nothing above this file assembles an argv or resolves a binary path, so
//! "what did the tool actually shell out to" has exactly one answer.

use std::process::Stdio;
use std::time::Duration;

use serde_json::Value;

use crate::tools::spec::{ToolContext, ToolError};

const DEFAULT_GH: &str = "/opt/homebrew/bin/gh";
const FALLBACK_GH_PATHS: &[&str] = &[
    "/usr/bin/gh",                       // Linux system package manager
    "/usr/local/bin/gh",                 // macOS Intel Homebrew / manual install
    "/home/linuxbrew/.linuxbrew/bin/gh", // Linux Homebrew (official prefix)
    "/opt/homebrew/bin/gh",              // macOS Apple Silicon Homebrew
];

/// Total wall-clock budget for one `gh` invocation. `gh` is network-bound
/// (api.github.com) and can block on credential prompts; previously a hung
/// `gh` ran on the executor thread with no timeout and stalled the turn
/// indefinitely.
const GH_COMMAND_BUDGET: Duration = Duration::from_secs(60);

fn gh_bin() -> String {
    if let Ok(bin) = std::env::var("CODEWHALE_GH_BIN").or_else(|_| std::env::var("DEEPSEEK_GH_BIN"))
    {
        return bin;
    }
    for path in FALLBACK_GH_PATHS {
        if std::path::Path::new(path).is_file() {
            return path.to_string();
        }
    }
    DEFAULT_GH.to_string()
}

pub(super) async fn run_gh_text(context: &ToolContext, args: &[&str]) -> Result<String, ToolError> {
    let bin = gh_bin();
    // `run_bounded_child` reports spawn failures generically; resolve the
    // install hint up front for the common "gh is not installed" case (the
    // fallback paths are absolute). A bare-name env override resolves via
    // PATH at spawn and keeps the generic error.
    if bin.contains('/') && !std::path::Path::new(&bin).is_file() {
        return Err(ToolError::not_available(
            "gh CLI not found; install it or set CODEWHALE_GH_BIN (legacy: DEEPSEEK_GH_BIN)",
        ));
    }
    let mut cmd = tokio::process::Command::new(&bin);
    // `Command::output()` ran the child with a null stdin; keep that
    // semantics so gh never reads (or waits on) our own stdin.
    cmd.stdin(Stdio::null());
    cmd.args(args).current_dir(&context.workspace);
    let out =
        crate::tools::process::run_bounded_child(&mut cmd, None, GH_COMMAND_BUDGET, "gh").await?;
    if !out.status.success() {
        return Err(ToolError::execution_failed(format!(
            "gh {} failed: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        )));
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

pub(super) async fn run_gh_json(context: &ToolContext, args: &[&str]) -> Result<Value, ToolError> {
    let text = run_gh_text(context, args).await?;
    serde_json::from_str(&text).map_err(|e| ToolError::execution_failed(e.to_string()))
}

pub(super) async fn ensure_github_repo(context: &ToolContext) -> Result<(), ToolError> {
    // Bounded like every other git spawn (the git tool's runner: 120 s
    // budget, `GIT_OPTIONAL_LOCKS=0`, null stdin). A hung `git rev-parse` on
    // a huge or NFS-backed worktree previously ran on the executor thread
    // with no timeout — the same hazard this tool's `gh` calls already had.
    let out = super::super::git::run_git_command(
        &context.workspace,
        &["rev-parse".to_string(), "--is-inside-work-tree".to_string()],
    )
    .await?;
    if out.status.success() {
        Ok(())
    } else {
        Err(ToolError::not_available(
            "current workspace is not a git repository",
        ))
    }
}

pub(super) async fn git_status_porcelain(context: &ToolContext) -> Result<String, ToolError> {
    let out = super::super::git::run_git_command(
        &context.workspace,
        &["status".to_string(), "--porcelain".to_string()],
    )
    .await?;
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}
