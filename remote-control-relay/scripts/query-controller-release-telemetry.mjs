#!/usr/bin/env node
import { lstat, readFile, realpath } from "node:fs/promises";
import { resolve } from "node:path";
import { isDirectCliEntry } from "../cli-entrypoint.js";

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

export async function queryControllerReleaseTelemetry(env = process.env, fetchImpl = fetch, now = Date.now()) {
  let url;
  try { url = new URL(env.PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL); } catch { fail("release_telemetry_url_invalid"); }
  if (url.protocol !== "https:" || url.username || url.password || url.hash || url.search) fail("release_telemetry_url_invalid");
  const tokenPath = resolve(env.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE || "");
  const metadata = await lstat(tokenPath).catch(() => fail("release_telemetry_token_unavailable"));
  if (!metadata.isFile() || metadata.isSymbolicLink() || (process.platform !== "win32" && (metadata.mode & 0o077) !== 0)) fail("release_telemetry_token_unsafe");
  if (await realpath(tokenPath) !== tokenPath) fail("release_telemetry_token_unsafe");
  const token = (await readFile(tokenPath, "utf8")).trim();
  if (token.length < 24 || /[\r\n]/u.test(token)) fail("release_telemetry_token_invalid");
  let response;
  try {
    response = await fetchImpl(url, {
      method: "GET", redirect: "error", signal: AbortSignal.timeout(5_000),
      headers: { authorization: `Bearer ${token}`, accept: "application/json" },
    });
  } catch { fail("release_telemetry_query_failed"); }
  if (!response.ok) fail("release_telemetry_query_failed");
  let snapshot;
  try { snapshot = await response.json(); } catch { fail("release_telemetry_response_invalid"); }
  const keys = ["schema", "version", "commit_sha", "source", "sampled_at", "window_seconds", "thresholds_pass"];
  if (!snapshot || typeof snapshot !== "object" || Array.isArray(snapshot)
    || JSON.stringify(Object.keys(snapshot).sort()) !== JSON.stringify(keys.sort())
    || snapshot.schema !== "pinvou-controller-release-telemetry" || snapshot.version !== 1
    || snapshot.commit_sha !== env.PINVOU_RELEASE_COMMIT_SHA || snapshot.source !== "production"
    || snapshot.thresholds_pass !== true || !Number.isSafeInteger(snapshot.window_seconds)
    || snapshot.window_seconds < 300 || snapshot.window_seconds > 3_600) fail("release_telemetry_response_invalid");
  const sampledAt = Date.parse(snapshot.sampled_at);
  if (!Number.isFinite(sampledAt) || sampledAt > now + 30_000 || now - sampledAt > 600_000) fail("release_telemetry_snapshot_stale");
  return { ok: true, sampled_at: snapshot.sampled_at, window_seconds: snapshot.window_seconds };
}

if (isDirectCliEntry(import.meta.url)) {
  queryControllerReleaseTelemetry()
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "release_telemetry_query_failed" })}\n`);
      process.exitCode = 1;
    });
}
