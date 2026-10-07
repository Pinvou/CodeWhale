import assert from "node:assert/strict";
import crypto from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test as nodeTest } from "node:test";

const helper = fileURLToPath(new URL("../../scripts/windows-e2e-orchestrator-helpers.ps1", import.meta.url));
const orchestrator = fileURLToPath(new URL("../../scripts/run-windows-remote-control-e2e.ps1", import.meta.url));
const guard = fileURLToPath(new URL("../../scripts/windows-e2e-process-guard.ps1", import.meta.url));
const uiSmoke = fileURLToPath(new URL("../../pinvou3-app/scripts/remote-control-ui-smoke.mjs", import.meta.url));
const pwsh = process.platform === "win32" ? "powershell.exe" : "pwsh";
const quote = (value) => `'${value.replaceAll("'", "''")}'`;
const psProbe = spawnSync(pwsh, ['-NoProfile', '-NonInteractive', '-Command', 'exit 0']);
const test = (name, run) => nodeTest(name, {
  skip: process.platform !== 'win32' && psProbe.error ? 'PowerShell unavailable; no production AST execution evidence' : false,
}, run);
// Load definitions from the production script without running its deployment.
const productionAst = `$tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile(${quote(orchestrator)},[ref]$tokens,[ref]$errors);if($errors.Count){throw 'production-parse-failed'}; `;
const productionFunctions = productionAst
  + `foreach($name in @('Get-PinvouSha256Hex','Assert-PinvouJourneyElapsed','Assert-PinvouRouteStateBinding')){$definition=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true);if(-not $definition){throw 'production-function-missing'};Invoke-Expression $definition.Extent.Text}; `;
const productionStaging = productionAst
  + `$assignment=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$RemoteCreateStaging'},$true);if(-not $assignment){throw 'production-staging-missing'};$create=[scriptblock]::Create($assignment.Right.Extent.Text.Trim().TrimStart('{').TrimEnd('}')); `;
// Only remoting variable transport is adapted to local fixture bindings. The
// actual cleanup branches, ordering and ownership checks are not duplicated.
const productionCleanup = `$cleanupNode=@($ast.FindAll({param($n)$n -is [Management.Automation.Language.ScriptBlockExpressionAst] -and $n.Extent.Text.Contains('$remoteCleanupErrors =')},$true)|Sort-Object {$_.Extent.Text.Length})[0];if(-not $cleanupNode){throw 'production-cleanup-missing'};$cleanupText=$cleanupNode.Extent.Text;$cleanup=[scriptblock]::Create($cleanupText.Substring(1,$cleanupText.Length-2).Replace('$using:','$')); `;

test("production orchestrator functions enforce measured elapsed time and route state binding", () => {
  const digest = "a".repeat(64);
  const combined = crypto.createHash("sha256").update(JSON.stringify(["pinvou-gateway-state-v1", digest, digest])).digest("hex");
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command",
    `${productionFunctions} Assert-PinvouJourneyElapsed 23900 24000; Assert-PinvouRouteStateBinding '${digest}' '${digest}' '${combined}' '${digest}' '${combined}' '${combined}'`], { stdio: "pipe" }));
  const fast = spawnSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", `${productionFunctions} Assert-PinvouJourneyElapsed 1000 23900`], { encoding: "utf8" });
  assert.notEqual(fast.status, 0); assert.match(`${fast.stdout}${fast.stderr}`, /e2e_monotonic_elapsed_mismatch/u);
  const wrongRoute = spawnSync(pwsh, ['-NoProfile', '-NonInteractive', '-Command',
    `${productionFunctions} Assert-PinvouRouteStateBinding '${digest}' '${digest}' '${digest}' '${digest}' '${combined}' '${combined}'`], { encoding: 'utf8' });
  assert.notEqual(wrongRoute.status, 0);
  assert.match(`${wrongRoute.stdout}${wrongRoute.stderr}`, /e2e_legacy_gateway_original_state_mismatch/u);
});

