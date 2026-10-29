import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const helper = join(root, "remote-control-relay/scripts/controller-lock-rollout.sh");
function bashExecutable() {
  if (process.env.PINVOU_TEST_BASH) {
    assert.ok(isAbsolute(process.env.PINVOU_TEST_BASH) && existsSync(process.env.PINVOU_TEST_BASH));
    return process.env.PINVOU_TEST_BASH;
  }
  if (process.platform !== "win32") return "/bin/bash";
  const paths = spawnSync("where.exe", ["git"], { encoding: "utf8", timeout: 10000, windowsHide: true });
  assert.ifError(paths.error);
  for (const path of (paths.stdout || "").trim().split(/\r?\n/u).filter(Boolean)) {
    const candidate = join(dirname(dirname(path)), "bin", "bash.exe");
    if (existsSync(candidate)) return candidate;
  }
  throw new Error("Lock rollout tests require Git for Windows or PINVOU_TEST_BASH");
}
const bash = bashExecutable();

async function fixture(t, mode, options = {}) {
  const dir = await fs.realpath(await fs.mkdtemp(join(tmpdir(), "pinvou-early-lock-")));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const stage = join(dir, "stage"), old = join(dir, "old"), state = join(dir, "state"), backup = join(dir, "backup");
  await fs.mkdir(join(stage, "scripts"), { recursive: true });
  await fs.mkdir(old); await fs.mkdir(state);
  await fs.copyFile(helper, join(stage, "scripts/controller-lock-rollout.sh"));
  await fs.writeFile(join(old, "server.js"), "original-service");
  await fs.writeFile(join(state, "state.json"), "preserve-state");
  if (options.guard) await fs.writeFile(join(state, ".pinvou-controller-state.guard"), "guard");
  if (options.guardCompatible) await fs.writeFile(join(old, "relay-state-guard.js"), "export const RELAY_CONTROLLER_LOCK_PROTOCOL = 2;\n");
  await fs.mkdir(join(stage, "docs/schemas"), { recursive: true });
  await fs.writeFile(join(stage, "docs/schemas/behavior-telemetry-v1.schema.json"), "{}");
  await fs.writeFile(join(stage, "docs/schemas/behavior-telemetry-registries.json"), "{}");
  const filename = mode === "production" ? "scripts/deploy-remote-relay.sh" : "scripts/deploy-remote-relay-test.sh";
  const source = await fs.readFile(join(root, filename), "utf8");
  const startMarker = mode === "production" ? 'source "$code_stage/scripts/controller-lock-rollout.sh"' : 'source "$test_dir.extract/scripts/controller-lock-rollout.sh"';
  const endMarker = mode === "production" ? "# The state store deliberately refuses" : 'rm -rf "$test_dir.bak"';
  const start = source.indexOf(startMarker), end = source.indexOf(endMarker, start);
  assert.ok(start >= 0 && end > start, "execute the actual early deployment segment");
  let segment = source.slice(start, end);
  if (mode === "production" && options.stageFailure) {
    const rollback = source.indexOf("rollback() {", end);
    assert.ok(rollback > end);
    segment += source.slice(end, rollback);
  }
  if (mode === "test" && options.stageFailure) {
    const mutation = source.indexOf('[ ! -d "$test_dir" ] || mv "$test_dir" "$test_dir.bak"', end);
    assert.ok(mutation > end);
    segment += source.slice(end, mutation + '[ ! -d "$test_dir" ] || mv "$test_dir" "$test_dir.bak"'.length);
  }
  if (mode === "test") segment = segment.replaceAll("/var/lib/pinvou-remote-relay-test/state", "$state_dir");
  const initial = options.initial || "active";
  const script = `set -eEuo pipefail
    source_dir="$1"; service=fixture.service; state_dir="$source_dir/state"; controller_state_dir="$state_dir"
    code_stage="$source_dir/stage"; remote_dir="$source_dir/old"; backup="$source_dir/backup"
    controller_state_root="$source_dir/controller-root"; web_stage="$source_dir/web-stage"; web_tmp="$source_dir/web.tar"
    dropin_dir="$source_dir/dropin"; dropin="$dropin_dir/10-hardening.conf"; controller_dropin="$dropin_dir/40-controller.conf"
    test_dir="$source_dir/old"; cp -a "$code_stage" "$test_dir.extract"
    controller="${options.controller === false ? "0" : "1"}"; lock_upgrade_confirmed="${options.confirmed === false ? "0" : "1"}"
    printf '%s' '${initial}' > "$source_dir/active"
    systemctl() {
      printf '%s\\n' "$*" >> "$source_dir/events"
      case "$1" in
        show) if [[ '${options.showFails ? "1" : "0"}' == 1 ]]; then return 20; fi; if [[ "$*" == *LoadState* ]]; then printf '%s\\n' '${options.missing ? "not-found" : "loaded"}'; elif [[ '${options.postStopShowFails ? "1" : "0"}' == 1 && "$(cat "$source_dir/active")" != active ]]; then return 21; else cat "$source_dir/active"; fi ;;
        stop) if [[ '${options.cleanupStopFails ? "1" : "0"}' == 1 && -e "$source_dir/stopped-once" ]]; then return 19; fi; : > "$source_dir/stopped-once"; printf '${options.stopUnconfirmed ? "activating" : "inactive"}' > "$source_dir/active"; return ${options.stopFails ? 19 : 0} ;;
        start) if [[ '${options.startPartial ? "1" : "0"}' == 1 ]]; then printf active > "$source_dir/active"; return 23; fi; if [[ '${options.startFails ? "1" : "0"}' == 1 ]]; then return 23; fi; printf active > "$source_dir/active" ;;
        is-active) [[ '${options.healthFails ? "1" : "0"}' != 1 && "$(cat "$source_dir/active")" == active ]] ;;
        *) return 41 ;;
      esac
    }
    cp() { if [[ '${options.archiveFails ? "1" : "0"}' == 1 && "$1" == -a && "$2" == -- ]]; then return 31; fi; if [[ '${options.stageFailure || ""}' == backup && "$1" == -a && "$2" == "$remote_dir/server.js" ]]; then return 35; fi; command cp "$@"; }
    install() { if [[ '${options.stageFailure || ""}' == install ]]; then return 32; fi; command mkdir -p "\${!#}"; }
    node() { if [[ '${options.stageFailure || ""}' == web ]]; then return 33; fi; return 0; }
    tar() { return 0; }
    chown() { return 0; }
    rm() { if [[ '${options.stageFailure || ""}' == remove-backup && "$1" == -rf && "$2" == "$test_dir.bak" ]]; then return 34; fi; command rm "$@"; }
    mv() { if [[ '${options.stageFailure || ""}' == replace && "$1" == "$test_dir" ]]; then printf changed > "$test_dir/server.js"; return 36; fi; command mv "$@"; }
    ${segment}
    false
  `;
  const result = spawnSync(bash, ["-c", script, "--", dir.replaceAll("\\", "/")], {
    encoding: "utf8", timeout: 15000, windowsHide: true, env: { ...process.env, MSYS_NO_PATHCONV: "1" },
  });
  assert.ifError(result.error);
  const events = await fs.readFile(join(dir, "events"), "utf8").catch(() => "");
  return { result, events, state: await fs.readFile(join(dir, "active"), "utf8"), old, stateDir: state };
}

