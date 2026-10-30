import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const relay = dirname(dirname(fileURLToPath(import.meta.url)));
const helper = join(relay, "scripts", "controller-lock-rollout.sh");
function bashExecutable() {
  const configured = process.env.PINVOU_TEST_BASH;
  if (configured) {
    assert.ok(isAbsolute(configured) && existsSync(configured), "PINVOU_TEST_BASH must name an existing absolute Bash executable");
    return configured;
  }
  if (process.platform !== "win32") return "/bin/bash";
  const gitPaths = spawnSync("where.exe", ["git"], {
    encoding: "utf8", timeout: 10000, windowsHide: true, maxBuffer: 64 * 1024,
  });
  assert.ifError(gitPaths.error);
  if (gitPaths.status === 0) {
    for (const gitPath of (gitPaths.stdout || "").trim().split(/\r?\n/u).filter(Boolean)) {
      const candidate = join(dirname(dirname(gitPath)), "bin", "bash.exe");
      if (existsSync(candidate)) return candidate;
    }
  }
  throw new Error("Lock rollout tests require Git for Windows on PATH or PINVOU_TEST_BASH");
}
const bash = bashExecutable();
const capability = "export const RELAY_CONTROLLER_LOCK_PROTOCOL = 2;\n";

async function setup(t) {
  const directory = await fs.realpath(await fs.mkdtemp(join(tmpdir(), "pinvou-lock-rollout-")));
  t.after(() => fs.rm(directory, { recursive: true, force: true }));
  const state = join(directory, "state"), code = join(directory, "code"), backup = join(directory, "backup");
  await fs.mkdir(state); await fs.mkdir(code);
  await fs.writeFile(join(state, "state.json"), "fixture-state-preserved", { mode: 0o600 });
  function run(operation, confirmed = "0", { missing = false, stopFails = false } = {}) {
    const script = `source "$1"
      fixture_dir="$2"
      systemctl() {
        printf '%s\\n' "$*" >> "$fixture_dir/events"
        if [[ "$1" == show ]]; then printf '%s\\n' '${missing ? "not-found" : "loaded"}'; return; fi
        if [[ "$1" == stop ]]; then return ${stopFails ? 19 : 0}; fi
      }
      if [[ "$3" == upgrade ]]; then
        controller_lock_prepare_upgrade fixture.service "$2/state" "$2/code" "$2/backup" "$4"
      else
        controller_lock_before_rollback fixture.service "$2/state" "$2/code"
      fi`;
    const result = spawnSync(bash, ["-c", script, "--", helper.replaceAll("\\", "/"),
      directory.replaceAll("\\", "/"), operation, confirmed], {
      encoding: "utf8", windowsHide: true, timeout: 10000,
      env: { ...process.env, MSYS_NO_PATHCONV: "1" },
    });
    assert.ifError(result.error);
    return result;
  }
  return { directory, state, code, backup, run,
    events: async () => (await fs.readFile(join(directory, "events"), "utf8")).trim().split("\n") };
}

test("lock rollout refuses an unconfirmed legacy upgrade before stopping or archiving", async t => {
  const f = await setup(t), result = f.run("upgrade");
  assert.equal(result.status, 78, result.stderr);
  assert.match(result.stderr, /controller_lock_upgrade_confirmation_required/u);
  await assert.rejects(fs.stat(f.backup), { code: "ENOENT" });
  await assert.rejects(fs.stat(join(f.directory, "events")), { code: "ENOENT" });
});

test("lock rollout confirmed upgrade stops before preserving complete state", async t => {
  const f = await setup(t), result = f.run("upgrade", "1");
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(await f.events(), ["show fixture.service --property=LoadState --value", "stop fixture.service"]);
  assert.equal(await fs.readFile(join(f.backup, "controller-state", "state.json"), "utf8"), "fixture-state-preserved");
  assert.equal(await fs.readFile(join(f.state, "state.json"), "utf8"), "fixture-state-preserved");
});

test("lock rollout stop failure cannot archive or claim a successful upgrade", async t => {
  const f = await setup(t), result = f.run("upgrade", "1", { stopFails: true });
  assert.equal(result.status, 19, result.stderr);
  await assert.rejects(fs.stat(f.backup), { code: "ENOENT" });
});

test("lock rollback stays stopped when a fixed guard outlives an unaware old release", async t => {
  const f = await setup(t);
  await fs.writeFile(join(f.state, ".pinvou-controller-state.guard"), "");
  const result = f.run("rollback");
  assert.equal(result.status, 78, result.stderr);
  assert.match(result.stderr, /controller_lock_offline_downgrade_required/u);
  assert.deepEqual(await f.events(), ["show fixture.service --property=LoadState --value", "stop fixture.service"]);
  assert.equal((await fs.stat(join(f.state, ".pinvou-controller-state.guard"))).size, 0);
});

test("lock rollback permits unactivated or protocol-aware predecessors after stop", async t => {
  const f = await setup(t);
  assert.equal(f.run("rollback").status, 0);
  await fs.writeFile(join(f.state, ".pinvou-controller-state.guard"), "");
  await fs.writeFile(join(f.code, "relay-state-guard.js"), capability);
  const result = f.run("rollback");
  assert.equal(result.status, 0, result.stderr);
  assert.equal((await f.events()).filter(line => line === "stop fixture.service").length, 2);
});

test("lock rollout first deployment handles a missing service without a false stop failure", async t => {
  const f = await setup(t);
  const result = f.run("upgrade", "1", { missing: true });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(await f.events(), ["show fixture.service --property=LoadState --value"]);
});
