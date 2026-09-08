import { createHash, randomBytes, X509Certificate } from 'node:crypto';
import { constants, promises as fs } from 'node:fs';
import https from 'node:https';
import { spawn } from 'node:child_process';
import path from 'node:path';
import net from 'node:net';

const roles = ['pinvou-token-gateway', 'pinvou-credential-portal'];
const hash = value => createHash('sha256').update(value).digest('hex');
const check = (condition, code) => { if (!condition) throw new Error(`native_kms_gate_${code}`); };
const absolute = (environment, name) => {
  const value = String(environment[name] || '');
  check(path.isAbsolute(value), `configuration_${name}`);
  return path.resolve(value);
};

export function nativeKmsConfiguration(environment) {
  check(environment.PINVOU_SECURITY_INSTALL_MODE === 'fresh-install', 'fresh_install_required');
  check(!environment.TOKEN_SECRET_PROVIDER_TOKEN && !environment.PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID, 'legacy_configuration_rejected');
  let endpoint;
  try { endpoint = new URL(environment.TOKEN_SECRET_PROVIDER_URL); } catch { check(false, 'endpoint_invalid'); }
  check(endpoint.protocol === 'https:' && !endpoint.username && !endpoint.password && !endpoint.search && !endpoint.hash && endpoint.pathname === '/', 'endpoint_invalid');
  const key = String(environment.TOKEN_SECRET_KEY_ID || '');
  check(/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$/.test(key), 'key_invalid');
  const tokenFiles = [absolute(environment, 'TOKEN_SECRET_PROVIDER_TOKEN_FILE'), absolute(environment, 'PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE')];
  check(tokenFiles[0] !== tokenFiles[1], 'shared_identity_rejected');
  return {
    endpoint: endpoint.origin, key, tokenFiles,
    caFile: absolute(environment, 'TOKEN_SECRET_PROVIDER_CA_FILE'),
    proofFile: absolute(environment, 'PINVOU_SECURITY_NATIVE_ROTATION_PROOF_FILE'),
    databaseFile: absolute(environment, 'PINVOU_SECURITY_FRESH_DATABASE_URL_FILE'),
  };
}

export async function readGateFile(filename, privateFile = true, maximum = 65536) {
  let handle;
  try {
    const before = await fs.lstat(filename);
    check(before.isFile() && !before.isSymbolicLink() && before.size > 0 && before.size <= maximum, 'file_invalid');
    check(await fs.realpath(filename) === path.resolve(filename), 'file_alias_rejected');
    if (process.platform !== 'win32') check((before.mode & (privateFile ? 0o077 : 0o022)) === 0, 'file_permissions');
    handle = await fs.open(filename, constants.O_RDONLY | (constants.O_NOFOLLOW || 0));
    const opened = await handle.stat();
    check(opened.isFile() && opened.dev === before.dev && opened.ino === before.ino, 'file_changed');
    const body = Buffer.alloc(maximum + 1);
    let length = 0;
    while (length < body.length) {
      const { bytesRead } = await handle.read(body, length, body.length - length, null);
      if (!bytesRead) break;
      length += bytesRead;
    }
    check(length > 0 && length <= maximum, 'file_size');
    return body.subarray(0, length);
  } catch {
    throw new Error('native_kms_gate_file_unavailable_or_insecure');
  } finally { await handle?.close(); }
}

export function validateNativeLease(data, role, now = Date.now()) {
  check(data && JSON.stringify(data.policies) === JSON.stringify([role]) && data.type === 'service'
    && (data.identity_policies === undefined || (Array.isArray(data.identity_policies) && data.identity_policies.length === 0))
    && data.renewable === true && data.orphan === true && data.path === 'auth/approle/login'
    && data.meta?.role_name === role && Number.isInteger(data.ttl) && data.ttl > 0 && data.ttl <= 300
    && data.explicit_max_ttl === 900, 'identity_or_lease_invalid');
  const renewed = Date.parse(data.last_renewal || '');
  check(Number.isFinite(renewed) && renewed > data.creation_time * 1000 && renewed <= now + 5000, 'renewal_unverified');
}

