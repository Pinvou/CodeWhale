import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { sealPairingCode } from "../pairing-code-sealer.js";
import { buildSealedCodeAad, parseSealedCodeV1 } from "../protocol-controller.js";

const X25519_SPKI_PREFIX = Buffer.from("302a300506032b656e032100", "hex");
const X25519_PKCS8_PREFIX = Buffer.from("302e020100300506032b656e04220420", "hex");
const BINDING = Object.freeze({
  registration_request_id: "registration-request-0001",
  pairing_id: "pairing-0001",
  remote_control_generation: "generation-0001",
  relay_epoch: "epoch-test-1",
  expires_at_ms: "30000",
});

function lp(value) {
  const bytes = Buffer.from(value, "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

function rawPublic(key) {
  return key.export({ type: "spki", format: "der" }).subarray(-32);
}

function openCode(privateKey, binding, sealed) {
  const parsed = parseSealedCodeV1(sealed);
  const relayPublic = crypto.createPublicKey({
    key: Buffer.concat([X25519_SPKI_PREFIX, parsed.relay_ephemeral_public_key]),
    format: "der",
    type: "spki",
  });
  const shared = crypto.diffieHellman({ privateKey, publicKey: relayPublic });
  const info = Buffer.concat([
    Buffer.from("pinvou-code-wrap-key-v1", "ascii"),
    ...[
      binding.registration_request_id, binding.pairing_id, binding.remote_control_generation,
      binding.relay_epoch, binding.expires_at_ms,
    ].map(lp),
  ]);
  const key = Buffer.from(crypto.hkdfSync(
    "sha256", shared, Buffer.from("pinvou-code-wrap-salt-v1", "ascii"), info, 32,
  ));
  try {
    const encrypted = parsed.ciphertext_and_tag;
    const decipher = crypto.createDecipheriv("aes-256-gcm", key, parsed.nonce);
    decipher.setAAD(buildSealedCodeAad(binding), { plaintextLength: 7 });
    decipher.setAuthTag(encrypted.subarray(-16));
    return Buffer.concat([decipher.update(encrypted.subarray(0, -16)), decipher.final()]).toString("ascii");
  } finally {
    shared.fill(0);
    key.fill(0);
  }
}

test("one-shot Relay sealer round-trips a seven-character code and changes every envelope", () => {
  const receiver = crypto.generateKeyPairSync("x25519");
  const input = {
    code: "2345678",
    codeWrapPublicKeyB64: rawPublic(receiver.publicKey).toString("base64"),
    binding: BINDING,
  };
  const first = sealPairingCode(input);
  const second = sealPairingCode(input);
  assert.notEqual(first, second);
  assert.equal(Buffer.from(first, "base64").length, 68);
  assert.equal(openCode(receiver.privateKey, BINDING, first), input.code);
  assert.equal(openCode(receiver.privateKey, BINDING, second), input.code);
});

test("sealed code rejects stale bindings, invalid codes and non-contributory peer keys", () => {
  const receiver = crypto.generateKeyPairSync("x25519");
  const publicKey = rawPublic(receiver.publicKey).toString("base64");
  const sealed = sealPairingCode({ code: "ABCDEFG", codeWrapPublicKeyB64: publicKey, binding: BINDING });
  assert.throws(() => openCode(receiver.privateKey, { ...BINDING, pairing_id: "pairing-other" }, sealed));
  assert.throws(() => sealPairingCode({ code: "1ABCDEF", codeWrapPublicKeyB64: publicKey, binding: BINDING }),
    (error) => error?.code === "pairing_code_invalid");

  const zeroPublic = Buffer.alloc(32).toString("base64");
  assert.throws(() => sealPairingCode({ code: "ABCDEFG", codeWrapPublicKeyB64: zeroPublic, binding: BINDING }),
    (error) => ["code_wrap_public_key_non_contributory", "pairing_code_seal_failed"].includes(error?.code));
});

test("production sealer owns ephemeral keys and nonce with no caller-controlled secret hooks", async () => {
  const source = await import("node:fs/promises").then(({ readFile }) => readFile(
    new URL("../pairing-code-sealer.js", import.meta.url), "utf8",
  ));
  assert.doesNotMatch(source, /nonce\s*[:=].*input|privateKey\s*[:=].*input|randomBytes\s*[:=].*input/u);
  assert.match(source, /generateKeyPairSync\("x25519"\)/u);
  assert.match(source, /crypto\.randomBytes\(12\)/u);

  const privateKey = crypto.createPrivateKey({
    key: Buffer.concat([X25519_PKCS8_PREFIX, Buffer.alloc(32, 7)]), format: "der", type: "pkcs8",
  });
  assert.equal(privateKey.asymmetricKeyType, "x25519");
});
