import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { access, chmod, copyFile, mkdir, mkdtemp, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repoRoot = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const deployScript = join(repoRoot, "scripts", "deploy-remote-relay.sh");
const productionHardening = join(repoRoot, "remote-control-relay", "10-hardening.conf");

function bashExecutable() {
  if (process.platform !== "win32") return "bash";
  // Resolve the active Git installation instead of assuming a drive or folder.
  const gitPaths = spawnSync("where.exe", ["git"], { encoding: "utf8", timeout: 10_000 });
  for (const path of (gitPaths.stdout || "").trim().split(/\r?\n/).filter(Boolean)) {
    const candidate = join(dirname(dirname(path)), "bin", "bash.exe");
    if (existsSync(candidate)) return candidate;
  }
  throw new Error("Deployment tests require Git for Windows on PATH");
}

test("production hardening keeps deployment-specific proxy addresses out of the repository", async () => {
  const source = await readFile(productionHardening, "utf8");
  assert.doesNotMatch(source, /PINVOU_REMOTE_(?:ALLOWED|TRUSTED)_PROXY_IPS=/);
  const preflight = await readFile(join(repoRoot, "remote-control-relay", "scripts", "deploy-controller-preflight.mjs"), "utf8");
  assert.match(preflight, /"PINVOU_REMOTE_ALLOWED_PROXY_IPS"/);
  assert.match(preflight, /"PINVOU_REMOTE_TRUSTED_PROXY_IPS"/);
  assert.match(preflight, /controller_env_proxy_ips_mismatch/);
});

test("production deployment requires explicit confirmed targets before network or upload", async () => {
  const source = await readFile(deployScript, "utf8");
  const config = source.match(/^SERVER=[^]*?(?=^BASE_PATH=)/m)?.[0];
  assert.ok(config, "execute the actual production configuration gate");
  const bash = bashExecutable();
  const valid = {
    PINVOU_REMOTE_DEPLOY_SERVER: "fixture@relay.invalid",
    PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER: "fixture@relay.invalid",
    SSH_KEY: "fixture-key", PINVOU_REMOTE_DIRECT_URL: "http://direct.invalid/pinvou3/remote",
  };
  const cases = [valid,
    ...Object.keys(valid).map((key) => ({ ...valid, [key]: "" })),
    { ...valid, PINVOU_REMOTE_DEPLOY_SERVER: "other@relay.invalid" },
    { ...valid, PINVOU_REMOTE_DEPLOY_SERVER: "-oProxyCommand=bad", PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER: "-oProxyCommand=bad" },
  ];
  for (let i = 0; i < cases.length; i++) {
    const result = spawnSync(bash, ["-c", "set -euo pipefail\n" + config + '\nprintf "validated\\n"'],
      { env: { ...process.env, ...cases[i] }, encoding: "utf8", timeout: 10_000 });
    assert.equal(result.error, undefined);
    assert.equal(result.status, i === 0 ? 0 : 64);
    assert.equal(result.stdout, i === 0 ? "validated\n" : "");
    for (const value of Object.values(cases[i]).filter(Boolean)) assert.equal(result.stderr.includes(value), false);
  }
});

test("production deploy builds and replaces the complete shared WebUI dist", async () => {
  const source = await readFile(deployScript, "utf8");
  assert.match(source, /PINVOU_REMOTE_PUBLIC_BASE_PATH="\$BASE_PATH" npm run build:web/);
  assert.match(source, /tar -czf - -C "\$RELAY_DIR\/web\/dist" \./);
  assert.match(source, /behavior-telemetry-schema\.js/);
  assert.doesNotMatch(source, /\bmapfile\b/);
  assert.match(source, /while IFS= read -r relay_module; do/);
  assert.match(source, /relay_modules\+=\("\$relay_module"\)/);
  assert.match(source, /"\$\{relay_modules\[@\]\}"/);
  assert.match(source, /relay_code_capture "\$remote_dir" "\$backup\/code-js"/);
  assert.match(source, /for module in "\$code_stage"\/\*\.js/);
  assert.equal((source.match(/relay_code_stage "\$backup\/code-js" "\$remote_dir"/g) || []).length, 2);
  assert.equal((source.match(/relay_code_replace "\$staged" "\$remote_dir"/g) || []).length, 2);
  assert.match(source, /docs\/schemas\/behavior-telemetry-v1\.schema\.json/);
  assert.match(source, /docs\/schemas\/behavior-telemetry-registries\.json/);
  assert.match(source, /await import\('\.\/telemetry-service\.js'\)/);
  assert.match(source, /cp -a "\$remote_dir\/docs\/schemas" "\$backup\/telemetry-schemas"/);
  assert.match(source, /deploy_output=.*<<'REMOTE'\r?\nset -Eeuo pipefail/);
  assert.match(source, /tar --no-same-owner --no-same-permissions -xzf "\$web_tmp" -C "\$web_stage"/);
  assert.match(source, /chown -R root:root "\$web_stage"/);
  assert.match(source, /find "\$web_stage" -type d -exec chmod 755 \{\} \+/);
  assert.match(source, /find "\$web_stage" -type f -exec chmod 644 \{\} \+/);
  assert.match(source, /cp -a "\$remote_dir\/web\/dist" "\$backup\/web-dist"/);
  assert.match(source, /mv "\$web_stage" "\$remote_dir\/web\/dist"/);
  assert.match(source, /PINVOU_CONFIRM_PRODUCTION_DEPLOY/);
  assert.match(source, /desktop_controller_enabled !== true/);
  assert.match(source, /"\$health" 2>\/dev\/null/);
  assert.match(source, /40-controller\.conf/);
  assert.match(source, /EnvironmentFile=%s/);
  assert.match(source, /pinvou-remote-relay-controller\.env/);
  assert.match(source, /scripts\/deploy-controller-preflight\.mjs/);
  assert.match(source, /controller_state_root="\/var\/lib\/pinvou-remote-relay"/);
  assert.match(source, /controller_state_dir="\$controller_state_root\/state"/);
  assert.match(source, /install -d -o root -g root -m 700 "\$controller_state_root"/);
  assert.match(source, /install -d -o root -g root -m 700 "\$controller_state_dir"/);
  assert.match(source, /scripts\/controller-route-smoke\.mjs/);
  assert.equal((source.match(/for _ in \$\(seq 1 30\); do/g) || []).length, 2);
  assert.match(source, /if curl -fsS --connect-timeout 3 --max-time 5 http:\/\/127\.0\.0\.1:8787\/pinvou3\/remote\/healthz >\/dev\/null 2>&1/);
  assert.match(source, /controller-v1 路由烟测失败/);
  assert.equal((source.match(/cp -a "\$module" "\$remote_dir\/"/g) || []).length, 1);
  assert.doesNotMatch(source, /PINVOU_ALLOW_LEGACY_REMOTE_DEPLOY/);
  assert.doesNotMatch(source, /nginx -t|systemctl reload nginx/);
});

test("production relay code artifact supports a cold ESM load", async (t) => {
  const relayDir = join(repoRoot, "remote-control-relay");
  const root = await mkdtemp(join(relayDir, ".deploy-cold-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const stage = join(root, "stage");
  await mkdir(stage);
  // Keep archive bytes on stdio and tar operands relative to cwd on Windows.
  const packed = spawnSync("tar", [
    "-czf", "-",
    "-C", "remote-control-relay",
    ...(await readdir(relayDir)).filter((name) => name.endsWith(".js")),
    "-C", "..",
    "docs/schemas/behavior-telemetry-v1.schema.json",
    "docs/schemas/behavior-telemetry-registries.json",
  ], { cwd: repoRoot, timeout: 10_000 });
  assert.equal(packed.status, 0, packed.stderr?.toString("utf8"));
  const extracted = spawnSync("tar", ["-xzf", "-"], {
    cwd: stage,
    input: packed.stdout,
    encoding: "utf8",
    timeout: 10_000,
  });
  assert.equal(extracted.status, 0, extracted.stderr);
  const loaded = spawnSync(process.execPath, [
    "--input-type=module",
    "-e",
    "await import('./telemetry-service.js')",
  ], { cwd: stage, encoding: "utf8", timeout: 10_000 });
  assert.equal(loaded.status, 0, loaded.stderr);
});

test("rollback verifies and stages the complete code backup before touching live modules", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "relay-code-backup-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const helper = join(repoRoot, "remote-control-relay", "scripts", "relay-code-backup.sh");
  const bash = bashExecutable();
  const run = (directory, script) => spawnSync(bash, ["-c", [
    "set -euo pipefail", 'source "$1"', 'cd -- "$2"', script,
  ].join("\n"), "fixture", helper.replaceAll("\\", "/"), directory.replaceAll("\\", "/")], {
    encoding: "utf8", timeout: 25_000,
  });
  for (const variant of ["valid", "empty", "missing", "truncated", "extra", "manifest",
    "paired-suffix", "missing-lf", "embedded-nul", "replace-failure"]) {
    const dir = join(root, variant);
    await mkdir(join(dir, "old"), { recursive: true });
    await mkdir(join(dir, "live"));
    await writeFile(join(dir, "live", "server.js"), "new server");
    await writeFile(join(dir, "live", "new-only.js"), "new only");
    if (variant !== "empty") {
      await writeFile(join(dir, "old", "server.js"), "old server");
      await writeFile(join(dir, "old", "z-module.js"), "old module");
    }
    const capture = run(dir, 'relay_code_capture "$PWD/old" "$PWD/backup"');
    assert.equal(capture.status, 0, capture.stderr);
    const backup = (name) => join(dir, "backup", name);
    if (variant === "missing") await rm(backup("z-module.js"));
    if (variant === "truncated") await writeFile(backup("z-module.js"), "short");
    if (variant === "extra") await writeFile(backup("extra.js"), "extra");
    if (variant === "manifest") await writeFile(backup(".manifest.sha256"), "");
    if (["paired-suffix", "missing-lf", "embedded-nul"].includes(variant)) {
      const manifest = await readFile(backup(".manifest.sha256"));
      if (variant === "paired-suffix") {
        await rm(backup("z-module.js"));
        await writeFile(backup(".manifest.sha256"), manifest.subarray(0, manifest.lastIndexOf(10, manifest.length - 2) + 1));
      } else if (variant === "missing-lf") {
        await writeFile(backup(".manifest.sha256"), manifest.subarray(0, -1));
      } else {
        await writeFile(backup(".manifest.sha256"), Buffer.concat([manifest.subarray(0, 8), Buffer.of(0), manifest.subarray(8)]));
      }
    }
    const inject = variant === "replace-failure"
      ? 'mv() { case "$2" in */incoming/z-module.js) return 31 ;; esac; command mv "$@"; }'
      : "";
    const result = run(dir, [
      'staged="$(relay_code_stage "$PWD/backup" "$PWD/live")"', inject,
      'relay_code_replace "$staged" "$PWD/live"',
    ].join("\n"));
    const live = join(dir, "live");
    if (variant === "valid" || variant === "empty") {
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual((await readdir(live)).sort(), variant === "empty" ? [] : ["server.js", "z-module.js"]);
      if (variant === "valid") assert.equal(await readFile(join(live, "server.js"), "utf8"), "old server");
    } else {
      assert.equal(result.status, 1, `${variant}: ${result.stderr}`);
      assert.match(result.stderr, variant === "replace-failure" ? /previous code restored/u : /backup_invalid/u);
      assert.deepEqual((await readdir(live)).sort(), ["new-only.js", "server.js"]);
      assert.equal(await readFile(join(live, "server.js"), "utf8"), "new server");
      assert.equal(await readFile(join(live, "new-only.js"), "utf8"), "new only");
    }
  }
});

test("both production rollback entries reject a corrupt backup before stopping the service", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "relay-rollback-entry-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const source = (await readFile(deployScript, "utf8")).replaceAll("\r\n", "\n");
  const jsBoundary = 'rm -rf "$remote_dir/web/dist"';
  const autoStart = source.indexOf("\nrollback() {\n");
  const manualStart = source.indexOf('\nstamp="${backup##*/}"\n');
  const autoEnd = source.indexOf(jsBoundary, autoStart);
  const manualEnd = source.indexOf(jsBoundary, manualStart);
  assert.ok(autoStart >= 0 && autoEnd > autoStart && manualStart >= 0 && manualEnd > manualStart);
  const helper = join(repoRoot, "remote-control-relay", "scripts", "relay-code-backup.sh");
  for (const [name, body] of [
    ["automatic", `${source.slice(autoStart, autoEnd)}\nprintf continued > restored\n}\nrollback`],
    ["public-check", `${source.slice(manualStart, manualEnd)}\nprintf continued > restored`],
  ]) {
    const dir = join(root, name);
    await mkdir(join(dir, "old"), { recursive: true });
    const backup = join(dir, "live", "backups", "20261109-010000");
    await mkdir(backup, { recursive: true });
    await copyFile(helper, join(backup, "relay-code-backup.sh"));
    await writeFile(join(backup, "controller-lock-rollout.sh"),
      'controller_lock_before_rollback() { printf called > stopped; test -f "$3/server.js"; }\n');
    await writeFile(join(dir, "old", "server.js"), "old server");
    await writeFile(join(dir, "live", "server.js"), "new server");
    const run = (script) => spawnSync(bashExecutable(), ["-c", [
      "set -euo pipefail", 'cd -- "$1"', 'remote_dir="$PWD/live"',
      'backup="$remote_dir/backups/20261109-010000"', 'service=fixture', 'controller_state_dir=fixture',
      'source "$backup/relay-code-backup.sh"', 'source "$backup/controller-lock-rollout.sh"',
      script, body,
    ].join("\n"), "fixture", dir.replaceAll("\\", "/")], { encoding: "utf8", timeout: 10_000 });
    const corrupt = run('relay_code_capture "$PWD/old" "$backup/code-js"; printf corrupt > "$backup/code-js/server.js"');
    assert.equal(corrupt.status, 1, corrupt.stderr);
    assert.match(corrupt.stderr, /backup_invalid/u);
    await assert.rejects(access(join(dir, "stopped")), { code: "ENOENT" });
    assert.equal(await readFile(join(dir, "live", "server.js"), "utf8"), "new server");
    if (name === "public-check" && process.platform !== "win32") {
      const alternate = join(dir, "live", "backups", "20261109-010001");
      await symlink(backup, alternate, "dir");
      const alias = run('cp -- "$PWD/old/server.js" "$backup/code-js/server.js"; backup="$remote_dir/backups/20261109-010001"');
      assert.equal(alias.status, 1, alias.stderr);
      assert.match(alias.stderr, /拒绝未知备份目录/u);
      await assert.rejects(access(join(dir, "stopped")), { code: "ENOENT" });
    }
    const valid = run([
      'cp -- "$PWD/old/server.js" "$backup/code-js/server.js"',
      'rm() { [[ "$*" != *"/.relay-code-rollback."* ]] || return 79; command rm "$@"; }',
    ].join("\n"));
    assert.equal(valid.status, 0, valid.stderr);
    assert.match(valid.stderr, /relay_code_cleanup_pending/u);
    assert.equal(await readFile(join(dir, "stopped"), "utf8"), "called");
    assert.equal(await readFile(join(dir, "restored"), "utf8"), "continued");
    assert.equal(await readFile(join(dir, "live", "server.js"), "utf8"), "old server");
  }
});

test("manual rollback CLI verifies real backup bytes before stopping and replacing code", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "relay-manual-rollback-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const bash = bashExecutable();
  const stamp = "20261109-010000";
  const dir = root.replaceAll("\\", "/");
  const live = join(root, "live");
  const backup = join(live, "backups", stamp);
  await mkdir(backup, { recursive: true });
  await mkdir(join(root, "old"));
  for (const helper of ["controller-lock-rollout.sh", "relay-code-backup.sh"]) {
    await copyFile(join(repoRoot, "remote-control-relay", "scripts", helper), join(backup, helper));
  }
  const runner = [
    'set -euo pipefail',
    'fixture="$1"; shift',
    'export PINVOU_REMOTE_DEPLOY_DIR="$fixture/live"',
    'systemctl() { case "$1" in show) printf loaded ;; stop) printf stopped > "$PINVOU_REMOTE_DEPLOY_DIR/../stopped" ;; *) return 98 ;; esac; }',
    'export -f systemctl',
    'ssh() {',
    '  printf ssh >> "$fixture/calls"',
    '  while (( $# > 0 )) && [[ "$1" != -- ]]; do shift; done; shift',
    // Execute the real SSH payload through the complete JS transaction. The
    // separate asset/systemd restoration is outside this isolated code fixture.
    '  local line',
    '  while IFS= read -r line; do',
    '    [[ "$line" != \'rm -rf "$remote_dir/web/dist"\' ]] || break',
    '    printf "%s\\n" "$line"',
    '  done > "$fixture/remote-code.sh"',
    '  cat >/dev/null',
    '  bash "$fixture/remote-code.sh" "$@"',
    '}',
    'curl() { case "${!#}" in http://direct.invalid/*) return 22 ;; */healthz) printf \'{"ok":true,"room_count":0}\' ;; */r/deploy-check) printf old-page ;; *) return 99 ;; esac; }',
    'npm() { echo unexpected-build >&2; return 99; }',
    'scp() { echo unexpected-upload >&2; return 99; }',
    'source "$0" "$@"',
  ].join("\n");
  const env = {
    ...process.env,
    PINVOU_CONFIRM_PRODUCTION_DEPLOY: "1", PINVOU_CONFIRM_CONTROLLER_LOCK_UPGRADE: "0",
    PINVOU_REMOTE_DEPLOY_SERVER: "fixture@relay.invalid",
    PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER: "fixture@relay.invalid", SSH_KEY: "fixture-key",
    PINVOU_REMOTE_PUBLIC_URL: "https://public.invalid/pinvou3/remote",
    PINVOU_REMOTE_DIRECT_URL: "http://direct.invalid/pinvou3/remote",
    PINVOU_REMOTE_PUBLIC_BASE_PATH: "/pinvou3/remote", PINVOU_REMOTE_HARDENING_FILE: "",
  };
  const run = (args, overrides = {}) => spawnSync(bash,
    ["-c", runner, deployScript.replaceAll("\\", "/"), dir, ...args],
    { env: { ...env, ...overrides }, encoding: "utf8", timeout: 15_000 });
  for (const [args, overrides] of [
    [["--rollback"], {}], [["--rollback", "../other"], {}],
    [["--rollback", stamp, "extra"], {}], [["--unknown", stamp], {}],
    [["--rollback", stamp], { PINVOU_CONFIRM_PRODUCTION_DEPLOY: "0" }],
    [["--rollback", stamp], { PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER: "other@relay.invalid" }],
  ]) {
    const result = run(args, overrides);
    assert.equal(result.status, 64, result.stdout + result.stderr);
    assert.equal(await pathExists(join(root, "calls")), false);
  }
  for (const mode of ["corrupt", "valid", "empty"]) {
    await rm(join(backup, "code-js"), { recursive: true, force: true });
    await rm(join(root, "stopped"), { force: true });
    await writeFile(join(live, "server.js"), "new server");
    await writeFile(join(live, "new-only.js"), "new module");
    await rm(join(root, "old", "server.js"), { force: true });
    if (mode !== "empty") await writeFile(join(root, "old", "server.js"), "old server");
    const captured = spawnSync(bash, ["-c", [
      'set -euo pipefail', 'source "$1/live/backups/20261109-010000/relay-code-backup.sh"',
      'relay_code_capture "$1/old" "$1/live/backups/20261109-010000/code-js"',
    ].join("\n"), "capture", dir], { encoding: "utf8", timeout: 10_000 });
    assert.equal(captured.status, 0, captured.stderr);
    if (mode === "corrupt") await writeFile(join(backup, "code-js", "server.js"), "truncated");
    const result = run(["--rollback", stamp]);
    assert.equal(result.error, undefined);
    assert.equal(result.status, mode === "corrupt" ? 1 : 0, result.stdout + result.stderr);
    if (mode === "corrupt") {
      assert.match(result.stderr, /relay_code_backup_invalid/u);
      assert.equal(await pathExists(join(root, "stopped")), false);
      assert.equal(await readFile(join(live, "server.js"), "utf8"), "new server");
      assert.equal(await readFile(join(live, "new-only.js"), "utf8"), "new module");
    } else {
      assert.match(result.stdout, /人工回滚完成并通过公网复检/u);
      assert.equal(await readFile(join(root, "stopped"), "utf8"), "stopped");
      assert.equal(await pathExists(join(live, "new-only.js")), false);
      assert.equal(await pathExists(join(live, "server.js")), mode === "valid");
      if (mode === "valid") assert.equal(await readFile(join(live, "server.js"), "utf8"), "old server");
    }
  }
});

