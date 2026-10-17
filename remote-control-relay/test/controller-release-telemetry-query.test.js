import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { queryControllerReleaseTelemetry } from "../scripts/query-controller-release-telemetry.mjs";

const commit = "d".repeat(40);
const now = Date.parse("2026-09-28T10:00:00.000Z");

async function fixture(t) {
  const root = await realpath(await mkdtemp(join(tmpdir(), "pinvou-telemetry-query-")));
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

const snapshot = () => ({ schema: 'pinvou-controller-release-telemetry', version: 1,
  commit_sha: commit, source: 'production', sampled_at: '2026-09-28T09:55:00.000Z',
  window_seconds: 600, thresholds_pass: true });
const success = async () => ({ ok: true, json: async () => snapshot() });
const rejectsWith = (code) => (error) => {
  assert.equal(error.code, code);
  assert.equal(error.message, code, 'diagnostics must not expose token, URL or path');
  return true;
};

test('unsafe telemetry URLs fail before any request or credential forwarding', async (t) => {
  const env = await fixture(t);
  for (const url of [undefined, 'not a URL', 'http://telemetry.example.invalid/snapshot',
    'https://user:password@telemetry.example.invalid/snapshot',
    'https://telemetry.example.invalid/snapshot#fragment', 'https://telemetry.example.invalid/snapshot?token=fixture']) {
    let calls = 0;
    await assert.rejects(() => queryControllerReleaseTelemetry({ ...env, PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL: url },
      async () => { calls++; return success(); }, now), rejectsWith('release_telemetry_url_invalid'));
    assert.equal(calls, 0);
  }
});

test('missing, directory, short and embedded-CRLF credentials never reach fetch', async (t) => {
  const cases = [
    ['missing', 'release_telemetry_token_unavailable', async (path) => rm(path)],
    ['directory', 'release_telemetry_token_unsafe', async (path) => { await rm(path); await mkdir(path); }],
    ['short', 'release_telemetry_token_invalid', async (path) => writeFile(path, 'short')],
    ['CRLF', 'release_telemetry_token_invalid', async (path) => writeFile(path, 'fixture-admin-token-123456789\r\nsecond-line')],
  ];
  for (const [name, code, change] of cases) {
    await t.test(name, async (child) => {
      const local = await fixture(child);
      await change(local.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE);
      let calls = 0;
      await assert.rejects(() => queryControllerReleaseTelemetry(local, async () => { calls++; return success(); }, now), rejectsWith(code));
      assert.equal(calls, 0);
    });
  }
});

test('POSIX world-readable credentials fail before fetch', { skip: process.platform === 'win32' ? 'POSIX mode contract; not Windows DACL evidence' : false }, async (t) => {
  const env = await fixture(t);
  await chmod(env.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE, 0o644);
  let calls = 0;
  await assert.rejects(() => queryControllerReleaseTelemetry(env, async () => { calls++; return success(); }, now), rejectsWith('release_telemetry_token_unsafe'));
  assert.equal(calls, 0);
});

test('symlink token is rejected before fetch', async (t) => {
  const env = await fixture(t);
  const link = `${env.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE}.link`;
  try { await symlink(env.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE, link); }
  catch (error) { if (['EPERM', 'EACCES', 'ENOTSUP'].includes(error.code)) { t.skip(`file symlink unavailable: ${error.code}; no symlink evidence`); return; } throw error; }
  let calls = 0;
  await assert.rejects(() => queryControllerReleaseTelemetry({ ...env, PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE: link },
    async () => { calls++; return success(); }, now), rejectsWith('release_telemetry_token_unsafe'));
  assert.equal(calls, 0);
});

test('noncanonical credential path through directory alias fails before fetch', async (t) => {
  const env = await fixture(t);
  const root = env.PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE.slice(0, -'admin-token'.length);
  const actual = join(root, 'private'); await mkdir(actual);
  await writeFile(join(actual, 'admin-token'), 'fixture-admin-token-123456789');
  if (process.platform !== 'win32') await chmod(join(actual, 'admin-token'), 0o600);
  const alias = join(root, 'alias');
  try { await symlink(actual, alias, process.platform === 'win32' ? 'junction' : 'dir'); }
  catch (error) { if (['EPERM', 'EACCES', 'ENOTSUP'].includes(error.code)) { t.skip(`directory alias unavailable: ${error.code}`); return; } throw error; }
  let calls = 0;
  await assert.rejects(() => queryControllerReleaseTelemetry({ ...env, PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE: join(alias, 'admin-token') },
    async () => { calls++; return success(); }, now), rejectsWith('release_telemetry_token_unsafe'));
  assert.equal(calls, 0);
});

test('fetch failures, non-success responses and non-JSON responses fail closed', async (t) => {
  const env = await fixture(t);
  for (const [fetchImpl, code] of [
    [async () => { throw new Error('fixture-secret-must-not-leak'); }, 'release_telemetry_query_failed'],
    [async () => ({ ok: false, json: async () => snapshot() }), 'release_telemetry_query_failed'],
    [async () => ({ ok: true, json: async () => { throw new Error('fixture-secret-must-not-leak'); } }), 'release_telemetry_response_invalid'],
  ]) await assert.rejects(() => queryControllerReleaseTelemetry(env, fetchImpl, now), rejectsWith(code));
});

test('response schema, production source, time window and freshness reject every mutation', async (t) => {
  const env = await fixture(t);
  const cases = [null, [], {}, { ...snapshot(), extra: true }, { ...snapshot(), schema: 'other' },
    { ...snapshot(), version: 2 }, { ...snapshot(), source: 'development' },
    { ...snapshot(), commit_sha: 'e'.repeat(40) }, { ...snapshot(), thresholds_pass: false },
    ...[299, 3601, 300.5, '600'].map((window_seconds) => ({ ...snapshot(), window_seconds })),
  ];
  for (const value of cases) await assert.rejects(() => queryControllerReleaseTelemetry(env,
    async () => ({ ok: true, json: async () => value }), now), rejectsWith('release_telemetry_response_invalid'));
  for (const sampled_at of ['invalid date', '2026-09-28T10:00:31.000Z', '2026-09-28T09:49:59.000Z']) {
    await assert.rejects(() => queryControllerReleaseTelemetry(env,
      async () => ({ ok: true, json: async () => ({ ...snapshot(), sampled_at }) }), now), rejectsWith('release_telemetry_snapshot_stale'));
  }
  for (const [sampled_at, window_seconds] of [['2026-09-28T10:00:30.000Z', 300], ['2026-09-28T09:50:00.000Z', 3600]]) {
    assert.deepEqual(await queryControllerReleaseTelemetry(env, async () => ({ ok: true,
      json: async () => ({ ...snapshot(), sampled_at, window_seconds }) }), now), { ok: true, sampled_at, window_seconds });
  }
});
