import assert from "node:assert/strict";
import crypto from "node:crypto";
import * as fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  ENROLLMENT_NAMESPACE,
  EnrollmentRegistry,
  loadEnrollmentRegistryConfig,
} from "../enrollment-registry.js";
import {
  RELAY_CONTROLLER_STATE_SCHEMA,
  RelayStateStore,
} from "../relay-state-store.js";

const HMAC_KEY = "test-only-hmac-key-material-32-bytes-minimum";
const FIRST_CREDENTIAL = "endpoint-credential-one-at-least-32-bytes";
const SECOND_CREDENTIAL = "endpoint-credential-two-at-least-32-bytes";

function environment(overrides = {}) {
  return {
    NODE_ENV: "test",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "test-key-1",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: HMAC_KEY,
    ...overrides,
  };
}

async function temporaryState(t) {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), "pinvou-enrollment-test-"));
  t.after(() => fs.rm(directory, { recursive: true, force: true }));
  return path.join(directory, "controller-state.json");
}

async function openRegistry(t, { env, clock, storeOptions } = {}) {
  const statePath = await temporaryState(t);
  const store = await RelayStateStore.open({ statePath, ...storeOptions });
  t.after(() => store.close());
  return {
    statePath,
    store,
    registry: EnrollmentRegistry.open({
      stateStore: store,
      config: loadEnrollmentRegistryConfig(env || environment()),
      clock,
    }),
  };
}

async function rejectsCode(promise, code) {
  await assert.rejects(promise, (error) => error?.code === code);
}

test("official registry enrolls explicitly and authorizes both registration paths", async (t) => {
  const { statePath, registry } = await openRegistry(t, { clock: () => Date.UTC(2026, 8, 16, 1, 2, 3) });
  const enrolled = await registry.enroll({ endpointId: "endpoint-0001", credential: FIRST_CREDENTIAL });
  assert.deepEqual(enrolled, {
    endpoint_id: "endpoint-0001",
    status: "active",
    revision: "1",
    enrolled_at: "2026-09-16T01:02:03.000Z",
    rotated_at: null,
    revoked_at: null,
  });
  assert.equal((await registry.authorizeRegistration({
    endpointId: "endpoint-0001",
    credential: FIRST_CREDENTIAL,
    purpose: "web_v2_reregister",
  })).purpose, "web_v2_reregister");
  assert.equal((await registry.authorizeRegistration({
    endpointId: "endpoint-0001",
    credential: FIRST_CREDENTIAL,
    purpose: "desktop_pairing_register",
  })).purpose, "desktop_pairing_register");

  const persisted = await fs.readFile(statePath, "utf8");
  assert.doesNotMatch(persisted, /endpoint-credential-one/u);
  assert.doesNotMatch(persisted, new RegExp(FIRST_CREDENTIAL, "u"));
  assert.match(persisted, /hmac-sha256-v1/u);
});

test("production modes reject anonymous registration and development TOFU", async (t) => {
  for (const deployment of ["official", "self_hosted"]) {
    const { registry } = await openRegistry(t, {
      env: environment({ NODE_ENV: "production", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: deployment }),
    });
    await rejectsCode(registry.authorizeRegistration({
      endpointId: `missing-${deployment}`,
      credential: FIRST_CREDENTIAL,
      purpose: "desktop_pairing_register",
    }), "enrollment_required");
  }
  assert.throws(() => loadEnrollmentRegistryConfig(environment({
    NODE_ENV: "production",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "1",
  })), (error) => error?.code === "development_tofu_forbidden");
  assert.throws(() => loadEnrollmentRegistryConfig(environment({
    NODE_ENV: "production",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "1",
  })), (error) => error?.code === "development_tofu_forbidden");
});

test("explicit non-production mode can opt into temporary TOFU", async (t) => {
  const { registry, store } = await openRegistry(t, {
    env: environment({
      NODE_ENV: "development",
      PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
      PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "true",
    }),
  });
  const authorized = await registry.authorizeRegistration({
    endpointId: "development-endpoint",
    credential: FIRST_CREDENTIAL,
    purpose: "desktop_pairing_register",
  });
  assert.equal(authorized.status, "active");
  const stored = store.readNamespace(ENROLLMENT_NAMESPACE).value;
  assert.equal(stored.entries[0].audit.source, "development_tofu");
});

