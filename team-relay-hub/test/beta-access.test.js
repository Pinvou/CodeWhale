import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { loadConfig } from "../src/config.js";
import { createHubServer } from "../src/http.js";

const digest = value => createHash("sha256").update(value).digest("hex");

test("beta access is verified only by the Hub", async t => {
  const server = createHubServer({
    pool: { query: async () => ({ rows: [] }) },
    config: {
      publicBasePath: "/pinvou3/team-relay",
      betaAccessPasswordHash: digest("test-only-access-code"),
      maxJsonBytes: 1024,
      trustProxy: false,
      joinRateLimitFailures: 5,
      joinRateLimitWindowSeconds: 600,
      joinRateLimitBlockSeconds: 900,
    },
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  t.after(async () => {
    server.closeEventStreams();
    await new Promise(resolve => server.close(resolve));
  });
  const endpoint = `http://127.0.0.1:${server.address().port}/pinvou3/team-relay/v1/beta-access/verify`;

  const rejected = await fetch(endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ access_code: "incorrect" }),
  });
  assert.equal(rejected.status, 403);
  assert.equal((await rejected.json()).error.code, "beta_access_invalid");

  const accepted = await fetch(endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ access_code: "test-only-access-code" }),
  });
  assert.equal(accepted.status, 200);
  assert.deepEqual(await accepted.json(), { authorized: true });
});

test("beta access digest configuration fails closed", () => {
  const base = { TEAM_RELAY_DATABASE_URL: "postgresql://test.invalid/team_relay" };
  assert.equal(loadConfig(base).betaAccessPasswordHash, "");
  assert.equal(
    loadConfig({ ...base, TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH: digest("test-only-access-code").toUpperCase() }).betaAccessPasswordHash,
    digest("test-only-access-code"),
  );
  assert.throws(
    () => loadConfig({ ...base, TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH: "not-a-digest" }),
    /must be an SHA-256 hex digest/,
  );
});
