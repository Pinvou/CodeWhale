#!/usr/bin/env node
import crypto from "node:crypto";
import { lstat, readFile, realpath } from "node:fs/promises";
import { dirname, isAbsolute, normalize, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import validateEvidenceSchema from "../docs/schemas/remote-control-release-evidence.v1.validator.mjs";
import { verifyProviderAttestation } from "./verify-provider-attestation.mjs";
import { validateSpdx } from "./verify-installer-supply-chain.mjs";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const CATALOG_PATH = resolve(SCRIPT_DIR, "../docs/schemas/remote-control-journeys.v1.json");
const SCHEMA_PATH = resolve(SCRIPT_DIR, "../docs/schemas/remote-control-release-evidence.v1.schema.json");
const EVIDENCE_SCHEMA = JSON.parse(await readFile(SCHEMA_PATH, "utf8"));
if (EVIDENCE_SCHEMA.$id !== "https://pinvou.com/schemas/remote-control-release-evidence-v1.json") {
  throw new Error("evidence_schema_identity_invalid");
}
const SHA1 = /^[a-f0-9]{40}$/u;
const SHA256 = /^[a-f0-9]{64}$/u;
const THUMBPRINT = /^[A-F0-9]{40,64}$/u;
const VERSION = /^[0-9]+\.[0-9]+\.[0-9]+(?:\.[0-9]+)?$/u;
const SAFE_VERSION = /^[A-Za-z0-9._+-]{1,64}$/u;
const WINDOWS_BUILD = /^[0-9]{4,6}$/u;
const SAFE_ID = /^[a-z][a-z0-9_]{2,63}$/u;
const SAFE_PATH = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$/u;
const ALLOWED_ARTIFACT_KINDS = new Set([
  "trace", "check", "log", "ui-smoke", "rollback", "installer", "driver",
  "legacy-relay", "toolchain",
]);
const TEXT_ARTIFACT_KINDS = new Set(["trace", "check", "log", "ui-smoke", "rollback"]);
const ROLES = new Set(["A", "B"]);
const ARTIFACT_ROLES = new Set(["A", "B", "orchestrator"]);
const FORBIDDEN_KEY = /(pairing|connection|lease|resume_token|role_token|access_token|auth_token|cookie|secret|prompt|reply|content|device_name|host_name|ip_address|file_name|file_path)/iu;
const REQUIRED_GATES = [
  "feature_flag_off", "sensitive_scan", "telemetry_thresholds",
  "registry_coverage", "rollback_preflight", "ui_smoke",
];
const REQUIRED_UI_PHASES = new Map([
  ["settings", "A"], ["controlled-panel", "B"], ["connected-chat", "A"],
  ["approval-allow", "A"], ["approval-deny", "A"], ["attachment", "A"], ["artifact", "A"],
]);
const REQUIRED_UI_ACTIONS = new Map([
  ["settings", ["switch_controller_tab", "submit_invalid_pairing_code", "assert_accessible_error"]],
  ["controlled-panel", ["refresh_controlled_generation", "assert_controlled_state"]],
  ["connected-chat", ["focus_unified_chat_composer", "type_connected_draft", "assert_connected_draft"]],
  ["approval-allow", ["click_permission_allow", "assert_permission_allow_applied"]],
  ["approval-deny", ["click_permission_deny", "assert_permission_deny_applied"]],
  ["attachment", ["set_attachment_via_cdp", "assert_attachment_selected", "submit_attachment_via_cdp", "assert_attachment_submitted"]],
  ["artifact", ["open_artifact_preview", "assert_artifact_preview", "close_artifact_preview", "download_artifact_expected_failure", "retry_artifact_download", "assert_artifact_recovery_ready"]],
]);

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}
function exactObject(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || Object.getPrototypeOf(value) !== Object.prototype
    || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) fail(code);
  for (const key of Object.keys(value)) if (FORBIDDEN_KEY.test(key)) fail("evidence_sensitive_field_forbidden");
  return value;
}

function timestamp(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/u.test(value)
    || Number.isNaN(Date.parse(value)) || new Date(value).toISOString() !== value) fail("evidence_timestamp_invalid");
  return Date.parse(value);
}

function monotonic(value) {
  if (!Number.isSafeInteger(value) || value < 0) fail("evidence_monotonic_invalid");
  return value;
}

export function validateMonotonicDeadlineObservation(value) {
  exactObject(value, ["clock", "rtt_ms", "deadline_ms", "terminal_elapsed_ms", "reconnect_extended_deadline", "terminal_state",
    "watchdog_fired", "task_cancelled", "pending_approvals_rejected", "credentials_revoked", "temp_model_cleaned"], "e2e_monotonic_deadline_invalid");
  if (value.clock !== "monotonic" || !Number.isSafeInteger(value.rtt_ms) || value.rtt_ms < 0
    || value.rtt_ms >= 29_000 || !Number.isSafeInteger(value.deadline_ms) || value.deadline_ms <= 0
    || value.deadline_ms > 30_000 - value.rtt_ms - 1_000
    || !Number.isSafeInteger(value.terminal_elapsed_ms) || value.terminal_elapsed_ms <= 0
    || value.terminal_elapsed_ms > value.deadline_ms || value.deadline_ms - value.terminal_elapsed_ms > 1_000
    || value.reconnect_extended_deadline !== false || value.terminal_state !== "peer_unavailable"
    || value.watchdog_fired !== true || value.task_cancelled !== true || value.pending_approvals_rejected !== true
    || value.credentials_revoked !== true || value.temp_model_cleaned !== true) {
    fail("e2e_monotonic_deadline_invalid");
  }
  return true;
}

function digest(value, code = "evidence_digest_invalid") {
  if (typeof value !== "string" || !SHA256.test(value)) fail(code);
  return value;
}

function combinedGatewayStateDigest(inputDigest, controlPlaneDigest) {
  return crypto.createHash("sha256").update(JSON.stringify(["pinvou-gateway-state-v1", inputDigest, controlPlaneDigest]), "utf8").digest("hex");
}

function canonicalGatewayInputDigest(inventoryDigest, configDigest) {
  return crypto.createHash("sha256").update(JSON.stringify({ gateway_config_sha256: configDigest, inventory_sha256: inventoryDigest }), "utf8").digest("hex");
}

function uniqueRoles(values, validator, code) {
  if (!Array.isArray(values) || values.length !== 2) fail(code);
  const roles = new Set(values.map(validator));
  if (roles.size !== 2 || !roles.has("A") || !roles.has("B")) fail(code);
}

function installer(value) {
  exactObject(value, ["role", "version", "sha256", "authenticode_thumbprint", "publisher", "artifact_path", "attested_subject_name", "sbom_attested_subject_name", "source_commit_sha", "build_provenance_artifact_sha256", "build_attestation_bundle_artifact_sha256", "sbom_artifact_sha256", "sbom_attestation_bundle_artifact_sha256"], "evidence_installer_invalid");
  if (!ROLES.has(value.role) || !VERSION.test(value.version) || !THUMBPRINT.test(value.authenticode_thumbprint)
    || typeof value.publisher !== "string" || value.publisher.length < 3 || !SAFE_PATH.test(value.artifact_path)
    || !/^[A-Za-z0-9][A-Za-z0-9._-]{1,127}\.exe$/u.test(value.attested_subject_name)
    || value.sbom_attested_subject_name !== "installer-sbom.spdx.json"
    || !SHA1.test(value.source_commit_sha)) fail("evidence_installer_invalid");
  digest(value.sha256); digest(value.build_provenance_artifact_sha256); digest(value.build_attestation_bundle_artifact_sha256); digest(value.sbom_artifact_sha256); digest(value.sbom_attestation_bundle_artifact_sha256);
  return value.role;
}

