#!/usr/bin/env node

import { createHash } from 'node:crypto';
import tls from 'node:tls';
import process from 'node:process';
import net from 'node:net';
import { isMainModule } from './account-platform-cli-entry.mjs';
import { nativeKmsConfiguration, prepareNativeRotation, verifyNativeRotation, parseGateJSON } from './account-platform-native-kms-gate.mjs';

function required(environment, name) {
  const value = String(environment[name] || '').trim();
  if (!value) throw new Error(`production_security_configuration_missing:${name}`);
  return value;
}

function secureURL(raw, label) {
  let value;
  try { value = new URL(raw); } catch { throw new Error(`production_security_endpoint_invalid:${label}`); }
  if (value.protocol !== 'https:' || value.username || value.password || value.hash) {
    throw new Error(`production_security_endpoint_invalid:${label}`);
  }
  return value;
}

function privateReadinessURL(raw, label) {
  let value;
  try { value = new URL(raw); } catch { throw new Error(`production_security_endpoint_invalid:${label}`); }
  const loopbackHTTP = value.protocol === 'http:'
    && ['127.0.0.1', 'localhost', '[::1]'].includes(value.hostname);
  if ((value.protocol !== 'https:' && !loopbackHTTP) || value.username || value.password || value.hash) {
    throw new Error(`production_security_endpoint_invalid:${label}`);
  }
  return value;
}

export function productionSecurityConfiguration(environment = process.env) {
  if (environment.NODE_TLS_REJECT_UNAUTHORIZED === '0') throw new Error('production_security_tls_verification_required');
  let publicEndpoints;
  try {
    publicEndpoints = parseGateJSON(required(environment, 'PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON'));
  } catch {
    throw new Error('production_security_configuration_invalid:PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON');
  }
  if (!Array.isArray(publicEndpoints) || publicEndpoints.length < 3 || publicEndpoints.length > 64) {
    throw new Error('production_security_public_endpoints_incomplete');
  }
  const endpoints = publicEndpoints.map((item, index) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)
      || JSON.stringify(Object.keys(item).sort()) !== JSON.stringify(['expected_status','name','url']) || !/^[a-z][a-z0-9-]{0,63}$/.test(String(item.name || ''))) {
      throw new Error(`production_security_public_endpoint_invalid:${index}`);
    }
    const expectedStatus = Number(item.expected_status);
    if (!Number.isInteger(expectedStatus) || expectedStatus < 200 || expectedStatus > 499) {
      throw new Error(`production_security_public_endpoint_status_invalid:${item.name}`);
    }
    return { name: String(item.name).trim(), url: secureURL(item.url, item.name), expectedStatus };
  });
  const names = new Set(endpoints.map(item => item.name));
  if (names.size !== endpoints.length || !['portal', 'identity', 'oidc'].every(name => names.has(name))) {
    throw new Error('production_security_public_endpoints_incomplete');
  }
  const activeKeyID = required(environment, 'TOKEN_SECRET_KEY_ID');
  const provider = String(environment.TOKEN_SECRET_PROVIDER || 'remote');
  if (provider === 'vault-transit') {
    if (!names.has('credentials')) throw new Error('production_security_public_endpoints_incomplete');
    return { provider, endpoints, activeKeyID,
      tokenReadyURL: privateReadinessURL(required(environment, 'PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL'), 'token-ready'),
      native: nativeKmsConfiguration(environment) };
  }
  if (provider !== 'remote' || (environment.PINVOU_SECURITY_INSTALL_MODE && environment.PINVOU_SECURITY_INSTALL_MODE !== 'migration')) {
    throw new Error('production_security_provider_or_install_mode_unsupported');
  }
  const previousKeyID = required(environment, 'PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID');
  if (activeKeyID === previousKeyID) throw new Error('production_security_rotation_key_unchanged');
  return {
    endpoints,
    tokenReadyURL: privateReadinessURL(required(environment, 'PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL'), 'token-ready'),
    kmsHealthURL: secureURL(required(environment, 'PINVOU_SECURITY_KMS_HEALTH_URL'), 'kms-health'),
    kmsToken: required(environment, 'TOKEN_SECRET_PROVIDER_TOKEN'),
    activeKeyID,
    previousKeyID,
  };
}

async function fetchBounded(fetchImpl, url, options = {}, jsonBody = false) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  try {
    const response = await fetchImpl(url, { ...options, signal: controller.signal, redirect: 'error' });
    if (!jsonBody) {
      await response.body?.cancel();
      return { status: response.status, ok: response.ok };
    }
    const reader = response.body?.getReader();
    if (!reader) throw new Error('production_security_response_invalid');
    const chunks = []; let size = 0;
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.byteLength;
        if (size > 65536) throw new Error('production_security_response_too_large');
        chunks.push(value);
      }
      let body;
      try { body = parseGateJSON(Buffer.concat(chunks)); }
      catch { throw new Error('production_security_response_invalid'); }
      return { status: response.status, ok: response.ok, body };
    } finally { await reader.cancel(); }
  } finally {
    clearTimeout(timeout);
  }
}

