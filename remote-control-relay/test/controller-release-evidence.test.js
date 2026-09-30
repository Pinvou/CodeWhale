import assert from "node:assert/strict";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { test } from "node:test";
import { runControllerReleaseGate } from "../scripts/controller-release-gate.mjs";
import { validateMonotonicDeadlineObservation, validateReleaseEvidence } from "../scripts/validate-controller-release-evidence.mjs";
import { providerAttestationPayload } from "../scripts/verify-provider-attestation.mjs";
import { parseSigstoreJsonl, uniqueStatementForDigest, validateGitHubBuildIdentity } from "../scripts/verify-installer-supply-chain.mjs";
import { requireCompleteCoverage } from "../../scripts/check-enrollment-gated-v2-rollback.mjs";

const commit = "b".repeat(40);
const signer = "A".repeat(40);
const publisher = "CN=Pinvou Release Signing";
const hash = (label) => crypto.createHash("sha256").update(label).digest("hex");
const runNonce = hash("explicit-test-run-nonce");
const canonicalInventorySha256 = hash("canonical-inventory");
const canonicalGatewayConfigSha256 = hash("canonical-gateway-config");
const canonicalInputStateDigest = hash(JSON.stringify({ gateway_config_sha256: canonicalGatewayConfigSha256, inventory_sha256: canonicalInventorySha256 }));
const controlPlaneStateDigest = hash("full-control-plane-before");
const originalGatewayStateDigest = hash(JSON.stringify(["pinvou-gateway-state-v1", canonicalInputStateDigest, controlPlaneStateDigest]));
const installerSubjectName = "pinvou3_1.0.0_x64-setup.exe";
const testKeys = crypto.generateKeyPairSync("ed25519");
const testKeysByRole = new Map([["orchestrator", testKeys], ["A", crypto.generateKeyPairSync("ed25519")], ["B", crypto.generateKeyPairSync("ed25519")]]);
const testPublicKeyPem = testKeys.publicKey.export({ type: "spki", format: "pem" });
const testPublicKeySha256 = crypto.createHash("sha256")
  .update(testKeys.publicKey.export({ type: "spki", format: "der" })).digest("hex");