function machine(value) {
  exactObject(value, ["role", "identity_digest", "windows_build"], "evidence_machine_invalid");
  if (!ROLES.has(value.role) || !WINDOWS_BUILD.test(value.windows_build)) fail("evidence_machine_invalid");
  digest(value.identity_digest);
  return value.role;
}

function artifact(value) {
  exactObject(value, ["path", "sha256", "kind", "machine_role", "signature"], "evidence_artifact_invalid");
  if (typeof value.path !== "string" || !SAFE_PATH.test(value.path)
    || isAbsolute(value.path) || normalize(value.path).split(/[\\/]/u).includes("..")
    || !ALLOWED_ARTIFACT_KINDS.has(value.kind) || !ARTIFACT_ROLES.has(value.machine_role)
    || typeof value.signature !== "string" || !/^[A-Za-z0-9+/]{80,128}={0,2}$/u.test(value.signature)) fail("evidence_artifact_invalid");
  digest(value.sha256);
  return value;
}

function artifactAttestationPayload(item, document) {
  return Buffer.from(JSON.stringify([
    "pinvou-remote-control-artifact-v1", item.path, item.kind, item.sha256,
    item.machine_role, document.commit_sha, document.attestations[0].run_nonce,
  ]), "utf8");
}

function journey(value, expected, evidenceBounds) {
  exactObject(value, [
    "id", "result", "started_at", "finished_at", "monotonic_start_ms",
    "monotonic_end_ms", "trace_sha256", "coordination_digest", "deadline_observation", "checks",
  ], "evidence_journey_invalid");
  if (value.id !== expected.id || value.result !== "pass") fail("evidence_journey_not_passed");
  const started = timestamp(value.started_at);
  const finished = timestamp(value.finished_at);
  const monotonicStart = monotonic(value.monotonic_start_ms);
  const monotonicEnd = monotonic(value.monotonic_end_ms);
  if (started < evidenceBounds.started || finished > evidenceBounds.finished || finished < started
    || monotonicStart < evidenceBounds.monotonicStart || monotonicEnd > evidenceBounds.monotonicEnd
    || monotonicEnd < monotonicStart) fail("evidence_journey_order_invalid");
  digest(value.trace_sha256);
  const coordinated = new Set(["RC-E2E-006", "RC-E2E-007", "RC-E2E-008"]).has(value.id);
  if (coordinated) digest(value.coordination_digest, "evidence_journey_coordination_invalid");
  else if (value.coordination_digest !== null) fail("evidence_journey_coordination_invalid");
  if (value.id === "RC-E2E-011") {
    validateMonotonicDeadlineObservation(value.deadline_observation);
    const observedJourneyElapsed = monotonicEnd - monotonicStart;
    if (observedJourneyElapsed <= 0 || Math.abs(observedJourneyElapsed - value.deadline_observation.terminal_elapsed_ms) > 1_500) {
      fail("e2e_monotonic_elapsed_mismatch");
    }
  }
  else if (value.deadline_observation !== null) fail("evidence_journey_deadline_invalid");
  if (!Array.isArray(value.checks) || value.checks.length === 0) fail("evidence_journey_checks_invalid");
  const checkIds = new Set();
  for (const check of value.checks) {
    exactObject(check, ["id", "result", "artifact_sha256"], "evidence_journey_check_invalid");
    if (typeof check.id !== "string" || !SAFE_ID.test(check.id) || checkIds.has(check.id)
      || check.result !== "pass") fail("evidence_journey_check_invalid");
    checkIds.add(check.id);
    digest(check.artifact_sha256);
  }
  for (const required of expected.required_checks) {
    if (!checkIds.has(required)) fail("evidence_required_check_missing");
  }
  if (checkIds.size !== expected.required_checks.length) fail("evidence_journey_checks_invalid");
}

async function inspectArtifact(path, kind) {
  const bytes = await readFile(path);
  const maxSize = TEXT_ARTIFACT_KINDS.has(kind) ? 32 * 1024 * 1024 : 1024 * 1024 * 1024;
  if (bytes.length === 0 || bytes.length > maxSize) fail("evidence_artifact_size_invalid");
  if (!TEXT_ARTIFACT_KINDS.has(kind)) return crypto.createHash("sha256").update(bytes).digest("hex");
  const text = bytes.toString("utf8");
  if (Buffer.byteLength(text, "utf8") !== bytes.length
    || /-----BEGIN [A-Z ]*PRIVATE KEY-----|\bBearer\s+\S+|\bsk-[A-Za-z0-9_-]{12,}|vault:\/\//u.test(text)
    || /"(?:pairing_code|resume_token|role_token|access_token|auth_token|cookie|secret|endpoint_id|connection_id|controller_lease_id|machine_id|uuid|device_name|host_name|hostname|user_name|ip_address|prompt|reply|content|tool_arguments|file_name|file_path)"\s*:/iu.test(text)
    || /\b(?:\d{1,3}\.){3}\d{1,3}\b|\b\d{7}\b|[A-Za-z]:\\(?:Users|ProgramData)\\|\/(?:home|private|tmp)\//u.test(text)) {
    fail("evidence_artifact_sensitive_content");
  }
  return crypto.createHash("sha256").update(bytes).digest("hex");
}

async function validateUiSmokeEvidence(root, artifacts, uiGateDigest, journeys) {
  const byDigest = new Map(artifacts.map((item) => [item.sha256, item]));
  const summaryArtifact = byDigest.get(uiGateDigest);
  if (!summaryArtifact || summaryArtifact.kind !== "ui-smoke" || summaryArtifact.machine_role !== "orchestrator") fail("evidence_ui_smoke_invalid");
  let summary;
  try {
    summary = JSON.parse(await readFile(resolve(root, ...summaryArtifact.path.split("/")), "utf8"));
  } catch { fail("evidence_ui_smoke_invalid"); }
  exactObject(summary, ["schema", "version", "result", "artifacts"], "evidence_ui_smoke_invalid");
  if (summary.schema !== "pinvou-remote-control-ui-suite" || summary.version !== 1
    || summary.result !== "pass" || !Array.isArray(summary.artifacts)
    || summary.artifacts.length !== REQUIRED_UI_PHASES.size) fail("evidence_ui_smoke_invalid");
  const phases = new Set();
  for (const reference of summary.artifacts) {
    exactObject(reference, ["role", "phase", "sha256"], "evidence_ui_smoke_invalid");
    if (reference.role !== REQUIRED_UI_PHASES.get(reference.phase) || phases.has(reference.phase)) fail("evidence_ui_smoke_invalid");
    phases.add(reference.phase);
    digest(reference.sha256);
    const artifactEntry = byDigest.get(reference.sha256);
    if (!artifactEntry || artifactEntry.kind !== "ui-smoke" || artifactEntry.machine_role !== reference.role
      || reference.sha256 === uiGateDigest) fail("evidence_ui_smoke_invalid");
    let phaseEvidence;
    try {
      phaseEvidence = JSON.parse(await readFile(resolve(root, ...artifactEntry.path.split("/")), "utf8"));
    } catch { fail("evidence_ui_smoke_invalid"); }
    const coordinatedPhase = new Set(["approval-allow", "approval-deny", "attachment"]).has(reference.phase);
    exactObject(phaseEvidence, coordinatedPhase
      ? ["schema", "version", "phase", "result", "checks", "actions", "coordination_digest", ...(reference.phase === "attachment" ? ["attachment_sha256"] : [])]
      : ["schema", "version", "phase", "result", "checks", "actions"], "evidence_ui_smoke_invalid");
    if (phaseEvidence.schema !== "pinvou-remote-control-ui-smoke" || phaseEvidence.version !== 1
      || phaseEvidence.phase !== reference.phase || phaseEvidence.result !== "pass"
      || !Array.isArray(phaseEvidence.checks)
      || !phaseEvidence.checks.includes("real_tauri_page")
      || !phaseEvidence.checks.includes(`phase_${reference.phase.replaceAll("-", "_")}`)
      || (coordinatedPhase && !SHA256.test(phaseEvidence.coordination_digest))
      || JSON.stringify(phaseEvidence.actions) !== JSON.stringify(REQUIRED_UI_ACTIONS.get(reference.phase))) fail("evidence_ui_smoke_invalid");
    if (coordinatedPhase) {
      const journeyId = new Map([["approval-allow", "RC-E2E-006"], ["approval-deny", "RC-E2E-007"], ["attachment", "RC-E2E-008"]]).get(reference.phase);
      if (journeys.find((item) => item.id === journeyId)?.coordination_digest !== phaseEvidence.coordination_digest) fail("evidence_ui_smoke_coordination_mismatch");
    }
    if (reference.phase === "attachment") digest(phaseEvidence.attachment_sha256, "evidence_ui_smoke_invalid");
  }
  if ([...REQUIRED_UI_PHASES.keys()].some((phase) => !phases.has(phase))) fail("evidence_ui_smoke_invalid");
}

