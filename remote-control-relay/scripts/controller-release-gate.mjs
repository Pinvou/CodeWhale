#!/usr/bin/env node
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parseControllerEnvironment } from "./deploy-controller-preflight.mjs";
import { validateReleaseEvidenceFile } from "./validate-controller-release-evidence.mjs";

const relayRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const repoRoot = dirname(relayRoot);

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function run(command, args, env = process.env) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { cwd: repoRoot, env, shell: false, windowsHide: true, stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    child.stderr.on("data", (chunk) => { stderr += String(chunk); });
    child.once("error", reject);
    child.once("close", (code) => code === 0 ? resolvePromise() : reject(Object.assign(new Error("release_subgate_failed"), { code: "release_subgate_failed", cause: stderr })));
  });
}

export async function validateTrustedGh(env) {
  const binary = env.PINVOU_TRUSTED_GH_PATH;
  if (!isAbsolute(binary || "")) fail("release_gh_tool_configuration_invalid");
  const metadata = await stat(resolve(binary)).catch(() => fail("release_gh_tool_configuration_invalid"));
  if (!metadata.isFile()) fail("release_gh_tool_configuration_invalid");
  return resolve(binary);
}

async function runTrustedGh(env, args) {
  const binary = await validateTrustedGh(env);
  return run(binary, args, buildReleaseSubprocessEnvironment(env, "attestation"));
}

const PROCESS_BASE_ENV = Object.freeze([
  "PATH", "Path", "PATHEXT", "SystemRoot", "SYSTEMROOT", "WINDIR", "ComSpec", "COMSPEC",
  "TEMP", "TMP", "TMPDIR", "HOME", "LANG", "LC_ALL", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
]);
const PURPOSE_ENV = Object.freeze({
  preflight: [],
  test: [],
  telemetry: ["PINVOU_RELEASE_COMMIT_SHA", "PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL", "PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE"],
  rollback: ["PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY", "PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON", "PINVOU_REMOTE_LEGACY_RELAY_URL", "PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST"],
  attestation: [],
});

export function buildReleaseSubprocessEnvironment(source, purpose) {
  if (!Object.hasOwn(PURPOSE_ENV, purpose)) fail("release_subprocess_environment_purpose_invalid");
  const result = {};
  for (const key of [...PROCESS_BASE_ENV, ...PURPOSE_ENV[purpose]]) {
    if (typeof source[key] === "string" && source[key] !== "") result[key] = source[key];
  }
  return result;
}