const certificateRoot = await mkdtemp(join(tmpdir(), "pinvou-provider-chain-"));
const openssl = process.platform === "win32" ? "C:\\Program Files\\Git\\usr\\bin\\openssl.exe" : "openssl";
execFileSync(openssl, ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", join(certificateRoot, "root.key"), "-out", join(certificateRoot, "root.pem"), "-subj", "/CN=Pinvou Test Provider Root", "-days", "2"], { stdio: "ignore" });
execFileSync(openssl, ["req", "-newkey", "rsa:2048", "-nodes", "-keyout", join(certificateRoot, "leaf.key"), "-out", join(certificateRoot, "leaf.csr"), "-subj", "/CN=Pinvou Test Provider Verifier"], { stdio: "ignore" });
execFileSync(openssl, ["x509", "-req", "-in", join(certificateRoot, "leaf.csr"), "-CA", join(certificateRoot, "root.pem"), "-CAkey", join(certificateRoot, "root.key"), "-CAcreateserial", "-out", join(certificateRoot, "leaf.pem"), "-days", "2"], { stdio: "ignore" });
const providerRootPem = await readFile(join(certificateRoot, "root.pem"), "utf8");
const providerLeafPem = await readFile(join(certificateRoot, "leaf.pem"), "utf8");
const providerLeafKey = crypto.createPrivateKey(await readFile(join(certificateRoot, "leaf.key"), "utf8"));
const providerRootSha256 = hash(new crypto.X509Certificate(providerRootPem).raw);
const providerLeafSha256 = hash(new crypto.X509Certificate(providerLeafPem).raw);
const testStartedAt = new Date().toISOString();
const testFinishedAt = new Date(Date.parse(testStartedAt) + 3_600_000).toISOString();
const uiActions = {
  settings: ["switch_controller_tab", "submit_invalid_pairing_code", "assert_accessible_error"],
  "controlled-panel": ["refresh_controlled_generation", "assert_controlled_state"],
  "connected-chat": ["focus_unified_chat_composer", "type_connected_draft", "assert_connected_draft"],
  "approval-allow": ["click_permission_allow", "assert_permission_allow_applied"],
  "approval-deny": ["click_permission_deny", "assert_permission_deny_applied"],
  attachment: ["set_attachment_via_cdp", "assert_attachment_selected", "submit_attachment_via_cdp", "assert_attachment_submitted"],
  artifact: ["open_artifact_preview", "assert_artifact_preview", "close_artifact_preview", "download_artifact_expected_failure", "retry_artifact_download", "assert_artifact_recovery_ready"],
};

test("the committed standalone JSON Schema validator is reproducible", () => {
  execFileSync(process.execPath, ["scripts/generate-evidence-schema-validator.mjs", "--check"], {
    cwd: new URL("..", import.meta.url),
    stdio: "pipe",
  });
});

function artifactRole(path) {
  if (path === "build/attestation-provider-A.json") return "A";
  if (path === "build/attestation-provider-B.json") return "B";
  if (path.startsWith("ui/B-")) return "B";
  if (path === "flag-off/B.json") return "B";
  if (path === "checks/RC-E2E-008-controlled_attachment_received.json") return "B";
  if (path === "rollback/legacy-route-spec.json") return "orchestrator";
  if (path.startsWith("ui/A-") || path.startsWith("traces/") || path.startsWith("checks/RC-E2E-") || path.startsWith("rollback/") || path === "flag-off/A.json") return "A";
  return "orchestrator";
}

function signArtifact(artifact) {
  const payload = Buffer.from(JSON.stringify([
    "pinvou-remote-control-artifact-v1", artifact.path, artifact.kind, artifact.sha256,
    artifact.machine_role, commit, runNonce,
  ]), "utf8");
  artifact.signature = crypto.sign(null, payload, testKeysByRole.get(artifact.machine_role).privateKey).toString("base64");
}

const validationOptions = (extra = {}) => ({ expectedCommit: commit, allowTestAttestation: true, ...extra });

test("RC-E2E-011 applies the RTT safety formula and rejects a terminal after its candidate deadline", () => {
  const terminal = {
    clock: "monotonic", rtt_ms: 5_000, deadline_ms: 24_000,
    terminal_elapsed_ms: 23_999, reconnect_extended_deadline: false,
    terminal_state: "peer_unavailable", watchdog_fired: true, task_cancelled: true,
    pending_approvals_rejected: true, credentials_revoked: true, temp_model_cleaned: true,
  };
  assert.equal(validateMonotonicDeadlineObservation(terminal), true);
  for (const observation of [
    { ...terminal, deadline_ms: 24_001 },
    { ...terminal, terminal_elapsed_ms: 24_001 },
    { ...terminal, rtt_ms: 0, deadline_ms: 29_000, terminal_elapsed_ms: 29_001 },
    { ...terminal, terminal_elapsed_ms: 1 },
    { ...terminal, terminal_state: "active" },
  ]) assert.throws(() => validateMonotonicDeadlineObservation(observation), /e2e_monotonic_deadline_invalid/u);
});

test("installer attestation identity is structurally bound to workflow run and source commit", () => {
  const options = {
    repository: "pinvou/pinvou3", workflow: ".github/workflows/windows-remote-control-build-evidence.yml",
    runId: "12345", sourceCommit: commit,
  };
  const statement = {
    _type: "https://in-toto.io/Statement/v1",
    predicate: {
      buildDefinition: {
        buildType: "https://actions.github.io/buildtypes/workflow/v1",
        externalParameters: { workflow: { repository: "https://github.com/pinvou/pinvou3", path: options.workflow } },
        resolvedDependencies: [{ uri: "git+https://github.com/pinvou/pinvou3@refs/heads/main", digest: { gitCommit: commit } }],
      },
      runDetails: { metadata: { invocationId: "https://github.com/pinvou/pinvou3/actions/runs/12345/attempts/1" } },
    },
  };
  assert.equal(validateGitHubBuildIdentity(statement, options), true);
  const wrongRepository = structuredClone(statement); wrongRepository.predicate.buildDefinition.externalParameters.workflow.repository = "https://github.com/attacker/repo";
  const wrongWorkflow = structuredClone(statement); wrongWorkflow.predicate.buildDefinition.externalParameters.workflow.path = ".github/workflows/untrusted.yml";
  const wrongCommit = structuredClone(statement); wrongCommit.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit = "c".repeat(40);
  const wrongRun = structuredClone(statement); wrongRun.predicate.runDetails.metadata.invocationId = "https://github.com/pinvou/pinvou3/actions/runs/54321/attempts/1";
  for (const invalid of [wrongRepository, wrongWorkflow, wrongCommit, wrongRun]) {
    assert.throws(() => validateGitHubBuildIdentity(invalid, options), /installer_attestation_identity_mismatch/u);
  }
});

test("Sigstore download parser consumes JSONL files and requires one matching subject", () => {
  const statement = (digest) => ({ dsseEnvelope: { payloadType: "application/vnd.in-toto+json",
    payload: Buffer.from(JSON.stringify({ subject: [{ digest: { sha256: digest } }] })).toString("base64") } });
  const digest = hash("installer");
  const bundles = parseSigstoreJsonl(`${JSON.stringify(statement(hash("other")))}\n${JSON.stringify(statement(digest))}\n`);
  assert.equal(uniqueStatementForDigest(bundles, digest).subject[0].digest.sha256, digest);
  assert.throws(() => parseSigstoreJsonl("not-json\n"), /installer_attestation_jsonl_invalid/u);
  assert.throws(() => uniqueStatementForDigest([statement(digest), statement(digest)], digest), /installer_attestation_statement_ambiguous/u);
  assert.throws(() => uniqueStatementForDigest(bundles, hash("missing")), /installer_attestation_statement_missing/u);
});

async function validEvidence() {
  const catalog = JSON.parse(await readFile(new URL("../docs/schemas/remote-control-journeys.v1.json", import.meta.url), "utf8"));
  const artifacts = [];
  const addArtifact = (path, kind) => {
    const sha256 = hash(path);
    const artifact = { path, sha256, kind, machine_role: artifactRole(path), signature: "" };
    signArtifact(artifact);
    artifacts.push(artifact);
    return sha256;
  };
  const installerSha = addArtifact("binaries/pinvou-installer.exe", "installer");
  const driverSha = addArtifact("binaries/pinvou-e2e-driver.exe", "driver");
  const legacyRelaySha = addArtifact("binaries/legacy-relay.bin", "legacy-relay");
  const nodeSha = addArtifact("binaries/pinvou-node.exe", "toolchain");
  const legacyRuntimeSha = addArtifact("rollback/legacy-runtime.json", "rollback");
  const legacyRouteSpecSha = addArtifact("rollback/legacy-route-spec.json", "rollback");
  const legacyConfigurationSha = addArtifact("rollback/legacy-configuration.json", "rollback");
  const legacyProbeSha = addArtifact("rollback/legacy-probe.json", "rollback");
  const legacyBareV2Sha = addArtifact("rollback/legacy-bare-v2.json", "rollback");
  const legacyRestorationSha = addArtifact("rollback/legacy-restoration.json", "rollback");
  const providerAttestationShas = new Map([
    ["orchestrator", addArtifact("build/attestation-provider.json", "check")],
    ["A", addArtifact("build/attestation-provider-A.json", "check")],
    ["B", addArtifact("build/attestation-provider-B.json", "check")],
  ]);
  const sbomSha = addArtifact("build/installer-sbom.spdx.json", "log");
  const buildAttestationBundleSha = addArtifact("build/installer-attestation.sigstore.jsonl", "check");
  const sbomAttestationBundleSha = addArtifact("build/installer-sbom-attestation.sigstore.jsonl", "check");
  const buildProvenanceSha = addArtifact("build/installer-provenance.json", "check");
  addArtifact("flag-off/A.json", "check");
  addArtifact("flag-off/B.json", "check");
  const journeys = catalog.journeys.map(({ id, required_checks: requiredChecks }, index) => ({
    id,
    result: "pass",
    started_at: testStartedAt,
    finished_at: testFinishedAt,
    monotonic_start_ms: 1000 + index,
    monotonic_end_ms: id === "RC-E2E-011" ? 24_999 + index : 2000 + index,
    trace_sha256: addArtifact(`traces/${id}.json`, "trace"),
    coordination_digest: new Set(["RC-E2E-006", "RC-E2E-007", "RC-E2E-008"]).has(id)
      ? hash(`coordination-${new Map([["RC-E2E-006", "approval-allow"], ["RC-E2E-007", "approval-deny"], ["RC-E2E-008", "attachment"]]).get(id)}`) : null,
    deadline_observation: id === "RC-E2E-011" ? {
      clock: "monotonic", rtt_ms: 5_000, deadline_ms: 24_000, terminal_elapsed_ms: 23_999,
      reconnect_extended_deadline: false, terminal_state: "peer_unavailable", watchdog_fired: true, task_cancelled: true,
      pending_approvals_rejected: true, credentials_revoked: true, temp_model_cleaned: true,
    } : null,
    checks: requiredChecks.map((checkId) => ({
      id: checkId,
      result: "pass",
      artifact_sha256: addArtifact(`checks/${id}-${checkId}.json`, "check"),
    })),
  }));
  for (const [role, phase] of [
    ["A", "settings"], ["B", "controlled-panel"], ["A", "connected-chat"],
    ["A", "approval-allow"], ["A", "approval-deny"], ["A", "attachment"], ["A", "artifact"],
  ]) addArtifact(`ui/${role}-${phase}.json`, "ui-smoke");
  const gates = Object.fromEntries([
    "feature_flag_off", "sensitive_scan", "telemetry_thresholds",
    "registry_coverage", "rollback_preflight", "ui_smoke",
  ].map((gate) => [gate, {
    result: "pass",
    artifact_sha256: addArtifact(`gates/${gate}.json`, gate === "ui_smoke" ? "ui-smoke" : "check"),
  }]));
  return {
    schema: "pinvou-remote-control-release-evidence",
    version: 1,
    commit_sha: commit,
    provenance: { kind: "physical-dual-windows", orchestrator: "pinvou-windows-e2e-v1", source_commit_sha: commit, synthetic: false,
      node_version: "24.8.0", node_sha256: nodeSha, node_artifact_path: "binaries/pinvou-node.exe",
      node_authenticode_thumbprint: signer, node_publisher: "CN=OpenJS Foundation" },
    attestations: ["orchestrator", "A", "B"].map((role) => {
      const publicKey = testKeysByRole.get(role).publicKey;
      return { role, role_policy_sha256: hash(`role-policy-${role}`), identity: "test", algorithm: "ed25519",
        public_key_spki_pem: publicKey.export({ type: "spki", format: "pem" }), public_key_sha256: hash(publicKey.export({ type: "spki", format: "der" })),
        run_nonce: runNonce, provider: "remote_signing_service", key_id: `test-key-id/${role}`, non_exportable: true,
        acl_attestation_sha256: hash("test-acl"), provider_measurement_sha256: hash("test-provider-measurement"), provider_vendor: "Pinvou Test Vendor",
        provider_root_certificate_sha256: providerRootSha256, provider_verifier_certificate_sha256: providerLeafSha256,
        provider_attestation_artifact_sha256: providerAttestationShas.get(role) };
    }),
    relay: {
      version: "1.0.0",
      config_fingerprint: hash("relay-config"),
      public_origin_digest: hash("relay-origin"),
      registry_coverage_percent: 100,
      production_equivalent: true,
      enrollment_deployment: "official",
    },
    legacy_relay: {
      version: "0.9.0", sha256: legacyRelaySha, artifact_path: "binaries/legacy-relay.bin",
      endpoint_inventory_sha256: hash("external-private-inventory"),
      gateway_config_sha256: hash("external-protected-gateway-config"),
      pid: 1234, listener_host: "127.0.0.1", listener_port: 28741,
      route_id: "legacy-primary", route_spec_artifact_sha256: legacyRouteSpecSha,
      runtime_artifact_sha256: legacyRuntimeSha, configuration_artifact_sha256: legacyConfigurationSha, probe_artifact_sha256: legacyProbeSha,
      bare_v2_artifact_sha256: legacyBareV2Sha, restoration_artifact_sha256: legacyRestorationSha,
      gateway_url_sha256: hash("legacy-gateway"), request_nonce: hash("legacy-request"), traffic_counter_before: 41, traffic_counter_after: 42,
      gateway_traffic_observed: true, response_correlated: true, bare_v2_rejected: true, route_restored: true,
    },
    driver: {
      version: "1.0.0", sha256: driverSha, authenticode_thumbprint: signer,
      publisher, artifact_path: "binaries/pinvou-e2e-driver.exe", source_commit_sha: commit,
      attestation_public_key_sha256: testPublicKeySha256, attestation_provider: "remote_signing_service", attestation_key_id: "test-key-id/orchestrator", private_key_embedded: false,
    },
    installers: [
      { role: "A", version: "1.0.0", sha256: installerSha, authenticode_thumbprint: signer, publisher, artifact_path: "binaries/pinvou-installer.exe", attested_subject_name: installerSubjectName, sbom_attested_subject_name: "installer-sbom.spdx.json", source_commit_sha: commit, build_provenance_artifact_sha256: buildProvenanceSha, build_attestation_bundle_artifact_sha256: buildAttestationBundleSha, sbom_artifact_sha256: sbomSha, sbom_attestation_bundle_artifact_sha256: sbomAttestationBundleSha },
      { role: "B", version: "1.0.0", sha256: installerSha, authenticode_thumbprint: signer, publisher, artifact_path: "binaries/pinvou-installer.exe", attested_subject_name: installerSubjectName, sbom_attested_subject_name: "installer-sbom.spdx.json", source_commit_sha: commit, build_provenance_artifact_sha256: buildProvenanceSha, build_attestation_bundle_artifact_sha256: buildAttestationBundleSha, sbom_artifact_sha256: sbomSha, sbom_attestation_bundle_artifact_sha256: sbomAttestationBundleSha },
    ],
    machines: [
      { role: "A", identity_digest: hash("physical-machine-a"), windows_build: "26100" },
      { role: "B", identity_digest: hash("physical-machine-b"), windows_build: "26100" },
    ],
    started_at: testStartedAt,
    finished_at: testFinishedAt,
    monotonic_start_ms: 1000,
    monotonic_end_ms: 3_601_000,
    journeys,
    artifacts,
    gates,
  };
}

async function materializeEvidence(evidence, root) {
  const uiReferences = evidence.artifacts
    .filter((artifact) => artifact.path.startsWith("ui/"))
    .map((artifact) => {
      const match = /^ui\/(A|B)-(.+)\.json$/u.exec(artifact.path);
      return { role: match[1], phase: match[2], sha256: artifact.sha256 };
    });
  const replacements = new Map();
  for (const [index, artifact] of evidence.artifacts.entries()) {
    if (artifact.path === "gates/ui_smoke.json") continue;
    const ui = uiReferences.find((item) => artifact.sha256 === item.sha256);
    let content = `{"status":"pass","ordinal":${index}}\n`;
    if (ui) {
      const value = { schema: "pinvou-remote-control-ui-smoke", version: 1, phase: ui.phase, result: "pass", checks: ["real_tauri_page", `phase_${ui.phase.replaceAll("-", "_")}`], actions: uiActions[ui.phase] };
      if (["approval-allow", "approval-deny", "attachment"].includes(ui.phase)) value.coordination_digest = hash(`coordination-${ui.phase}`);
      if (ui.phase === "attachment") value.attachment_sha256 = hash("attachment-fixture");
      content = `${JSON.stringify(value)}\n`;
    }
    if (artifact.path.startsWith("flag-off/")) {
      const role = artifact.path.includes("/A.") ? "A" : "B";
      content = `${JSON.stringify({ schema: "pinvou-remote-control-feature-off-observation", version: 1, role, source_commit_sha: commit, successful_socket_count: 0, relay_allocation_delta: 0, registry_before_sha256: hash(`registry-${role}`), registry_after_sha256: hash(`registry-${role}`), tombstone_before_sha256: hash(`tombstone-${role}`), tombstone_after_sha256: hash(`tombstone-${role}`), relay_rejected: true, ui_entry_hidden: true, registered_command_count: 34, all_commands_fail_closed: true, error_code: "desktop_remote_control_unsupported", mock: false })}\n`;
    }
    if (artifact.path === "rollback/legacy-runtime.json") content = `${JSON.stringify({ schema: "pinvou-legacy-relay-runtime-evidence", version: 1, source_commit_sha: commit, pid: 1234, relay_version: "0.9.0", binary_sha256: replacements.get(evidence.legacy_relay.sha256), executable_sha256: replacements.get(evidence.legacy_relay.sha256), executable_verified: true, listener_scope: "loopback", listener_port: 28741, listener_owner_pid: 1234, process_started: true, mock: false })}\n`;
    if (artifact.path === "rollback/legacy-route-spec.json") content = `${JSON.stringify({ schema: "pinvou-legacy-gateway-route-spec", version: 1, run_nonce: runNonce, route_id: "legacy-primary", gateway_url_sha256: evidence.legacy_relay.gateway_url_sha256, target: "legacy_relay", no_bypass: true, endpoint_inventory_sha256: evidence.legacy_relay.endpoint_inventory_sha256, gateway_config_sha256: evidence.legacy_relay.gateway_config_sha256, canonical_inventory_sha256: canonicalInventorySha256, canonical_gateway_config_sha256: canonicalGatewayConfigSha256, canonical_input_state_digest: canonicalInputStateDigest })}\n`;
    if (artifact.path === "rollback/legacy-configuration.json") content = `${JSON.stringify({ schema: "pinvou-legacy-relay-configuration-evidence", version: 1, source_commit_sha: commit, run_nonce: runNonce, pid: 1234, route_id: "legacy-primary", no_bypass: true, gateway_url_sha256: evidence.legacy_relay.gateway_url_sha256, gateway_config_sha256: evidence.legacy_relay.gateway_config_sha256, endpoint_inventory_sha256: evidence.legacy_relay.endpoint_inventory_sha256, canonical_input_state_digest: canonicalInputStateDigest, control_plane_state_before_digest: controlPlaneStateDigest, original_state_digest: originalGatewayStateDigest, route_state_digest: hash("legacy-active-route"), listener_url_sha256: hash("legacy-listener"), listener_port: 28741, route_activated: true, mock: false })}\n`;
    if (artifact.path === "rollback/legacy-probe.json") content = `${JSON.stringify({ schema: "pinvou-legacy-relay-probe-evidence", version: 1, source_commit_sha: commit, run_nonce: runNonce, pid: 1234, route_id: "legacy-primary", no_bypass: true, gateway_url_sha256: evidence.legacy_relay.gateway_url_sha256, gateway_config_sha256: evidence.legacy_relay.gateway_config_sha256, endpoint_inventory_sha256: evidence.legacy_relay.endpoint_inventory_sha256, route_state_digest: hash("legacy-active-route"), listener_url_sha256: hash("legacy-listener"), listener_port: 28741, request_nonce: evidence.legacy_relay.request_nonce, traffic_counter_before: 41, traffic_counter_after: 42, response_nonce: evidence.legacy_relay.request_nonce, gateway_traffic_observed: true, listener_flow_observed: true, response_correlated: true, mock: false })}\n`;
    if (artifact.path === "rollback/legacy-bare-v2.json") content = `${JSON.stringify({ schema: "pinvou-legacy-bare-v2-rejection", version: 1, source_commit_sha: commit, run_nonce: runNonce, role: "A", route_id: "legacy-primary", gateway_url_sha256: evidence.legacy_relay.gateway_url_sha256, request_nonce: evidence.legacy_relay.request_nonce, response_kind: "upgrade_rejected", rejected: true, mock: false })}\n`;
    if (artifact.path === "rollback/legacy-restoration.json") content = `${JSON.stringify({ schema: "pinvou-legacy-gateway-restoration-evidence", version: 1, source_commit_sha: commit, run_nonce: runNonce, route_id: "legacy-primary", owned_route_removed: true, public_route_withdrawn: true, canonical_input_state_digest: canonicalInputStateDigest, control_plane_state_before_digest: controlPlaneStateDigest, control_plane_state_after_digest: controlPlaneStateDigest, original_state_digest: originalGatewayStateDigest, restored_state_digest: originalGatewayStateDigest, mock: false })}\n`;
    if (artifact.path === "checks/RC-E2E-008-controlled_attachment_received.json") content = `${JSON.stringify({ schema: "pinvou-controlled-attachment-receipt", version: 1, source_commit_sha: commit, run_nonce: runNonce, role: "B", coordination_digest: hash("coordination-attachment"), attachment_sha256: hash("attachment-fixture"), received: true, mock: false })}\n`;
    if (artifact.path.startsWith("build/attestation-provider")) {
      const role = artifact.path.endsWith("-A.json") ? "A" : artifact.path.endsWith("-B.json") ? "B" : "orchestrator";
      const identity = evidence.attestations.find((item) => item.role === role);
      const receipt = { schema: "pinvou-provider-attestation-receipt", version: 1, role, role_policy_sha256: identity.role_policy_sha256, provider: "remote_signing_service", key_id: identity.key_id, run_nonce: runNonce, public_key_spki_pem: identity.public_key_spki_pem, public_key_sha256: identity.public_key_sha256, non_exportable: true, acl_attestation_sha256: hash("test-acl"), provider_measurement_sha256: hash("test-provider-measurement"), driver_sha256: replacements.get(evidence.driver.sha256), issued_at: testStartedAt, verifier: { kind: "independent-provider-verifier", vendor: "Pinvou Test Vendor", leaf_certificate_pem: providerLeafPem, certificate_chain_pem: [providerRootPem], signature_algorithm: "sha256", signature: "" } };
      receipt.verifier.signature = crypto.sign("sha256", providerAttestationPayload(receipt), providerLeafKey).toString("base64");
      content = `${JSON.stringify(receipt)}\n`;
    }
    if (artifact.path === "build/installer-sbom.spdx.json") {
      const stagedFiles = ["nsis-stage/resources/common/test.json", "nsis-stage/manifest.json", "nsis-stage/tauri.nsis-stage.conf.json", "nsis-stage/installer-hooks.nsh", "bundle/pinvou3-tauri.exe"];
      const resourceEntries = stagedFiles.map((fileName, stagedIndex) => ({ SPDXID: `SPDXRef-Staged-${stagedIndex}`, fileName, checksums: [{ algorithm: "SHA256", checksumValue: hash(fileName) }], ...(fileName.startsWith("nsis-stage/resources/") ? { fileComment: "pinvou_stage_category=staged-resource;mapping_source=private-runtime;mapping_destination=runtime" } : {}) }));
      content = `${JSON.stringify({ spdxVersion: "SPDX-2.3", dataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT", name: "Pinvou Agent", documentNamespace: `https://github.com/pinvou/pinvou3/spdx/${commit}`, documentComment: `source_commit_sha=${commit};dependency_inventory_sha256=${hash("deps")};staging_manifest_sha256=${hash("manifest")};staged_file_set_sha256=${hash("staged-files")};mapping_inventory_sha256=${hash("mappings")};dependency_count=1;staged_file_count=${stagedFiles.length}`, creationInfo: { created: testStartedAt, creators: ["Tool: pinvou-test"] }, packages: [{ SPDXID: "SPDXRef-Package-Pinvou", name: "Pinvou Agent" }, { SPDXID: "SPDXRef-Dependency-Test", name: "test-dependency" }], files: [{ SPDXID: "SPDXRef-Installer", fileName: installerSubjectName, checksums: [{ algorithm: "SHA256", checksumValue: replacements.get(evidence.installers[0].sha256) }] }, ...resourceEntries], relationships: [{ spdxElementId: "SPDXRef-DOCUMENT", relationshipType: "DESCRIBES", relatedSpdxElement: "SPDXRef-Package-Pinvou" }, { spdxElementId: "SPDXRef-Package-Pinvou", relationshipType: "CONTAINS", relatedSpdxElement: "SPDXRef-Installer" }, { spdxElementId: "SPDXRef-Package-Pinvou", relationshipType: "DEPENDS_ON", relatedSpdxElement: "SPDXRef-Dependency-Test" }, ...resourceEntries.map((resource) => ({ spdxElementId: "SPDXRef-Package-Pinvou", relationshipType: "CONTAINS", relatedSpdxElement: resource.SPDXID }))] })}\n`;
    }
    if (artifact.path === "build/installer-attestation.sigstore.jsonl" || artifact.path === "build/installer-sbom-attestation.sigstore.jsonl") content = `${JSON.stringify({ mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", verificationMaterial: {}, dsseEnvelope: { payloadType: "application/vnd.in-toto+json", payload: Buffer.from(JSON.stringify({ _type: "https://in-toto.io/Statement/v1", subject: [{ name: artifact.path.includes("sbom-attestation") ? "installer-sbom.spdx.json" : installerSubjectName, digest: { sha256: replacements.get(artifact.sha256) } }], predicate: { buildDefinition: { buildType: "https://actions.github.io/buildtypes/workflow/v1", externalParameters: { workflow: { repository: "https://github.com/pinvou/pinvou3", path: ".github/workflows/windows-remote-control-build-evidence.yml" } }, resolvedDependencies: [{ uri: "git+https://github.com/pinvou/pinvou3@refs/heads/main", digest: { gitCommit: commit } }] }, runDetails: { metadata: { invocationId: "https://github.com/pinvou/pinvou3/actions/runs/12345/attempts/1" } } } })).toString("base64"), signatures: [] } })}\n`;
    if (artifact.path === "build/installer-provenance.json") content = `${JSON.stringify({ schema: "pinvou-installer-supply-chain-verification", version: 1, source_commit_sha: commit, installer_sha256: replacements.get(evidence.installers[0].sha256), attestation_bundle_sha256: replacements.get(evidence.installers[0].build_attestation_bundle_artifact_sha256), sbom_sha256: replacements.get(evidence.installers[0].sbom_artifact_sha256), sbom_attestation_bundle_sha256: replacements.get(evidence.installers[0].sbom_attestation_bundle_artifact_sha256), installer_subject_name: installerSubjectName, sbom_subject_name: "installer-sbom.spdx.json", repository: "pinvou/pinvou3", workflow: ".github/workflows/windows-remote-control-build-evidence.yml", build_run_id: 12345, protected_environment: "windows-release", verification_tool: "gh-attestation-verify", mock: false })}\n`;
    const path = join(root, ...artifact.path.split("/"));
    await mkdir(dirname(path), { recursive: true });
    await writeFile(path, content, "utf8");
    const actual = crypto.createHash("sha256").update(content).digest("hex");
    replacements.set(artifact.sha256, actual);
    artifact.sha256 = actual;
  }
  for (const item of uiReferences) item.sha256 = replacements.get(item.sha256);
  for (const item of evidence.installers) item.sha256 = replacements.get(item.sha256);
  evidence.driver.sha256 = replacements.get(evidence.driver.sha256);
  evidence.provenance.node_sha256 = replacements.get(evidence.provenance.node_sha256);
  evidence.legacy_relay.sha256 = replacements.get(evidence.legacy_relay.sha256);
  evidence.legacy_relay.runtime_artifact_sha256 = replacements.get(evidence.legacy_relay.runtime_artifact_sha256);
  evidence.legacy_relay.route_spec_artifact_sha256 = replacements.get(evidence.legacy_relay.route_spec_artifact_sha256);
  evidence.legacy_relay.configuration_artifact_sha256 = replacements.get(evidence.legacy_relay.configuration_artifact_sha256);
  evidence.legacy_relay.probe_artifact_sha256 = replacements.get(evidence.legacy_relay.probe_artifact_sha256);
  evidence.legacy_relay.bare_v2_artifact_sha256 = replacements.get(evidence.legacy_relay.bare_v2_artifact_sha256);
  evidence.legacy_relay.restoration_artifact_sha256 = replacements.get(evidence.legacy_relay.restoration_artifact_sha256);
  for (const item of evidence.attestations) item.provider_attestation_artifact_sha256 = replacements.get(item.provider_attestation_artifact_sha256);
  for (const item of evidence.installers) {
    item.build_provenance_artifact_sha256 = replacements.get(item.build_provenance_artifact_sha256);
    item.build_attestation_bundle_artifact_sha256 = replacements.get(item.build_attestation_bundle_artifact_sha256);
    item.sbom_artifact_sha256 = replacements.get(item.sbom_artifact_sha256);
    item.sbom_attestation_bundle_artifact_sha256 = replacements.get(item.sbom_attestation_bundle_artifact_sha256);
  }
  for (const journey of evidence.journeys) {
    journey.trace_sha256 = replacements.get(journey.trace_sha256);
    for (const check of journey.checks) check.artifact_sha256 = replacements.get(check.artifact_sha256);
  }
  for (const [name, gate] of Object.entries(evidence.gates)) {
    if (name !== "ui_smoke" && name !== "feature_flag_off") gate.artifact_sha256 = replacements.get(gate.artifact_sha256);
  }
  const featureArtifact = evidence.artifacts.find((artifact) => artifact.path === "gates/feature_flag_off.json");
  const featureRoles = ["A", "B"].map((role) => ({
    role,
    sha256: evidence.artifacts.find((artifact) => artifact.path === `flag-off/${role}.json`).sha256,
    successful_socket_count: 0, relay_allocation_delta: 0, state_unchanged: true, relay_rejected: true, ui_entry_hidden: true, command_surface_checked: true,
  }));
  const featureContent = `${JSON.stringify({ schema: "pinvou-remote-control-feature-off-suite", version: 1, result: "pass", source_commit_sha: commit, relay_rejected: true, roles: featureRoles })}\n`;
  const featurePath = join(root, ...featureArtifact.path.split("/"));
  await mkdir(dirname(featurePath), { recursive: true });
  await writeFile(featurePath, featureContent, "utf8");
  featureArtifact.sha256 = crypto.createHash("sha256").update(featureContent).digest("hex");
  evidence.gates.feature_flag_off.artifact_sha256 = featureArtifact.sha256;
  const uiSummaryArtifact = evidence.artifacts.find((artifact) => artifact.path === "gates/ui_smoke.json");
  const summaryContent = `${JSON.stringify({ schema: "pinvou-remote-control-ui-suite", version: 1, result: "pass", artifacts: uiReferences })}\n`;
  const summaryPath = join(root, ...uiSummaryArtifact.path.split("/"));
  await mkdir(dirname(summaryPath), { recursive: true });
  await writeFile(summaryPath, summaryContent, "utf8");
  uiSummaryArtifact.sha256 = crypto.createHash("sha256").update(summaryContent).digest("hex");
  evidence.gates.ui_smoke.artifact_sha256 = uiSummaryArtifact.sha256;
  for (const artifact of evidence.artifacts) signArtifact(artifact);
}

test("strict evidence validator accepts the exact 27 stable journey IDs", async () => {
  const result = await validateReleaseEvidence(await validEvidence(), validationOptions());
  assert.equal(result.ok, true);
  assert.equal(result.journey_count, 27);
  assert.ok(result.artifact_count > 60);
});

test("validator executes the published draft-2020-12 JSON Schema before semantic checks", async () => {
  const evidence = await validEvidence();
  evidence.relay.unpublished_field = true;
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions()),
    (error) => error?.code === "evidence_schema_invalid",
  );
});