async function validateFeatureOffEvidence(root, artifacts, gateDigest, commitSha) {
  const byDigest = new Map(artifacts.map((item) => [item.sha256, item]));
  const gateArtifact = byDigest.get(gateDigest);
  if (!gateArtifact || gateArtifact.kind !== "check" || gateArtifact.machine_role !== "orchestrator") fail("evidence_feature_off_invalid");
  let summary;
  try { summary = JSON.parse(await readFile(resolve(root, ...gateArtifact.path.split("/")), "utf8")); } catch { fail("evidence_feature_off_invalid"); }
  exactObject(summary, ["schema", "version", "result", "source_commit_sha", "relay_rejected", "roles"], "evidence_feature_off_invalid");
  if (summary.schema !== "pinvou-remote-control-feature-off-suite" || summary.version !== 1 || summary.result !== "pass"
    || summary.source_commit_sha !== commitSha || summary.relay_rejected !== true || !Array.isArray(summary.roles) || summary.roles.length !== 2) fail("evidence_feature_off_invalid");
  const roles = new Set();
  for (const role of summary.roles) {
    exactObject(role, ["role", "sha256", "successful_socket_count", "relay_allocation_delta", "state_unchanged", "relay_rejected", "ui_entry_hidden", "command_surface_checked"], "evidence_feature_off_invalid");
    if (!ROLES.has(role.role) || roles.has(role.role) || role.successful_socket_count !== 0 || role.relay_allocation_delta !== 0
      || role.state_unchanged !== true || role.relay_rejected !== true || role.ui_entry_hidden !== true || role.command_surface_checked !== true) fail("evidence_feature_off_invalid");
    roles.add(role.role); digest(role.sha256);
    const observation = byDigest.get(role.sha256);
    if (!observation || observation.kind !== "check" || observation.machine_role !== role.role) fail("evidence_feature_off_invalid");
    let measured;
    try { measured = JSON.parse(await readFile(resolve(root, ...observation.path.split("/")), "utf8")); } catch { fail("evidence_feature_off_invalid"); }
    exactObject(measured, ["schema", "version", "role", "source_commit_sha", "successful_socket_count", "relay_allocation_delta",
      "registry_before_sha256", "registry_after_sha256", "tombstone_before_sha256", "tombstone_after_sha256", "relay_rejected",
      "ui_entry_hidden", "registered_command_count", "all_commands_fail_closed", "error_code", "mock"], "evidence_feature_off_invalid");
    if (measured.schema !== "pinvou-remote-control-feature-off-observation" || measured.version !== 1 || measured.role !== role.role
      || measured.source_commit_sha !== commitSha || measured.successful_socket_count !== 0 || measured.relay_allocation_delta !== 0
      || measured.registry_before_sha256 !== measured.registry_after_sha256 || measured.tombstone_before_sha256 !== measured.tombstone_after_sha256
      || measured.relay_rejected !== true || measured.ui_entry_hidden !== true || !Number.isSafeInteger(measured.registered_command_count)
      || measured.registered_command_count < 30 || measured.all_commands_fail_closed !== true
      || measured.error_code !== "desktop_remote_control_unsupported" || measured.mock !== false) fail("evidence_feature_off_invalid");
    for (const key of ["registry_before_sha256", "registry_after_sha256", "tombstone_before_sha256", "tombstone_after_sha256"]) digest(measured[key]);
  }
  if (!roles.has("A") || !roles.has("B")) fail("evidence_feature_off_invalid");
}

async function validateControlledAttachmentReceipt(root, artifacts, document) {
  const journey = document.journeys.find((item) => item.id === "RC-E2E-008");
  const check = journey?.checks.find((item) => item.id === "controlled_attachment_received");
  const entry = artifacts.find((item) => item.sha256 === check?.artifact_sha256);
  if (!entry || entry.kind !== "check" || entry.machine_role !== "B") fail("evidence_controlled_attachment_receipt_invalid");
  let receipt;
  try { receipt = JSON.parse(await readFile(resolve(root, ...entry.path.split("/")), "utf8")); } catch { fail("evidence_controlled_attachment_receipt_invalid"); }
  exactObject(receipt, ["schema", "version", "source_commit_sha", "run_nonce", "role", "coordination_digest", "attachment_sha256", "received", "mock"], "evidence_controlled_attachment_receipt_invalid");
  if (receipt.schema !== "pinvou-controlled-attachment-receipt" || receipt.version !== 1 || receipt.source_commit_sha !== document.commit_sha
    || receipt.run_nonce !== document.attestations[0].run_nonce || receipt.role !== "B" || receipt.received !== true || receipt.mock !== false
    || receipt.coordination_digest !== journey.coordination_digest) fail("evidence_controlled_attachment_receipt_invalid");
  digest(receipt.attachment_sha256, "evidence_controlled_attachment_receipt_invalid");
  const uiSummaryEntry = artifacts.find((item) => item.sha256 === document.gates.ui_smoke.artifact_sha256);
  const summary = JSON.parse(await readFile(resolve(root, ...uiSummaryEntry.path.split("/")), "utf8"));
  const uiReference = summary.artifacts.find((item) => item.phase === "attachment");
  const uiEntry = artifacts.find((item) => item.sha256 === uiReference?.sha256);
  const ui = JSON.parse(await readFile(resolve(root, ...uiEntry.path.split("/")), "utf8"));
  if (ui.attachment_sha256 !== receipt.attachment_sha256) fail("evidence_controlled_attachment_receipt_invalid");
}

