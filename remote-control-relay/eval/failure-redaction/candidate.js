// Offline comparison only. This module is not imported by the application or relay.
import { createHmac } from "node:crypto";

const credentialKeys =
  /^(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|client[_-]?secret|secret|password|passwd|authorization|cookie|set-cookie)$/i;
const contentKeys = /^(?:prompt|messages|request_body|response_body|user_input|output_text)$/i;
const idKeys = /^(?:(?:request|task|session|trace|event|operation|correlation)_id|x-request-id)$/i;
const safeEndpoints = new Set([
  "/v1/chat/completions",
  "/v1/responses",
  "/relay/events",
  "/relay/tasks",
]);

/** A report-scoped prototype: aliases are stable within one report, never global. */
export function fieldRedactor(reportSalt) {
  const alias = (kind, value) =>
    `[${kind}:${createHmac("sha256", reportSalt).update(String(value)).digest("hex").slice(0, 12)}]`;
  const already = (value) =>
    /^\[(?:id|file|service|endpoint|email):[a-f0-9]{12}(?:[^\]]*)\]$/.test(value);
  function text(input) {
    return String(input)
      .replace(/\bBearer\s+[^\s"',;]+/gi, "Bearer [removed]")
      .replace(
        /\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)/g,
        "[removed]",
      )
      .replace(
        /(["']?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|password|passwd|token|secret|authorization|cookie)["']?\s*[=:]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)/gi,
        "$1[removed]",
      )
      .replace(
        /(["']?(?:prompt|request_body|response_body|user_input|output_text)["']?\s*[=:]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)/gi,
        "$1[content_removed]",
      )
      .replace(/https?:\/\/[^\s<>"']+/gi, (address) => {
        try {
          const url = new URL(address);
          return `${alias("service", url.origin)}${safeEndpoints.has(url.pathname) ? url.pathname : "/[endpoint removed]"}`;
        } catch {
          return "[endpoint removed]";
        }
      })
      .replace(/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi, (value) => alias("email", value))
      .replace(
        /(?:[A-Za-z]:[\\/]|\\\\|~\/|\/(?:Users|home|tmp|var|private|opt|mnt)\/)[^\s<>"']+/g,
        (value) => {
          const extension = /\.([a-z0-9]{1,8})$/i.exec(value)?.[1];
          return alias("file", value) + (extension ? `.${extension}` : "");
        },
      )
      .replace(
        /\b((?:request|task|session|trace|event|operation|correlation)_id|x-request-id)\s*[=:]\s*([^\s,;"']+)/gi,
        (whole, key, value) => `${key}=${already(value) ? value : alias("id", value)}`,
      )
      .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, "");
  }
  function visit(value, key = "") {
    if (credentialKeys.test(key)) return "[removed]";
    if (contentKeys.test(key)) return "[content removed]";
    if (idKeys.test(key)) return already(String(value)) ? value : alias("id", value);
    if (Array.isArray(value)) return value.map((item) => visit(item));
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([name, item]) => [name, visit(item, name)]),
      );
    return typeof value === "string" ? text(value) : value;
  }
  return (input) =>
    String(input)
      .split(/\r?\n/)
      .map((line) => {
        try {
          const value = JSON.parse(line);
          if (value && typeof value === "object") return JSON.stringify(visit(value));
        } catch {
          /* Normal log text is not JSON. */
        }
        return text(line);
      })
      .join("\n");
}
