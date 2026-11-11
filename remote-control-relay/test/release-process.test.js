import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import crypto from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { runReleaseProcess } from "../scripts/release-process.mjs";
import { buildReleaseSubprocessEnvironment } from "../scripts/controller-release-gate.mjs";

const guard = fileURLToPath(new URL("../../scripts/windows-e2e-process-guard.ps1", import.meta.url));
const orchestrator = fileURLToPath(new URL("../../scripts/run-windows-remote-control-e2e.ps1", import.meta.url));
const env = buildReleaseSubprocessEnvironment(process.env, "test");
const nodeHash = crypto.createHash("sha256").update(readFileSync(process.execPath)).digest("hex");
const quote = (text) => `'${text.replaceAll("'", "''")}'`;

test("real release runner drains both pipes beyond capacity without exposing diagnostics", async () => {
  await runReleaseProcess(process.execPath, ["-e", `
    const data=Buffer.alloc(2*1024*1024,120);
    process.stderr.write(data);process.stdout.write(data);
    process.stderr.write('PRIVATE_DIAGNOSTIC_SENTINEL');
  `], { env, timeoutMs: 15000 });
});

test("real release runner preserves nonzero exit while redacting stderr", async () => {
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    process.stderr.write('PRIVATE_DIAGNOSTIC_SENTINEL');process.exitCode=23;
  `], { env, timeoutMs: 15000 }), (error) => {
    assert.equal(error.code, "release_subgate_failed");
    assert.equal(error.exitCode, 23);
    assert.equal(error.cleanupConfirmed, true);
    assert.doesNotMatch(JSON.stringify(error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
    assert.equal(error.cause, undefined);
    return true;
  });
});

test("real release timeout retires a detached descendant within a finite budget", async () => {
  const began = Date.now();
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],
      {detached:true,stdio:'ignore'}).unref();
    setInterval(()=>{},1000);
  `], { env, timeoutMs: 1000 }), (error) => {
    assert.equal(error.code, "release_subgate_timeout");
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
  assert.ok(Date.now() - began < 18000);
});

