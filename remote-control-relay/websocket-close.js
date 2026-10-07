import { WebSocket } from "ws";

export function normalizeUpstreamClose(code, rawReason) {
  const forwardable = Number.isInteger(code)
    && ((code >= 1000 && code <= 1014 && ![1004, 1005, 1006].includes(code))
      || (code >= 3000 && code <= 4999));
  const source = Buffer.isBuffer(rawReason) ? rawReason.toString("utf8")
    : typeof rawReason === "string" ? rawReason : "";
  let reason = "";
  let bytes = 0;
  // Real ws close frames already cap the reason at 123 bytes. Keep the same
  // bound defensively for internal callers, without splitting Unicode points.
  for (const point of source) {
    const size = Buffer.byteLength(point, "utf8");
    if (bytes + size > 123) break;
    reason += point;
    bytes += size;
  }
  return Object.freeze({ code: forwardable ? code : 1011, reason });
}

export function closeClientFromUpstream(client, code, reason) {
  if (client.readyState !== WebSocket.OPEN) return;
  try {
    const normalized = normalizeUpstreamClose(code, reason);
    client.close(normalized.code, normalized.reason);
  } catch {
    // A transport implementation/race must not escape an EventEmitter callback
    // and kill the public gateway. Only this connection is affected.
    try { client.terminate(); } catch { /* Already closed. */ }
  }
}
