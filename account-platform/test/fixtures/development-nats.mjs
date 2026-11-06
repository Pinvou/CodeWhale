import { generateKeyPairSync, sign } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { DEV_NATS_ROLES, sha256 } from '../../../scripts/account-platform-dev-nats.mjs';
const der = (tag, ...values) => { const body = Buffer.concat(values); const length = body.length < 128 ? Buffer.from([body.length]) : body.length < 256 ? Buffer.from([129, body.length]) : Buffer.from([130, body.length >> 8, body.length & 255]); return Buffer.concat([Buffer.from([tag]), length, body]); };
const oid = value => { const parts = value.split('.').map(Number); const result = [parts[0] * 40 + parts[1]]; for (const value of parts.slice(2)) { let n = value; const group = [n & 127]; while (n >>= 7) group.unshift(128 | n & 127); result.push(...group); } return der(6, Buffer.from(result)); };
const sequence = (...values) => der(48, ...values);
const algorithm = sequence(oid('1.3.101.112'));
const name = value => sequence(der(49, sequence(oid('2.5.4.3'), der(12, Buffer.from(value)))));
const extension = (id, value) => sequence(oid(id), der(4, value));
const pem = (type, value) => `-----BEGIN ${type}-----\n${value.toString('base64').match(/.{1,64}/g).join('\n')}\n-----END ${type}-----\n`;
function certificate(pair, issuerPair, ca) {
  const from = new Date(Date.now() - 60000), until = new Date(Date.now() + 3600000);
  const date = value => der(24, Buffer.from(value.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z')));
  const extensions = ca ? [extension('2.5.29.19', sequence(der(1, Buffer.from([255])))), extension('2.5.29.15', der(3, Buffer.from([2, 4])))] : [extension('2.5.29.17', sequence(der(130, Buffer.from('nats')))), extension('2.5.29.15', der(3, Buffer.from([7, 128]))), extension('2.5.29.37', sequence(oid('1.3.6.1.5.5.7.3.1')))];
  const tbs = sequence(der(160, der(2, Buffer.from([2]))), der(2, Buffer.from([ca ? 1 : 2])), algorithm, name('development fixture CA'), sequence(date(from), date(until)), name(ca ? 'development fixture CA' : 'nats'), pair.publicKey.export({ format: 'der', type: 'spki' }), der(163, sequence(...extensions)));
  return pem('CERTIFICATE', sequence(tbs, algorithm, der(3, Buffer.concat([Buffer.from([0]), sign(null, tbs, issuerPair.privateKey)]))));
}
// Syntax/assembly fixture, not an issued identity, canonical ACL or cutover receipt.
// Real Go CLI/NATS tests separately validate those production contracts.
export function developmentNATSFixture(directory) {
  const file = (folder, name, content) => { mkdirSync(path.join(directory, folder), { mode: 0o700, recursive: true }); const target = path.join(directory, folder, name); writeFileSync(target, content, { mode: 0o600 }); return target; };
  const ca = generateKeyPairSync('ed25519'), server = generateKeyPairSync('ed25519');
  const config = { version: 1, uid: process.getuid?.() || 65532, gid: process.getgid?.() || 65532,
    caFile: file('tls', 'ca.crt', certificate(ca, ca, true)), serverCertificateFile: file('tls', 'server.crt', certificate(server, ca, false)), serverKeyFile: file('tls', 'server.key', server.privateKey.export({ format: 'pem', type: 'pkcs8' })),
    authorizationFile: file('authorization', 'authorization.conf', '{"authorization":{"users":[]}}'), currentPublicKeysFile: '', manifestFile: '', seeds: {}, seedSHA256: {}, dataDirectory: path.join(directory, 'data'), receiptFile: path.join(directory, 'receipt', 'receipt.json'), receiptSHA256: '' };
  config.authorizationSHA256 = sha256(Buffer.from('{"authorization":{"users":[]}}'));
  const roles = [...DEV_NATS_ROLES, 'token-outbox-relay', 'token-command-executor'];
  const identity = Buffer.from(JSON.stringify({ version: 2, purpose: 'pinvou-nats-service-identities', public_keys: Object.fromEntries(roles.map((role, index) => [role, 'U' + String.fromCharCode(65 + index).repeat(55)])) }));
  config.currentPublicKeysFile = file('identity', 'current.json', identity); config.currentPublicKeysSHA256 = sha256(identity);
  const manifest = Buffer.from(JSON.stringify({ schema: 'approval-consumer-resources-v1', identity_sha256: sha256(identity), stream: 'ACCOUNT_PLATFORM', consumers: [{ workload: 'billing-command-executor', durable: 'fixture-only-durable' }] }));
  config.manifestFile = file('manifest', 'manifest.json', manifest); config.manifestSHA256 = sha256(manifest);
  DEV_NATS_ROLES.forEach(role => { const raw = `syntax-fixture-not-an-issued-seed-${role}`; config.seeds[role] = file(role, 'nkey.seed', raw); config.seedSHA256[role] = sha256(Buffer.from(raw)); });
  mkdirSync(config.dataDirectory, { mode: 0o700 }); mkdirSync(path.dirname(config.receiptFile), { mode: 0o700 });
  const declaration = file('declaration', 'development.json', JSON.stringify(config));
  return { config, declaration, environment: { PINVOU_DEV_NATS_CONFIG_FILE: declaration }, save: () => writeFileSync(declaration, JSON.stringify(config), { mode: 0o600 }) };
}