export async function runControllerReleaseGate(env = process.env) {
  const enabled = env.PINVOU_DESKTOP_CONTROLLER_ENABLED === "1";
  if (!enabled) {
    if (env.PINVOU_DESKTOP_CONTROLLER_ENABLED && env.PINVOU_DESKTOP_CONTROLLER_ENABLED !== "0") fail("release_feature_flag_invalid");
    return { ok: true, enabled: false, evidence_required: false };
  }
  const evidence = env.PINVOU_REMOTE_RELEASE_EVIDENCE;
  const commit = env.PINVOU_RELEASE_COMMIT_SHA;
  if (!evidence || !commit) fail("release_evidence_configuration_missing");
  const required = [
    "PINVOU_TRUSTED_WINDOWS_SIGNER_THUMBPRINT", "PINVOU_TRUSTED_WINDOWS_SIGNER_PUBLISHER",
    "PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY", "PINVOU_REMOTE_CONTROLLER_ENV_FILE",
    "PINVOU_REMOTE_GATEWAY_CONFIG_PATH",
    "PINVOU_REMOTE_CONTROLLER_DISABLED_ENV_FILE", "PINVOU_REMOTE_CONTROLLER_STATE_PATH",
    "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT", "PINVOU_TRUSTED_E2E_DRIVER_SHA256",
    "PINVOU_TRUSTED_E2E_NODE_SHA256", "PINVOU_TRUSTED_E2E_NODE_SIGNER_THUMBPRINT", "PINVOU_TRUSTED_E2E_NODE_SIGNER_PUBLISHER",
    "PINVOU_TRUSTED_E2E_ATTESTATION_PUBLIC_KEY_SHA256",
    "PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER", "PINVOU_TRUSTED_E2E_ATTESTATION_KEY_ID",
    "PINVOU_TRUSTED_E2E_ATTESTATION_ACL_SHA256", "PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_MEASUREMENT_SHA256",
    "PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_VENDOR", "PINVOU_TRUSTED_E2E_PROVIDER_ROOT_CERTIFICATE_SHA256", "PINVOU_TRUSTED_E2E_PROVIDER_VERIFIER_CERTIFICATE_SHA256",
    "PINVOU_TRUSTED_E2E_ORCHESTRATOR_ROLE_POLICY_SHA256", "PINVOU_TRUSTED_E2E_A_ROLE_POLICY_SHA256", "PINVOU_TRUSTED_E2E_B_ROLE_POLICY_SHA256",
    "PINVOU_RELEASE_BUILD_PROVENANCE_SHA256",
    "PINVOU_RELEASE_BUILD_REPOSITORY", "PINVOU_RELEASE_BUILD_WORKFLOW", "PINVOU_RELEASE_BUILD_RUN_ID",
    "PINVOU_TRUSTED_GH_PATH",
    "PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL", "PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE",
    "PINVOU_RELEASE_INSTALLER_SHA256", "PINVOU_RELEASE_VERSION",
  ];
  if (required.some((key) => !env[key])) fail("release_production_configuration_missing");
  if (env.PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT !== "self_hosted") fail("release_enrollment_deployment_invalid");
  const productionFiles = await Promise.all([
    stat(resolve(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH)),
    stat(resolve(env.PINVOU_REMOTE_CONTROLLER_ENV_FILE)),
    stat(resolve(env.PINVOU_REMOTE_CONTROLLER_DISABLED_ENV_FILE)),
  ]).catch(() => fail("release_production_state_unavailable"));
  if (productionFiles.some((item) => !item.isFile())) fail("release_production_state_unavailable");
  const inventoryBytes = await readFile(resolve(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY))
    .catch(() => fail("release_legacy_inventory_unavailable"));
  const inventorySha256 = crypto.createHash("sha256").update(inventoryBytes).digest("hex");
  const gatewayConfigBytes = await readFile(resolve(env.PINVOU_REMOTE_GATEWAY_CONFIG_PATH))
    .catch(() => fail("release_gateway_config_unavailable"));
  const gatewayConfigSha256 = crypto.createHash("sha256").update(gatewayConfigBytes).digest("hex");
  const validated = await validateReleaseEvidenceFile(resolve(evidence), {
    expectedCommit: commit,
    expectedSignerThumbprint: env.PINVOU_TRUSTED_WINDOWS_SIGNER_THUMBPRINT,
    expectedSignerPublisher: env.PINVOU_TRUSTED_WINDOWS_SIGNER_PUBLISHER,
    expectedInventorySha256: inventorySha256,
    expectedGatewayConfigSha256: gatewayConfigSha256,
    expectedEnrollmentDeployment: env.PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT,
    expectedDriverSha256: env.PINVOU_TRUSTED_E2E_DRIVER_SHA256,
    expectedNodeSha256: env.PINVOU_TRUSTED_E2E_NODE_SHA256,
    expectedNodeSignerThumbprint: env.PINVOU_TRUSTED_E2E_NODE_SIGNER_THUMBPRINT,
    expectedNodeSignerPublisher: env.PINVOU_TRUSTED_E2E_NODE_SIGNER_PUBLISHER,
    expectedInstallerSha256: env.PINVOU_RELEASE_INSTALLER_SHA256,
    expectedInstallerVersion: env.PINVOU_RELEASE_VERSION,
    expectedAttestationPublicKeySha256: env.PINVOU_TRUSTED_E2E_ATTESTATION_PUBLIC_KEY_SHA256,
    expectedAttestationProvider: env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER,
    expectedAttestationKeyId: env.PINVOU_TRUSTED_E2E_ATTESTATION_KEY_ID,
    expectedAttestationAclSha256: env.PINVOU_TRUSTED_E2E_ATTESTATION_ACL_SHA256,
    expectedAttestationProviderMeasurementSha256: env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_MEASUREMENT_SHA256,
    expectedAttestationProviderVendor: env.PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_VENDOR,
    expectedAttestationProviderRootCertificateSha256: env.PINVOU_TRUSTED_E2E_PROVIDER_ROOT_CERTIFICATE_SHA256,
    expectedAttestationProviderVerifierCertificateSha256: env.PINVOU_TRUSTED_E2E_PROVIDER_VERIFIER_CERTIFICATE_SHA256,
    expectedOrchestratorRolePolicySha256: env.PINVOU_TRUSTED_E2E_ORCHESTRATOR_ROLE_POLICY_SHA256,
    expectedARolePolicySha256: env.PINVOU_TRUSTED_E2E_A_ROLE_POLICY_SHA256,
    expectedBRolePolicySha256: env.PINVOU_TRUSTED_E2E_B_ROLE_POLICY_SHA256,
    expectedBuildProvenanceSha256: env.PINVOU_RELEASE_BUILD_PROVENANCE_SHA256,
    expectedBuildRepository: env.PINVOU_RELEASE_BUILD_REPOSITORY,
    expectedBuildWorkflow: env.PINVOU_RELEASE_BUILD_WORKFLOW,
    expectedBuildRunId: env.PINVOU_RELEASE_BUILD_RUN_ID,
    allowTestAttestation: false,
  });
  const evidenceRoot = dirname(resolve(evidence));
  await runTrustedGh(env, ["attestation", "verify", join(evidenceRoot, "binaries/pinvou-installer.exe"),
    "--bundle", join(evidenceRoot, "build/installer-attestation.sigstore.jsonl"), "--repo", env.PINVOU_RELEASE_BUILD_REPOSITORY,
    "--signer-workflow", env.PINVOU_RELEASE_BUILD_WORKFLOW, "--format", "json"]);
  await runTrustedGh(env, ["attestation", "verify", join(evidenceRoot, "build/installer-sbom.spdx.json"),
    "--bundle", join(evidenceRoot, "build/installer-sbom-attestation.sigstore.jsonl"), "--repo", env.PINVOU_RELEASE_BUILD_REPOSITORY,
    "--signer-workflow", env.PINVOU_RELEASE_BUILD_WORKFLOW, "--format", "json"]);
  await run(process.execPath, [join(relayRoot, "scripts/deploy-controller-preflight.mjs"), env.PINVOU_REMOTE_CONTROLLER_ENV_FILE], buildReleaseSubprocessEnvironment(env, "preflight"));
  await run(process.execPath, [join(relayRoot, "scripts/deploy-controller-preflight.mjs"), env.PINVOU_REMOTE_CONTROLLER_DISABLED_ENV_FILE, "--expect-feature-off"], buildReleaseSubprocessEnvironment(env, "preflight"));
  const enabledEntries = parseControllerEnvironment(await readFile(resolve(env.PINVOU_REMOTE_CONTROLLER_ENV_FILE), "utf8"));
  const disabledEntries = parseControllerEnvironment(await readFile(resolve(env.PINVOU_REMOTE_CONTROLLER_DISABLED_ENV_FILE), "utf8"));
  for (const [key, value] of enabledEntries) {
    if (key !== "PINVOU_DESKTOP_CONTROLLER_ENABLED" && disabledEntries.get(key) !== value) fail("release_feature_off_configuration_mismatch");
  }
  if (disabledEntries.size !== enabledEntries.size
    || enabledEntries.get("PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT") !== env.PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT
    || enabledEntries.get("PINVOU_REMOTE_CONTROLLER_STATE_PATH") !== resolve(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH)) {
    fail("release_production_configuration_mismatch");
  }
  await run(process.execPath, ["--test", "--test-concurrency=1", join(relayRoot, "test/controller-sensitive-scan.test.js")], buildReleaseSubprocessEnvironment(env, "test"));
  await run(process.execPath, ["--test", "--test-concurrency=1", join(relayRoot, "test/controller-telemetry.test.js")], buildReleaseSubprocessEnvironment(env, "test"));
  // Expected commit and telemetry/rollback endpoints come only from the
  // protected gate environment, never from an overlay of a Relay env file.
  await run(process.execPath, [join(relayRoot, "scripts/query-controller-release-telemetry.mjs")], buildReleaseSubprocessEnvironment(env, "telemetry"));
  await run(process.execPath, [join(repoRoot, "scripts/check-enrollment-gated-v2-rollback.mjs")], buildReleaseSubprocessEnvironment(env, "rollback"));
  return { ok: true, enabled: true, evidence_required: true, ...validated };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  runControllerReleaseGate()
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "controller_release_gate_failed" })}\n`);
      process.exitCode = 1;
    });
}
