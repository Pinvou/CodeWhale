import { integerMoney } from "./finance-contract.js";
import { adminModelTokenQueryUpdates } from "./admin-navigation.js";

const COUNTS = new Set([
  "latency_ms",
  "pending_events",
  "dead_lettered_events",
  "pending",
  "stopped",
]);

// These fields are int64 in the Portal read DTO. Preserve their wire digits;
// relay age remains its existing floating-point seconds value.
export function parseSystemStatusJSON(text) {
  let result = "";
  for (let i = 0; i < text.length; ) {
    if (text[i] !== '"') {
      result += text[i++];
      continue;
    }
    const start = i++;
    while (i < text.length) {
      if (text[i] === "\\") {
        i += 2;
        continue;
      }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i);
    result += token;
    let cursor = i;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    if (text[cursor] !== ":" || !COUNTS.has(JSON.parse(token))) continue;
    cursor++;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    const match = text.slice(cursor).match(/^-?(?:0|[1-9]\d*)(?=\s*[,}\]])/);
    if (!match) continue;
    result += text.slice(i, cursor) + JSON.stringify(match[0]);
    i = cursor + match[0].length;
  }
  return JSON.parse(result);
}

export function formatSystemCount(value, language) {
  if (!["string", "number"].includes(typeof value)) return "—";
  try {
    const count = integerMoney(value);
    return count < 0n ? "—" : count.toLocaleString(language);
  } catch {
    return "—";
  }
}

export function requireSystemStatus(value) {
  if (
    !value ||
    typeof value !== "object" ||
    !Array.isArray(value.services) ||
    !Array.isArray(value.relays) ||
    !Array.isArray(value.recent_errors) ||
    !value.reconciliation ||
    typeof value.reconciliation !== "object" ||
    Array.isArray(value.reconciliation)
  ) {
    throw new Error("INVALID_SYSTEM_STATUS");
  }
  for (const item of [...value.services, ...value.relays]) {
    if (!item || typeof item.key !== "string" || typeof item.status !== "string" ||
      (item.message !== undefined && typeof item.message !== "string") ||
      (item.code !== undefined && typeof item.code !== "string"))
      throw new Error("INVALID_SYSTEM_STATUS");
  }
  for (const item of value.recent_errors) {
    if (!item || typeof item.source !== "string" || typeof item.message !== "string" ||
      (item.resource !== undefined && typeof item.resource !== "string") ||
      (item.updated_at !== undefined && typeof item.updated_at !== "string") ||
      (item.code !== undefined && typeof item.code !== "string"))
      throw new Error("INVALID_SYSTEM_STATUS");
  }
  return value;
}

const diagnosticLabelKeys = Object.freeze({
  STATUS_ENDPOINT_NOT_CONFIGURED: "statusEndpointNotConfigured",
  PROBE_REQUEST_TIMED_OUT: "probeRequestTimedOut",
  PROBE_CONNECTION_FAILED: "probeConnectionFailed",
  PROBE_HTTP_ERROR: "probeHTTPError",
  PROBE_RESPONSE_INVALID: "probeResponseInvalid",
  PROBE_ASSERTION_FAILED: "probeAssertionFailed",
  RELAY_METRICS_HTTP_ERROR: "relayMetricsHTTPError",
  RELAY_METRICS_INVALID: "relayMetricsInvalid",
  RELAY_DEAD_LETTERED_EVENTS: "relayDeadLetteredEvents",
});

export function systemDiagnosticMessage(item, labels) {
  if (Object.prototype.hasOwnProperty.call(diagnosticLabelKeys, item.code)) {
    const label = labels[diagnosticLabelKeys[item.code]];
    // Only our bounded transport formats contribute details; arbitrary external
    // messages still follow the unchanged fallback below.
    const detail = item.code === "PROBE_HTTP_ERROR"
      ? /^HTTP ([1-9][0-9]{2})$/.exec(item.message || "")
      : item.code === "RELAY_METRICS_HTTP_ERROR"
        ? /^metrics HTTP ([1-9][0-9]{2})$/.exec(item.message || "")
        : item.code === "RELAY_DEAD_LETTERED_EVENTS"
          ? /^(0|[1-9][0-9]{0,18}) dead-lettered events$/.exec(item.message || "")
          : null;
    return detail ? `${label} (${detail[1]})` : label;
  }
  if (item.code === "RECONCILIATION_STATUS_UNAVAILABLE") return labels.reconciliationUnavailable;
  if (item.code === "RECONCILIATION_ERRORS_UNAVAILABLE") return labels.reconciliationErrorsUnavailable;
  return item.message;
}

// This is a related review queue, not an exact stopped-usage filter. Keep its
// permission and route contract separate from the system-status read scope.
export function systemRelatedReconciliationRoute(available = [], permissions = []) {
  if (!permissions.includes("platform.usage.read")) return null;
  const section = available.includes("modelOperations") ? "modelOperations" : available.includes("tokens") ? "tokens" : null;
  return section ? { section, query: {
    operations_tab: "reconciliation", usage_request_id: null,
    ...adminModelTokenQueryUpdates("reconciliation", { case_type: "USAGE_RECONCILIATION_REVIEW" }),
  } } : null;
}
