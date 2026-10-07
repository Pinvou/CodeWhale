import assert from "node:assert/strict";
import { createDecipheriv, createHash, createPrivateKey, createPublicKey, diffieHellman, hkdfSync, verify } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";
import * as controllerProtocol from "../protocol-controller.js";

import {
  CONTROLLER_CAPABILITIES,
  CONTROLLER_CONTRACT_SHA256,
  CONTROLLER_ERROR_CODES,
  CONTROLLER_MESSAGE_TYPES,
  CONTROLLER_PAYLOAD_TYPES,
  TASK_STATUSES,
  ControllerProtocolError,
  LeaseInstallValidationContext,
  ParsedTranscript,
  RegistrationValidationContext,
  LEGACY_WEB_CONFLICT,
  MAX_CONTROL_MESSAGE_BYTES,
  MAX_DECRYPTED_PAYLOAD_BYTES,
  MAX_ENCRYPTED_FRAME_BYTES,
  MAX_TASK_INPUT_TEXT_BYTES,
  SEALED_CODE_V1_BYTES,
  CONTROLLER_SENSITIVE_FIELDS,
  applyLivenessDeadline,
  buildDirectLivenessAad,
  buildLivenessAttestationPreimage,
  buildPairingTranscriptPreimage,
  buildReceiptCommitmentPreimage,
  buildSealedCodeAad,
  canonicalControllerContractHash,
  classifyLivenessAttestation,
  classifyMonotonicSequence,
  controllerAllowedRoles,
  controllerCapabilityHash,
  controllerLogProjection,
  negotiateControllerCapabilities,
  parseControllerPairingTranscript,
  parseControllerWireJson,
  parseSealedCodeV1,
  receiptCommitmentHex,
  validateControllerBaseUrl,
  validateControllerLivenessPayload,
  validateControllerLeaseInstallMessage,
  validateControllerMessage,
  validateControllerPayload,
  validateControllerRegistrationMessage,
  validateControllerRuntimeMessage,
  validateControllerRuntimePayload,
  verifyReceiptCommitment,
} from "../protocol-controller.js";

const fixtureUrl = new URL("./fixtures/controller-protocol-v1.json", import.meta.url);
const fixture = JSON.parse(await readFile(fixtureUrl, "utf8"));
const ZERO_HASH = "0".repeat(64);
const ZERO_KEY = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
const ZERO_NONCE = "AAAAAAAAAAAAAAAAAAAAAA==";
const LEASE = Object.freeze({
  connection_id: "connection-0001",
  controller_lease_id: "lease-0001",
  lease_epoch: "1",
  relay_epoch: "epoch-test-1",
  remote_control_generation: "generation-0001",
});
const RUNTIME = Object.freeze({ lease: LEASE, negotiated_capabilities: Object.freeze([...CONTROLLER_CAPABILITIES].sort()) });

function assertProtocolError(expectedCode, callback, label, expectedField) {
  assert.throws(callback, (error) => error instanceof ControllerProtocolError
    && error.code === expectedCode
    && (expectedField === undefined || error.message === `${expectedCode}: ${expectedField}`), label);
}

