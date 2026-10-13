import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { ControllerLivenessHooks } from "../controller-liveness.js";
import { ControllerLeaseManager } from "../controller-lease.js";
import { isRelaySigningKeyIdentifier, isControllerIdentifier, isLegacyWebEndpointIdentifier } from "../identifier-contract.js";

test("signing configuration rejects IDs that cannot be installed in native pinned trust", () => {
  const { privateKey } = crypto.generateKeyPairSync("ed25519");
  let signCalls = 0;
  const sign = () => { signCalls += 1; return Buffer.alloc(64); };
  const noop = () => {};
  const leaseOptions = {
    pairingStore: { activePairing: noop, consumeForControllerLease: noop },
    reservationManager: { claim: noop, beginHandshake: noop, promoteToLease: noop,
      release: noop, reservationForOwner: noop, releaseControllerLease: noop },
    relayEpoch: "relay:epoch", signTranscript: sign, resolveControlledEndpoint: noop,
  };
  for (const signingKeyId of ["relay:key", "a".repeat(65), "a".repeat(128), "relay~key",
    "_key", "-key", ".key", "key\n", "key\r", "key ", "密钥", "", null, 1]) {
    assert.equal(isRelaySigningKeyIdentifier(signingKeyId), false);
    for (const options of [{ signingPrivateKey: privateKey }, { sign }]) {
      assert.throws(() => new ControllerLivenessHooks({ signingKeyId, ...options }), error => {
        assert.equal(error.code, "controller_liveness_signing_key_invalid");
        assert.equal(error.message, error.code);
        assert.equal(error.cause, undefined);
        return true;
      });
    }
    assert.throws(() => new ControllerLeaseManager({ ...leaseOptions, signingKeyId }), error => {
      assert.equal(error.code, "controller_lease_config_invalid");
      assert.equal(error.message, error.code);
      assert.equal(error.cause, undefined);
      return true;
    });
  }
  assert.equal(signCalls, 0);
});

test("supported signing IDs preserve exact configuration bytes and other identity domains", () => {
  const { privateKey } = crypto.generateKeyPairSync("ed25519");
  for (const signingKeyId of ["a", "0", "relay-key-1", "A._-9", "a".repeat(64)]) {
    assert.equal(isRelaySigningKeyIdentifier(signingKeyId), true);
    assert.doesNotThrow(() => new ControllerLivenessHooks({ signingKeyId, signingPrivateKey: privateKey }));
  }
  assert.equal(isControllerIdentifier("relay:key"), true);
  assert.equal(isControllerIdentifier("a".repeat(128)), true);
  for (const endpoint of ["_old", "-old"]) assert.equal(isLegacyWebEndpointIdentifier(endpoint), true);
});

test("server validates dedicated signing ID before private key decoding without changing native trust", async () => {
  const source = await readFile(new URL("../server.js", import.meta.url), "utf8");
  const gate = source.indexOf("if (!isRelaySigningKeyIdentifier(signingKeyId))");
  assert.ok(gate > 0);
  assert.ok(gate < source.indexOf('const signingKeyB64 ='));
  assert.match(source.slice(gate, source.indexOf('const signingKeyB64 =')), /throw new Error\("relay_signing_key_id_invalid"\)/u);
  // This is source wiring evidence; real server startup is a separate test.
});
