"use strict";

// Local selftest identity preflight. Secret input is stdin only; report solely
// safe status, never key values, parser messages or crypto exception stacks.
const fs = require("node:fs");
const crypto = require("node:crypto");

try {
  const input = fs.readFileSync(0, "utf8");
  if (Buffer.byteLength(input) > 16 * 1024) throw new Error();
  const key = JSON.parse(input.replace(/^\uFEFF/, ""));
  if (!key || Array.isArray(key) || key.signing_key_id !== "selftest-relay-key-1") throw new Error();
  const decode = (value) => {
    if (typeof value !== "string" || value.length === 0) throw new Error();
    const bytes = Buffer.from(value, "base64");
    if (bytes.toString("base64") !== value) throw new Error();
    return bytes;
  };
  const privateBytes = decode(key.private_key_pkcs8_der_b64);
  const publicBytes = decode(key.public_key_raw_b64);
  if (privateBytes.length > 512 || publicBytes.length !== 32) throw new Error();
  const privateKey = crypto.createPrivateKey({ key: privateBytes, type: "pkcs8", format: "der" });
  if (privateKey.asymmetricKeyType !== "ed25519") throw new Error();
  const spki = crypto.createPublicKey(privateKey).export({ type: "spki", format: "der" });
  if (!crypto.timingSafeEqual(spki.subarray(spki.length - 32), publicBytes)) throw new Error();
  process.stdout.write('{"ok":true}\n');
} catch {
  process.stderr.write("selftest_signing_identity_invalid\n");
  process.exitCode = 1;
}
