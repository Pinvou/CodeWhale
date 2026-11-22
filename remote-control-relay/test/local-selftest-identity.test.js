import assert from "node:assert/strict";
import { generateKeyPairSync, createHash } from "node:crypto";
import { readFileSync, writeFileSync, mkdtempSync, mkdirSync, rmSync, statSync } from "node:fs";
import { spawnSyncWithRetry } from "./relay-tools-spawn.mjs";
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

const signerDetailStages = ["read_begin", "read_done", "validator_call", "stdin_begin", "stdin_done", "key_parsed", "private_key_created", "validator_accepted", "validator_rejected", "validator_return", "receiver_done"];
function lastSignerDetail(value) {
  if (typeof value !== "string") return "unknown";
  const tail = value.slice(-8192).split(/\r?\n/u);
  if (value.length > 8192) tail.shift();
  let stage = "unknown";
  for (const line of tail) if (signerDetailStages.includes(line)) stage = line;
  return stage;
}
function signerDetailFile(path) {
  if (!path) return "";
  try { const size = statSync(path).size; return size >= 0 && size <= 8192 ? readFileSync(path, "utf8") : ""; } catch { return ""; }
}
function insertSignerProbes(source, probes) {
  let result = source;
  for (const [anchor, before, after] of probes) {
    assert.equal(result.split(anchor).length, 2, "diagnostic anchor must be unique");
    result = result.replace(anchor, before + anchor + after);
  }
  let inverse = result;
  for (const [anchor, before, after] of [...probes].reverse()) inverse = inverse.replace(before + anchor + after, anchor);
  assert.equal(inverse, source, "removing probes must restore the whole actual source");
  return result;
}
function observedSigningValidator(source) {
  const observe = `\nconst observeSigner = (phase) => { try { fs.appendFileSync(process.env.PINVOU_TEST_SIGNER_PHASE_FILE, phase + "\\n"); } catch { /* Diagnostic I/O cannot replace validation. */ } };\n`;
  return insertSignerProbes(source, [
    ['const crypto = require("node:crypto");', "", observe],
    ['  const input = fs.readFileSync(0, "utf8");', '  observeSigner("stdin_begin");\n', '\n  observeSigner("stdin_done");'],
    ['  const key = JSON.parse(input.replace(/^\\uFEFF/, ""));', "", '\n  observeSigner("key_parsed");'],
    ['  const privateKey = crypto.createPrivateKey({ key: privateBytes, type: "pkcs8", format: "der" });', "", '\n  observeSigner("private_key_created");'],
    ["  process.stdout.write('{\"ok\":true}\\n');", '  observeSigner("validator_accepted");\n', ""],
    ['  process.stderr.write("selftest_signing_identity_invalid\\n");', '  observeSigner("validator_rejected");\n', ""],
  ]);
}
function signerPhaseCommands() {
  return [
    "function Write-SignerDetail([string]$phase) { try { [IO.File]::AppendAllText($env:PINVOU_TEST_SIGNER_PHASE_FILE,$phase+[Environment]::NewLine) } catch { } }",
    "function Expand-SignerDetail([string]$definition) {",
    " $original=$definition",
    " $needle='        $raw = Get-Content -LiteralPath $keyFile -Raw -Encoding UTF8'",
    " $replacement='        Write-SignerDetail ''read_begin''\n        $raw = Get-Content -LiteralPath $keyFile -Raw -Encoding UTF8\n        Write-SignerDetail ''read_done'''",
    " if($definition.Split([string[]]@($needle),[StringSplitOptions]::None).Length -ne 2){throw 'diagnostic_anchor_mismatch'}",
    " $definition=$definition.Replace($needle,$replacement)",
    " $needle='        $raw | & $NodeExe $validator | Out-Null'",
    " $replacement='        Write-SignerDetail ''validator_call''\n        $raw | & $NodeExe $validator | Out-Null\n        Write-SignerDetail ''validator_return'''",
    " if($definition.Split([string[]]@($needle),[StringSplitOptions]::None).Length -ne 2){throw 'diagnostic_anchor_mismatch'}",
    " $definition=$definition.Replace($needle,$replacement)",
    " $inverse=$definition",
    " $inverse=$inverse.Replace('        Write-SignerDetail ''validator_call''\n        $raw | & $NodeExe $validator | Out-Null\n        Write-SignerDetail ''validator_return''','        $raw | & $NodeExe $validator | Out-Null')",
    " $inverse=$inverse.Replace('        Write-SignerDetail ''read_begin''\n        $raw = Get-Content -LiteralPath $keyFile -Raw -Encoding UTF8\n        Write-SignerDetail ''read_done''','        $raw = Get-Content -LiteralPath $keyFile -Raw -Encoding UTF8')",
    " if($inverse -cne $original){throw 'diagnostic_inverse_mismatch'}",
    " return $definition",
    "}"
];
}