test("per-run staging is hash checked, collision-safe and ownership-bound on cleanup", () => {
  const nonce = "d".repeat(64);
  const script = `$ErrorActionPreference='Stop'; . ${quote(helper)}; ${productionStaging} ${productionCleanup} $base=Join-Path ([IO.Path]::GetTempPath()) ('pinvou-e2e-stage-'+[Guid]::NewGuid().ToString('N')); `
    + `try { $s=& $create $base '${nonce}' ([DateTime]::UtcNow); if(-not [IO.Path]::IsPathRooted($s.root)){throw 'staging-contract'}; `
    + `$file=Join-Path $s.root 'probe.bin'; [IO.File]::WriteAllBytes($file,[byte[]](1,2,3)); $sha=(Get-FileHash $file -Algorithm SHA256).Hash.ToLowerInvariant(); `
    + `[void](Assert-PinvouTrustedStagedFile 'probe.bin' $sha); `
    + `[IO.File]::WriteAllBytes($file,[byte[]](4,5,6)); try {[void](Assert-PinvouTrustedStagedFile 'probe.bin' $sha); throw 'tamper-not-rejected'} catch {if($_.Exception.Message -eq 'tamper-not-rejected'){throw}}; `
    + `try { & $create $base '${nonce}' ([DateTime]::UtcNow) | Out-Null; throw 'collision-not-rejected' } catch { if($_.Exception.Message -notmatch 'e2e_staging_already_exists'){throw} }; `
    + `$RemoteBase=$base;$RemoteRoot=$s.root;$runNonce='${nonce}';$cleanupLegacyOwned=$false; `
    + `$markerPath=Join-Path $s.root '.pinvou-e2e-owner.json';$marker=Get-Content -Raw $markerPath;$wrong=$marker.Replace('${nonce}','${'e'.repeat(64)}');[IO.File]::WriteAllText($markerPath,$wrong); `
    + `try{& $cleanup;throw 'owner-mismatch-accepted'}catch{if($_.Exception.Message -notmatch 'e2e_staging_cleanup_owner_mismatch'){throw}};if(-not(Test-Path $s.root)){throw 'foreign-owner-deleted'};[IO.File]::WriteAllText($markerPath,$marker); `
    + `& $cleanup; if(Test-Path $s.root){throw 'cleanup-incomplete'} } finally { if(Test-Path $base){Remove-Item -LiteralPath $base -Recurse -Force} }`;
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", script], { stdio: "pipe" }));
});

test("production cleanup continues after route and role faults and reports their aggregate", () => {
  const result = spawnSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command",
    `$ErrorActionPreference='Stop'; . ${quote(helper)}; ${productionStaging} ${productionCleanup} `
    + `$base=Join-Path ([IO.Path]::GetTempPath()) ('pinvou-e2e-fault-'+[Guid]::NewGuid().ToString('N'));try{ `
    + `$runNonce='${'d'.repeat(64)}';$s=& $create $base $runNonce ([DateTime]::UtcNow);$RemoteRoot=$s.root;$RemoteBase=$base;$cleanupLegacyOwned=$true;$cleanupLegacyPid=$null; `
    + `$LegacyGatewayUrl='fixture';$cleanupRouteId='fixture';$cleanupOriginalStateDigest='fixture';$cleanupCanonicalInputDigest='fixture';$cleanupControlPlaneDigest='fixture';$actualHead='fixture'; `
    + `[IO.File]::WriteAllText((Join-Path $s.root 'pinvou-e2e-driver.exe'),'fixture');$script:calls=@();function Invoke-PinvouTrustedGuard{param($FilePath,$Arguments);$script:calls+=@($Arguments[0]);$global:LASTEXITCODE=0;switch($Arguments[0]){ `
    + `'legacy-deconfigure-gateway'{throw 'route-fixture-fault'} 'legacy-owned-pids'{[ordered]@{schema='pinvou-legacy-owned-processes';version=1;run_nonce=$runNonce;mock=$false;active=@();terminated=@()}|ConvertTo-Json -Compress} 'legacy-stop'{} 'stop-role'{throw 'role-fixture-fault'} default{throw 'unexpected-fixture-command'}}}; `
    + `$message='';try{& $cleanup}catch{$message=$_.Exception.Message};if($message -notmatch 'route:route-fixture-fault.*role-process:role-fixture-fault'){throw 'faults-not-aggregated'};if(($script:calls -join ',') -ne 'legacy-deconfigure-gateway,legacy-owned-pids,legacy-stop,legacy-owned-pids,stop-role'){throw 'later-cleanup-skipped'};if(Test-Path $s.root){throw 'files-cleanup-skipped'};[Console]::Out.Write('production-cleanup-aggregate-pass') `
    + `}finally{if(Test-Path -LiteralPath $base){Remove-Item -LiteralPath $base -Recurse -Force}}`], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, 'production-cleanup-aggregate-pass');
});