for (const mode of ["production", "test"]) {
  test(`${mode}: archive failure restores only the previously active unchanged service`, async t => {
    const f = await fixture(t, mode, { archiveFails: true });
    assert.equal(f.result.status, 31, f.result.stderr);
    assert.equal(f.state, "active");
    assert.match(f.events, /stop fixture\.service[\s\S]*start fixture\.service/u);
    assert.equal(await fs.readFile(join(f.old, "server.js"), "utf8"), "original-service");
    assert.equal(await fs.readFile(join(f.stateDir, "state.json"), "utf8"), "preserve-state");
  });
  test(`${mode}: failures after preparation recover without using an incomplete backup`, async t => {
    const f = await fixture(t, mode);
    assert.equal(f.result.status, 1, f.result.stderr);
    assert.equal(f.state, "active");
    assert.match(f.events, /stop fixture\.service[\s\S]*start fixture\.service/u);
  });
  test(`${mode}: confirmation refusal and inactive or missing units never start`, async t => {
    for (const options of [{ confirmed: false }, { initial: "inactive" }, { missing: true }]) {
      const f = await fixture(t, mode, options);
      assert.notEqual(f.result.status, 0);
      assert.doesNotMatch(f.events, /start fixture\.service/u);
    }
  });
  test(`${mode}: failed stop, incompatible guard, and failed start remain fail-closed`, async t => {
    for (const options of [{ stopFails: true }, { guard: true }, { startFails: true }, { startPartial: true }, { healthFails: true }]) {
      const f = await fixture(t, mode, options);
      assert.notEqual(f.result.status, 0);
      assert.equal(f.state, "inactive");
      if (!options.startFails && !options.startPartial && !options.healthFails) assert.doesNotMatch(f.events, /start fixture\.service/u);
      if (options.guard) assert.equal(await fs.readFile(join(f.stateDir, ".pinvou-controller-state.guard"), "utf8"), "guard");
    }
  });
  test(`${mode}: unreadable or unconfirmed service state never triggers recovery`, async t => {
    const unreadable = await fixture(t, mode, { showFails: true });
    assert.notEqual(unreadable.result.status, 0);
    assert.doesNotMatch(unreadable.events, /stop fixture\.service|start fixture\.service/u);
    const uncertain = await fixture(t, mode, { stopUnconfirmed: true });
    assert.notEqual(uncertain.result.status, 0);
    assert.doesNotMatch(uncertain.events, /start fixture\.service/u);
    assert.equal(uncertain.state, "activating");
    const afterStop = await fixture(t, mode, { postStopShowFails: true });
    assert.notEqual(afterStop.result.status, 0);
    assert.equal(afterStop.state, "inactive");
    assert.doesNotMatch(afterStop.events, /start fixture\.service/u);
    const changing = await fixture(t, mode, { initial: "activating" });
    assert.notEqual(changing.result.status, 0);
    assert.doesNotMatch(changing.events, /stop fixture\.service|start fixture\.service/u);
  });
  test(`${mode}: a partially started unit with failed cleanup is reported as unconfirmed`, async t => {
    const f = await fixture(t, mode, { startPartial: true, cleanupStopFails: true });
    assert.equal(f.result.status, 1, f.result.stderr);
    assert.equal(f.state, "active", "failed best-effort stop cannot be claimed as stopped");
    assert.match(f.result.stderr, /controller_lock_early_recovery_unconfirmed/u);
  });
  test(`${mode}: a guard-compatible old unit may be restored`, async t => {
    const f = await fixture(t, mode, { guard: true, guardCompatible: true });
    assert.equal(f.result.status, 1, f.result.stderr);
    assert.equal(f.state, "active");
    assert.match(f.events, /stop fixture\.service[\s\S]*start fixture\.service/u);
  });
}

