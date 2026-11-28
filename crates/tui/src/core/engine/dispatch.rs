//! Tool dispatch — plan/execute helpers for the per-turn tool batch.
//!
//! Extracted from `core/engine.rs` (P1.3). The high-level ordering still
//! lives in `Engine::run_turn`; this module owns:
//!
//! * Streaming-buffer parsing into a finalized `serde_json::Value` tool input
//!   (`final_tool_input`, `parse_tool_input`, fenced/JSON segment helpers).
//! * The `multi_tool_use.parallel` payload parser.
//! * Policy predicates the turn loop consults — when a batch can run in
//!   parallel and the small set of read-only MCP tools that are safe to run
//!   in parallel.
//! * The tool execution plan/outcome types the batch driver passes around.
//!
//! All items are `pub(super)`-only: the public engine surface (Op/Event,
//! `EngineHandle`, `spawn_engine`) stays in `core/engine.rs`.

use serde_json::json;

use crate::models::{Tool, ToolCaller};
use crate::tools::spec::{
    ResourceClaim, ToolError, ToolExecutionOutcome, ToolResult, ToolResultContentBlock,
    schedule_non_conflicting,
};

use super::ToolUseState;

const MAX_SCHEMA_CONTAINER_REPAIR_BYTES: usize = 64 * 1024;

// === Types ============================================================

#[allow(dead_code)] // `index` mirrors batch order for diagnostic ergonomics.
pub(super) struct ToolExecOutcome {
    pub(super) index: usize,
    pub(super) id: String,
    pub(super) name: String,
    pub(super) input: serde_json::Value,
    pub(super) started_at: std::time::Instant,
    pub(super) terminal: ToolExecutionOutcome,
    pub(super) content_blocks: Vec<ToolResultContentBlock>,
}

#[derive(Debug, Clone)]
pub(super) struct ToolExecutionPlan {
    pub(super) index: usize,
    pub(super) id: String,
    pub(super) name: String,
    pub(super) input: serde_json::Value,
    pub(super) caller: Option<ToolCaller>,
    pub(super) interactive: bool,
    pub(super) approval_required: bool,
    pub(super) approval_description: String,
    pub(super) approval_force_prompt: bool,
    pub(super) supports_parallel: bool,
    pub(super) read_only: bool,
    pub(super) detached_start: bool,
    pub(super) resources: Vec<ResourceClaim>,
    pub(super) blocked_error: Option<ToolError>,
    pub(super) guard_result: Option<ToolResult>,
}

pub(super) enum ToolExecutionBatch {
    Parallel(Vec<ToolExecutionPlan>),
    Serial(Box<ToolExecutionPlan>),
}

#[derive(Debug, serde::Serialize)]
pub(super) struct ParallelToolResultEntry {
    pub(super) tool_name: String,
    pub(super) success: bool,
    pub(super) content: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(super) error: Option<String>,
}

#[derive(Debug, serde::Serialize)]
pub(super) struct ParallelToolResult {
    pub(super) results: Vec<ParallelToolResultEntry>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum ToolApprovalStamp {
    ApprovedByUser,
    ApprovedWithPolicy,
}

impl ToolApprovalStamp {
    fn decision(self) -> &'static str {
        match self {
            Self::ApprovedByUser => "approved_by_user",
            Self::ApprovedWithPolicy => "approved_with_policy",
        }
    }

    fn model_visible_note(self) -> &'static str {
        match self {
            Self::ApprovedByUser => {
                "[approval] This tool call required approval and was approved by the user before execution."
            }
            Self::ApprovedWithPolicy => {
                "[approval] This tool call required approval and was approved by the user with an adjusted execution policy before execution."
            }
        }
    }
}

