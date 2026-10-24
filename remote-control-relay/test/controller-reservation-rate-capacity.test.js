import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";

const handshakePublicKeyB64 = crypto.generateKeyPairSync("x25519").publicKey
  .export({ type: "spki", format: "der" }).subarray(-32).toString("base64");

function harness(options = {}) {
  let now = 0;
  let request = 0;
  let lookups = 0;
  const events = [];
  const manager = new ControllerReservationManager({
    pairingStore: { findActiveByCode() { lookups += 1; return null; }, terminate: async () => {} },
    interactionSlots: new EndpointInteractionSlots(), clock: () => now, elapsedClock: () => 0,
    sourceLimit: 1_000_000, connectionLimit: 1_000_000, globalLimit: 1_000_000, windowMs: 100,
    audit: event => events.push(event), ...options,
  });
  return {
    time(value) { now = value; },
    attempt(source = "source-shared", socketIdentity = "socket-shared") {
      const before = lookups;
      const beforeEvents = events.length;
      assert.throws(() => manager.claim({
        pairingCode: "2345678", requestId: `request-${++request}`, source, socketIdentity,
        handshakePublicKeyB64,
      }), { code: "pairing_code_invalid" });
      assert.equal(events.length, beforeEvents + 1);
      const outcome = events.at(-1);
      assert.ok(["claim_invalid", "claim_rate_limited"].includes(outcome));
      assert.equal(lookups - before, outcome === "claim_invalid" ? 1 : 0,
        "audit must distinguish reaching the real pairing lookup from rate rejection");
      return outcome;
    },
  };
}

function table(kind, capacity, limit = 1_000_000) {
  const subject = harness(kind === "source"
    ? { maxSourceBuckets: capacity, maxConnectionBuckets: 1, sourceLimit: limit }
    : { maxConnectionBuckets: capacity, maxSourceBuckets: 1, connectionLimit: limit });
  return {
    time: subject.time,
    attempt: key => kind === "source"
      ? subject.attempt(`source-${key}`) : subject.attempt("source-shared", `socket-${key}`),
  };
}

for (const kind of ["source", "connection"]) {
  for (const capacity of [65, 4096]) {
    test(`${kind} capacity ${capacity} makes fair progress past renewed prefixes across repeated windows`, () => {
      const subject = table(kind, capacity);
      let keys = Array.from({ length: capacity }, (_, index) => `old-${index}`);
      for (const key of keys) assert.equal(subject.attempt(key), "claim_invalid");
      // Keep every bucket except the last one live. Existing-key renewal must
      // not hide that expired tail, including after replacement and wraparound.
      for (let round = 1; round <= 3; round += 1) {
        subject.time(round * 100);
        for (const key of keys.slice(0, -1)) assert.equal(subject.attempt(key), "claim_invalid");
        const replacement = `new-${round}`;
        let admitted = false;
        for (let attempt = 0; attempt < Math.ceil(capacity / 64); attempt += 1) {
          if (subject.attempt(replacement) === "claim_invalid") { admitted = true; break; }
        }
        assert.equal(admitted, true, "bounded sweeps eventually reach reusable capacity");
        keys = [...keys.slice(0, -1), replacement];
      }
    });
  }

  test(`${kind} full active table retains every bucket and its consumed allowance`, () => {
    const subject = table(kind, 65, 2);
    for (let index = 0; index < 65; index += 1) {
      assert.equal(subject.attempt(`active-${index}`), "claim_invalid");
    }
    assert.equal(subject.attempt("active-0"), "claim_invalid");
    for (let index = 0; index < 130; index += 1) {
      assert.equal(subject.attempt(`rejected-${index}`), "claim_rate_limited");
    }
    assert.equal(subject.attempt("active-0"), "claim_rate_limited", "sweeping never resets an exhausted bucket");
    assert.equal(subject.attempt("active-1"), "claim_invalid");
    assert.equal(subject.attempt("active-1"), "claim_rate_limited", "sweeping also preserves partial counts");
  });
}

for (const dimension of ["sourceLimit", "connectionLimit", "globalLimit"]) {
  test(`${dimension} retains the fixed window boundary and rejects wall-clock rollback`, () => {
    const subject = harness({ [dimension]: 1 });
    assert.equal(subject.attempt(), "claim_invalid");
    subject.time(99);
    assert.equal(subject.attempt(), "claim_rate_limited");
    subject.time(100);
    assert.equal(subject.attempt(), "claim_invalid");
    subject.time(50);
    assert.equal(subject.attempt(), "claim_rate_limited");
    subject.time(199);
    assert.equal(subject.attempt(), "claim_rate_limited");
    subject.time(200);
    assert.equal(subject.attempt(), "claim_invalid");
    subject.time(201);
    assert.equal(subject.attempt(), "claim_rate_limited");
  });
}

test("source and connection refusals preserve the existing downstream charging order", () => {
  const source = harness({ sourceLimit: 1, connectionLimit: 1, globalLimit: 2 });
  assert.equal(source.attempt("source-1", "socket-1"), "claim_invalid");
  assert.equal(source.attempt("source-1", "socket-2"), "claim_rate_limited");
  assert.equal(source.attempt("source-2", "socket-2"), "claim_invalid",
    "a source refusal must not spend connection or global allowance");
  assert.equal(source.attempt("source-3", "socket-3"), "claim_rate_limited");

  const connection = harness({ sourceLimit: 2, connectionLimit: 1, globalLimit: 2 });
  assert.equal(connection.attempt("source-1", "socket-1"), "claim_invalid");
  assert.equal(connection.attempt("source-2", "socket-1"), "claim_rate_limited");
  assert.equal(connection.attempt("source-2", "socket-2"), "claim_invalid",
    "a connection refusal spends its source allowance but not global allowance");
  assert.equal(connection.attempt("source-2", "socket-3"), "claim_rate_limited");
  assert.equal(connection.attempt("source-3", "socket-3"), "claim_rate_limited");
});
