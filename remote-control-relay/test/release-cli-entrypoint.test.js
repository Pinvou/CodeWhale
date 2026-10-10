import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, realpath, rmdir, symlink, unlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

const root = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const entries = [
  { path: "remote-control-relay/scripts/controller-release-gate.mjs", args: [] },
  { path: "remote-control-relay/scripts/validate-controller-release-evidence.mjs", args: [] },
  { path: "remote-control-relay/scripts/query-controller-release-telemetry.mjs", args: [] },
  { path: "remote-control-relay/scripts/verify-installer-supply-chain.mjs", args: [] },
  { path: "remote-control-relay/scripts/verify-provider-attestation.mjs", args: [] },
  { path: "remote-control-relay/scripts/prepare-legacy-gateway-route.mjs", args: [] },
  { path: "remote-control-relay/scripts/deploy-controller-preflight.mjs", args: [] },
  { path: "remote-control-relay/scripts/deploy-controller-preflight.mjs", args: ["--expect-feature-off"] },
];

function safeEnvironment() {
  // Never consume real release evidence, credentials, state or deployment settings.
  const allowed = new Set(["PATH", "Path", "PATHEXT", "SystemRoot", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"]);
  return {
    ...Object.fromEntries(Object.entries(process.env).filter(([key]) => allowed.has(key))),
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "invalid",
  };
}

function run(script, args) {
  const result = spawnSync(process.execPath, [script, ...args], {
    cwd: root, env: safeEnvironment(), shell: false, windowsHide: true,
    encoding: "utf8", timeout: 10_000, maxBuffer: 64 * 1024,
  });
  assert.ifError(result.error);
  assert.equal(result.signal, null);
  return result;
}

function assertFailure(result) {
  assert.equal(result.status, 1, "missing or invalid release input must fail closed");
  assert.notEqual(result.stderr.trim(), "", "a silent no-op is not validation");
}

for (const entry of entries) {
  const label = `${entry.path} ${entry.args.join(" ")}`.trim();
  test(`release CLI executes fail-closed through directory link: ${label}`, async (t) => {
    const directory = await realpath(await mkdtemp(join(tmpdir(), "pinvou-release-entry-")));
    const alias = join(directory, "发布 链接 %alias");
    let created = false;
    t.after(async () => {
      // Unlink only the alias; never recursively remove its repository target.
      if (created) await unlink(alias);
      await rmdir(directory);
    });
    await symlink(root, alias, process.platform === "win32" ? "junction" : "dir");
    created = true;
    const canonical = run(join(root, entry.path), entry.args);
    assertFailure(canonical);
    const linked = run(join(alias, entry.path), entry.args);
    assertFailure(linked);
    assert.equal(linked.stdout, canonical.stdout);
    assert.equal(linked.stderr, canonical.stderr);
  });

  test(`release CLI executes with Windows path casing: ${label}`, { skip: process.platform !== "win32" }, () => {
    const canonical = run(join(root, entry.path), entry.args);
    assertFailure(canonical);
    const alternate = run(join(root.toUpperCase(), entry.path), entry.args);
    assertFailure(alternate);
    assert.equal(alternate.stdout, canonical.stdout);
    assert.equal(alternate.stderr, canonical.stderr);
  });

  test(`release CLI import has no validation or deployment side effects: ${label}`, () => {
    const expression = `await import(${JSON.stringify(pathToFileURL(join(root, entry.path)).href)})`;
    const result = spawnSync(process.execPath, ["--input-type=module", "--eval", expression], {
      cwd: root, env: safeEnvironment(), shell: false, windowsHide: true,
      encoding: "utf8", timeout: 10_000, maxBuffer: 64 * 1024,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0);
    assert.equal(result.stdout, "");
    assert.equal(result.stderr, "");
  });
}