test("validator binds the signed driver/installers and private inventory fingerprint to release policy", async () => {
  const evidence = await validEvidence();
  await assert.doesNotReject(() => validateReleaseEvidence(evidence, validationOptions({
    expectedSignerThumbprint: signer,
    expectedSignerPublisher: publisher,
    expectedInventorySha256: evidence.legacy_relay.endpoint_inventory_sha256,
    expectedGatewayConfigSha256: evidence.legacy_relay.gateway_config_sha256,
    expectedDriverSha256: evidence.driver.sha256,
    expectedNodeSha256: evidence.provenance.node_sha256,
    expectedNodeSignerThumbprint: evidence.provenance.node_authenticode_thumbprint,
    expectedNodeSignerPublisher: evidence.provenance.node_publisher,
    expectedInstallerSha256: evidence.installers[0].sha256,
    expectedInstallerVersion: evidence.installers[0].version,
    expectedAttestationProvider: "remote_signing_service",
    expectedAttestationKeyId: "test-key-id/orchestrator",
    expectedAttestationAclSha256: evidence.attestations[0].acl_attestation_sha256,
    expectedAttestationProviderMeasurementSha256: evidence.attestations[0].provider_measurement_sha256,
    expectedBuildProvenanceSha256: evidence.installers[0].build_provenance_artifact_sha256,
  })));
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedSignerThumbprint: "F".repeat(40) })),
    (error) => error?.code === "evidence_signer_mismatch",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedDriverSha256: "f".repeat(64) })),
    (error) => error?.code === "evidence_driver_digest_mismatch",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedNodeSha256: "f".repeat(64) })),
    (error) => error?.code === "evidence_node_digest_mismatch",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedNodeSignerPublisher: "CN=Untrusted Node" })),
    (error) => error?.code === "evidence_node_signer_mismatch",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedInstallerSha256: "f".repeat(64) })),
    (error) => error?.code === "evidence_installer_digest_mismatch",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedAttestationProvider: "unsupported_provider" })),
    (error) => error?.code === "evidence_attestation_provider_untrusted",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedBuildProvenanceSha256: "f".repeat(64) })),
    (error) => error?.code === "evidence_installer_provenance_untrusted",
  );
});

