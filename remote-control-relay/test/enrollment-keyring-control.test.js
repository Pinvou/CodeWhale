import assert from "node:assert/strict";
import path from "node:path";
import os from "node:os";
import test from "node:test";
import { loadEnrollmentCoverageControlConfig, requestEnrollmentCoverage } from "../enrollment-coverage-control.js";

const OLD_KEY = "old-key-test-only-material-at-least-32-bytes";
const OTHER_KEY = "other-key-test-only-material-at-least-32-bytes";
function environment(previous) {
  return {
    NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: path.join(os.tmpdir(), "keyring-controller", "state.json"),
    PINVOU_REMOTE_STATE_PATH: path.join(os.tmpdir(), "keyring-legacy.json"),
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "active-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "active-key-test-only-material-at-least-32-bytes",
    PINVOU_REMOTE_ENROLLMENT_HMAC_PREVIOUS_KEYS_JSON: JSON.stringify(previous),
    PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1",
    PINVOU_REMOTE_GATEWAY_CONTROL_PORT: "8791",
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: "control-key-test-only-material-at-least-32-bytes",
  };
}

async function binding(env) {
  let captured;
  await assert.rejects(requestEnrollmentCoverage({ endpointIds: [], env,
    fetchImpl: async (_target, options) => {
      captured = JSON.parse(options.body.toString("utf8"));
      throw new Error("test transport capture");
    },
  }), { code: "coverage_control_unavailable" });
  assert.match(captured.store_binding, /^[a-f0-9]{64}$/u);
  const wire = JSON.stringify(captured);
  assert.equal(wire.includes(OLD_KEY), false);
  assert.equal(wire.includes(OTHER_KEY), false);
  assert.equal(wire.includes("keyring-controller"), false);
  return captured.store_binding;
}

test("coverage binding includes previous key IDs and material with canonical order", async () => {
  const previous = [{ key_id: "old", key: OLD_KEY }, { key_id: "other", key: OTHER_KEY }];
  const original = await binding(environment(previous));
  assert.equal(await binding(environment([...previous].reverse())), original);
  assert.notEqual(await binding(environment(previous.slice(0, 1))), original);
  assert.notEqual(await binding(environment([{ key_id: "old", key: OTHER_KEY }, previous[1]])), original);
  assert.notEqual(await binding(environment([{ key_id: "renamed", key: OLD_KEY }, previous[1]])), original);
  assert.notEqual(await binding(environment([])), original);
});

test("coverage control secret must be independent of every retained enrollment key", () => {
  const env = environment([{ key_id: "old", key: OLD_KEY }]);
  assert.doesNotThrow(() => loadEnrollmentCoverageControlConfig(env, { required: true }));
  assert.throws(() => loadEnrollmentCoverageControlConfig({ ...env,
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: OLD_KEY }, { required: true }),
  error => error.code === "coverage_control_key_invalid" && error.message === error.code && !error.cause);
});

test("coverage without retained keys preserves the legacy v1 binding", async () => {
  const env = environment([]);
  env.PINVOU_REMOTE_CONTROLLER_STATE_PATH = process.platform === "win32"
    ? "C:\\pinvou-test-controller\\state.json" : "/pinvou-test-controller/state.json";
  env.PINVOU_REMOTE_STATE_PATH = process.platform === "win32"
    ? "C:\\pinvou-test-legacy.json" : "/pinvou-test-legacy.json";
  // Fixed legacy-v1 protocol vectors; not derived from the current implementation.
  const expected = process.platform === "win32"
    ? "cd0568dda8acba404c191f158e0a35b52b1bf73a35c14f1c830552181310d0e0"
    : "3107b565549231cd22e91a62a072c0f87d8f2a0a1cc2d1d1e8172870e7ba406a";
  assert.equal(await binding(env), expected);
  delete env.PINVOU_REMOTE_ENROLLMENT_HMAC_PREVIOUS_KEYS_JSON;
  assert.equal(await binding(env), expected);
});
