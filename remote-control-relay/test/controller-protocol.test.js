import assert from "node:assert/strict";
import { createHash, createPublicKey, verify } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  CONTROLLER_CAPABILITIES,
  CONTROLLER_CONTRACT_SHA256,
  CONTROLLER_ERROR_CODES,
  CONTROLLER_MESSAGE_TYPES,
  CONTROLLER_PAYLOAD_TYPES,
  TASK_STATUSES,
  ControllerProtocolError,
  RegistrationValidationContext,
  LEGACY_WEB_CONFLICT,
  MAX_CONTROL_MESSAGE_BYTES,
  MAX_DECRYPTED_PAYLOAD_BYTES,
  MAX_ENCRYPTED_FRAME_BYTES,
  MAX_TASK_INPUT_TEXT_BYTES,
  CONTROLLER_SENSITIVE_FIELDS,
  applyLivenessDeadline,
  buildDirectLivenessAad,
  buildLivenessAttestationPreimage,
  buildPairingTranscriptPreimage,
  canonicalControllerContractHash,
  classifyLivenessAttestation,
  classifyMonotonicSequence,
  controllerAllowedRoles,
  controllerCapabilityHash,
  controllerLogProjection,
  negotiateControllerCapabilities,
  validateControllerBaseUrl,
  validateControllerLivenessPayload,
  validateControllerMessage,
  validateControllerPayload,
  validateControllerRegistrationMessage,
  validateControllerRuntimeMessage,
  validateControllerRuntimePayload,
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
const RUNTIME = Object.freeze({ lease: LEASE, negotiated_capabilities: CONTROLLER_CAPABILITIES });

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
    case "controller_joined": return { ...common, ...request, ...LEASE, liveness_epoch_a: "live-a-1", liveness_epoch_b: "live-b-1", resume_token_b64: "AQID", resume_deadline_ms: "30000" };
    case "controller_liveness": return { ...common, ...LEASE, sender_role: role, liveness_epoch: role === "A" ? "live-a-1" : "live-b-1", heartbeat_sequence: "1" };
    case "controller_liveness_challenge_a": return { ...common, ...request, ...LEASE, challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
    case "encrypted_controller_liveness_a": return structuredClone(fixture.direct_liveness_vector.outer);
    case "controller_liveness_attestation_request_a": return { ...common, ...request, ...LEASE, challenge_id: "challenge-0001", challenge_b64: ZERO_NONCE };
    case "controller_liveness_attestation_a": return structuredClone(fixture.attestation_vector.message);
    case "controller_resume_prepare": return { ...common, ...request, ...LEASE, sender_role: role, resume_token_b64: "AQID", transport_challenge_b64: ZERO_NONCE, original_deadline_ms: "30000" };
    case "controller_resume_provisional": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "encrypted_resume_confirm": return { ...common, ...request, ...activation, sender_role: role, ciphertext_b64: "AQID", ciphertext_bytes: "3" };
    case "controller_resume_activation_ack": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "controller_resume_activation_pending": return { ...common, ...request, ...activation, original_deadline_ms: "30000" };
    case "controller_post_activation_liveness":
    case "controller_post_activation_liveness_confirmed": return { ...common, ...request, ...activation, liveness_epoch_b: "live-b-2", heartbeat_sequence_b: "1" };
    case "controller_resume_ready_ack": return { ...common, ...request, ...activation, sender_role: role, liveness_epoch_b: "live-b-2", heartbeat_sequence_b: "1" };
    case "controller_resumed": return { ...common, ...request, ...activation, liveness_epoch_a: "live-a-2", liveness_epoch_b: "live-b-2", new_resume_token_b64: "AQID" };
    case "encrypted_frame": return { ...common, ...LEASE, frame_sequence: "1", direction: role === "A" ? "a_to_b" : "b_to_a", payload_kind: role === "A" ? "task_submit" : "task_status", ciphertext_b64: "AQID", ciphertext_bytes: "3" };
    case "controller_disconnect": return { ...common, ...request, ...LEASE, sender_role: role, reason: "local_stop" };
    case "error": return { ...common, ...request, code: "pairing_code_invalid", message: "连接码无效或已失效" };
    default: throw new Error(`missing message fixture for ${type}`);
  }
}

