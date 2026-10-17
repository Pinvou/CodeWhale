import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { PairingStore } from "../pairing-store.js";
import { sealPairingCode } from "../pairing-code-sealer.js";

// Fault injection is at the persistence port, not a copied reservation state
// machine. Registration, termination and invalidation use production modules.
class FaultStateStore {
  revision = "0";
  value = null;
  failWrites = false;
  attempts = 0;
  beforeFailure = () => {};
  readNamespace() {
    return { namespace_revision: this.revision, value: structuredClone(this.value) };
  }
  async compareAndSwap(_name, expected, value) {
    await new Promise((resolve) => setImmediate(resolve));
    this.attempts += 1;
    if (this.failWrites) {
      this.beforeFailure();
      throw Object.assign(new Error("injected persistence failure"), { code: "persist_failed" });
    }
    assert.equal(expected, this.revision);
    this.revision = String(Number(this.revision) + 1);
    this.value = structuredClone(value);
    return { namespace_revision: this.revision, value: structuredClone(value) };
  }
}

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

async function fixture({ sample = 0, reservationBytes = crypto.randomBytes,
  onInvalidated = async () => {} } = {}) {
  let wall = 1000;
  let monotonic = 0;
  let clockFault = null;
  let entropy = 0;
  const state = new FaultStateStore();
  const pairings = await PairingStore.open({
    stateStore: state,
    digestKeyring: { activeKeyId: "digest-fixture", keys: [{ keyId: "digest-fixture", key: "test-only-digest-material-at-least-32-bytes" }] },
    clock: () => wall,
    randomBytes: (size) => size === 16 ? Buffer.alloc(size, entropy++) : crypto.randomBytes(size),
  });
  const slots = new EndpointInteractionSlots();
  const audits = [];
  const manager = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: slots, clock: () => wall,
    retryClock: () => {
      if (clockFault === "throw") throw new Error("injected timing failure");
      return clockFault === "invalid" ? Infinity : monotonic;
    }, retryRandom: () => sample,
    randomBytes: reservationBytes, onPairingInvalidated: onInvalidated,
    audit: (event) => audits.push(event),
  });
  let serial = 0;
  async function registerAndClaim(endpointId = "endpoint-0001") {
    serial += 1;
    let code;
    const registered = await pairings.register({
      request: {
        v: 1, type: "desktop_pairing_register", request_id: `transport-request-${serial}`,
        registration_request_id: `registration-request-${serial}`, endpoint_id: endpointId,
        remote_control_generation: `generation-${serial}`, code_wrap_public_key_b64: publicKey(),
        handshake_public_key_b64: publicKey(), key_algorithm: "x25519", capabilities: ["pairing_v1"],
      },
      relayEpoch: "relay-epoch-0001",
      sealCode: (input) => { code = input.code; return sealPairingCode(input); },
    });
    const reservation = manager.claim({
      pairingCode: code, requestId: `claim-request-${serial}`, socketIdentity: `socket-${serial}`,
      source: `source-${serial}`, handshakePublicKeyB64: publicKey(),
    });
    assert.equal(reservation.pairing_id, registered.result.pairing_id);
    return reservation;
  }
  return { state, slots, pairings, manager, audits, registerAndClaim,
    clockFault: (value) => { clockFault = value; },
    wall: (value) => { wall = value; }, monotonic: (value) => { monotonic = value; } };
}