// Direct HTTPS, private CA only, no proxy, redirect, retry or unbounded body.
async function request(configuration, ca, apiPath, token, payload) {
  return await new Promise((resolve, reject) => {
    const body = payload === undefined ? undefined : Buffer.from(JSON.stringify(payload));
    const req = https.request(configuration.endpoint + '/v1/' + apiPath, {
      method: body ? 'POST' : 'GET', ca, minVersion: 'TLSv1.2', agent: false,
      headers: { Accept: 'application/json', ...(token ? { 'X-Vault-Token': token } : {}),
        ...(body ? { 'Content-Type': 'application/json', 'Content-Length': body.length } : {}) },
    });
    const fail = () => reject(new Error('native_kms_gate_request_failed'));
    const timer = setTimeout(() => req.destroy(new Error('deadline')), 10000);
    req.on('error', () => { clearTimeout(timer); fail(); });
    req.on('socket', socket => socket.once('secureConnect', () => {
      const expires = Date.parse(socket.getPeerCertificate()?.valid_to || '');
      if (!Number.isFinite(expires) || expires <= Date.now() + 14 * 86400000) req.destroy(new Error('certificate_expiring'));
    }));
    req.on('response', response => {
      const chunks = []; let size = 0;
      response.on('error', () => { clearTimeout(timer); fail(); });
      response.on('data', chunk => {
        size += chunk.length;
        if (size > 65536) { response.destroy(); req.destroy(); }
        else chunks.push(chunk);
      });
      response.on('end', () => {
        clearTimeout(timer);
        try { resolve({ status: response.statusCode, body: JSON.parse(Buffer.concat(chunks).toString()) }); }
        catch { fail(); }
      });
    });
    req.end(body);
  });
}

async function connection(configuration, invoke = request) {
  const ca = await readGateFile(configuration.caFile, false);
  check(!ca.includes(Buffer.from('PRIVATE KEY')), 'ca_contains_private_material');
  const pem = ca.toString();
  const certificates = pem.match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g) || [];
  check(certificates.length > 0 && !pem.replace(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g, '').trim(), 'ca_invalid');
  for (const item of certificates) {
    let certificate;
    try { certificate = new X509Certificate(item); } catch { check(false, 'ca_invalid'); }
    check(certificate.ca && Date.parse(certificate.validFrom) <= Date.now()
      && Date.parse(certificate.validTo) > Date.now() + 14 * 86400000, 'ca_invalid_or_expiring');
  }
  const caHash = hash(ca);
  const health = await invoke(configuration, ca, 'sys/health');
  check(health.status === 200 && health.body.initialized === true && health.body.sealed === false, 'health_failed');
  const call = async (index, apiPath, payload, expected = 200) => {
    const token = (await readGateFile(configuration.tokenFiles[index], true, 32768)).toString().trim();
    check(/^[A-Za-z0-9._-]{16,32768}$/.test(token), 'token_invalid');
    if (apiPath !== 'auth/token/lookup-self') {
      // Agent may replace the file between operations. Bind each crypto call to
      // the exact token just checked, not a role observed on an earlier read.
      const lease = await invoke(configuration, ca, 'auth/token/lookup-self', token);
      check(lease.status === 200, 'identity_lookup_failed');
      validateNativeLease(lease.body?.data, roles[index]);
    }
    const response = await invoke(configuration, ca, apiPath, token, payload);
    check(response.status === expected, 'operation_rejected');
    return response.body?.data;
  };
  const identities = [];
  for (let index = 0; index < roles.length; index++) {
    const data = await call(index, 'auth/token/lookup-self');
    validateNativeLease(data, roles[index]);
    check(typeof data.id === 'string' && data.id.length >= 16, 'identity_missing');
    identities.push(hash(data.id));
  }
  check(identities[0] !== identities[1], 'shared_identity_rejected');
  return { call, caHash };
}

export function databaseQuery(key) {
  check(/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$/.test(key), 'key_invalid');
  const tables = ['provider_credentials', 'usage_dispatch_preparations', 'usage_dispatch_intents', 'usage_stream_events', 'usage_replay_results', 'credential_isolation_sessions'];
  const count = condition => tables.map(table => `(SELECT count(*) FROM public.${table}${condition})`).join('+');
  const encryptedColumns = ['ciphertext', 'encrypted_envelope', 'encrypted_envelope', 'encrypted_payload', 'encrypted_payload', 'pkce_verifier_ciphertext'];
  // Completed credential sessions deliberately erase their PKCE bytes/key ID.
  // Empty tombstones are not historical ciphertext; prepare still counts rows.
  const foreign = tables.map((table, index) => `(SELECT count(*) FROM public.${table} WHERE octet_length(${encryptedColumns[index]})>0 AND (key_id IS NULL OR key_id<>'${key}'))`).join('+');
  return `BEGIN READ ONLY; SELECT json_build_object('identity', json_build_array(current_database(),inet_server_addr()::text,inet_server_port(),(SELECT oid FROM pg_database WHERE datname=current_database()),pg_postmaster_start_time()::text),'total',${count('')},'foreign',${foreign}); ROLLBACK;`;
}

