import assert from "node:assert/strict";
import { access, chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repoRoot = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const deployScript = join(repoRoot, "scripts", "deploy-remote-relay.sh");
const productionHardening = join(repoRoot, "remote-control-relay", "10-hardening.conf");

test("production hardening keeps deployment-specific proxy addresses out of the repository", async () => {
  const source = await readFile(productionHardening, "utf8");
  assert.doesNotMatch(source, /PINVOU_REMOTE_(?:ALLOWED|TRUSTED)_PROXY_IPS=/);
  const preflight = await readFile(join(repoRoot, "remote-control-relay", "scripts", "deploy-controller-preflight.mjs"), "utf8");
  assert.match(preflight, /"PINVOU_REMOTE_ALLOWED_PROXY_IPS"/);
  assert.match(preflight, /"PINVOU_REMOTE_TRUSTED_PROXY_IPS"/);
  assert.match(preflight, /controller_env_proxy_ips_mismatch/);
});

test("production deploy builds and replaces the complete shared WebUI dist", async () => {
  const source = await readFile(deployScript, "utf8");
  assert.match(source, /PINVOU_REMOTE_PUBLIC_BASE_PATH="\$BASE_PATH" npm run build:web/);
  assert.match(source, /tar -czf - -C "\$RELAY_DIR\/web\/dist" \./);
  assert.match(source, /behavior-telemetry-schema\.js/);
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
  assert.match(source, /if curl -fsS http:\/\/127\.0\.0\.1:8787\/pinvou3\/remote\/healthz >\/dev\/null 2>&1/);
  assert.match(source, /controller-v1 路由烟测失败/);
  assert.equal((source.match(/backup\/code-js\/\*\.js/g) || []).length, 2);
  assert.doesNotMatch(source, /PINVOU_ALLOW_LEGACY_REMOTE_DEPLOY/);
  assert.doesNotMatch(source, /nginx -t|systemctl reload nginx/);
});

test("production relay code artifact supports a cold ESM load", async (t) => {
  const relayDir = join(repoRoot, "remote-control-relay");
  const root = await mkdtemp(join(relayDir, ".deploy-cold-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const stage = join(root, "stage");
  await mkdir(stage);
  // Keep the archive on stdio and every tar path operand relative to `cwd`.
  // GNU tar treats a Windows drive-letter archive (for example D:\\...) as a
  // remote-shell target, and directly spawned MSYS tar does not translate an
  // absolute `-C` operand. This remains a real compress/extract cold load and
  // works consistently with GNU tar and the Windows BSD tar implementation.
  const packed = spawnSync("tar", [
    "-czf", "-",
    "-C", "remote-control-relay",
    "server.js", "telemetry-service.js", "behavior-telemetry-schema.js", "controller-telemetry.js",
    "-C", "..",
    "docs/schemas/behavior-telemetry-v1.schema.json",
    "docs/schemas/behavior-telemetry-registries.json",
  ], { cwd: repoRoot });
  assert.equal(packed.status, 0, packed.stderr?.toString("utf8"));
  const extracted = spawnSync("tar", ["-xzf", "-"], {
    cwd: stage,
    input: packed.stdout,
    encoding: "utf8",
  });
  assert.equal(extracted.status, 0, extracted.stderr);
  const loaded = spawnSync(process.execPath, [
    "--input-type=module",
    "-e",
    "await import('./telemetry-service.js')",
  ], { cwd: stage, encoding: "utf8" });
  assert.equal(loaded.status, 0, loaded.stderr);
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
  const bridge = join(dist, "platform", "tauri", "bridge.js");
  const distExisted = await pathExists(dist);
  const createdFiles = [];

  await mkdir(dist, { recursive: true });
  if (!(await pathExists(index))) {
    await writeFile(index, [
      "<!doctype html>",
      '<base href="/pinvou3/remote/">',
      '<script type="module" src="/pinvou3/remote/assets/app.js"></script>',
    ].join("\n"));
    createdFiles.push(index);
  }
  if (!(await pathExists(bridge))) {
    await mkdir(dirname(bridge), { recursive: true });
    await writeFile(bridge, "");
    createdFiles.push(bridge);
  }

  t.after(async () => {
    if (!distExisted) {
      await rm(dist, { recursive: true, force: true });
      return;
    }
    await Promise.all(createdFiles.map((path) => rm(path, { force: true })));
  });
}

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
elif [[ "$command" == *"bash -s --"* && $# -eq 11 ]]; then
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
  const bash = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "bash";
  const remoteDir = "/opt/fixture-relay";
  const remoteCommand = (...args) => spawnSync(bash, [
    "-c", mockedSshScript(shellLog), "mock-ssh", "-i", "fixture-key",
    "-o", "BatchMode=yes", "fixture.invalid", "bash", "-s", "--", ...args,
  ], { input: "", encoding: "utf8" });
  const deployed = remoteCommand(remoteDir, "fixture.service", "20261001-123456", ...Array(8).fill("/tmp/fixture"));
  assert.equal(deployed.status, 0, deployed.stderr);
  for (const stamp of ["20261001-123456", "20261102-234567"]) {
    const rolledBack = remoteCommand(remoteDir, "fixture.service", `${remoteDir}/backups/${stamp}`);
    assert.equal(rolledBack.status, 0, rolledBack.stderr);
  }
  for (const args of [
    [remoteDir, "fixture.service", "/opt/other-relay/backups/20261001-123456"],
    [remoteDir, "fixture.service", `${remoteDir}/backups/20261001-123456`, "unexpected"],
    ["unknown"],
  ]) {
    const rejected = remoteCommand(...args);
    assert.equal(rejected.status, 97, rejected.stdout + rejected.stderr);
    assert.match(rejected.stderr, /unexpected mocked SSH command/);
  }
  assert.deepEqual((await readFile(log, "utf8")).trim().split(/\r?\n/), ["deploy", "rollback", "rollback"]);
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

  await executable(join(bin, "node"), "#!/usr/bin/env bash\nexit 0\n");
  await executable(join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  await executable(join(bin, "sha256sum"), "#!/usr/bin/env bash\nexit 0\n");
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
      PINVOU_REMOTE_DIRECT_URL: "http://direct.invalid/pinvou3/remote",
      PINVOU_CONFIRM_PRODUCTION_DEPLOY: "1",
      SKIP_WEB_BUILD: "1",
      SKIP_LOCAL_TESTS: "1",
    },
    encoding: "utf8",
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