test("automatic expiry backs off a failed production pairing termination without releasing its slot", async () => {
  const f = await fixture();
  const reservation = await f.registerAndClaim();
  f.wall(11_000);
  const snapshot = f.state.readNamespace();
  f.state.failWrites = true;
  const initial = f.state.attempts;
  await assert.rejects(f.manager.expire(), (error) => error instanceof AggregateError);
  assert.equal(f.state.attempts, initial + 1);
  assert.equal(f.manager.reservationForOwner({ reservationId: reservation.reservation_id,
    socketIdentity: reservation.owner_socket_identity }).state, "invalidating");
  assert.deepEqual(f.state.readNamespace(), snapshot);
  const repeats = await Promise.allSettled([f.manager.expire(), f.manager.expire(), f.manager.expire()]);
  assert.deepEqual(repeats.map((result) => result.status), ["fulfilled", "fulfilled", "fulfilled"]);
  assert.deepEqual(repeats.map((result) => result.value), [0, 0, 0]);
  assert.equal(f.state.attempts, initial + 1);
  assert.equal(f.slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
  assert.deepEqual(f.audits.filter((event) => event === "reservation_invalidation_failed"),
    ["reservation_invalidation_failed"]);
});

test("automatic retry jitter endpoints and repeated failures have fixed finite bounds", async (t) => {
  for (const [sample, delays] of [
    [0, [800, 1600, 3200, 6400, 12800, 25600, 48000, 48000, 48000]],
    [1, [1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 60000]],
  ]) {
    await t.test(`sample ${sample}`, async () => {
      const f = await fixture({ sample });
      const reservation = await f.registerAndClaim();
      f.wall(11_000);
      f.state.failWrites = true;
      const initial = f.state.attempts;
      await assert.rejects(f.manager.expire(), AggregateError);
      let elapsed = 0;
      for (const [index, delay] of delays.entries()) {
        f.monotonic(elapsed + delay - 1);
        assert.equal(await f.manager.expire(), 0);
        assert.equal(f.state.attempts, initial + index + 1);
        elapsed += delay;
        f.monotonic(elapsed);
        await assert.rejects(f.manager.expire(), AggregateError);
        assert.equal(f.state.attempts, initial + index + 2);
        assert.equal(f.slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
      }
    });
  }
});

test("recovery at the due boundary preserves the other owner and terminal evidence", async () => {
  const f = await fixture();
  const expired = await f.registerAndClaim();
  f.wall(9000);
  const other = await f.registerAndClaim("endpoint-0002");
  f.wall(11_000);
  f.state.failWrites = true;
  await assert.rejects(f.manager.expire(), AggregateError);
  assert.throws(() => f.manager.beginHandshake({ reservationId: expired.reservation_id,
    socketIdentity: expired.owner_socket_identity }), (error) => error.code === "reservation_state_invalid");
  const snapshot = f.state.readNamespace();
  f.state.failWrites = false;
  f.monotonic(799);
  assert.equal(await f.manager.expire(), 0);
  assert.deepEqual(f.state.readNamespace(), snapshot);
  f.monotonic(800);
  assert.equal(await f.manager.expire(), 1);
  assert.equal(f.slots.read("endpoint-0001").owner, null);
  assert.equal(f.slots.read("endpoint-0002").owner.owner_id, other.reservation_id);
  const terminal = f.state.value.tombstones.find((row) => row.pairing_id === expired.pairing_id);
  assert.equal(terminal.terminal_at_ms, "11000");
  assert.equal(f.manager.reservationForOwner({ reservationId: expired.reservation_id,
    socketIdentity: expired.owner_socket_identity }), null);
  assert.equal(await f.manager.expire(), 0);
});

test("direct release bypasses an automatic cooldown and preserves the first reason", async () => {
  const events = [];
  const f = await fixture({ onInvalidated: async (event) => events.push(event) });
  const reservation = await f.registerAndClaim();
  f.wall(11_000);
  f.state.failWrites = true;
  await assert.rejects(f.manager.expire(), AggregateError);
  const attempts = f.state.attempts;
  f.state.failWrites = false;
  assert.equal(await f.manager.release({ reservationId: reservation.reservation_id,
    socketIdentity: reservation.owner_socket_identity, reason: "owner_disconnected" }), true);
  assert.equal(f.state.attempts, attempts + 1);
  assert.equal(events[0].reason, "timeout");
  f.monotonic(60_000);
  assert.equal(await f.manager.expire(), 0);
  assert.equal(f.state.attempts, attempts + 1);
});

test("direct release failure does not add an automatic retry delay", async () => {
  const f = await fixture();
  const reservation = await f.registerAndClaim();
  f.state.failWrites = true;
  await assert.rejects(f.manager.release({ reservationId: reservation.reservation_id,
    socketIdentity: reservation.owner_socket_identity }), (error) => error.code === "pairing_persist_failed");
  f.state.failWrites = false;
  assert.equal(await f.manager.expire(), 1);
});

test("concurrent automatic callers share one failure and one backoff step", async () => {
  const f = await fixture();
  await f.registerAndClaim();
  f.wall(11_000);
  f.state.failWrites = true;
  const initial = f.state.attempts;
  const first = await Promise.allSettled([f.manager.expire(), f.manager.expire(), f.manager.expire()]);
  assert.ok(first.every((result) => result.status === "rejected"));
  assert.equal(f.state.attempts, initial + 1);
  f.monotonic(800);
  const second = await Promise.allSettled([f.manager.expire(), f.manager.expire()]);
  assert.ok(second.every((result) => result.status === "rejected"));
  assert.equal(f.state.attempts, initial + 2);
  f.monotonic(2399);
  assert.equal(await f.manager.expire(), 0);
  f.monotonic(2400);
  await assert.rejects(f.manager.expire(), AggregateError);
  assert.equal(f.state.attempts, initial + 3);
});

test("a later owner with a reused reservation ID never inherits the old cooldown", async () => {
  const f = await fixture({ reservationBytes: (size) => Buffer.alloc(size, 1) });
  const old = await f.registerAndClaim();
  f.wall(11_000);
  f.state.failWrites = true;
  for (let index = 0; index < 8; index += 1) {
    f.monotonic(index * 60_000);
    await assert.rejects(f.manager.expire(), AggregateError);
  }
  f.state.failWrites = false;
  await f.manager.release({ reservationId: old.reservation_id, socketIdentity: old.owner_socket_identity });
  f.wall(20_000);
  const fresh = await f.registerAndClaim();
  assert.equal(fresh.reservation_id, old.reservation_id);
  assert.notEqual(fresh.pairing_id, old.pairing_id);
  assert.notEqual(fresh.remote_control_generation, old.remote_control_generation);
  f.wall(30_000);
  f.state.failWrites = true;
  await assert.rejects(f.manager.expire(), AggregateError);
  f.monotonic(420_799);
  assert.equal(await f.manager.expire(), 0);
  f.monotonic(420_800);
  await assert.rejects(f.manager.expire(), AggregateError);
  assert.equal(f.slots.read("endpoint-0001").owner.owner_id, fresh.reservation_id);
});

test("a failed notification retries without rewriting the already persisted terminal", async () => {
  let notifications = 0;
  const f = await fixture({ onInvalidated: async () => {
    notifications += 1;
    if (notifications === 1) throw new Error("injected notification failure");
  } });
  await f.registerAndClaim();
  f.wall(11_000);
  await assert.rejects(f.manager.expire(), AggregateError);
  const snapshot = f.state.readNamespace();
  const attempts = f.state.attempts;
  assert.equal(await f.manager.expire(), 0);
  f.monotonic(800);
  assert.equal(await f.manager.expire(), 1);
  assert.equal(notifications, 2);
  assert.equal(f.state.attempts, attempts);
  assert.deepEqual(f.state.readNamespace(), snapshot);
});

test("invalid retry sources fail closed without a persistence attempt or owner release", async (t) => {
  for (const [label, options, time, code] of [
    ["random", { sample: NaN }, 0, "reservation_retry_random_invalid"],
    ["clock", {}, Infinity, "reservation_retry_clock_invalid"],
  ]) {
    await t.test(label, async () => {
      const f = await fixture(options);
      const reservation = await f.registerAndClaim();
      const snapshot = f.state.readNamespace();
      const attempts = f.state.attempts;
      f.wall(11_000);
      f.monotonic(time);
      await assert.rejects(f.manager.expire(), (error) => error instanceof AggregateError
        && error.errors.length === 1 && error.errors[0].code === code);
      assert.equal(f.state.attempts, attempts);
      assert.deepEqual(f.state.readNamespace(), snapshot);
      assert.equal(f.slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
    });
  }
});

test("a regressing retry clock cannot retry or release an invalidating owner", async () => {
  const f = await fixture();
  const reservation = await f.registerAndClaim();
  f.wall(11_000);
  f.monotonic(100);
  f.state.failWrites = true;
  await assert.rejects(f.manager.expire(), AggregateError);
  const attempts = f.state.attempts;
  f.monotonic(99);
  await assert.rejects(f.manager.expire(), (error) => error.code === "reservation_retry_clock_invalid");
  assert.equal(f.state.attempts, attempts);
  assert.equal(f.slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
  f.monotonic(900);
  f.state.failWrites = false;
  assert.equal(await f.manager.expire(), 1);
});

test("unknown failure-completion time starts a full cooldown only after clock recovery", async (t) => {
  for (const fault of ["throw", "invalid"]) {
    await t.test(fault, async () => {
      const f = await fixture();
      const reservation = await f.registerAndClaim();
      f.wall(11_000);
      f.state.failWrites = true;
      const initial = f.state.attempts;
      f.state.beforeFailure = () => {
        // The persistence operation completes after the whole initial delay,
        // but its completion timestamp cannot be obtained.
        f.monotonic(5000);
        f.clockFault(fault);
      };
      await assert.rejects(f.manager.expire(), (error) => error instanceof AggregateError
        && error.errors[0].code === "pairing_persist_failed");
      assert.equal(f.state.attempts, initial + 1);
      const snapshot = f.state.readNamespace();
      await assert.rejects(f.manager.expire());
      await assert.rejects(f.manager.expire());
      assert.equal(f.state.attempts, initial + 1);
      f.clockFault(null);
      f.state.beforeFailure = () => {};
      assert.equal(await f.manager.expire(), 0);
      assert.equal(f.state.attempts, initial + 1);
      f.monotonic(5500);
      assert.equal(await f.manager.expire(), 0);
      f.monotonic(5799);
      assert.equal(await f.manager.expire(), 0);
      assert.deepEqual(f.state.readNamespace(), snapshot);
      assert.equal(f.slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
      f.clockFault(fault);
      await assert.rejects(f.manager.expire());
      f.clockFault(null);
      f.monotonic(5800);
      await assert.rejects(f.manager.expire(), AggregateError);
      assert.equal(f.state.attempts, initial + 2);
      // The failed attempt count is not reset by a missing timestamp or by
      // invalid clock reads: the next delay is the second step, 1600 ms.
      f.monotonic(7399);
      assert.equal(await f.manager.expire(), 0);
      f.monotonic(7400);
      f.state.failWrites = false;
      assert.equal(await f.manager.expire(), 1);
    });
  }
});

test("direct release bypasses a pending cooldown while completion clock is still invalid", async () => {
  const f = await fixture();
  const reservation = await f.registerAndClaim();
  f.wall(11_000);
  f.state.failWrites = true;
  f.state.beforeFailure = () => {
    f.monotonic(5000);
    f.clockFault("throw");
  };
  await assert.rejects(f.manager.expire(), (error) => error instanceof AggregateError
    && error.errors[0].code === "pairing_persist_failed");
  f.state.failWrites = false;
  assert.equal(await f.manager.release({ reservationId: reservation.reservation_id,
    socketIdentity: reservation.owner_socket_identity }), true);
  assert.equal(f.slots.read("endpoint-0001").owner, null);
  f.clockFault(null);
  assert.equal(await f.manager.expire(), 0);
});