async function inspectTLS(url) {
  return await new Promise((resolve, reject) => {
    const socket = tls.connect({ host:url.hostname, port:Number(url.port || 443), servername:net.isIP(url.hostname) ? undefined : url.hostname, rejectUnauthorized:true });
    socket.setTimeout(10_000);
    socket.once('secureConnect', () => {
      const certificate = socket.getPeerCertificate();
      socket.end();
      const expiresAt = Date.parse(certificate.valid_to || '');
      if (!certificate || !Number.isFinite(expiresAt)) reject(new Error('production_security_certificate_invalid'));
      else resolve({ expiresAt });
    });
    socket.once('timeout', () => socket.destroy(new Error('production_security_certificate_timeout')));
    socket.once('error', reject);
  });
}

export async function runProductionSecurityGate(configuration, fetchImpl = fetch, tlsInspector = inspectTLS, nativeVerifier = verifyNativeRotation) {
  const endpointEvidence = [];
  for (const endpoint of configuration.endpoints) {
    const certificate = await tlsInspector(endpoint.url);
    if (!Number.isFinite(certificate?.expiresAt) || certificate.expiresAt - Date.now() < 14 * 24 * 60 * 60 * 1000) {
      throw new Error(`production_security_certificate_expiring:${endpoint.name}`);
    }
    const response = await fetchBounded(fetchImpl, endpoint.url, { method: 'GET', headers: { Accept: 'application/json,text/html' } });
    if (response.status !== endpoint.expectedStatus) {
      throw new Error(`production_security_endpoint_status_mismatch:${endpoint.name}:${response.status}`);
    }
    endpointEvidence.push({ name: endpoint.name, origin: endpoint.url.origin, status: response.status, certificate_expires_at:new Date(certificate.expiresAt).toISOString() });
  }

  const readyResponse = await fetchBounded(fetchImpl, configuration.tokenReadyURL, { headers: { Accept: 'application/json' } }, true);
  const ready = readyResponse.ok ? readyResponse.body : {};
  if (!readyResponse.ok || !ready || typeof ready !== 'object' || Array.isArray(ready) || ready.status !== 'ready') throw new Error('production_security_token_rotation_not_ready');

  if (configuration.provider === 'vault-transit') {
    const native = await nativeVerifier(configuration.native);
    const evidence = { decision: 'GO', checked_at: new Date().toISOString(), public_endpoints: endpointEvidence,
      token_rotation_status: ready.status, native };
    return { ...evidence, evidence_sha256: createHash('sha256').update(JSON.stringify(evidence)).digest('hex') };
  }

  const kmsResponse = await fetchBounded(fetchImpl, configuration.kmsHealthURL, {
    headers: { Accept: 'application/json', Authorization: `Bearer ${configuration.kmsToken}` },
  }, true);
  const kms = kmsResponse.ok ? kmsResponse.body : {};
  if (!kmsResponse.ok || !kms || typeof kms !== 'object' || Array.isArray(kms)
    || !kms.key_states || typeof kms.key_states !== 'object' || Array.isArray(kms.key_states)
    || kms.status !== 'ready' || kms.active_key_id !== configuration.activeKeyID
    || kms.key_states?.[configuration.activeKeyID] !== 'ENCRYPT_DECRYPT'
    || kms.key_states?.[configuration.previousKeyID] !== 'DISABLED') {
    throw new Error('production_security_kms_rotation_unverified');
  }

  const checkedAt = new Date().toISOString();
  const evidence = {
    decision: 'GO',
    checked_at: checkedAt,
    public_endpoints: endpointEvidence,
    token_rotation_status: ready.status,
    active_key_id: configuration.activeKeyID,
    retired_key_id: configuration.previousKeyID,
    retired_key_state: kms.key_states[configuration.previousKeyID],
  };
  return { ...evidence, evidence_sha256: createHash('sha256').update(JSON.stringify(evidence)).digest('hex') };
}

if (isMainModule(import.meta.url)) {
  try {
    const configuration = productionSecurityConfiguration();
    const args = process.argv.slice(2);
    const prepare = args.length === 1 && args[0] === '--prepare-native-rotation';
    if (args.length && !prepare) throw new Error('production_security_arguments_invalid');
    if (prepare && configuration.provider !== 'vault-transit') throw new Error('production_security_native_provider_required');
    const evidence = prepare ? await prepareNativeRotation(configuration.native) : await runProductionSecurityGate(configuration);
    process.stdout.write(`${JSON.stringify(evidence)}\n`);
  } catch (error) {
    const message = String(error?.message || '');
    process.stderr.write(`${/^(?:production_security|native_kms_gate)_[A-Za-z0-9_:-]+$/.test(message) ? message : 'production_security_check_failed'}\n`);
    process.exitCode = 1;
  }
}
