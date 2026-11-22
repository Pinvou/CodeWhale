import { createPublicKey, verify } from 'node:crypto';

const requiredChecks = [
  'GATEWAY_CONTRACT:PASS', 'ACCOUNT_CONTRACT:PASS', 'OIDC_METADATA:PASS',
  'MODEL_CATALOG:PASS', 'MANAGED_ROUTE:PASS', 'PRICING_FORMULA:PASS',
];

/** Validate the existing signed readiness binding before permitting release. */
export function validateReadinessBindings(claims, { environment, digest, clientVersion, now }) {
  const issuedAt = Date.parse(claims.issued_at);
  const expiresAt = Date.parse(claims.expires_at);
  const catalogVersion = claims.catalog_version;
  if (claims.version !== 1 || claims.environment !== environment || claims.artifact_sha256 !== digest
    || claims.client_version !== clientVersion || claims.protocol_code !== 'CHAT'
    || typeof catalogVersion !== 'number' || !Number.isSafeInteger(catalogVersion) || catalogVersion <= 0
    || typeof claims.issued_at !== 'string' || typeof claims.expires_at !== 'string'
    || !Number.isFinite(issuedAt) || !Number.isFinite(expiresAt)
    || expiresAt <= now || issuedAt > now + 60_000 || expiresAt <= issuedAt
    || expiresAt - issuedAt > 15 * 60_000) {
    throw new Error('release_gate_attestation_binding_invalid');
  }
}

/** Verify the real release attestation; human session authority remains server-side. */
export function verifyReadinessAttestation(attestation, binding, keyID, rawPublicKey) {
  const claims = attestation?.claims;
  if (!claims || typeof claims !== 'object' || Array.isArray(claims)) {
    throw new Error('release_gate_attestation_binding_invalid');
  }
  validateReadinessBindings(claims, binding);
  if (rawPublicKey.length !== 32) throw new Error('release_gate_public_key_invalid');
  const prefix = Buffer.from('302a300506032b6570032100', 'hex');
  const publicKey = createPublicKey({ key: Buffer.concat([prefix, rawPublicKey]), format: 'der', type: 'spki' });
  const signature = Buffer.from(attestation.signature_base64url || '', 'base64url');
  if (attestation.algorithm !== 'Ed25519' || attestation.key_id !== keyID
    || !verify(null, Buffer.from(JSON.stringify(claims)), publicKey, signature)) {
    throw new Error('release_gate_attestation_signature_invalid');
  }
  if (!Array.isArray(claims.check_codes)) throw new Error('release_gate_checks_invalid');
  for (const check of requiredChecks) {
    if (!claims.check_codes.includes(check)) throw new Error(`release_gate_check_missing:${check}`);
  }
}
