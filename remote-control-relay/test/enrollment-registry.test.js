import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import crypto from "node:crypto";
import * as fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { once } from "node:events";
import { promisify } from "node:util";

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
const execFileAsync = promisify(execFile);
const TEST_RESOURCES = new WeakMap();

function resourcesFor(t) {
  let resources = TEST_RESOURCES.get(t);
  if (!resources) {
    resources = { directories: [], stores: [] };
    TEST_RESOURCES.set(t, resources);
    t.after(async () => {
      for (const store of resources.stores) {
        try { await store.close(); } catch {}
      }
      for (const directory of resources.directories) {
        await fs.rm(directory, { recursive: true, force: true });
      }
    });
  }
  return resources;
}

function environment(overrides = {}) {
  return {
    NODE_ENV: "test",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "test-key-1",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: HMAC_KEY,
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: path.join(os.tmpdir(), "pinvou-controller-config", "state.json"),
    PINVOU_REMOTE_STATE_PATH: path.join(os.tmpdir(), "pinvou-legacy-config.json"),
    ...overrides,
  };
}

async function temporaryState(t) {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), "pinvou-enrollment-test-"));
  resourcesFor(t).directories.push(directory);
  return path.join(directory, "controller", "state.json");
}

function legacyStatePath(statePath) {
  return path.join(path.dirname(path.dirname(statePath)), "legacy-web-state.json");
}

function testSecurityOptions() {
  if (process.platform !== "win32") return {};
  return {
    protectPath: async () => {},
    verifyPath: async () => {},
    verifyAncestors: async () => {},
  };
}

async function openStateStore(statePath, options = {}) {
  return RelayStateStore.open({
    statePath,
    legacyStatePath: legacyStatePath(statePath),
    processIdentity: async (pid) => `test-process:${pid}`,
    ...testSecurityOptions(),
    ...options,
  });
}

function configForState(statePath, overrides = {}) {
  return loadEnrollmentRegistryConfig(environment({
    ...overrides,
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath,
    PINVOU_REMOTE_STATE_PATH: legacyStatePath(statePath),
  }));
}

async function openRegistry(t, { env, clock, storeOptions } = {}) {
  const statePath = await temporaryState(t);
  const store = await openStateStore(statePath, storeOptions);
  resourcesFor(t).stores.push(store);
  return {
    statePath,
    store,
    registry: EnrollmentRegistry.open({
      stateStore: store,
      config: configForState(statePath, env),
      clock,
    }),
  };
}

async function rejectsCode(promise, code) {
  await assert.rejects(promise, (error) => error?.code === code);
}

function lockPath(statePath) {
  return `${statePath}.lock`;
}

function lockRecord({ pid = process.pid, identity = `test-process:${pid}`, owner = "AAAAAAAAAAAAAAAAAAAAAAAA" } = {}) {
  return {
    schema: "pinvou-relay-controller-state-lock",
    version: 1,
    pid,
    process_identity: identity,
    owner_id: owner,
  };
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

test("development TOFU exhausts bounded CAS retries without authorizing or persisting", async (t) => {
  const { registry, store, statePath } = await openRegistry(t, {
    env: environment({
      NODE_ENV: "development",
      PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
      PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "true",
    }),
  });
  const before = await fs.readFile(statePath);
  let attempts = 0;
  store.compareAndSwap = async () => {
    attempts += 1;
    const error = new Error("injected CAS conflict");
    error.code = "namespace_revision_conflict";
    throw error;
  };
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "development-conflict",
    credential: FIRST_CREDENTIAL,
    purpose: "desktop_pairing_register",
  }), "enrollment_revision_conflict");
  assert.equal(attempts, 4);
  assert.equal(registry.getStatus("development-conflict"), null);
  assert.deepEqual(await fs.readFile(statePath), before);
  assert.deepEqual(await fs.readFile(`${statePath}.bak`), before);
});

