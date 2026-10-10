import { integerMoney } from "./finance-contract.js";

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
      (item.message !== undefined && typeof item.message !== "string"))
      throw new Error("INVALID_SYSTEM_STATUS");
  }
  for (const item of value.recent_errors) {
    if (!item || typeof item.source !== "string" || typeof item.message !== "string" ||
      (item.resource !== undefined && typeof item.resource !== "string") ||
      (item.updated_at !== undefined && typeof item.updated_at !== "string"))
      throw new Error("INVALID_SYSTEM_STATUS");
  }
  return value;
}
