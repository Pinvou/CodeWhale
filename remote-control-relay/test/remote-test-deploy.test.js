import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync, readdirSync, existsSync, mkdtempSync, mkdirSync, copyFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repoRoot = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const deployScript = join(repoRoot, "scripts", "deploy-remote-relay-test.sh");
const posixOnly = {
  skip: process.platform === "win32" ? "deployment script targets a POSIX host" : false,
};

function validate(overrides = {}) {
  return spawnSync("bash", [deployScript, "--validate-only"], {
    cwd: repoRoot,
    env: { ...process.env, ...overrides },
    encoding: "utf8",
  });
}

test("remote-test deploy validation accepts the registered safe layout", posixOnly, () => {
  const syntax = spawnSync("bash", ["-n", deployScript], { encoding: "utf8" });
  assert.equal(syntax.status, 0, syntax.stderr);
  const result = validate();
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /配置校验通过/);
});

test("remote-test deploy builds and uploads the shared WebUI dist", () => {
  const source = readFileSync(deployScript, "utf8");
  assert.match(source, /PINVOU_REMOTE_PUBLIC_BASE_PATH="\$BASE_PATH" npm run build:web/);
  assert.match(source, /grep -Fq "\$BASE_PATH\/" "\$RELAY_DIR\/web\/dist\/index\.html"/);
  assert.match(source, /test -f "\$RELAY_DIR\/web\/dist\/platform\/tauri\/bridge\.js"/);
  assert.match(source, /package-lock\.json web\/dist web\/stats\.html/);
  assert.match(source, /relay_js_files="\$\(cd "\$RELAY_DIR" && ls \*\.js\)"/);
  assert.match(source, /\$relay_js_files package\.json package-lock\.json web\/dist web\/stats\.html/);
  assert.match(source, /docs\/schemas\/behavior-telemetry-v1\.schema\.json/);
  assert.match(source, /docs\/schemas\/behavior-telemetry-registries\.json/);
  assert.match(source, /await import\('\.\/telemetry-service\.js'\)/);
});

test("remote-test archive uses the script's actual complete runtime module selection", (t) => {
  const source = readFileSync(deployScript, "utf8");
  const selection = source.match(/^relay_js_files="[^\n]+"$/m);
  const archive = source.match(/^tar -czf - -C "\$RELAY_DIR"[^]*?\| relay_ssh /m);
  assert.ok(selection, "runtime module selection must be explicit");
  assert.ok(archive, "archive command must precede the mocked upload boundary");
  const bash = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "bash";
  assert.ok(process.platform !== "win32" || existsSync(bash), "Git Bash is required for archive verification");
  const shellPath = (value) => process.platform === "win32"
    ? value.replaceAll("\\", "/").replace(/^([A-Za-z]):/, (_, drive) => `/${drive.toLowerCase()}`)
    : value;
  const relayDir = join(repoRoot, "remote-control-relay");
  const fixtureRoot = mkdtempSync(join(tmpdir(), "pinvou-test-archive-"));
  t.after(() => rmSync(fixtureRoot, { recursive: true, force: true }));
  const fixtureRelay = join(fixtureRoot, "remote-control-relay");
  mkdirSync(join(fixtureRelay, "web", "dist"), { recursive: true });
  for (const file of [...readdirSync(relayDir).filter((entry) => entry.endsWith(".js")), "package.json", "package-lock.json", "web/stats.html"]) {
    copyFileSync(join(relayDir, file), join(fixtureRelay, file));
  }
  mkdirSync(join(fixtureRoot, "docs", "schemas"), { recursive: true });
  for (const file of ["behavior-telemetry-v1.schema.json", "behavior-telemetry-registries.json"]) {
    copyFileSync(join(repoRoot, "docs", "schemas", file), join(fixtureRoot, "docs", "schemas", file));
  }
  // Execute only archive creation, never the deployment or its upload command.
  const script = 'set -euo pipefail\nROOT="$1"\nRELAY_DIR="$2"\n'
    + selection[0] + "\n" + archive[0].replace(/\| relay_ssh $/, "");
  const packed = spawnSync(bash, ["-c", script, "archive-test", shellPath(fixtureRoot), shellPath(fixtureRelay)], {
    maxBuffer: 16 * 1024 * 1024,
  });
  assert.equal(packed.status, 0, packed.stderr?.toString());
  const listed = spawnSync("tar", ["-tzf", "-"], { input: packed.stdout, encoding: "utf8" });
  assert.equal(listed.status, 0, listed.stderr);
  const files = new Set(listed.stdout.split(/\r?\n/).map((entry) => entry.replace(/^\.\//, "")));
  for (const file of readdirSync(relayDir).filter((entry) => entry.endsWith(".js"))) {
    assert.ok(files.has(file), `runtime module missing from actual archive: ${file}`);
  }
  for (const file of ["behavior-telemetry-schema.js", "docs/schemas/behavior-telemetry-v1.schema.json", "docs/schemas/behavior-telemetry-registries.json"]) {
    assert.ok(files.has(file), `schema dependency missing from actual archive: ${file}`);
  }
});

test("remote-test tunnel account is restricted to the registered remote forward", () => {
  const source = readFileSync(deployScript, "utf8");
  assert.match(source, /Match User %s/);
  assert.match(source, /AllowTcpForwarding remote/);
  assert.match(source, /PermitListen 127\.0\.0\.1:%s/);
  assert.match(source, /BEGIN PINVOU3 REMOTE TEST TUNNEL/);
  assert.match(source, /sshd -t/);
});

test("remote-test deploy validation rejects TEST_DIR traversal and aliases", posixOnly, () => {
  for (const testDir of [
    "/opt/pinvou-remote-relay-test/..",
    "/opt/pinvou-remote-relay-test/sub/../..",
    "/opt/pinvou-remote-relay-test-child",
    "/opt/pinvou-remote-relay-test/subdir",
  ]) {
    const result = validate({ TEST_DIR: testDir });
    assert.notEqual(result.status, 0, `must reject TEST_DIR=${testDir}`);
    assert.match(result.stderr, /TEST_DIR=.*必须精确等于/);
  }
});

test("remote-test deploy validation rejects BASE_PATH dot segments", posixOnly, () => {
  for (const basePath of [
    "/pinvou3/remote-test/../remote",
    "/pinvou3/remote-test/./child",
  ]) {
    const result = validate({
      BASE_PATH: basePath,
      PUBLIC_URL: `https://pinvou.com${basePath}`,
    });
    assert.notEqual(result.status, 0, `must reject BASE_PATH=${basePath}`);
    assert.match(result.stderr, /不允许包含 \. 或 \.\./);
  }
});