function powerShellDiagnostic(result, kind, elapsedMs, detail = "") {
  const cases = ["missing-id", "missing-private", "missing-public", "wrong-id", "bad-pkcs8", "noncanonical", "wrong-type", "mismatched-public", "fresh-reload"];
  const errors = ["ETIMEDOUT", "ENOENT", "EACCES", "EPERM", "EINVAL", "ENOBUFS", "ENOMEM", "EAGAIN"];
  const signals = ["SIGTERM", "SIGKILL", "SIGINT", "SIGABRT", "SIGSEGV", "SIGBREAK"];
  const stderr = typeof result.stderr === "string" ? result.stderr : "";
  // Inspect a bounded tail, excluding a potentially truncated first line.
  const lines = stderr.slice(-8192).split(/\r?\n/u);
  if (stderr.length > 8192) lines.shift();
  let stage = "unknown";
  for (const line of lines) {
    const match = /^pinvou_signer_stage:(parse|ready|signer|writer|reload|rejected|complete)$/u.exec(line);
    if (match) stage = match[1];
  }
  return JSON.stringify({
    case: cases.includes(kind) ? kind : "unknown",
    status: Number.isInteger(result.status) && result.status >= -0x80000000 && result.status <= 0xffffffff ? result.status : null,
    spawn_error: result.error == null ? "none" : errors.includes(result.error.code) ? result.error.code : "unknown",
    signal: result.signal == null ? "none" : signals.includes(result.signal) ? result.signal : "unknown",
    last_stage: stage,
    last_detail: lastSignerDetail(detail),
    elapsed_ms: Number.isFinite(elapsedMs) && elapsedMs >= 0 && elapsedMs <= 60_000 ? Math.round(elapsedMs) : "unknown",
  });
}

function assertPowerShellOutcome(result, expected, kind, elapsedMs, phaseFile) {
  const diagnostic = powerShellDiagnostic(result, kind, elapsedMs, signerDetailFile(phaseFile));
  // Boolean assertions cannot accidentally render an unrecognized raw value.
  assert.ok(result.status === 0 && result.error == null && result.signal == null, `signer_process_failed ${diagnostic}`);
  let receipt;
  try { receipt = JSON.parse(result.stdout.trim()); } catch { /* Report only the closed diagnostic below. */ }
  const keys = Object.keys(expected);
  assert.ok(
    receipt != null && typeof receipt === "object" && !Array.isArray(receipt) &&
      Object.keys(receipt).length === keys.length &&
      keys.every((key) => Object.hasOwn(receipt, key) && receipt[key] === expected[key]),
    `signer_result_invalid ${diagnostic}`,
  );
}

test("signer detail parser admits only bounded complete closed phase lines", () => {
  const secret = "TEST_ONLY_UNRECOGNIZED_PRIVATE_VALUE";
  assert.equal(lastSignerDetail(secret), "unknown");
  assert.equal(lastSignerDetail("read_begin\nstdin_done\n" + secret), "stdin_done");
  assert.equal(lastSignerDetail("validator_call extra\n" + secret), "unknown");
  assert.equal(lastSignerDetail("read_begin\n" + "x".repeat(8192)), "unknown");
  assert.equal(signerDetailFile(""), "");
  const root = mkdtempSync(join(tmpdir(), "pinvou-signer-diagnostic-parser-"));
  try {
    const missing = join(root, "missing");
    assert.equal(signerDetailFile(missing), "");
    assert.equal(signerDetailFile(root), "");
    const oversized = join(root, "large");
    writeFileSync(oversized, "x".repeat(8193));
    assert.equal(signerDetailFile(oversized), "");
    const exact = join(root, "small");
    writeFileSync(exact, "validator_call\nTEST_ONLY_UNRECOGNIZED_PRIVATE_VALUE");
    assert.equal(lastSignerDetail(signerDetailFile(exact)), "validator_call");
    let original;
    try { assertPowerShellOutcome({ status: null, error: { code: "ETIMEDOUT" }, signal: "SIGTERM", stdout: "", stderr: "" }, { refused: true }, "wrong-type", 20_000, missing); } catch (error) { original = error; }
    assert.ok(original instanceof assert.AssertionError);
    assert.match(original.message, /^signer_process_failed /);
    assert.equal(original.message.includes("TEST_ONLY_UNRECOGNIZED_PRIVATE_VALUE"), false);
  } finally { rmSync(root, { recursive: true, force: true }); }

  assert.throws(() => observedSigningValidator("missing anchors"), /anchor must be unique/);
});