test("production validation rejects the explicit test attestation identity and untrusted keys", async () => {
  const evidence = await validEvidence();
  await assert.rejects(
    () => validateReleaseEvidence(evidence, { expectedCommit: commit }),
    (error) => error?.code === "evidence_attestation_identity_untrusted",
  );
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ expectedAttestationPublicKeySha256: "f".repeat(64) })),
    (error) => error?.code === "evidence_attestation_key_untrusted",
  );
  for (const attestation of evidence.attestations) attestation.identity = "production";
  await assert.rejects(
    () => validateReleaseEvidence(evidence, { expectedCommit: commit }),
    (error) => error?.code === "evidence_attestation_key_untrusted",
  );
  await assert.doesNotReject(() => validateReleaseEvidence(evidence, {
    expectedCommit: commit, expectedAttestationPublicKeySha256: testPublicKeySha256,
  }));
  evidence.artifacts[0].signature = evidence.artifacts[1].signature;
  await assert.rejects(
    () => validateReleaseEvidence(evidence, { expectedCommit: commit, expectedAttestationPublicKeySha256: testPublicKeySha256 }),
    (error) => error?.code === "evidence_artifact_signature_invalid",
  );
});

test("evidence validator rejects missing, reordered, blocked, stale and sensitive evidence", async () => {
  for (const mutate of [
    (value) => { value.journeys.pop(); },
    (value) => { [value.journeys[0], value.journeys[1]] = [value.journeys[1], value.journeys[0]]; },
    (value) => { value.journeys[4].result = "blocked"; },
    (value) => { value.commit_sha = "c".repeat(40); },
    (value) => { value.prompt = "forbidden"; },
    (value) => { value.journeys[0].trace_sha256 = "c".repeat(64); },
    (value) => { value.journeys[20].checks.pop(); },
    (value) => { value.relay.registry_coverage_percent = 99; },
    (value) => { value.machines[1].identity_digest = value.machines[0].identity_digest; },
    (value) => { value.journeys[0].checks[0].artifact_sha256 = value.journeys[0].trace_sha256; },
    (value) => { value.provenance.synthetic = true; },
    (value) => { value.attestations[0].non_exportable = false; },
    (value) => { value.attestations[2].role_policy_sha256 = value.attestations[1].role_policy_sha256; },
    (value) => { value.driver.private_key_embedded = true; },
    (value) => { value.installers[0].source_commit_sha = "c".repeat(40); },
    (value) => { value.legacy_relay.traffic_counter_after = value.legacy_relay.traffic_counter_before; },
  ]) {
    const evidence = await validEvidence();
    mutate(evidence);
    await assert.rejects(() => validateReleaseEvidence(evidence, validationOptions()));
  }
});

