#!/usr/bin/env node
import { createHash, createPublicKey, verify } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { runAuthorizationCoverage } from './check-account-platform-authorization-coverage.mjs';

const requiredChecks = new Set([
  'GATEWAY_CONTRACT:PASS', 'ACCOUNT_CONTRACT:PASS', 'OIDC_METADATA:PASS',
  'MODEL_CATALOG:PASS', 'MANAGED_ROUTE:PASS', 'PRICING_FORMULA:PASS',
]);

await runAuthorizationCoverage();

function required(name) {
  const value = String(process.env[name] || '').trim();
  if (!value) throw new Error(`release_gate_configuration_missing:${name}`);
  return value;
}

function base64url(value) {
  return Buffer.from(value, 'base64url');
}

function semver(value) {
  const match = String(value).match(/^(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/);
  if (!match) throw new Error(`release_gate_version_invalid:${value}`);
  return match.slice(1).map(Number);
}

function compare(left, right) {
  const a = semver(left); const b = semver(right);
  for (let index = 0; index < 3; index += 1) {
    if (a[index] !== b[index]) return a[index] - b[index];
  }
  return 0;
}

function ed25519PublicKey(raw) {
  if (raw.length !== 32) throw new Error('release_gate_public_key_invalid');
  const prefix = Buffer.from('302a300506032b6570032100', 'hex');
  return createPublicKey({ key: Buffer.concat([prefix, raw]), format: 'der', type: 'spki' });
}

async function jsonFetch(url, label) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  try {
    const response = await fetch(url, { signal: controller.signal, redirect: 'error' });
    if (!response.ok) throw new Error(`${label}_http_${response.status}`);
    return await response.json();
  } finally {
    clearTimeout(timeout);
  }
}

const artifactPath = required('PINVOU_RELEASE_ARTIFACT');
const attestationPath = required('PINVOU_RELEASE_ATTESTATION');
const compatibilityURL = new URL(required('PINVOU_CLIENT_COMPATIBILITY_URL'));
const issuer = new URL(required('PINVOU_ACCOUNT_OIDC_ISSUER'));
const accountURL = new URL(required('PINVOU_ACCOUNT_SERVICE_URL'));
const gatewayURL = new URL(required('PINVOU_TOKEN_GATEWAY_URL'));
const portalURL = new URL(required('PINVOU_ACCOUNT_PORTAL_URL'));
const environment = required('PINVOU_ACCOUNT_ENVIRONMENT');
const keyID = required('PINVOU_CLIENT_READINESS_KEY_ID');
const publicKey = base64url(required('PINVOU_CLIENT_READINESS_PUBLIC_KEY'));
const clientVersion = required('PINVOU_RELEASE_CLIENT_VERSION');

for (const url of [compatibilityURL, issuer, accountURL, gatewayURL, portalURL]) {
  if (url.protocol !== 'https:' && !['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)) {
    throw new Error(`release_gate_insecure_endpoint:${url.origin}`);
  }
  if (url.username || url.password || url.hash) throw new Error(`release_gate_endpoint_invalid:${url.origin}`);
}

const artifact = await readFile(artifactPath);
const digest = createHash('sha256').update(artifact).digest('hex');
const attestation = JSON.parse(await readFile(attestationPath, 'utf8'));
const compatibility = await jsonFetch(compatibilityURL, 'compatibility');
const discoveryURL = new URL(`${issuer.href.replace(/\/$/, '')}/.well-known/openid-configuration`);
const discovery = await jsonFetch(discoveryURL, 'oidc_discovery');
const claims = attestation.claims || {};
const payload = Buffer.from(JSON.stringify(claims));
const signature = base64url(attestation.signature_base64url || '');
const now = Date.now();

if (attestation.algorithm !== 'Ed25519' || attestation.key_id !== keyID
  || !verify(null, payload, ed25519PublicKey(publicKey), signature)) {
  throw new Error('release_gate_attestation_signature_invalid');
}
if (claims.version !== 1 || claims.environment !== environment || claims.artifact_sha256 !== digest
  || claims.client_version !== clientVersion || claims.protocol_code !== 'CHAT'
  || Number(claims.catalog_version) <= 0 || Date.parse(claims.expires_at) <= now
  || Date.parse(claims.issued_at) > now + 60_000
  || Date.parse(claims.expires_at) - Date.parse(claims.issued_at) > 15 * 60_000) {
  throw new Error('release_gate_attestation_binding_invalid');
}
for (const check of requiredChecks) {
  if (!(claims.check_codes || []).includes(check)) throw new Error(`release_gate_check_missing:${check}`);
}
if (!compatibility.attestation_available || compatibility.environment !== environment
  || compatibility.attestation_key_id !== keyID
  || compatibility.contract_version !== claims.contract_version
  || compatibility.service_version !== claims.service_version
  || compare(clientVersion, compatibility.minimum_client_version) < 0
  || compare(clientVersion, compatibility.maximum_client_version) > 0) {
  throw new Error('release_gate_compatibility_mismatch');
}
if (String(discovery.issuer || '').replace(/\/$/, '') !== issuer.href.replace(/\/$/, '') || !discovery.jwks_uri) {
  throw new Error('release_gate_oidc_metadata_invalid');
}

process.stdout.write(`${JSON.stringify({
  decision: 'GO', environment, client_version: clientVersion, artifact_sha256: digest,
  attestation_id: claims.attestation_id, catalog_version: claims.catalog_version,
  service_version: claims.service_version, contract_version: claims.contract_version,
})}\n`);
