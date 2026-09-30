import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import yaml from "yaml";

const read = (relative) => readFileSync(new URL(`../../${relative}`, import.meta.url), "utf8");
const orchestrator = read("scripts/run-windows-remote-control-e2e.ps1");
const helper = read("scripts/windows-e2e-orchestrator-helpers.ps1");
const processGuard = read("scripts/windows-e2e-process-guard.ps1");
const workflowText = read(".github/workflows/windows-remote-control-evidence.yml");
const workflow = yaml.parse(workflowText);

test("evidence workflow is protected, fail-closed and validates the protected build identity", () => {
  assert.equal(workflow.permissions.actions, "read");
  const job = workflow.jobs["physical-dual-windows-evidence"];
  assert.equal(job.environment, "windows-release");
  const rendered = JSON.stringify(job.steps);
  assert.equal(job.steps.some((step) => step?.uses?.startsWith("actions/setup-node@")), false);
  assert.doesNotMatch(rendered, /npm(?:\s+--prefix\s+remote-control-relay)?\s+ci/u);
  for (const marker of ["getWorkflowRun", "getWorkflow", "listWorkflowRunArtifacts", "selectProtectedWorkflowArtifact", "artifactDigest", "context.sha", "actions/download-artifact@v8", "if-no-files-found"]) {
    assert.ok(rendered.includes(marker), `missing ${marker}`);
  }
  assert.match(workflowText, /run-windows-remote-control-e2e\.ps1/u);
});

test("orchestrator binds the protected commit and requires two distinct Windows machines", () => {
  assert.match(orchestrator, /ExpectedSourceCommit/u);
  assert.match(orchestrator, /GITHUB_SHA[\s\S]*e2e_source_head_mismatch/u);
  assert.match(orchestrator, /New-PSSession -ComputerName \$AComputerName/u);
  assert.match(orchestrator, /New-PSSession -ComputerName \$BComputerName/u);
  assert.match(orchestrator, /e2e_machines_not_isolated/u);
  assert.match(orchestrator, /start-role[\s\S]*--role/u);
});

test("orchestrator runs the immutable 27-journey catalog and real coordinated CDP phases", () => {
  const catalog = JSON.parse(read("remote-control-relay/docs/schemas/remote-control-journeys.v1.json"));
  assert.equal(catalog.journeys.length, 27);
  assert.deepEqual(catalog.journeys.map((item) => item.id), Array.from({ length: 27 }, (_, index) => `RC-E2E-${String(index + 1).padStart(3, "0")}`));
  assert.match(orchestrator, /catalog\.journeys\.Count -ne 27/u);
  for (const marker of ["journey-prepare", "journey-finalize", "coordination-id", "remote-control-ui-smoke.mjs", "verify-attachment-received", "approval-allow", "approval-deny"]) {
    assert.ok(orchestrator.includes(marker), `missing ${marker}`);
  }
});

test("RC-E2E-011 uses the deadline observation and measured monotonic duration", () => {
  assert.match(orchestrator, /--deadline-observation-base64/u);
  assert.match(orchestrator, /Assert-PinvouJourneyElapsed/u);
  assert.match(orchestrator, /terminal_elapsed_ms/u);
  assert.match(helper, /Math\]::Abs\(\$ObservedMilliseconds - \$TerminalElapsedMilliseconds\)/u);
  assert.doesNotMatch(orchestrator, /ElapsedMilliseconds\s*-le\s*30000/u);
});

test("real legacy relay rollback proves routing, traffic, bare-v2 rejection and restoration", () => {
  for (const marker of ["legacy-start", "legacy-configure-gateway", "legacy-traffic-counter", "legacy-probe", "legacy-bare-v2-probe", "legacy-deconfigure-gateway", "require-public-route-withdrawn", "control_plane_state_after_digest", "legacy-owned-pids", "legacy-stop"]) {
    assert.ok(orchestrator.includes(marker), `missing ${marker}`);
  }
  assert.match(orchestrator, /Get-CimInstance Win32_Process/u);
  assert.match(orchestrator, /Get-NetTCPConnection -OwningProcess/u);
  assert.match(orchestrator, /observed_executable_sha256/u);
});

test("feature-off and release prerequisites are measured on both roles", () => {
  for (const marker of ["verify-feature-off", "successful_socket_count", "relay_allocation_delta", "registry_fingerprint_before", "registry_fingerprint_after", "ui_entry_hidden", "all_commands_fail_closed", "desktop_remote_control_unsupported"]) {
    assert.ok(orchestrator.includes(marker), `missing ${marker}`);
  }
  assert.match(orchestrator, /require-telemetry-thresholds/u);
  assert.match(orchestrator, /require-registry-coverage', '100/u);
  assert.match(orchestrator, /ValidateSet\('official', 'self_hosted'\)/u);
});

test("ordinary path escape, reparse points, hash mismatch and secret environment leakage remain rejected", () => {
  assert.match(orchestrator, /MakeRelativeUri/u);
  assert.match(orchestrator, /ReparsePoint/u);
  assert.match(helper, /e2e_trusted_file_escape/u);
  assert.match(processGuard, /e2e_process_hash_mismatch/u);
  assert.match(processGuard, /e2e_process_environment_sensitive_key_forbidden/u);
});
