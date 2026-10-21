// One logical invocation may be observed by two consenting installations.
// Keep diagnostics from both, but project a single task into existing metrics.
import { namedBehaviorEvents } from './behavior-event-view.js';
const TERMINAL = new Set(["success", "failed", "cancelled", "interrupted"]);
const identity = event => event.identity_id || event.device_id;
const time = event => Number(event.occurred_at);
const ordered = events => [...events].sort((a, b) => time(a) - time(b) || String(a.event_id).localeCompare(String(b.event_id)));
const taskError = reason => ({ permission_denied: "permission_denied", permission_required: "configuration_missing",
  permission_changed: "permission_denied", permission_timeout: "timeout", task_timeout: "timeout",
  capability_stale: "configuration_missing", model_unavailable: "configuration_missing", rate_limited: "rate_limited",
  none: "unknown", unknown: "unknown" }[reason] || "internal_error");

export function remoteTaskGroups(events) {
  const groups = new Map();
  for (const event of events) {
    if (event.event_name !== "remote_control_event" || !/^[a-f0-9]{64}$/.test(event.remote_task_key || "")) continue;
    const rows = groups.get(event.remote_task_key) || [];
    rows.push(event);
    groups.set(event.remote_task_key, rows);
  }
  return [...groups.entries()].map(([key, rows]) => {
    rows = ordered(rows);
    const submitted = rows.find(e => e.remote_stage === "task_submit"
      && (e.remote_outcome === "ok" || e.remote_task_status === "unknown"));
    const notSent = rows.find(e => e.remote_stage === "task_not_sent"
      || (e.remote_stage === "task_submit" && e.remote_outcome !== "ok" && e.remote_task_status !== "unknown"));
    const authoritative = rows.find(e => e.remote_role === "controlled" && e.remote_stage === "task_terminal");
    // A later resolved receipt supersedes an earlier result-unknown snapshot.
    // B's ledger remains authoritative when both ends report a terminal.
    const receipts = rows.filter(e => e.remote_role === "controller" && e.remote_stage === "result_received");
    const received = receipts.findLast(e => TERMINAL.has(e.remote_task_status)) || receipts[0];
    // Unknown on B cannot be overwritten by an optimistic observer on A.
    const terminal = authoritative || received;
    const admitted = rows.find(e => e.remote_role === "controlled" && e.remote_stage === "task_admit");
    // An execution/receipt proves admission even if its admission event was
    // unavailable. Ignore a stale rejection from an older client or failed retry.
    const rejected = !admitted && !terminal ? rows.find(e => e.remote_stage === "task_rejected") : undefined;
    const owner = submitted || admitted || terminal || rows[0];
    const mode = rows.find(e => ["assistant", "code"].includes(e.remote_mode))?.remote_mode;
    return { key, rows, submitted, admitted, terminal, received, rejected, notSent, owner, mode,
      attributed: Boolean(submitted), authoritative,
      status: terminal?.remote_task_status || (rejected ? "rejected"
        : !admitted && submitted?.remote_task_status === "unknown" ? "unknown"
        : !submitted && !admitted && notSent ? "not_sent" : "pending") };
  });
}

export function projectRemoteTasks(events, taskGroups = null) {
  // Idempotent: callers may pass an already projected report to sub-builders.
  const raw = events.filter(e => !e.remote_projection);
  const projected = [];
  const tasks = taskGroups || remoteTaskGroups(raw);
  if (!tasks.length) return raw;
  for (const task of tasks) {
    if (!task.mode) continue; // Older clients remain diagnostics, never guesses.
    const base = {
      ...task.owner, session_id: `remote_${task.key}`, attempt_id: `remote_${task.key}`,
      turn_id: `remote_${task.key}`, mode: task.mode, initiator_type: "user",
      remote_projection: true,
      remote_attribution_unknown: !task.attributed,
    };
    const emit = (name, source, extra = {}) => {
      if (!source) return;
      projected.push({ ...base, ...extra, event_name: name,
        event_id: `remote_${task.key}_${name}${name === "model_used" ? `_${extra.provider_key}_${extra.model_key}` : ""}`,
        occurred_at: source.occurred_at });
    };
    // Admission is evidence of a submission, even if the initiating end opted
    // out. Such evidence contributes to calls, never passive-device activity.
    emit("task_submitted", task.submitted || task.admitted);
    emit("task_admitted", task.admitted);
    emit("task_rejected", task.rejected, { error_category: taskError(task.rejected?.remote_reason), stage: "submission" });
    const output = task.rows.find(e => e.remote_stage === "first_output");
    emit("task_first_output", output, { duration_ms: output?.remote_duration_ms, output_type: "text" });
    if (TERMINAL.has(task.status)) {
      emit("task_finished", task.terminal, { status: task.status,
        duration_ms: task.authoritative ? task.terminal.remote_duration_ms : undefined,
        error_category: task.status === "success" ? undefined : task.status === "cancelled" ? "user_cancelled" : taskError(task.terminal.remote_reason) });
    }
    const models = new Map(task.rows.filter(e => e.remote_stage === "model_used").map(e => [`${e.remote_provider_key}:${e.remote_model_key}`, e]));
    for (const model of models.values()) {
      emit("model_used", model, { provider_key: model.remote_provider_key, model_key: model.remote_model_key,
        selection_source: "system_routed" });
    }
  }
  const taskByKey = new Map(tasks.map(task => [task.key, task]));
  const normalized = raw.map(event => {
    if (!["tool_call_started", "tool_call_completed"].includes(event.event_name)) return event;
    const task = taskByKey.get(event.execution_task_key);
    if (!task) return event;
    return { ...event, device_id: task.owner.device_id, identity_id: task.owner.identity_id,
      session_id: `remote_${task.key}`, turn_id: `remote_${task.key}`, remote_attribution_unknown: !task.attributed };
  });
  return [...normalized, ...projected];
}

