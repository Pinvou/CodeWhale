// Offline binding of an independently controlled manifest to ordinary local artifacts.
// This verifies bytes only; reality of measurements and release approval need independent review.
import { createHash } from 'node:crypto';
import { constants } from 'node:fs';
import { lstat, open, realpath } from 'node:fs/promises';
import path from 'node:path';

const hash = /^[a-f0-9]{64}$/;
const identifier = /^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$/;
function required(value) { if (!value) throw new Error('artifact_bindings_invalid'); }
function within(root, target) { const relative = path.relative(root, target); return relative === '' || (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)); }
function exact(value, fields) { required(value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === fields.length && fields.every(field => Object.prototype.hasOwnProperty.call(value, field))); }
function decode(bytes) {
  const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  const value = JSON.parse(text);
  const tokens = text.match(/"(?:\\[\s\S]|[^"\\])*"|[{}\[\]:,]|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/g) || [];
  const stack = [];
  for (let i = 0; i < tokens.length; i += 1) {
    const token = tokens[i];
    if (token === '{') stack.push(new Set());
    else if (token === '[') stack.push(null);
    else if (token === '}' || token === ']') stack.pop();
    else if (token.startsWith('"') && tokens[i + 1] === ':') { const key = JSON.parse(token); required(!stack.at(-1).has(key)); stack.at(-1).add(key); }
    else if (/^-?\d/.test(token)) {
      const number = Number(token);
      required(token.length <= 128 && Number.isFinite(number) && (!Number.isInteger(number) || Number.isSafeInteger(number))
        && normalizedDecimal(token) === normalizedDecimal(String(number)));
    }
  }
  return value;
}
// Match the existing signed evidence reader: decimals must not round onto a gate
// threshold or underflow to zero during JSON decoding.
function normalizedDecimal(token) {
  const match = /^(-?)(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?$/.exec(token);
  required(match && (match[4] || '').length <= 6);
  let exponent = BigInt(match[4] || '0') - BigInt((match[3] || '').length);
  required(exponent >= -10000n && exponent <= 10000n);
  let digits = `${match[2]}${match[3] || ''}`.replace(/^0+/, '');
  if (!digits) return '0';
  while (digits.endsWith('0')) { digits = digits.slice(0, -1); exponent += 1n; }
  return `${match[1]}${digits}e${exponent}`;
}
async function ordinary(filename) {
  const absolute = path.resolve(filename);
  let current = path.parse(absolute).root;
  for (const part of absolute.slice(current.length).split(path.sep).filter(Boolean)) { current = path.join(current, part); required(!(await lstat(current)).isSymbolicLink()); }
  required(await realpath(absolute) === absolute);
  return absolute;
}
async function readBound(filename, json = false) {
  let handle;
  try {
    const before = await lstat(filename);
    required(before.isFile() && !before.isSymbolicLink() && before.nlink === 1 && Number.isSafeInteger(before.size) && before.size > 0 && (!json || before.size <= 1024 * 1024));
    handle = await open(filename, constants.O_RDONLY | (constants.O_NOFOLLOW || 0));
    const opened = await handle.stat(); required(opened.dev === before.dev && opened.ino === before.ino && opened.size === before.size && opened.isFile());
    const digest = createHash('sha256'); const chunks = []; const buffer = Buffer.alloc(64 * 1024); let total = 0;
    while (true) { const read = await handle.read(buffer, 0, buffer.length, null); if (!read.bytesRead) break; const bytes = buffer.subarray(0, read.bytesRead); digest.update(bytes); if (json) chunks.push(Buffer.from(bytes)); total += read.bytesRead; required(total <= opened.size); }
    const after = await handle.stat(); const current = await lstat(filename);
    required(total === opened.size && after.size === opened.size && after.mtimeMs === opened.mtimeMs && after.ctimeMs === opened.ctimeMs && current.dev === opened.dev && current.ino === opened.ino && current.size === opened.size && current.mtimeMs === opened.mtimeMs && current.ctimeMs === opened.ctimeMs && !current.isSymbolicLink());
    return { sha256: digest.digest('hex'), value: json ? decode(Buffer.concat(chunks)) : undefined };
  } finally { await handle?.close(); }
}
function claims(value, pointer = '', result = new Map()) {
  if (value && typeof value === 'object') for (const [key, item] of Object.entries(value)) {
    const location = `${pointer}/${key.replace(/~/g, '~0').replace(/\//g, '~1')}`;
    if (key.endsWith('_sha256') && key !== 'release_sha256') { required(typeof item === 'string' && hash.test(item)); result.set(location, item); }
    else claims(item, location, result);
  }
  return result;
}
export function artifactGateArguments(args) {
  const names = ['trust-file', 'artifact-root', 'deployment-id', 'expected-release-sha256']; const options = {}; const remaining = [];
  for (const arg of args) {
    const name = names.find(item => arg.startsWith(`--${item}=`));
    if (!name) { remaining.push(arg); continue; }
    required(!Object.prototype.hasOwnProperty.call(options, name) && arg.length > name.length + 3); options[name] = arg.slice(name.length + 3);
  }
  return { remaining, trustPath: options['trust-file'], rootPath: options['artifact-root'], deploymentId: options['deployment-id'], expectedReleaseSHA256: options['expected-release-sha256'] };
}
export async function verifyArtifactBindings({ evidencePath, trustPath, rootPath, kind, environment, deploymentId, expectedReleaseSHA256 }) {
  try {
    required(trustPath && rootPath && identifier.test(deploymentId || '') && hash.test(expectedReleaseSHA256 || ''));
    const root = await ordinary(rootPath); required((await lstat(root)).isDirectory());
    const trustFile = await ordinary(trustPath); const evidenceFile = await ordinary(evidencePath);
    required(!within(root, trustFile) && within(root, evidenceFile));
    const [trusted, captured] = await Promise.all([readBound(trustFile, true), readBound(evidenceFile, true)]); const trust = trusted.value; const evidence = captured.value;
    exact(trust, ['schema_version', 'kind', 'environment', 'deployment_id', 'release_sha256', 'evidence_sha256', 'artifacts']);
    required(trust.schema_version === 1 && trust.kind === kind && trust.environment === environment && trust.deployment_id === deploymentId && trust.release_sha256 === expectedReleaseSHA256 && trust.evidence_sha256 === captured.sha256 && evidence.environment === environment);
    if (Object.prototype.hasOwnProperty.call(evidence, 'release_sha256')) required(evidence.release_sha256 === expectedReleaseSHA256);
    if (Object.prototype.hasOwnProperty.call(evidence, 'deployment_id')) required(evidence.deployment_id === deploymentId);
    const declared = claims(evidence); required(declared.size > 0 && Array.isArray(trust.artifacts) && trust.artifacts.length === declared.size);
    const seen = new Set(); const files = new Map(); const artifacts = new Map();
    for (const item of trust.artifacts) {
      exact(item, ['claim', 'path', 'sha256']); required(typeof item.claim === 'string' && !seen.has(item.claim) && declared.get(item.claim) === item.sha256 && hash.test(item.sha256)); seen.add(item.claim);
      required(typeof item.path === 'string' && /^[A-Za-z0-9_-][A-Za-z0-9_.-]*(?:\/[A-Za-z0-9_.-]+)*$/.test(item.path) && !item.path.split('/').some(part => part === '.' || part === '..'));
      const file = await ordinary(path.resolve(root, item.path)); required(within(root, file) && file !== evidenceFile && file !== trustFile);
      if (!files.has(file)) files.set(file, (await readBound(file)).sha256); required(files.get(file) === item.sha256); artifacts.set(item.claim, { path: file, sha256: files.get(file) });
    }
    return { evidence, artifacts, result: Object.freeze({ status: 'ARTIFACT_BINDINGS_VERIFIED', kind, environment, deployment_id: deploymentId, release_sha256: expectedReleaseSHA256, evidence_sha256: captured.sha256, artifacts_verified: files.size, deployment_status: 'NOT_DEPLOYED', exercise_verified: false, release_authorized: false }) };
  } catch { throw new Error('artifact_bindings_invalid'); }
}
