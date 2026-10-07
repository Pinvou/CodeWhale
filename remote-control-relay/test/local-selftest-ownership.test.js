import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const selftest = fileURLToPath(new URL("../../scripts/remote-control-local-selftest.ps1", import.meta.url));

test("actual local stop rejects stale or foreign processes and never kills by port", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, () => {
  // Execute real functions with every process/IO boundary mocked. Never start
  // a Relay, touch its keystore or stop an actual process.
  const command = [
    "$ErrorActionPreference='Stop'",
    "$tokens=$null; $errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "if($errors.Count){throw 'selftest parse failed'}",
    "foreach($name in @('Get-RelayProcess','Stop-LocalRelay')) {",
    " $defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq $name})",
    " if($defs.Count -ne 1){throw 'missing actual function'}",
    " . ([scriptblock]::Create($defs[0].Extent.Text))",
    "}",
    "$pidFile='TEST_ONLY_NO_FILE'; $relayDir='C:\\fixture\\remote-control-relay'; $NodeExe='TEST_ONLY_NODE'; $Port=8787",
    "$nodePath='C:\\fixture\\node.exe'; $entry=Join-Path $relayDir 'server.js'",
    "$started=[datetime]'2026-01-01T00:00:00Z'",
    "$script:details=[pscustomobject]@{CommandLine=('\"'+$nodePath+'\" \"'+$entry+'\"')}",
    "$script:process=[pscustomobject]@{Id=1234;Path=$nodePath}",
    "$script:handlePinned=$false; $script:handleBeforeStart=$true; $script:actualStart=$started",
    "$script:process|Add-Member -MemberType ScriptProperty -Name Handle -Value {if($script:mode -eq 'handle-fails'){throw 'cannot pin handle'}; if($script:mode -eq 'handle-zero'){return [IntPtr]::Zero}; $script:handlePinned=$true; return [IntPtr]123}",
    "$script:process|Add-Member -MemberType ScriptProperty -Name StartTime -Value {if(-not $script:handlePinned){$script:handleBeforeStart=$false;throw 'time read before handle pin'}; return $script:actualStart}",
    "$script:record=[pscustomobject]@{version=1;process_id=1234;started_at_ticks=$started.ToUniversalTime().Ticks.ToString()}",
    "$script:exists=$true; $script:stopped=0; $script:portLookups=0; $script:mode='valid'",
    "function Test-Path {param($LiteralPath) return $script:exists}",
    "function Get-Content {param($LiteralPath,[switch]$Raw) if($script:mode -eq 'legacy'){return '1234'}; if($script:mode -eq 'corrupt'){return '{bad'}; return ($script:record|ConvertTo-Json -Compress)}",
    "function Get-Process {param($Id,$ErrorAction) if($script:mode -eq 'missing'){throw 'absent'}; return $script:process}",
    "function Get-Command {param($Name,$CommandType,$ErrorAction) return [pscustomobject]@{Source=$nodePath}}",
    "function Get-CimInstance {param($ClassName,$Filter,$ErrorAction) if($script:mode -eq 'cim-fails'){throw 'unavailable'}; if($script:mode -eq 'cim-missing'){return $null}; return $script:details}",
    "function Get-PortOwner {param($localPort) $script:portLookups++;throw 'stop must never inspect port ownership'}",
    "function Stop-Process {param($InputObject,[switch]$Force) if(-not $script:handlePinned -or -not [object]::ReferenceEquals($InputObject,$script:process)){throw 'must use handle-pinned verified process object'}; $script:stopped++}",
    "function Remove-Item {param($LiteralPath,[switch]$Force,$ErrorAction) if($LiteralPath -ne $pidFile){throw 'wrong cleanup target'}}",
    "$out=@()",
    "foreach($case in @('no-file','legacy','corrupt','missing','handle-fails','handle-zero','reused-pid','foreign-exe','foreign-script','suffix-script','extra-arg','cim-fails','cim-missing','valid')) {",
    " $script:exists=$case -ne 'no-file'; $script:mode=$case",
    " $script:handlePinned=$false; $script:actualStart=if($case -eq 'reused-pid'){$started.AddSeconds(1)}else{$started}",
    " $script:process.Path=if($case -eq 'foreign-exe'){'C:\\fixture\\other.exe'}else{$nodePath}",
    " $script:details.CommandLine=('\"'+$nodePath+'\" \"'+$entry+'\"')",
    " if($case -eq 'foreign-script'){$script:details.CommandLine='\"C:\\fixture\\node.exe\" \"C:\\fixture\\other.js\"'}",
    " if($case -eq 'suffix-script'){$script:details.CommandLine=('\"'+$nodePath+'\" \"'+$entry+'.other\"')}",
    " if($case -eq 'extra-arg'){$script:details.CommandLine+=' --other-service'}",
    " $before=$script:stopped; Stop-LocalRelay *> $null",
    " $out+=@{case=$case;stops=($script:stopped-$before)}",
    "}",
    "@{cases=$out;port_lookups=$script:portLookups;handle_before_start=$script:handleBeforeStart}|ConvertTo-Json -Depth 5 -Compress",
  ].join("\n");
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: { ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest }, encoding: "utf8", timeout: 20_000,
  });
  assert.equal(result.status, 0, result.stderr);
  const projected = JSON.parse(result.stdout.trim());
  assert.equal(projected.port_lookups, 0);
  assert.equal(projected.cases.length, 14);
  assert.equal(projected.handle_before_start, true);
  for (const row of projected.cases) assert.equal(row.stops, row.case === "valid" ? 1 : 0, row.case);
});

