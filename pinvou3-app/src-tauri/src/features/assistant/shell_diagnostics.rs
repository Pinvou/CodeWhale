//! Content-free projection of existing Engine-owned shell jobs.
//! This observer never executes, consumes or reassigns a job/completion receipt.
use deepseek_tui::tools::shell::{SharedShellManager, ShellJobSnapshot, ShellStatus};
use serde_json::{Value, json};

pub(crate) fn is_shell_execution_tool(name: &str) -> bool {
    matches!(name, "bash" | "exec_shell" | "task_shell_start" | "Bash")
}

/// The production completion projection, shared with the real Engine fixture.
/// Explicit metadata wins; only a failed shell without metadata may be observed.
pub(crate) fn tool_completion_metadata(
    manager: &SharedShellManager,
    session: &str,
    turn: &str,
    call: &str,
    tool: &str,
    success: bool,
    metadata: Option<&Value>,
) -> Option<Value> {
    metadata.cloned().or_else(|| {
        (!success && is_shell_execution_tool(tool))
            .then(|| failed_shell_metadata(manager, session, turn, call))
            .flatten()
    })
}

pub(crate) fn failed_shell_metadata(
    manager: &SharedShellManager,
    session: &str,
    turn: &str,
    call: &str,
) -> Option<Value> {
    let mut manager = manager.lock().ok()?;
    select_failed_shell_metadata(&manager.list_jobs_for_session(session), session, turn, call)
}

fn select_failed_shell_metadata(
    jobs: &[ShellJobSnapshot],
    session: &str,
    turn: &str,
    call: &str,
) -> Option<Value> {
    if [session, turn, call].iter().any(|id| id.trim().is_empty()) {
        return None;
    }
    let mut matches = jobs.iter().filter(|job| {
        job.owner_session_id == session
            && job.origin_turn_id.as_deref() == Some(turn)
            && job.origin_tool_call_id.as_deref() == Some(call)
            && job.owner_agent_id.is_none()
    });
    let job = matches.next()?;
    // Check uniqueness before filtering status, including running/stale rows.
    if matches.next().is_some() || job.stale || job.status != ShellStatus::Failed {
        return None;
    }
    let exit = job
        .exit_code
        .filter(|exit| *exit != 0 && (i64::from(i32::MIN)..=i64::from(u32::MAX)).contains(exit))?;
    // Killed is not proof of a timeout. Snapshot has no sandbox-denial fact.
    // Never project command, paths, output tails or correlation identities.
    Some(json!({"status":"Failed", "exit_code":exit}))
}

#[cfg(test)]
mod tests {
    use super::*;
    fn job() -> ShellJobSnapshot {
        ShellJobSnapshot {
            id: "fixture_job".into(),
            job_id: "fixture_job".into(),
            command: "PRIVATE_COMMAND".into(),
            cwd: "private_workspace".into(),
            status: ShellStatus::Failed,
            exit_code: Some(7),
            elapsed_ms: 1,
            stdout_tail: "PRIVATE_OUTPUT".into(),
            stderr_tail: "PRIVATE_ERROR".into(),
            stdout_len: 14,
            stderr_len: 13,
            stdin_available: false,
            stale: false,
            elapsed_since_output_ms: None,
            linked_task_id: None,
            owner_agent_id: None,
            owner_agent_name: None,
            origin_tool_call_id: Some("call".into()),
            origin_turn_id: Some("turn".into()),
            owner_session_id: "session".into(),
        }
    }
    #[test]
    fn diagnostic_shell_projection_requires_exact_unique_natural_failure() {
        assert_eq!(
            select_failed_shell_metadata(&[job()], "session", "turn", "call"),
            Some(json!({"status":"Failed","exit_code":7}))
        );
        for identities in [
            ("other", "turn", "call"),
            ("session", "other", "call"),
            ("session", "turn", "other"),
            ("", "turn", "call"),
        ] {
            assert!(
                select_failed_shell_metadata(&[job()], identities.0, identities.1, identities.2)
                    .is_none()
            );
        }
        for kind in 0..6 {
            let mut bad = job();
            match kind {
                0 => bad.owner_agent_id = Some("child".into()),
                1 => bad.stale = true,
                2 => bad.origin_turn_id = None,
                3 => bad.origin_tool_call_id = None,
                4 => bad.status = ShellStatus::Killed,
                _ => bad.exit_code = Some(0),
            }
            assert!(select_failed_shell_metadata(&[bad], "session", "turn", "call").is_none());
        }
        for status in [ShellStatus::Running, ShellStatus::Failed] {
            let mut duplicate = job();
            duplicate.status = status;
            assert!(
                select_failed_shell_metadata(&[job(), duplicate], "session", "turn", "call")
                    .is_none()
            );
        }
        let value = select_failed_shell_metadata(&[job()], "session", "turn", "call").unwrap();
        assert!(!value.to_string().contains("PRIVATE"));
        assert_eq!(value.as_object().unwrap().len(), 2);
    }
}