export function buildRemoteOverview(events, start, end, minimumSample = 30, countedTaskKeys = null, countedEvents = null, taskGroups = null) {
  const inWindow = event => time(event) >= start && time(event) <= end;
  const raw = namedBehaviorEvents(countedEvents || events, "remote_control_event").filter(inWindow);
  const stageKeys = new Map();
  for (const event of countedEvents || []) {
    if (!event.remote_projection) continue;
    const keys = stageKeys.get(event.event_name) || new Set();
    keys.add(event.remote_task_key); stageKeys.set(event.event_name, keys);
  }
  const allowed = (task, stage) => !countedEvents || stageKeys.get(stage)?.has(task.key);
  // Resolve attribution across the whole retained history before time slicing.
  const tasks = (taskGroups || remoteTaskGroups(events)).filter(t => t.rows.some(inWindow));
  const completed = tasks.filter(t => t.terminal && inWindow(t.terminal) && TERMINAL.has(t.status)
    && (!countedTaskKeys || countedTaskKeys.has(t.key)));
  // Receipt coverage uses the same clipped completion cohort as call counts,
  // while receipt evidence still comes from the full retained task history.
  const knownExecutions = completed.filter(t => t.authoritative
    && t.submitted?.remote_role !== "controlled");
  const awaitingReceipt = knownExecutions.filter(t => !t.received);
  const stageCounts = new Map(), outcomeCounts = new Map();
  const groupedFields = ["remote_permission_step", "remote_size_bucket", "remote_latency_bucket", "remote_model_source"];
  const groups = new Map(groupedFields.map(field => [field, new Map()]));
  const rows = new Map(), controllerIds = new Set(), controlledIds = new Set();
  let legacyCount = 0;
  for (const event of raw) {
    stageCounts.set(event.remote_stage, (stageCounts.get(event.remote_stage) || 0) + 1);
    const outcomeKey = `${event.remote_stage}|${event.remote_outcome}`;
    outcomeCounts.set(outcomeKey, (outcomeCounts.get(outcomeKey) || 0) + 1);
    const key = [event.remote_stage, event.remote_outcome, event.remote_reason].join("|");
    const row = rows.get(key) || { stage: event.remote_stage, outcome: event.remote_outcome, reason: event.remote_reason, count: 0 };
    row.count++; rows.set(key, row);
    const id = identity(event);
    if (id && event.remote_role === "controller") controllerIds.add(id);
    if (id && event.remote_role === "controlled") controlledIds.add(id);
    if (!event.remote_task_key && ["task_submit", "task_admit", "task_rejected", "task_terminal"].includes(event.remote_stage)) legacyCount++;
    for (const field of groupedFields) {
      const value = event[field];
      if (value) groups.get(field).set(value, (groups.get(field).get(value) || 0) + 1);
    }
  }
  const count = (stage, outcome) => outcome
    ? outcomeCounts.get(`${stage}|${outcome}`) || 0 : stageCounts.get(stage) || 0;
  const ratio = (part, total) => total >= minimumSample ? part / total : null;
  const percentile = (values, fraction) => {
    const ordered = values.filter(v => typeof v === "number" && Number.isFinite(v)).sort((a, b) => a - b);
    return ordered.length ? ordered[Math.max(0, Math.ceil(ordered.length * fraction) - 1)] : null;
  };
  const firstOutputDurations = tasks.filter(t => allowed(t, "task_first_output")).flatMap(t => t.rows.find(e => e.remote_stage === "first_output" && inWindow(e))?.remote_duration_ms ?? []);
  const taskDurations = completed.flatMap(t => t.authoritative?.remote_duration_ms ?? []);
  // Wait-time samples belong to the receipt day, unlike the execution-cohort
  // coverage above. Honor the same raw-event contribution clipping as rows.
  const visibleReceipts = new Set(raw.filter(e => e.remote_stage === "result_received").map(e => e.event_id));
  const resultWait = tasks.filter(t => t.received && inWindow(t.received)
    && visibleReceipts.has(t.received.event_id)).flatMap(t => t.received.remote_duration_ms ?? []);
  const visibleTaskKeys = new Set(raw.map(e => e.remote_task_key));
  const visibleTask = task => !countedEvents || visibleTaskKeys.has(task.key);
  const group = field => Object.fromEntries([...groups.get(field)].sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0));
  return {
    task_count: completed.length,
    submitted_count: tasks.filter(t => t.submitted && inWindow(t.submitted) && allowed(t, "task_submitted")).length,
    admitted_count: tasks.filter(t => t.admitted && inWindow(t.admitted) && allowed(t, "task_admitted")).length,
    succeeded_count: completed.filter(t => t.status === "success").length,
    failed_count: completed.filter(t => ["failed", "interrupted"].includes(t.status)).length,
    cancelled_count: completed.filter(t => t.status === "cancelled").length,
    rejected_count: tasks.filter(t => t.rejected && inWindow(t.rejected) && allowed(t, "task_rejected")).length,
    unknown_count: tasks.filter(t => t.status === "unknown" && visibleTask(t)).length,
    pending_count: tasks.filter(t => t.status === "pending" && visibleTask(t)).length,
    not_sent_count: tasks.filter(t => t.status === "not_sent" && visibleTask(t)).length,
    unattributed_count: completed.filter(t => !t.attributed).length,
    controller_installations: controllerIds.size,
    controlled_installations: controlledIds.size,
    success_rate: ratio(completed.filter(t => t.status === "success").length, completed.length),
    execution_terminal_count: knownExecutions.length,
    result_received_count: knownExecutions.filter(t => t.received).length,
    awaiting_receipt_count: awaitingReceipt.length,
    // Missing A telemetry can also mean opt-out/offline, not proven packet loss.
    result_receipt_rate: ratio(knownExecutions.filter(t => t.received).length, knownExecutions.length),
    connection_attempts: count("connection"), connection_successes: count("connection", "ok"),
    connection_success_rate: ratio(count("connection", "ok"), count("connection")),
    disconnect_count: count("disconnection"), resume_attempts: count("resume"), resume_successes: count("resume", "ok"),
    resume_success_rate: ratio(count("resume", "ok"), count("resume")),
    attachment_success_count: count("attachment", "ok"), attachment_failure_count: count("attachment") - count("attachment", "ok"),
    artifact_success_count: count("artifact", "ok"), artifact_failure_count: count("artifact") - count("artifact", "ok"),
    task_modes: Object.fromEntries(["assistant", "code"].map(mode => [mode, completed.filter(t => t.mode === mode).length])),
    task_model_sources: Object.fromEntries(["controlled", "controller_temporary"].map(source => [source, completed.filter(t => t.rows.some(e => e.remote_model_source === source)).length])),
    first_output_p50_ms: percentile(firstOutputDurations, 0.5), first_output_p90_ms: percentile(firstOutputDurations, 0.9),
    task_duration_p50_ms: percentile(taskDurations, 0.5), task_duration_p90_ms: percentile(taskDurations, 0.9),
    result_wait_p50_ms: percentile(resultWait, 0.5), result_wait_p90_ms: percentile(resultWait, 0.9),
    permission_funnel: { request: 0, allowed: 0, denied: 0, expired: 0, ...group("remote_permission_step") }, size_buckets: group("remote_size_bucket"),
    latency_buckets: group("remote_latency_bucket"), model_sources: group("remote_model_source"),
    rows: [...rows.values()].sort((a, b) => a.stage.localeCompare(b.stage) || a.outcome.localeCompare(b.outcome) || a.reason.localeCompare(b.reason)), event_count: raw.length,
    legacy_event_count: legacyCount,
  };
}