test("real release cancellation is bounded and never becomes success", async () => {
  const controller = new AbortController();
  const running = runReleaseProcess(process.execPath, ["-e", "setInterval(()=>{},1000)"],
    { env, timeoutMs: 15000, signal: controller.signal });
  controller.abort();
  await assert.rejects(running, (error) => {
    assert.equal(error.code, "release_subgate_cancelled");
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
});

test("release runner pre-start cancellation and invalid budget launch nothing", async () => {
  await assert.rejects(runReleaseProcess("does-not-exist", [],
    { env, signal: AbortSignal.abort() }), { code: "release_subgate_cancelled" });
  await assert.rejects(runReleaseProcess("does-not-exist", [], { env, timeoutMs: 0 }),
    { code: "release_subgate_timeout_invalid" });
});

test("owned command retirement does not terminate an unrelated live sentinel", async () => {
  const sentinel = spawn(process.execPath, ["-e", "setInterval(()=>{},1000)"], { stdio: "ignore", windowsHide: true });
  try {
    await once(sentinel, "spawn");
    await assert.rejects(runReleaseProcess(process.execPath, ["-e", "setInterval(()=>{},1000)"],
      { env, timeoutMs: 500 }), { code: "release_subgate_timeout", cleanupConfirmed: true });
    assert.equal(sentinel.exitCode, null);
    assert.equal(sentinel.signalCode, null);
  } finally {
    if (sentinel.exitCode === null && sentinel.signalCode === null) {
      const closed = once(sentinel, "close"); sentinel.kill(); await closed;
    }
  }
});

for (const shell of ["powershell.exe", "pwsh"]) {
  test(`production Windows process entry drains both pipes and keeps argv/env [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $env:PINVOU_SECRET_SENTINEL='PRIVATE_ENV_SENTINEL';
        $code=${quote(`process.stderr.write('e'.repeat(2*1024*1024));process.stdout.write('o'.repeat(2*1024*1024));if(process.env.PINVOU_SECRET_SENTINEL)process.exitCode=9;`)};
        $r=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$code) -TimeoutMilliseconds 15000;
        if($r.ExitCode -ne 0 -or $r.StdOut.Length -ne 2097152 -or $r.StdErr.Length -ne 2097152){throw 'pipe-drain-failed'};
        $argsCode=${quote("process.stdout.write(JSON.stringify(process.argv.slice(1)))")};
        $a=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$argsCode,'','a b','a"b','x\') -TimeoutMilliseconds 15000;
        [Console]::Out.Write($a.StdOut)`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(JSON.parse(result.stdout), ["", "a b", 'a"b', "x\"]);
      assert.doesNotMatch(result.stderr, /PRIVATE_ENV_SENTINEL/u);
    });

  test(`production local entry delegates to the same bounded process scope [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile(${quote(orchestrator)},[ref]$tokens,[ref]$errors);
        if($errors.Count){throw 'production-parse-failed'};
        $f=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Invoke-PinvouLocalWhitelistedProcess'},$true);Invoke-Expression $f.Extent.Text;
        $r=Invoke-PinvouLocalWhitelistedProcess -FilePath ${quote(process.execPath)} -Arguments @('-e',${quote("process.stderr.write('e'.repeat(1048576));process.stdout.write('o'.repeat(1048576))")}) -TimeoutMilliseconds 15000;
        if($r.ExitCode -ne 0 -or $r.StdOut.Length -ne 1048576 -or $r.StdErr.Length -ne 1048576){throw 'local-drain-failed'};[Console]::Out.Write('ok')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "ok");
    });

  test(`production Windows timeout kills its exact detached descendants [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote("require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'}).unref();setInterval(()=>{},1000)")}) -TimeoutMilliseconds 500 | Out-Null; throw 'timeout-not-detected' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_timeout' -or $e.CleanupFailed){throw};[Console]::Out.Write('retired') }`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "retired");
    });

  test(`successful intentional Windows service remains owned until explicit cleanup [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $r=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote("require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'}).unref()")}) -TimeoutMilliseconds 15000 -RetainSuccessfulScope;
        if(-not $r.OwnedScope -or $r.OwnedScope.ActiveProcesses -ne 1){throw 'service-not-retained'};
        $r.OwnedScope.Dispose();if(-not $r.OwnedScope.IsClosed){throw 'scope-not-closed'};[Console]::Out.Write('retained-and-retired')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "retained-and-retired");
    });

  test(`production Windows cancellation follows an actual child-ready barrier [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        Add-Type -TypeDefinition 'using System;using System.IO;using System.Threading;using System.Threading.Tasks;public static class PinvouCancelProbe {public static Task Watch(string path,CancellationTokenSource source){return Task.Run(()=>{var end=DateTime.UtcNow.AddSeconds(10);while(!File.Exists(path)){if(DateTime.UtcNow>end)throw new Exception("child-ready-missing");Thread.Sleep(5);}source.Cancel();});}}';
        $warm=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e','');
        $marker=Join-Path $env:PINVOU_CI_FIXTURE_ROOT ([Guid]::NewGuid().ToString('N')+'.ready');
        $cts=[Threading.CancellationTokenSource]::new();$watch=[PinvouCancelProbe]::Watch($marker,$cts);
        $code='require("node:fs").writeFileSync('+($marker|ConvertTo-Json -Compress)+',"ready");setInterval(()=>{},1000)';
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$code) -TimeoutMilliseconds 15000 -CancellationToken $cts.Token | Out-Null;throw 'cancellation-not-detected' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_cancelled' -or $e.CleanupFailed){throw} }
        finally { $cts.Dispose() };
        $watch.GetAwaiter().GetResult();if(-not (Test-Path -LiteralPath $marker)){throw 'child-never-ready'};[Console]::Out.Write('cancelled-and-retired')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "cancelled-and-retired");
    });
}

function ps(shell, script) {
  const environment = { ...process.env };
  for (const key of Object.keys(environment)) if (key.toUpperCase() === "PSMODULEPATH") delete environment[key];
  return spawnSync(shell, ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")],
    { env: environment, encoding: "utf8", timeout: 30000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
}
