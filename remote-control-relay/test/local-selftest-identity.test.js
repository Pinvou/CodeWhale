import assert from "node:assert/strict";
import { generateKeyPairSync, createHash } from "node:crypto";
import { readFileSync, writeFileSync, mkdtempSync, mkdirSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const validator = fileURLToPath(new URL("../scripts/check-local-signing-key.cjs", import.meta.url));
const selftest = fileURLToPath(new URL("../../scripts/remote-control-local-selftest.ps1", import.meta.url));
const relayDir = fileURLToPath(new URL("../", import.meta.url));

function signingKey() {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  return {
    signing_key_id: "selftest-relay-key-1",
    private_key_pkcs8_der_b64: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
    public_key_raw_b64: publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64"),
  };
}
function invalidKeys(valid) {
  const ec = generateKeyPairSync("ec", { namedCurve: "prime256v1" });
  return [
    ["missing-id", { ...valid, signing_key_id: undefined }],
    ["missing-private", { ...valid, private_key_pkcs8_der_b64: undefined }],
    ["missing-public", { ...valid, public_key_raw_b64: undefined }],
    ["wrong-id", { ...valid, signing_key_id: "another-id" }],
    ["bad-pkcs8", { ...valid, private_key_pkcs8_der_b64: Buffer.from("TEST_ONLY_NOT_DER").toString("base64") }],
    ["noncanonical", { ...valid, public_key_raw_b64: valid.public_key_raw_b64 + " " }],
    ["wrong-type", { ...valid, private_key_pkcs8_der_b64: ec.privateKey.export({ type: "pkcs8", format: "der" }).toString("base64") }],
    ["mismatched-public", { ...valid, public_key_raw_b64: signingKey().public_key_raw_b64 }],
  ];
}

test("actual local signing validator checks Ed25519 identity without sensitive output", () => {
  const valid = signingKey();
  const good = spawnSync(process.execPath, [validator], { input: JSON.stringify(valid), encoding: "utf8", timeout: 10_000 });
  assert.equal(good.status, 0);
  assert.equal(good.stdout, '{"ok":true}\n');
  assert.equal(good.stderr, "");
  for (const [kind, key] of invalidKeys(valid)) {
    const bad = spawnSync(process.execPath, [validator], { input: JSON.stringify(key), encoding: "utf8", timeout: 10_000 });
    assert.equal(bad.status, 1, kind);
    assert.equal(bad.stdout, "", kind);
    assert.equal(bad.stderr, "selftest_signing_identity_invalid\n", kind);
  }
});

test("actual local signer rejects invalid stored material before touching either env file", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, (t) => {
  const root = mkdtempSync(join(tmpdir(), "pinvou-signing-preflight-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "relay-state"));
  const keyFile = join(root, "relay-signing-key.json");
  const relayEnv = join(root, "relay.env.ps1");
  const clientEnv = join(root, "client.env.ps1");
  writeFileSync(relayEnv, "TEST_ONLY_ORIGINAL_RELAY_ENV");
  writeFileSync(clientEnv, "TEST_ONLY_ORIGINAL_CLIENT_ENV");
  const command = [
    "$ErrorActionPreference='Stop'",
    "$tokens=$null;$errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'parse failed'}",
    "foreach($name in @('New-OrLoadSigningKey','Write-EnvFiles')){",
    " $defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq $name})",
    " if($defs.Count -ne 1){throw 'missing actual function'}",
    " . ([scriptblock]::Create($defs[0].Extent.Text))",
    "}",
    "$KeyStore=$env:PINVOU_TEST_SIGNING_ROOT;$relayDir=$env:PINVOU_TEST_RELAY_DIR;$NodeExe=$env:PINVOU_TEST_NODE",
    "$keyFile=Join-Path $KeyStore 'relay-signing-key.json';$stateDir=Join-Path $KeyStore 'relay-state';$Port=8787",
    "$refused=$false;try{$key=New-OrLoadSigningKey -stateDir $stateDir;$null=Write-EnvFiles -stateDir $stateDir -key $key}catch{$refused=$true}",
    "@{refused=$refused}|ConvertTo-Json -Compress",
  ].join("\n");
  const digest = (path) => createHash("sha256").update(readFileSync(path)).digest("hex");
  for (const [kind, key] of invalidKeys(signingKey())) {
    writeFileSync(keyFile, JSON.stringify(key));
    const before = [digest(keyFile), digest(relayEnv), digest(clientEnv)];
    const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
      env: {
        ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest, PINVOU_TEST_SIGNING_ROOT: root,
        PINVOU_TEST_RELAY_DIR: relayDir, PINVOU_TEST_NODE: process.execPath,
      }, encoding: "utf8", timeout: 20_000,
    });
    assert.equal(result.status, 0, kind);
    assert.deepEqual(JSON.parse(result.stdout.trim()), { refused: true }, kind);
    assert.deepEqual([digest(keyFile), digest(relayEnv), digest(clientEnv)], before, kind);
  }
});

test("actual PowerShell fresh signer and validated reload preserve complete identity", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, (t) => {
  const root = mkdtempSync(join(tmpdir(), "pinvou-signing-positive-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "relay-state"));
  const command = [
    "$ErrorActionPreference='Stop'",
    "$tokens=$null;$errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'parse failed'}",
    "foreach($name in @('New-OrLoadSigningKey','Write-EnvFiles')){",
    " $defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq $name})",
    " if($defs.Count -ne 1){throw 'missing actual function'}",
    " . ([scriptblock]::Create($defs[0].Extent.Text))",
    "}",
    "$KeyStore=$env:PINVOU_TEST_SIGNING_ROOT;$relayDir=$env:PINVOU_TEST_RELAY_DIR;$NodeExe=$env:PINVOU_TEST_NODE",
    "$keyFile=Join-Path $KeyStore 'relay-signing-key.json';$stateDir=Join-Path $KeyStore 'relay-state';$Port=8787",
    "$firstKey=New-OrLoadSigningKey -stateDir $stateDir",
    "$first=Write-EnvFiles -stateDir $stateDir -key $firstKey",
    "$envBefore=[IO.File]::ReadAllText($first.RelayEnvPath)",
    "$keyBefore=[IO.File]::ReadAllText($keyFile)",
    "$againKey=New-OrLoadSigningKey -stateDir $stateDir",
    "$again=Write-EnvFiles -stateDir $stateDir -key $againKey",
    "@{ok=($againKey.signing_key_id -eq 'selftest-relay-key-1');env_stable=($envBefore -ceq [IO.File]::ReadAllText($again.RelayEnvPath));key_stable=($keyBefore -ceq [IO.File]::ReadAllText($keyFile))}|ConvertTo-Json -Compress",
  ].join("\n");
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: {
      ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest, PINVOU_TEST_SIGNING_ROOT: root,
      PINVOU_TEST_RELAY_DIR: relayDir, PINVOU_TEST_NODE: process.execPath,
    }, encoding: "utf8", timeout: 20_000,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout.trim()), { ok: true, env_stable: true, key_stable: true });
});