test("process handle is acquired before ownership checks and the same object reaches stop", () => {
  const source = readFileSync(selftest, "utf8");
  const body = source.slice(source.indexOf("function Get-RelayProcess"), source.indexOf("function Test-LocalPort"));
  assert.ok(body.indexOf("$handle = $process.Handle") < body.indexOf("$process.StartTime"));
  assert.ok(body.indexOf("$handle = $process.Handle") < body.indexOf("$process.Path"));
  assert.match(source, /Stop-Process -InputObject \$process -Force/);
  assert.doesNotMatch(source, /Stop-Process -Id/);
});

test("actual local signer refuses replacement when env or recoverable state survives", {
  skip: process.platform !== "win32" ? "requires Windows PowerShell" : false,
}, () => {
  const command = [
    "$ErrorActionPreference='Stop'",
    "$tokens=$null; $errors=$null",
    "$ast=[Management.Automation.Language.Parser]::ParseFile($env:PINVOU_TEST_SELFTEST_PATH,[ref]$tokens,[ref]$errors)",
    "$defs=@($ast.EndBlock.Statements|Where-Object {$_ -is [Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -eq 'New-OrLoadSigningKey'})",
    "if($errors.Count -or $defs.Count -ne 1){throw 'expected signer function'}",
    ". ([scriptblock]::Create($defs[0].Extent.Text))",
    "$keyFile='TEST_ONLY_MISSING_KEY';$KeyStore='C:\\fixture';$NodeExe='TEST_ONLY_NODE'",
    "$script:hasRelayEnv=$false;$script:hasClientEnv=$false;$script:trace=$false;$script:writes=0",
    "function Test-Path {param($LiteralPath) if($LiteralPath -eq $keyFile){return $false};if($LiteralPath.EndsWith('client.env.ps1')){return $script:hasClientEnv};return $script:hasRelayEnv}",
    "function Get-ChildItem {param($LiteralPath,[switch]$Force,$ErrorAction) if($script:trace){return 'TEST_ONLY_BACKUP_MARKER'}}",
    "function Set-Content {$script:writes++;throw 'must not reach generation writes'}",
    "$results=@()",
    "foreach($case in @('relay-env-only','client-env-only','backup-or-marker-only')){",
    " $script:hasRelayEnv=$case -eq 'relay-env-only';$script:hasClientEnv=$case -eq 'client-env-only';$script:trace=$case -eq 'backup-or-marker-only';$refused=$false",
    " try{$null=New-OrLoadSigningKey -stateDir 'C:\\fixture\\state'}catch{$refused=$true}",
    " $results+=@{case=$case;refused=$refused}",
    "}",
    "@{results=$results;writes=$script:writes}|ConvertTo-Json -Depth 4 -Compress",
  ].join("\n");
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], {
    env: { ...process.env, PINVOU_TEST_SELFTEST_PATH: selftest }, encoding: "utf8", timeout: 20_000,
  });
  assert.equal(result.status, 0, result.stderr);
  const projected = JSON.parse(result.stdout.trim());
  assert.equal(projected.writes, 0);
  assert.equal(projected.results.length, 3);
  for (const row of projected.results) assert.equal(row.refused, true, row.case);
});
