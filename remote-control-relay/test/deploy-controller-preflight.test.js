import assert from "node:assert/strict";
import { test } from "node:test";
import { spawnSync } from "node:child_process";
import { chmod, lstat, mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  parseControllerEnvironment,
  validateControllerEnvironment,
  validateControllerEnvironmentMetadata,
  validateControllerStateDirectories,
} from "../scripts/deploy-controller-preflight.mjs";
import { validateControllerRouteSmokeResponse } from "../scripts/controller-route-smoke.mjs";
import { loadEnrollmentRegistryConfig } from "../enrollment-registry.js";

const VALID_ENV = `
NODE_ENV=production
PINVOU_DESKTOP_CONTROLLER_ENABLED=1
PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted
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
`;

test("production controller environment matches the supported secure runtime profile", () => {
  assert.doesNotThrow(() => validateControllerEnvironment(parseControllerEnvironment(VALID_ENV)));
  const config = loadEnrollmentRegistryConfig(Object.fromEntries(parseControllerEnvironment(VALID_ENV)));
  assert.equal(config.deployment, "self_hosted");
  assert.equal(config.runtime_environment, "production");
  assert.equal(config.development_tofu, false);
  for (const replacement of [
    ["NODE_ENV=production", "NODE_ENV=development"],
    ["NODE_ENV=production", "NODE_ENV=test"],
    ["PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted", "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development"],
    ["PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false", "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=true"],
    ["PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false", "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=1"],
    ["PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false", "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU="],
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

test("production preflight rejects the old TOFU downgrade and unavailable official dispatcher", () => {
  const oldProfile = VALID_ENV
    .replace("NODE_ENV=production", "NODE_ENV=development")
    .replace("PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted", "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development")
    .replace("PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false", "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=true");
  assert.throws(
    () => validateControllerEnvironment(parseControllerEnvironment(oldProfile)),
    /controller_env_required_value_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironment(parseControllerEnvironment(
      VALID_ENV.replace("PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted", "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=official"),
    )),
    /official_controller_claim_dispatcher_unavailable/u,
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
    () => validateControllerEnvironmentMetadata({ ...valid, gid: 1000 }),
    /controller_env_owner_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, isFile: false }),
    /controller_env_file_type_invalid/u,
  );
  assert.throws(
    () => validateControllerEnvironmentMetadata({ ...valid, resolvedPath: "/tmp/controller.env" }),
    /controller_env_path_invalid/u,
  );
});

async function directoryFixture(t) {
  const root = await mkdtemp(join(await realpath(tmpdir()), "pinvou-production-state-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  return root;
}

test("production CLI never skips validation through a directory link", async (t) => {
  const root = await directoryFixture(t);
  const relay = fileURLToPath(new URL("../", import.meta.url));
  const alias = join(root, "linked relay");
  await symlink(relay, alias, process.platform === "win32" ? "junction" : "dir");
  const direct = join(relay, "scripts", "deploy-controller-preflight.mjs");
  const linked = join(alias, "scripts", "deploy-controller-preflight.mjs");
  for (const [entry, label] of [[direct, "real"], [linked, "linked"]]) {
    for (const args of [[join(root, "missing-controller.env")], []]) {
      const result = spawnSync(process.execPath, [entry, ...args], {
        cwd: root, encoding: "utf8", env: { ...process.env, NODE_OPTIONS: "" },
      });
      assert.ifError(result.error);
      assert.equal(result.status, 1, `${label} must reject invalid or missing env arguments`);
      assert.equal(result.stdout, "");
      assert.match(result.stderr.trim(), /^(?:ENOENT|controller_env_file_type_invalid)$/u);
      assert.ok(!result.stderr.includes(root), "failure output must not expose host paths");
    }
  }
});

test("importing the production preflight does not run the CLI", () => {
  const url = new URL("../scripts/deploy-controller-preflight.mjs", import.meta.url).href;
  const result = spawnSync(process.execPath, ["--input-type=module", "-e",
    `await import(${JSON.stringify(url)}); console.log('imported');`,
  ], { encoding: "utf8", env: { ...process.env, NODE_OPTIONS: "" } });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), "imported");
  assert.equal(result.stderr, "");
});

test("route smoke CLI never skips its gate through a directory link", async (t) => {
  const root = await directoryFixture(t);
  const relay = fileURLToPath(new URL("../", import.meta.url));
  const alias = join(root, "linked relay smoke");
  await symlink(relay, alias, process.platform === "win32" ? "junction" : "dir");
  for (const entry of [
    join(relay, "scripts", "controller-route-smoke.mjs"),
    join(alias, "scripts", "controller-route-smoke.mjs"),
  ]) {
    const sentinel = "REVIEW_SYNTHETIC_SENTINEL";
    const result = spawnSync(process.execPath, [entry, `invalid-url/${sentinel}?secret=${sentinel}`], {
      cwd: root, encoding: "utf8", env: { ...process.env, NODE_OPTIONS: "" },
    });
    assert.ifError(result.error);
    assert.equal(result.status, 1, "invalid smoke input must never pass the deployment gate");
    assert.equal(result.stdout, "");
    assert.equal(result.stderr.trim(), "controller_route_smoke_failed");
    assert.ok(!result.stderr.includes(sentinel), "smoke failure must not echo URL or credentials");
  }
});

test("importing the route smoke does not open a WebSocket", () => {
  const url = new URL("../scripts/controller-route-smoke.mjs", import.meta.url).href;
  const result = spawnSync(process.execPath, ["--input-type=module", "-e",
    `await import(${JSON.stringify(url)}); console.log('imported');`,
  ], { encoding: "utf8", env: { ...process.env, NODE_OPTIONS: "" }, timeout: 3000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), "imported");
  assert.equal(result.stderr, "");
});

test("state preflight does not create absent directories", async (t) => {
  const root = await directoryFixture(t);
  const missing = join(root, "missing");
  await validateControllerStateDirectories([{ path: missing, privateDirectory: true }]);
  await assert.rejects(lstat(missing), { code: "ENOENT" });
});

test("state preflight rejects real file and linked root/state before touching targets", async (t) => {
  const root = await directoryFixture(t);
  const target = join(root, "unrelated");
  await mkdir(target, { mode: 0o755 });
  const before = await lstat(target);
  for (const name of ["managed-root", "state"]) {
    const link = join(root, name);
    await symlink(target, link, process.platform === "win32" ? "junction" : "dir");
    await assert.rejects(validateControllerStateDirectories([{ path: link, privateDirectory: true }]),
      /controller_state_directory_type_invalid/u);
    const after = await lstat(target);
    assert.equal(after.mode, before.mode);
    assert.equal(after.uid, before.uid);
    assert.equal(after.gid, before.gid);
  }
  const file = join(root, "file");
  await writeFile(file, "unchanged");
  await assert.rejects(validateControllerStateDirectories([{ path: file, privateDirectory: true }]),
    /controller_state_directory_type_invalid/u);
  assert.equal(await readFile(file, "utf8"), "unchanged");
});

test("state preflight enforces real POSIX owners and private/ancestor modes", {
  skip: process.platform === "win32" ? "requires POSIX uid/gid and modes" : false,
}, async (t) => {
  const root = await directoryFixture(t);
  const owners = { expectedUid: process.getuid(), expectedGid: process.getgid() };
  const directories = [{ path: root, privateDirectory: true }];
  await chmod(root, 0o700);
  await validateControllerStateDirectories(directories, owners);
  await assert.rejects(validateControllerStateDirectories(directories, { ...owners, expectedUid: owners.expectedUid + 1 }),
    /controller_state_directory_owner_invalid/u);
  await assert.rejects(validateControllerStateDirectories(directories, { ...owners, expectedGid: owners.expectedGid + 1 }),
    /controller_state_directory_owner_invalid/u);
  await chmod(root, 0o755);
  await assert.rejects(validateControllerStateDirectories(directories, owners),
    /controller_state_directory_permissions_invalid/u);
  await validateControllerStateDirectories([{ path: root, privateDirectory: false }], owners);
  await chmod(root, 0o777);
  await assert.rejects(validateControllerStateDirectories([{ path: root, privateDirectory: false }], owners),
    /controller_state_directory_permissions_invalid/u);
  assert.equal((await lstat(root)).mode & 0o777, 0o777, "preflight must not chmod unsafe directories");
});

test("production CLI checks every fixed state ancestor before install mutates it", async () => {
  const source = await readFile(new URL("../scripts/deploy-controller-preflight.mjs", import.meta.url), "utf8");
  for (const path of ["/", "/var", "/var/lib", "/var/lib/pinvou-remote-relay", "/var/lib/pinvou-remote-relay/state"]) {
    assert.ok(source.includes(`{ path: "${path}", privateDirectory:`));
  }
  assert.match(source, /validateControllerEnvironmentFile\(process\.argv\[2\] \|\| ""\)\s*\.then\(\(\) => validateControllerStateDirectories\(\)\)/u);
  const deploy = await readFile(new URL("../../scripts/deploy-remote-relay.sh", import.meta.url), "utf8");
  assert.ok(deploy.indexOf('node "$code_stage/scripts/deploy-controller-preflight.mjs" "$controller_env"')
    < deploy.indexOf('install -d -o root -g root -m 700 "$controller_state_root"'));
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