async function validateLegacyRuntimeEvidence(root, artifacts, legacy, document) {
  const commitSha = document.commit_sha;
  const byDigest = new Map(artifacts.map((item) => [item.sha256, item]));
  const runtimeEntry = byDigest.get(legacy.runtime_artifact_sha256);
  const routeSpecEntry = byDigest.get(legacy.route_spec_artifact_sha256);
  const configurationEntry = byDigest.get(legacy.configuration_artifact_sha256);
  const probeEntry = byDigest.get(legacy.probe_artifact_sha256);
  const bareV2Entry = byDigest.get(legacy.bare_v2_artifact_sha256);
  const restorationEntry = byDigest.get(legacy.restoration_artifact_sha256);
  let runtime; let routeSpec; let configuration; let probe; let bareV2; let restoration;
  try {
    runtime = JSON.parse(await readFile(resolve(root, ...runtimeEntry.path.split("/")), "utf8"));
    routeSpec = JSON.parse(await readFile(resolve(root, ...routeSpecEntry.path.split("/")), "utf8"));
    configuration = JSON.parse(await readFile(resolve(root, ...configurationEntry.path.split("/")), "utf8"));
    probe = JSON.parse(await readFile(resolve(root, ...probeEntry.path.split("/")), "utf8"));
    bareV2 = JSON.parse(await readFile(resolve(root, ...bareV2Entry.path.split("/")), "utf8"));
    restoration = JSON.parse(await readFile(resolve(root, ...restorationEntry.path.split("/")), "utf8"));
  } catch { fail("evidence_legacy_relay_invalid"); }
  exactObject(runtime, ["schema", "version", "source_commit_sha", "pid", "relay_version", "binary_sha256", "executable_sha256", "executable_verified", "listener_scope", "listener_port", "listener_owner_pid", "process_started", "mock"], "evidence_legacy_relay_invalid");
  exactObject(routeSpec, ["schema", "version", "run_nonce", "route_id", "gateway_url_sha256", "target", "no_bypass", "endpoint_inventory_sha256", "gateway_config_sha256", "canonical_inventory_sha256", "canonical_gateway_config_sha256", "canonical_input_state_digest"], "evidence_legacy_relay_invalid");
  exactObject(configuration, ["schema", "version", "source_commit_sha", "run_nonce", "pid", "route_id", "no_bypass", "gateway_url_sha256", "gateway_config_sha256", "endpoint_inventory_sha256", "canonical_input_state_digest", "control_plane_state_before_digest", "original_state_digest", "route_state_digest", "listener_url_sha256", "listener_port", "route_activated", "mock"], "evidence_legacy_relay_invalid");
  exactObject(probe, ["schema", "version", "source_commit_sha", "run_nonce", "pid", "route_id", "no_bypass", "gateway_url_sha256", "gateway_config_sha256", "endpoint_inventory_sha256", "route_state_digest", "listener_url_sha256", "listener_port", "request_nonce", "traffic_counter_before", "traffic_counter_after", "response_nonce", "gateway_traffic_observed", "listener_flow_observed", "response_correlated", "mock"], "evidence_legacy_relay_invalid");
  exactObject(bareV2, ["schema", "version", "source_commit_sha", "run_nonce", "role", "route_id", "gateway_url_sha256", "request_nonce", "response_kind", "rejected", "mock"], "evidence_legacy_relay_invalid");
  exactObject(restoration, ["schema", "version", "source_commit_sha", "run_nonce", "route_id", "owned_route_removed", "public_route_withdrawn", "canonical_input_state_digest", "control_plane_state_before_digest", "control_plane_state_after_digest", "original_state_digest", "restored_state_digest", "mock"], "evidence_legacy_relay_invalid");
  if (runtime.schema !== "pinvou-legacy-relay-runtime-evidence" || runtime.version !== 1 || runtime.source_commit_sha !== commitSha
    || runtime.pid !== legacy.pid || runtime.relay_version !== legacy.version || runtime.binary_sha256 !== legacy.sha256
    || runtime.executable_sha256 !== legacy.sha256 || runtime.executable_verified !== true || runtime.listener_scope !== "loopback"
    || runtime.listener_port !== legacy.listener_port || runtime.listener_owner_pid !== legacy.pid || runtime.process_started !== true || runtime.mock !== false
    || routeSpec.schema !== "pinvou-legacy-gateway-route-spec" || routeSpec.version !== 1 || routeSpec.run_nonce !== document.attestations[0].run_nonce
    || routeSpec.route_id !== legacy.route_id || routeSpec.gateway_url_sha256 !== legacy.gateway_url_sha256 || routeSpec.target !== "legacy_relay"
    || routeSpec.no_bypass !== true || routeSpec.endpoint_inventory_sha256 !== legacy.endpoint_inventory_sha256
    || routeSpec.gateway_config_sha256 !== legacy.gateway_config_sha256
    || routeSpec.canonical_input_state_digest !== canonicalGatewayInputDigest(routeSpec.canonical_inventory_sha256, routeSpec.canonical_gateway_config_sha256)
    || configuration.schema !== "pinvou-legacy-relay-configuration-evidence" || configuration.version !== 1 || configuration.source_commit_sha !== commitSha
    || configuration.pid !== legacy.pid || configuration.run_nonce !== document.attestations[0].run_nonce || configuration.route_id !== legacy.route_id || configuration.no_bypass !== true || configuration.gateway_url_sha256 !== legacy.gateway_url_sha256
    || configuration.gateway_config_sha256 !== legacy.gateway_config_sha256 || configuration.endpoint_inventory_sha256 !== legacy.endpoint_inventory_sha256
    || configuration.listener_port !== legacy.listener_port || configuration.route_activated !== true || configuration.mock !== false
    || probe.schema !== "pinvou-legacy-relay-probe-evidence" || probe.version !== 1 || probe.source_commit_sha !== commitSha
    || probe.pid !== legacy.pid || probe.run_nonce !== document.attestations[0].run_nonce || probe.route_id !== legacy.route_id || probe.no_bypass !== true || probe.gateway_url_sha256 !== legacy.gateway_url_sha256
    || probe.gateway_config_sha256 !== legacy.gateway_config_sha256 || probe.endpoint_inventory_sha256 !== legacy.endpoint_inventory_sha256
    || probe.listener_url_sha256 !== configuration.listener_url_sha256 || probe.listener_port !== legacy.listener_port
    || probe.request_nonce !== legacy.request_nonce || probe.response_nonce !== legacy.request_nonce
    || probe.traffic_counter_before !== legacy.traffic_counter_before || probe.traffic_counter_after !== legacy.traffic_counter_after
    || probe.route_state_digest !== configuration.route_state_digest || probe.traffic_counter_after <= probe.traffic_counter_before || probe.gateway_traffic_observed !== true
    || probe.listener_flow_observed !== true || probe.response_correlated !== true || probe.mock !== false
    || bareV2.schema !== "pinvou-legacy-bare-v2-rejection" || bareV2.version !== 1 || bareV2.source_commit_sha !== commitSha
    || bareV2.run_nonce !== document.attestations[0].run_nonce || bareV2.role !== "A" || bareV2.route_id !== legacy.route_id
    || bareV2.gateway_url_sha256 !== legacy.gateway_url_sha256 || bareV2.request_nonce !== legacy.request_nonce
    || bareV2.response_kind !== "upgrade_rejected" || bareV2.rejected !== true || bareV2.mock !== false
    || restoration.schema !== "pinvou-legacy-gateway-restoration-evidence" || restoration.version !== 1 || restoration.source_commit_sha !== commitSha
    || restoration.run_nonce !== document.attestations[0].run_nonce || restoration.route_id !== legacy.route_id
    || restoration.owned_route_removed !== true || restoration.public_route_withdrawn !== true
    || routeSpec.canonical_input_state_digest !== configuration.canonical_input_state_digest || restoration.canonical_input_state_digest !== routeSpec.canonical_input_state_digest
    || restoration.control_plane_state_before_digest !== configuration.control_plane_state_before_digest
    || restoration.control_plane_state_after_digest !== configuration.control_plane_state_before_digest
    || configuration.original_state_digest !== combinedGatewayStateDigest(routeSpec.canonical_input_state_digest, configuration.control_plane_state_before_digest)
    || restoration.original_state_digest !== configuration.original_state_digest || restoration.restored_state_digest !== configuration.original_state_digest
    || restoration.mock !== false) fail("evidence_legacy_relay_invalid");
  for (const value of [routeSpec.canonical_inventory_sha256, routeSpec.canonical_gateway_config_sha256, routeSpec.canonical_input_state_digest, configuration.control_plane_state_before_digest, configuration.listener_url_sha256, configuration.route_state_digest, configuration.original_state_digest, restoration.control_plane_state_after_digest, probe.listener_url_sha256, probe.request_nonce, probe.response_nonce]) digest(value, "evidence_legacy_relay_invalid");
}