test("test deployment also recovers an active controller-disabled unit before replacement", async t => {
  const f = await fixture(t, "test", { controller: false });
  assert.equal(f.result.status, 1, f.result.stderr);
  assert.equal(f.state, "active");
  assert.match(f.events, /stop fixture\.service[\s\S]*start fixture\.service/u);
});

test("production real preparation failures preserve old code and recover the unit", async t => {
  for (const [stageFailure, status] of [["install", 32], ["web", 33], ["backup", 35]]) {
    const f = await fixture(t, "production", { stageFailure });
    assert.equal(f.result.status, status, f.result.stderr);
    assert.equal(f.state, "active");
    assert.match(f.events, /stop fixture\.service[\s\S]*start fixture\.service/u);
    assert.equal(await fs.readFile(join(f.old, "server.js"), "utf8"), "original-service");
  }
});

test("test deployment handles backup cleanup failure but never restarts after replacement begins", async t => {
  const before = await fixture(t, "test", { stageFailure: "remove-backup" });
  assert.equal(before.result.status, 34, before.result.stderr);
  assert.equal(before.state, "active");
  const during = await fixture(t, "test", { stageFailure: "replace" });
  assert.equal(during.result.status, 36, during.result.stderr);
  assert.equal(during.state, "inactive");
  assert.doesNotMatch(during.events, /start fixture\.service/u);
  assert.equal(await fs.readFile(join(during.old, "server.js"), "utf8"), "changed");
});

test("production switches to complete-backup rollback before live replacement", async () => {
  const source = await fs.readFile(join(root, "scripts/deploy-remote-relay.sh"), "utf8");
  const switchAt = source.indexOf("controller_lock_early_armed=0\ntrap rollback_on_error ERR");
  assert.ok(switchAt > source.indexOf('cp -a "$module" "$backup/code-js/"'));
  assert.ok(switchAt < source.indexOf("install_db() {"));
});