test("RC-E2E-011 rejects a driver self-report that returned immediately", async () => {
  const evidence = await validEvidence();
  const journey = evidence.journeys.find((item) => item.id === "RC-E2E-011");
  journey.monotonic_end_ms = journey.monotonic_start_ms + 1_000;
  await assert.rejects(() => validateReleaseEvidence(evidence, validationOptions()),
    (error) => error?.code === "e2e_monotonic_elapsed_mismatch");
});

test("artifact signatures are bound to the signer for their machine role", async () => {
  const evidence = await validEvidence();
  const artifactA = evidence.artifacts.find((item) => item.machine_role === "A");
  const payload = Buffer.from(JSON.stringify(["pinvou-remote-control-artifact-v1", artifactA.path, artifactA.kind,
    artifactA.sha256, artifactA.machine_role, commit, runNonce]), "utf8");
  artifactA.signature = crypto.sign(null, payload, testKeysByRole.get("B").privateKey).toString("base64");
  await assert.rejects(() => validateReleaseEvidence(evidence, validationOptions()),
    (error) => error?.code === "evidence_artifact_signature_invalid");
});

test("release gate is fail-closed when enabled and evidence configuration is absent", async () => {
  await assert.rejects(
    () => runControllerReleaseGate({ PINVOU_DESKTOP_CONTROLLER_ENABLED: "1" }),
    (error) => error?.code === "release_evidence_configuration_missing",
  );
  assert.deepEqual(await runControllerReleaseGate({ PINVOU_DESKTOP_CONTROLLER_ENABLED: "0" }), {
    ok: true, enabled: false, evidence_required: false,
  });
  await assert.rejects(
    () => runControllerReleaseGate({ PINVOU_DESKTOP_CONTROLLER_ENABLED: "true" }),
    (error) => error?.code === "release_feature_flag_invalid",
  );
});