async function validateProviderAttestation(root, artifacts, document) {
  for (const attestation of document.attestations) {
    const entry = artifacts.find((item) => item.sha256 === attestation.provider_attestation_artifact_sha256);
    if (!entry || entry.kind !== "check" || entry.machine_role !== attestation.role) fail("evidence_attestation_provider_invalid");
    let value;
    try { value = JSON.parse(await readFile(resolve(root, ...entry.path.split("/")), "utf8")); } catch { fail("evidence_attestation_provider_invalid"); }
    const issuedAt = Date.parse(value.issued_at);
    const runStartedAt = Date.parse(document.started_at); const runFinishedAt = Date.parse(document.finished_at);
    if (!Number.isFinite(issuedAt) || issuedAt < runStartedAt || issuedAt > runFinishedAt) fail("evidence_attestation_provider_invalid");
    try {
      verifyProviderAttestation(value, {
        role: attestation.role, rolePolicySha256: attestation.role_policy_sha256,
        provider: attestation.provider, keyId: attestation.key_id, runNonce: attestation.run_nonce,
        aclSha256: attestation.acl_attestation_sha256, measurementSha256: attestation.provider_measurement_sha256,
        driverSha256: document.driver.sha256, vendor: attestation.provider_vendor,
        rootCertificateSha256: attestation.provider_root_certificate_sha256,
        verifierCertificateSha256: attestation.provider_verifier_certificate_sha256,
      }, issuedAt);
    } catch { fail("evidence_attestation_provider_invalid"); }
    if (value.public_key_sha256 !== attestation.public_key_sha256) fail("evidence_attestation_provider_invalid");
  }
}

async function validateBuildProvenance(root, artifacts, document, {
  expectedRepository, expectedWorkflow, expectedRunId,
} = {}) {
  const installer = document.installers[0];
  const entry = artifacts.find((item) => item.sha256 === installer.build_provenance_artifact_sha256);
  const sbom = artifacts.find((item) => item.sha256 === installer.sbom_artifact_sha256);
  if (!entry || entry.kind !== "check" || entry.machine_role !== "orchestrator" || !sbom || sbom.kind !== "log") fail("evidence_installer_provenance_invalid");
  let value;
  try { value = JSON.parse(await readFile(resolve(root, ...entry.path.split("/")), "utf8")); } catch { fail("evidence_installer_provenance_invalid"); }
  exactObject(value, ["schema", "version", "source_commit_sha", "installer_sha256", "attestation_bundle_sha256", "sbom_sha256", "sbom_attestation_bundle_sha256", "installer_subject_name", "sbom_subject_name", "repository", "workflow", "build_run_id", "protected_environment", "verification_tool", "mock"], "evidence_installer_provenance_invalid");
  if (value.schema !== "pinvou-installer-supply-chain-verification" || value.version !== 1 || value.source_commit_sha !== document.commit_sha
    || value.installer_sha256 !== installer.sha256 || value.sbom_sha256 !== installer.sbom_artifact_sha256
    || value.attestation_bundle_sha256 !== installer.build_attestation_bundle_artifact_sha256
    || value.sbom_attestation_bundle_sha256 !== installer.sbom_attestation_bundle_artifact_sha256
    || value.installer_subject_name !== installer.attested_subject_name || value.sbom_subject_name !== installer.sbom_attested_subject_name
    || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(value.repository)
    || !/^\.github\/workflows\/[A-Za-z0-9_.-]+\.ya?ml$/u.test(value.workflow)
    || !Number.isSafeInteger(value.build_run_id) || value.build_run_id <= 0 || value.protected_environment !== "windows-release"
    || value.verification_tool !== "gh-attestation-verify" || value.mock !== false) fail("evidence_installer_provenance_invalid");
  if ((expectedRepository && value.repository !== expectedRepository)
    || (expectedWorkflow && value.workflow !== expectedWorkflow)
    || (expectedRunId && value.build_run_id !== Number(expectedRunId))) fail("evidence_installer_provenance_untrusted");
  let spdxValue;
  try { spdxValue = JSON.parse(await readFile(resolve(root, ...sbom.path.split("/")), "utf8")); } catch { fail("evidence_installer_provenance_invalid"); }
  try { validateSpdx(spdxValue, { installerName: installer.attested_subject_name, installerSha256: installer.sha256, sourceCommit: document.commit_sha }); }
  catch { fail("evidence_installer_provenance_invalid"); }
}

