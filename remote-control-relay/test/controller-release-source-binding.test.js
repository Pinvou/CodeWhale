import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repo = fileURLToPath(new URL("../../", import.meta.url));
const quote = (value) => `'${value.replaceAll("'", "''")}'`;

test("production validator source capture rejects changed artifact reader and recovers after restore", {
  skip: process.platform !== "win32" ? "Windows signed-Node source binding; no cross-platform signing evidence" : false,
}, () => {
  const env = { ...process.env };
  for (const key of Object.keys(env)) if (key.toUpperCase() === "PSMODULEPATH") delete env[key];
  const script = `
    $ErrorActionPreference='Stop'
    $originalRoot=${quote(repo)}
    $tokens=$null;$errors=$null
    $ast=[Management.Automation.Language.Parser]::ParseFile((Join-Path $originalRoot 'scripts/run-windows-remote-control-e2e.ps1'),[ref]$tokens,[ref]$errors)
    if($errors.Count){throw 'production-parse-failed'}
    foreach($name in @('Get-PinvouFileSha256','Invoke-PinvouLocalWhitelistedProcess','Invoke-PinvouVerifiedLocalProcess')){
      $fn=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)
      if(-not $fn){throw 'production-function-missing'}
      . ([scriptblock]::Create($fn.Extent.Text))
    }
    $base=[IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    $scope=Join-Path $base ('pinvou-validator-binding-'+[Guid]::NewGuid().ToString('N'))
    [void](New-Item -ItemType Directory -Path $scope)
    try {
      $bindings=@{
        JourneyCatalog='remote-control-relay/docs/schemas/remote-control-journeys.v1.json'
        EvidenceSchema='remote-control-relay/docs/schemas/remote-control-release-evidence.v1.schema.json'
        EvidenceSchemaValidator='remote-control-relay/docs/schemas/remote-control-release-evidence.v1.validator.mjs'
        EvidenceValidator='remote-control-relay/scripts/validate-controller-release-evidence.mjs'
        EvidenceArtifactReader='remote-control-relay/scripts/release-evidence-artifacts.mjs'
        UiSmokeScript='pinvou3-app/scripts/remote-control-ui-smoke.mjs'
        ProcessGuardScript='scripts/windows-e2e-process-guard.ps1'
        OwnedProcessSource='scripts/windows-owned-process.cs'
        OrchestratorHelperScript='scripts/windows-e2e-orchestrator-helpers.ps1'
        RouteSpecPreparer='remote-control-relay/scripts/prepare-legacy-gateway-route.mjs'
        ProviderAttestationVerifier='remote-control-relay/scripts/verify-provider-attestation.mjs'
        InstallerSupplyChainVerifier='remote-control-relay/scripts/verify-installer-supply-chain.mjs'
      }
      foreach($name in $bindings.Keys){
        $target=Join-Path $scope $bindings[$name]
        [void](New-Item -ItemType Directory -Path (Split-Path $target -Parent) -Force)
        Copy-Item -LiteralPath (Join-Path $originalRoot $bindings[$name]) -Destination $target
        Set-Variable -Name $name -Value $target
      }
      foreach($relative in @('remote-control-relay/cli-entrypoint.js','remote-control-relay/package.json')){
        Copy-Item -LiteralPath (Join-Path $originalRoot $relative) -Destination (Join-Path $scope $relative)
      }
      $trustedSourceHashes=@{};$trustedSourceBytes=@{}
      $capture=$ast.Find({param($n)$n -is [Management.Automation.Language.ForEachStatementAst] -and $n.Variable.VariablePath.UserPath -eq 'trustedSource'},$true)
      if(-not $capture){throw 'production-capture-missing'}
      . ([scriptblock]::Create($capture.Extent.Text))
      $pins=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$validatorSourceHashes'},$true)
      if(-not $pins){throw 'production-pins-missing'}
      . ([scriptblock]::Create($pins.Extent.Text))
      if(-not $trustedSourceBytes.ContainsKey($EvidenceArtifactReader)){throw 'reader-not-captured'}
      if(-not $validatorSourceHashes.ContainsKey($EvidenceArtifactReader)){throw 'reader-not-pinned'}
      . $ProcessGuardScript
      $global:PinvouE2EOwnedProcessSha256=$trustedSourceHashes[$OwnedProcessSource]
      $node=${quote(process.execPath)}
      $signature=Get-AuthenticodeSignature -LiteralPath $node
      if($signature.Status -ne 'Valid'){throw 'test-node-not-signed'}
      $nodeHash=Get-PinvouFileSha256 $node
      $thumb=$signature.SignerCertificate.Thumbprint.ToUpperInvariant()
      $publisher=$signature.SignerCertificate.Subject
      $observation='{"clock":"monotonic","rtt_ms":100,"deadline_ms":28000,"terminal_elapsed_ms":27500,"reconnect_extended_deadline":false,"terminal_state":"peer_unavailable","watchdog_fired":true,"task_cancelled":true,"pending_approvals_rejected":true,"credentials_revoked":true,"temp_model_cleaned":true}'
      $argsForNode=@($EvidenceValidator,'--deadline-observation-base64',[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($observation)))
      $baseline=Invoke-PinvouVerifiedLocalProcess -FilePath $node -ExpectedSha256 $nodeHash -ExpectedThumbprint $thumb -ExpectedPublisher $publisher -Arguments $argsForNode -AdditionalExpectedSha256 $validatorSourceHashes
      if($baseline.ExitCode -ne 0){throw 'baseline-validator-failed'}
      Add-Content -LiteralPath $EvidenceArtifactReader -Value '// changed after capture'
      $blocked=$false
      try {$null=Invoke-PinvouVerifiedLocalProcess -FilePath $node -ExpectedSha256 $nodeHash -ExpectedThumbprint $thumb -ExpectedPublisher $publisher -Arguments $argsForNode -AdditionalExpectedSha256 $validatorSourceHashes}
      catch {if($_.Exception.Message -ne 'e2e_local_dependency_hash_mismatch'){throw};$blocked=$true}
      if(-not $blocked){throw 'changed-reader-was-executed'}
      [IO.File]::WriteAllBytes($EvidenceArtifactReader,$trustedSourceBytes[$EvidenceArtifactReader])
      $restored=Invoke-PinvouVerifiedLocalProcess -FilePath $node -ExpectedSha256 $nodeHash -ExpectedThumbprint $thumb -ExpectedPublisher $publisher -Arguments $argsForNode -AdditionalExpectedSha256 $validatorSourceHashes
      if($restored.ExitCode -ne 0){throw 'restored-validator-failed'}
      'source-binding-recovery-pass'
    } finally {
      $resolved=[IO.Path]::GetFullPath($scope)
      if(-not $resolved.StartsWith(($base.TrimEnd([IO.Path]::DirectorySeparatorChar)+[IO.Path]::DirectorySeparatorChar),[StringComparison]::OrdinalIgnoreCase)){throw 'fixture-cleanup-path-invalid'}
      Remove-Item -LiteralPath $resolved -Recurse -Force
    }
  `;
  const output = execFileSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", script], { env, encoding: "utf8" });
  assert.match(output, /source-binding-recovery-pass/u);
});
