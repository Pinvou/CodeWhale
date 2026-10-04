import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { FULL_FRONTEND_SMOKES } from "../../scripts/select-frontend-smokes.mjs";
import { commandFor } from "../../scripts/run-frontend-smokes.mjs";

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const testsRoot = path.join(appRoot, "tests");
const packageJson = JSON.parse(
  fs.readFileSync(path.join(appRoot, "package.json"), "utf8"),
);

const helperFiles = new Set([
  "behavior_telemetry_source.mjs",
  "bridge_domain_contract.mjs",
  "chromium-safe-port.mjs",
  "ui_test_server.js",
]);
// This gate deliberately runs its child smoke five times, not once.
const browserGateChildren = new Map([
  ["admin_token_account_detail_ui_gate.mjs", ["admin_token_account_detail_ui_smoke.mjs"]],
]);
// Requires isolated Go/Postgres/OIDC fixtures and five pre-issued launches;
// the Go integration suite owns setup, invocation, and cleanup.
const environmentIntegrationSmokes = new Map([
  ["admin_token_credential_origin_real_smoke.mjs", "../account-platform/internal/token/credential_origin_integration_test.go"],
]);
const platformRuntimeSmokes = new Set([
  "codex_acp_macos_runtime_smoke.js",
  "codex_acp_windows_runtime_smoke.js",
]);

const javascriptFiles = fs.readdirSync(testsRoot)
  .filter((name) => /\.(?:js|mjs)$/u.test(name))
  .sort();
const nodeTests = javascriptFiles.filter((name) => /\.test\.(?:js|mjs)$/u.test(name));
const browserSmokes = javascriptFiles.filter((name) => /(?:_|\.)smoke\.(?:js|mjs)$/u.test(name));

function collectScriptSmokeFiles(scriptName, collected, visiting = new Set()) {
  if (visiting.has(scriptName)) {
    throw new Error(`cyclic npm test script: ${scriptName}`);
  }
  const command = packageJson.scripts[scriptName];
  assert.equal(typeof command, "string", `missing npm script: ${scriptName}`);

  const nextVisiting = new Set(visiting).add(scriptName);
  for (const match of command.matchAll(/\bnpm run ([a-z0-9:-]+)/giu)) {
    collectScriptSmokeFiles(match[1], collected, nextVisiting);
  }
  for (const match of command.matchAll(/\bnode(?:\s+--[^\s&]+)*\s+([^\s&]+)/gu)) {
    const filename = path.basename(match[1]);
    if (/(?:_|\.)smoke\.(?:js|mjs)$/u.test(filename)) collected.add(filename);
    for (const child of browserGateChildren.get(filename) || []) collected.add(child);
  }
}

function fullSuiteSmokeFiles() {
  const collected = new Set();
  for (const item of FULL_FRONTEND_SMOKES) {
    if (item.kind === "node") {
      const filename = path.basename(item.target);
      if (/(?:_|\.)smoke\.(?:js|mjs)$/u.test(filename)) collected.add(filename);
    } else if (item.kind === "npm") {
      collectScriptSmokeFiles(item.target, collected);
    }
  }
  return collected;
}

test("every JavaScript test file belongs to one explicit layer", () => {
  const unclassified = javascriptFiles.filter((name) => (
    !nodeTests.includes(name)
    && !browserSmokes.includes(name)
    && !helperFiles.has(name)
    && !browserGateChildren.has(name)
  ));
  assert.deepEqual(
    unclassified,
    [],
    "rename deterministic tests to *.test.js/mjs, browser tests to *_smoke.js/mjs, or explicitly classify a helper",
  );
});

test("the default test command uses Node automatic discovery", () => {
  assert.equal(packageJson.scripts["test:node"], "node --test --test-concurrency=4");
  assert.equal(packageJson.scripts.test, "npm run test:node && npm run validate:pet-assets");
  assert.ok(nodeTests.length > 0);
});

test("the full browser suite covers every platform-independent smoke", () => {
  const covered = fullSuiteSmokeFiles();
  const missing = browserSmokes.filter((name) => (
    !platformRuntimeSmokes.has(name) && !environmentIntegrationSmokes.has(name) && !covered.has(name)
  ));
  assert.deepEqual(missing, [], "register new browser smokes in FULL_FRONTEND_SMOKES");
});

test("environment-owned smokes and repeated gates retain executable owners", () => {
  for (const [smoke, owner] of environmentIntegrationSmokes) {
    assert.ok(fs.readFileSync(path.resolve(appRoot, owner), "utf8").includes(smoke));
  }
  for (const [gate, children] of browserGateChildren) {
    const source = fs.readFileSync(path.join(testsRoot, gate), "utf8");
    for (const child of children) assert.ok(source.includes(child));
    assert.ok(Object.values(packageJson.scripts).some(command => command.includes(gate)));
  }
});

test("the full browser suite has unique commands and a portable runner", () => {
  const labels = FULL_FRONTEND_SMOKES.map(({ kind, target }) => `${kind}:${target}`);
  assert.equal(new Set(labels).size, labels.length);
  const npmCommand = commandFor({ kind: "npm", target: "test:bridge-smoke" });
  if (process.platform === "win32") {
    assert.equal(npmCommand.executable, process.execPath);
    assert.match(npmCommand.args[0], /npm-cli\.js$/u);
    assert.deepEqual(npmCommand.args.slice(1), ["run", "test:bridge-smoke"]);
  } else {
    assert.deepEqual(npmCommand, {
      executable: "npm",
      args: ["run", "test:bridge-smoke"],
    });
  }
  assert.deepEqual(commandFor({ kind: "node", target: "tests/example_smoke.js" }), {
    executable: process.execPath,
    args: ["tests/example_smoke.js"],
  });
  assert.throws(() => commandFor({ kind: "unknown", target: "x" }), /unsupported/u);
});

test("the full browser suite references existing files and scripts", () => {
  for (const item of FULL_FRONTEND_SMOKES) {
    if (item.kind === "node") {
      assert.ok(
        fs.existsSync(path.join(appRoot, item.target)),
        `missing smoke file: ${item.target}`,
      );
    } else if (item.kind === "npm") {
      assert.equal(
        typeof packageJson.scripts[item.target],
        "string",
        `missing npm script: ${item.target}`,
      );
    }
  }
});