test("credential rotation invalidates old credential and revocation is terminal", async (t) => {
  let now = Date.UTC(2026, 8, 16, 1, 0, 0);
  const { registry, store, statePath } = await openRegistry(t, { clock: () => now });
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
  const terminalSnapshot = store.readNamespace(ENROLLMENT_NAMESPACE);
  const terminalPrimary = await fs.readFile(statePath);
  const terminalBackup = await fs.readFile(`${statePath}.bak`);
  for (const expectedEndpointRevision of ["2", "999"]) {
    assert.deepEqual(await registry.revoke({ endpointId: "endpoint-rotate", expectedEndpointRevision }), revoked);
    assert.deepEqual(store.readNamespace(ENROLLMENT_NAMESPACE), terminalSnapshot);
    assert.deepEqual(await fs.readFile(statePath), terminalPrimary);
    assert.deepEqual(await fs.readFile(`${statePath}.bak`), terminalBackup);
  }
  await rejectsCode(registry.revoke({
    endpointId: "endpoint-rotate", expectedEndpointRevision: "not-a-revision",
  }), "expected_endpoint_revision_invalid");
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
  let store = await openStateStore(statePath);
  let registry = EnrollmentRegistry.open({ stateStore: store, config: configForState(statePath) });
  await registry.enroll({ endpointId: "endpoint-keyring", credential: FIRST_CREDENTIAL });
  await store.close();

  store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  registry = EnrollmentRegistry.open({
    stateStore: store,
    config: configForState(statePath, {
      PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "replacement-key",
      PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "replacement-hmac-key-material-at-least-32-bytes",
    }),
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
  let store = await openStateStore(statePath);
  let registry = EnrollmentRegistry.open({ stateStore: store, config: configForState(statePath) });
  await registry.enroll({ endpointId: "endpoint-restart", credential: FIRST_CREDENTIAL });
  await store.close();

  store = await openStateStore(statePath);
  registry = EnrollmentRegistry.open({ stateStore: store, config: configForState(statePath) });
  await registry.authorizeRegistration({
    endpointId: "endpoint-restart",
    credential: FIRST_CREDENTIAL,
    purpose: "web_v2_reregister",
  });
  await store.close();

  await fs.writeFile(statePath, "{corrupt", "utf8");
  store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  registry = EnrollmentRegistry.open({ stateStore: store, config: configForState(statePath) });
  await registry.authorizeRegistration({
    endpointId: "endpoint-restart",
    credential: FIRST_CREDENTIAL,
    purpose: "desktop_pairing_register",
  });
  assert.doesNotMatch(await fs.readFile(statePath, "utf8"), /corrupt/u);
});

test("legacy controller state migrates once without touching the existing v2 relay state", async (t) => {
  const statePath = await temporaryState(t);
  const initialized = await openStateStore(statePath);
  await initialized.close();
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
  await fs.copyFile(statePath, `${statePath}.bak`);
  const store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  assert.equal(store.readNamespace("future_pairing_tombstones").namespace_revision, "2");
  const current = JSON.parse(await fs.readFile(statePath, "utf8"));
  assert.equal(current.version, 1);
  assert.equal(current.revision, "4");
  assert.deepEqual(current.namespaces.future_pairing_tombstones.value, { tombstones: [] });
  assert.deepEqual(await fs.readFile(`${statePath}.bak`), await fs.readFile(statePath));
});

test("legacy migration resumes with a normalized equivalent v1 backup", async (t) => {
  const statePath = await temporaryState(t);
  const initialized = await openStateStore(statePath);
  await initialized.compareAndSwap("migration_probe", "0", { value: 7 });
  await initialized.close();
  const current = JSON.parse(await fs.readFile(statePath, "utf8"));
  const legacy = {
    ...current,
    version: 0,
    revision: Number(current.revision),
    namespaces: Object.entries(current.namespaces).map(([name, record]) => ({
      name, revision: Number(record.revision), value: record.value,
    })),
  };
  await fs.writeFile(statePath, JSON.stringify(legacy), "utf8");
  const store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  assert.deepEqual(store.readNamespace("migration_probe"), {
    store_revision: "1", namespace_revision: "1", value: { value: 7 },
  });
  assert.deepEqual(JSON.parse(await fs.readFile(statePath, "utf8")), current);
  assert.deepEqual(await fs.readFile(`${statePath}.bak`), await fs.readFile(statePath));
});

test("legacy migration rejects inconsistent backup without rewriting either candidate", async (t) => {
  const primary = {
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: 0,
    revision: 5,
    namespaces: [{ name: "ns_a", revision: 2, value: { source: "primary" } }],
  };
  const matching = {
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: 1,
    revision: "5",
    namespaces: { ns_a: { revision: "2", value: { source: "primary" } } },
  };
  for (const [label, backup] of [
    ["newer v1 with another namespace", JSON.stringify({
      ...matching, revision: "9", namespaces: { ns_b: { revision: "1", value: { source: "backup" } } },
    })],
    ["another v1 revision", JSON.stringify({ ...matching, revision: "9" })],
    ["another v1 namespace", JSON.stringify({
      ...matching, namespaces: { ns_b: { revision: "2", value: { source: "primary" } } },
    })],
    ["same revision with another value", JSON.stringify({
      ...matching, namespaces: { ns_a: { revision: "2", value: { source: "backup" } } },
    })],
    ["another v0 revision", JSON.stringify({ ...primary, revision: 6 })],
    ["another v0 namespace", JSON.stringify({
      ...primary, namespaces: [{ ...primary.namespaces[0], name: "ns_b" }],
    })],
    ["another v0 value", JSON.stringify({
      ...primary, namespaces: [{ ...primary.namespaces[0], value: { source: "backup" } }],
    })],
    ["damaged backup", "{corrupt"],
    ["unsupported backup", JSON.stringify({ ...matching, version: 2 })],
    ["missing backup", null],
  ]) {
    await t.test(label, async (subtest) => {
      const statePath = await temporaryState(subtest);
      const initialized = await openStateStore(statePath);
      await initialized.close();
      const primaryBefore = JSON.stringify(primary);
      await fs.writeFile(statePath, primaryBefore, "utf8");
      if (backup === null) await fs.rm(`${statePath}.bak`);
      else await fs.writeFile(`${statePath}.bak`, backup, "utf8");
      await rejectsCode(openStateStore(statePath).then((store) => {
        resourcesFor(subtest).stores.push(store);
        return store;
      }), "relay_state_unavailable");
      assert.deepEqual(await fs.readFile(statePath), Buffer.from(primaryBefore, "utf8"));
      if (backup === null) await assert.rejects(fs.stat(`${statePath}.bak`), { code: "ENOENT" });
      else assert.deepEqual(await fs.readFile(`${statePath}.bak`), Buffer.from(backup, "utf8"));
    });
  }
});

test("strict state validation rejects unknown fields, oversized input and unsafe paths", async (t) => {
  const statePath = await temporaryState(t);
  let initialized = await openStateStore(statePath);
  await initialized.close();
  const malformed = JSON.stringify({
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: 1,
    revision: "0",
    namespaces: {},
    unexpected: true,
  });
  await fs.writeFile(statePath, malformed, "utf8");
  await fs.writeFile(`${statePath}.bak`, malformed, "utf8");
  await rejectsCode(openStateStore(statePath), "relay_state_unavailable");

  const oversizedPath = await temporaryState(t);
  initialized = await openStateStore(oversizedPath);
  await initialized.close();
  await fs.writeFile(oversizedPath, "x".repeat(2048), "utf8");
  await fs.writeFile(`${oversizedPath}.bak`, "x".repeat(2048), "utf8");
  await rejectsCode(openStateStore(oversizedPath, { maxBytes: 1024 }), "relay_state_unavailable");
  await rejectsCode(RelayStateStore.open({
    statePath: "relative.json",
    legacyStatePath: path.resolve("legacy.json"),
  }), "relay_state_path_invalid");
});

test("unsafe JSON keys and depth fail before a namespace write", async (t) => {
  const statePath = await temporaryState(t);
  const store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  const before = await fs.readFile(statePath);
  let boundary = null;
  for (let depth = 0; depth < 32; depth += 1) boundary = [boundary];
  for (const value of [
    JSON.parse('{"__proto__":{"polluted":true}}'),
    JSON.parse('{"constructor":{"prototype":{"polluted":true}}}'),
    [boundary],
  ]) {
    await rejectsCode(store.compareAndSwap("json_probe", "0", value), "namespace_value_malformed");
    assert.deepEqual(await fs.readFile(statePath), before);
    assert.deepEqual(await fs.readFile(`${statePath}.bak`), before);
  }
  assert.equal({}.polluted, undefined);
  await store.compareAndSwap("json_probe", "0", boundary);
  assert.equal(store.readNamespace("json_probe").namespace_revision, "1");
});

test("configuration and store reject lexical and hardlink aliases of legacy Web state", async (t) => {
  const statePath = await temporaryState(t);
  assert.throws(() => loadEnrollmentRegistryConfig(environment({
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath,
    PINVOU_REMOTE_STATE_PATH: statePath,
  })), (error) => error?.code === "controller_state_path_aliases_legacy");
  if (process.platform === "win32") {
    assert.throws(() => loadEnrollmentRegistryConfig(environment({
      PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath.toUpperCase(),
      PINVOU_REMOTE_STATE_PATH: statePath.toLowerCase(),
    })), (error) => error?.code === "controller_state_path_aliases_legacy");
  }
  for (const legacyPath of [
    `${statePath}.bak`,
    `${statePath}.lock`,
    path.join(path.dirname(statePath), ".pinvou-controller-state-directory"),
    path.join(path.dirname(statePath), "unrelated-legacy-name.json"),
    `${statePath}.123.0123456789abcdef.tmp`,
  ]) {
    assert.throws(() => loadEnrollmentRegistryConfig(environment({
      PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath,
      PINVOU_REMOTE_STATE_PATH: legacyPath,
    })), (error) => error?.code === "legacy_state_path_inside_controller_directory");
    await rejectsCode(RelayStateStore.open({
      statePath,
      legacyStatePath: legacyPath,
    }), "legacy_state_path_inside_controller_directory");
  }

  for (const reservedName of [
    ".pinvou-controller-state-directory", "state.lock", "state.bak", "state.tmp", "state.stale.old",
  ]) {
    assert.throws(() => loadEnrollmentRegistryConfig(environment({
      PINVOU_REMOTE_CONTROLLER_STATE_PATH: path.join(path.dirname(statePath), reservedName),
      PINVOU_REMOTE_STATE_PATH: legacyStatePath(statePath),
    })), (error) => error?.code === "controller_state_filename_reserved");
    await rejectsCode(RelayStateStore.open({
      statePath: path.join(path.dirname(statePath), reservedName),
      legacyStatePath: legacyStatePath(statePath),
    }), "relay_state_filename_reserved");
  }

  let store = await openStateStore(statePath);
  await store.close();
  await fs.link(statePath, legacyStatePath(statePath));
  await rejectsCode(openStateStore(statePath), "relay_state_aliases_legacy");
  await fs.rm(legacyStatePath(statePath));
  await fs.link(path.join(path.dirname(statePath), ".pinvou-controller-state-directory"), legacyStatePath(statePath));
  await rejectsCode(openStateStore(statePath), "relay_state_aliases_legacy");
});

test("registry rejects a store opened for different configured paths", async (t) => {
  const statePath = await temporaryState(t);
  const otherStatePath = path.join(path.dirname(path.dirname(statePath)), "other-controller", "state.json");
  const store = await openStateStore(statePath);
  resourcesFor(t).stores.push(store);
  assert.throws(() => EnrollmentRegistry.open({
    stateStore: store,
    config: configForState(otherStatePath),
  }), (error) => error?.code === "enrollment_state_store_path_mismatch");
});

test("junction or symlink ancestors are rejected", async (t) => {
  const statePath = await temporaryState(t);
  const root = path.dirname(path.dirname(statePath));
  const actualParent = path.join(root, "actual-parent");
  const aliasParent = path.join(root, "alias-parent");
  await fs.mkdir(actualParent);
  try {
    await fs.symlink(actualParent, aliasParent, process.platform === "win32" ? "junction" : "dir");
  } catch (error) {
    if (["EPERM", "EACCES", "ENOTSUP"].includes(error?.code)) {
      t.skip(`symlink creation unavailable: ${error.code}`);
      return;
    }
    throw error;
  }
  await rejectsCode(openStateStore(path.join(aliasParent, "controller", "state.json")),
    "relay_state_path_component_unsafe");
});

test("final state symlinks are rejected without restoring the backup", async (t) => {
  const statePath = await temporaryState(t);
  const store = await openStateStore(statePath);
  await store.close();
  await fs.rm(statePath);
  try {
    await fs.symlink(`${statePath}.bak`, statePath, "file");
  } catch (error) {
    if (["EPERM", "EACCES", "ENOTSUP"].includes(error?.code)) {
      t.skip(`final state symlink creation unavailable: ${error.code}`);
      return;
    }
    throw error;
  }
  await rejectsCode(openStateStore(statePath), "relay_state_file_unsafe");
  assert.equal((await fs.lstat(statePath)).isSymbolicLink(), true);
});

test("unsafe state file types fail closed before any backup recovery", async (t) => {
  for (const suffix of ["", ".bak"]) {
    for (const kind of ["symlink", "non-regular"]) {
      await t.test(`${suffix || "primary"}: ${kind}`, async (t) => {
        const statePath = await temporaryState(t);
        const store = await openStateStore(statePath);
        await store.close();
        const primaryBefore = await fs.readFile(statePath);
        const backupBefore = await fs.readFile(`${statePath}.bak`);
        let lockPublished = false;
        let classified = false;
        let stateReplacements = 0;
        const injectedFs = {
          ...fs,
          async link(source, target) {
            await fs.link(source, target);
            if (target === lockPath(statePath)) lockPublished = true;
          },
          async lstat(target, ...args) {
            const metadata = await fs.lstat(target, ...args);
            if (lockPublished && target === `${statePath}${suffix}`) {
              classified = true;
              return {
                ...metadata,
                isFile: () => kind === "symlink",
                isSymbolicLink: () => kind === "symlink",
              };
            }
            return metadata;
          },
          async rename(source, target) {
            if (target === statePath || target === `${statePath}.bak`) stateReplacements += 1;
            return fs.rename(source, target);
          },
        };
        await rejectsCode(openStateStore(statePath, { fs: injectedFs }), "relay_state_file_unsafe");
        assert.equal(classified, true);
        assert.equal(stateReplacements, 0);
        assert.deepEqual(await fs.readFile(statePath), primaryBefore);
        assert.deepEqual(await fs.readFile(`${statePath}.bak`), backupBefore);
      });
    }
  }
});

test("open-handle identity checks detect final-file exchange", async (t) => {
  const statePath = await temporaryState(t);
  let store = await openStateStore(statePath);
  await store.close();
  let exchanged = false;
  const injectedFs = {
    ...fs,
    async open(target, flags, mode) {
      const handle = await fs.open(target, flags, mode);
      if (!exchanged && target === statePath && typeof flags === "number") {
        exchanged = true;
        await fs.rename(statePath, `${statePath}.exchanged`);
        await fs.copyFile(`${statePath}.bak`, statePath);
      }
      return handle;
    },
  };
  await assert.rejects(openStateStore(statePath, { fs: injectedFs }),
    (error) => error?.code === "relay_state_file_unsafe");
});

test("ancestor snapshots reject a directory identity exchange before lock publication", async (t) => {
  const statePath = await temporaryState(t);
  const dedicatedDirectory = path.dirname(statePath);
  let dedicatedRealpaths = 0;
  const injectedFs = {
    ...fs,
    async realpath(target, ...args) {
      const canonical = await fs.realpath(target, ...args);
      if (target === dedicatedDirectory) {
        dedicatedRealpaths += 1;
        if (dedicatedRealpaths > 1) return `${canonical}-exchanged`;
      }
      return canonical;
    },
  };
  await rejectsCode(openStateStore(statePath, { fs: injectedFs }), "relay_state_path_changed");
  assert.equal(await fs.stat(dedicatedDirectory).then(() => true), true);
  assert.equal((await fs.readdir(dedicatedDirectory)).some((name) => name.endsWith(".lock")), false);
});

test("bounded handle reads reject a file that grows after fstat", async (t) => {
  const statePath = await temporaryState(t);
  let store = await openStateStore(statePath, { maxBytes: 1024 });
  await store.close();
  let enlarged = false;
  const injectedFs = {
    ...fs,
    async open(target, flags, mode) {
      const handle = await fs.open(target, flags, mode);
      if (!enlarged && target === statePath && typeof flags === "number") {
        return {
          stat: (...args) => handle.stat(...args),
          read: async (...args) => {
            if (!enlarged) {
              enlarged = true;
              await fs.appendFile(statePath, "x".repeat(2048));
            }
            return handle.read(...args);
          },
          close: (...args) => handle.close(...args),
        };
      }
      return handle;
    },
  };
  await assert.rejects(openStateStore(statePath, { fs: injectedFs, maxBytes: 1024 }),
    (error) => error?.code === "relay_state_file_unsafe");
});

test("POSIX setup changes only its dedicated child directory", {
  skip: process.platform === "win32",
}, async (t) => {
  const statePath = await temporaryState(t);
  const callerParent = path.dirname(path.dirname(statePath));
  await fs.chmod(callerParent, 0o755);
  const before = (await fs.stat(callerParent)).mode & 0o777;
  const store = await openStateStore(statePath);
  await store.close();
  assert.equal((await fs.stat(callerParent)).mode & 0o777, before);
  assert.equal((await fs.stat(path.dirname(statePath))).mode & 0o777, 0o700);
});

test("Windows state directory and every durable/lock file have protected DACLs", {
  skip: process.platform !== "win32",
}, async (t) => {
  const statePath = await temporaryState(t);
  const store = await RelayStateStore.open({
    statePath,
    legacyStatePath: legacyStatePath(statePath),
    processIdentity: async (pid) => `acl-test-process:${pid}`,
  });
  const inspectScript = `
    $ErrorActionPreference = 'Stop'
    $result = @()
    $index = 0
    foreach ($target in ($env:PINVOU_ACL_PATHS -split '\\|')) {
      $acl = Get-Acl -LiteralPath $target
      $sids = @($acl.Access | ForEach-Object { $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value } | Sort-Object -Unique)
      $ownerAccount = New-Object System.Security.Principal.NTAccount -ArgumentList $acl.Owner
      $ownerSid = $ownerAccount.Translate([System.Security.Principal.SecurityIdentifier]).Value
      $validRules = $true
      foreach ($rule in @($acl.Access)) {
        if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { $validRules = $false }
        if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::FullControl) -ne [System.Security.AccessControl.FileSystemRights]::FullControl) { $validRules = $false }
        $expectedInheritance = if ($index -eq 0) { [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit' } else { [System.Security.AccessControl.InheritanceFlags]::None }
        if ($rule.InheritanceFlags -ne $expectedInheritance) { $validRules = $false }
      }
      $result += [pscustomobject]@{ protected = $acl.AreAccessRulesProtected; owner = $ownerSid; sids = $sids; count = @($acl.Access).Count; valid_rules = $validRules }
      $index += 1
    }
    [Console]::Out.Write(($result | ConvertTo-Json -Compress))
  `;
  const paths = [
    path.dirname(statePath),
    path.join(path.dirname(statePath), ".pinvou-controller-state-directory"),
    statePath,
    `${statePath}.bak`,
    lockPath(statePath),
  ];
  const { stdout } = await execFileAsync("powershell.exe", [
    "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
    "-EncodedCommand", Buffer.from(inspectScript, "utf16le").toString("base64"),
  ], {
    windowsHide: true,
    env: { ...process.env, PINVOU_ACL_PATHS: paths.join("|") },
    timeout: 15_000,
    maxBuffer: 64 * 1024,
  });
  const results = JSON.parse(stdout);
  assert.equal(results.length, paths.length);
  for (const result of results) {
    assert.equal(result.protected, true);
    assert.equal(result.count, result.sids.length);
    assert.equal(result.valid_rules, true);
    assert.ok(result.sids.includes(result.owner));
    assert.ok(result.sids.includes("S-1-5-18"));
    assert.ok(result.sids.includes("S-1-5-32-544"));
  }
  await store.close();
});

test("Windows refuses startup when the dedicated ACL cannot be set or verified", {
  skip: process.platform !== "win32",
}, async (t) => {
  for (const phase of ["set", "verify"]) {
    await t.test(phase, async () => {
      const statePath = await temporaryState(t);
      if (phase === "verify") {
        const initialized = await openStateStore(statePath);
        await initialized.close();
      }
      await rejectsCode(RelayStateStore.open({
        statePath,
        legacyStatePath: legacyStatePath(statePath),
        processIdentity: async (pid) => `acl-failure-test:${pid}`,
        protectPath: phase === "set" ? async () => { throw new Error("injected ACL set failure"); } : async () => {},
        verifyPath: phase === "verify" ? async () => { throw new Error("injected ACL verify failure"); } : async () => {},
        verifyAncestors: async () => {},
      }), "relay_state_acl_unverified");
    });
  }
});

test("Windows never repairs an existing caller-created directory ACL", {
  skip: process.platform !== "win32",
}, async (t) => {
  const statePath = await temporaryState(t);
  await fs.mkdir(path.dirname(statePath));
  let protectionCalls = 0;
  await rejectsCode(RelayStateStore.open({
    statePath,
    legacyStatePath: legacyStatePath(statePath),
    processIdentity: async (pid) => `acl-existing-test:${pid}`,
    protectPath: async () => { protectionCalls += 1; },
    verifyPath: async () => { throw new Error("existing ACL is unsafe"); },
    verifyAncestors: async () => {},
  }), "relay_state_acl_unverified");
  assert.equal(protectionCalls, 0);
});

test("live process lock prevents a second writer and is released on close", async (t) => {
  const statePath = await temporaryState(t);
  const first = await openStateStore(statePath);
  await rejectsCode(openStateStore(statePath), "relay_state_locked");
  await first.close();
  const second = await openStateStore(statePath);
  await second.close();
});

test("close retries transient Windows-style lock unlink and directory sync failures", async (t) => {
  const statePath = await temporaryState(t);
  const directory = path.dirname(statePath);
  let armed = false;
  let removeFailures = 0;
  let syncFailures = 0;
  const injectedFs = {
    ...fs,
    async rm(target, options) {
      if (armed && target === lockPath(statePath) && removeFailures < 2) {
        removeFailures += 1;
        const error = new Error("transient lock unlink");
        error.code = removeFailures === 1 ? "EACCES" : "EPERM";
        throw error;
      }
      return fs.rm(target, options);
    },
    async open(target, flags, mode) {
      if (armed && target === directory && flags === "r" && syncFailures < 2) {
        syncFailures += 1;
        const error = new Error("transient directory sync");
        error.code = "EBUSY";
        throw error;
      }
      return fs.open(target, flags, mode);
    },
  };
  const store = await openStateStore(statePath, { fs: injectedFs, sleep: async () => {} });
  armed = true;
  await store.close();
  assert.equal(removeFailures, 2);
  assert.equal(syncFailures, 2);
  const reopened = await openStateStore(statePath);
  await reopened.close();
});

test("failed close stays locked and the same object can retry release", async (t) => {
  for (const phase of ["unlink", "sync"]) {
    await t.test(phase, async () => {
      const statePath = await temporaryState(t);
      const directory = path.dirname(statePath);
      let armed = false;
      const injectedFs = {
        ...fs,
        async rm(target, options) {
          if (armed && phase === "unlink" && target === lockPath(statePath)) {
            const error = new Error("persistent lock unlink failure");
            error.code = "EBUSY";
            throw error;
          }
          return fs.rm(target, options);
        },
        async open(target, flags, mode) {
          if (armed && phase === "sync" && target === directory && flags === "r") {
            const error = new Error("persistent directory sync failure");
            error.code = "EBUSY";
            throw error;
          }
          return fs.open(target, flags, mode);
        },
      };
      const store = await openStateStore(statePath, { fs: injectedFs, sleep: async () => {} });
      armed = true;
      await assert.rejects(store.close(), (error) => error?.code === "EBUSY");
      assert.throws(() => store.readNamespace("test"), (error) => error?.code === "relay_state_unavailable");
      await rejectsCode(openStateStore(statePath), "relay_state_locked");
      armed = false;
      await store.close();
      const reopened = await openStateStore(statePath);
      await reopened.close();
    });
  }
});

test("a failed open retains its primary error and a later open recovers the verified owner lock", async (t) => {
  const statePath = await temporaryState(t);
  const initialized = await openStateStore(statePath);
  await initialized.close();
  let loadFault = true;
  let releaseFault = true;
  let releaseAttempts = 0;
  const injectedFs = {
    ...fs,
    async open(target, flags, mode) {
      if (loadFault && target === statePath && typeof flags === "number") {
        loadFault = false;
        const error = new Error("injected state load failure");
        error.code = "EIO";
        throw error;
      }
      return fs.open(target, flags, mode);
    },
    async rm(target, options) {
      if (releaseFault && target === lockPath(statePath)) {
        releaseAttempts += 1;
        const error = new Error("injected abandoned lock release failure");
        error.code = "EBUSY";
        throw error;
      }
      return fs.rm(target, options);
    },
  };
  let openingError;
  try {
    await openStateStore(statePath, { fs: injectedFs, sleep: async () => {} });
  } catch (error) {
    openingError = error;
  }
  assert.equal(openingError?.code, "relay_state_unavailable");
  assert.equal(openingError?.cause?.code, "EIO");
  assert.equal(openingError?.releaseError?.code, "EBUSY");
  assert.equal(releaseAttempts, 5);

  releaseFault = false;
  const originalLock = await fs.readFile(lockPath(statePath), "utf8");
  const replacementLock = `${JSON.stringify(lockRecord({ owner: "BBBBBBBBBBBBBBBBBBBBBBBB" }))}\n`;
  await fs.writeFile(lockPath(statePath), replacementLock, "utf8");
  await rejectsCode(openStateStore(statePath), "relay_state_locked");
  assert.equal(await fs.readFile(lockPath(statePath), "utf8"), replacementLock);

  await fs.writeFile(lockPath(statePath), originalLock, "utf8");
  const recovered = await openStateStore(statePath);
  await recovered.close();
});

test("post-publication temp cleanup failure preserves the primary and recoverable owner", async (t) => {
  const statePath = await temporaryState(t);
  const initialized = await openStateStore(statePath);
  await initialized.close();
  let publishedTemporaryPath;
  let tempRemoveFault = true;
  let lockRemoveFault = true;
  const injectedFs = {
    ...fs,
    async link(source, target) {
      const result = await fs.link(source, target);
      if (target === lockPath(statePath)) publishedTemporaryPath = source;
      return result;
    },
    async rm(target, options) {
      if (tempRemoveFault && target === publishedTemporaryPath) {
        const error = new Error("injected published temp unlink failure");
        error.code = "EACCES";
        throw error;
      }
      if (lockRemoveFault && target === lockPath(statePath)) {
        const error = new Error("injected published lock unlink failure");
        error.code = "EBUSY";
        throw error;
      }
      return fs.rm(target, options);
    },
  };
  let openingError;
  try {
    await openStateStore(statePath, { fs: injectedFs, sleep: async () => {} });
  } catch (error) {
    openingError = error;
  }
  assert.equal(openingError?.code, "relay_state_unavailable");
  assert.equal(openingError?.cause?.code, "EACCES");
  assert.equal(openingError?.releaseError?.code, "EBUSY");
  assert.deepEqual(openingError?.releaseError?.cleanupErrors?.map((error) => error.code), ["EACCES"]);
  assert.ok(publishedTemporaryPath);

  tempRemoveFault = false;
  lockRemoveFault = false;
  await fs.rm(lockPath(statePath));
  const replacementLock = `${JSON.stringify(lockRecord({ owner: "CCCCCCCCCCCCCCCCCCCCCCCC" }))}\n`;
  await fs.writeFile(lockPath(statePath), replacementLock, "utf8");
  await rejectsCode(openStateStore(statePath), "relay_state_locked");
  assert.equal(await fs.readFile(lockPath(statePath), "utf8"), replacementLock);

  await fs.rm(lockPath(statePath));
  await fs.link(publishedTemporaryPath, lockPath(statePath));
  const recovered = await openStateStore(statePath);
  await recovered.close();
  await assert.rejects(fs.access(publishedTemporaryPath), (error) => error?.code === "ENOENT");
});

test("post-publication directory sync and lock removal failures recover in process", async (t) => {
  const statePath = await temporaryState(t);
  const directory = path.dirname(statePath);
  const initialized = await openStateStore(statePath);
  await initialized.close();
  let published = false;
  let directorySyncFault = true;
  let lockRemoveFault = true;
  const injectedFs = {
    ...fs,
    async link(source, target) {
      const result = await fs.link(source, target);
      if (target === lockPath(statePath)) published = true;
      return result;
    },
    async open(target, flags, mode) {
      if (published && directorySyncFault && target === directory && flags === "r") {
        const error = new Error("injected post-publication directory sync failure");
        error.code = "EACCES";
        throw error;
      }
      return fs.open(target, flags, mode);
    },
    async rm(target, options) {
      if (published && lockRemoveFault && target === lockPath(statePath)) {
        const error = new Error("injected lock cleanup failure");
        error.code = "EBUSY";
        throw error;
      }
      return fs.rm(target, options);
    },
  };
  let openingError;
  try {
    await openStateStore(statePath, { fs: injectedFs, sleep: async () => {} });
  } catch (error) {
    openingError = error;
  }
  assert.equal(openingError?.code, "relay_state_unavailable");
  assert.equal(openingError?.cause?.message, "injected post-publication directory sync failure");
  assert.equal(openingError?.releaseError?.message, "injected lock cleanup failure");

  directorySyncFault = false;
  lockRemoveFault = false;
  const recovered = await openStateStore(statePath);
  await recovered.close();
});

test("late legacy hardlinks to retired state in each persistence window fail closed", async (t) => {
  for (const phase of ["backup", "primary"]) {
    await t.test(phase, async () => {
      const statePath = await temporaryState(t);
      const legacyPath = legacyStatePath(statePath);
      const targetPath = phase === "backup" ? `${statePath}.bak` : statePath;
      let armed = false;
      let injected = false;
      const injectedFs = {
        ...fs,
        async rename(source, target) {
          if (armed && !injected && target === targetPath) {
            await fs.link(target, legacyPath);
            injected = true;
          }
          return fs.rename(source, target);
        },
      };
      const store = await openStateStore(statePath, { fs: injectedFs });
      const retiredContents = await fs.readFile(targetPath, "utf8");
      armed = true;
      await assert.rejects(store.compareAndSwap("test", "0", { value: phase }),
        (error) => error?.code === "relay_state_persist_failed"
          && error?.cause?.code === "relay_state_aliases_legacy");
      assert.equal(injected, true);
      assert.equal(await fs.readFile(legacyPath, "utf8"), retiredContents);
      assert.throws(() => store.readNamespace("test"),
        (error) => error?.code === "relay_state_unavailable");
      await store.close();
    });
  }
});

test("late legacy hardlink during backup restore preserves legacy bytes and aborts open", async (t) => {
  const statePath = await temporaryState(t);
  const legacyPath = legacyStatePath(statePath);
  const initialized = await openStateStore(statePath);
  await initialized.close();
  const corrupted = "{corrupt-primary";
  await fs.writeFile(statePath, corrupted, "utf8");
  let injected = false;
  const injectedFs = {
    ...fs,
    async rename(source, target) {
      if (!injected && target === statePath) {
        await fs.link(target, legacyPath);
        injected = true;
      }
      return fs.rename(source, target);
    },
  };
  await rejectsCode(openStateStore(statePath, { fs: injectedFs }), "relay_state_aliases_legacy");
  assert.equal(injected, true);
  assert.equal(await fs.readFile(legacyPath, "utf8"), corrupted);
});

test("a CAS uses one verified Windows ACL application per new state file", {
  skip: process.platform !== "win32",
}, async (t) => {
  const statePath = await temporaryState(t);
  let protectionCalls = 0;
  let verificationCalls = 0;
  const store = await RelayStateStore.open({
    statePath,
    legacyStatePath: legacyStatePath(statePath),
    processIdentity: async (pid) => `acl-count-test:${pid}`,
    protectPath: async () => { protectionCalls += 1; },
    verifyPath: async () => { verificationCalls += 1; },
    verifyAncestors: async () => {},
  });
  protectionCalls = 0;
  verificationCalls = 0;
  await store.compareAndSwap("test", "0", { value: 1 });
  assert.equal(protectionCalls, 2);
  assert.equal(verificationCalls, 0);
  await store.close();
});

test("empty and truncated crash locks are recovered after a bounded retry", async (t) => {
  for (const contents of ["", "{\"schema\":"]) {
    const statePath = await temporaryState(t);
    let store = await openStateStore(statePath);
    await store.close();
    await fs.writeFile(lockPath(statePath), contents, "utf8");
    store = await openStateStore(statePath, { sleep: async () => {} });
    assert.equal(store.readNamespace("test").namespace_revision, "0");
    await store.close();
  }
});

test("a crashed child process leaves a lock that another process can recover", async (t) => {
  const statePath = await temporaryState(t);
  const moduleUrl = new URL("../relay-state-store.js", import.meta.url).href;
  const childSource = `
    const { RelayStateStore } = await import(${JSON.stringify(moduleUrl)});
    await RelayStateStore.open({
      statePath: process.argv[1],
      legacyStatePath: process.argv[2],
      processIdentity: async (pid) => 'child-process:' + pid,
      protectPath: async () => {},
      verifyPath: async () => {},
      verifyAncestors: async () => {},
    });
    process.stdout.write('ready\\n');
    setTimeout(() => process.exit(0), 25);
  `;
  const child = spawn(process.execPath, [
    "--input-type=module", "-e", childSource, statePath, legacyStatePath(statePath),
  ], { stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
  const [ready] = await once(child.stdout, "data");
  assert.match(String(ready), /ready/u);
  const [exitCode] = await once(child, "exit");
  assert.equal(exitCode, 0);
  const store = await openStateStore(statePath, {
    processIdentity: async (pid) => {
      try {
        process.kill(pid, 0);
        return pid === process.pid ? `parent-process:${pid}` : `child-process:${pid}`;
      } catch {
        return null;
      }
    },
    sleep: async () => {},
  });
  await store.close();
});

test("PID reuse cannot make an old lock look live", async (t) => {
  const statePath = await temporaryState(t);
  let store = await openStateStore(statePath);
  await store.close();
  await fs.writeFile(
    lockPath(statePath),
    `${JSON.stringify(lockRecord({ identity: "old-boot:old-start" }))}\n`,
    { encoding: "utf8", mode: 0o600 },
  );
  store = await openStateStore(statePath, {
    processIdentity: async (pid) => pid === process.pid ? "new-boot:new-start" : null,
    sleep: async () => {},
  });
  await store.close();
});

test("lock publication cleans its handle and temporary file after create/write/sync failures", async (t) => {
  for (const phase of ["create", "write", "sync"]) {
    await t.test(phase, async () => {
      const statePath = await temporaryState(t);
      let injected = false;
      const injectedFs = {
        ...fs,
        async open(target, flags, mode) {
          const isLockTemporary = String(target).includes(`${path.basename(statePath)}.lock.`)
            && String(target).endsWith(".tmp");
          if (!injected && isLockTemporary && phase === "create") {
            injected = true;
            const error = new Error("injected lock create failure");
            error.code = "EIO";
            throw error;
          }
          const handle = await fs.open(target, flags, mode);
          if (!injected && isLockTemporary) {
            injected = true;
            return {
              stat: (...args) => handle.stat(...args),
              writeFile: phase === "write" ? async () => {
                const error = new Error("injected lock write failure");
                error.code = "EIO";
                throw error;
              } : (...args) => handle.writeFile(...args),
              sync: phase === "sync" ? async () => {
                const error = new Error("injected lock sync failure");
                error.code = "EIO";
                throw error;
              } : (...args) => handle.sync(...args),
              close: (...args) => handle.close(...args),
            };
          }
          return handle;
        },
      };
      await assert.rejects(openStateStore(statePath, { fs: injectedFs }),
        (error) => error?.cause?.code === "EIO");
      const names = await fs.readdir(path.dirname(statePath));
      assert.equal(names.some((name) => name.includes(".lock.")), false);
      assert.equal(names.includes(path.basename(lockPath(statePath))), false);
      const store = await openStateStore(statePath);
      await store.close();
    });
  }
});

test("a persistence failure degrades the process and never exposes an unpersisted mutation", async (t) => {
  const statePath = await temporaryState(t);
  let temporaryOpens = 0;
  const injectedFs = {
    ...fs,
    async open(target, flags, mode) {
      if (flags === "wx" && String(target).endsWith(".tmp")) {
        temporaryOpens += 1;
        if (temporaryOpens === 4) {
          const error = new Error("injected write failure");
          error.code = "EIO";
          throw error;
        }
      }
      return fs.open(target, flags, mode);
    },
  };
  const store = await openStateStore(statePath, {
    fs: injectedFs,
    randomBytes: (size) => crypto.createHash("sha256").update(String(size + temporaryOpens)).digest().subarray(0, size),
    sleep: async () => {},
  });
  resourcesFor(t).stores.push(store);
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
  const store = await openStateStore(statePath, { fs: injectedFs });
  await rejectsCode(store.compareAndSwap("test", "0", { grants_access: true }), "relay_state_persist_failed");
  await store.close();
  const committedPrimary = await fs.readFile(statePath);
  assert.equal(JSON.parse(await fs.readFile(`${statePath}.bak`, "utf8")).namespaces.test.value.grants_access, true);
  await rejectsCode(openStateStore(statePath), "relay_state_unavailable");
  assert.deepEqual(await fs.readFile(statePath), committedPrimary);

  // The injected failure proves that only the primary was committed. Emulate
  // the documented offline, evidence-backed recovery; never promote the staged backup.
  await fs.copyFile(statePath, `${statePath}.bak`);
  const recovered = await openStateStore(statePath);
  resourcesFor(t).stores.push(recovered);
  assert.deepEqual(recovered.readNamespace("test"), {
    store_revision: "0", namespace_revision: "0", value: null,
  });
  assert.deepEqual(await fs.readFile(statePath), committedPrimary);
});

test("a damaged or mismatched backup prevents authorization", async (t) => {
  const statePath = await temporaryState(t);
  let store = await openStateStore(statePath);
  await store.compareAndSwap("test", "0", { value: 1 });
  await store.close();

  await fs.writeFile(`${statePath}.bak`, "{corrupt", "utf8");
  await rejectsCode(openStateStore(statePath), "relay_state_unavailable");

  await fs.copyFile(statePath, `${statePath}.bak`);
  const backup = JSON.parse(await fs.readFile(`${statePath}.bak`, "utf8"));
  backup.revision = "2";
  await fs.writeFile(`${statePath}.bak`, JSON.stringify(backup), "utf8");
  await rejectsCode(openStateStore(statePath), "relay_state_unavailable");
});

test("registry fails closed on corrupted namespace, missing digest key and malformed configuration", async (t) => {
  const { store } = await openRegistry(t);
  await store.compareAndSwap(ENROLLMENT_NAMESPACE, "0", { unexpected: true });
  assert.throws(() => EnrollmentRegistry.open({
    stateStore: store,
    config: configForState(store.getPathPolicy().controller_state_path),
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
