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