test("PowerShell signer diagnostics retain closed observations without exposing child data", () => {
  const secret = "TEST_ONLY_PRIVATE_CHILD_DATA";
  const known = {
    status: null, error: { code: "ETIMEDOUT", message: secret, path: secret }, signal: "SIGTERM",
    stdout: secret, stderr: `${secret}\npinvou_signer_stage:signer\n${secret}`,
  };
  const observeFailure = (result, kind, elapsed) => {
    assert.throws(() => assertPowerShellOutcome(result, { refused: true }, kind, elapsed), (error) => {
      assert.equal(error.message.includes(secret), false);
      return true;
    });
  };
  observeFailure(known, "wrong-type", 20_001);
  assert.deepEqual(JSON.parse(powerShellDiagnostic(known, "wrong-type", 20_001)), {
    case: "wrong-type", status: null, spawn_error: "ETIMEDOUT", signal: "SIGTERM", last_stage: "signer", last_detail: "unknown", elapsed_ms: 20_001,
  });
  const unknown = { ...known, status: secret, error: { code: secret }, signal: secret, stderr: `pinvou_signer_stage:${secret}` };
  observeFailure(unknown, secret, Infinity);
  assert.deepEqual(JSON.parse(powerShellDiagnostic(unknown, secret, Infinity)), {
    case: "unknown", status: null, spawn_error: "unknown", signal: "unknown", last_stage: "unknown", last_detail: "unknown", elapsed_ms: "unknown",
  });
  for (const stdout of [secret, JSON.stringify({ refused: true, extra: secret })]) {
    observeFailure({ status: 0, signal: null, stdout, stderr: secret }, "wrong-type", 1);
  }
  assertPowerShellOutcome({ status: 0, signal: null, stdout: '{"refused":true}', stderr: "" }, { refused: true }, "wrong-type", 1);
});

test("actual local signing validator checks Ed25519 identity without sensitive output", () => {
  const valid = signingKey();
  const good = spawnSyncWithRetry(process.execPath, [validator], { input: JSON.stringify(valid), encoding: "utf8", timeout: 10_000 });
  assert.equal(good.status, 0);
  assert.equal(good.stdout, '{"ok":true}\n');
  assert.equal(good.stderr, "");
  for (const [kind, key] of invalidKeys(valid)) {
    const bad = spawnSyncWithRetry(process.execPath, [validator], { input: JSON.stringify(key), encoding: "utf8", timeout: 10_000 });
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
  const phaseFile = join(root, "signer-phase.txt");
  const observedRelay = join(root, "observed-relay");
  mkdirSync(join(observedRelay, "scripts"), { recursive: true });
  writeFileSync(join(observedRelay, "scripts/check-local-signing-key.cjs"), observedSigningValidator(readFileSync(validator, "utf8")));
  const keyFile = join(root, "relay-signing-key.json");
  const relayEnv = join(root, "relay.env.ps1");
  const clientEnv = join(root, "client.env.ps1");
  writeFileSync(relayEnv, "TEST_ONLY_ORIGINAL_RELAY_ENV");
  writeFileSync(clientEnv, "TEST_ONLY_ORIGINAL_CLIENT_ENV");
  const command = [
    "$ErrorActionPreference='Stop'",
    ...signerPhaseCommands(),
    "[Console]::Error.WriteLine('pinvou_signer_stage:parse')",
    "$tokens=$null;$errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'parse failed'}",
    "foreach($name in @('New-OrLoadSigningKey')){",
    " $defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq $name})",
    " if($defs.Count -ne 1){throw 'missing actual function'}",
    " $definition=$defs[0].Extent.Text;if($name -eq 'New-OrLoadSigningKey'){$definition=Expand-SignerDetail $definition}",
    " . ([scriptblock]::Create($definition))",
    "}",
    "[Console]::Error.WriteLine('pinvou_signer_stage:ready')",
    "$KeyStore=$env:PINVOU_TEST_SIGNING_ROOT;$relayDir=$env:PINVOU_TEST_RELAY_DIR;$NodeExe=$env:PINVOU_TEST_NODE",
    "$keyFile=Join-Path $KeyStore 'relay-signing-key.json';$stateDir=Join-Path $KeyStore 'relay-state';$Port=8787",
    // Only the signer may satisfy this refusal; the writer has an independent guard.
    "$refused=$false;[Console]::Error.WriteLine('pinvou_signer_stage:signer')",
    "try{$null=New-OrLoadSigningKey -stateDir $stateDir}catch{",
    " $reason=([string]$_.Exception.Message).Trim()",
    " if($reason -cnotin @('selftest_signing_identity_invalid','自测签名身份无效；拒绝覆盖既有环境文件。')){throw 'signer_refusal_not_observed'}",
    " $refused=$true;[Console]::Error.WriteLine('pinvou_signer_stage:rejected')",
    "}",
    "Write-SignerDetail 'receiver_done'",
    "@{refused=$refused}|ConvertTo-Json -Compress",
    "[Console]::Error.WriteLine('pinvou_signer_stage:complete')",
  ].join("\n");
  const digest = (path) => createHash("sha256").update(readFileSync(path)).digest("hex");
  for (const [kind, key] of invalidKeys(signingKey())) {
    writeFileSync(phaseFile, "");
    writeFileSync(keyFile, JSON.stringify(key));
    const before = [digest(keyFile), digest(relayEnv), digest(clientEnv)];
    const started = performance.now();
    const result = spawnSyncWithRetry("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
      env: {
        ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest, PINVOU_TEST_SIGNING_ROOT: root,
        PINVOU_TEST_RELAY_DIR: observedRelay, PINVOU_TEST_NODE: process.execPath, PINVOU_TEST_SIGNER_PHASE_FILE: phaseFile,
      }, encoding: "utf8", timeout: 20_000,
    });
    assertPowerShellOutcome(result, { refused: true }, kind, performance.now() - started, phaseFile);
    assert.deepEqual([digest(keyFile), digest(relayEnv), digest(clientEnv)], before, kind);
  }
});