export async function validateReleaseEvidence(document, {
  evidenceRoot,
  expectedCommit,
  expectedSignerThumbprint,
  expectedSignerPublisher,
  expectedInventorySha256,
  expectedGatewayConfigSha256,
  expectedEnrollmentDeployment,
  expectedDriverSha256,
  expectedNodeSha256,
  expectedNodeSignerThumbprint,
  expectedNodeSignerPublisher,
  expectedInstallerSha256,
  expectedInstallerVersion,
  expectedAttestationPublicKeySha256,
  expectedAttestationProvider,
  expectedAttestationKeyId,
  expectedAttestationAclSha256,
  expectedAttestationProviderMeasurementSha256,
  expectedAttestationProviderVendor,
  expectedAttestationProviderRootCertificateSha256,
  expectedAttestationProviderVerifierCertificateSha256,
  expectedOrchestratorRolePolicySha256,
  expectedARolePolicySha256,
  expectedBRolePolicySha256,
  expectedBuildProvenanceSha256,
  expectedBuildRepository,
  expectedBuildWorkflow,
  expectedBuildRunId,
  allowTestAttestation = false,
} = {}) {
  if (!validateEvidenceSchema(document)) fail("evidence_schema_invalid");
  exactObject(document, [
    "schema", "version", "commit_sha", "provenance", "attestations", "relay", "legacy_relay", "driver", "installers", "machines",
    "started_at", "finished_at", "monotonic_start_ms", "monotonic_end_ms",
    "journeys", "artifacts", "gates",
  ], "evidence_document_invalid");
  if (document.schema !== "pinvou-remote-control-release-evidence" || document.version !== 1
    || !SHA1.test(document.commit_sha)) fail("evidence_document_invalid");
  if (expectedCommit && document.commit_sha !== expectedCommit) fail("evidence_commit_mismatch");
  const provenance = exactObject(document.provenance,
    ["kind", "orchestrator", "source_commit_sha", "synthetic", "node_version", "node_sha256", "node_artifact_path", "node_authenticode_thumbprint", "node_publisher"], "evidence_provenance_invalid");
  if (provenance.kind !== "physical-dual-windows" || provenance.orchestrator !== "pinvou-windows-e2e-v1"
    || provenance.synthetic !== false || provenance.source_commit_sha !== document.commit_sha
    || !VERSION.test(provenance.node_version) || !SAFE_PATH.test(provenance.node_artifact_path)
    || !THUMBPRINT.test(provenance.node_authenticode_thumbprint) || typeof provenance.node_publisher !== "string"
    || provenance.node_publisher.length < 3) fail("evidence_provenance_invalid");
  digest(provenance.node_sha256);
  if (expectedNodeSha256 && provenance.node_sha256 !== expectedNodeSha256) fail("evidence_node_digest_mismatch");
  if (expectedNodeSignerThumbprint && provenance.node_authenticode_thumbprint !== expectedNodeSignerThumbprint) fail("evidence_node_signer_mismatch");
  if (expectedNodeSignerPublisher && provenance.node_publisher !== expectedNodeSignerPublisher) fail("evidence_node_signer_mismatch");
  if (!Array.isArray(document.attestations) || document.attestations.length !== 3) fail("evidence_attestation_invalid");
  const attestationByRole = new Map(); const attestationKeys = new Map();
  for (const attestation of document.attestations) {
    exactObject(attestation, ["role", "role_policy_sha256", "identity", "algorithm", "public_key_spki_pem", "public_key_sha256", "run_nonce", "provider", "key_id", "non_exportable", "acl_attestation_sha256", "provider_measurement_sha256", "provider_vendor", "provider_root_certificate_sha256", "provider_verifier_certificate_sha256", "provider_attestation_artifact_sha256"], "evidence_attestation_invalid");
    if (!new Set(["orchestrator", "A", "B"]).has(attestation.role) || attestationByRole.has(attestation.role)
      || !new Set(["production", "test"]).has(attestation.identity) || attestation.algorithm !== "ed25519"
      || (attestation.identity === "test" && !allowTestAttestation)) fail("evidence_attestation_identity_untrusted");
    if (attestation.provider !== "remote_signing_service"
      || typeof attestation.key_id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._:/-]{2,127}$/u.test(attestation.key_id) || attestation.non_exportable !== true) fail("evidence_attestation_provider_invalid");
    for (const value of [attestation.role_policy_sha256, attestation.public_key_sha256, attestation.run_nonce, attestation.acl_attestation_sha256, attestation.provider_measurement_sha256, attestation.provider_root_certificate_sha256, attestation.provider_verifier_certificate_sha256, attestation.provider_attestation_artifact_sha256]) digest(value);
    if (!/^[A-Za-z0-9][A-Za-z0-9 ._-]{1,63}$/u.test(attestation.provider_vendor)) fail("evidence_attestation_provider_invalid");
    if (attestation.run_nonce !== document.attestations[0].run_nonce) fail("evidence_attestation_provider_invalid");
    if (expectedAttestationProvider && attestation.provider !== expectedAttestationProvider) fail("evidence_attestation_provider_untrusted");
    if (expectedAttestationAclSha256 && attestation.acl_attestation_sha256 !== expectedAttestationAclSha256) fail("evidence_attestation_acl_untrusted");
    if (expectedAttestationProviderMeasurementSha256 && attestation.provider_measurement_sha256 !== expectedAttestationProviderMeasurementSha256) fail("evidence_attestation_provider_untrusted");
    if (expectedAttestationProviderVendor && attestation.provider_vendor !== expectedAttestationProviderVendor) fail("evidence_attestation_provider_untrusted");
    if (expectedAttestationProviderRootCertificateSha256 && attestation.provider_root_certificate_sha256 !== expectedAttestationProviderRootCertificateSha256) fail("evidence_attestation_provider_untrusted");
    if (expectedAttestationProviderVerifierCertificateSha256 && attestation.provider_verifier_certificate_sha256 !== expectedAttestationProviderVerifierCertificateSha256) fail("evidence_attestation_provider_untrusted");
    let key; try { key = crypto.createPublicKey(attestation.public_key_spki_pem); } catch { fail("evidence_attestation_key_invalid"); }
    const actual = crypto.createHash("sha256").update(key.export({ type: "spki", format: "der" })).digest("hex");
    if (actual !== attestation.public_key_sha256) fail("evidence_attestation_key_invalid");
    attestationByRole.set(attestation.role, attestation); attestationKeys.set(attestation.role, key);
  }
  if (attestationByRole.size !== 3) fail("evidence_attestation_invalid");
  if (new Set(document.attestations.map((item) => item.role_policy_sha256)).size !== 3) fail("evidence_attestation_role_policy_reused");
  for (const [role, expected] of [["orchestrator", expectedOrchestratorRolePolicySha256], ["A", expectedARolePolicySha256], ["B", expectedBRolePolicySha256]]) {
    if (expected && attestationByRole.get(role).role_policy_sha256 !== expected) fail("evidence_attestation_role_policy_untrusted");
  }
  const orchestratorAttestation = attestationByRole.get("orchestrator");
  if (orchestratorAttestation.identity === "production" && !expectedAttestationPublicKeySha256) fail("evidence_attestation_key_untrusted");
  if (expectedAttestationPublicKeySha256 && orchestratorAttestation.public_key_sha256 !== expectedAttestationPublicKeySha256) fail("evidence_attestation_key_untrusted");
  if (expectedAttestationKeyId && orchestratorAttestation.key_id !== expectedAttestationKeyId) fail("evidence_attestation_key_untrusted");
  const relay = exactObject(document.relay, [
    "version", "config_fingerprint", "public_origin_digest", "registry_coverage_percent", "production_equivalent", "enrollment_deployment",
  ], "evidence_relay_invalid");
  if (!SAFE_VERSION.test(relay.version) || relay.registry_coverage_percent !== 100
    || relay.production_equivalent !== true || !new Set(["official", "self_hosted"]).has(relay.enrollment_deployment)) fail("evidence_relay_invalid");
  if (expectedEnrollmentDeployment && relay.enrollment_deployment !== expectedEnrollmentDeployment) fail("evidence_enrollment_deployment_mismatch");
  digest(relay.config_fingerprint);
  digest(relay.public_origin_digest);
  uniqueRoles(document.installers, installer, "evidence_installers_invalid");
  uniqueRoles(document.machines, machine, "evidence_machines_invalid");
  if (document.machines[0].identity_digest === document.machines[1].identity_digest) fail("evidence_machines_not_isolated");
  const driver = exactObject(document.driver,
    ["version", "sha256", "authenticode_thumbprint", "publisher", "artifact_path", "source_commit_sha", "attestation_public_key_sha256", "attestation_provider", "attestation_key_id", "private_key_embedded"], "evidence_driver_invalid");
  if (!VERSION.test(driver.version) || !THUMBPRINT.test(driver.authenticode_thumbprint)
    || typeof driver.publisher !== "string" || driver.publisher.length < 3 || !SAFE_PATH.test(driver.artifact_path)
    || driver.source_commit_sha !== document.commit_sha
    || driver.attestation_public_key_sha256 !== orchestratorAttestation.public_key_sha256 || driver.attestation_provider !== orchestratorAttestation.provider
    || driver.attestation_key_id !== orchestratorAttestation.key_id || driver.private_key_embedded !== false) fail("evidence_driver_invalid");
  digest(driver.sha256);
  if (expectedDriverSha256 && driver.sha256 !== expectedDriverSha256) fail("evidence_driver_digest_mismatch");
  if (expectedInstallerSha256 && document.installers.some((item) => item.sha256 !== expectedInstallerSha256)) fail("evidence_installer_digest_mismatch");
  if (expectedInstallerVersion && document.installers.some((item) => item.version !== expectedInstallerVersion)) fail("evidence_installer_version_mismatch");
  if (document.installers.some((item) => item.source_commit_sha !== document.commit_sha
    || item.build_provenance_artifact_sha256 !== document.installers[0].build_provenance_artifact_sha256
    || item.build_attestation_bundle_artifact_sha256 !== document.installers[0].build_attestation_bundle_artifact_sha256
    || item.sbom_artifact_sha256 !== document.installers[0].sbom_artifact_sha256
    || item.sbom_attestation_bundle_artifact_sha256 !== document.installers[0].sbom_attestation_bundle_artifact_sha256)) fail("evidence_installer_provenance_invalid");
  if (expectedBuildProvenanceSha256 && document.installers[0].build_provenance_artifact_sha256 !== expectedBuildProvenanceSha256) fail("evidence_installer_provenance_untrusted");
  if (expectedSignerThumbprint) {
    if (!THUMBPRINT.test(expectedSignerThumbprint) || driver.authenticode_thumbprint !== expectedSignerThumbprint
      || document.installers.some((item) => item.authenticode_thumbprint !== expectedSignerThumbprint)) fail("evidence_signer_mismatch");
  }
  if (expectedSignerPublisher && (driver.publisher !== expectedSignerPublisher
    || document.installers.some((item) => item.publisher !== expectedSignerPublisher))) fail("evidence_signer_mismatch");
  const legacy = exactObject(document.legacy_relay, [
    "version", "sha256", "artifact_path", "endpoint_inventory_sha256", "gateway_config_sha256", "pid", "listener_host", "listener_port",
    "route_id", "route_spec_artifact_sha256", "runtime_artifact_sha256", "configuration_artifact_sha256", "probe_artifact_sha256",
    "bare_v2_artifact_sha256", "restoration_artifact_sha256", "gateway_url_sha256", "request_nonce",
    "traffic_counter_before", "traffic_counter_after", "gateway_traffic_observed", "response_correlated", "bare_v2_rejected", "route_restored",
  ], "evidence_legacy_relay_invalid");
  if (!SAFE_VERSION.test(legacy.version) || !SAFE_PATH.test(legacy.artifact_path)
    || !Number.isSafeInteger(legacy.pid) || legacy.pid <= 0 || !new Set(["127.0.0.1", "::1"]).has(legacy.listener_host)
    || !Number.isSafeInteger(legacy.listener_port) || legacy.listener_port < 1 || legacy.listener_port > 65_535
    || !Number.isSafeInteger(legacy.traffic_counter_before) || legacy.traffic_counter_before < 0
    || !Number.isSafeInteger(legacy.traffic_counter_after) || legacy.traffic_counter_after <= legacy.traffic_counter_before
    || !/^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$/u.test(legacy.route_id)
    || legacy.gateway_traffic_observed !== true || legacy.response_correlated !== true || legacy.bare_v2_rejected !== true || legacy.route_restored !== true) fail("evidence_legacy_relay_invalid");
  digest(legacy.sha256); digest(legacy.endpoint_inventory_sha256); digest(legacy.gateway_config_sha256);
  digest(legacy.route_spec_artifact_sha256); digest(legacy.runtime_artifact_sha256); digest(legacy.configuration_artifact_sha256); digest(legacy.probe_artifact_sha256); digest(legacy.bare_v2_artifact_sha256); digest(legacy.restoration_artifact_sha256);
  digest(legacy.gateway_url_sha256); digest(legacy.request_nonce);
  if (expectedInventorySha256 && legacy.endpoint_inventory_sha256 !== expectedInventorySha256) fail("evidence_inventory_mismatch");
  if (expectedGatewayConfigSha256 && legacy.gateway_config_sha256 !== expectedGatewayConfigSha256) fail("evidence_gateway_config_mismatch");
  const bounds = {
    started: timestamp(document.started_at), finished: timestamp(document.finished_at),
    monotonicStart: monotonic(document.monotonic_start_ms), monotonicEnd: monotonic(document.monotonic_end_ms),
  };
  if (bounds.finished < bounds.started || bounds.monotonicEnd < bounds.monotonicStart) fail("evidence_order_invalid");
  const catalog = JSON.parse(await readFile(CATALOG_PATH, "utf8"));
  const expectedJourneys = catalog.journeys;
  if (!Array.isArray(document.journeys) || document.journeys.length !== expectedJourneys.length) fail("evidence_journey_set_invalid");
  document.journeys.forEach((item, index) => journey(item, expectedJourneys[index], bounds));
  if (!Array.isArray(document.artifacts) || document.artifacts.length === 0) fail("evidence_artifacts_invalid");
  const artifacts = document.artifacts.map(artifact);
  if (new Set(artifacts.map(({ path }) => path)).size !== artifacts.length) fail("evidence_artifact_path_duplicate");
  const artifactDigests = new Set(artifacts.map(({ sha256 }) => sha256));
  const byPath = new Map(artifacts.map((item) => [item.path, item]));
  for (const item of artifacts) {
    let signature;
    try { signature = Buffer.from(item.signature, "base64"); } catch { fail("evidence_artifact_signature_invalid"); }
    const attestationKey = attestationKeys.get(item.machine_role);
    if (!attestationKey || !crypto.verify(null, artifactAttestationPayload(item, document), attestationKey, signature)) fail("evidence_artifact_signature_invalid");
  }
  const requireArtifact = (path, sha256, kind) => {
    const item = byPath.get(path);
    if (!item || item.sha256 !== sha256 || item.kind !== kind) fail("evidence_artifact_reference_missing");
  };
  for (const item of document.installers) requireArtifact(item.artifact_path, item.sha256, "installer");
  requireArtifact("build/installer-provenance.json", document.installers[0].build_provenance_artifact_sha256, "check");
  requireArtifact("build/installer-attestation.sigstore.jsonl", document.installers[0].build_attestation_bundle_artifact_sha256, "check");
  requireArtifact("build/installer-sbom.spdx.json", document.installers[0].sbom_artifact_sha256, "log");
  requireArtifact("build/installer-sbom-attestation.sigstore.jsonl", document.installers[0].sbom_attestation_bundle_artifact_sha256, "check");
  requireArtifact(driver.artifact_path, driver.sha256, "driver");
  requireArtifact(provenance.node_artifact_path, provenance.node_sha256, "toolchain");
  requireArtifact(legacy.artifact_path, legacy.sha256, "legacy-relay");
  const claimedCheckDigests = new Set();
  const claimUniqueEvidence = (sha256) => {
    if (!artifactDigests.has(sha256)) fail("evidence_artifact_reference_missing");
    if (claimedCheckDigests.has(sha256)) fail("evidence_artifact_reused");
    claimedCheckDigests.add(sha256);
  };
  for (const legacyDigest of [legacy.route_spec_artifact_sha256, legacy.runtime_artifact_sha256, legacy.configuration_artifact_sha256, legacy.probe_artifact_sha256, legacy.bare_v2_artifact_sha256, legacy.restoration_artifact_sha256]) {
    const legacyArtifact = artifacts.find((item) => item.sha256 === legacyDigest);
    const expectedRole = legacyDigest === legacy.route_spec_artifact_sha256 ? "orchestrator" : "A";
    if (!legacyArtifact || legacyArtifact.kind !== "rollback" || legacyArtifact.machine_role !== expectedRole) fail("evidence_legacy_relay_invalid");
    claimUniqueEvidence(legacyDigest);
  }
  for (const attestation of document.attestations) claimUniqueEvidence(attestation.provider_attestation_artifact_sha256);
  for (const item of document.journeys) {
    const trace = artifacts.find((artifact) => artifact.sha256 === item.trace_sha256);
    if (!trace || trace.kind !== "trace" || trace.machine_role !== "A") fail("evidence_journey_artifact_invalid");
    claimUniqueEvidence(item.trace_sha256);
    for (const check of item.checks) {
      const artifact = artifacts.find((entry) => entry.sha256 === check.artifact_sha256);
      const role = item.id === "RC-E2E-008" && check.id === "controlled_attachment_received" ? "B" : "A";
      if (!artifact || artifact.kind !== "check" || artifact.machine_role !== role) fail("evidence_journey_artifact_invalid");
      claimUniqueEvidence(check.artifact_sha256);
    }
  }
  exactObject(document.gates, REQUIRED_GATES, "evidence_gates_invalid");
  for (const gate of REQUIRED_GATES) {
    const value = exactObject(document.gates[gate], ["result", "artifact_sha256"], "evidence_gate_invalid");
    if (value.result !== "pass") fail("evidence_gate_not_passed");
    digest(value.artifact_sha256);
    // Match the actual orchestrator producers: the four driver gates come
    // from A; only the aggregate UI and flag-off suites come from orchestrator.
    const artifact = artifacts.find((item) => item.sha256 === value.artifact_sha256);
    const aggregate = gate === "ui_smoke" || gate === "feature_flag_off";
    if (!artifact || artifact.machine_role !== (aggregate ? "orchestrator" : "A")
      || artifact.kind !== (gate === "ui_smoke" ? "ui-smoke" : "check")) fail("evidence_gate_artifact_invalid");
    claimUniqueEvidence(value.artifact_sha256);
  }
  if (evidenceRoot) {
    const root = resolve(evidenceRoot);
    const rootMetadata = await lstat(root).catch(() => fail("evidence_root_unsafe"));
    if (!rootMetadata.isDirectory() || rootMetadata.isSymbolicLink()) fail("evidence_root_unsafe");
    const realRoot = await realpath(root).catch(() => fail("evidence_root_unsafe"));
    for (const item of artifacts) {
      const path = resolve(root, ...item.path.split("/"));
      if (path !== root && !path.startsWith(`${root}${sep}`)) fail("evidence_artifact_path_escape");
      let cursor = root;
      for (const component of item.path.split("/")) {
        cursor = resolve(cursor, component);
        const metadata = await lstat(cursor).catch(() => fail("evidence_artifact_unreadable"));
        if (metadata.isSymbolicLink()) fail("evidence_artifact_path_escape");
      }
      const realTarget = await realpath(path).catch(() => fail("evidence_artifact_unreadable"));
      if (realTarget !== realRoot && !realTarget.startsWith(`${realRoot}${sep}`)) fail("evidence_artifact_path_escape");
      let actual;
      try { actual = await inspectArtifact(path, item.kind); } catch (error) {
        if (error?.code === "evidence_artifact_sensitive_content" || error?.code === "evidence_artifact_size_invalid") throw error;
        fail("evidence_artifact_unreadable");
      }
      if (actual !== item.sha256) fail("evidence_artifact_digest_mismatch");
    }
    await validateUiSmokeEvidence(root, artifacts, document.gates.ui_smoke.artifact_sha256, document.journeys);
    await validateFeatureOffEvidence(root, artifacts, document.gates.feature_flag_off.artifact_sha256, document.commit_sha);
    await validateControlledAttachmentReceipt(root, artifacts, document);
    await validateLegacyRuntimeEvidence(root, artifacts, legacy, document);
    await validateProviderAttestation(root, artifacts, document);
    await validateBuildProvenance(root, artifacts, document, {
      expectedRepository: expectedBuildRepository,
      expectedWorkflow: expectedBuildWorkflow,
      expectedRunId: expectedBuildRunId,
    });
  }
  return { ok: true, journey_count: expectedJourneys.length, artifact_count: artifacts.length };
}

