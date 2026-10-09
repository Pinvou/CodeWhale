import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, realpath, rmdir, symlink, unlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";
import { isDirectCliEntry } from "../cli-entrypoint.js";

const root = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const entries = [
  { path: "scripts/check-enrollment-gated-v2-rollback.mjs", args: [] },
  { path: "remote-control-relay/scripts/enrollment-migrate.js", args: ["--invalid-cli-entry-regression-option"] },
  { path: "remote-control-relay/scripts/enrollment-gateway-preflight.js", args: [] },
];

test("canonical CLI entry identity rejects other and unreadable files", () => {
  assert.equal(isDirectCliEntry(import.meta.url, fileURLToPath(import.meta.url)), true);
  assert.equal(isDirectCliEntry(import.meta.url, join(root, entries[0].path)), false);
  assert.equal(isDirectCliEntry(import.meta.url, ""), false);
  assert.throws(() => isDirectCliEntry(import.meta.url, join(root, "nonexistent-cli-entry-regression.mjs")),
    (error) => error.code === "cli_entrypoint_unreadable" && !error.message.includes(root));
});

function safeEnvironment() {
  // Tests must never consume a developer's actual inventory/store/service.
  return Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("PINVOU_REMOTE_")));
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

for (const entry of entries) {
  test(`enrollment CLI executes fail-closed through directory link: ${entry.path}`, async (t) => {
    const directory = await realpath(await mkdtemp(join(tmpdir(), "pinvou-cli-entry-")));
    const alias = join(directory, "仓库 链接 %alias");
    let aliasCreated = false;
    t.after(async () => {
      if (aliasCreated) await unlink(alias);
      await rmdir(directory);
    });
    await symlink(root, alias, process.platform === "win32" ? "junction" : "dir");
    aliasCreated = true;
    // Unlink only the alias before removing the empty private fixture. Never
    // recursively remove a junction pointing at the real repository.
    const canonical = run(join(root, entry.path), entry.args);
    assert.equal(canonical.status, 1, "canonical invocation must reject missing/invalid configuration");
    const canonicalFailure = JSON.parse(canonical.stderr.trim());
    assert.equal(canonicalFailure.ok, false);
    const linked = run(join(alias, entry.path), entry.args);
    assert.equal(linked.status, canonical.status, "linked invocation must not silently succeed without executing checks");
    assert.equal(linked.stdout, canonical.stdout);
    assert.deepEqual(JSON.parse(linked.stderr.trim()), canonicalFailure);
  });

  test(`enrollment CLI executes with Windows path casing: ${entry.path}`, { skip: process.platform !== "win32" }, () => {
    const canonical = run(join(root, entry.path), entry.args);
    const alternate = run(join(root.toUpperCase(), entry.path), entry.args);
    assert.equal(canonical.status, 1);
    assert.equal(alternate.status, canonical.status);
    assert.equal(alternate.stdout, canonical.stdout);
    assert.deepEqual(JSON.parse(alternate.stderr.trim()), JSON.parse(canonical.stderr.trim()));
  });

  test(`enrollment CLI import has no side effects: ${entry.path}`, () => {
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
