import assert from "node:assert/strict";
import { test } from "node:test";
import {
  parseControllerEnvironment,
  validateControllerEnvironment,
  validateControllerEnvironmentMetadata,
} from "../scripts/deploy-controller-preflight.mjs";
import { validateControllerRouteSmokeResponse } from "../scripts/controller-route-smoke.mjs";

const VALID_ENV = `
NODE_ENV=production
PINVOU_DESKTOP_CONTROLLER_ENABLED=1
PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=official
PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false
PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID=enrollment-key
PINVOU_REMOTE_ENROLLMENT_HMAC_KEY=enrollment-secret-at-least-32-bytes
PINVOU_REMOTE_PAIRING_HMAC_KEY_ID=pairing-key
PINVOU_REMOTE_PAIRING_HMAC_KEY=pairing-secret-at-least-32-bytes
PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=relay-signing-key
PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64=private-key-material
PINVOU_REMOTE_CONTROLLER_STATE_PATH=/var/lib/pinvou-remote-relay/state/controller-state.json
PINVOU_REMOTE_STATE_PATH=/opt/pinvou-remote-relay/data/relay-state.json
PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.10
PINVOU_REMOTE_TRUSTED_PROXY_IPS=192.0.2.10
PINVOU_TELEMETRY_ENROLLMENT_TOKEN=telemetry-enrollment-secret-24
PINVOU_TELEMETRY_DEVICE_PEPPER=telemetry-device-pepper-24
PINVOU_STATS_ADMIN_PASSWORD=operations-admin-password
PINVOU_TELEMETRY_DATA_DIR=/var/lib/pinvou-telemetry
`;

test("production controller environment supports explicit official and self-hosted enrollment with TOFU disabled", () => {
  assert.doesNotThrow(() => validateControllerEnvironment(parseControllerEnvironment(VALID_ENV)));
  assert.doesNotThrow(() => validateControllerEnvironment(parseControllerEnvironment(
    VALID_ENV.replace("PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=official", "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted"),
  )));
  for (const replacement of [
    ["NODE_ENV=production", "NODE_ENV=development"],
    ["PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=official", "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development"],
    ["PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false", "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=true"],
    ["PINVOU_REMOTE_STATE_PATH=/opt/pinvou-remote-relay/data/relay-state.json", "PINVOU_REMOTE_STATE_PATH="],
  ]) {
    assert.throws(
      () => validateControllerEnvironment(parseControllerEnvironment(VALID_ENV.replace(...replacement))),
      /controller_env_required_value_(?:missing|invalid)/u,
    );
  }
  assert.throws(
    () => parseControllerEnvironment(`${VALID_ENV}\nNODE_ENV=production\n`),
    /controller_env_duplicate_key/u,
  );
});

test("production controller environment rejects undeclared keys", () => {
  assert.throws(
    () => validateControllerEnvironment(parseControllerEnvironment(`${VALID_ENV}\nPINVOU_UNREVIEWED_ESCAPE_HATCH=enabled\n`)),
    /controller_env_unknown_key/u,
  );
});

test("production preflight can verify the exact feature-off rollback configuration", () => {
  const disabled = parseControllerEnvironment(
    VALID_ENV.replace("PINVOU_DESKTOP_CONTROLLER_ENABLED=1", "PINVOU_DESKTOP_CONTROLLER_ENABLED=0"),
  );
  assert.doesNotThrow(() => validateControllerEnvironment(disabled, { expectedFeatureFlag: "0" }));
  assert.throws(
    () => validateControllerEnvironment(disabled),
    /controller_env_required_value_invalid/u,
  );
});

test("production controller environment requires an enabled protected telemetry service", () => {
  for (const replacement of [
    ["PINVOU_TELEMETRY_ENROLLMENT_TOKEN=telemetry-enrollment-secret-24", "PINVOU_TELEMETRY_ENROLLMENT_TOKEN=short"],
    ["PINVOU_TELEMETRY_DEVICE_PEPPER=telemetry-device-pepper-24", "PINVOU_TELEMETRY_DEVICE_PEPPER=short"],
    ["PINVOU_STATS_ADMIN_PASSWORD=operations-admin-password", "PINVOU_STATS_ADMIN_PASSWORD=short"],
  ]) {
    assert.throws(
      () => validateControllerEnvironment(parseControllerEnvironment(VALID_ENV.replace(...replacement))),
      /controller_env_telemetry_credentials_invalid/u,
    );
  }
  assert.throws(
    () => validateControllerEnvironment(parseControllerEnvironment(
      VALID_ENV.replace("PINVOU_TELEMETRY_DATA_DIR=/var/lib/pinvou-telemetry", "PINVOU_TELEMETRY_DATA_DIR=relative/path"),
    )),
    /controller_env_telemetry_data_dir_invalid/u,
  );
});

test("production controller environment requires matching literal proxy addresses", () => {
  for (const replacement of [
    ["PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.10", "PINVOU_REMOTE_ALLOWED_PROXY_IPS="],
    ["PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.10", "PINVOU_REMOTE_ALLOWED_PROXY_IPS=proxy.internal"],
  ]) {
    assert.throws(
      () => validateControllerEnvironment(parseControllerEnvironment(VALID_ENV.replace(...replacement))),
      /controller_env_(?:required_value_missing|proxy_ips_invalid)/u,
    );
  }
  assert.throws(
    () => validateControllerEnvironment(parseControllerEnvironment(
      VALID_ENV.replace("PINVOU_REMOTE_TRUSTED_PROXY_IPS=192.0.2.10", "PINVOU_REMOTE_TRUSTED_PROXY_IPS=192.0.2.11"),
    )),
    /controller_env_proxy_ips_mismatch/u,
  );
  const multiple = VALID_ENV
    .replace("PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.10", "PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.10,2001:db8::10")
    .replace("PINVOU_REMOTE_TRUSTED_PROXY_IPS=192.0.2.10", "PINVOU_REMOTE_TRUSTED_PROXY_IPS=2001:db8::10,192.0.2.10");
  assert.doesNotThrow(() => validateControllerEnvironment(parseControllerEnvironment(multiple)));
});

test("production controller environment rejects links, loose modes, and non-root owners", () => {
  const valid = {
    isFile: true,
    isSymbolicLink: false,
    mode: 0o600,
    uid: 0,
    gid: 0,
    expectedPath: "/etc/pinvou-remote-relay-controller.env",
    resolvedPath: "/etc/pinvou-remote-relay-controller.env",
  };
  assert.doesNotThrow(() => validateControllerEnvironmentMetadata(valid));
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, isSymbolicLink: true }),
    /controller_env_file_type_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, mode: 0o640 }),
    /controller_env_permissions_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, uid: 1000 }),
    /controller_env_owner_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, resolvedPath: "/tmp/controller.env" }),
    /controller_env_path_invalid/u,
  );
});

test("controller route smoke requires a bounded v1 router response", () => {
  assert.doesNotThrow(() => validateControllerRouteSmokeResponse({
    v: 1,
    type: "error",
    code: "invalid_message",
  }));
  for (const response of [
    { v: 2, type: "error", code: "unsupported_protocol_version" },
    { v: 2, type: "error", code: "desktop_controller_unavailable" },
    { v: 1, type: "error", code: "enrollment_required" },
  ]) {
    assert.throws(
      () => validateControllerRouteSmokeResponse(response),
      /controller_route_smoke_response_invalid/u,
    );
  }
});
