import assert from "node:assert/strict";
import { resolve } from "node:path";
import { test } from "node:test";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";

const ACCESS = "access-credential-at-least-32-bytes-0001";
const DESKTOP = "desktop-credential-at-least-32-bytes-0001";
const ENDPOINT = "ep_abcdefghijklmnopqrstuvwx";

function fixture(selfRegistration = true, overrides = {}) {
  const statePath = resolve("public-enrollment-fixture/state/controller-state.json");
  const legacyPath = resolve("public-enrollment-fixture/legacy.json");
  const config = loadEnrollmentRegistryConfig({
    NODE_ENV: "production",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_ENROLLMENT_SELF_REGISTRATION: selfRegistration ? "true" : "false",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "false",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "public-enrollment-test-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "public-enrollment-test-secret-at-least-32-bytes",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath,
    PINVOU_REMOTE_STATE_PATH: legacyPath,
    ...overrides,
  });
  let value = null, revision = "0", writes = 0;
  const store = {
    getPathPolicy: () => ({ controller_state_path: statePath, legacy_web_state_path: legacyPath }),
    readNamespaceRevision: () => ({ store_revision: revision, namespace_revision: revision }),
    readNamespace: () => ({ store_revision: revision, namespace_revision: revision, value: structuredClone(value) }),
    async compareAndSwap(_name, expected, next) {
      await Promise.resolve();
      if (expected !== revision) throw Object.assign(new Error("conflict"), { code: "namespace_revision_conflict" });
      value = structuredClone(next);
      revision = String(Number(revision) + 1);
      writes++;
      return this.readNamespace();
    },
  };
  const registry = EnrollmentRegistry.open({ stateStore: store, config });
  return { registry, config, store, writes: () => writes };
}

test("public production first registration binds both roles atomically without account or admin import", async () => {
  const { registry, config, writes } = fixture();
  assert.equal(config.runtime_environment, "production");
  assert.equal(config.development_tofu, false);
  await registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP });
  assert.equal(writes(), 1);
  assert.equal(registry.getControllerReadiness(ENDPOINT).web_ready, true);
  assert.equal(registry.getControllerReadiness(ENDPOINT).desktop_ready, true);
  const exported = registry.exportSnapshot().registry;
  assert.equal(exported.entries[0].audit.source, "self_hosted");
  assert.equal(JSON.stringify(exported).includes(ACCESS), false);
  assert.equal(JSON.stringify(exported).includes(DESKTOP), false);
});

test("public registration retries authenticate existing roles without extra writes or credential replacement", async () => {
  const { registry, writes } = fixture();
  const request = { admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP };
  await registry.authorizeDesktopRegistration(request);
  await registry.authorizeDesktopRegistration(request);
  assert.equal(writes(), 1);
  for (const override of [{ accessToken: DESKTOP }, { desktopSecret: ACCESS }]) {
    await assert.rejects(registry.authorizeDesktopRegistration({ ...request, ...override }), { code: "invalid_enrollment_credential" });
  }
  assert.equal(writes(), 1);
});

test("public first registration requires the native random-ID format and distinct complete credentials", async () => {
  const { registry, writes } = fixture();
  for (const endpointId of ["short", "ep_abcdefghijklmnopqrstuvw", "chosen_device_name"]) {
    await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId, accessToken: ACCESS, desktopSecret: DESKTOP }), { code: "endpoint_id_invalid" });
  }
  await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: ACCESS }), { code: "invalid_enrollment_credential" });
  await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: "short" }), { code: "desktop_credential_invalid" });
  assert.equal(writes(), 0);
});

test("public single-role requests never create a partial enrollment", async () => {
  const { registry, writes } = fixture();
  await assert.rejects(registry.authorizeWebRegistration({ endpointId: ENDPOINT, accessToken: ACCESS }), { code: "enrollment_required" });
  await assert.rejects(registry.authorizePairingRegistration({ endpointId: ENDPOINT, desktopSecret: DESKTOP }), { code: "enrollment_required" });
  assert.equal(writes(), 0);
});

test("public generic authorization cannot create identities without explicit live admission", async () => {
  const { registry, writes } = fixture();
  for (const admitSelfRegistration of [undefined, false, "true"]) {
    await assert.rejects(registry.authorizeDesktopRegistration({ endpointId: ENDPOINT,
      accessToken: ACCESS, desktopSecret: DESKTOP, admitSelfRegistration }), { code: "enrollment_required" });
  }
  assert.equal(writes(), 0);
});

test("public registration cannot resurrect a revoked identity", async () => {
  const { registry, writes } = fixture();
  await registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP });
  await registry.revoke({ endpointId: ENDPOINT, expectedEndpointRevision: "1" });
  const before = writes();
  await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP }), { code: "endpoint_revoked" });
  assert.equal(writes(), before);
});

test("concurrent public registration accepts one credential pair and rejects the losing conflicting pair", async () => {
  const { registry, writes } = fixture();
  const results = await Promise.allSettled([
    registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP }),
    registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: DESKTOP, desktopSecret: ACCESS }),
  ]);
  assert.equal(results.filter(result => result.status === "fulfilled").length, 1);
  assert.equal(results.find(result => result.status === "rejected").reason.code, "invalid_enrollment_credential");
  assert.equal(writes(), 1);
});

test("self-hosted remains administrator-only and public never permits development TOFU", async () => {
  const { registry } = fixture(false);
  await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP }), { code: "enrollment_required" });
  assert.throws(() => fixture(true, { PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "true" }), { code: "development_tofu_forbidden" });
  assert.throws(() => fixture(true, { PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official" }), { code: "self_registration_forbidden" });
  assert.throws(() => fixture(true, { PINVOU_REMOTE_ENROLLMENT_SELF_REGISTRATION: "yes" }), { code: "self_registration_invalid" });
});

test("public registration never fills or replaces an existing partial administrator enrollment", async () => {
  const { registry, writes } = fixture();
  await registry.enroll({ endpointId: ENDPOINT, credential: ACCESS });
  await assert.rejects(registry.authorizeDesktopRegistration({ admitSelfRegistration: true, endpointId: ENDPOINT, accessToken: ACCESS, desktopSecret: DESKTOP }), { code: "desktop_credential_required" });
  assert.equal(writes(), 1);
  assert.equal(registry.getControllerReadiness(ENDPOINT).desktop_ready, false);
});
