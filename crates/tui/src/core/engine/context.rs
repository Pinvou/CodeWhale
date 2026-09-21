//! Context budgeting and prompt-shaping helpers for the engine.
//!
//! These functions are shared by the streaming turn loop, capacity flow, and
//! engine session maintenance code. Keeping them here prevents the top-level
//! engine module from accumulating unrelated context-policy details.

use crate::config::ApiProvider;
use crate::context_budget::ContextBudget;
use crate::models::SystemPrompt;
#[cfg(test)]
pub(super) use crate::route_budget::effective_max_output_tokens;
pub(super) use crate::route_budget::effective_max_output_tokens_for_route;
use crate::tools::spec::ToolResult;
use codewhale_config::route::RouteLimits;
use serde_json::Value;
/// Keep this many most recent messages when emergency trimming is required.
pub(super) const MIN_RECENT_MESSAGES_TO_KEEP: usize = 4;
/// Allow a few emergency recovery attempts before failing the turn.
pub(super) const MAX_CONTEXT_RECOVERY_ATTEMPTS: u8 = 2;
/// Emergency-recovery trim target: this fraction of the input budget below
/// the budget itself. Trimming exactly to the budget lands the session a few
/// hundred tokens under the preflight line, so the next turn step's output
/// re-crosses it and recovery runs again — a per-step "trim a few oldest"
/// thrash that eats the transcript from the front and invalidates the provider
/// prefix cache each time. The margin buys several steps of regrowth headroom.
pub(super) const EMERGENCY_TRIM_MARGIN_DIVISOR: usize = 5;

/// Local-trim target for emergency context recovery, strictly below the input
/// budget so one recovery buys regrowth headroom instead of landing on the
/// preflight line.
pub(super) fn emergency_trim_budget(input_budget: usize) -> usize {
    input_budget.saturating_sub(input_budget / EMERGENCY_TRIM_MARGIN_DIVISOR)
}
/// Hard cap for any tool output inserted into model context.
const TOOL_RESULT_CONTEXT_HARD_LIMIT_CHARS: usize = 12_000;
/// Soft cap for known noisy tools inserted into model context.
const TOOL_RESULT_CONTEXT_SOFT_LIMIT_CHARS: usize = 2_000;
/// Snippet length kept when compacting tool output for model context.
const TOOL_RESULT_CONTEXT_SNIPPET_CHARS: usize = 900;
/// Hard cap for tool output inserted into a large-context model.
const LARGE_CONTEXT_TOOL_RESULT_HARD_LIMIT_CHARS: usize = 48_000;
/// Soft cap for known noisy tools inserted into a large-context model.
const LARGE_CONTEXT_TOOL_RESULT_SOFT_LIMIT_CHARS: usize = 8_000;
/// Snippet length kept when compacting large-context noisy output.
const LARGE_CONTEXT_TOOL_RESULT_SNIPPET_CHARS: usize = 4_000;
/// Context window size at which tool output limits can be relaxed.
const LARGE_CONTEXT_WINDOW_TOKENS: u32 = 500_000;
/// Max chars to keep from metadata-provided output summaries.
const TOOL_RESULT_METADATA_SUMMARY_CHARS: usize = 320;

#[cfg(test)]
pub(super) use crate::compaction::COMPACTION_SUMMARY_MARKER;

#[derive(Debug, Clone, Copy)]
struct ToolResultContextLimits {
    hard_limit_chars: usize,
    noisy_soft_limit_chars: usize,
    snippet_chars: usize,
}

pub(super) fn summarize_text(text: &str, limit: usize) -> String {
    if text.chars().count() <= limit {
        return text.to_string();
    }
    let take = limit.saturating_sub(3);
    let mut out: String = text.chars().take(take).collect();
    out.push_str("...");
    out
}

fn summarize_text_head_tail(text: &str, limit: usize) -> String {
    let total = text.chars().count();
    if total <= limit {
        return text.to_string();
    }
    if limit <= 20 {
        return summarize_text(text, limit);
    }

    let marker = "\n\n[... output truncated for context ...]\n\n";
    let marker_len = marker.chars().count();
    if limit <= marker_len + 20 {
        return summarize_text(text, limit);
    }

    let remaining = limit - marker_len;
    let head_len = remaining.saturating_mul(2) / 3;
    let tail_len = remaining.saturating_sub(head_len);
    let head: String = text.chars().take(head_len).collect();
    let tail_vec: Vec<char> = text.chars().rev().take(tail_len).collect();
    let tail: String = tail_vec.into_iter().rev().collect();
    format!("{head}{marker}{tail}")
}