test("legacy ownership registry is strict and recovers a lost response PID", () => {
  const nonce = "e".repeat(64);
  const command = `. ${quote(helper)}; $registry=[pscustomobject]@{schema='pinvou-legacy-owned-processes';version=[int]1;run_nonce='${nonce}';mock=$false;active=[object[]]@([int]43210);terminated=[object[]]@()}; `
    + `$resolved=Assert-PinvouLegacyOwnershipRegistry -RunNonce '${nonce}' -Registry $registry -CallerObservedPid $null; if($resolved.active.Count -ne 1 -or $resolved.active[0] -ne 43210){throw 'failure_window_pid_not_recovered'}; `
    + `function Expect-Rejected([scriptblock]$Action){try{& $Action;throw 'invalid-registry-accepted'}catch{if($_.Exception.Message -eq 'invalid-registry-accepted'){throw}}}; `
    + `$registry.mock='false';Expect-Rejected {Assert-PinvouLegacyOwnershipRegistry -RunNonce '${nonce}' -Registry $registry};$registry.mock=$false; `
    + `$registry.active=[object[]]@([int]43210,[int]43210);Expect-Rejected {Assert-PinvouLegacyOwnershipRegistry -RunNonce '${nonce}' -Registry $registry}`;
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", command], { stdio: "pipe" }));
  const invalid = spawnSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command",
    `${command.replace("$resolved=Assert-PinvouLegacyOwnershipRegistry", "$registry | Add-Member -NotePropertyName injected -NotePropertyValue $true; $resolved=Assert-PinvouLegacyOwnershipRegistry")}`], { encoding: "utf8" });
  assert.notEqual(invalid.status, 0);
  assert.match(`${invalid.stdout}${invalid.stderr}`, /e2e_legacy_ownership_query_failed/u);
});