test("legacy rollback is safe only at exactly 100 percent coverage with zero misses", () => {
  assert.doesNotThrow(() => requireCompleteCoverage({ coverage_percent: 100, not_migrated: 0 }));
  for (const result of [
    { coverage_percent: 99, not_migrated: 1 },
    { coverage_percent: 100, not_migrated: 1 },
    { coverage_percent: "100", not_migrated: 0 },
  ]) assert.throws(() => requireCompleteCoverage(result), /legacy_endpoint_coverage_incomplete/u);
});

test("validator re-hashes evidence files and rejects sensitive trace content", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-release-evidence-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const evidence = await validEvidence();
  await materializeEvidence(evidence, root);
  await assert.doesNotReject(() => validateReleaseEvidence(evidence, validationOptions({
    evidenceRoot: root,
    expectedBuildRepository: "pinvou/pinvou3",
    expectedBuildWorkflow: ".github/workflows/windows-remote-control-build-evidence.yml",
    expectedBuildRunId: "12345",
  })));
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ evidenceRoot: root, expectedBuildRunId: "54321" })),
    (error) => error?.code === "evidence_installer_provenance_untrusted",
  );
  const leaked = '{"prompt":"private body"}\n';
  const traceSha = evidence.journeys[0].trace_sha256;
  const traceArtifact = evidence.artifacts.find((artifact) => artifact.sha256 === traceSha);
  await writeFile(join(root, ...traceArtifact.path.split("/")), leaked, "utf8");
  traceArtifact.sha256 = crypto.createHash("sha256").update(leaked).digest("hex");
  evidence.journeys[0].trace_sha256 = traceArtifact.sha256;
  signArtifact(traceArtifact);
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ evidenceRoot: root })),
    (error) => error?.code === "evidence_artifact_sensitive_content",
  );
});

