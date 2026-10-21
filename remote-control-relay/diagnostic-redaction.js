// Detailed, explicitly previewed reports only. Unknown structured values are not evidence.
import { createHash, randomBytes } from "node:crypto";

const removed = "[removed]";
const content = new Set([
  "prompt",
  "messages",
  "requestbody",
  "responsebody",
  "userinput",
  "outputtext",
  "body",
  "content",
  "input",
  "output",
  "stderr",
  "stdout",
  "message",
  "stack",
]);
const credentials = new Set([
  "apikey",
  "accesskey",
  "accesskeyid",
  "secretaccesskey",
  "accesstoken",
  "refreshtoken",
  "idtoken",
  "token",
  "clientsecret",
  "secret",
  "password",
  "passwd",
  "authorization",
  "proxyauthorization",
  "cookie",
  "setcookie",
  "privatekey",
  "credentials",
]);
const ids = new Set([
  "requestid",
  "taskid",
  "sessionid",
  "traceid",
  "eventid",
  "operationid",
  "correlationid",
  "xrequestid",
]);
const containers = new Set([
  "error",
  "errors",
  "headers",
  "fault",
  "diagnostic",
  "details",
  "context",
  "environment",
  "cause",
  "result",
  "events",
  "steps",
]);
const numbers = new Set([
  "httpstatus",
  "statuscode",
  "exitcode",
  "errno",
  "inputtokens",
  "outputtokens",
  "totaltokens",
  "limittokens",
  "limit",
  "retryafterms",
  "retryafter",
  "deadlinems",
  "elapsedms",
  "durationms",
  "offsetms",
  "receivedframes",
  "expectedframes",
  "attempt",
  "attempts",
]);
const booleans = new Set([
  "billingenabled",
  "executionstarted",
  "retryable",
  "hasattachment",
  "responsebodyunavailable",
  "retryafterunavailable",
  "inferred",
]);
const categories = new Set([
  "code",
  "errorcode",
  "diagnosticcode",
  "transportcode",
  "component",
  "diagnosticcomponent",
  "stage",
  "phase",
  "event",
  "status",
  "ataskstatus",
  "btaskstatus",
  "previoustaskstatus",
  "relayreason",
  "tool",
  "filerole",
  "op",
  "mode",
  "remotetaskpermissions",
]);
// Closed vocabulary: unknown provider messages/identifiers may contain private content.
const vocabulary = new Set(
  `unknown permission_denied permission_required permission_changed authentication_failed configuration_missing network_unavailable connection_reset dns_failed tls_failed request_timeout rate_limited quota_exhausted context_length_exceeded model_not_found provider_unavailable stream_unexpected_eof response_invalid tool_failed process_failed unsupported validation_failed internal_error shutdown_unconfirmed capability_stale remote_disconnected remote_result_unknown artifact_missing attachment_failed user_reported_quality key_revoked invalid_api_key invalid_api_key_error invalid_authentication_error authentication_error unauthorized forbidden rate_limit_exceeded insufficient_quota upstream_unavailable stream_incomplete upstream_reset deadline_exceeded task_permission_mode_required endpoint_not_live result_delivery_rejected request_started render_worker_failed ECONNRESET ENOENT EACCES EPERM ETIMEDOUT ENOTFOUND EPIPE ECONNREFUSED DEADLINE_EXCEEDED model proxy filesystem configuration transport app assistant acp tool remote_control voice unknown submission queue permission recording recognition output shutdown response_headers response_read forward_response connect read write open spawn succeeded failed waiting running pending interrupted completed rejected cancelled canceled success error ok unavailable renderer shell bash powershell web web_search read_file edit_file search_files model_config yolo plan`.split(
    /\s+/,
  ),
);
const normalize = (key) => key.replace(/[-_]/g, "").toLowerCase();
const spellings =
  `api_key access_key access_key_id secret_access_key access_token refresh_token id_token client_secret proxy_authorization set-cookie private_key request_body response_body user_input output_text request_id task_id session_id trace_id event_id operation_id correlation_id x-request-id http_status status_code exit_code input_tokens output_tokens total_tokens limit_tokens retry_after_ms retry_after deadline_ms elapsed_ms duration_ms offset_ms received_frames expected_frames billing_enabled execution_started has_attachment response_body_unavailable retry_after_unavailable error_code diagnostic_code transport_code diagnostic_component a_task_status b_task_status previous_task_status relay_reason file_role remote_task_permissions`.split(
    /\s+/,
  );
const display = new Map(spellings.map((key) => [normalize(key), key]));
const safeMarker = (line) => [removed, "[unparsed material removed]", "[truncated]"].includes(line);
const aliasPattern = /^\[(?:id|value):[a-f0-9]{24}\]$/;

