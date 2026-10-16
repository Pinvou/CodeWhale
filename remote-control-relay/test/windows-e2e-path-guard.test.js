import assert from "node:assert/strict";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";

const orchestrator = fileURLToPath(new URL("../../scripts/run-windows-remote-control-e2e.ps1", import.meta.url));
const processGuard = fileURLToPath(new URL("../../scripts/windows-e2e-process-guard.ps1", import.meta.url));

const loadProductionPathGuard = `$tokens=$null;$errors=$null;$ast=[System.Management.Automation.Language.Parser]::ParseFile('${orchestrator.replaceAll("'", "''")}',[ref]$tokens,[ref]$errors);if($errors.Count){throw 'orchestrator-parse-failed'};$definition=$ast.Find({param($node)$node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Assert-PinvouPathInside'},$true);if(-not $definition){throw 'production-path-guard-missing'};Invoke-Expression $definition.Extent.Text;`;

test("production orchestrator path guard uses component boundaries, not string prefixes", { skip: process.platform !== "win32" }, () => {
  const escaped = spawnSync("powershell", ["-NoProfile", "-NonInteractive", "-Command",
    `${loadProductionPathGuard} Assert-PinvouPathInside 'C:\\evidence' 'C:\\evidence-escape\\artifact.json' 'path_escape'`], { encoding: "utf8" });
  assert.notEqual(escaped.status, 0);
  assert.match(escaped.stderr, /path_escape/u);

  const traversal = spawnSync("powershell", ["-NoProfile", "-NonInteractive", "-Command",
    `${loadProductionPathGuard} Assert-PinvouPathInside 'C:\\evidence' 'C:\\evidence\\..\\outside.json' 'path_escape'`], { encoding: "utf8" });
  assert.notEqual(traversal.status, 0);
  assert.match(traversal.stderr, /path_escape/u);

  const allowed = spawnSync("powershell", ["-NoProfile", "-NonInteractive", "-Command",
    `${loadProductionPathGuard} Assert-PinvouPathInside 'C:\\evidence' 'C:\\evidence\\checks\\artifact.json' 'path_escape'`], { encoding: "utf8" });
  assert.equal(allowed.status, 0, allowed.stderr);
});

test("PowerShell process guard clears inherited secrets and rejects sensitive purpose keys", { skip: process.platform !== "win32" }, () => {
  const node = process.execPath.replaceAll("'", "''");
  const nodeSha256 = crypto.createHash("sha256").update(readFileSync(process.execPath)).digest("hex");
  const invocation = spawnSync("powershell", ["-NoProfile", "-NonInteractive", "-Command",
    `$env:PINVOU_SECRET_SENTINEL='must-not-leak'; . '${processGuard.replaceAll("'", "''")}'; $r=Invoke-PinvouWhitelistedProcess -FilePath '${node}' -ExpectedSha256 '${nodeSha256}' -Arguments @('-e','process.stdout.write(process.env.PINVOU_SECRET_SENTINEL||"absent")'); if($r.ExitCode -ne 0){exit 7}; [Console]::Out.Write($r.StdOut)`], { encoding: "utf8" });
  assert.equal(invocation.status, 0, invocation.stderr);
  assert.equal(invocation.stdout, "absent");
  const rejected = spawnSync("powershell", ["-NoProfile", "-NonInteractive", "-Command",
    `. '${processGuard.replaceAll("'", "''")}'; Invoke-PinvouWhitelistedProcess -FilePath '${node}' -ExpectedSha256 '${nodeSha256}' -Arguments @('--version') -PurposeEnvironment @{ API_TOKEN='forbidden' }`], { encoding: "utf8" });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /e2e_process_environment_sensitive_key_forbidden/u);
});