test("production bootstrap retains the complete helper call chain across runspace invocations", () => {
  const script = `$ErrorActionPreference='Stop'; $base=Join-Path ([IO.Path]::GetTempPath()) ('pinvou-e2e-scope-'+[Guid]::NewGuid().ToString('N')); `
    + `$runspace=[RunspaceFactory]::CreateRunspace(); $runspace.Open(); $ps=[PowerShell]::Create(); $ps.Runspace=$runspace; `
    + `try { New-Item -ItemType Directory -Path $base | Out-Null; `
    + `$inputs=@{ 'windows-e2e-orchestrator-helpers.ps1'=${quote(helper)}; 'windows-e2e-process-guard.ps1'=${quote(guard)}; 'remote-control-ui-smoke.mjs'=${quote(uiSmoke)}; 'pinvou-node.exe'=${quote(process.execPath)} }; `
    + `$files=@(foreach($name in $inputs.Keys){$path=Join-Path $base $name; Copy-Item -LiteralPath $inputs[$name] -Destination $path; [pscustomobject]@{RelativePath=$name;Sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant();SignaturePolicy='none'}}); `
    + `$tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile(${quote(orchestrator)},[ref]$tokens,[ref]$errors);if($errors.Count){throw 'bootstrap-parse-failed'}; `
    + `$assignment=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$RemoteBootstrap'},$true);if(-not $assignment){throw 'bootstrap-missing'}; `
    + `$literal=$assignment.Right.Extent.Text;$bootstrap=$literal.Substring(1,$literal.Length-2); `
    + `[void]$ps.AddScript('param($root,$bootstrap,$files); & { param($root,$bootstrap,$files); $global:PinvouE2ERunRoot=$root; & ([scriptblock]::Create($bootstrap)) $root $files fixture fixture fixture fixture } $root $bootstrap $files').AddArgument($base).AddArgument($bootstrap).AddArgument(($files|ConvertTo-Json -Compress)); `
    + `$first=@($ps.Invoke());if($ps.HadErrors){throw ($ps.Streams.Error|Out-String)};if($first[0].verified_file_count -ne 4){throw 'bootstrap-did-not-run'};$ps.Commands.Clear(); `
    // Only Authenticode is a local test stub. Bootstrap and every transitive
    // production helper run unmodified, after the first local scope has ended.
    + `$call={ param($root,$files); $ErrorActionPreference='Stop'; function global:Get-AuthenticodeSignature { [pscustomobject]@{Status='Valid';SignerCertificate=[pscustomobject]@{Thumbprint='fixture';Subject='fixture'}} }; `
    + `$expected=$files|ConvertFrom-Json;$sha=($expected|Where-Object RelativePath -eq 'remote-control-ui-smoke.mjs').Sha256; `
    + `[void](Assert-PinvouTrustedStagedFile 'remote-control-ui-smoke.mjs' $sha); `
    + `try {[void](Assert-PinvouTrustedStagedFile '../outside' $sha);throw 'escape-accepted'}catch{if($_.Exception.Message -notmatch 'e2e_trusted_file_escape'){throw}}; `
    + `$guard=Invoke-PinvouTrustedGuard -FilePath (Join-Path $root 'pinvou-node.exe') -Arguments @('-e','process.stdout.write("guard-ok")') -ReturnProcessResult; if($guard.ExitCode -ne 0 -or $guard.StdOut -ne 'guard-ok'){throw 'guard-chain-failed'}; `
    + `$output=Invoke-PinvouTrustedNode -Arguments @('-e','process.stdout.write("node-ok")');if($output -ne 'node-ok'){throw 'node-chain-failed'}; `
    + `$nonce='${"e".repeat(64)}';$registry=[pscustomobject]@{schema='pinvou-legacy-owned-processes';version=[int]1;run_nonce=$nonce;mock=$false;active=[object[]]@([int]43210);terminated=[object[]]@()}; `
    + `$owned=Assert-PinvouLegacyOwnershipRegistry -RunNonce $nonce -Registry $registry;if($owned.active[0] -ne 43210){throw 'registry-chain-failed'}; `
    + `if((Get-PinvouSha256Hex ([Text.Encoding]::UTF8.GetBytes('scope'))).Length -ne 64){throw 'digest-chain-failed'}; 'scope-contract-pass' }; `
    + `[void]$ps.AddScript($call.ToString()).AddArgument($base).AddArgument(($files|ConvertTo-Json -Compress));$second=@($ps.Invoke());if($ps.Streams.Error.Count){throw ($ps.Streams.Error|Out-String)};if($second[-1] -ne 'scope-contract-pass'){throw 'second-invocation-did-not-run'} `
    + `} finally { $ps.Dispose();$runspace.Dispose();if(Test-Path -LiteralPath $base){Remove-Item -LiteralPath $base -Recurse -Force} }`;
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", script], { stdio: "pipe", timeout: 30_000 }));
});