export function diagnosticRedactor(
  scope = randomBytes(32).toString("hex"),
  { stored = false } = {},
) {
  const alias = (kind, value) =>
    `[${kind}:${createHash("sha256")
      .update(JSON.stringify([scope, kind, value]))
      .digest("hex")
      .slice(0, 24)}]`;
  function scalar(key, value) {
    if (ids.has(key)) {
      if (typeof value !== "string" || value.length === 0 || Buffer.byteLength(value) > 1024)
        return removed;
      return stored && aliasPattern.test(value) ? value : alias("id", value);
    }
    if (booleans.has(key)) return typeof value === "boolean" ? value : removed;
    if (numbers.has(key) || (key === "status" && typeof value === "number")) {
      if (!Number.isSafeInteger(value)) return removed;
      if (["httpstatus", "statuscode", "status"].includes(key))
        return value >= 100 && value <= 599 ? value : removed;
      if (key === "exitcode") return value >= -2147483648 && value <= 2147483647 ? value : removed;
      return value >= 0 && value <= 1000000000 ? value : removed;
    }
    if (categories.has(key)) {
      if (key === "remotetaskpermissions" && value === null) return null;
      if (typeof value !== "string" || Buffer.byteLength(value) > 128) return removed;
      return vocabulary.has(value) || (stored && aliasPattern.test(value))
        ? value
        : alias("value", value);
    }
    return removed;
  }
  function visit(value, depth = 0) {
    if (depth > 16) return removed;
    if (Array.isArray(value)) return value.slice(0, 128).map((item) => visit(item, depth + 1));
    if (!value || typeof value !== "object") return removed;
    const result = {};
    for (const [key, item] of Object.entries(value).slice(0, 256)) {
      const name = normalize(key);
      const outputKey = display.get(name) || name;
      if (Object.hasOwn(result, outputKey)) {
        result[outputKey] = removed;
        continue;
      }
      // Never return unknown original key names, which can themselves be private.
      if (content.has(name) || credentials.has(name)) result[outputKey] = removed;
      else if (containers.has(name)) result[outputKey] = visit(item, depth + 1);
      else if (ids.has(name) || numbers.has(name) || booleans.has(name) || categories.has(name))
        result[outputKey] = scalar(name, item);
      else result.unknown_fields_removed = true;
    }
    return result;
  }
  function keyValues(line) {
    // This is a complete record grammar, not a search for safe-looking substrings.
    const pairs = [];
    let offset = 0;
    while (offset < line.length) {
      while (/\s/.test(line[offset] || "") && offset < line.length) offset++;
      if (offset === line.length) break;
      const head = /^([A-Za-z][A-Za-z0-9_-]{0,63})\s*[=:]\s*/.exec(line.slice(offset));
      if (!head) return null;
      offset += head[0].length;
      const start = offset;
      if (line[offset] === '"') {
        offset++;
        while (offset < line.length && line[offset] !== '"') {
          if (line[offset] === "\\") offset++;
          offset++;
        }
        if (line[offset] !== '"') return null;
        offset++;
      } else {
        while (offset < line.length && !/\s/.test(line[offset])) offset++;
      }
      if (start === offset || (offset < line.length && !/\s/.test(line[offset]))) return null;
      const raw = line.slice(start, offset);
      let value;
      try {
        value = JSON.parse(raw);
      } catch {
        value = raw;
      }
      pairs.push([normalize(head[1]), value]);
      if (pairs.length > 256) return null;
    }
    return pairs;
  }
  return (input) => {
    const text = String(input ?? "");
    if (Buffer.byteLength(text) > 65536) return "[unparsed material removed]";
    // Pretty-printed JSON is parsed as a whole, including multiline sensitive parents.
    try {
      const value = JSON.parse(text);
      return JSON.stringify(visit(value));
    } catch {
      /* NDJSON / plain log records follow. */
    }
    const lines = text.split(/\r?\n/);
    // Unclosed/pretty malformed structured documents cannot be safely recovered by line.
    if (
      /^[\s]*[\[{]/.test(text) &&
      !lines.every((line) => {
        if (!line.trim() || safeMarker(line)) return true;
        try {
          const value = JSON.parse(line);
          return value && typeof value === "object";
        } catch {
          return false;
        }
      })
    )
      return "[unparsed material removed]";
    const output = [];
    let blocked = false;
    for (const line of lines) {
      if (blocked) {
        output.push(removed);
        continue;
      }
      if (safeMarker(line)) {
        output.push(line);
        continue;
      }
      try {
        const value = JSON.parse(line);
        output.push(JSON.stringify(visit(value)));
        continue;
      } catch {
        /* A complete key=value record or explicitly reviewed prose. */
      }
      const pairs = keyValues(line);
      if (pairs?.length) {
        const record = {};
        for (const [name, value] of pairs) {
          const outputKey = display.get(name) || name;
          if (Object.hasOwn(record, outputKey)) {
            record[outputKey] = removed;
            blocked = true;
            continue;
          }
          if (content.has(name) || credentials.has(name)) record[outputKey] = removed;
          else if (
            ids.has(name) ||
            numbers.has(name) ||
            booleans.has(name) ||
            categories.has(name)
          ) {
            const cleaned = scalar(name, value);
            record[outputKey] = cleaned;
            if (
              cleaned === removed ||
              (categories.has(name) && typeof cleaned === "string" && cleaned.startsWith("[value:"))
            )
              blocked = true;
          } else record.unknown_fields_removed = true;
        }
        output.push(JSON.stringify(record));
        // A plain content/header value has no provable multiline end. Do not resume.
        if (
          pairs.some(
            ([name]) =>
              content.has(name) ||
              credentials.has(name) ||
              !(ids.has(name) || numbers.has(name) || booleans.has(name) || categories.has(name)),
          )
        )
          blocked = true;
      } else {
        // Prose is useful for the user's reproduction notes, but anything ambiguous
        // (assignments, encodings, credentials, paths, control codes) fails closed.
        const risky =
          /[=:{\[<\\/@%"'`]|~~~|(?:AKIA|ASIA)[A-Z0-9]{16}|bearer|password|passwd|token|secret|cookie|authorization|api[_-]?key|prompt|messages|content|body|sk-|gh[pousr]_|github_pat_|begin|[A-Za-z0-9+_-]{32,}|[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2060-\u206f\ufeff]/i.test(
            line,
          );
        output.push(risky ? removed : line);
        if (risky) blocked = true;
      }
    }
    return output.join("\n");
  };
}
