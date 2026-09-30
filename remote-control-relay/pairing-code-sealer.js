import crypto from "node:crypto";
import { buildSealedCodeAad, parseSealedCodeV1 } from "./protocol-controller.js";

const CODE = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u;
const X25519_SPKI_PREFIX = Buffer.from("302a300506032b656e032100", "hex");
const HKDF_SALT = Buffer.from("pinvou-code-wrap-salt-v1", "ascii");

export class PairingCodeSealerError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "PairingCodeSealerError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new PairingCodeSealerError(code, cause ? { cause } : undefined);
}

function lp(value) {
  const bytes = Buffer.from(value, "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

function peerPublicKey(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9+/]+={0,2}$/u.test(value)) fail("code_wrap_public_key_invalid");
  const raw = Buffer.from(value, "base64");
  if (raw.length !== 32 || raw.toString("base64") !== value) fail("code_wrap_public_key_invalid");
  try {
    const key = crypto.createPublicKey({
      key: Buffer.concat([X25519_SPKI_PREFIX, raw]),
      format: "der",
      type: "spki",
    });
    if (key.asymmetricKeyType !== "x25519") fail("code_wrap_public_key_invalid");
    return key;
  } catch (error) {
    if (error instanceof PairingCodeSealerError) throw error;
    fail("code_wrap_public_key_invalid", error);
  }
}

function deriveInfo(binding) {
  return Buffer.concat([
    Buffer.from("pinvou-code-wrap-key-v1", "ascii"),
    lp(binding.registration_request_id),
    lp(binding.pairing_id),
    lp(binding.remote_control_generation),
    lp(binding.relay_epoch),
    lp(binding.expires_at_ms),
  ]);
}

export function sealPairingCode({ code, codeWrapPublicKeyB64, binding } = {}) {
  if (typeof code !== "string" || !CODE.test(code)) fail("pairing_code_invalid");
  const aad = buildSealedCodeAad(binding);
  const peer = peerPublicKey(codeWrapPublicKeyB64);
  const { publicKey, privateKey } = crypto.generateKeyPairSync("x25519");
  let shared;
  let key;
  try {
    shared = crypto.diffieHellman({ privateKey, publicKey: peer });
    if (shared.every((byte) => byte === 0)) fail("code_wrap_public_key_non_contributory");
    key = Buffer.from(crypto.hkdfSync("sha256", shared, HKDF_SALT, deriveInfo(binding), 32));
    const nonce = crypto.randomBytes(12);
    const cipher = crypto.createCipheriv("aes-256-gcm", key, nonce);
    cipher.setAAD(aad, { plaintextLength: 7 });
    const ciphertext = Buffer.concat([cipher.update(code, "ascii"), cipher.final()]);
    const tag = cipher.getAuthTag();
    const publicDer = publicKey.export({ type: "spki", format: "der" });
    const envelope = Buffer.concat([Buffer.from([1]), publicDer.subarray(-32), nonce, ciphertext, tag]);
    const sealed = envelope.toString("base64");
    parseSealedCodeV1(sealed);
    return sealed;
  } catch (error) {
    if (error instanceof PairingCodeSealerError) throw error;
    fail("pairing_code_seal_failed", error);
  } finally {
    shared?.fill(0);
    key?.fill(0);
  }
}
