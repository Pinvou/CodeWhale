import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

const quote = (value) => `'${value.replaceAll("'", "''")}'`;
const guard = fileURLToPath(new URL("../../scripts/windows-e2e-process-guard.ps1", import.meta.url));
const helper = fileURLToPath(new URL("../../scripts/windows-e2e-orchestrator-helpers.ps1", import.meta.url));
const core = fileURLToPath(new URL("../../scripts/windows-owned-process.cs", import.meta.url));
const orchestrator = fileURLToPath(new URL("../../scripts/run-windows-remote-control-e2e.ps1", import.meta.url));
const driverSource = fileURLToPath(new URL("./fixtures/windows-owned-driver.cs", import.meta.url));
const ui = fileURLToPath(new URL("../../pinvou3-app/scripts/remote-control-ui-smoke.mjs", import.meta.url));

for (const shell of ["powershell.exe", "pwsh"]) {
  test(`real runspace staging retains nonce-owned services until production final cleanup [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      // Authentication/provider/gateway and the executable driver protocol are
      // explicit seams. Staging, hashes, Bootstrap, TrustedGuard, OS children,
      // durable identity/registry, runspace custody and final cleanup are real.
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $base=Join-Path $env:PINVOU_CI_FIXTURE_ROOT ('runspace-'+[Guid]::NewGuid().ToString('N'));
        New-Item -ItemType Directory -Path $base | Out-Null;
        $compiled=Join-Path $base 'fixture-driver.exe';
        $csc=Join-Path $env:WINDIR 'Microsoft.NET/Framework64/v4.0.30319/csc.exe';
        $reference=Join-Path (Split-Path $csc) 'System.Web.Extensions.dll';
        $extra=@{};$extra[${quote(driverSource)}]=(Get-FileHash -LiteralPath ${quote(driverSource)} -Algorithm SHA256).Hash.ToLowerInvariant();$extra[$reference]=(Get-FileHash -LiteralPath $reference -Algorithm SHA256).Hash.ToLowerInvariant();
        $build=Invoke-PinvouWhitelistedProcess -FilePath $csc -ExpectedSha256 (Get-FileHash -LiteralPath $csc -Algorithm SHA256).Hash.ToLowerInvariant() -Arguments @('/nologo','/target:exe',('/out:'+$compiled),('/r:'+$reference),${quote(driverSource)}) -AdditionalExpectedSha256 $extra;
        if($build.ExitCode -ne 0){throw 'fixture-driver-build-failed'};
        [Console]::Error.WriteLine('fixture-phase:compiled');
        $tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile(${quote(orchestrator)},[ref]$tokens,[ref]$errors);if($errors.Count){throw 'orchestrator-parse-failed'};
        $create=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$RemoteCreateStaging'},$true).Right.Extent.Text;
        $bootstrap=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$RemoteBootstrap'},$true).Right.Extent.Text;
        $cleanup=@($ast.FindAll({param($n)$n -is [Management.Automation.Language.ScriptBlockExpressionAst] -and $n.Extent.Text.Contains('$remoteCleanupErrors =')},$true)|Sort-Object {$_.Extent.Text.Length})[0].Extent.Text;
        $create=$create.Substring(1,$create.Length-2);$bootstrap=$bootstrap.Substring(1,$bootstrap.Length-2);$cleanup=$cleanup.Substring(1,$cleanup.Length-2).Replace('$using:','$');
        $nonce='${"b".repeat(64)}';
        $inputs=@{ 'windows-e2e-process-guard.ps1'=${quote(guard)};'windows-e2e-orchestrator-helpers.ps1'=${quote(helper)};'windows-owned-process.cs'=${quote(core)};'remote-control-ui-smoke.mjs'=${quote(ui)};'pinvou-e2e-driver.exe'=$compiled;'pinvou-node.exe'=${quote(process.execPath)};'legacy-relay.exe'=$compiled };
        $runspace=[RunspaceFactory]::CreateRunspace();$runspace.Open();$ps=[PowerShell]::Create();$ps.Runspace=$runspace;
        $testFailure=$null;
        try {
          [void]$ps.AddScript({param($base,$nonce,$create,$bootstrap,$inputs)
            $ErrorActionPreference='Stop';$stage=& ([scriptblock]::Create($create)) $base $nonce ([DateTime]::UtcNow);
            $files=@(foreach($name in $inputs.Keys){$path=Join-Path $stage.root $name;Copy-Item -LiteralPath $inputs[$name] -Destination $path;[pscustomobject]@{RelativePath=$name;Sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant();SignaturePolicy='none'}});
            & ([scriptblock]::Create($bootstrap)) $stage.root ($files|ConvertTo-Json -Compress) fixture fixture fixture fixture | Out-Null;
            $stage.root
          }).AddArgument($base).AddArgument($nonce).AddArgument($create).AddArgument($bootstrap).AddArgument($inputs);
          $first=@($ps.Invoke());if($ps.HadErrors){throw ($ps.Streams.Error|Out-String)};$root=[string]$first[-1];$ps.Commands.Clear();
          [Console]::Error.WriteLine('fixture-phase:bootstrapped');
          [void]$ps.AddScript({param($nonce)
            $ErrorActionPreference='Stop';$PSDefaultParameterValues['Invoke-PinvouWhitelistedProcess:TimeoutMilliseconds']=5000;$driver=Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe';
            try { $runtime=(Invoke-PinvouTrustedGuard -FilePath $driver -Arguments @('legacy-start','--run-nonce',$nonce,'--persist-ownership-before-response'))|ConvertFrom-Json }
            catch {
              $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};[Console]::Error.WriteLine(('fixture-root-exit:'+ $e.ChildExitCode));
              $phase=Join-Path $global:PinvouE2ERunRoot 'fixture-driver-phase.log';if(Test-Path -LiteralPath $phase){[Console]::Error.WriteLine([IO.File]::ReadAllText($phase))};throw
            };
            if($LASTEXITCODE -ne 0){throw 'legacy-start-failed'};
            [Console]::Error.WriteLine('fixture-phase:legacy-started');
            $registry=(Invoke-PinvouTrustedGuard -FilePath $driver -Arguments @('legacy-owned-pids','--run-nonce',$nonce,'--durable-registry-only'))|ConvertFrom-Json;
            $owned=Assert-PinvouLegacyOwnershipRegistry -RunNonce $nonce -Registry $registry -CallerObservedPid ([Nullable[int]][int]$runtime.pid);
            if($owned.active.Count -ne 1){throw 'durable-ownership-not-confirmed'};
            [void](Invoke-PinvouTrustedGuard -FilePath $driver -Arguments @('start-role','--role','A'));
            if($global:PinvouE2EOwnedProcessScopes.Count -ne 2){throw 'run-scopes-not-retained'};
            foreach($scope in $global:PinvouE2EOwnedProcessScopes){if($scope.ActiveProcesses -ne 1){throw ('real-service-not-live:'+ $scope.ActiveProcesses)}};
            $runtime.pid
          }).AddArgument($nonce);
          $second=@($ps.Invoke());if($ps.HadErrors){throw ($ps.Streams.Error|Out-String)};$legacyPid=[int]$second[-1];$ps.Commands.Clear();
          [Console]::Error.WriteLine('fixture-phase:services-retained');
          [void]$ps.AddScript({param($root,$base,$nonce,$legacyPid,$cleanup)
            $ErrorActionPreference='Stop';
            if($global:PinvouE2EOwnedProcessScopes.Count -ne 2){throw 'scope-custody-lost-between-invocations'};
            $global:savedScopes=@($global:PinvouE2EOwnedProcessScopes);
            $RemoteRoot=$root;$RemoteBase=$base;$runNonce=$nonce;$cleanupLegacyOwned=$true;$cleanupLegacyPid=[Nullable[int]]$legacyPid;
            $LegacyGatewayUrl='fixture';$cleanupRouteId='fixture';$cleanupOriginalStateDigest='original';$cleanupCanonicalInputDigest='canonical';$cleanupControlPlaneDigest='before';$actualHead='fixture';
            & ([scriptblock]::Create($cleanup));
            if(Test-Path -LiteralPath $root){throw 'production-final-cleanup-left-staging'};
            foreach($scope in $global:savedScopes){if(-not $scope.IsClosed){throw 'held-job-not-closed'}};
            if($global:PinvouE2EOwnedProcessScopes.Count -ne 0){throw 'run-custody-not-cleared'};
            'real-runspace-services-retired'
          }).AddArgument($root).AddArgument($base).AddArgument($nonce).AddArgument($legacyPid).AddArgument($cleanup);
          $third=@($ps.Invoke());if($ps.HadErrors){throw ($ps.Streams.Error|Out-String)};
          if($third[-1] -ne 'real-runspace-services-retired'){throw 'final-path-not-executed'};
          [Console]::Out.Write($third[-1]);
        } catch { $testFailure=$_;throw } finally {
          try {
            $ps.Commands.Clear();$ps.Streams.Error.Clear();
            [void]$ps.AddScript('if(Get-Command Close-PinvouOwnedProcessScopes -ErrorAction SilentlyContinue){Close-PinvouOwnedProcessScopes}');[void]$ps.Invoke();
            if($ps.HadErrors){throw 'fixture-runspace-final-cleanup-unconfirmed'};
          } catch {
            if($testFailure){$testFailure.Exception.Data['FixtureCleanupFailed']=$true;[Console]::Error.WriteLine('fixture-runspace-final-cleanup-unconfirmed')}else{throw}
          } finally { $ps.Dispose();$runspace.Dispose() }
        }`;
      const environment = { ...process.env };
      for (const key of Object.keys(environment)) if (key.toUpperCase() === "PSMODULEPATH") delete environment[key];
      const result = spawnSync(shell, ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")],
        { env: environment, encoding: "utf8", timeout: 40000, maxBuffer: 4 * 1024 * 1024, windowsHide: true });
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "real-runspace-services-retired");
    });
}