test("credential rotation invalidates old credential and revocation is terminal", async (t) => {
  let now = Date.UTC(2026, 8, 16, 1, 0, 0);
  const { registry } = await openRegistry(t, { clock: () => now });
  await registry.enroll({ endpointId: "endpoint-rotate", credential: FIRST_CREDENTIAL });
  now += 1000;
  const rotated = await registry.rotateCredential({
    endpointId: "endpoint-rotate",
    currentCredential: FIRST_CREDENTIAL,
    newCredential: SECOND_CREDENTIAL,
    expectedEndpointRevision: "1",
  });
  assert.equal(rotated.revision, "2");
  assert.equal(rotated.rotated_at, "2026-09-16T01:00:01.000Z");
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "endpoint-rotate",
    credential: FIRST_CREDENTIAL,
    purpose: "web_v2_reregister",
  }), "invalid_enrollment_credential");
  await registry.authorizeRegistration({
    endpointId: "endpoint-rotate",
    credential: SECOND_CREDENTIAL,
    purpose: "web_v2_reregister",
  });
  now += 1000;
  const revoked = await registry.revoke({ endpointId: "endpoint-rotate", expectedEndpointRevision: "2" });
  assert.equal(revoked.status, "revoked");
  assert.equal(revoked.revision, "3");
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "endpoint-rotate",
    credential: SECOND_CREDENTIAL,
    purpose: "desktop_pairing_register",
  }), "endpoint_revoked");
  await rejectsCode(registry.enroll({
    endpointId: "endpoint-rotate",
    credential: SECOND_CREDENTIAL,
  }), "endpoint_revoked");
});

test("a registry opened without the persisted digest key fails closed", async (t) => {
  const statePath = await temporaryState(t);
  let store = await RelayStateStore.open({ statePath });
  let registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(environment()) });
  await registry.enroll({ endpointId: "endpoint-keyring", credential: FIRST_CREDENTIAL });
  await store.close();

  store = await RelayStateStore.open({ statePath });
  t.after(() => store.close());
  registry = EnrollmentRegistry.open({
    stateStore: store,
    config: loadEnrollmentRegistryConfig(environment({
      PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "replacement-key",
      PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "replacement-hmac-key-material-at-least-32-bytes",
    })),
  });
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "endpoint-keyring",
    credential: FIRST_CREDENTIAL,
    purpose: "web_v2_reregister",
  }), "credential_key_unavailable");
});

test("endpoint and namespace revisions reject stale concurrent mutations", async (t) => {
  const { registry } = await openRegistry(t);
  await registry.enroll({ endpointId: "endpoint-cas", credential: FIRST_CREDENTIAL });
  const results = await Promise.allSettled([
    registry.rotateCredential({
      endpointId: "endpoint-cas",
      currentCredential: FIRST_CREDENTIAL,
      newCredential: SECOND_CREDENTIAL,
      expectedEndpointRevision: "1",
    }),
    registry.revoke({ endpointId: "endpoint-cas", expectedEndpointRevision: "1" }),
  ]);
  assert.equal(results.filter((result) => result.status === "fulfilled").length, 1);
  assert.equal(results.filter((result) => result.status === "rejected").length, 1);
  assert.ok(["enrollment_revision_conflict", "endpoint_revision_conflict"].includes(results.find(
    (result) => result.status === "rejected",
  ).reason.code));
});

test("restart reloads committed registry and valid backup restores a corrupted primary", async (t) => {
  const statePath = await temporaryState(t);
  let store = await RelayStateStore.open({ statePath });
  let registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(environment()) });
  await registry.enroll({ endpointId: "endpoint-restart", credential: FIRST_CREDENTIAL });
  await store.close();

  store = await RelayStateStore.open({ statePath });
  registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(environment()) });
  await registry.authorizeRegistration({
    endpointId: "endpoint-restart",
    credential: FIRST_CREDENTIAL,
    purpose: "web_v2_reregister",
  });
  await store.close();

  await fs.writeFile(statePath, "{corrupt", "utf8");
  store = await RelayStateStore.open({ statePath });
  t.after(() => store.close());
  registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(environment()) });
  await registry.authorizeRegistration({
    endpointId: "endpoint-restart",
    credential: FIRST_CREDENTIAL,
    purpose: "desktop_pairing_register",
  });
  assert.doesNotMatch(await fs.readFile(statePath, "utf8"), /corrupt/u);
});

test("legacy controller state migrates once without touching the existing v2 relay state", async (t) => {
  const statePath = await temporaryState(t);
  const legacy = {
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: 0,
    revision: 4,
    namespaces: [{
      name: "future_pairing_tombstones",
      revision: 2,
      value: { tombstones: [] },
    }],
  };
  await fs.writeFile(statePath, JSON.stringify(legacy), "utf8");
  const store = await RelayStateStore.open({ statePath });
  t.after(() => store.close());
  assert.equal(store.readNamespace("future_pairing_tombstones").namespace_revision, "2");
  const current = JSON.parse(await fs.readFile(statePath, "utf8"));
  assert.equal(current.version, 1);
  assert.equal(current.revision, "4");
  assert.deepEqual(current.namespaces.future_pairing_tombstones.value, { tombstones: [] });
});

test("strict state validation rejects unknown fields, oversized input and unsafe paths", async (t) => {
  const statePath = await temporaryState(t);
  await fs.writeFile(statePath, JSON.stringify({
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: 1,
    revision: "0",
    namespaces: {},
    unexpected: true,
  }), "utf8");
  await rejectsCode(RelayStateStore.open({ statePath }), "relay_state_unavailable");

  const oversizedPath = path.join(path.dirname(statePath), "oversized.json");
  await fs.writeFile(oversizedPath, "x".repeat(2048), "utf8");
  await rejectsCode(RelayStateStore.open({ statePath: oversizedPath, maxBytes: 1024 }), "relay_state_unavailable");
  await rejectsCode(RelayStateStore.open({ statePath: "relative.json" }), "relay_state_path_invalid");
});

