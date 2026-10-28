import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync, writeFileSync, mkdtempSync, mkdirSync, renameSync, rmSync, statSync, existsSync, symlinkSync, chmodSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const deploy = readFileSync(new URL("../../scripts/deploy-remote-relay-test.sh", import.meta.url), "utf8").replaceAll("\r\n", "\n");
const identity = deploy.match(/<<'CONTROLLER_IDENTITY'\n([^]*?)\nCONTROLLER_IDENTITY/);
assert.ok(identity, "test must execute the actual deployment identity block");
const pinReport = deploy.match(/^report_test_controller_pin_status\(\) \{[^]*?^\}/m);
assert.ok(pinReport, "test must execute the actual deployment pin report");

function fixture(t) {
  const root = mkdtempSync(join(realpathSync(tmpdir()), "pinvou-test-identity-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const dir = join(root, "identity");
  const legacy = join(root, "code", "controller.env");
  mkdirSync(join(root, "state"), { mode: 0o700 });
  const state = join(root, "state", "controller-state.json");
  return { root, dir, legacy, state, env: join(dir, "controller.env"), pub: join(dir, "controller-public-key.b64") };
}
function prepare(f) {
  return spawnSync(process.execPath, ["-", f.dir, f.legacy, f.state], {
    input: identity[1], encoding: "utf8", timeout: 10_000,
  });
}
function passed(f) {
  const result = prepare(f);
  assert.equal(result.status, 0, "identity preparation should succeed without exposing key material");
  assert.equal(result.stdout, "");
}

test("actual deployment pin report distinguishes fresh and reused identities from both checkout defaults", (t) => {
  const f = fixture(t);
  const bash = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "bash";
  assert.ok(process.platform !== "win32" || existsSync(bash), "Git Bash is required");
  const sourceRoot = fileURLToPath(new URL("../..", import.meta.url));
  const managerPath = "pinvou3-app/src-tauri/src/features/remote_control/manager/mod.rs";
  const launcherPath = "scripts/run-remote-control-endpoint.ps1";
  const manager = readFileSync(join(sourceRoot, managerPath), "utf8");
  const launcher = readFileSync(join(sourceRoot, launcherPath), "utf8");
  const managerFile = join(f.root, managerPath);
  const launcherFile = join(f.root, launcherPath);
  mkdirSync(join(f.root, "pinvou3-app/src-tauri/src/features/remote_control/manager"), { recursive: true });
  mkdirSync(join(f.root, "scripts"), { recursive: true });
  writeFileSync(managerFile, manager);
  writeFileSync(launcherFile, launcher);
  const toShellPath = (value) => process.platform === "win32"
    ? value.replaceAll("\\", "/").replace(/^([A-Za-z]):/, (_, drive) => "/" + drive.toLowerCase())
    : value;
  const report = (key) => {
    const shell = "set -euo pipefail\n" + pinReport[0] + '\nreport_test_controller_pin_status "$1" "$2" "$3"\n';
    const result = spawnSync(bash, ["-c", shell, "pin-report-test", key, "relay-test-key-1", toShellPath(f.root)], {
      encoding: "utf8", timeout: 10_000,
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout + result.stderr;
  };
  const defaultKey = manager.match(/pub\(crate\) const TEST_PROFILE:[^]*?signing_public_key_b64: "([^"]+)"/)?.[1];
  assert.ok(defaultKey);
  assert.match(report(defaultKey), /内置 Test profile 的默认信任根与部署身份一致/);
  assert.match(report(defaultKey), /Windows launcher 的默认信任根与部署身份一致/);

  passed(f);
  const freshKey = readFileSync(f.pub, "utf8").trim();
  assert.notEqual(freshKey, defaultKey);
  const fresh = report(freshKey);
  assert.match(fresh, /内置 Test profile 不一致/);
  assert.match(fresh, /Windows launcher 的默认信任根不一致/);
  passed(f);
  assert.equal(readFileSync(f.pub, "utf8").trim(), freshKey);
  assert.match(report(freshKey), /内置 Test profile 不一致/);

  writeFileSync(managerFile, manager.replace(defaultKey, freshKey));
  const managerMatch = report(freshKey);
  assert.match(managerMatch, /内置 Test profile 的默认信任根与部署身份一致/);
  assert.match(managerMatch, /Windows launcher 的默认信任根不一致/);
  writeFileSync(launcherFile, launcher.replace(defaultKey, freshKey));
  const bothMatch = report(freshKey);
  assert.match(bothMatch, /内置 Test profile 的默认信任根与部署身份一致/);
  assert.match(bothMatch, /Windows launcher 的默认信任根与部署身份一致/);
  const matchedManager = manager.replace(defaultKey, freshKey);
  const matchedLauncher = launcher.replace(defaultKey, freshKey);
  const changedManagerId = matchedManager.replace('signing_key_id: "relay-test-key-1"', 'signing_key_id: "other-test-key"');
  assert.notEqual(changedManagerId, matchedManager);
  writeFileSync(managerFile, changedManagerId);
  const managerIdMismatch = report(freshKey);
  assert.match(managerIdMismatch, /内置 Test profile 不一致/);
  assert.match(managerIdMismatch, /Windows launcher 的默认信任根与部署身份一致/);
  writeFileSync(managerFile, matchedManager);
  const changedLauncherId = matchedLauncher.replace("$SigningKeyId = 'relay-test-key-1'", "$SigningKeyId = 'other-test-key'");
  assert.notEqual(changedLauncherId, matchedLauncher);
  writeFileSync(launcherFile, changedLauncherId);
  const launcherIdMismatch = report(freshKey);
  assert.match(launcherIdMismatch, /内置 Test profile 的默认信任根与部署身份一致/);
  assert.match(launcherIdMismatch, /Windows launcher 的默认信任根不一致/);
  writeFileSync(launcherFile, matchedLauncher);

  const tail = deploy.slice(deploy.indexOf('\nSTAGE="完成"\n') + 1);
  assert.ok(tail.startsWith('STAGE="完成"\n'));
  const runTail = (key, enabled) => {
    const shell = "set -euo pipefail\n" + pinReport[0]
      + '\nROOT="$1"; FIXTURE_KEY="$2"; ENABLE_DESKTOP_CONTROLLER="$3"; PUBLIC_URL="https://example.invalid"; BASE_PATH="/test"\n'
      + 'relay_ssh() { printf "%s" "$FIXTURE_KEY"; }\n' + tail;
    return spawnSync(bash, ["-c", shell, "pin-tail-test", toShellPath(f.root), key, enabled], {
      encoding: "utf8", timeout: 10_000,
    });
  };
  const success = runTail(freshKey, "1");
  assert.equal(success.status, 0, success.stderr);
  assert.match(success.stdout, /内置 Test profile 的默认信任根与部署身份一致/);
  assert.match(success.stdout, /Windows launcher 的默认信任根与部署身份一致/);
  const invalidKey = "A".repeat(42) + "B=";
  const invalid = runTail(invalidKey, "1");
  assert.notEqual(invalid.status, 0);
  assert.match(invalid.stderr, /已部署服务的 controller 公钥格式无效/);
  assert.doesNotMatch(invalid.stdout + invalid.stderr, new RegExp(invalidKey));
  const disabled = runTail(invalidKey, "0");
  assert.equal(disabled.status, 0);
  assert.doesNotMatch(disabled.stdout + disabled.stderr, /信任根检查|公钥格式无效/);
  rmSync(launcherFile);
  assert.match(report(freshKey), /无法确认当前源码 Windows launcher 的默认信任根/);
  rmSync(managerFile);
  const bothUnavailable = report(freshKey);
  assert.match(bothUnavailable, /无法确认当前源码内置 Test profile 的默认信任根/);
  assert.match(bothUnavailable, /无法确认当前源码 Windows launcher 的默认信任根/);
  assert.match(deploy, /report_test_controller_pin_status "\$controller_public_key" "relay-test-key-1" "\$ROOT"/);
});

test("test redeploy keeps exact signing and both HMAC identities outside the replaced code", (t) => {
  const f = fixture(t);
  passed(f);
  const originalEnv = readFileSync(f.env);
  const originalPublic = readFileSync(f.pub);
  writeFileSync(f.state, "{}", { mode: 0o600 });
  for (let i = 0; i < 3; i++) {
    const code = join(f.root, "code");
    mkdirSync(code, { recursive: true });
    renameSync(code, join(f.root, "replaced-code-" + i));
    passed(f);
    assert.deepEqual(readFileSync(f.env), originalEnv);
    assert.deepEqual(readFileSync(f.pub), originalPublic);
  }
  if (process.platform !== "win32") {
    assert.equal(statSync(f.dir).mode & 0o777, 0o700);
    assert.equal(statSync(f.env).mode & 0o777, 0o600);
    assert.equal(statSync(f.pub).mode & 0o777, 0o600);
  }
  assert.match(deploy, /EnvironmentFile=\$controller_identity_dir\/controller\.env/);
  assert.doesNotMatch(deploy, /EnvironmentFile=-\$test_dir\/controller\.env/);
  assert.ok(deploy.indexOf("CONTROLLER_IDENTITY\nfi") < deploy.indexOf('mv "$test_dir.extract" "$test_dir"'));
});

test("one-time code-directory migration retains exact old HMAC/signing bytes", (t) => {
  const f = fixture(t);
  passed(f);
  const env = readFileSync(f.env);
  const pub = readFileSync(f.pub);
  mkdirSync(join(f.root, "code"));
  renameSync(f.env, f.legacy);
  rmSync(f.dir, { recursive: true });
  writeFileSync(f.state, "{}", { mode: 0o600 });
  passed(f);
  assert.deepEqual(readFileSync(f.env), env);
  assert.deepEqual(readFileSync(f.pub), pub);
  assert.deepEqual(readFileSync(f.legacy), env);
});

test("missing public projection is recovered from the same persisted private key", (t) => {
  const f = fixture(t);
  passed(f);
  const env = readFileSync(f.env);
  const pub = readFileSync(f.pub);
  rmSync(f.pub);
  passed(f);
  assert.deepEqual(readFileSync(f.env), env);
  assert.deepEqual(readFileSync(f.pub), pub);
});

test("missing identity with existing state never silently creates a replacement", (t) => {
  const f = fixture(t);
  writeFileSync(f.state, "{}", { mode: 0o600 });
  assert.notEqual(prepare(f).status, 0);
  assert.equal(existsSync(f.env), false);
  assert.equal(existsSync(f.pub), false);
});

test("backup-only, marker-only and other managed traces never permit fresh identity", (t) => {
  for (const name of ["controller-state.json.bak", ".relay-state-marker", ".controller-state.lock", "controller-state.json.tmp-stale"]) {
    const f = fixture(t);
    const trace = join(f.root, "state", name);
    const original = Buffer.from("TEST_ONLY_EXISTING_MANAGED_STATE");
    writeFileSync(trace, original, { mode: 0o600 });
    assert.notEqual(prepare(f).status, 0, name);
    assert.equal(existsSync(f.env), false, name);
    assert.equal(existsSync(f.pub), false, name);
    assert.deepEqual(readFileSync(trace), original, name);
  }
});

test("unsafe identity ancestors fail before writing or repairing permissions", {
  skip: process.platform === "win32" ? "POSIX symlink and ownership contract" : false,
}, (t) => {
  for (const kind of ["symlink-root", "symlink-state", "symlink-legacy-parent", "public-root", "public-state"]) {
    const f = fixture(t);
    const external = join(f.root, "external");
    mkdirSync(external, { mode: 0o700 });
    let input = f;
    if (kind === "symlink-root") {
      const alias = join(f.root, "alias");
      symlinkSync(external, alias);
      input = { ...f, dir: join(alias, "identity") };
    }
    if (kind === "symlink-state") {
      rmSync(join(f.root, "state"), { recursive: true });
      symlinkSync(external, join(f.root, "state"));
    }
    if (kind === "symlink-legacy-parent") symlinkSync(external, join(f.root, "code"));
    if (kind === "public-root") chmodSync(f.root, 0o755);
    if (kind === "public-state") chmodSync(join(f.root, "state"), 0o755);
    const result = prepare(input);
    assert.notEqual(result.status, 0, kind);
    assert.equal(existsSync(f.env), false, kind);
    assert.equal(existsSync(f.pub), false, kind);
    assert.equal(existsSync(join(external, "identity", "controller.env")), false, kind);
    assert.equal(statSync(kind === "public-state" ? join(f.root, "state") : f.root).mode & 0o777,
      kind.startsWith("public-") ? 0o755 : 0o700, kind);
  }
});

test("corrupt, duplicate, partial or conflicting identity fails without overwriting", (t) => {
  for (const kind of ["corrupt-key", "duplicate", "missing-hmac", "conflicting-public", "wrong-deployment", "wrong-signing-key-id"]) {
    const f = fixture(t);
    passed(f);
    let content = readFileSync(f.env, "utf8");
    if (kind === "corrupt-key") content = content.replace(/PRIVATE_KEY_B64=.+/, "PRIVATE_KEY_B64=invalid");
    if (kind === "duplicate") content += "NODE_ENV=development\n";
    if (kind === "missing-hmac") content = content.replace(/^PINVOU_REMOTE_PAIRING_HMAC_KEY=.+\n/m, "");
    if (kind === "conflicting-public") writeFileSync(f.pub, "INVALID_TEST_ONLY\n");
    if (kind === "wrong-deployment") content = content.replace("NODE_ENV=development", "NODE_ENV=production");
    if (kind === "wrong-signing-key-id") content = content.replace("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=relay-test-key-1", "PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=wrong-test-key");
    writeFileSync(f.env, content);
    const beforeEnv = readFileSync(f.env);
    const beforePublic = readFileSync(f.pub);
    const result = prepare(f);
    assert.notEqual(result.status, 0, kind);
    assert.equal(result.stdout, "");
    assert.deepEqual(readFileSync(f.env), beforeEnv, kind);
    assert.deepEqual(readFileSync(f.pub), beforePublic, kind);
  }
});

test("release instructions refer to existing enrollment gate scripts from the repo root", () => {
  const root = fileURLToPath(new URL("../../", import.meta.url));
  for (const doc of ["Windows双端远程控制服务部署说明.md", "Windows双端远程控制双机演练清单.md", "Windows双端远程控制发布与回滚.md"]) {
    const body = readFileSync(join(root, "docs", doc), "utf8");
    assert.doesNotMatch(body, /(?<!remote-control-relay\/)scripts\/enrollment-(?:migrate|gateway-server|gateway-preflight)\.js/);
    for (const [entry] of body.matchAll(/remote-control-relay\/scripts\/enrollment-(?:migrate|gateway-server|gateway-preflight)\.js/g)) {
      assert.ok(existsSync(join(root, entry)), doc + ": " + entry);
    }
  }
});
