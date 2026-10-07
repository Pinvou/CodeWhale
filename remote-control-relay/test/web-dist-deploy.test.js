import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { verifyWebDist } from "../verify-web-dist.js";

const root = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const verifier = join(root, "remote-control-relay", "verify-web-dist.js");
const bash = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "bash";
const shellPath = (value) => process.platform === "win32"
  ? value.replaceAll("\\", "/").replace(/^([A-Za-z]):/, (_, drive) => `/${drive.toLowerCase()}`)
  : value;

test("actual local and staged deployment checks require current Web entry resources, not a bridge", (t) => {
  assert.ok(process.platform !== "win32" || existsSync(bash));
  const production = readFileSync(join(root, "scripts", "deploy-remote-relay.sh"), "utf8");
  const testing = readFileSync(join(root, "scripts", "deploy-remote-relay-test.sh"), "utf8");
  const local = /^node "\$RELAY_DIR\/verify-web-dist\.js" "\$RELAY_DIR\/web\/dist" "\$BASE_PATH"$/m;
  const staged = /^node "\$code_stage\/verify-web-dist\.js" "\$web_stage" "\/pinvou3\/remote"$/m;
  const prepare = production.match(/^cp "\$package_tmp" "\$code_stage\/package\.json"$/m)?.[0];
  assert.ok(prepare, "stage must use this upload's ESM manifest before verification");
  assert.ok(production.indexOf(prepare) < production.indexOf('node --check "$code_stage/server.js"'));
  const commands = [production.match(local)?.[0], testing.match(local)?.[0],
    production.match(staged) ? prepare + "\n" + production.match(staged)[0] : undefined];
  assert.ok(commands.every(Boolean), "execute all three actual script checks");
  assert.doesNotMatch(production + testing, /test -f [^\n]*platform\/tauri\/bridge\.js/);
  const fixtures = ["current", "missing-module", "missing-startup", "wrong-base", "missing-index", "bridge-only", "traversal"];
  for (const fixture of fixtures) {
    const directory = mkdtempSync(join(tmpdir(), "pinvou-web-dist-check-"));
    t.after(() => rmSync(directory, { recursive: true, force: true }));
    const relay = join(directory, "relay");
    const stage = join(directory, "code-stage");
    mkdirSync(stage);
    copyFileSync(verifier, join(stage, "verify-web-dist.js"));
    const uploadedPackage = join(directory, "package-upload.json");
    copyFileSync(join(root, "remote-control-relay", "package.json"), uploadedPackage);
    const dist = join(relay, "web", "dist");
    mkdirSync(join(dist, "assets"), { recursive: true });
    mkdirSync(join(dist, "startup"), { recursive: true });
    copyFileSync(verifier, join(relay, "verify-web-dist.js"));
    // Local checkout has its manifest; production stage receives it separately.
    copyFileSync(uploadedPackage, join(relay, "package.json"));
    assert.equal(existsSync(join(stage, "package.json")), false);
    const base = fixture === "wrong-base" ? "/other/" : "/pinvou3/remote/";
    let html = `<title>PINVOU 智能助手</title><base href="${base}">`
      + `<script src="${base}startup/fixture.js"></script>`
      + `<script type="module" src="${base}assets/fixture.js"></script>`;
    if (fixture === "bridge-only") html = `<title>PINVOU 智能助手</title><base href="${base}"><script src="${base}platform/tauri/bridge.js"></script>`;
    if (fixture === "traversal") {
      writeFileSync(join(dist, "..", "outside.js"), "/* outside artifact */");
      html += '<script src="../outside.js"></script>';
    }
    if (fixture !== "missing-index") writeFileSync(join(dist, "index.html"), html);
    if (fixture !== "missing-module") writeFileSync(join(dist, "assets", "fixture.js"), "/* isolated module */");
    if (fixture !== "missing-startup") writeFileSync(join(dist, "startup", "fixture.js"), "/* isolated startup */");
    if (fixture === "bridge-only") {
      mkdirSync(join(dist, "platform", "tauri"), { recursive: true });
      writeFileSync(join(dist, "platform", "tauri", "bridge.js"), "/* obsolete fixture */");
    } else assert.equal(existsSync(join(dist, "platform", "tauri", "bridge.js")), false);
    for (const command of commands) {
      const result = spawnSync(bash, ["-c", 'set -euo pipefail\n' + command], {
        env: { ...process.env, RELAY_DIR: shellPath(relay), code_stage: shellPath(stage),
          package_tmp: shellPath(uploadedPackage),
          web_stage: shellPath(dist), BASE_PATH: "/pinvou3/remote",
          // Git Bash must not rewrite the URL base as a Windows filesystem path.
          MSYS2_ARG_CONV_EXCL: "/pinvou3/remote" },
        encoding: "utf8", timeout: 10_000,
      });
      assert.equal(result.error, undefined, `${fixture}: ${result.error}`);
      assert.equal(result.status, fixture === "current" ? 0 : 1, `${fixture}: ${command}: ${result.stderr}`);
    }
    assert.equal(readFileSync(join(stage, "package.json"), "utf8"), readFileSync(uploadedPackage, "utf8"));
  }
});

test("Web entry itself cannot resolve outside dist", (t) => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-web-index-boundary-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const dist = join(directory, "dist");
  mkdirSync(dist);
  const outside = join(directory, "outside.html");
  writeFileSync(outside, '<title>PINVOU 智能助手</title><base href="/pinvou3/remote/">');
  try {
    symlinkSync(outside, join(dist, "index.html"), "file");
  } catch (error) {
    if (process.platform === "win32" && error.code === "EPERM") {
      t.skip("file symlink creation requires Windows Developer Mode or privilege");
      return;
    }
    throw error;
  }
  assert.throws(() => verifyWebDist(dist, "/pinvou3/remote"), /outside dist/);
});
