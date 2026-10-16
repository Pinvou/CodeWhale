import assert from "node:assert/strict";
import test from "node:test";
import { PairingStore } from "../pairing-store.js";

test("reservation retain and release reject the same invalid identifier domain", async () => {
  let value = null;
  let revision = "0";
  let writes = 0;
  const store = await PairingStore.open({
    stateStore: {
      readNamespace: () => ({ namespace_revision: revision, value: structuredClone(value) }),
      compareAndSwap: async (_name, expected, next) => {
        assert.equal(expected, revision);
        value = structuredClone(next);
        revision = String(BigInt(revision) + 1n);
        writes++;
        return { namespace_revision: revision, value: structuredClone(value) };
      },
    },
    digestKeyring: { activeKeyId: "release-test-key", keys: [
      { keyId: "release-test-key", key: "release-test-key-material-at-least-32-bytes" },
    ] },
    clock: () => 1000,
  });
  const beforeWrites = writes;
  for (const value of [undefined, null, 1, {}, [], "", "_leading", "pairing~bad", "x".repeat(129)]) {
    assert.throws(() => store.retainForReservation(value), { code: "pairing_id_invalid" });
    assert.throws(() => store.releaseReservation(value), { code: "pairing_id_invalid" });
  }
  for (const value of ["pairing:valid-1", "x".repeat(128)]) {
    store.retainForReservation(value);
    assert.doesNotThrow(() => store.releaseReservation(value));
    assert.doesNotThrow(() => store.releaseReservation(value));
  }
  assert.doesNotThrow(() => store.releaseReservation("pairing:unknown"));
  assert.equal(writes, beforeWrites, "retention ownership changes do not alter durable evidence");
});