function validPayload(type) {
  const base = { v: 1, type, ...LEASE };
  switch (type) {
    case "capability_snapshot_request": return { ...base, request_id: "request-capability-1", known_revision: "0" };
    case "capability_snapshot": return structuredClone(fixture.capability_snapshot_vector);
    case "task_submit": return structuredClone(fixture.task_submit_vectors.controlled);
    case "task_cancel": return { ...base, request_id: "request-1", task_id: "task-1", session_id: "session-1" };
    case "task_status": return { ...base, request_id: "request-1", task_id: "task-1", session_id: "session-1", task_status: "succeeded", result_digest: ZERO_HASH };
    case "permission_request": return { ...base, task_id: "task-1", permission_request_id: "permission-1", expires_at_ms: "1000", operation_type: "write_file", risk_level: "high", target_scope: "workspace", preview: "write file" };
    case "permission_decision": return { ...base, task_id: "task-1", permission_request_id: "permission-1", request_id: "request-1", decision: "allow" };
    case "attachment_begin": return { ...base, task_id: "task-1", session_id: "session-1", transfer_id: "transfer-1", file_name: "input.txt", mime_type: "text/plain", total_bytes: "3", sha256: ZERO_HASH };
    case "attachment_chunk": return { ...base, task_id: "task-1", session_id: "session-1", transfer_id: "transfer-1", chunk_index: "0", chunk_b64: "AQID", chunk_bytes: "3" };
    case "attachment_commit": return { ...base, task_id: "task-1", session_id: "session-1", transfer_id: "transfer-1", sha256: ZERO_HASH };
    case "artifact_index_request": return { ...base, request_id: "request-1", session_id: "session-1" };
    case "artifact_index": return structuredClone(fixture.artifact_index_vector);
    case "artifact_read_request": return { ...base, request_id: "request-1", session_id: "session-1", artifact_id: "artifact-1", offset: "0", max_bytes: "1024" };
    case "artifact_chunk": return { ...base, request_id: "request-1", session_id: "session-1", artifact_id: "artifact-1", offset: "0", chunk_b64: "AQID", chunk_bytes: "3", eof: true, sha256: ZERO_HASH };
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
    const role = ["capability_snapshot_request", "task_submit", "task_cancel", "permission_decision", "attachment_begin", "attachment_chunk", "attachment_commit", "artifact_index_request", "artifact_read_request", "session_snapshot_request", "journal_replay_request"].includes(type) ? "A" : "B";
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
  const leakedCapability = structuredClone(capability);
  leakedCapability.workspaces[0].path = "C:\\private";
  assertProtocolError("unknown_field", () => validateControllerPayload(leakedCapability, "B", LEASE));
  const artifact = validPayload("artifact_index");
  validateControllerPayload(artifact, "B", LEASE);
  const leakedArtifact = structuredClone(artifact);
  leakedArtifact.artifacts[0].absolute_path = "C:\\private\\report.md";
  assertProtocolError("unknown_field", () => validateControllerPayload(leakedArtifact, "B", LEASE));
  assertProtocolError("invalid_enum", () => validateControllerPayload({ ...capability, modes: ["future"] }, "B", LEASE));
});

test("post-lease messages reject stale generation and epochs, including late frames", () => {
  for (const type of ["controller_joined", "controller_liveness", "controller_resume_prepare", "controller_resume_activation_ack", "controller_resumed", "encrypted_frame", "controller_disconnect"]) {
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
});

test("runtime validation requires negotiated outer and inner capabilities", () => {
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
});

test("finished receipt commitment is built independently from fields", () => {
  const vector = fixture.receipt_commitment_vector;
  const preimage = Buffer.concat([Buffer.from("pinvou-finished-receipt-commit-v1", "ascii"), lp(vector.relay_epoch), lp(vector.pairing_id), lp(vector.reservation_id), lp(vector.transcript_hash), lp(vector.sender_role), lp(Buffer.from(vector.secret_hex, "hex"))]);
  assert.equal(preimage.toString("hex"), vector.preimage_hex);
  assert.equal(createHash("sha256").update(preimage).digest("hex"), vector.commitment_hex);
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

test("controller v1 schema is not wired into the live v2 Relay dispatcher", async () => {
  const server = await readFile(new URL("../server.js", import.meta.url), "utf8");
  assert.equal(server.includes("protocol-controller"), false);
  assert.equal(server.includes("controller_pairing_claim"), false);
  for (const type of ["desktop_endpoint_register", "desktop_endpoint_registered", "web_client_join", "web_client_joined"]) {
    assert.equal(server.includes(`\"${type}\"`), true, `live v2 contains ${type}`);
  }
  assert.equal(server.includes('type: "client_join"'), false);
  assert.equal(server.includes('type: "desktop_registered"'), false);
});
