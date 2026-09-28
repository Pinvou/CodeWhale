#!/usr/bin/env node
import crypto, { X509Certificate } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

function fail(code) { const error = new Error(code); error.code = code; throw error; }
function exact(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).sort().join("\0") !== [...keys].sort().join("\0")) fail(code);
  return value;
}
function digest(value) { return crypto.createHash("sha256").update(value).digest("hex"); }
function arg(name) { const index = process.argv.indexOf(name); return index < 0 ? undefined : process.argv[index + 1]; }

export function providerAttestationPayload(value) {
  return Buffer.from(JSON.stringify({
    schema: value.schema, version: value.version, role: value.role, role_policy_sha256: value.role_policy_sha256, provider: value.provider, key_id: value.key_id,
    run_nonce: value.run_nonce, public_key_spki_pem: value.public_key_spki_pem,
    public_key_sha256: value.public_key_sha256, non_exportable: value.non_exportable,
    acl_attestation_sha256: value.acl_attestation_sha256,
    provider_measurement_sha256: value.provider_measurement_sha256, driver_sha256: value.driver_sha256,
    issued_at: value.issued_at,
  }), "utf8");
}

export function verifyProviderAttestation(value, expected, now = Date.now()) {
  exact(value, ["schema", "version", "role", "role_policy_sha256", "provider", "key_id", "run_nonce", "public_key_spki_pem", "public_key_sha256",
    "non_exportable", "acl_attestation_sha256", "provider_measurement_sha256", "driver_sha256", "issued_at", "verifier"], "provider_attestation_schema_invalid");
  exact(value.verifier, ["kind", "vendor", "leaf_certificate_pem", "certificate_chain_pem", "signature_algorithm", "signature"], "provider_attestation_schema_invalid");
  if (value.schema !== "pinvou-provider-attestation-receipt" || value.version !== 1
    || value.role !== expected.role || value.role_policy_sha256 !== expected.rolePolicySha256
    || value.provider !== expected.provider || value.key_id !== expected.keyId || value.run_nonce !== expected.runNonce
    || value.acl_attestation_sha256 !== expected.aclSha256 || value.provider_measurement_sha256 !== expected.measurementSha256
    || value.driver_sha256 !== expected.driverSha256 || value.non_exportable !== true
    || value.verifier.kind !== "independent-provider-verifier" || value.verifier.vendor !== expected.vendor
    || value.verifier.signature_algorithm !== "sha256" || !Array.isArray(value.verifier.certificate_chain_pem)
    || value.verifier.certificate_chain_pem.length < 1 || !/^[A-Za-z0-9+/]{80,}={0,2}$/u.test(value.verifier.signature)) fail("provider_attestation_invalid");
  for (const field of [value.role_policy_sha256, value.run_nonce, value.public_key_sha256, value.acl_attestation_sha256, value.provider_measurement_sha256, value.driver_sha256]) {
    if (!/^[a-f0-9]{64}$/u.test(field)) fail("provider_attestation_invalid");
  }
  let providerKey; let leaf; let chain;
  try {
    providerKey = crypto.createPublicKey(value.public_key_spki_pem);
    leaf = new X509Certificate(value.verifier.leaf_certificate_pem);
    chain = value.verifier.certificate_chain_pem.map((pem) => new X509Certificate(pem));
  } catch { fail("provider_attestation_certificate_invalid"); }
  if (digest(providerKey.export({ type: "spki", format: "der" })) !== value.public_key_sha256) fail("provider_attestation_key_invalid");
  const certificates = [leaf, ...chain];
  for (let index = 0; index < certificates.length - 1; index += 1) {
    if (!certificates[index].checkIssued(certificates[index + 1])
      || !certificates[index].verify(certificates[index + 1].publicKey)
      || (index + 1 > 0 && certificates[index + 1].ca !== true)) fail("provider_attestation_chain_invalid");
  }
  const root = certificates.at(-1);
  if (root.ca !== true || !root.checkIssued(root) || !root.verify(root.publicKey)) fail("provider_attestation_chain_invalid");
  if (digest(root.raw) !== expected.rootCertificateSha256 || digest(leaf.raw) !== expected.verifierCertificateSha256) fail("provider_attestation_certificate_untrusted");
  const issuedAt = Date.parse(value.issued_at);
  if (!Number.isFinite(issuedAt) || issuedAt > now + 30_000 || now - issuedAt > 300_000
    || certificates.some((certificate) => Date.parse(certificate.validFrom) > issuedAt || Date.parse(certificate.validTo) < issuedAt)) fail("provider_attestation_stale");
  const signature = Buffer.from(value.verifier.signature, "base64");
  if (!crypto.verify("sha256", providerAttestationPayload(value), leaf.publicKey, signature)) fail("provider_attestation_signature_invalid");
  return { publicKeySha256: value.public_key_sha256, issuedAt: value.issued_at };
}

export async function verifyProviderAttestationFile(input, output, expected) {
  let value;
  try { value = JSON.parse(await readFile(resolve(input), "utf8")); } catch { fail("provider_attestation_unreadable"); }
  verifyProviderAttestation(value, expected);
  if (output) await writeFile(resolve(output), `${JSON.stringify(value)}\n`, { encoding: "utf8", flag: "wx" });
  return value;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  verifyProviderAttestationFile(arg("--input"), arg("--output"), {
    role: arg("--role"), rolePolicySha256: arg("--role-policy-sha256"), provider: arg("--provider"), keyId: arg("--key-id"), runNonce: arg("--run-nonce"),
    aclSha256: arg("--acl-sha256"), measurementSha256: arg("--measurement-sha256"), driverSha256: arg("--driver-sha256"),
    vendor: arg("--vendor"), rootCertificateSha256: arg("--root-certificate-sha256"),
    verifierCertificateSha256: arg("--verifier-certificate-sha256"),
  }).then((value) => process.stdout.write(`${JSON.stringify({
    ok: true, role: value.role, provider: value.provider, key_id: value.key_id,
    public_key_spki_pem: value.public_key_spki_pem, public_key_sha256: value.public_key_sha256,
  })}\n`)).catch((error) => {
    process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "provider_attestation_verification_failed" })}\n`);
    process.exitCode = 1;
  });
}