export async function validateReleaseEvidenceFile(path, options = {}) {
  let document;
  try { document = JSON.parse(await readFile(path, "utf8")); } catch { fail("evidence_json_invalid"); }
  return validateReleaseEvidence(document, { evidenceRoot: dirname(resolve(path)), ...options });
}

const invokedPath = process.argv[1] ? resolve(process.argv[1]) : null;
if (invokedPath === fileURLToPath(import.meta.url)) {
  const deadlineFixture = process.argv[2] === "--deadline-observation-base64";
  const operation = deadlineFixture
    ? Promise.resolve().then(() => validateMonotonicDeadlineObservation(JSON.parse(Buffer.from(process.argv[3] || "", "base64").toString("utf8"))))
    : validateReleaseEvidenceFile(process.argv[2] || "", {
    expectedCommit: process.env.PINVOU_RELEASE_COMMIT_SHA,
    expectedSignerThumbprint: process.env.PINVOU_TRUSTED_WINDOWS_SIGNER_THUMBPRINT,
    expectedSignerPublisher: process.env.PINVOU_TRUSTED_WINDOWS_SIGNER_PUBLISHER,
    expectedInventorySha256: process.env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY_SHA256,
    expectedGatewayConfigSha256: process.env.PINVOU_REMOTE_GATEWAY_CONFIG_SHA256,
    expectedEnrollmentDeployment: process.env.PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT,
    expectedDriverSha256: process.env.PINVOU_TRUSTED_E2E_DRIVER_SHA256,
    expectedNodeSha256: process.env.PINVOU_TRUSTED_E2E_NODE_SHA256,
    expectedNodeSignerThumbprint: process.env.PINVOU_TRUSTED_E2E_NODE_SIGNER_THUMBPRINT,
    expectedNodeSignerPublisher: process.env.PINVOU_TRUSTED_E2E_NODE_SIGNER_PUBLISHER,
    expectedInstallerSha256: process.env.PINVOU_RELEASE_INSTALLER_SHA256,
    expectedInstallerVersion: process.env.PINVOU_RELEASE_VERSION,
    expectedAttestationPublicKeySha256: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_PUBLIC_KEY_SHA256,
    expectedAttestationProvider: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER,
    expectedAttestationKeyId: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_KEY_ID,
    expectedAttestationAclSha256: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_ACL_SHA256,
    expectedAttestationProviderMeasurementSha256: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_MEASUREMENT_SHA256,
    expectedAttestationProviderVendor: process.env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_VENDOR,
    expectedAttestationProviderRootCertificateSha256: process.env.PINVOU_TRUSTED_E2E_PROVIDER_ROOT_CERTIFICATE_SHA256,
    expectedAttestationProviderVerifierCertificateSha256: process.env.PINVOU_TRUSTED_E2E_PROVIDER_VERIFIER_CERTIFICATE_SHA256,
    expectedOrchestratorRolePolicySha256: process.env.PINVOU_TRUSTED_E2E_ORCHESTRATOR_ROLE_POLICY_SHA256,
    expectedARolePolicySha256: process.env.PINVOU_TRUSTED_E2E_A_ROLE_POLICY_SHA256,
    expectedBRolePolicySha256: process.env.PINVOU_TRUSTED_E2E_B_ROLE_POLICY_SHA256,
    expectedBuildProvenanceSha256: process.env.PINVOU_RELEASE_BUILD_PROVENANCE_SHA256,
    expectedBuildRepository: process.env.PINVOU_RELEASE_BUILD_REPOSITORY,
    expectedBuildWorkflow: process.env.PINVOU_RELEASE_BUILD_WORKFLOW,
    expectedBuildRunId: process.env.PINVOU_RELEASE_BUILD_RUN_ID,
    allowTestAttestation: false,
    });
  operation
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "evidence_validation_failed" })}\n`);
      process.exitCode = 1;
    });
}