test("staged controller smoke resolves its protocol after the real module install loop", async (t) => {
  const relayDir = join(repoRoot, "remote-control-relay");
  const root = await mkdtemp(join(relayDir, ".deploy-stage-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const stage = join(root, "stage");
  const installed = join(root, "installed");
  await mkdir(join(stage, "scripts"), { recursive: true });
  await mkdir(installed);
  for (const name of (await readdir(relayDir)).filter((entry) => entry.endsWith(".js"))) {
    await copyFile(join(relayDir, name), join(stage, name));
  }
  for (const name of ["controller-route-smoke.mjs", "deploy-controller-preflight.mjs"]) {
    await copyFile(join(relayDir, "scripts", name), join(stage, "scripts", name));
  }
  // Replay the install loop from the actual deployment script, not a test copy.
  const source = await readFile(deployScript, "utf8");
  const loop = source.match(/for module in "\$code_stage"\/\*\.js; do[\s\S]*?\r?\ndone/);
  assert.ok(loop, "the deployment must install the complete root module set");
  const bash = bashExecutable();
  const copied = spawnSync(bash, ["-c", `set -euo pipefail\ncode_stage=stage\nremote_dir=installed\n${loop[0]}`], { cwd: root, encoding: "utf8", timeout: 10_000 });
  assert.equal(copied.status, 0, copied.stderr || String(copied.error));
  const loaded = spawnSync(process.execPath, [
    "--input-type=module",
    "-e",
    "await import('./stage/scripts/controller-route-smoke.mjs'); await import('./stage/scripts/deploy-controller-preflight.mjs')",
  ], { cwd: root, encoding: "utf8", timeout: 10_000 });
  assert.equal(loaded.status, 0, loaded.stderr);
  assert.equal(await readFile(join(installed, "protocol-controller.js"), "utf8"), await readFile(join(stage, "protocol-controller.js"), "utf8"));
  assert.equal(await readFile(join(installed, "cli-entrypoint.js"), "utf8"), await readFile(join(stage, "cli-entrypoint.js"), "utf8"));
  // Replay the deployment's preflight command with invalid input. Its staged
  // helper dependency must resolve, and the gate must execute before mutation.
  const command = source.match(/^node "\$code_stage\/scripts\/deploy-controller-preflight\.mjs" "\$controller_env"$/m);
  assert.ok(command, "exercise the actual staged CLI invocation");
  const rejected = spawnSync(bash, ["-c", `set -euo pipefail\ncode_stage=stage\ncontroller_env=missing-controller.env\n${command[0]}`], { cwd: root, encoding: "utf8", timeout: 10_000 });
  assert.equal(rejected.status, 1, rejected.error?.message || rejected.stderr);
  assert.equal(rejected.stderr.trim(), "ENOENT");
});

test("deploy module collection does not require mapfile and preserves relative names", async (t) => {
  const source = await readFile(deployScript, "utf8");
  const collection = source.match(/^relay_modules=\(\)[\s\S]*?^done < <\(cd "\$RELAY_DIR" && printf '%s\\n' \.\/\*\.js\)/m);
  assert.ok(collection, "exercise the actual deployment module collection");
  const root = await mkdtemp(join(tmpdir(), "pinvou-relay-modules-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const relayDir = join(root, "relay with spaces");
  await mkdir(relayDir);
  for (const name of ["server.js", "extra module.js", "ignored.txt"]) {
    await writeFile(join(relayDir, name), "");
  }
  const bash = bashExecutable();
  const collected = spawnSync(bash, ["-c", [
    "set -euo pipefail",
    "enable -n mapfile 2>/dev/null || true",
    'RELAY_DIR="$1"',
    collection[0],
    'printf \'%s\\n\' "${relay_modules[@]}"',
  ].join("\n"), "collect", relayDir.replaceAll("\\", "/")], { encoding: "utf8", timeout: 10_000 });
  assert.equal(collected.status, 0, collected.error?.message || collected.stderr);
  assert.deepEqual(collected.stdout.trim().split(/\r?\n/), ["./extra module.js", "./server.js"]);
});

async function executable(path, content) {
  await writeFile(path, content);
  await chmod(path, 0o755);
}

async function pathExists(path) {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

async function ensureMinimalWebDist(t) {
  const dist = join(repoRoot, "remote-control-relay", "web", "dist");
  const index = join(dist, "index.html");
  const app = join(dist, "assets", "app.js");
  const startup = join(dist, "startup", "fixture.js");
  const distExisted = await pathExists(dist);
  const createdFiles = [];

  await mkdir(dist, { recursive: true });
  if (!(await pathExists(index))) {
    await writeFile(index, [
      "<!doctype html>",
      '<title>PINVOU 智能助手</title>',
      '<base href="/pinvou3/remote/">',
      '<script src="/pinvou3/remote/startup/fixture.js"></script>',
      '<script type="module" src="/pinvou3/remote/assets/app.js"></script>',
    ].join("\n"));
    createdFiles.push(index);
  }
  if (createdFiles.includes(index)) {
    for (const asset of [app, startup]) {
      await mkdir(dirname(asset), { recursive: true });
      await writeFile(asset, "/* isolated deployment fixture */");
      createdFiles.push(asset);
    }
  }

  t.after(async () => {
    if (!distExisted) {
      await rm(dist, { recursive: true, force: true });
      return;
    }
    await Promise.all(createdFiles.map((path) => rm(path, { force: true })));
  });
}

function mockedChecksumScript() {
  // sha256sum -c consumes stdin. An early-exiting replacement can close the
  // pipe before echo writes, aborting the deploy fixture with SIGPIPE (141).
  return "#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n";
}

test("deployment checksum mock consumes all piped input under pipefail", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-deploy-checksum-mock-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const checksum = join(root, "sha256sum");
  await executable(checksum, mockedChecksumScript());
  const bash = bashExecutable();
  // Exceed pipe capacity so an early-exiting consumer fails deterministically.
  // The inner Bash runs the mock script; -c/--status are its positional
  // arguments, matching the real sha256sum invocation (not Bash options).
  const result = spawnSync(bash, [
    "-c", 'set -euo pipefail\n{ for ((i=0;i<4096;i++)); do printf "%01024d\\n" 0; done; } | bash "$1" -c --status',
    "checksum-fixture", checksum.replaceAll("\\", "/"),
  ], {
    encoding: "utf8", timeout: 20_000,
  });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

function mockedSshScript(log) {
  return `#!/usr/bin/env bash
cat >/dev/null
command="$*"
# Inspect the remote positional arguments after bash -s --, not stdout's fake backup.
while (( $# > 0 )) && [[ "$1" != "--" ]]; do shift; done
if (( $# > 0 )); then shift; fi
if [[ "$command" == *"cat > '/tmp/pinvou-remote-code-"* ]]; then
  echo upload_code >> "${log}"
elif [[ "$command" == *"cat > '/tmp/pinvou-remote-web-"* ]]; then
  echo upload_web >> "${log}"
elif [[ "$command" == *"bash -s --"* && $# -eq 3 && "$3" == "$1/backups/"* ]]; then
  echo rollback >> "${log}"
  echo '已恢复备份'
elif [[ "$command" == *"bash -s --"* && $# -eq 12 && ( "\${12}" == "0" || "\${12}" == "1" ) ]]; then
  echo deploy >> "${log}"
  echo 'backup=/opt/pinvou-remote-relay/backups/fake'
else
  echo 'unexpected mocked SSH command' >&2
  exit 97
fi
`;
}

test("deployment SSH mock recognizes timestamped rollback and rejects unknown calls", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-deploy-ssh-mock-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const log = join(root, "calls.log");
  const shellLog = process.platform === "win32"
    ? log.replaceAll("\\\\", "/").replace(/^([A-Za-z]):/, (_, drive) => `/${drive.toLowerCase()}`)
    : log;
  const bash = bashExecutable();
  const remoteDir = "/opt/fixture-relay";
  const remoteCommand = (...args) => spawnSync(bash, [
    "-c", mockedSshScript(shellLog), "mock-ssh", "-i", "fixture-key",
    "-o", "BatchMode=yes", "fixture.invalid", "bash", "-s", "--", ...args,
  ], { input: "", encoding: "utf8", timeout: 10_000 });
  for (const confirmed of ["0", "1"]) {
    const deployed = remoteCommand(remoteDir, "fixture.service", "20261001-123456", ...Array(8).fill("/tmp/fixture"), confirmed);
    assert.equal(deployed.status, 0, deployed.stderr);
  }
  for (const stamp of ["20261001-123456", "20261102-234567"]) {
    const rolledBack = remoteCommand(remoteDir, "fixture.service", `${remoteDir}/backups/${stamp}`);
    assert.equal(rolledBack.status, 0, rolledBack.stderr);
  }
  for (const args of [
    [remoteDir, "fixture.service", "20261001-123456", ...Array(8).fill("/tmp/fixture")],
    [remoteDir, "fixture.service", "20261001-123456", ...Array(8).fill("/tmp/fixture"), "unexpected"],
    [remoteDir, "fixture.service", "20261001-123456", ...Array(8).fill("/tmp/fixture"), "1", "unexpected"],
    [remoteDir, "fixture.service", "/opt/other-relay/backups/20261001-123456"],
    [remoteDir, "fixture.service", `${remoteDir}/backups/20261001-123456`, "unexpected"],
    ["unknown"],
  ]) {
    const rejected = remoteCommand(...args);
    assert.equal(rejected.status, 97, rejected.stdout + rejected.stderr);
    assert.match(rejected.stderr, /unexpected mocked SSH command/);
  }
  assert.deepEqual((await readFile(log, "utf8")).trim().split(/\r?\n/), ["deploy", "deploy", "rollback", "rollback"]);
});

test("deploy script rolls back when post-deploy public verification fails", {
  skip: process.platform === "win32" ? "deployment script targets a POSIX host" : false,
}, async (t) => {
  await ensureMinimalWebDist(t);
  const root = await mkdtemp(join(tmpdir(), "pinvou-relay-deploy-test-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const bin = join(root, "bin");
  const log = join(root, "calls.log");
  await mkdir(bin);
  const hardening = join(root, "operator-hardening.conf");
  await writeFile(hardening, "TEST_ONLY_OFFLINE_SSH_MOCK_INPUT");

  await executable(join(bin, "node"), "#!/usr/bin/env bash\nexit 0\n");
  await executable(join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  await executable(join(bin, "sha256sum"), mockedChecksumScript());
  await executable(join(bin, "scp"), `#!/usr/bin/env bash\necho scp >> "${log}"\n`);
  await executable(join(bin, "ssh"), mockedSshScript(log));
  await executable(join(bin, "curl"), `#!/usr/bin/env bash
output=""
previous=""
for arg in "$@"; do
  if [[ "$previous" == "-o" ]]; then output="$arg"; fi
  previous="$arg"
  url="$arg"
done
if [[ -n "$output" ]]; then
  : > "$output"
  exit 0
fi
case "$url" in
  http://direct.invalid/*) exit 22 ;;
  */healthz) printf '%s' '{"ok":true,"room_count":0}' ;;
  */r/deploy-check) printf '%s' '<!doctype html><title>PINVOU Remote</title>' ;;
  *) exit 22 ;;
esac
`);

  const result = spawnSync("bash", [deployScript], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      XDG_CACHE_HOME: join(root, "cache"),
      PINVOU_REMOTE_PUBLIC_URL: "https://public.invalid/pinvou3/remote",
      PINVOU_REMOTE_DEPLOY_SERVER: "fixture@relay.invalid",
      PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER: "fixture@relay.invalid",
      SSH_KEY: "fixture-key",
      PINVOU_REMOTE_DIRECT_URL: "http://direct.invalid/pinvou3/remote",
      PINVOU_CONFIRM_PRODUCTION_DEPLOY: "1",
      PINVOU_REMOTE_HARDENING_FILE: hardening,
      SKIP_WEB_BUILD: "1",
      SKIP_LOCAL_TESTS: "1",
    },
    encoding: "utf8",
    timeout: 20_000,
  });
  const output = `${result.stdout}\n${result.stderr}`;
  assert.equal(result.status, 1, output);
  assert.match(output, /部署后检查失败，开始回滚/);
  assert.match(output, /部署失败，已恢复并验证上一线上版本/);
  const calls = await readFile(log, "utf8");
  assert.match(calls, /deploy/);
  assert.match(calls, /rollback/);
  assert.deepEqual(
    calls.split(/\r?\n/).filter((entry) => ["upload_code", "upload_web", "deploy", "rollback"].includes(entry)),
    ["upload_code", "upload_web", "deploy", "rollback"],
  );
});
