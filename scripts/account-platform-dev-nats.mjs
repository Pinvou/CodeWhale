// Development-only assembly. Daemons and canonical resource validators remain authoritative.
import { createHash, randomUUID, X509Certificate, createPrivateKey } from 'node:crypto';
import { lstatSync, realpathSync, readFileSync, mkdtempSync, mkdirSync, writeFileSync, rmSync, existsSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

export const DEV_NATS_ROLES = Object.freeze(['account-outbox-relay', 'billing-outbox-relay', 'billing-command-executor', 'account-approval-relay', 'event-resource-operator']);
const fields = ['version', 'uid', 'gid', 'caFile', 'serverCertificateFile', 'serverKeyFile', 'authorizationFile', 'authorizationSHA256', 'currentPublicKeysFile', 'currentPublicKeysSHA256', 'manifestFile', 'manifestSHA256', 'seeds', 'seedSHA256', 'dataDirectory', 'receiptFile', 'receiptSHA256'];
const fail = () => { throw new Error('DEV_NATS_MATERIALS_INVALID: see account-platform/deploy/runbooks/development-nats.md'); };
export const sha256 = raw => createHash('sha256').update(raw).digest('hex');
const digest = value => typeof value === 'string' && /^[a-f0-9]{64}$/.test(value);
function absolute(value) {
  if (typeof value !== 'string' || !path.isAbsolute(value) || path.normalize(value) !== value || /[\r\n\0]/.test(value)) fail();
  if (process.platform === 'win32' && !/^[A-Za-z]:\\/.test(value)) fail();
  return value;
}
function checkedPath(value, directory, privateFile = false) {
  absolute(value);
  const info = lstatSync(value);
  if (info.isSymbolicLink() || realpathSync(value) !== value || (directory ? !info.isDirectory() : !info.isFile())) fail();
  if (process.platform !== 'win32' && (info.mode & (privateFile ? 0o077 : 0o022))) fail();
  if (!directory && (info.size < 1 || info.size > (privateFile ? 16384 : 262144))) fail();
  return info;
}
function bytes(file, expected, privateFile = false) {
  checkedPath(file, false, privateFile);
  const raw = readFileSync(file);
  if (expected !== undefined && (!digest(expected) || sha256(raw) !== expected)) fail();
  return raw;
}
function within(parent, child) {
  const relative = path.relative(parent, child);
  return relative === '' || (!path.isAbsolute(relative) && relative !== '..' && !relative.startsWith(`..${path.sep}`));
}
function isolated(directories) {
  for (let i = 0; i < directories.length; i++) {
    for (let j = i + 1; j < directories.length; j++) {
      if (within(directories[i], directories[j]) || within(directories[j], directories[i])) fail();
    }
  }
}

export function developmentNATSConfiguration(environment = process.env, { fresh = false, cleanup = false, initializing = false } = {}) {
  try {
    const configFile = absolute(environment.PINVOU_DEV_NATS_CONFIG_FILE);
    checkedPath(path.dirname(configFile), true, true);
    const config = JSON.parse(bytes(configFile, undefined, true));
    if (!config || Object.keys(config).some(key => !fields.includes(key)) || config.version !== 1 || !Number.isSafeInteger(config.uid) || config.uid <= 0 || !Number.isSafeInteger(config.gid) || config.gid <= 0 || config.uid > 2147483647 || config.gid > 2147483647) fail();
    if (!config.seeds || !config.seedSHA256 || Object.keys(config.seeds).sort().join() !== [...DEV_NATS_ROLES].sort().join() || Object.keys(config.seedSHA256).sort().join() !== [...DEV_NATS_ROLES].sort().join()) fail();
    const fileNames = ['caFile', 'serverCertificateFile', 'serverKeyFile', 'authorizationFile', 'currentPublicKeysFile', 'manifestFile'];
    for (const key of fileNames) absolute(config[key]);
    for (const role of DEV_NATS_ROLES) { absolute(config.seeds[role]); if (path.basename(config.seeds[role]) !== 'nkey.seed') fail(); }
    if (path.basename(config.serverCertificateFile) !== 'server.crt' || path.basename(config.serverKeyFile) !== 'server.key' || path.dirname(config.serverCertificateFile) !== path.dirname(config.serverKeyFile)) fail();
    if (!fresh) { absolute(config.dataDirectory); absolute(config.receiptFile); }
    const directories = [path.dirname(config.currentPublicKeysFile), path.dirname(config.manifestFile), path.dirname(config.authorizationFile), ...DEV_NATS_ROLES.map(role => path.dirname(config.seeds[role])), path.dirname(config.serverKeyFile)];
    if (!fresh) directories.push(path.dirname(config.receiptFile), config.dataDirectory);
    isolated(directories);
    const output = {
      DEV_NATS_UID: String(config.uid), DEV_NATS_GID: String(config.gid),
      DEV_NATS_CA_FILE: config.caFile, DEV_NATS_SERVER_CERTIFICATE_FILE: config.serverCertificateFile, DEV_NATS_SERVER_KEY_FILE: config.serverKeyFile, DEV_NATS_SERVER_TLS_DIRECTORY: path.dirname(config.serverKeyFile),
      DEV_NATS_AUTHORIZATION_FILE: config.authorizationFile, DEV_NATS_PUBLIC_KEYS_FILE: config.currentPublicKeysFile,
      DEV_NATS_PUBLIC_KEYS_SHA256: config.currentPublicKeysSHA256, DEV_NATS_MANIFEST_FILE: config.manifestFile, DEV_NATS_MANIFEST_SHA256: config.manifestSHA256,
      DEV_NATS_DATA_DIRECTORY: fresh ? '' : config.dataDirectory, DEV_NATS_RECEIPT_DIRECTORY: fresh ? '' : path.dirname(config.receiptFile),
      DEV_NATS_RECEIPT_FILE_NAME: fresh ? 'receipt.json' : path.basename(config.receiptFile), DEV_NATS_RECEIPT_SHA256: fresh ? '' : config.receiptSHA256,
    };
    DEV_NATS_ROLES.forEach(role => { output[`DEV_NATS_${role.replaceAll('-', '_').toUpperCase()}_DIRECTORY`] = path.dirname(config.seeds[role]); });
    if (cleanup) return output; // Down requires the declaration, not still-valid or present material files.
    if (process.platform !== 'win32' && (process.getuid() !== config.uid || process.getgid() !== config.gid)) fail();
    for (const directory of new Set(fileNames.map(key => path.dirname(config[key])).concat(DEV_NATS_ROLES.map(role => path.dirname(config.seeds[role]))))) checkedPath(directory, true);
    for (const directory of [path.dirname(config.serverKeyFile), ...DEV_NATS_ROLES.map(role => path.dirname(config.seeds[role]))]) checkedPath(directory, true, true);
    // The real TLS broker validates the complete CA/intermediate chain. Do not
    // require a direct root signature here: the supported leaf may include intermediates.
    const ca = new X509Certificate(bytes(config.caFile));
    const certificate = new X509Certificate(bytes(config.serverCertificateFile));
    const key = createPrivateKey(bytes(config.serverKeyFile, undefined, true));
    if (!ca.ca || !certificate.checkHost('nats') || !certificate.checkPrivateKey(key) || Date.parse(certificate.validFrom) > Date.now() || Date.parse(certificate.validTo) <= Date.now() || Date.parse(ca.validFrom) > Date.now() || Date.parse(ca.validTo) <= Date.now()) fail();
    bytes(config.authorizationFile, config.authorizationSHA256);
    const identity = JSON.parse(bytes(config.currentPublicKeysFile, config.currentPublicKeysSHA256));
    if (identity.version !== 2 || identity.purpose !== 'pinvou-nats-service-identities' || !identity.public_keys || new Set(Object.values(identity.public_keys)).size !== 7 || Object.keys(identity.public_keys).length !== 7 || DEV_NATS_ROLES.some(role => !/^U[A-Z2-7]{55}$/.test(identity.public_keys[role] || ''))) fail();
    const manifest = JSON.parse(bytes(config.manifestFile, config.manifestSHA256));
    if (manifest.schema !== 'approval-consumer-resources-v1' || manifest.identity_sha256 !== config.currentPublicKeysSHA256 || manifest.stream !== 'ACCOUNT_PLATFORM' || !Array.isArray(manifest.consumers) || !manifest.consumers.length) fail();
    const seeds = DEV_NATS_ROLES.map(role => bytes(config.seeds[role], config.seedSHA256[role], true));
    if (new Set(seeds.map(sha256)).size !== seeds.length) fail();
    if (!fresh) {
      checkedPath(config.dataDirectory, true, true);
      checkedPath(path.dirname(config.receiptFile), true);
      if (!initializing) validateDevelopmentReceipt(bytes(config.receiptFile, config.receiptSHA256), output);
    }
    // Canonical Go artifact readers and the real TLS/NKey broker retain their
    // closed ACL, exact consumer and immutable receipt checks. Offline custody
    // review owns the public-key/seed mapping; this input binds its per-role digest.
    return output;
  } catch { fail(); }
}

export function newDevelopmentNATSProject(environment) {
  const parent = realpathSync(tmpdir());
  const directory = mkdtempSync(path.join(parent, 'pinvou-account-nats-'));
  const project = `pinvou-account-platform-test-${process.pid}-${randomUUID().replaceAll('-', '')}`;
  mkdirSync(path.join(directory, 'data'), { mode: 0o700 });
  mkdirSync(path.join(directory, 'receipt'), { mode: 0o700 });
  writeFileSync(path.join(directory, 'owner.json'), JSON.stringify({ project }), { mode: 0o600, flag: 'wx' });
  const output = { ...environment, COMPOSE_PROJECT_NAME: project, DEV_NATS_DATA_DIRECTORY: path.join(directory, 'data'), DEV_NATS_RECEIPT_DIRECTORY: path.join(directory, 'receipt'), DEV_NATS_RECEIPT_FILE_NAME: 'receipt.json', DEV_NATS_RECEIPT_SHA256: '' };
  return { environment: output, directory, project, parent };
}

function validateDevelopmentReceipt(raw, environment) {
  const receipt = JSON.parse(raw);
  if (receipt.schema !== 'approval-consumer-cutover-receipt-v1' || receipt.schema_version !== 1 || receipt.identity_sha256 !== environment.DEV_NATS_PUBLIC_KEYS_SHA256 || receipt.manifest_sha256 !== environment.DEV_NATS_MANIFEST_SHA256 || receipt.captured_mode !== 'QUIESCING' || receipt.captured_relay_state !== 'FENCED' || receipt.stream !== 'ACCOUNT_PLATFORM' || receipt.control !== 'ADMIN_APPROVAL' || !/^(0|[1-9][0-9]*)$/.test(receipt.high_watermark) || !Array.isArray(receipt.billing_starts) || receipt.billing_starts.length === 0) fail();
  if (['control_version', 'approval_epoch', 'authorization_relay_epoch'].some(key => !/^[1-9][0-9]*$/.test(receipt[key])) || !Number.isFinite(Date.parse(receipt.captured_at)) || Date.parse(receipt.captured_at) > Date.now() || !receipt.captured_at.endsWith('Z')) fail();
  const expected = JSON.parse(bytes(environment.DEV_NATS_MANIFEST_FILE, environment.DEV_NATS_MANIFEST_SHA256)).consumers.filter(entry => entry.workload === 'billing-command-executor').map(entry => entry.durable).sort();
  if (!expected.length || new Set(expected).size !== expected.length || receipt.billing_starts.map(entry => entry.durable).sort().join() !== expected.join() || receipt.billing_starts.some(entry => entry.start_sequence !== String(BigInt(receipt.high_watermark) + 1n))) fail();
  return receipt;
}

export function acceptFreshDevelopmentReceipt(owned) {
  const file = path.join(owned.directory, 'receipt', 'receipt.json');
  const raw = bytes(file);
  const receipt = validateDevelopmentReceipt(raw, owned.environment);
  if (receipt.control_version !== '3' || receipt.approval_epoch !== '2' || receipt.authorization_relay_epoch !== '2') fail();
  owned.environment.DEV_NATS_RECEIPT_SHA256 = sha256(raw);
  return sha256(raw);
}

export function retainDevelopmentNATSEvidence(owned, failed) {
  const directory = mkdtempSync(path.join(owned.parent, 'pinvou-account-nats-evidence-'));
  const receipt = path.join(owned.directory, 'receipt', 'receipt.json');
  // Only the bounded public CLI output is retained, never the bus data or
  // any externally supplied seed, TLS key, environment or database secret.
  if (existsSync(receipt)) {
    checkedPath(receipt, false);
    copyFileSync(receipt, path.join(directory, 'receipt.json'));
  }
  writeFileSync(path.join(directory, 'result.json'), JSON.stringify({ project: owned.project, failed: Boolean(failed), receipt_sha256: owned.environment.DEV_NATS_RECEIPT_SHA256 || null, formally_activated: false }), { mode: 0o600, flag: 'wx' });
  return directory;
}

export function removeDevelopmentNATSProject(owned) {
  const directory = path.resolve(owned.directory);
  if (directory === owned.parent || path.dirname(directory) !== owned.parent || !path.basename(directory).startsWith('pinvou-account-nats-') || realpathSync(directory) !== directory || JSON.parse(readFileSync(path.join(directory, 'owner.json'), 'utf8')).project !== owned.project) fail();
  rmSync(directory, { recursive: true, force: false });
}