test("actual PowerShell fresh signer and validated reload preserve complete identity", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, (t) => {
  const root = mkdtempSync(join(tmpdir(), "pinvou-signing-positive-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "relay-state"));
  const phaseFile = join(root, "signer-phase.txt");
  const observedRelay = join(root, "observed-relay");
  mkdirSync(join(observedRelay, "scripts"), { recursive: true });
  writeFileSync(join(observedRelay, "scripts/check-local-signing-key.cjs"), observedSigningValidator(readFileSync(validator, "utf8")));
  const command = [
    "$ErrorActionPreference='Stop'",
    ...signerPhaseCommands(),
    "[Console]::Error.WriteLine('pinvou_signer_stage:parse')",
    "$tokens=$null;$errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'parse failed'}",
    "foreach($name in @('New-OrLoadSigningKey','Write-EnvFiles')){",
    " $defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq $name})",
    " if($defs.Count -ne 1){throw 'missing actual function'}",
    " $definition=$defs[0].Extent.Text;if($name -eq 'New-OrLoadSigningKey'){$definition=Expand-SignerDetail $definition}",
    " . ([scriptblock]::Create($definition))",
    "}",
    "[Console]::Error.WriteLine('pinvou_signer_stage:ready')",
    "$KeyStore=$env:PINVOU_TEST_SIGNING_ROOT;$relayDir=$env:PINVOU_TEST_RELAY_DIR;$NodeExe=$env:PINVOU_TEST_NODE",
    "$keyFile=Join-Path $KeyStore 'relay-signing-key.json';$stateDir=Join-Path $KeyStore 'relay-state';$Port=8787",
    "[Console]::Error.WriteLine('pinvou_signer_stage:signer');$firstKey=New-OrLoadSigningKey -stateDir $stateDir",
    "[Console]::Error.WriteLine('pinvou_signer_stage:writer');$first=Write-EnvFiles -stateDir $stateDir -key $firstKey",
    "$envBefore=[IO.File]::ReadAllText($first.RelayEnvPath)",
    "$keyBefore=[IO.File]::ReadAllText($keyFile)",
    "[Console]::Error.WriteLine('pinvou_signer_stage:reload');$againKey=New-OrLoadSigningKey -stateDir $stateDir",
    "$again=Write-EnvFiles -stateDir $stateDir -key $againKey",
    "@{ok=($againKey.signing_key_id -eq 'selftest-relay-key-1');env_stable=($envBefore -ceq [IO.File]::ReadAllText($again.RelayEnvPath));key_stable=($keyBefore -ceq [IO.File]::ReadAllText($keyFile))}|ConvertTo-Json -Compress",
    "[Console]::Error.WriteLine('pinvou_signer_stage:complete')",
  ].join("\n");
  const started = performance.now();
  const result = spawnSyncWithRetry("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: {
      ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest, PINVOU_TEST_SIGNING_ROOT: root,
      PINVOU_TEST_RELAY_DIR: observedRelay, PINVOU_TEST_NODE: process.execPath, PINVOU_TEST_SIGNER_PHASE_FILE: phaseFile,
    }, encoding: "utf8", timeout: 20_000,
  });
  assertPowerShellOutcome(result, { ok: true, env_stable: true, key_stable: true }, "fresh-reload", performance.now() - started, phaseFile);
});
