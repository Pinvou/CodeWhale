import assert from "node:assert/strict";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { queryControllerReleaseTelemetry } from "../scripts/query-controller-release-telemetry.mjs";

const commit = "d".repeat(40);
const now = Date.parse("2026-09-28T10:00:00.000Z");

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-telemetry-query-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const tokenPath = join(root, "admin-token");
  await writeFile(tokenPath, "release-admin-token-value-123456789", "utf8");
  if (process.platform !== "win32") await chmod(tokenPath, 0o600);
  return { PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL: "https://telemetry.example.invalid/release-snapshot", PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE: tokenPath, PINVOU_RELEASE_COMMIT_SHA: commit };
}

test("release telemetry gate actively queries a protected, fresh production snapshot", async (t) => {
  const env = await fixture(t);
  let authorization;
  const result = await queryControllerReleaseTelemetry(env, async (_url, options) => {
    authorization = options.headers.authorization;
    return { ok: true, async json() { return { schema: "pinvou-controller-release-telemetry", version: 1, commit_sha: commit, source: "production", sampled_at: "2026-09-28T09:55:00.000Z", window_seconds: 600, thresholds_pass: true }; } };
  }, now);
  assert.equal(authorization, "Bearer release-admin-token-value-123456789");
  assert.deepEqual(result, { ok: true, sampled_at: "2026-09-28T09:55:00.000Z", window_seconds: 600 });
});

test("release telemetry gate rejects stale, failed-threshold and wrong-commit snapshots", async (t) => {
  const env = await fixture(t);
  const base = { schema: "pinvou-controller-release-telemetry", version: 1, commit_sha: commit, source: "production", sampled_at: "2026-09-28T09:55:00.000Z", window_seconds: 600, thresholds_pass: true };
  for (const mutation of [
    { sampled_at: "2026-09-28T09:40:00.000Z" },
    { thresholds_pass: false },
    { commit_sha: "e".repeat(40) },
  ]) {
    await assert.rejects(
      () => queryControllerReleaseTelemetry(env, async () => ({ ok: true, async json() { return { ...base, ...mutation }; } }), now),
      /release_telemetry_(?:snapshot_stale|response_invalid)/u,
    );
  }
});