function lp(value) {
  const bytes = Buffer.isBuffer(value) ? value : Buffer.from(String(value), "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

function fixturePublicKey(rawBase64) {
  const prefix = Buffer.from("302a300506032b6570032100", "hex");
  return createPublicKey({ key: Buffer.concat([prefix, Buffer.from(rawBase64, "base64")]), format: "der", type: "spki" });
}

function fixtureX25519PrivateKey(rawHex) {
  const prefix = Buffer.from("302e020100300506032b656e04220420", "hex");
  return createPrivateKey({ key: Buffer.concat([prefix, Buffer.from(rawHex, "hex")]), format: "der", type: "pkcs8" });
}

function validMessage(type, role = fixture.message_role_matrix[type][0]) {
  const common = { v: 1, type };
  const request = { request_id: "request-0001" };
  const pairing = { pairing_id: "pairing-0001", reservation_id: "reservation-0001", remote_control_generation: LEASE.remote_control_generation, relay_epoch: LEASE.relay_epoch };
  const activation = { ...LEASE, activation_id: "activation-0001" };
  switch (type) {
    case "controller_capabilities": return { ...common, ...request, client_role: role === "A" ? "desktop_controller" : "desktop_controlled_endpoint", sender_role: role, protocol_version: 1, capabilities: ["pairing_v1"] };
    case "desktop_endpoint_claim": return { ...common, ...request, endpoint_id: "endpoint-0001", enrollment_proof_b64: "AQID" };
    case "desktop_pairing_register": return structuredClone(fixture.pairing_registration_vector.request);
    case "desktop_pairing_registered": return structuredClone(fixture.pairing_registration_vector.result);
    case "desktop_pairing_revoke": return { ...common, ...request, endpoint_id: "endpoint-0001", remote_control_generation: LEASE.remote_control_generation, reason: "refreshed", pairing_id: "pairing-0001" };
    case "controller_pairing_claim": return { ...common, ...request, claim_request_id: "claim-0001", pairing_code: "2345678", handshake_public_key_b64: ZERO_KEY, key_algorithm: "x25519", capabilities: ["pairing_v1"] };
    case "controller_pairing_reserved": return { ...common, ...request, ...pairing, transcript: fixture.transcript_vector.transcript, transcript_hash: fixture.transcript_vector.sha256, relay_signing_key_id: "relay-key-test-1", signature_algorithm: "ed25519", reservation_expires_at_ms: "10000", handshake_expires_at_ms: "20000", relay_signature_b64: fixture.transcript_vector.signature_b64 };
    case "finished_receipt_commit": return { ...common, ...request, ...pairing, transcript_hash: fixture.transcript_vector.sha256, sender_role: role, commitment: ZERO_HASH };
    case "encrypted_handshake_finished": return { ...common, ...request, ...pairing, transcript_hash: fixture.transcript_vector.sha256, sender_role: role, ciphertext_b64: "AQID", ciphertext_bytes: "3" };
    case "finished_receipt_ack": return { ...common, ...request, ...pairing, transcript_hash: fixture.transcript_vector.sha256, sender_role: role === "A" ? "B" : "A", receiver_role: role, receipt_secret_b64: ZERO_KEY };
    case "controller_joined": return structuredClone(fixture.lease_install_vector.message);
    case "controller_liveness": return { ...common, ...LEASE, sender_role: role, liveness_epoch: role === "A" ? "live-a-1" : "live-b-1", heartbeat_sequence: "1" };
    case "controller_liveness_challenge_a": return { ...common, ...request, ...LEASE, challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
    case "encrypted_controller_liveness_a": return structuredClone(fixture.direct_liveness_vector.outer);
    case "controller_liveness_attestation_request_a": return { ...common, ...request, ...LEASE, challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
    case "controller_liveness_attestation_request_b": return { ...common, ...request, ...LEASE, challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
    case "controller_liveness_attestation_a": return structuredClone(fixture.attestation_vector.message);
    case "controller_liveness_attestation_b": return structuredClone(fixture.peer_b_attestation_vector.message);
    case "controller_resume_prepare": return { ...common, ...request, ...LEASE, sender_role: role, resume_token_b64: "AQID", transport_challenge_b64: ZERO_NONCE, original_deadline_ms: "30000" };
    case "controller_resume_provisional": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "encrypted_resume_confirm": return { ...common, ...request, ...activation, sender_role: role, ciphertext_b64: "AQID", ciphertext_bytes: "3" };
    case "controller_resume_activation_ack": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "controller_resume_activation_pending": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "controller_post_activation_liveness":
    case "controller_post_activation_liveness_confirmed": return { ...common, ...request, ...activation, liveness_epoch_b: "live-b-2", heartbeat_sequence_b: "1" };
    case "controller_resume_ready_ack": return { ...common, ...request, ...activation, sender_role: role, liveness_epoch_b: "live-b-2", heartbeat_sequence_b: "1" };
    case "controller_resumed": return { ...common, ...request, ...activation, liveness_epoch_a: "live-a-2", liveness_epoch_b: "live-b-2", new_resume_token_b64: "AQID", resume_deadline_ms: "60000" };
    case "encrypted_frame": return { ...common, ...LEASE, frame_sequence: "1", direction: role === "A" ? "a_to_b" : "b_to_a", payload_kind: role === "A" ? "task_submit" : "task_status", ciphertext_b64: "AQID", ciphertext_bytes: "3" };
    case "controller_disconnect": return { ...common, ...request, ...LEASE, sender_role: role, reason: "local_stop" };
    case "error": return { ...common, ...request, code: "pairing_code_invalid", message: "连接码无效或已失效" };
    default: throw new Error(`missing message fixture for ${type}`);
  }
}

function validPayload(type) {
  const base = { v: 1, type, ...LEASE };
  switch (type) {
    case "capability_snapshot_request": return { ...base, request_id: "request-capability-1", session_id: "session-ref-0001", known_revision: "0" };
    case "capability_snapshot": return structuredClone(fixture.capability_snapshot_vector);
    case "capability_snapshot_error": return { ...base, request_id: "request-capability-1", session_id: "session-1", code: "capability_snapshot_unavailable" };
    case "task_submit": return structuredClone(fixture.task_submit_vectors.controlled);
    case "task_cancel": return { ...base, request_id: "request-1", task_id: "task-1", session_id: "session-1" };
    case "task_status": return { ...base, request_id: "request-1", task_id: "task-1", session_id: "session-1", task_status: "succeeded", result_digest: ZERO_HASH };
    case "remote_action_request": return { ...base, request_id: "request-1", session_id: "session-1", operation: "permission_list", body: { task_id: "task-1" } };
    case "remote_action_result": return { ...base, request_id: "request-1", session_id: "session-1", operation: "permission_list", ok: true, body: { permissions: [] } };
    case "session_snapshot_request": return { ...base, request_id: "request-1", session_id: "session-1" };
    case "session_snapshot": return { ...base, request_id: "request-1", session_id: "session-1", journal_epoch: "journal-1", base_sequence: "1", snapshot_b64: "AQID", snapshot_bytes: "3", sha256: ZERO_HASH };
    case "journal_replay_request": return { ...base, request_id: "request-1", session_id: "session-1", journal_epoch: "journal-1", after_sequence: "1" };
    case "journal_event": return { ...base, session_id: "session-1", journal_epoch: "journal-1", event_sequence: "2", event_kind: "assistant_delta", event_b64: "AQID", event_bytes: "3" };
    default: throw new Error(`missing payload fixture for ${type}`);
  }
}

test("controller v1 fixture freezes registries and contract hash", () => {
  assert.deepEqual(CONTROLLER_CAPABILITIES, fixture.expected_capabilities);
  assert.deepEqual(CONTROLLER_ERROR_CODES, fixture.expected_error_codes);
  assert.deepEqual(CONTROLLER_MESSAGE_TYPES, fixture.expected_message_types);
  assert.deepEqual(CONTROLLER_PAYLOAD_TYPES, fixture.expected_payload_types);
  assert.notEqual(CONTROLLER_CONTRACT_SHA256, "TO_BE_REPLACED");
  assert.equal(canonicalControllerContractHash(fixture), CONTROLLER_CONTRACT_SHA256);
  assert.equal(fixture.contract.sha256, CONTROLLER_CONTRACT_SHA256);
  const changedFixture = structuredClone(fixture);
  changedFixture.legacy_web_v2.web_client_join.after_seq = 8;
  assert.notEqual(canonicalControllerContractHash(changedFixture), CONTROLLER_CONTRACT_SHA256);
});

test("protocol documentation anchors match the executable registries and digests", async () => {
  const document = await readFile(new URL("../PROTOCOL.md", import.meta.url), "utf8");
  const capabilityList = document.match(/The frozen capability values are ([\s\S]*?)\. Unknown capabilities/u)?.[1];
  const errorList = document.match(/Controller error envelopes use only these codes: ([\s\S]*?)\.\r?\nMalformed schema/u)?.[1];
  assert.ok(capabilityList);
  assert.ok(errorList);
  const quoted = (value) => [...value.matchAll(/`([^`]+)`/gu)].map((match) => match[1]);
  assert.deepEqual(quoted(capabilityList), CONTROLLER_CAPABILITIES);
  assert.deepEqual(quoted(errorList), CONTROLLER_ERROR_CODES);
  assert.match(document, new RegExp("Its complete-v1 digest is\\s+`" + fixture.capability_vector.sha256 + "`"));
  assert.match(document, new RegExp("The contract digest is\\s+`" + CONTROLLER_CONTRACT_SHA256 + "`"));
  assert.match(document, /standalone declaration, so clients must not send it to the live router/u);
  assert.match(document, /A's `controller_pairing_claim`\s+carry their capability lists/u);
  const artifactSection = document.match(/The `artifact_index` operation([\s\S]*?)The shared fixture/u)?.[1];
  assert.ok(artifactSection, 'artifact_index must be documented as a remote action');
  assert.match(artifactSection, /`body\.artifacts`[\s\S]*?`remote_action_result`/u);
  const artifactFields = artifactSection.match(/Each item is exactly `([^`]+)`/u)?.[1];
  assert.ok(artifactFields);
  const documentedArtifactFields = artifactFields.split(',').map((field) => field.trim());
  assert.deepEqual(documentedArtifactFields,
    ['artifact_id', 'task_id', 'display_name', 'mime_type', 'total_bytes', 'sha256', 'preview_kind']);
  const item = { artifact_id: 'artifact-1', task_id: 'task-1', display_name: 'report.txt',
    mime_type: 'text/plain', total_bytes: '3', sha256: ZERO_HASH, preview_kind: 'text' };
  const result = { ...validPayload('remote_action_result'), operation: 'artifact_index',
    body: { artifacts: [item] } };
  assert.deepEqual(Object.keys(item), documentedArtifactFields);
  validateControllerPayload(result, 'B', LEASE);
  for (const field of documentedArtifactFields) {
    const missing = structuredClone(result);
    delete missing.body.artifacts[0][field];
    assertProtocolError('missing_field', () => validateControllerPayload(missing, 'B', LEASE), field);
  }
  const previewKinds = artifactSection.match(/preview kinds are ([\s\S]*?)\. Subsequent/u)?.[1];
  assert.ok(previewKinds);
  assert.deepEqual(quoted(previewKinds), ['text', 'download_only', 'unsupported']);
  for (const previewKind of quoted(previewKinds)) {
    const variant = structuredClone(result);
    variant.body.artifacts[0].preview_kind = previewKind;
    validateControllerPayload(variant, 'B', LEASE);
  }
  assert.match(artifactSection, /`remote_action_request`[\s\S]*?`artifact_id`[\s\S]*?`task_id`[\s\S]*?`transfer_id`/u);
  assert.doesNotMatch(artifactSection, /size_bytes|preview_mode|inline_text|inline_image/u);
});

test('artifact remote-action validator rejects legacy fields mixed into the current shape', () => {
  const result = { ...validPayload('remote_action_result'), operation: 'artifact_index', body: {
    artifacts: [{ artifact_id: 'artifact-1', task_id: 'task-1', display_name: 'report.txt',
      mime_type: 'text/plain', total_bytes: '3', sha256: ZERO_HASH, preview_kind: 'text' }],
  } };
  validateControllerPayload(result, 'B', LEASE);
  for (const [field, value] of [['size_bytes', '3'], ['preview_mode', 'inline_text'],
    ['inline_text', 'old content'], ['inline_image', 'AQID']]) {
    const mixed = structuredClone(result);
    mixed.body.artifacts[0][field] = value;
    assertProtocolError('unknown_field', () => validateControllerPayload(mixed, 'B', LEASE), field);
  }
  for (const previewKind of ['inline_text', 'inline_image']) {
    const legacyEnum = structuredClone(result);
    legacyEnum.body.artifacts[0].preview_kind = previewKind;
    assertProtocolError('invalid_enum', () => validateControllerPayload(legacyEnum, 'B', LEASE), previewKind);
  }
});

test("raw controller JSON rejects duplicate keys before object construction", () => {
  for (const item of fixture.duplicate_raw_json_examples) {
    const validator = item.target === "message" ? validateControllerMessage : validateControllerPayload;
    assertProtocolError(item.expected_error, () => parseControllerWireJson(item.raw_json), item.name);
    assertProtocolError(item.expected_error, () => validator(item.raw_json, item.sender_role), item.name);
  }
});

test("every outer message has an exhaustive transport-role matrix", () => {
  assert.deepEqual(Object.keys(fixture.message_role_matrix), fixture.expected_message_types);
  for (const type of CONTROLLER_MESSAGE_TYPES) {
    const allowed = fixture.message_role_matrix[type];
    assert.deepEqual(controllerAllowedRoles(type), allowed, type);
    for (const role of ["A", "B", "relay"]) {
      if (allowed.includes(role)) assert.deepEqual(validateControllerMessage(validMessage(type, role), role), validMessage(type, role), `${type}/${role}`);
      else assertProtocolError("role_mismatch", () => validateControllerMessage(validMessage(type), role));
    }
  }
});

test("pairing registration has a stable generation-scoped idempotency key", () => {
  const { request, result } = fixture.pairing_registration_vector;
  const expected = { ...LEASE, registration_request_id: request.registration_request_id };
  validateControllerMessage(request, "B", expected);
  validateControllerMessage(result, "relay", expected);
  const retry = fixture.pairing_registration_vector.retry_request;
  const retryResult = fixture.pairing_registration_vector.retry_result;
  const failureResult = fixture.pairing_registration_vector.failure_result;
  const failureRetryResult = fixture.pairing_registration_vector.failure_retry_result;
  validateControllerMessage(retry, "B", expected);
  validateControllerMessage(retryResult, "relay", expected);
  validateControllerMessage(failureResult, "relay");
  validateControllerMessage(failureRetryResult, "relay");
  assert.equal(retry.registration_request_id, request.registration_request_id);
  const { request_id: ignoredResultRequest, ...sealedResult } = result;
  const { request_id: ignoredRetryRequest, ...sealedRetryResult } = retryResult;
  assert.deepEqual(sealedRetryResult, sealedResult);
  const { request_id: ignoredFailureRequest, ...sealedFailure } = failureResult;
  const { request_id: ignoredFailureRetryRequest, ...sealedFailureRetry } = failureRetryResult;
  assert.deepEqual(sealedFailureRetry, sealedFailure);
  assertProtocolError("stale_context", () => validateControllerMessage(
    { ...retry, registration_request_id: "registration-request-other" }, "B",
    expected,
  ));
  assertProtocolError("missing_field", () => {
    const missing = { ...request };
    delete missing.registration_request_id;
    validateControllerMessage(missing, "B");
  });
  const successContext = new RegistrationValidationContext(request, result);
  validateControllerRegistrationMessage(retry, "B", successContext);
  validateControllerRegistrationMessage(retryResult, "relay", successContext);
  for (const field of fixture.pairing_registration_vector.conflict_fields) {
    const conflict = structuredClone(retry);
    conflict[field] = field === "capabilities" ? ["pairing_v1"] : field.endsWith("_b64") ? (conflict[field] === ZERO_KEY ? "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" : ZERO_KEY) : `${conflict[field]}-other`;
    assertProtocolError(fixture.pairing_registration_vector.conflict_expected_error, () => validateControllerRegistrationMessage(conflict, "B", successContext));
  }
  const failureContext = new RegistrationValidationContext(request, failureResult);
  validateControllerRegistrationMessage(failureRetryResult, "relay", failureContext);
  for (const field of ["registration_request_id", "remote_control_generation"]) {
    const missingResult = structuredClone(failureRetryResult);
    delete missingResult[field];
    assertProtocolError("missing_field", () => validateControllerRegistrationMessage(missingResult, "relay", failureContext));
  }
});

test("every decrypted payload is strict, role bound and lease bound", () => {
  for (const type of CONTROLLER_PAYLOAD_TYPES) {
    const payload = validPayload(type);
    const role = ["capability_snapshot_request", "task_submit", "task_cancel", "remote_action_request", "session_snapshot_request", "journal_replay_request"].includes(type) ? "A" : "B";
    assert.deepEqual(validateControllerPayload(payload, role, LEASE), payload, type);
    assertProtocolError("role_mismatch", () => validateControllerPayload(payload, role === "A" ? "B" : "A", LEASE));
    const stale = { ...payload, remote_control_generation: "generation-old" };
    assertProtocolError("stale_context", () => validateControllerPayload(stale, role, LEASE));
  }
});

test("task submission carries bounded input, exact statuses and the full temporary model package", () => {
  assert.deepEqual(TASK_STATUSES, ["admitted", "running", "waiting_permission", "succeeded", "failed", "cancelled", "interrupted", "result_unknown"]);
  assert.deepEqual(validateControllerPayload(fixture.task_submit_vectors.controlled, "A", LEASE), fixture.task_submit_vectors.controlled);
  assert.deepEqual(validateControllerPayload(fixture.task_submit_vectors.temporary, "A", LEASE), fixture.task_submit_vectors.temporary);
  assertProtocolError("invalid_field", () => validateControllerPayload({ ...fixture.task_submit_vectors.temporary, mode: "code" }, "A", LEASE));
  const status = validPayload("task_status");
  for (const task_status of TASK_STATUSES) validateControllerPayload({ ...status, task_status }, "B", LEASE);
  assertProtocolError("invalid_enum", () => validateControllerPayload({ ...status, task_status: "completed" }, "B", LEASE));
  assertProtocolError("invalid_field", () => validateControllerPayload({ ...fixture.task_submit_vectors.controlled, input: { text: "", attachment_ids: [], skill_ids: [] } }, "A", LEASE));
  assertProtocolError("invalid_field", () => validateControllerPayload({ ...fixture.task_submit_vectors.controlled, input: { text: "x".repeat(MAX_TASK_INPUT_TEXT_BYTES + 1), attachment_ids: [], skill_ids: [] } }, "A", LEASE));
  const nestedUnknown = structuredClone(fixture.task_submit_vectors.temporary);
  nestedUnknown.temporary_model_package.primary.cookie = "forbidden";
  assertProtocolError("unknown_field", () => validateControllerPayload(nestedUnknown, "A", LEASE));
  const wrongBinding = structuredClone(fixture.task_submit_vectors.temporary);
  wrongBinding.temporary_model_package.binding.task_id = "task-other";
  assertProtocolError("stale_context", () => validateControllerPayload(wrongBinding, "A", LEASE));
  const oauth = structuredClone(fixture.task_submit_vectors.temporary);
  oauth.temporary_model_package.primary_credential.kind = "oauth";
  assertProtocolError("invalid_enum", () => validateControllerPayload(oauth, "A", LEASE));
  const oversizedPackage = structuredClone(fixture.task_submit_vectors.temporary);
  oversizedPackage.temporary_model_package.primary_credential.secret = "x".repeat(65_536);
  assertProtocolError("message_too_large", () => validateControllerPayload(oversizedPackage, "A", LEASE));
});

test("temporary model URLs follow the shared strict ASCII grammar", () => {
  for (const url of fixture.base_url_vectors.valid) assert.equal(validateControllerBaseUrl(url), url);
  for (const url of fixture.base_url_vectors.invalid) assertProtocolError("invalid_field", () => validateControllerBaseUrl(url));
  const exactBoundary = `https://a/${"x".repeat(2038)}`;
  assert.equal(Buffer.byteLength(exactBoundary), 2048);
  assert.equal(validateControllerBaseUrl(exactBoundary), exactBoundary);
  assertProtocolError("invalid_field", () => validateControllerBaseUrl(`${exactBoundary}x`));
});

test("runtime capability and artifact metadata stay minimal, revision-bound and path-free", () => {
  const capability = validPayload("capability_snapshot");
  validateControllerPayload(capability, "B", LEASE);
  assert.deepEqual(capability.skills[0].scopes, ["chat", "code"]);
  assertProtocolError("invalid_field", () => validateControllerPayload({
    ...capability,
    skills: [{ ...capability.skills[0], scopes: [] }],
  }, "B", LEASE));
  assertProtocolError("invalid_field", () => validateControllerPayload({
    ...capability,
    skills: [{ ...capability.skills[0], scopes: ["chat", "chat"] }],
  }, "B", LEASE));
  for (const field of ["session_id", "controlled_session_id"]) {
    const missing = { ...capability };
    delete missing[field];
    assertProtocolError("missing_field", () => validateControllerPayload(missing, "B", LEASE));
  }
  const requestWithoutRef = validPayload("capability_snapshot_request");
  delete requestWithoutRef.session_id;
  assertProtocolError("missing_field", () => validateControllerPayload(requestWithoutRef, "A", LEASE));
  const errorWithoutRef = validPayload("capability_snapshot_error");
  delete errorWithoutRef.session_id;
  assertProtocolError("missing_field", () => validateControllerPayload(errorWithoutRef, "B", LEASE));
  const leakedCapability = structuredClone(capability);
  leakedCapability.workspaces[0].path = "C:\\private";
  assertProtocolError("unknown_field", () => validateControllerPayload(leakedCapability, "B", LEASE));
  const artifact = { ...validPayload("remote_action_result"), operation: "artifact_index", body: {
    artifacts: [{ artifact_id: "artifact-1", task_id: "task-1", display_name: "report.txt", mime_type: "text/plain",
      total_bytes: "3", sha256: ZERO_HASH, preview_kind: "text" }],
  } };
  validateControllerPayload(artifact, "B", LEASE);
  const leakedArtifact = structuredClone(artifact);
  leakedArtifact.body.artifacts[0].absolute_path = "C:\\private\\report.md";
  assertProtocolError("unknown_field", () => validateControllerPayload(leakedArtifact, "B", LEASE));
  assertProtocolError("invalid_enum", () => validateControllerPayload({ ...capability, modes: ["future"] }, "B", LEASE));
});

test("post-lease messages reject stale generation and epochs, including late frames", () => {
  for (const type of ["controller_liveness", "controller_resume_prepare", "controller_resume_activation_ack", "controller_resumed", "encrypted_frame", "controller_disconnect"]) {
    const role = fixture.message_role_matrix[type][0];
    const message = validMessage(type, role);
    validateControllerMessage(message, role, LEASE);
    for (const [field, stale] of [["remote_control_generation", "generation-old"], ["relay_epoch", "epoch-old"], ["lease_epoch", "0"]]) {
      assertProtocolError("stale_context", () => validateControllerMessage({ ...message, [field]: stale }, role, LEASE));
    }
  }
  assertProtocolError("role_mismatch", () => validateControllerMessage({ ...validMessage("encrypted_frame", "A"), payload_kind: "task_status" }, "A", LEASE));
  assertProtocolError("missing_field", () => validateControllerMessage(validMessage("error"), "relay", LEASE));
  validateControllerMessage({ ...validMessage("error"), ...LEASE }, "relay", LEASE);
});

test("JSON counters and timestamps use canonical decimal strings", () => {
  const frame = validMessage("encrypted_frame", "A");
  for (const invalid of [1, 1.0, 1e0, "01", "18446744073709551616", "-1"]) {
    assertProtocolError("invalid_field", () => validateControllerMessage({ ...frame, frame_sequence: invalid }, "A"));
  }
  validateControllerMessage({ ...frame, frame_sequence: "18446744073709551615" }, "A");
});

test("capability and signed pairing transcript vectors are canonical and tamper evident", () => {
  const capabilities = fixture.capability_vector;
  const capabilityPreimage = Buffer.concat([Buffer.from("pinvou-controller-capabilities-v1", "ascii"), ...[...capabilities.capabilities].sort().map(lp)]);
  assert.equal(capabilityPreimage.toString("hex"), capabilities.preimage_hex);
  assert.equal(controllerCapabilityHash(capabilities.capabilities), capabilities.sha256);
  const [subset, empty] = fixture.capability_negotiation_vectors;
  const negotiated = negotiateControllerCapabilities(subset.capabilities_a, subset.capabilities_b);
  assert.deepEqual(negotiated, subset.negotiated);
  assert.equal(controllerCapabilityHash(negotiated), subset.capability_hash);
  assertProtocolError(empty.expected_error, () => negotiateControllerCapabilities(empty.capabilities_a, empty.capabilities_b));
  const vector = fixture.transcript_vector;
  const preimage = buildPairingTranscriptPreimage(vector.transcript);
  assert.equal(preimage.toString("hex"), vector.preimage_hex);
  assert.equal(createHash("sha256").update(preimage).digest("hex"), vector.sha256);
  assert.equal(verify(null, preimage, fixturePublicKey(vector.public_key_b64), Buffer.from(vector.signature_b64, "base64")), true);
  const verifiedReserved = validMessage("controller_pairing_reserved", "relay");
  assert.ok(parseControllerPairingTranscript(verifiedReserved) instanceof ParsedTranscript);
  assert.equal(buildPairingTranscriptPreimage(structuredClone(vector.transcript)).toString("hex"), preimage.toString("hex"), "A and B reconstruct identical bytes");
  assert.equal(controllerCapabilityHash(vector.transcript.negotiated_capabilities), vector.transcript.capability_hash);
  const reserved = validMessage("controller_pairing_reserved");
  assertProtocolError("invalid_field", () => validateControllerMessage({ ...reserved, transcript: { ...reserved.transcript, endpoint_id: "endpoint-tampered" } }, "relay"));
  const tamperedPreimage = buildPairingTranscriptPreimage({ ...vector.transcript, endpoint_id: "endpoint-tampered" });
  assert.equal(verify(null, tamperedPreimage, fixturePublicKey(vector.public_key_b64), Buffer.from(vector.signature_b64, "base64")), false);
  assertProtocolError("invalid_field", () => buildPairingTranscriptPreimage({ ...vector.transcript, negotiated_capabilities: [...vector.transcript.negotiated_capabilities].reverse() }));
  assertProtocolError("invalid_field", () => buildPairingTranscriptPreimage({ ...vector.transcript, negotiated_capabilities: ["pairing_v1"], capability_hash: vector.transcript.capability_hash }));
  assertProtocolError("unsupported_capability", () => buildPairingTranscriptPreimage({ ...vector.transcript, negotiated_capabilities: [], capability_hash: controllerCapabilityHash([]) }));

  const onlyPairing = { lease: LEASE, negotiated_capabilities: ["pairing_v1"] };
  assertProtocolError("unsupported_capability", () => validateControllerRuntimePayload(validPayload("task_submit"), "A", onlyPairing));
  assertProtocolError("unsupported_capability", () => validateControllerRuntimeMessage(validMessage("encrypted_frame", "A"), "A", onlyPairing));
  validateControllerRuntimePayload(validPayload("task_submit"), "A", { lease: LEASE, negotiated_capabilities: ["task_v1"] });
  assertProtocolError("unsupported_capability", () => validateControllerRuntimePayload(
    fixture.task_submit_vectors.temporary,
    "A",
    { lease: LEASE, negotiated_capabilities: ["task_v1"] },
  ), undefined, "temporary_model_package_v1");
  validateControllerRuntimePayload(
    fixture.task_submit_vectors.temporary,
    "A",
    { lease: LEASE, negotiated_capabilities: ["task_v1", "temporary_model_package_v1"] },
  );
});

test("runtime validation requires negotiated outer and inner capabilities", () => {
  validateControllerMessage(validMessage("controller_capabilities", "A"), "A");
  validateControllerMessage(validMessage("error"), "relay");
  for (const item of fixture.runtime_capability_cases) {
    const value = item.target === "message" ? validMessage(item.type, item.sender_role) : validPayload(item.type);
    const validator = item.target === "message" ? validateControllerRuntimeMessage : validateControllerRuntimePayload;
    const context = item.capabilities === undefined ? undefined : {
      ...(item.omit_lease ? {} : { lease: { ...LEASE, ...(item.lease_overrides ?? {}) } }),
      negotiated_capabilities: item.capabilities,
    };
    assertProtocolError(item.expected_error, () => validator(value, item.sender_role, context), item.name, item.expected_field);
  }
  validateControllerRuntimeMessage(validMessage("encrypted_frame", "A"), "A", { lease: LEASE, negotiated_capabilities: ["encrypted_frames_v1", "task_v1"] });
  validateControllerRuntimeMessage(validMessage("controller_liveness", "A"), "A", RUNTIME);
  validateControllerRuntimePayload(validPayload("task_submit"), "A", RUNTIME);
  const preLease = fixture.runtime_pre_lease_case;
  validateControllerRuntimeMessage(validMessage(preLease.type, preLease.sender_role), preLease.sender_role, { negotiated_capabilities: preLease.capabilities });
  validateControllerRuntimeMessage(validMessage("controller_capabilities", "A"), "A", { negotiated_capabilities: [] });
  validateControllerRuntimeMessage(validMessage("error"), "relay", { negotiated_capabilities: [] });
  for (const item of fixture.invalid_runtime_capability_contexts) {
    assertProtocolError(item.expected_error, () => validateControllerRuntimeMessage(
      validMessage("controller_capabilities", "A"),
      "A",
      { negotiated_capabilities: item.capabilities },
    ), item.name, item.expected_field);
  }
});

test("remote actions require their operation capability in both encrypted directions", () => {
  const cases = [
    ["permission_list", { task_id: "task-1" }, { permissions: [] }, "permission_v1"],
    ["attachment_abort", { task_id: "task-1", transfer_id: "ctr-up-1" }, { aborted: true }, "attachment_v1"],
    ["artifact_index", {}, { artifacts: [] }, "artifact_v1"],
  ];
  for (const [operation, requestBody, resultBody, capability] of cases) {
    for (const [type, role, body, ok] of [
      ["remote_action_request", "A", requestBody, undefined],
      ["remote_action_result", "B", resultBody, true],
    ]) {
      const payload = { v: 1, type, ...LEASE, request_id: "request-action-1",
        session_id: "session-ref-1", operation, body, ...(ok === undefined ? {} : { ok }) };
      const base = { lease: LEASE, negotiated_capabilities: ["task_v1"] };
      assertProtocolError("unsupported_capability", () => validateControllerRuntimePayload(payload, role, base),
        `${type}/${operation} must not borrow task_v1`);
      validateControllerRuntimePayload(payload, role, {
        lease: LEASE, negotiated_capabilities: ["task_v1", capability].sort(),
      });
    }
  }
});

test("controller_joined has no DEV-00 production path from untrusted data to a lease context", () => {
  const vector = fixture.lease_install_vector;
  const verification = vector.verification_input;
  assertProtocolError("missing_field", () => validateControllerLeaseInstallMessage(vector.message, "relay"), undefined, "lease_install_validation_context");
  assert.equal("VerifiedTranscript" in controllerProtocol, false);
  assert.equal("verifyControllerPairingTranscript" in controllerProtocol, false);
  assert.equal("PinnedRelayKey" in controllerProtocol, false);
  assert.equal("RelayTrustStore" in controllerProtocol, false);
  assert.equal("loadRelayTrustStoreFromEnrollmentConfig" in controllerProtocol, false);
  for (const candidate of fixture.untrusted_authentication_inputs) {
    assertProtocolError("invalid_field", () => new LeaseInstallValidationContext({ verified_transcript: candidate.value }), candidate.name, "lease_install_validation_context.verified_transcript");
  }
  assertProtocolError("missing_field", () => validateControllerLeaseInstallMessage(
    vector.message,
    "relay",
    Object.create(LeaseInstallValidationContext.prototype),
  ), "prototype forgery cannot impersonate the context", "lease_install_validation_context");
  const parsed = parseControllerPairingTranscript(verification.reserved_message);
  assert.ok(parsed instanceof ParsedTranscript, "parsing is explicitly not an authentication token");
  assertProtocolError("invalid_field", () => new LeaseInstallValidationContext({ verified_transcript: parsed }), "parsed transcript cannot impersonate an authenticated transcript", "lease_install_validation_context.verified_transcript");
  const trustedPublicKey = fixturePublicKey(fixture.transcript_vector.public_key_b64);
  for (const mutation of vector.unauthenticated_transcript_mutations) {
    const message = {
      ...verification.reserved_message,
      transcript: mutation.transcript,
      transcript_hash: mutation.transcript_hash,
      relay_signature_b64: mutation.relay_signature_b64,
    };
    assert.ok(parseControllerPairingTranscript(message) instanceof ParsedTranscript, `${mutation.name} remains only parsed`);
    const mutationPreimage = buildPairingTranscriptPreimage(mutation.transcript);
    assert.equal(createHash("sha256").update(mutationPreimage).digest("hex"), mutation.transcript_hash, mutation.name);
    assert.equal(verify(null, mutationPreimage, trustedPublicKey, Buffer.from(mutation.relay_signature_b64, "base64")), false, `${mutation.name} requires DEV-05 authentication rejection`);
  }
});

test("finished receipt commitment is built independently from fields", () => {
  const vector = fixture.receipt_commitment_vector;
  const binding = {
    relay_epoch: vector.relay_epoch,
    pairing_id: vector.pairing_id,
    reservation_id: vector.reservation_id,
    transcript_hash: vector.transcript_hash,
    sender_role: vector.sender_role,
  };
  const secret = Buffer.from(vector.secret_hex, "hex");
  const preimage = buildReceiptCommitmentPreimage(binding, secret);
  assert.equal(preimage.toString("hex"), vector.preimage_hex);
  assert.equal(receiptCommitmentHex(binding, secret), vector.commitment_hex);
  assert.equal(verifyReceiptCommitment(binding, secret, vector.commitment_hex), true);
  assert.equal(verifyReceiptCommitment({ ...binding, sender_role: "B" }, secret, vector.commitment_hex), false);
  assert.equal(verifyReceiptCommitment(binding, Buffer.alloc(32, 7), vector.commitment_hex), false);
  assertProtocolError("invalid_field", () => buildReceiptCommitmentPreimage(binding, Buffer.alloc(31)));
  assertProtocolError("invalid_field", () => verifyReceiptCommitment(binding, secret, "00"));
});

test("sealed_code_v1 is independently reconstructed and decrypted by Node crypto", () => {
  const vector = fixture.sealed_code_vector;
  const parsed = parseSealedCodeV1(vector.sealed_code_b64);
  assert.equal(SEALED_CODE_V1_BYTES, 68);
  assert.equal(parsed.version, vector.version);
  assert.equal(parsed.relay_ephemeral_public_key.toString("hex"), vector.relay_public_hex);
  assert.equal(parsed.nonce.toString("hex"), vector.nonce_hex);
  assert.equal(parsed.ciphertext_and_tag.toString("hex"), vector.ciphertext_hex);

  const receiverPrivate = fixtureX25519PrivateKey(vector.receiver_private_hex);
  const relayPublicPrefix = Buffer.from("302a300506032b656e032100", "hex");
  const relayPublic = createPublicKey({
    key: Buffer.concat([relayPublicPrefix, parsed.relay_ephemeral_public_key]),
    format: "der",
    type: "spki",
  });
  const shared = diffieHellman({ privateKey: receiverPrivate, publicKey: relayPublic });
  assert.equal(shared.toString("hex"), vector.shared_secret_hex);
  const aad = buildSealedCodeAad(vector.binding);
  assert.equal(aad.toString("hex"), vector.aad_hex);
  const info = Buffer.concat([
    Buffer.from("pinvou-code-wrap-key-v1", "ascii"),
    ...[
      vector.binding.registration_request_id,
      vector.binding.pairing_id,
      vector.binding.remote_control_generation,
      vector.binding.relay_epoch,
      vector.binding.expires_at_ms,
    ].map(lp),
  ]);
  const key = Buffer.from(hkdfSync("sha256", shared, Buffer.from("pinvou-code-wrap-salt-v1", "ascii"), info, 32));
  assert.equal(key.toString("hex"), vector.derived_key_hex);
  const decrypt = (candidate, candidateAad = aad) => {
    const ciphertext = candidate.ciphertext_and_tag.subarray(0, -16);
    const tag = candidate.ciphertext_and_tag.subarray(-16);
    const decipher = createDecipheriv("aes-256-gcm", key, candidate.nonce);
    decipher.setAAD(candidateAad);
    decipher.setAuthTag(tag);
    return Buffer.concat([decipher.update(ciphertext), decipher.final()]);
  };
  assert.equal(decrypt(parsed).toString("utf8"), vector.pairing_code);

  const reusedNonceEnvelope = Buffer.from(vector.sealed_code_b64, "base64");
  reusedNonceEnvelope[45] ^= 1;
  const sameKeyAndNonceDifferentCiphertext = parseSealedCodeV1(reusedNonceEnvelope.toString("base64"));
  assert.deepEqual(sameKeyAndNonceDifferentCiphertext.nonce, parsed.nonce);
  assert.throws(() => decrypt(sameKeyAndNonceDifferentCiphertext));
  const changedNonce = { ...parsed, nonce: Buffer.from(parsed.nonce) };
  changedNonce.nonce[0] ^= 1;
  assert.throws(() => decrypt(changedNonce));
  assert.throws(() => decrypt(parsed, buildSealedCodeAad({ ...vector.binding, pairing_id: "pairing-other" })));

  const tampered = Buffer.from(vector.sealed_code_b64, "base64");
  tampered[0] = 2;
  assertProtocolError("unsupported_version", () => parseSealedCodeV1(tampered.toString("base64")));
  assertProtocolError("invalid_field", () => parseSealedCodeV1(Buffer.alloc(67).toString("base64")));
});

test("direct A-to-B liveness exposes routing only and binds strict inner data via AAD", () => {
  const vector = fixture.direct_liveness_vector;
  validateControllerMessage(vector.outer, "A", LEASE);
  assert.deepEqual(Object.keys(vector.outer), ["v", "type", "connection_id", "controller_lease_id", "lease_epoch", "ciphertext_b64", "ciphertext_bytes"]);
  const expected = { ...LEASE, liveness_epoch_a: "live-a-1", last_heartbeat_sequence_a: "1", challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
  assert.deepEqual(validateControllerLivenessPayload(vector.inner, expected), vector.inner);
  const aad = buildDirectLivenessAad(vector.aad_binding);
  assert.equal(aad.toString("hex"), vector.aad_preimage_hex);
  assert.equal(createHash("sha256").update(aad).digest("hex"), vector.aad_sha256);
  assert.equal(classifyMonotonicSequence("1", "1", "heartbeat_sequence_a"), "same_sequence_tighten");
  assert.equal(classifyMonotonicSequence("2", "1", "heartbeat_sequence_a"), "advance");
  validateControllerLivenessPayload({ ...vector.inner, heartbeat_sequence_a: "1" }, expected);
  assertProtocolError("replay", () => validateControllerLivenessPayload({ ...vector.inner, heartbeat_sequence_a: "0" }, expected));
  assertProtocolError("stale_context", () => validateControllerLivenessPayload({ ...vector.inner, controller_lease_id: "lease-other" }, expected));
  assertProtocolError("stale_context", () => validateControllerLivenessPayload({ ...vector.inner, remote_control_generation: "generation-old" }, expected));
  assertProtocolError("unknown_field", () => validateControllerMessage({ ...vector.outer, challenge_id: "leak" }, "A"));
});

test("Relay liveness attestation vector binds time, generation, epoch, challenge and lease", () => {
  const vector = fixture.attestation_vector;
  const expectation = { ...LEASE, liveness_epoch_a: "live-a-1", challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE, last_heartbeat_sequence_a: "1", last_attestation_sequence: "0" };
  validateControllerMessage(vector.message, "relay", expectation);
  const preimage = buildLivenessAttestationPreimage(vector.message);
  assert.equal(preimage.toString("hex"), vector.preimage_hex);
  assert.equal(createHash("sha256").update(preimage).digest("hex"), vector.sha256);
  const key = fixturePublicKey(vector.public_key_b64);
  assert.equal(verify(null, preimage, key, Buffer.from(vector.message.relay_signature_b64, "base64")), true);
  for (const [field, value] of [["issued_at_relay", "9001"], ["remote_control_generation", "generation-old"], ["relay_epoch", "epoch-old"], ["controller_lease_id", "lease-other"], ["challenge_b64", "AQEBAQEBAQEBAQEBAQEBAQ=="]]) {
    assert.equal(verify(null, buildLivenessAttestationPreimage({ ...vector.message, [field]: value }), key, Buffer.from(vector.message.relay_signature_b64, "base64")), false, field);
  }
  assert.deepEqual(classifyLivenessAttestation(vector.message, expectation), {
    heartbeat_sequence: "advance", attestation_sequence: "advance", deadline_policy: "replace",
  });
  assert.deepEqual(classifyLivenessAttestation(vector.message, { ...expectation, last_heartbeat_sequence_a: "2" }), {
    heartbeat_sequence: "same_sequence_tighten", attestation_sequence: "advance", deadline_policy: "tighten_only",
  });
  assertProtocolError("replay", () => classifyLivenessAttestation(vector.message, { ...expectation, last_attestation_sequence: "1" }));
  assert.equal(applyLivenessDeadline("20000", "25000", "tighten_only"), "20000");
  assert.equal(applyLivenessDeadline("20000", "25000", "replace"), "25000");
  assertProtocolError("replay", () => validateControllerMessage(vector.message, "relay", { ...expectation, last_attestation_sequence: "1" }));
  assertProtocolError("replay", () => validateControllerMessage({ ...vector.message, heartbeat_sequence_a: "0" }, "relay", expectation));
  assertProtocolError("replay", () => validateControllerMessage({ ...vector.message, attestation_sequence: "0" }, "relay", { ...expectation, last_attestation_sequence: "1" }));
  assertProtocolError("stale_context", () => validateControllerMessage({ ...vector.message, challenge_id: "challenge-other" }, "relay", expectation));
});

test("strict validation covers missing, unknown, enums, version/capability combinations and sizes", () => {
  for (const item of fixture.invalid_wire_examples) {
    const validator = item.target === "message" ? validateControllerMessage : validateControllerPayload;
    assertProtocolError(item.expected_error, () => validator(item.value, item.sender_role), item.name);
  }
  const claim = validMessage("controller_pairing_claim");
  const { request_id: ignored, ...missing } = claim;
  assertProtocolError("missing_field", () => validateControllerMessage(missing, "A"));
  assertProtocolError("unknown_field", () => validateControllerMessage({ ...claim, future: true }, "A"));
  assertProtocolError("unknown_message_type", () => validateControllerMessage({ v: 1, type: "future" }, "A"));
  assertProtocolError("unsupported_version", () => validateControllerMessage({ ...validMessage("controller_capabilities", "A"), v: 2 }, "A"));
  assertProtocolError("unsupported_version", () => validateControllerMessage({ ...validMessage("controller_capabilities", "A"), protocol_version: 2 }, "A"));
  assertProtocolError("unsupported_capability", () => validateControllerMessage({ ...validMessage("controller_capabilities", "A"), capabilities: ["future_v2"] }, "A"));
  assertProtocolError("role_mismatch", () => validateControllerMessage(validMessage("desktop_pairing_register"), "A"));
  const message = validMessage("controller_capabilities", "A");
  assertProtocolError("message_too_large", () => validateControllerMessage(`${JSON.stringify(message)}${" ".repeat(MAX_CONTROL_MESSAGE_BYTES)}`, "A"));
  const frame = validMessage("encrypted_frame", "A");
  frame.ciphertext_b64 = Buffer.alloc(MAX_ENCRYPTED_FRAME_BYTES + 1).toString("base64");
  frame.ciphertext_bytes = String(MAX_ENCRYPTED_FRAME_BYTES + 1);
  assertProtocolError("invalid_field", () => validateControllerMessage(frame, "A"));
  const payload = validPayload("task_cancel");
  assertProtocolError("message_too_large", () => validateControllerPayload(`${JSON.stringify(payload)}${" ".repeat(MAX_DECRYPTED_PAYLOAD_BYTES)}`, "A"));
  assert.deepEqual(new Set(fixture.negative_contracts), new Set(["missing_field", "unknown_field", "overlong_text", "unknown_message_type", "unsupported_version", "unsupported_capability", "capability_version_mismatch", "invalid_enum", "cross_role", "noncanonical_integer", "integer_overflow", "stale_generation", "stale_relay_epoch", "stale_lease_epoch", "late_frame", "transcript_tamper", "liveness_tamper", "liveness_regression", "same_sequence_tighten", "cross_lease", "attestation_tamper", "url_grammar", "registration_idempotency", "missing_runtime_lease"]));
});

test("public pairing errors and real legacy WebUI v2 fixture stay stable", () => {
  for (const cause of ["pairing_absent", "pairing_expired", "pairing_consumed", "pairing_revoked", "generation_stale"]) assert.equal(fixture.public_error_mappings[cause], "pairing_code_invalid");
  assert.equal(fixture.public_error_mappings.generation_exhausted, "pairing_code_generation_failed");
  assert.deepEqual(LEGACY_WEB_CONFLICT, fixture.legacy_web_v2.conflict);
  assert.deepEqual(fixture.legacy_web_v2.desktop_endpoint_register, {
    v: 2, type: "desktop_endpoint_register", endpoint_id: "endpoint_fixture", access_token: "fixture-access-token", desktop_secret: "fixture-desktop-secret",
  });
  assert.deepEqual(fixture.legacy_web_v2.desktop_endpoint_registered, {
    v: 2, type: "desktop_endpoint_registered", endpoint_id: "endpoint_fixture", web_client_connected: false, lease_id: null,
  });
  assert.deepEqual(fixture.legacy_web_v2.web_client_join, {
    v: 2, type: "web_client_join", endpoint_id: "endpoint_fixture", access_token: "fixture-access-token", stream_epoch: "stream-fixture", after_seq: 7,
  });
  assert.deepEqual(fixture.legacy_web_v2.web_client_joined, {
    v: 2, type: "web_client_joined", endpoint_id: "endpoint_fixture", lease_id: "lease_fixture", desktop_connected: true,
  });
  assert.equal(fixture.legacy_web_v2.controller_conflict_expectations.none, "accept_v2");
  for (const state of ["pairing_reserved", "handshake_pending", "controller_connected", "reconnecting", "resume_provisional", "resume_activated_pending_liveness"]) {
    assert.deepEqual(fixture.legacy_web_v2.controller_conflict_expectations[state], fixture.legacy_web_v2.conflict);
  }
  assert.equal(fixture.legacy_web_v2.conflict.message.message.length > 0, true);
});

test("log projection excludes routing, keys, tokens, model credentials and content", () => {
  const source = { ...validMessage("controller_joined"), pairing_code: "2345678", ciphertext_b64: "AQID", credential_ref: "secret-ref", temporary_model_package: fixture.task_submit_vectors.temporary.temporary_model_package };
  assert.deepEqual(controllerLogProjection(source), { v: 1, type: "controller_joined", request_id: "request-0001" });
  const serialized = JSON.stringify(controllerLogProjection(source));
  for (const forbidden of ["connection-0001", "lease-0001", "generation-0001", "2345678", "AQID", "secret-ref", "fixture-not-a-real-secret"]) assert.equal(serialized.includes(forbidden), false);
  const allSensitive = Object.fromEntries(CONTROLLER_SENSITIVE_FIELDS.map((field) => [field, `secret-${field}`]));
  assert.deepEqual(controllerLogProjection({ v: 1, type: "controller_joined", ...allSensitive }), { v: 1, type: "controller_joined" });
});

test("controller v1 is explicitly gated beside the unchanged live v2 dispatcher", async () => {
  const server = await readFile(new URL("../server.js", import.meta.url), "utf8");
  assert.equal(server.includes("protocol-controller"), true);
  assert.equal(server.includes("PINVOU_DESKTOP_CONTROLLER_ENABLED"), true);
  assert.equal(server.includes("msg.v === 1 && ws.protocol === CONTROLLER_SUBPROTOCOL"), true);
  assert.equal(server.includes("ws.protocol !== CONTROLLER_SUBPROTOCOL"), true);
  assert.equal(server.includes("msg.v !== 2"), true);
  for (const type of ["desktop_endpoint_register", "desktop_endpoint_registered", "web_client_join", "web_client_joined"]) {
    assert.equal(server.includes(`\"${type}\"`), true, `live v2 contains ${type}`);
  }
  assert.equal(server.includes('type: "client_join"'), false);
  assert.equal(server.includes('type: "desktop_registered"'), false);
});