test("validator rejects a self-reported UI gate unless all seven direct CDP phases and actions are present", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-release-ui-evidence-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const evidence = await validEvidence();
  await materializeEvidence(evidence, root);
  const summaryArtifact = evidence.artifacts.find((artifact) => artifact.path === "gates/ui_smoke.json");
  const summaryPath = join(root, ...summaryArtifact.path.split("/"));
  const summary = JSON.parse(await readFile(summaryPath, "utf8"));
  summary.artifacts.pop();
  const forged = `${JSON.stringify(summary)}\n`;
  await writeFile(summaryPath, forged, "utf8");
  summaryArtifact.sha256 = crypto.createHash("sha256").update(forged).digest("hex");
  evidence.gates.ui_smoke.artifact_sha256 = summaryArtifact.sha256;
  signArtifact(summaryArtifact);
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ evidenceRoot: root })),
    (error) => error?.code === "evidence_ui_smoke_invalid",
  );
});

test("validator rejects an artifact path that escapes through a symlink or junction", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-release-path-evidence-"));
  const outside = await mkdtemp(join(tmpdir(), "pinvou-release-path-outside-"));
  t.after(() => Promise.all([rm(root, { recursive: true, force: true }), rm(outside, { recursive: true, force: true })]));
  const evidence = await validEvidence();
  await materializeEvidence(evidence, root);
  await rm(join(root, "traces"), { recursive: true, force: true });
  await symlink(outside, join(root, "traces"), process.platform === "win32" ? "junction" : "dir");
  await assert.rejects(
    () => validateReleaseEvidence(evidence, validationOptions({ evidenceRoot: root })),
    (error) => error?.code === "evidence_artifact_path_escape",
  );
});