pub(super) fn stamp_tool_result_approval(result: &mut ToolResult, approval: ToolApprovalStamp) {
    let approval_metadata = json!({
        "required": true,
        "decision": approval.decision(),
        "model_visible": true,
    });
    let metadata = result.metadata.get_or_insert_with(|| json!({}));
    if let Some(object) = metadata.as_object_mut() {
        object.insert("approval".to_string(), approval_metadata);
    } else {
        let prior = std::mem::replace(metadata, json!({}));
        if let Some(object) = metadata.as_object_mut() {
            object.insert("_prior".to_string(), prior);
            object.insert("approval".to_string(), approval_metadata);
        }
    }

    let note = approval.model_visible_note();
    if result.content.starts_with("[approval] ") {
        return;
    }
    if result.content.is_empty() {
        result.content = note.to_string();
    } else {
        result.content = format!("{note}\n\n{}", result.content);
    }
}

// Hold the lock guard for the duration of a tool execution.
// The inner guards are held for RAII purposes (dropped when the guard is dropped).
pub(super) enum ToolExecGuard<'a> {
    Read(#[allow(dead_code)] tokio::sync::RwLockReadGuard<'a, ()>),
    Write(#[allow(dead_code)] tokio::sync::RwLockWriteGuard<'a, ()>),
}

// === Caller policy and errors ========================================

pub(super) fn caller_type_for_tool_use(caller: Option<&ToolCaller>) -> &str {
    caller.map_or("direct", |c| c.caller_type.as_str())
}

pub(super) fn caller_allowed_for_tool(
    caller: Option<&ToolCaller>,
    tool_def: Option<&Tool>,
) -> bool {
    let requested = caller_type_for_tool_use(caller);
    if let Some(def) = tool_def
        && let Some(allowed) = &def.allowed_callers
    {
        if allowed.is_empty() {
            return requested == "direct";
        }
        return allowed.iter().any(|item| item == requested);
    }
    requested == "direct"
}

/// Whole-word check for "mode"/"modes" — a plain `contains("mode")` also
/// matched "model", letting provider model errors skip the actionable-hint
/// suffix (#3020).
fn mentions_mode_word(lower: &str) -> bool {
    lower
        .split(|ch: char| !ch.is_ascii_alphanumeric())
        .any(|word| word == "mode" || word == "modes")
}

#[cfg(test)]
pub(super) fn format_tool_error(err: &ToolError, tool_name: &str) -> String {
    format_tool_error_with_schema(err, tool_name, None)
}

pub(super) fn format_tool_error_with_schema(
    err: &ToolError,
    tool_name: &str,
    input_schema: Option<&serde_json::Value>,
) -> String {
    let message = match err {
        ToolError::InvalidInput { message } => {
            format!("Invalid input for tool '{tool_name}': {message}")
        }
        ToolError::MissingField { field } => {
            format!("Tool '{tool_name}' is missing required field '{field}'")
        }
        ToolError::PathEscape { path } => format!(
            "Path escapes the workspace boundary: {}. Relative paths resolve against the primary workspace; use the absolute spelling of a folder listed under Accessible folders, or enable trust mode.",
            path.display()
        ),
        ToolError::ExecutionFailed { message } => message.clone(),
        ToolError::Timeout { seconds } => format!(
            "Tool '{tool_name}' timed out after {seconds}s. Try a narrower scope or a longer timeout."
        ),
        ToolError::Cancelled { message } => message.clone(),
        ToolError::NotAvailable { message } => {
            let lower = message.to_ascii_lowercase();
            // #3020: Pass through self-explanatory messages that already name the
            // cause (mode switch, allow_shell, feature flag).  Avoids appending a
            // conflicting "Check mode, feature flags" suffix on top of
            // "switch to Act mode" which already gives the recovery path.
            if lower.contains("current tool catalog")
                || lower.contains("did you mean:")
                || mentions_mode_word(&lower)
                || lower.contains("allow_shell")
                || lower.contains("feature flag")
            {
                message.clone()
            } else {
                format!(
                    "Tool '{tool_name}' is not available: {message}. Check mode, feature flags, or tool name."
                )
            }
        }
        ToolError::PermissionDenied { message } => {
            let lower = message.to_ascii_lowercase();
            // #3020: Pass through messages that already name the denial cause.
            if mentions_mode_word(&lower)
                || lower.contains("allow_shell")
                || lower.contains("denied by user")
            {
                message.clone()
            } else {
                format!(
                    "Tool '{tool_name}' was denied: {message}. Adjust approval mode or request permission."
                )
            }
        }
    };

    let (category, bad_field) = match err {
        ToolError::InvalidInput { .. } => ("invalid_input", None),
        ToolError::MissingField { field } => ("missing_field", Some(field.as_str())),
        ToolError::PathEscape { .. } => ("path_escape", Some("path")),
        ToolError::NotAvailable { .. } => ("tool_not_available", Some("tool_name")),
        _ => return message,
    };
    let valid_shape = bounded_valid_shape(tool_name, input_schema);
    let feedback = serde_json::json!({
        "category": category,
        "bad_field": bad_field,
        "valid_shape": valid_shape,
        "retryable": true,
        "side_effect_status": "not_started"
    });
    format!("{message}\nTool validation feedback: {feedback}")
}

/// Largest input schema echoed verbatim into a validation receipt. The
/// `agent` tool's schema alone is several kilobytes of prose; replaying it on
/// every semantic refusal (unknown agent id, write-scope contention, unknown
/// profile) buried the one-line cause and cost the parent that much context
/// per retry. Above the budget the receipt keeps the field inventory — which
/// is what a shape repair needs — and points back at the advertised schema.
const VALID_SHAPE_BUDGET_BYTES: usize = 2048;
const VALID_SHAPE_MAX_PROPERTIES: usize = 40;
/// Byte budget for the reduced field inventory within the receipt, so the
/// reduction honors the budget it enforces even when few property names are
/// individually long.
const VALID_SHAPE_INVENTORY_BYTES: usize = 1024;
/// A single name longer than this is skipped from the inventory (counted in
/// the omitted tail) instead of overflowing the inventory budget alone.
const VALID_SHAPE_MAX_NAME_BYTES: usize = 200;

/// Keep the leading names that fit `max_items` entries and `budget_bytes`
/// total (quotes and separators included), skipping any single name over
/// `VALID_SHAPE_MAX_NAME_BYTES`; everything dropped is counted as omitted.
fn bounded_name_inventory(names: Vec<String>) -> (Vec<String>, usize) {
    let mut kept = Vec::new();
    let mut omitted = 0usize;
    let mut used = 0usize;
    for name in names {
        let cost = name.len() + 4;
        if name.len() > VALID_SHAPE_MAX_NAME_BYTES
            || kept.len() >= VALID_SHAPE_MAX_PROPERTIES
            || used + cost > VALID_SHAPE_INVENTORY_BYTES
        {
            omitted += 1;
            continue;
        }
        used += cost;
        kept.push(name);
    }
    (kept, omitted)
}

fn bounded_valid_shape(
    tool_name: &str,
    input_schema: Option<&serde_json::Value>,
) -> serde_json::Value {
    let Some(schema) = input_schema else {
        return serde_json::json!({
            "type": "object",
            "guidance": format!("Use the advertised input schema for '{tool_name}'")
        });
    };
    let serialized_len = schema.to_string().len();
    if serialized_len <= VALID_SHAPE_BUDGET_BYTES {
        return schema.clone();
    }
    let (mut properties, omitted_properties) = bounded_name_inventory(
        schema
            .get("properties")
            .and_then(serde_json::Value::as_object)
            .map(|properties| properties.keys().cloned().collect::<Vec<_>>())
            .unwrap_or_default(),
    );
    if omitted_properties > 0 {
        properties.push(format!(
            "... and {omitted_properties} more properties (see the advertised schema)"
        ));
    }
    let (required, omitted_required) = bounded_name_inventory(
        schema
            .get("required")
            .and_then(serde_json::Value::as_array)
            .map(|values| {
                values
                    .iter()
                    .filter_map(serde_json::Value::as_str)
                    .map(str::to_string)
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default(),
    );
    // Carry the schema's own type only when it declares one: a
    // composition-only (oneOf/anyOf) schema may admit non-objects, and a
    // hardcoded `"type":"object"` would let a model reading only this
    // receipt infer a shape the tool never promised.
    let mut reduced = serde_json::Map::new();
    if let Some(schema_type) = schema.get("type") {
        reduced.insert("type".to_string(), schema_type.clone());
    }
    reduced.insert(
        "properties".to_string(),
        serde_json::Value::Array(
            properties
                .into_iter()
                .map(serde_json::Value::String)
                .collect(),
        ),
    );
    if !required.is_empty() {
        let mut required_values = required
            .into_iter()
            .map(serde_json::Value::String)
            .collect::<Vec<_>>();
        if omitted_required > 0 {
            required_values.push(serde_json::Value::String(format!(
                "... and {omitted_required} more required fields (see the advertised schema)"
            )));
        }
        reduced.insert(
            "required".to_string(),
            serde_json::Value::Array(required_values),
        );
    }
    reduced.insert(
        "guidance".to_string(),
        serde_json::json!(format!(
            "Full schema for '{tool_name}' ({serialized_len} bytes) omitted from this receipt; it is the advertised input schema already in your tool list. Fix the cause named above rather than the shape."
        )),
    );
    serde_json::Value::Object(reduced)
}

// === Streaming-buffer parsing =========================================

/// Promote a streaming `ToolUseState` to a finalized JSON input.
///
/// Order of preference:
///
///   1. `input_buffer` (the raw streamed delta concatenation) — parsed as
///      JSON. This is the most authoritative because it's what the model
///      actually emitted.
///   2. `input` (the per-delta best-effort parse mirror) — used when the
///      buffer is empty (pre-streaming tool calls take this path).
///   3. `input_buffer` non-empty but unparseable → fall back to `input`
///      (the per-delta parser has already mirrored the most recent valid
///      partial parse into `tool_state.input`).
pub(super) fn final_tool_input(state: &ToolUseState) -> serde_json::Value {
    if state.input_parse_error.is_some() {
        return malformed_tool_arguments_input(&state.input_buffer);
    }
    if !state.input_buffer.trim().is_empty()
        && let Some(parsed) = parse_tool_input(&state.input_buffer)
    {
        return parsed;
    }
    state.input.clone()
}

pub(super) fn parse_completed_tool_input_for_provider(
    provider: crate::config::ApiProvider,
    buffer: &str,
) -> Result<serde_json::Value, String> {
    let value = if provider == crate::config::ApiProvider::Moonshot {
        serde_json::from_str(buffer).map_err(|_| malformed_tool_arguments_error(buffer))?
    } else {
        parse_tool_input(buffer).ok_or_else(|| malformed_tool_arguments_error(buffer))?
    };
    if provider == crate::config::ApiProvider::Moonshot && !value.is_object() {
        return Err(format!(
            "malformed tool arguments from model: expected a JSON object, got {}",
            bounded_raw_arguments_excerpt(buffer)
        ));
    }
    Ok(value)
}

/// Bound the raw-arguments excerpt carried in error text.
///
/// The `raw_arguments` sentinel keeps the full payload for replay, but the
/// human- and model-facing error message must not: a hostile or broken
/// multi-megabyte argument stream would otherwise double its memory in the
/// error string and flood the model's context on every subsequent turn
/// (review 6061318874).
pub(crate) fn bounded_raw_arguments_excerpt(buffer: &str) -> String {
    const MAX_EXCERPT_BYTES: usize = 512;
    if buffer.len() <= MAX_EXCERPT_BYTES {
        return format!("{buffer:?}");
    }
    let mut end = MAX_EXCERPT_BYTES;
    while !buffer.is_char_boundary(end) {
        end -= 1;
    }
    format!("{:?}… ({} bytes total)", &buffer[..end], buffer.len())
}

pub(super) fn parse_tool_input(buffer: &str) -> Option<serde_json::Value> {
    let trimmed = buffer.trim();
    if trimmed.is_empty() {
        return None;
    }
    // Try the deterministic arg-repair ladder first (handles trailing commas,
    // unclosed braces, embedded control chars, etc.)
    if let Ok(value) = crate::tools::arg_repair::repair(trimmed) {
        return Some(value);
    }
    // Fall back to existing strategies for code-fenced, double-encoded, and
    // segment-extraction patterns that the repair ladder doesn't cover.
    if let Some(stripped) = strip_code_fences(trimmed)
        && let Ok(value) = serde_json::from_str::<serde_json::Value>(&stripped)
    {
        return Some(value);
    }
    if let Ok(serde_json::Value::String(inner)) = serde_json::from_str::<serde_json::Value>(trimmed)
        && let Ok(value) = serde_json::from_str::<serde_json::Value>(&inner)
    {
        return Some(value);
    }
    extract_json_segment(trimmed)
        .and_then(|segment| serde_json::from_str::<serde_json::Value>(&segment).ok())
}

/// Decode a JSON container that a provider encoded as a string when the tool
/// schema explicitly requires an object or array.
///
/// This intentionally avoids general argument coercion: the string must be
/// bounded, parse as strict JSON, and decode to the declared container type.
/// Primitive strings are never coerced.
pub(super) fn normalize_schema_json_containers(
    value: &mut serde_json::Value,
    schema: &serde_json::Value,
) -> usize {
    let expected_container = if schema_declares_type(schema, "object") {
        Some("object")
    } else if schema_declares_type(schema, "array") {
        Some("array")
    } else {
        None
    };

    if let (Some(expected), serde_json::Value::String(encoded)) = (expected_container, &*value)
        && encoded.len() <= MAX_SCHEMA_CONTAINER_REPAIR_BYTES
        && let Ok(decoded) = serde_json::from_str::<serde_json::Value>(encoded)
        && ((expected == "object" && decoded.is_object())
            || (expected == "array" && decoded.is_array()))
    {
        *value = decoded;
        return 1 + normalize_schema_json_containers(value, schema);
    }

    match value {
        serde_json::Value::Object(object) => {
            let properties = schema
                .get("properties")
                .and_then(serde_json::Value::as_object);
            object
                .iter_mut()
                .map(|(key, child)| {
                    properties
                        .and_then(|items| items.get(key))
                        .map(|child_schema| normalize_schema_json_containers(child, child_schema))
                        .unwrap_or(0)
                })
                .sum()
        }
        serde_json::Value::Array(items) => schema
            .get("items")
            .map(|item_schema| {
                items
                    .iter_mut()
                    .map(|item| normalize_schema_json_containers(item, item_schema))
                    .sum()
            })
            .unwrap_or(0),
        _ => 0,
    }
}

fn schema_declares_type(schema: &serde_json::Value, expected: &str) -> bool {
    match schema.get("type") {
        Some(serde_json::Value::String(value)) => value == expected,
        Some(serde_json::Value::Array(values)) => values.iter().any(|value| value == expected),
        _ => false,
    }
}

/// The shared malformed-arguments sentinel (`raw_arguments`). `pub(crate)`
/// so the blocking response decoder can represent one malformed Moonshot
/// call through the same per-tool shape instead of failing the whole
/// response (review 4689524205); the engine remains the only consumer that
/// turns it into a tool error.
pub(crate) fn malformed_tool_arguments_input(buffer: &str) -> serde_json::Value {
    json!({ "raw_arguments": buffer })
}

pub(super) fn malformed_tool_arguments_error(buffer: &str) -> String {
    format!(
        "malformed tool arguments from model: expected valid JSON, got {}",
        bounded_raw_arguments_excerpt(buffer)
    )
}

fn strip_code_fences(text: &str) -> Option<String> {
    if !text.contains("```") {
        return None;
    }
    let line_count = text.lines().count();
    let mut lines = Vec::with_capacity(line_count);
    for line in text.lines() {
        if line.trim_start().starts_with("```") {
            continue;
        }
        lines.push(line);
    }
    let stripped = lines.join("\n");
    let stripped = stripped.trim();
    if stripped.is_empty() {
        None
    } else {
        Some(stripped.to_string())
    }
}

fn extract_json_segment(text: &str) -> Option<String> {
    extract_balanced_segment(text, '{', '}').or_else(|| extract_balanced_segment(text, '[', ']'))
}

fn extract_balanced_segment(text: &str, open: char, close: char) -> Option<String> {
    // Raw segment selection is mirrored by engine::partial_input; keep in sync.
    let start = text.find(open)?;
    let mut depth = 0i32;
    let mut end = None;
    for (offset, ch) in text[start..].char_indices() {
        if ch == open {
            depth += 1;
        } else if ch == close {
            depth -= 1;
            if depth == 0 {
                end = Some(start + offset + ch.len_utf8());
                break;
            }
        }
    }
    end.map(|end_idx| text[start..end_idx].to_string())
}

fn normalize_parallel_tool_name(raw: &str) -> String {
    let mut name = raw.trim();
    for prefix in ["functions.", "tools.", "tool."] {
        if let Some(stripped) = name.strip_prefix(prefix) {
            name = stripped;
            break;
        }
    }
    name.to_string()
}

pub(super) fn parse_parallel_tool_calls(
    input: &serde_json::Value,
) -> Result<Vec<(String, serde_json::Value)>, ToolError> {
    let tool_uses = input
        .get("tool_uses")
        .and_then(|v| v.as_array())
        .ok_or_else(|| ToolError::missing_field("tool_uses"))?;
    if tool_uses.is_empty() {
        return Err(ToolError::invalid_input(
            "multi_tool_use.parallel requires at least one tool call",
        ));
    }

    let mut calls = Vec::with_capacity(tool_uses.len());
    for item in tool_uses {
        let name = item
            .get("recipient_name")
            .or_else(|| item.get("tool_name"))
            .or_else(|| item.get("name"))
            .or_else(|| item.get("tool"))
            .and_then(|v| v.as_str())
            .ok_or_else(|| ToolError::missing_field("recipient_name"))?;
        let params = item
            .get("parameters")
            .or_else(|| item.get("input"))
            .or_else(|| item.get("args"))
            .or_else(|| item.get("arguments"))
            .cloned()
            .unwrap_or_else(|| json!({}));
        calls.push((normalize_parallel_tool_name(name), params));
    }

    Ok(calls)
}

// === Dispatch policy ==================================================

#[cfg(test)]
pub(super) fn should_parallelize_tool_batch(plans: &[ToolExecutionPlan]) -> bool {
    if plans.is_empty() || !plans.iter().all(tool_plan_can_join_parallel_batch) {
        return false;
    }
    schedule_non_conflicting(
        plans
            .iter()
            .map(|plan| ((), plan.resources.clone()))
            .collect(),
    )
    .len()
        == 1
}

pub(super) fn tool_plan_is_parallel_safe(plan: &ToolExecutionPlan) -> bool {
    plan.read_only && plan.supports_parallel && !plan.approval_required && !plan.interactive
}

pub(super) fn tool_plan_can_join_parallel_batch(plan: &ToolExecutionPlan) -> bool {
    plan.blocked_error.is_none()
        && (tool_plan_is_parallel_safe(plan)
            || (plan.detached_start && !plan.approval_required && !plan.interactive))
}

pub(super) fn plan_tool_execution_batches(
    plans: Vec<ToolExecutionPlan>,
) -> Vec<ToolExecutionBatch> {
    let mut batches = Vec::new();
    let mut parallel_candidates = Vec::new();

    let flush_parallel = |parallel_candidates: &mut Vec<_>,
                          batches: &mut Vec<ToolExecutionBatch>| {
        for chunk in schedule_non_conflicting(std::mem::take(parallel_candidates)) {
            batches.push(ToolExecutionBatch::Parallel(chunk));
        }
    };

    for plan in plans {
        if tool_plan_can_join_parallel_batch(&plan) {
            let resources = plan.resources.clone();
            parallel_candidates.push((plan, resources));
            continue;
        }

        flush_parallel(&mut parallel_candidates, &mut batches);
        batches.push(ToolExecutionBatch::Serial(Box::new(plan)));
    }

    flush_parallel(&mut parallel_candidates, &mut batches);

    batches
}

/// Same set as [`crate::mcp::MCP_READ_HELPER_TOOLS`] by policy: read-only
/// MCP helpers have no parallel hazard, so keep both gates on the shared
/// list instead of duplicating it here.
pub(super) fn mcp_tool_is_parallel_safe(name: &str) -> bool {
    crate::mcp::is_mcp_read_helper(name)
}

/// Engine-side classification of MCP tools, shared with the registry
/// adapter via [`crate::mcp::MCP_READ_HELPER_TOOLS`] so the turn loop and
/// the fleet/subagent registries can never classify the same tool
/// differently.
pub(super) fn mcp_tool_is_read_only(name: &str) -> bool {
    crate::mcp::is_mcp_read_helper(name)
}

pub(super) fn mcp_tool_approval_description(name: &str) -> String {
    if mcp_tool_is_read_only(name) {
        format!("Read-only MCP tool '{name}'")
    } else {
        format!("MCP tool '{name}' may have side effects")
    }
}

#[cfg(test)]
mod schema_json_container_tests {
    use super::*;
    use crate::tools::spec::ToolSpec;
    use serde_json::json;

    #[test]
    fn decodes_nested_containers_and_passes_tool_validation() {
        let schema = crate::tools::user_input::RequestUserInputTool.input_schema();
        let encoded_options = serde_json::to_string(&json!([
            { "label": "Repository", "description": "Inspect the current repository" },
            { "label": "Workspace", "description": "Inspect the whole workspace" }
        ]))
        .expect("encode options");
        let encoded_questions = serde_json::to_string(&json!([{
            "header": "Scope",
            "id": "scope",
            "question": "Which scope should be inspected?",
            "options": encoded_options
        }]))
        .expect("encode questions");
        let mut input = json!({ "questions": encoded_questions });

        assert_eq!(normalize_schema_json_containers(&mut input, &schema), 2);
        assert!(input["questions"].is_array());
        assert!(input["questions"][0]["options"].is_array());
        crate::tools::user_input::UserInputRequest::from_value(&input)
            .expect("normalized input must still pass tool-specific validation");
    }

    #[test]
    fn leaves_primitives_wrong_types_and_unbounded_strings_unchanged() {
        let schema = json!({
            "type": "object",
            "properties": {
                "text": { "type": "string" },
                "count": { "type": "integer" },
                "items": { "type": "array" },
                "oversized": { "type": "array" }
            }
        });
        let oversized = format!("[\"{}\"]", "x".repeat(MAX_SCHEMA_CONTAINER_REPAIR_BYTES));
        let mut input = json!({
            "text": "[\"still text\"]",
            "count": "10",
            "items": "{\"wrong\":\"container\"}",
            "oversized": oversized
        });
        let before = input.clone();

        assert_eq!(normalize_schema_json_containers(&mut input, &schema), 0);
        assert_eq!(input, before);
    }

    #[test]
    fn moonshot_completed_arguments_require_a_json_object() {
        for raw in ["[]", "1", "null", "\"text\"", "{"] {
            let error =
                parse_completed_tool_input_for_provider(crate::config::ApiProvider::Moonshot, raw)
                    .expect_err("non-object or invalid JSON must be malformed");
            assert!(error.contains("malformed tool arguments"), "{error}");
        }
        assert_eq!(
            parse_completed_tool_input_for_provider(
                crate::config::ApiProvider::Moonshot,
                "{\"ok\":true}",
            )
            .unwrap(),
            json!({"ok": true})
        );
        assert_eq!(
            parse_completed_tool_input_for_provider(crate::config::ApiProvider::Openai, "[]")
                .unwrap(),
            json!([])
        );
    }

    #[test]
    fn bounded_raw_arguments_excerpt_keeps_short_buffers_intact() {
        // In-bound buffers are only debug-quoted: full content, no marker.
        assert_eq!(bounded_raw_arguments_excerpt(""), "\"\"");
        assert_eq!(
            bounded_raw_arguments_excerpt("not json {"),
            "\"not json {\""
        );
        let excerpt = bounded_raw_arguments_excerpt("not json {");
        assert!(!excerpt.contains('…'), "{excerpt}");
        assert!(!excerpt.contains("bytes total"), "{excerpt}");
    }

    #[test]
    fn bounded_raw_arguments_excerpt_keeps_exactly_512_bytes_whole() {
        let buffer = "a".repeat(512);
        let excerpt = bounded_raw_arguments_excerpt(&buffer);
        // Exactly 512 bytes still counts as in-bound (`<=`): quoted content
        // only, never the truncation marker.
        assert_eq!(excerpt, format!("\"{}\"", "a".repeat(512)));
        assert_eq!(excerpt.len(), 514);
        assert!(!excerpt.contains('…'), "{excerpt}");
        assert!(!excerpt.contains("bytes total"), "{excerpt}");
    }

    #[test]
    fn bounded_raw_arguments_excerpt_truncates_oversized_ascii_at_512_bytes() {
        let buffer = "a".repeat(600);
        let excerpt = bounded_raw_arguments_excerpt(&buffer);
        assert_eq!(
            excerpt,
            format!("\"{}\"… (600 bytes total)", "a".repeat(512))
        );
        assert_eq!(excerpt.len(), 512 + 2 + "… (600 bytes total)".len());
        assert_eq!(excerpt, bounded_raw_arguments_excerpt(&buffer));
    }

    #[test]
    fn bounded_raw_arguments_excerpt_truncates_multibyte_without_splitting_chars() {
        let buffer = "🦀".repeat(150); // 150 * 4 = 600 bytes.
        let excerpt = bounded_raw_arguments_excerpt(&buffer);
        assert!(std::str::from_utf8(excerpt.as_bytes()).is_ok());
        assert!(!excerpt.contains('\u{FFFD}'), "{excerpt}");
        assert!(excerpt.ends_with("… (600 bytes total)"), "{excerpt}");
        // The 512-byte window lands on a char boundary, so the core is 128
        // whole crabs (512 bytes) and no partial character is emitted.
        assert_eq!(
            excerpt,
            format!("\"{}\"… (600 bytes total)", "🦀".repeat(128))
        );
        let core = &excerpt[1..excerpt.len() - 1 - "… (600 bytes total)".len()];
        assert_eq!(core.len(), 512);
        assert_eq!(core.chars().count(), 128);
    }

    #[test]
    fn bounded_raw_arguments_excerpt_keeps_multibyte_window_on_a_char_boundary() {
        // 129 crabs = 516 bytes: the 512-byte window ends exactly where the
        // 129th crab starts, so the core is 128 whole crabs (512 bytes).
        let buffer = "🦀".repeat(129);
        assert!(buffer.is_char_boundary(512));
        let excerpt = bounded_raw_arguments_excerpt(&buffer);
        assert_eq!(
            excerpt,
            format!("\"{}\"… (516 bytes total)", "🦀".repeat(128))
        );
        assert_eq!(excerpt.len(), 512 + 2 + "… (516 bytes total)".len());
        assert!(!excerpt.contains('\u{FFFD}'), "{excerpt}");
        let core = &excerpt[1..excerpt.len() - 1 - "… (516 bytes total)".len()];
        assert_eq!(core.len(), 512);
        assert_eq!(core.chars().count(), 128);
    }

    #[test]
    fn bounded_raw_arguments_excerpt_backs_off_to_a_char_boundary_when_the_window_splits_a_char() {
        // 510 ASCII bytes then two crabs (518 bytes): byte 512 falls inside
        // the first crab, so the excerpt must back off to the boundary at
        // 510 instead of slicing mid-character (which would panic).
        let buffer = format!("{}🦀🦀", "a".repeat(510));
        assert_eq!(buffer.len(), 518);
        assert!(!buffer.is_char_boundary(512));
        let excerpt = bounded_raw_arguments_excerpt(&buffer);
        assert_eq!(
            excerpt,
            format!("\"{}\"… (518 bytes total)", "a".repeat(510))
        );
        assert_eq!(excerpt.len(), 510 + 2 + "… (518 bytes total)".len());
        assert!(!excerpt.contains('\u{FFFD}'), "{excerpt}");
        let core = &excerpt[1..excerpt.len() - 1 - "… (518 bytes total)".len()];
        assert_eq!(core.len(), 510);
        assert_eq!(core, "a".repeat(510));
        assert!(buffer.is_char_boundary(510));
    }
}