export async function inspectFreshDatabase(configuration) {
  let url;
  try { url = new URL((await readGateFile(configuration.databaseFile)).toString().trim()); }
  catch { check(false, 'database_configuration_invalid'); }
  check(['postgres:', 'postgresql:'].includes(url.protocol) && url.hostname && url.username && url.password
    && url.pathname.length > 1 && !url.hash && url.searchParams.get('sslmode') === 'verify-full', 'database_tls_required');
  const caFile = url.searchParams.get('sslrootcert') || '';
  check(path.isAbsolute(caFile), 'database_ca_required');
  const databaseCA = await readGateFile(caFile, false);
  check(!databaseCA.includes(Buffer.from('PRIVATE KEY')), 'database_ca_contains_private_material');
  const optionKeys = [...url.searchParams.keys()];
  check(new Set(optionKeys).size === optionKeys.length && optionKeys.every(key => ['sslmode', 'sslrootcert', 'hostaddr'].includes(key)), 'database_options_rejected');
  const hostAddress = url.searchParams.get('hostaddr');
  check(!hostAddress || net.isIP(hostAddress), 'database_hostaddr_invalid');
  const environment = {
    PATH: process.env.PATH, ...(process.env.SystemRoot ? { SystemRoot: process.env.SystemRoot } : {}),
    PGHOST: url.hostname, PGPORT: url.port || '5432', PGUSER: decodeURIComponent(url.username),
    PGPASSWORD: decodeURIComponent(url.password), PGDATABASE: decodeURIComponent(url.pathname.slice(1)),
    PGSSLMODE: 'verify-full', PGSSLROOTCERT: caFile, PGTZ: 'UTC', PGCONNECT_TIMEOUT: '5',
    PGOPTIONS: '-c default_transaction_read_only=on -c statement_timeout=5000 -c lock_timeout=2000 -c row_security=off -c search_path=pg_catalog',
    ...(hostAddress ? { PGHOSTADDR: hostAddress } : {}),
  };
  const raw = await new Promise((resolve, reject) => {
    const child = spawn('psql', ['-X', '-q', '-t', '-A', '-w', '-v', 'ON_ERROR_STOP=1', '-c', databaseQuery(configuration.key)],
      { env: environment, windowsHide: true, stdio: ['ignore', 'pipe', 'ignore'] });
    let output = '', overflow = false;
    const timer = setTimeout(() => { overflow = true; child.kill(); }, 12000);
    child.stdout.on('data', chunk => { output += chunk; if (output.length > 4096) { overflow = true; child.kill(); } });
    child.on('error', () => { clearTimeout(timer); reject(new Error('native_kms_gate_database_query_failed')); });
    child.on('close', code => {
      clearTimeout(timer);
      if (code !== 0 || overflow) reject(new Error('native_kms_gate_database_query_failed'));
      else resolve(output);
    });
  });
  let result;
  try { result = JSON.parse(raw); } catch { check(false, 'database_result_invalid'); }
  check(Array.isArray(result.identity) && result.identity.length === 5 && result.identity.every(value => value !== null)
    && Number.isSafeInteger(result.total) && result.total >= 0 && Number.isSafeInteger(result.foreign) && result.foreign >= 0,
  'database_result_invalid');
  return { identity: hash(JSON.stringify(result.identity)), total: result.total, foreign: result.foreign };
}

export function ciphertextVersion(value) {
  const match = /^vault:v([1-9][0-9]*):[A-Za-z0-9+/]+={0,2}$/.exec(value || '');
  check(match && value.length < 4096 && Number.isSafeInteger(Number(match[1])), 'ciphertext_invalid');
  return Number(match[1]);
}

