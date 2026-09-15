import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  CONTROLLER_MESSAGE_TYPES,
  CONTROLLER_PAYLOAD_TYPES,
  ControllerProtocolError,
  LEGACY_WEB_CONFLICT,
  MAX_CONTROL_MESSAGE_BYTES,
  MAX_DECRYPTED_PAYLOAD_BYTES,
  MAX_ENCRYPTED_FRAME_BYTES,
  CONTROLLER_SENSITIVE_FIELDS,
  controllerLogProjection,
  validateControllerMessage,
  validateControllerPayload,
} from "../protocol-controller.js";

const fixtureUrl = new URL("./fixtures/controller-protocol-v1.json", import.meta.url);
const fixture = JSON.parse(await readFile(fixtureUrl, "utf8"));

function assertProtocolError(expectedCode, callback) {
  assert.throws(callback, (error) => error instanceof ControllerProtocolError && error.code === expectedCode);
}

function lengthPrefix(value) {
  const bytes = Buffer.isBuffer(value) ? value : Buffer.from(value, "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

test("controller v1 fixture freezes the complete message and payload registries", () => {
  assert.deepEqual([...CONTROLLER_MESSAGE_TYPES].sort(), [...fixture.expected_message_types].sort());
  assert.deepEqual([...CONTROLLER_PAYLOAD_TYPES].sort(), [...fixture.expected_payload_types].sort());
});

test("controller v1 accepts every shared valid fixture", () => {
  for (const item of fixture.valid_messages) {
    assert.deepEqual(validateControllerMessage(item.message, item.sender_role), item.message, item.name);
  }
  for (const item of fixture.valid_payloads) {
    assert.deepEqual(validateControllerPayload(item.payload, item.sender_role), item.payload, item.name);
  }
});

test("controller v1 rejects shared unknown, enum, length and cross-role fixtures", () => {
  for (const item of fixture.invalid_messages) {
    assertProtocolError(item.expected_error, () => validateControllerMessage(item.message, item.sender_role));
  }
  for (const item of fixture.invalid_payloads) {
    assertProtocolError(item.expected_error, () => validateControllerPayload(item.payload, item.sender_role));
  }
});

test("finished receipt commitment has one canonical cross-language vector", () => {
  const vector = fixture.receipt_commitment_vector;
  const preimage = Buffer.concat([
    Buffer.from("pinvou-finished-receipt-commit-v1", "ascii"),
    lengthPrefix(vector.relay_epoch),
    lengthPrefix(vector.pairing_id),
    lengthPrefix(vector.reservation_id),
    lengthPrefix(vector.transcript_hash),
    lengthPrefix(vector.sender_role),
    lengthPrefix(Buffer.from(vector.secret_hex, "hex")),
  ]);
  assert.equal(preimage.length, 194);
  assert.equal(preimage.toString("hex"), vector.preimage_hex);
  assert.equal(createHash("sha256").update(preimage).digest("hex"), vector.commitment_hex);
});

test("wire and decrypted payload size limits use encoded bytes", () => {
  assertProtocolError("invalid_type", () => validateControllerMessage(Buffer.from("binary"), "A"));

  const message = fixture.valid_messages.find((item) => item.name === "capabilities_a").message;
  const padded = `${JSON.stringify(message)}${" ".repeat(MAX_CONTROL_MESSAGE_BYTES)}`;
  assertProtocolError("message_too_large", () => validateControllerMessage(padded, "A"));

  const frame = structuredClone(fixture.valid_messages.find((item) => item.name === "encrypted_frame").message);
  frame.ciphertext_b64 = Buffer.alloc(MAX_ENCRYPTED_FRAME_BYTES + 1).toString("base64");
  frame.ciphertext_bytes = MAX_ENCRYPTED_FRAME_BYTES + 1;
  assertProtocolError("invalid_field", () => validateControllerMessage(frame, "A"));

  const payload = fixture.valid_payloads.find((item) => item.name === "task_cancel").payload;
  const paddedPayload = `${JSON.stringify(payload)}${" ".repeat(MAX_DECRYPTED_PAYLOAD_BYTES)}`;
  assertProtocolError("message_too_large", () => validateControllerPayload(paddedPayload, "A"));
});

test("log projection excludes routing, pairing, key, token and content fields", () => {
  const source = {
    ...fixture.valid_messages.find((item) => item.name === "joined").message,
    pairing_code: "2345678",
    ciphertext_b64: "AQID",
    credential_ref: "secret-ref",
  };
  assert.deepEqual(controllerLogProjection(source), { v: 1, type: "controller_joined", request_id: "req-9" });
  const serialized = JSON.stringify(controllerLogProjection(source));
  for (const forbidden of ["connection-1", "lease-1", "generation-1", "2345678", "AQID", "secret-ref"]) {
    assert.equal(serialized.includes(forbidden), false);
  }
  const allSensitive = Object.fromEntries(CONTROLLER_SENSITIVE_FIELDS.map((field) => [field, `secret-${field}`]));
  assert.deepEqual(controllerLogProjection({ v: 1, type: "controller_joined", ...allSensitive }), { v: 1, type: "controller_joined" });
  assert.deepEqual(controllerLogProjection({
    v: 1,
    type: "\nforged-log-line",
    request_id: "x".repeat(129),
    sender_role: "relay",
    code: "future_error",
  }), { v: 1 });
});

test("legacy WebUI conflict mapping remains the exact v2 envelope", () => {
  assert.deepEqual(LEGACY_WEB_CONFLICT, fixture.legacy_web_conflict);
});

test("controller v1 schema is not wired into the live v2 Relay dispatcher", async () => {
  const server = await readFile(new URL("../server.js", import.meta.url), "utf8");
  assert.equal(server.includes("protocol-controller"), false);
  assert.equal(server.includes("controller_pairing_claim"), false);
});