fn tool_result_is_noisy(tool_name: &str) -> bool {
    matches!(
        tool_name,
        "exec_shell"
            | "exec_shell_wait"
            | "exec_shell_interact"
            | "exec_shell_cancel"
            | "task_shell_start"
            | "task_shell_wait"
            | "run_tests"
            | "run_verifiers"
            | "task_gate_run"
            | "multi_tool_use.parallel"
            | "Web"
            | "web_search"
            | "web.run"
            | "fetch_url"
    )
}

fn tool_result_metadata_summary(metadata: Option<&serde_json::Value>) -> Option<String> {
    let obj = metadata?.as_object()?;
    for key in ["summary", "stdout_summary", "stderr_summary", "message"] {
        if let Some(text) = obj.get(key).and_then(serde_json::Value::as_str) {
            let trimmed = text.trim();
            if !trimmed.is_empty() {
                return Some(summarize_text(trimmed, TOOL_RESULT_METADATA_SUMMARY_CHARS));
            }
        }
    }
    None
}

fn summarize_subagent_status(status: &serde_json::Value) -> String {
    if let Some(raw) = status.as_str() {
        return raw.to_string();
    }
    if let Some(obj) = status.as_object()
        && let Some((kind, value)) = obj.iter().next()
    {
        if let Some(reason) = value.as_str().filter(|s| !s.trim().is_empty()) {
            return format!("{kind}({})", summarize_text(reason.trim(), 120));
        }
        return kind.to_string();
    }
    status.to_string()
}

/// The per-row `route:` line prints the child's effective provider/model
/// routing from the optional typed `child_route` receipt (upstream
/// 0c03b5a81 carries it on every compact status row; `resolved_profile_id`
/// is the receipt's connection identity). The emitter bounds the receipt
/// itself, but this renderer must not depend on that: one unbounded route
/// value would eat the per-row budget that keeps the eight-row fleet
/// summary bounded. Every string field is previewed and the assembled line
/// is hard-guarded, so the row stays short whatever a producer sent.
const SUBAGENT_ROUTE_FIELD_PREVIEW_CHARS: usize = 64;
const SUBAGENT_ROUTE_LINE_MAX_CHARS: usize = 200;

/// `None` when the receipt carries nothing printable (missing/empty fields,
/// a non-object non-string value) — the row then shows no route line rather
/// than a placeholder.
fn subagent_route_line(route: &serde_json::Value) -> Option<String> {
    if let Some(object) = route.as_object() {
        let field = |key: &str| -> Option<String> {
            object
                .get(key)
                .and_then(serde_json::Value::as_str)
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(|s| summarize_text(s, SUBAGENT_ROUTE_FIELD_PREVIEW_CHARS))
        };
        let provider = field("provider_id")?;
        let model = field("model_id")?;
        let mut line = format!("  route: {provider}/{model}");
        let source = field("route_source");
        let profile = field("resolved_profile_id");
        match (source, profile) {
            (Some(source), Some(profile)) => {
                line.push_str(&format!(" (source={source}, profile={profile})"));
            }
            (Some(source), None) => line.push_str(&format!(" (source={source})")),
            (None, Some(profile)) => line.push_str(&format!(" (profile={profile})")),
            (None, None) => {}
        }
        // Hard guard: field previews alone do not bound the assembled line.
        return Some(summarize_text(&line, SUBAGENT_ROUTE_LINE_MAX_CHARS));
    }
    // Defensive: a producer may serialize the receipt as a bare
    // "provider/model" label; anything else shapeless is skipped.
    let raw = route.as_str()?.trim();
    if raw.is_empty() {
        return None;
    }
    Some(summarize_text(
        &format!(
            "  route: {}",
            summarize_text(raw, SUBAGENT_ROUTE_FIELD_PREVIEW_CHARS)
        ),
        SUBAGENT_ROUTE_LINE_MAX_CHARS,
    ))
}

