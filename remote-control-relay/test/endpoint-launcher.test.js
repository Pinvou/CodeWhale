import assert from "node:assert/strict";
import { readFileSync, mkdtempSync, mkdirSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { loadEnrollmentRegistryConfig } from "../enrollment-registry.js";

const launcher = fileURLToPath(new URL("../../scripts/run-remote-control-endpoint.ps1", import.meta.url));
const source = readFileSync(launcher, "utf8");
const selftest = readFileSync(new URL("../../scripts/remote-control-local-selftest.ps1", import.meta.url), "utf8");

test("local selftest places legacy state outside the controller state directory", () => {
  assert.match(selftest, /\$controllerState = Join-Path \$stateDir 'controller-state\.json'/);
  assert.match(selftest, /\$legacyState = Join-Path \$KeyStore 'legacy-web-state\.json'/);
  const root = join(tmpdir(), "pinvou-selftest-contract-fixture");
  const env = {
    NODE_ENV: "development",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "true",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "fixture-hmac-id",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "test-only-hmac-key-material-32-bytes-minimum",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(root, "relay-state", "controller-state.json"),
    PINVOU_REMOTE_STATE_PATH: join(root, "legacy-web-state.json"),
  };
  assert.doesNotThrow(() => loadEnrollmentRegistryConfig(env));
  assert.throws(() => loadEnrollmentRegistryConfig({
    ...env, PINVOU_REMOTE_STATE_PATH: join(root, "relay-state", "legacy-web-state.json"),
  }), (error) => error?.code === "legacy_state_path_inside_controller_directory");
});

test("endpoint launcher documents persistent configuration and removes stale overrides", () => {
  assert.match(source, /Production 只清除进程覆盖，不改用户持久配置/);
  assert.match(source, /\$env:PINVOU_REMOTE_ENDPOINT_PROFILE = 'test'/);
  assert.match(source, /Remove-Item Env:PINVOU_REMOTE_ENDPOINT_PROFILE/);
  assert.equal((source.match(/Remove-Item Env:PINVOU_REMOTE_RUNTIME_RELAY_WS_URL/g) || []).length, 2);
});

test("actual selftest environment generator produces accepted non-nested state paths", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, (t) => {
  const root = mkdtempSync(join(tmpdir(), "pinvou-selftest-env-contract-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "relay-state"));
  const command = String.raw`
$ErrorActionPreference = 'Stop'
$tokens = $null; $errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw 'selftest parse failed' }
$functions = @($ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq 'Write-EnvFiles' })
if ($functions.Count -ne 1) { throw 'expected one environment generator' }
. ([scriptblock]::Create($functions[0].Extent.Text))
$KeyStore = $env:PINVOU_TEST_SELFTEST_ROOT; $Port = 8787
$stateDir = Join-Path $KeyStore 'relay-state'
$key = [pscustomobject]@{signing_key_id='TEST_ONLY_KEY_ID'; private_key_pkcs8_der_b64='TEST_ONLY_NOT_A_PRIVATE_KEY'; public_key_raw_b64='TEST_ONLY_NOT_A_PUBLIC_KEY'}
$generated = Write-EnvFiles -stateDir $stateDir -key $key
. $generated.RelayEnvPath
@{
 NODE_ENV = $env:NODE_ENV
 PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT = $env:PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT
 PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU = $env:PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU
 PINVOU_REMOTE_CONTROLLER_STATE_PATH = $env:PINVOU_REMOTE_CONTROLLER_STATE_PATH
 PINVOU_REMOTE_STATE_PATH = $env:PINVOU_REMOTE_STATE_PATH
 hmac_length = $env:PINVOU_REMOTE_ENROLLMENT_HMAC_KEY.Length
} | ConvertTo-Json -Compress
`;
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: {
      ...process.env,
      PINVOU_TEST_SELFTEST_PATH: fileURLToPath(new URL("../../scripts/remote-control-local-selftest.ps1", import.meta.url)),
      PINVOU_TEST_SELFTEST_ROOT: root,
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  const projected = JSON.parse(result.stdout.trim());
  assert.ok(projected.hmac_length >= 32);
  assert.equal(projected.PINVOU_REMOTE_CONTROLLER_STATE_PATH, join(root, "relay-state", "controller-state.json"));
  assert.equal(projected.PINVOU_REMOTE_STATE_PATH, join(root, "legacy-web-state.json"));
  assert.doesNotThrow(() => loadEnrollmentRegistryConfig({
    ...projected,
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "fixture-hmac-id",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "test-only-hmac-key-material-32-bytes-minimum",
  }));
});

test("actual launcher environment block switches Test to Production without IO", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, () => {
  // Parse the real script and execute ONLY its environment block. Never execute
  // health checks, executable discovery, exit statements or Start-Process.
  const command = String.raw`
$ErrorActionPreference = 'Stop'
$tokens = $null; $errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_LAUNCHER_PATH, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw 'launcher parse failed' }
$blocks = @($ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.IfStatementAst] -and $_.ElseClause -and $_.Clauses[0].Item1.Extent.Text -eq '$Mode -eq ''Test''' })
if ($blocks.Count -ne 1) { throw 'expected one environment block' }
$block = [scriptblock]::Create($blocks[0].Extent.Text)
$WsUrl = 'wss://fixture.invalid/test/ws'; $PublicUrl = 'https://fixture.invalid/test'
$SigningKeyId = 'fixture-public-key-id'; $SigningPublicKeyB64 = 'fixture-public-key'
$env:PINVOU_REMOTE_ENDPOINT_PROFILE = 'production'
$env:PINVOU_REMOTE_RUNTIME_RELAY_WS_URL = 'ws://fixture.invalid/stale'
$Mode = 'Test'; . $block *> $null
$names = @('PINVOU_REMOTE_ENDPOINT_PROFILE', 'PINVOU_REMOTE_RUNTIME_RELAY_WS_URL', 'PINVOU_REMOTE_PUBLIC_URL', 'PINVOU_REMOTE_RELAY_WS_URL', 'PINVOU_REMOTE_RELAY_SIGNING_KEY_ID', 'PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64')
$test = @{}; foreach ($name in $names) { $test[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$Mode = 'Production'; . $block *> $null
$production = @{}; foreach ($name in $names) { $production[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
@{test = $test; production = $production} | ConvertTo-Json -Compress
`;
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: { ...process.env, PINVOU_TEST_LAUNCHER_PATH: launcher },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  const projected = JSON.parse(result.stdout.trim());
  assert.equal(projected.test.PINVOU_REMOTE_ENDPOINT_PROFILE, "test");
  assert.equal(projected.test.PINVOU_REMOTE_RUNTIME_RELAY_WS_URL, null);
  assert.equal(projected.test.PINVOU_REMOTE_RELAY_WS_URL, "wss://fixture.invalid/test/ws");
  assert.equal(projected.test.PINVOU_REMOTE_PUBLIC_URL, "https://fixture.invalid/test");
  assert.equal(projected.test.PINVOU_REMOTE_RELAY_SIGNING_KEY_ID, "fixture-public-key-id");
  assert.equal(projected.test.PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64, "fixture-public-key");
  for (const value of Object.values(projected.production)) assert.equal(value, null);
});

test("actual selftest prepare preserves both HMACs and refuses lost or corrupt identity", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, (t) => {
  const root = mkdtempSync(join(tmpdir(), "pinvou-selftest-hmac-contract-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "relay-state"));
  const command = [
    "$ErrorActionPreference='Stop'",
    "$tokens=$null; $errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'parse failed'}",
    "$defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq 'Write-EnvFiles'})",
    "if($defs.Count -ne 1){throw 'expected actual env generator'}",
    ". ([scriptblock]::Create($defs[0].Extent.Text))",
    "$KeyStore=$env:PINVOU_TEST_SELFTEST_ROOT; $Port=8787; $stateDir=Join-Path $KeyStore 'relay-state'",
    "$key=[pscustomobject]@{signing_key_id='TEST_ONLY_ID';private_key_pkcs8_der_b64='TEST_ONLY_FAKE_PRIVATE_KEY';public_key_raw_b64='TEST_ONLY_FAKE_PUBLIC_KEY'}",
    "$first=Write-EnvFiles -stateDir $stateDir -key $key",
    "$before=Get-Content -Raw -Encoding UTF8 -LiteralPath $first.RelayEnvPath",
    "$again=Write-EnvFiles -stateDir $stateDir -key $key",
    "$same=$before -ceq (Get-Content -Raw -Encoding UTF8 -LiteralPath $again.RelayEnvPath)",
    "Set-Content -LiteralPath $first.RelayEnvPath -Value 'TEST_ONLY_CORRUPT_IDENTITY' -Encoding UTF8",
    "$invalid=$false; try{$null=Write-EnvFiles -stateDir $stateDir -key $key}catch{$invalid=$true}",
    "$unchanged=(Get-Content -Raw -Encoding UTF8 -LiteralPath $first.RelayEnvPath).Trim() -eq 'TEST_ONLY_CORRUPT_IDENTITY'",
    "Remove-Item -LiteralPath $first.RelayEnvPath",
    "Set-Content -LiteralPath (Join-Path $stateDir 'controller-state.json.bak') -Value 'TEST_ONLY_STATE' -Encoding ASCII",
    "$missing=$false; try{$null=Write-EnvFiles -stateDir $stateDir -key $key}catch{$missing=$true}",
    "@{same=$same;invalid_refused=$invalid;invalid_unchanged=$unchanged;missing_refused=$missing;no_new_env=(-not (Test-Path -LiteralPath $first.RelayEnvPath))}|ConvertTo-Json -Compress",
  ].join("\n");
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: { ...process.env, PINVOU_TEST_SELFTEST_PATH: fileURLToPath(new URL("../../scripts/remote-control-local-selftest.ps1", import.meta.url)), PINVOU_TEST_SELFTEST_ROOT: root },
    encoding: "utf8", timeout: 20_000,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout.trim()), {
    same: true, invalid_refused: true, invalid_unchanged: true, missing_refused: true, no_new_env: true,
  });
});