test("live process lock prevents a second writer and is released on close", async (t) => {
  const statePath = await temporaryState(t);
  const first = await RelayStateStore.open({ statePath });
  await rejectsCode(RelayStateStore.open({ statePath }), "relay_state_locked");
  await first.close();
  const second = await RelayStateStore.open({ statePath });
  await second.close();
});

test("a persistence failure degrades the process and never exposes an unpersisted mutation", async (t) => {
  const statePath = await temporaryState(t);
  let temporaryOpens = 0;
  const injectedFs = {
    ...fs,
    async open(target, flags, mode) {
      if (flags === "wx" && String(target).endsWith(".tmp")) {
        temporaryOpens += 1;
        if (temporaryOpens === 3) {
          const error = new Error("injected write failure");
          error.code = "EIO";
          throw error;
        }
      }
      return fs.open(target, flags, mode);
    },
  };
  const store = await RelayStateStore.open({
    statePath,
    fs: injectedFs,
    randomBytes: (size) => crypto.createHash("sha256").update(String(size + temporaryOpens)).digest().subarray(0, size),
    sleep: async () => {},
  });
  t.after(() => store.close());
  await rejectsCode(store.compareAndSwap("test", "0", { value: 1 }), "relay_state_persist_failed");
  assert.throws(() => store.readNamespace("test"), (error) => error?.code === "relay_state_unavailable");
  assert.equal(JSON.parse(await fs.readFile(statePath, "utf8")).revision, "0");
});

test("a failure after staging backup cannot silently authorize after restart", async (t) => {
  const statePath = await temporaryState(t);
  let primaryReplaces = 0;
  const injectedFs = {
    ...fs,
    async rename(source, target) {
      if (target === statePath) {
        primaryReplaces += 1;
        if (primaryReplaces === 2) {
          const error = new Error("injected primary replace failure");
          error.code = "EIO";
          throw error;
        }
      }
      return fs.rename(source, target);
    },
  };
  const store = await RelayStateStore.open({ statePath, fs: injectedFs });
  await rejectsCode(store.compareAndSwap("test", "0", { grants_access: true }), "relay_state_persist_failed");
  await store.close();
  await rejectsCode(RelayStateStore.open({ statePath }), "relay_state_unavailable");
});

test("a damaged or mismatched backup prevents authorization", async (t) => {
  const statePath = await temporaryState(t);
  let store = await RelayStateStore.open({ statePath });
  await store.compareAndSwap("test", "0", { value: 1 });
  await store.close();

  await fs.writeFile(`${statePath}.bak`, "{corrupt", "utf8");
  await rejectsCode(RelayStateStore.open({ statePath }), "relay_state_unavailable");

  await fs.copyFile(statePath, `${statePath}.bak`);
  const backup = JSON.parse(await fs.readFile(`${statePath}.bak`, "utf8"));
  backup.revision = "2";
  await fs.writeFile(`${statePath}.bak`, JSON.stringify(backup), "utf8");
  await rejectsCode(RelayStateStore.open({ statePath }), "relay_state_unavailable");
});

test("registry fails closed on corrupted namespace, missing digest key and malformed configuration", async (t) => {
  const { store } = await openRegistry(t);
  await store.compareAndSwap(ENROLLMENT_NAMESPACE, "0", { unexpected: true });
  assert.throws(() => EnrollmentRegistry.open({
    stateStore: store,
    config: loadEnrollmentRegistryConfig(environment()),
  }), (error) => error?.code === "enrollment_registry_malformed");

  assert.throws(() => loadEnrollmentRegistryConfig(environment({ PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "short" })),
    (error) => error?.code === "enrollment_hmac_key_invalid");
  assert.throws(() => loadEnrollmentRegistryConfig(environment({ PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "" })),
    (error) => error?.code === "enrollment_deployment_invalid");
  const opaqueConfig = loadEnrollmentRegistryConfig(environment());
  assert.doesNotMatch(JSON.stringify(opaqueConfig), /test-only-hmac-key/u);
});

test("persisted registry contains only digest and minimal non-secret audit fields", async (t) => {
  const { registry, store } = await openRegistry(t);
  await registry.enroll({ endpointId: "endpoint-audit", credential: FIRST_CREDENTIAL });
  const value = store.readNamespace(ENROLLMENT_NAMESPACE).value;
  assert.deepEqual(Object.keys(value.entries[0]).sort(), [
    "audit", "credential", "endpoint_id", "enrolled_at", "revision", "revoked_at", "rotated_at", "status",
  ]);
  assert.deepEqual(Object.keys(value.entries[0].audit).sort(), ["last_action", "source"]);
  assert.equal(value.entries[0].credential.digest_hex.length, 64);
  assert.notEqual(value.entries[0].credential.digest_hex, FIRST_CREDENTIAL);
});