fn summarize_subagent_snapshot(
    snapshot: &serde_json::Value,
    index: usize,
    transcript_handle_fallback: Option<&str>,
    child_route_fallback: Option<&serde_json::Value>,
) -> String {
    // Session projections (`SubAgentSessionProjection`) keep `transcript_handle`
    // on the outer envelope while the wrapped result row carries none, so the
    // handle is captured before unwrapping and handed down as a fallback: the
    // visible row prints the value the hint gate saw instead of a phantom.
    // `child_route` mirrors that rule in reverse: compact fleet rows and the
    // compact spawn receipt strip the `snapshot` wrapper and keep the receipt
    // on the envelope, and a legacy projection can predate the wrapped row's
    // own field — the wrapped value wins when both exist.
    let outer_transcript_handle = snapshot
        .get("transcript_handle")
        .and_then(transcript_handle_row_value);
    let outer_child_route = snapshot.get("child_route");
    if let Some(inner) = snapshot.get("snapshot") {
        let fallback = outer_transcript_handle
            .as_deref()
            .or(transcript_handle_fallback);
        return summarize_subagent_snapshot(inner, index, fallback, outer_child_route);
    }

    let Some(obj) = snapshot.as_object() else {
        return format!(
            "- item {index}: {}",
            summarize_text(&snapshot.to_string(), 240)
        );
    };

    let agent_id = obj
        .get("agent_id")
        .and_then(serde_json::Value::as_str)
        .unwrap_or("unknown");
    let agent_type = obj
        .get("agent_type")
        .and_then(serde_json::Value::as_str)
        .unwrap_or("agent");
    let status = obj
        .get("status")
        .map(summarize_subagent_status)
        .unwrap_or_else(|| "unknown".to_string());
    // The hint below names `transcript_handle`, so the summarized rows must
    // carry the value it points at — otherwise the hint would name a value
    // the model never receives (the Pinvou #490 phantom-value class).
    // Producers serialize the field either as a `session_id/name` string or
    // as the full `var_handle` object; both reduce to the readable identity
    // below, and the value is engine-generated and never truncated.
    let transcript_handle = obj
        .get("transcript_handle")
        .and_then(transcript_handle_row_value)
        .or_else(|| transcript_handle_fallback.map(str::to_string));
    let objective = obj
        .get("assignment")
        .and_then(|assignment| assignment.get("objective"))
        .and_then(serde_json::Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(|s| summarize_text(s, 220));
    let result = obj
        .get("result")
        .and_then(serde_json::Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(|s| summarize_text(s, 1_600));
    let steps = obj.get("steps_taken").and_then(serde_json::Value::as_u64);
    let duration_ms = obj.get("duration_ms").and_then(serde_json::Value::as_u64);
    // The wrapped row's receipt outranks the envelope fallback; absent or
    // unprintable values render no line at all (route is optional).
    let route_line = obj
        .get("child_route")
        .or(child_route_fallback)
        .and_then(subagent_route_line);

    let mut lines = vec![format!("- {agent_id} ({agent_type}) status={status}")];
    if let Some(route_line) = route_line {
        lines.push(route_line);
    }
    if let Some(transcript_handle) = transcript_handle {
        lines.push(format!("  transcript: {transcript_handle}"));
    }
    if let Some(objective) = objective {
        lines.push(format!("  objective: {objective}"));
    }
    match result {
        Some(result) => lines.push(format!("  result: {result}")),
        None => lines.push("  result: not available yet".to_string()),
    }
    if steps.is_some() || duration_ms.is_some() {
        let steps = steps
            .map(|n| n.to_string())
            .unwrap_or_else(|| "?".to_string());
        let duration_ms = duration_ms
            .map(|n| n.to_string())
            .unwrap_or_else(|| "?".to_string());
        lines.push(format!("  stats: steps={steps}, duration_ms={duration_ms}"));
    }
    lines.join("\n")
}

/// Agent-tool receipts that are not per-child result snapshots — `roster`
/// role catalogs, `wait` join state, and similar action payloads — carry
/// facts the snapshot summarizer cannot represent (the members catalog,
/// settled/`timed_out` state). Pass them through bounded instead of
/// collapsing them into "- unknown (agent) status=unknown" noise.
pub(crate) const SUBAGENT_RECEIPT_PASSTHROUGH_MAX_CHARS: usize = 2_000;

const SUBAGENT_RESULT_SUMMARY_HEADER: &str = "[sub-agent result summarized for parent context]\n";
const SUBAGENT_FLEET_SUMMARY_HEADER: &str =
    "[sub-agent fleet status summarized for parent context]\n";
const SUBAGENT_SELF_REPORT_NOTICE: &str = "Child results are self-reports; verify side effects with `read` or `bash` before claiming success.\n";

/// A per-child result snapshot: an object carrying `agent_id` or `status`.
fn subagent_snapshot_shaped(value: &serde_json::Value) -> bool {
    value
        .as_object()
        .is_some_and(|object| object.contains_key("agent_id") || object.contains_key("status"))
}

/// True when the parsed receipt structurally carries a `transcript_handle`
/// whose value a summarized row can actually print — the field the guidance
/// names. Free text that merely mentions the word must not summon the hint,
/// and neither must an empty or shapeless handle (Pinvou #490 class). Both
/// shapes that summarize rows are covered: a bare per-child object/array and
/// the unscoped fleet listing whose rows live under `agents[]`, each looked
/// through the same `snapshot` wrapper the row summarizer unwraps, so the
/// hint always has a visible value to point at. Only the rows the summarizer
/// actually renders gate the hint — it truncates past the eighth snapshot, so
/// a handle stranded on a truncated row would print nothing.
fn carries_transcript_handle(parsed: &serde_json::Value) -> bool {
    // Must stay in step with the `idx >= 8` truncation in
    // `compact_subagent_tool_result_for_context`.
    const VISIBLE_SNAPSHOT_ROWS: usize = 8;
    fn row_carries(row: &serde_json::Value) -> bool {
        row.get("transcript_handle")
            .or_else(|| {
                row.get("snapshot")
                    .and_then(|inner| inner.get("transcript_handle"))
            })
            .and_then(transcript_handle_row_value)
            .is_some()
    }
    match parsed {
        serde_json::Value::Array(items) => {
            items.iter().take(VISIBLE_SNAPSHOT_ROWS).any(row_carries)
        }
        serde_json::Value::Object(object) => {
            row_carries(parsed)
                || object
                    .get("agents")
                    .and_then(serde_json::Value::as_array)
                    .is_some_and(|fleet| fleet.iter().take(VISIBLE_SNAPSHOT_ROWS).any(row_carries))
        }
        _ => false,
    }
}

/// The value a summarized row prints for a `transcript_handle` field:
/// a non-empty `session_id/name` string `handle_read` accepts directly.
/// Producers serialize either that string shape or the full `var_handle`
/// object whose `session_id`/`name` fields identify the payload; `None`
/// means the field carries nothing the model could act on.
fn transcript_handle_row_value(value: &serde_json::Value) -> Option<String> {
    if let Some(raw) = value.as_str() {
        let raw = raw.trim();
        return (!raw.is_empty()).then(|| raw.to_string());
    }
    let object = value.as_object()?;
    let session_id = object.get("session_id")?.as_str()?.trim();
    let name = object.get("name")?.as_str()?.trim();
    (!session_id.is_empty() && !name.is_empty()).then(|| format!("{session_id}/{name}"))
}

/// Bounded verbatim passthrough for non-snapshot agent action receipts.
fn bounded_subagent_receipt(raw: &str) -> String {
    let mut out = String::from("[sub-agent receipt]\n");
    let total_chars = raw.chars().count();
    if total_chars <= SUBAGENT_RECEIPT_PASSTHROUGH_MAX_CHARS {
        out.push_str(raw);
        return out;
    }
    out.push_str(
        &raw.chars()
            .take(SUBAGENT_RECEIPT_PASSTHROUGH_MAX_CHARS)
            .collect::<String>(),
    );
    out.push_str(&format!(
        "\n[receipt truncated: showing {SUBAGENT_RECEIPT_PASSTHROUGH_MAX_CHARS} of {total_chars} characters]"
    ));
    out
}

/// How the snapshot summarizer should treat a parsed agent receipt.
enum SubagentSnapshotBatch<'a> {
    /// Per-child result snapshots: spawn receipts and scoped status/peek
    /// rows, where each object carries one child's identity and outcome.
    ChildResults(Vec<&'a serde_json::Value>),
    /// The unscoped status/peek fleet listing: the envelope is a
    /// collection, not one child, and can outgrow any bounded passthrough
    /// once two children are running — summarize the rows instead.
    FleetStatus(Vec<&'a serde_json::Value>),
}

fn compact_subagent_tool_result_for_context(tool_name: &str, raw: &str) -> Option<String> {
    if tool_name != "agent" {
        return None;
    }

    let parsed: serde_json::Value = serde_json::from_str(raw).ok()?;
    let batch: Option<SubagentSnapshotBatch> = match &parsed {
        serde_json::Value::Array(items) => {
            if !items.is_empty() && items.iter().all(subagent_snapshot_shaped) {
                Some(SubagentSnapshotBatch::ChildResults(items.iter().collect()))
            } else {
                None
            }
        }
        serde_json::Value::Object(object) => {
            // The unscoped fleet listing is matched before the action-receipt
            // rule: its envelope carries `action` too, and its rows must
            // summarize per child instead of truncating as one blob.
            if let Some(fleet) = object.get("agents").and_then(Value::as_array)
                && !fleet.is_empty()
                && fleet.iter().all(subagent_snapshot_shaped)
            {
                Some(SubagentSnapshotBatch::FleetStatus(fleet.iter().collect()))
            } else if object.contains_key("action") {
                // Action receipts — roster catalogs, message/followup/
                // interrupt acks, wait joins, the unchanged nudge — are
                // coordination payloads: the queued/woke/queue_depth/note
                // facts are what the model coordinates with, and
                // snapshot-summarizing them collapses the receipt into
                // "- unknown (agent) status=…" placeholder noise. Pass them
                // through bounded like the other non-snapshot receipts.
                // Spawn-start projections and write-claim receipts carry no
                // `action` key on their content (spawn's lives in tool
                // metadata); they reach the passthrough through the
                // shape fall-through below.
                None
            } else if subagent_snapshot_shaped(&parsed) {
                Some(SubagentSnapshotBatch::ChildResults(vec![&parsed]))
            } else {
                None
            }
        }
        _ => None,
    };
    let Some(batch) = batch else {
        // Not a per-child snapshot (`roster`/`wait`/`claim`/action-ack
        // receipts): the raw JSON is the payload the model needs — pass it
        // through bounded.
        return Some(bounded_subagent_receipt(raw));
    };
    let (header, snapshots) = match batch {
        SubagentSnapshotBatch::ChildResults(snapshots) => {
            (SUBAGENT_RESULT_SUMMARY_HEADER, snapshots)
        }
        SubagentSnapshotBatch::FleetStatus(snapshots) => (SUBAGENT_FLEET_SUMMARY_HEADER, snapshots),
    };

    let mut out = String::from(header);
    out.push_str(SUBAGENT_SELF_REPORT_NOTICE);
    // Only point at `transcript_handle` when this receipt actually carries
    // one: compact spawn receipts strip the handle before it reaches the
    // parent, so an unconditional hint would name a value the model never
    // received (Pinvou #490 phantom-tool class).
    if carries_transcript_handle(&parsed) {
        out.push_str(&format!(
            "Use `handle_read` on `transcript_handle` for bounded transcript slices when the returned summary is not enough — {handle_read_hint}.\n",
            handle_read_hint = crate::tools::subagent::HANDLE_READ_ACTIVATION_HINT
        ));
    }
    for (idx, snapshot) in snapshots.iter().enumerate() {
        if idx >= 8 {
            out.push_str(&format!(
                "- ... {} more sub-agent result(s) omitted from context summary\n",
                snapshots.len().saturating_sub(idx)
            ));
            break;
        }
        out.push_str(&summarize_subagent_snapshot(snapshot, idx + 1, None, None));
        out.push('\n');
    }
    Some(out.trim_end().to_string())
}

fn json_text<'a>(value: &'a Value, key: &str) -> Option<&'a str> {
    value
        .get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
}

fn json_number_text(value: &Value, key: &str) -> Option<String> {
    value
        .get(key)
        .and_then(|value| {
            value
                .as_i64()
                .map(|n| n.to_string())
                .or_else(|| value.as_u64().map(|n| n.to_string()))
        })
        .or_else(|| {
            value
                .get(key)
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(ToString::to_string)
        })
}

fn compact_run_tests_result_for_context(raw: &str) -> Option<String> {
    let parsed: Value = serde_json::from_str(raw).ok()?;
    let success = parsed.get("success")?.as_bool()?;
    let exit_code = json_number_text(&parsed, "exit_code").unwrap_or_else(|| "?".to_string());
    let command = json_text(&parsed, "command").unwrap_or("(unknown command)");
    let stdout = json_text(&parsed, "stdout");
    let stderr = json_text(&parsed, "stderr");
    let stream_limit = if success { 500 } else { 1_000 };

    let mut lines = vec![
        "[run_tests result summarized for context]".to_string(),
        format!(
            "status: {}, exit_code: {exit_code}",
            if success { "passed" } else { "failed" }
        ),
        format!("command: {}", summarize_text(command, 300)),
    ];
    if let Some(stderr) = stderr {
        lines.push(format!(
            "stderr: {}",
            summarize_text_head_tail(stderr, stream_limit)
        ));
    }
    if let Some(stdout) = stdout {
        lines.push(format!(
            "stdout: {}",
            summarize_text_head_tail(stdout, stream_limit)
        ));
    }
    Some(lines.join("\n"))
}

fn run_verifier_status_rank(status: Option<&str>) -> u8 {
    match status.unwrap_or_default() {
        "failed" | "timeout" => 0,
        "skipped" => 1,
        "passed" => 2,
        _ => 3,
    }
}

fn compact_run_verifiers_result_for_context(raw: &str) -> Option<String> {
    let parsed: Value = serde_json::from_str(raw).ok()?;
    let gates = parsed.get("gates")?.as_array()?;
    let summary = json_text(&parsed, "summary")
        .map(ToString::to_string)
        .unwrap_or_else(|| {
            let passed = json_number_text(&parsed, "passed").unwrap_or_else(|| "?".to_string());
            let failed = json_number_text(&parsed, "failed").unwrap_or_else(|| "?".to_string());
            let skipped = json_number_text(&parsed, "skipped").unwrap_or_else(|| "?".to_string());
            format!("{passed} passed, {failed} failed, {skipped} skipped")
        });

    let mut ordered: Vec<&Value> = gates.iter().collect();
    ordered.sort_by(|a, b| {
        run_verifier_status_rank(json_text(a, "status"))
            .cmp(&run_verifier_status_rank(json_text(b, "status")))
            .then_with(|| json_text(a, "name").cmp(&json_text(b, "name")))
    });

    let mut lines = vec![
        "[run_verifiers result summarized for context]".to_string(),
        format!("summary: {summary}"),
    ];
    let profile = json_text(&parsed, "profile");
    let level = json_text(&parsed, "level");
    if profile.is_some() || level.is_some() {
        lines.push(format!(
            "selection: profile={}, level={}",
            profile.unwrap_or("?"),
            level.unwrap_or("?")
        ));
    }

    for (idx, gate) in ordered.iter().enumerate() {
        if idx >= 12 {
            lines.push(format!(
                "- ... {} more gate(s) omitted from context summary",
                ordered.len().saturating_sub(idx)
            ));
            break;
        }

        let name = json_text(gate, "name").unwrap_or("gate");
        let ecosystem = json_text(gate, "ecosystem").unwrap_or("unknown");
        let status = json_text(gate, "status").unwrap_or("unknown");
        let exit = json_number_text(gate, "exit_code")
            .map(|code| format!(" exit={code}"))
            .unwrap_or_default();
        lines.push(format!("- {name} ({ecosystem}): {status}{exit}"));

        if status != "passed" {
            if let Some(command) = json_text(gate, "command") {
                lines.push(format!("  command: {}", summarize_text(command, 240)));
            }
            if let Some(detail) = json_text(gate, "skipped_reason")
                .or_else(|| json_text(gate, "stderr"))
                .or_else(|| json_text(gate, "stdout"))
            {
                lines.push(format!(
                    "  detail: {}",
                    summarize_text_head_tail(detail, 600)
                ));
            }
        }
    }

    Some(lines.join("\n"))
}

fn compact_task_gate_run_result_for_context(raw: &str) -> Option<String> {
    let parsed: Value = serde_json::from_str(raw).ok()?;
    let gate = parsed.get("gate")?;
    let gate_name = json_text(gate, "gate").unwrap_or("gate");
    let status = json_text(gate, "status").unwrap_or("unknown");
    let command = json_text(gate, "command").unwrap_or("(unknown command)");
    let summary = json_text(gate, "summary")
        .or_else(|| json_text(&parsed, "stderr_summary"))
        .or_else(|| json_text(&parsed, "stdout_summary"));
    let exit = json_number_text(gate, "exit_code")
        .map(|code| format!(", exit_code: {code}"))
        .unwrap_or_default();

    let mut lines = vec![
        "[task_gate_run result summarized for context]".to_string(),
        format!("gate: {gate_name}, status: {status}{exit}"),
        format!("command: {}", summarize_text(command, 300)),
    ];
    if let Some(summary) = summary {
        lines.push(format!("summary: {}", summarize_text(summary, 800)));
    }
    if let Some(log_path) = json_text(gate, "log_path") {
        lines.push(format!("log_path: {log_path}"));
    }
    Some(lines.join("\n"))
}

fn compact_structured_tool_result_for_context(tool_name: &str, raw: &str) -> Option<String> {
    match tool_name {
        "run_tests" => compact_run_tests_result_for_context(raw),
        "run_verifiers" => compact_run_verifiers_result_for_context(raw),
        // `tasks` is the unified durable-task tool (piagent phase B); its
        // gate_run action emits the same gate payload as the legacy
        // `task_gate_run` alias. The compactor returns None unless the
        // content actually parses as a gate result, so non-gate `tasks`
        // results fall through to the generic limits unchanged.
        "task_gate_run" | "tasks" => compact_task_gate_run_result_for_context(raw),
        _ => None,
    }
}

fn tool_result_context_limits_for_window(context_window: u32) -> ToolResultContextLimits {
    let is_large_context = context_window >= LARGE_CONTEXT_WINDOW_TOKENS;

    let mut limits = if is_large_context {
        ToolResultContextLimits {
            hard_limit_chars: LARGE_CONTEXT_TOOL_RESULT_HARD_LIMIT_CHARS,
            noisy_soft_limit_chars: LARGE_CONTEXT_TOOL_RESULT_SOFT_LIMIT_CHARS,
            snippet_chars: LARGE_CONTEXT_TOOL_RESULT_SNIPPET_CHARS,
        }
    } else {
        ToolResultContextLimits {
            hard_limit_chars: TOOL_RESULT_CONTEXT_HARD_LIMIT_CHARS,
            noisy_soft_limit_chars: TOOL_RESULT_CONTEXT_SOFT_LIMIT_CHARS,
            snippet_chars: TOOL_RESULT_CONTEXT_SNIPPET_CHARS,
        }
    };
    if let Some(bytes) =
        crate::tools::large_output_router::WorkshopConfig::active_tool_result_max_bytes()
    {
        // Opt-in long-context profiles may raise the model-visible budget.
        // Never lower the compile-time floor; cap at 2 MiB (#5367).
        let raised = bytes.clamp(limits.hard_limit_chars, 2 * 1024 * 1024);
        limits.hard_limit_chars = raised;
        limits.snippet_chars = (raised / 3).max(limits.snippet_chars);
        limits.noisy_soft_limit_chars = limits.noisy_soft_limit_chars.max(raised / 6);
    }
    limits
}

#[cfg(test)]
pub(crate) fn compact_tool_result_for_context(
    model: &str,
    tool_name: &str,
    output: &ToolResult,
) -> String {
    compact_tool_result_for_route(ApiProvider::Deepseek, model, None, tool_name, output)
}

pub(crate) fn compact_tool_result_for_route(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
    tool_name: &str,
    output: &ToolResult,
) -> String {
    let raw = output.content.trim();
    if raw.is_empty() {
        return String::new();
    }

    // A result already bounded by the adaptive evidence envelope is an
    // honest, context-sized preview whose footer names the artifact path and
    // a recovery instruction. Re-compacting it would strip that recovery
    // contract and double-truncate the output, so pass it through unchanged.
    if output
        .metadata
        .as_ref()
        .and_then(|metadata| metadata.get("evidence_available"))
        .and_then(serde_json::Value::as_bool)
        .unwrap_or(false)
    {
        return raw.to_string();
    }

    // A `read` result that already fit its byte budget carries a resume
    // footer instead of mid-line truncation; compacting it again would
    // discard content the budget deliberately kept. A result that somehow
    // exceeded its declared budget still falls through to the ordinary
    // limits below.
    if output
        .metadata
        .as_ref()
        .and_then(|metadata| metadata.get("read_budget_bytes"))
        .and_then(serde_json::Value::as_u64)
        .is_some_and(|budget| raw.len() as u64 <= budget)
    {
        return raw.to_string();
    }

    if let Some(summary) = compact_subagent_tool_result_for_context(tool_name, raw) {
        return summary;
    }

    if let Some(summary) = compact_structured_tool_result_for_context(tool_name, raw) {
        return summary;
    }

    let context_window =
        crate::route_budget::route_context_window_tokens(provider, model, route_limits);
    let limits = tool_result_context_limits_for_window(context_window);
    let raw_chars = raw.chars().count();
    let should_compact = raw_chars > limits.hard_limit_chars
        || (tool_result_is_noisy(tool_name) && raw_chars > limits.noisy_soft_limit_chars);
    if !should_compact {
        return raw.to_string();
    }

    let snippet = summarize_text_head_tail(raw, limits.snippet_chars);
    let omitted = raw_chars.saturating_sub(snippet.chars().count());
    let summary = tool_result_metadata_summary(output.metadata.as_ref());

    if let Some(summary) = summary {
        format!(
            "[{tool_name} output compacted to protect context]\nSummary: {summary}\nSnippet: {snippet}\n(Original: {raw_chars} chars, omitted: {omitted} chars.)"
        )
    } else {
        format!(
            "[{tool_name} output compacted to protect context]\nSnippet: {snippet}\n(Original: {raw_chars} chars, omitted: {omitted} chars.)"
        )
    }
}

pub(super) fn extract_compaction_summary_prompt(
    prompt: Option<SystemPrompt>,
) -> Option<SystemPrompt> {
    crate::compaction::extract_compaction_summary(prompt.as_ref())
}

/// Internal input-side token budget for a provider/model route:
/// `window - reserved_output - headroom`. Used by the preflight check,
/// emergency recovery, and capacity trimming to decide when to compact.
/// Unknown model ids fall back to the provider's conservative default instead
/// of disabling preflight; custom long-context deployments can still advertise
/// their window with a `-256k`/`-1024k` model suffix.
///
/// The reserved-output term is the route-effective request cap: exactly what
/// the API can receive after explicit overrides, compatibility/route ceilings,
/// and the route window are intersected. A second hidden reasoning reserve
/// would make preflight disagree with the wire request and can cause premature
/// compaction on otherwise valid large-window inputs.
#[cfg(test)]
pub(super) fn context_input_budget_for_provider(
    provider: ApiProvider,
    model: &str,
) -> Option<usize> {
    context_input_budget_for_route(provider, model, None, 0)
}

/// Public so external callers (e.g. a host/bridge deriving its own compaction
/// trigger line) can reuse the *exact* same internal input-budget math — window
/// minus the route-effective output reservation
/// (`route_output_reservation`) minus headroom —
/// instead of re-deriving those constants and silently drifting from the engine.
/// Pass `input_tokens = 0` to get the full emergency input budget for the route.
pub fn context_input_budget_for_route(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
    input_tokens: usize,
) -> Option<usize> {
    route_context_budget_for_route(provider, model, route_limits, input_tokens)
        .and_then(|budget| usize::try_from(budget.available_input_tokens).ok())
}

#[cfg(test)]
pub(super) fn route_context_budget_for_provider(
    provider: ApiProvider,
    model: &str,
    input_tokens: usize,
) -> Option<ContextBudget> {
    route_context_budget_for_route(provider, model, None, input_tokens)
}

pub(super) fn route_context_budget_for_route(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<RouteLimits>,
    input_tokens: usize,
) -> Option<ContextBudget> {
    crate::route_budget::route_context_budget(provider, model, route_limits, input_tokens)
}

pub(super) fn is_context_length_error_message(message: &str) -> bool {
    // Only genuine context-length rejections may drive the bounded
    // context-recovery retry. The broader `InvalidInput` bucket also holds
    // wrong-model rejections ("Model not exist."), malformed requests, and
    // truncated-output terminations, where re-sending a compacted history
    // cannot help and would hide the real error.
    let lower = message.to_lowercase();
    lower.contains("model output truncated")
        || lower.contains("model response incomplete")
        || lower.contains("maximum context length")
        || lower.contains("context length")
        || lower.contains("context_length")
        || lower.contains("prompt is too long")
        || lower.contains("context window")
        || (lower.contains("requested") && lower.contains("tokens") && lower.contains("maximum"))
}

pub(super) fn is_image_input_rejection_message(message: &str) -> bool {
    let lower = message.to_lowercase();
    let image_signal = lower.contains("image_url")
        || lower.contains("content.type")
        || lower.contains("content type")
        || lower.contains("does not support image")
        || lower.contains("image input")
        || lower.contains("unsupported modality")
        || lower
            .split(|character: char| !character.is_alphanumeric())
            .any(|term| term == "vision");
    let rejection_signal = lower.contains("400")
        || lower.contains("invalid")
        || lower.contains("unsupported")
        || lower.contains("not support");
    image_signal && rejection_signal
}

#[cfg(test)]
mod tests {
    use super::{emergency_trim_budget, is_image_input_rejection_message};

    #[test]
    fn emergency_trim_budget_leaves_a_hysteresis_margin() {
        assert_eq!(emergency_trim_budget(246_784), 197_428);
        // Tiny budgets lose most of the margin to integer division but never
        // trim past the line itself.
        assert_eq!(emergency_trim_budget(10), 8);
        assert_eq!(emergency_trim_budget(4), 4);
        assert_eq!(emergency_trim_budget(0), 0);
    }

    #[test]
    fn image_rejection_classifier_matches_provider_400s() {
        assert!(is_image_input_rejection_message(
            r#"request (400): {"error":{"code":"1214","message":"messages.content.type 参数非法, 取值范围 ['text']"}}"#
        ));
        assert!(is_image_input_rejection_message(
            "Invalid content type. image_url is only supported by certain models."
        ));
        assert!(!is_image_input_rejection_message("Model not exist."));
        assert!(!is_image_input_rejection_message("invalid revision id"));
        assert!(!is_image_input_rejection_message(
            "This model's maximum context length is 131072 tokens."
        ));
    }
}