export async function prepareNativeRotation(configuration, dependencies = {}) {
  const database = await (dependencies.database || inspectFreshDatabase)(configuration);
  check(database.total === 0 && database.foreign === 0, 'database_not_fresh');
  const { call, caHash } = await connection(configuration, dependencies.request);
  const plaintext = randomBytes(32).toString('base64');
  const aad = randomBytes(32).toString('base64');
  const result = await call(0, 'transit/encrypt/' + configuration.key, { plaintext, associated_data: aad });
  const version = ciphertextVersion(result?.ciphertext);
  const proof = { schema: 1, prepared_at: new Date().toISOString(), endpoint: configuration.endpoint, ca_sha256: caHash,
    key: configuration.key, database: database.identity, plaintext, aad, ciphertext: result.ciphertext, version };
  // Exclusive state file. Never overwrite an earlier or uncertain attempt.
  const parent = await fs.lstat(path.dirname(configuration.proofFile));
  check(parent.isDirectory() && !parent.isSymbolicLink()
    && await fs.realpath(path.dirname(configuration.proofFile)) === path.dirname(configuration.proofFile)
    && (process.platform === 'win32' || (parent.mode & 0o077) === 0), 'proof_directory_insecure');
  const handle = await fs.open(configuration.proofFile, 'wx', 0o600);
  try { await handle.writeFile(JSON.stringify(proof)); await handle.sync(); }
  finally { await handle.close(); }
  return { decision: 'PREPARED_NOT_GO', mode: 'fresh-install', provider: 'vault-transit', version,
    proof_sha256: hash(await readGateFile(configuration.proofFile)) };
}

export async function verifyNativeRotation(configuration, dependencies = {}) {
  const bytes = await readGateFile(configuration.proofFile);
  let proof;
  try { proof = JSON.parse(bytes); } catch { check(false, 'proof_invalid'); }
  check(proof && typeof proof === 'object' && JSON.stringify(Object.keys(proof).sort()) === JSON.stringify([
    'schema', 'prepared_at', 'endpoint', 'ca_sha256', 'key', 'database', 'plaintext', 'aad', 'ciphertext', 'version',
  ].sort()), 'proof_schema_invalid');
  const prepared = Date.parse(proof.prepared_at);
  check(proof.schema === 1 && proof.endpoint === configuration.endpoint && proof.key === configuration.key
    && Number.isFinite(prepared) && prepared <= Date.now() && Date.now() - prepared < 86400000
    && /^[A-Za-z0-9+/]{43}=$/.test(proof.plaintext) && /^[A-Za-z0-9+/]{43}=$/.test(proof.aad)
    && ciphertextVersion(proof.ciphertext) === proof.version, 'proof_invalid_or_expired');
  const database = await (dependencies.database || inspectFreshDatabase)(configuration);
  check(database.identity === proof.database && database.foreign === 0, 'database_changed_or_legacy_ciphertext');
  const { call, caHash } = await connection(configuration, dependencies.request);
  check(caHash === proof.ca_sha256, 'ca_changed');
  const versions = [];
  for (let index = 0; index < 2; index++) {
    const result = await call(index, 'transit/encrypt/' + configuration.key, { plaintext: proof.plaintext, associated_data: proof.aad });
    const version = ciphertextVersion(result?.ciphertext);
    check(version > proof.version, 'rotation_not_observed');
    const current = await call(index, 'transit/decrypt/' + configuration.key, { ciphertext: result.ciphertext, associated_data: proof.aad });
    check(current?.plaintext === proof.plaintext, 'current_probe_not_decryptable');
    const decoded = await call(index, 'transit/decrypt/' + configuration.key, { ciphertext: proof.ciphertext, associated_data: proof.aad });
    check(decoded?.plaintext === proof.plaintext, 'historical_probe_not_decryptable');
    await call(index, 'transit/decrypt/' + configuration.key,
      { ciphertext: result.ciphertext, associated_data: randomBytes(32).toString('base64') }, 400);
    versions.push(version);
  }
  check(versions[0] === versions[1], 'rotation_changed_during_gate');
  return { provider: 'vault-transit', install_mode: 'fresh-install', key_id: configuration.key,
    previous_version: proof.version, active_version: versions[0], historical_probe_decryptable: true,
    aad_mismatch_rejected: true, identities: roles, leases_renewed: true, database_sha256: database.identity,
    foreign_key_ciphertexts: database.foreign, proof_sha256: hash(bytes), ca_sha256: caHash };
}