test("legacy route spec, active configuration and restoration share one original-state digest", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-route-binding-")); t.after(() => rm(root, { recursive: true, force: true }));
  const evidence = await validEvidence(); await materializeEvidence(evidence, root);
  const artifact = evidence.artifacts.find((item) => item.path === "rollback/legacy-route-spec.json");
  const path = join(root, ...artifact.path.split("/")); const route = JSON.parse(await readFile(path, "utf8"));
  route.canonical_input_state_digest = hash("wrong-canonical-input"); const content = `${JSON.stringify(route)}\n`; await writeFile(path, content);
  const previous = artifact.sha256; artifact.sha256 = crypto.createHash("sha256").update(content).digest("hex");
  evidence.legacy_relay.route_spec_artifact_sha256 = artifact.sha256;
  for (const attestation of evidence.attestations) if (attestation.provider_attestation_artifact_sha256 === previous) attestation.provider_attestation_artifact_sha256 = artifact.sha256;
  signArtifact(artifact);
  await assert.rejects(() => validateReleaseEvidence(evidence, validationOptions({ evidenceRoot: root })),
    (error) => error?.code === "evidence_legacy_relay_invalid");
});

test("catalog and JSON Schema remain a strict 27-journey contract", async () => {
  const schema = JSON.parse(await readFile(new URL("../docs/schemas/remote-control-release-evidence.v1.schema.json", import.meta.url), "utf8"));
  const catalog = JSON.parse(await readFile(new URL("../docs/schemas/remote-control-journeys.v1.json", import.meta.url), "utf8"));
  assert.equal(schema.properties.journeys.minItems, 27);
  assert.equal(schema.properties.journeys.maxItems, 27);
  assert.deepEqual(catalog.journeys.map(({ id }) => id),
    Array.from({ length: 27 }, (_, index) => `RC-E2E-${String(index + 1).padStart(3, "0")}`));
});
