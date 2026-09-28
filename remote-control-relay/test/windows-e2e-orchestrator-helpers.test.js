import assert from "node:assert/strict";
import crypto from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const helper = fileURLToPath(new URL("../../scripts/windows-e2e-orchestrator-helpers.ps1", import.meta.url));
const pwsh = process.platform === "win32" ? "powershell.exe" : "pwsh";
const quote = (value) => `'${value.replaceAll("'", "''")}'`;

test("orchestrator helpers enforce measured elapsed time and route state binding", () => {
  const digest = "a".repeat(64);
  const combined = crypto.createHash("sha256").update(JSON.stringify(["pinvou-gateway-state-v1", digest, digest])).digest("hex");
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command",
    `. ${quote(helper)}; Assert-PinvouJourneyElapsed 23900 24000; Assert-PinvouRouteStateBinding '${digest}' '${digest}' '${combined}' '${digest}' '${combined}' '${combined}'`], { stdio: "pipe" }));
  const fast = spawnSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", `. ${quote(helper)}; Assert-PinvouJourneyElapsed 1000 23900`], { encoding: "utf8" });
  assert.notEqual(fast.status, 0); assert.match(`${fast.stdout}${fast.stderr}`, /e2e_monotonic_elapsed_mismatch/u);
});

test("per-run staging is hash checked, collision-safe and ownership-bound on cleanup", () => {
  const nonce = "d".repeat(64);
  const script = `. ${quote(helper)}; $base=Join-Path ([IO.Path]::GetTempPath()) ('pinvou-e2e-stage-'+[Guid]::NewGuid().ToString('N')); `
    + `try { $s=New-PinvouRunStaging $base '${nonce}' ([DateTime]::UtcNow); if(-not [IO.Path]::IsPathRooted($s.root)){throw 'staging-contract'}; `
    + `$file=Join-Path $s.root 'probe.bin'; [IO.File]::WriteAllBytes($file,[byte[]](1,2,3)); $sha=(Get-FileHash $file -Algorithm SHA256).Hash.ToLowerInvariant(); `
    + `Assert-PinvouStagedFiles @([pscustomobject]@{RelativePath='probe.bin';Sha256=$sha}); [void](Assert-PinvouTrustedStagedFile 'probe.bin' $sha); `
    + `[IO.File]::WriteAllBytes($file,[byte[]](4,5,6)); try {[void](Assert-PinvouTrustedStagedFile 'probe.bin' $sha); throw 'tamper-not-rejected'} catch {if($_.Exception.Message -eq 'tamper-not-rejected'){throw}}; `
    + `try { New-PinvouRunStaging $base '${nonce}' ([DateTime]::UtcNow) | Out-Null; throw 'collision-not-rejected' } catch { if($_.Exception.Message -eq 'collision-not-rejected'){throw} }; `
    + `Remove-PinvouRunStaging $base '${nonce}'; if(Test-Path $s.root){throw 'cleanup-incomplete'} } finally { if(Test-Path $base){Remove-Item $base -Recurse -Force} }`;
  assert.doesNotThrow(() => execFileSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command", script], { stdio: "pipe" }));
});

test("cleanup helper continues after faults and reports their aggregate", () => {
  const result = spawnSync(pwsh, ["-NoProfile", "-NonInteractive", "-Command",
    `. ${quote(helper)}; $script:steps=''; Invoke-PinvouCleanupSteps @({$script:steps+='A';throw 'first'},{$script:steps+='B'},{$script:steps+='C';throw 'third'})`], { encoding: "utf8" });
  assert.notEqual(result.status, 0); assert.match(`${result.stdout}${result.stderr}`, /step-0:first.*step-2:third/su);
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
