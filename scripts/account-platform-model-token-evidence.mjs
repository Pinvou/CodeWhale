#!/usr/bin/env node
// Offline verification only. Trust roots are external inputs, never report-provided keys.
import { createHash, createPublicKey, verify } from 'node:crypto';
import { constants } from 'node:fs';
import { lstat, open, realpath, readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { parseReleaseJSON, validateModelTokenReleasePlan, validateModelTokenEvidenceEnvelope } from './account-platform-model-token-release-plan.mjs';
import { evidenceKinds, evidenceRejected as reject, evidenceSignedBytes } from './account-platform-model-token-evidence-policy.mjs';
import { validateReportSemantics } from './account-platform-model-token-evidence-report.mjs';

const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const Ajv = require('ajv'); const addFormats = require('ajv-formats');
const ajv = new Ajv({ strict: true, allErrors: false, coerceTypes: false, useDefaults: false });
addFormats(ajv);
const schemaRoot = new URL('../account-platform/contracts/deployment/', import.meta.url);
ajv.addSchema(JSON.parse(await readFile(new URL('model-token-release-evidence.schema.json', schemaRoot), 'utf8')));
const validators = {};
for (const name of ['bundle', 'trust', 'report']) validators[name] = ajv.compile(JSON.parse(await readFile(new URL(`model-token-evidence-${name}.schema.json`, schemaRoot), 'utf8')));
const JSON_LIMIT = 1024 * 1024;
const ARTIFACT_LIMIT = 4 * 1024 * 1024;
const TOTAL_LIMIT = 32 * 1024 * 1024;
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

function within(root, target) {
  const relative = path.relative(root, target);
  return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
}

// Reject link traversal and pin each read to the previously inspected ordinary file.
// Inputs must be a controlled immutable local export, not a concurrently modified workspace.
async function boundedFile(filename, limit) {
  let handle;
  try {
    const before = await lstat(filename);
    if (!before.isFile() || before.isSymbolicLink() || before.size < 1 || before.size > limit) reject('file_invalid');
    handle = await open(filename, constants.O_RDONLY | (constants.O_NOFOLLOW || 0));
    const opened = await handle.stat();
    if (!opened.isFile() || opened.dev !== before.dev || opened.ino !== before.ino || opened.size !== before.size) reject('file_changed');
    const bytes = Buffer.alloc(opened.size);
    let offset = 0;
    while (offset < bytes.length) {
      const read = await handle.read(bytes, offset, bytes.length - offset, offset);
      if (read.bytesRead === 0) reject('file_changed');
      offset += read.bytesRead;
    }
    const after = await handle.stat(); const current = await lstat(filename);
    if (after.size !== opened.size || after.mtimeMs !== opened.mtimeMs || after.ctimeMs !== opened.ctimeMs
      || current.isSymbolicLink() || current.ino !== opened.ino || current.dev !== opened.dev) reject('file_changed');
    return bytes;
  } catch (error) {
    if (/^model_token_evidence_/.test(error?.message)) throw error;
    reject('file_unavailable');
  } finally { await handle?.close(); }
}

async function containedFile(root, relative, limit) {
  if (typeof relative !== 'string' || !/^[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*\.(?:json|ndjson|txt)$/.test(relative)) reject('path_invalid');
  const filename = path.resolve(root, relative);
  if (!within(root, filename)) reject('path_outside_root');
  let current = root;
  for (const part of relative.split('/')) {
    current = path.join(current, part);
    let info;
    try { info = await lstat(current); } catch { reject('file_unavailable'); }
    if (info.isSymbolicLink()) reject('path_link');
  }
  let resolved;
  try { resolved = await realpath(filename); } catch { reject('file_unavailable'); }
  if (!within(root, resolved) || resolved !== filename) reject('path_outside_root');
  return boundedFile(filename, limit);
}

function decodedJSON(bytes) {
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    const value = parseReleaseJSON(text);
    // Compare decimal values, not IEEE-754 binary expansions: 0.10 and 1e1 remain
    // valid, but a value rounded onto a threshold or underflowed to zero is not.
    const tokens = text.match(/"(?:\\[\s\S]|[^"\\])*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g) || [];
    for (const token of tokens) {
      if (token.startsWith('"')) continue;
      if (token.length > 128) reject('number_invalid');
      const number = Number(token);
      if (!Number.isFinite(number) || (Number.isInteger(number) && !Number.isSafeInteger(number))
        || normalizedDecimal(token) !== normalizedDecimal(String(number))) reject('number_invalid');
    }
    return value;
  } catch { reject('json_invalid'); }
}
function normalizedDecimal(token) {
  const match = /^(-?)(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?$/.exec(token);
  if (!match || (match[4] || '').length > 6) reject('number_invalid');
  let exponent = BigInt(match[4] || '0') - BigInt((match[3] || '').length);
  if (exponent < -10000n || exponent > 10000n) reject('number_invalid');
  let digits = `${match[2]}${match[3] || ''}`.replace(/^0+/, '');
  if (!digits) return '0';
  while (digits.endsWith('0')) { digits = digits.slice(0, -1); exponent += 1n; }
  return `${match[1]}${digits}e${exponent}`;
}
function canonicalBase64(value, length) {
  const bytes = Buffer.from(value, 'base64url');
  if (bytes.length !== length || bytes.toString('base64url') !== value) reject('signature_encoding_invalid');
  return bytes;
}
function signatureKey(value) {
  try {
    const raw = canonicalBase64(value, 32);
    return createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), raw]), format: 'der', type: 'spki' });
  } catch { reject('public_key_invalid'); }
}
function validateEntryTrust(entry, trust, plan, now) {
  const envelope = entry.envelope;
  const key = trust.keys.find(item => item.key_id === entry.key_id);
  if (!key || key.source !== entry.source || key.environment !== plan.environment || !key.kinds.includes(envelope.kind)
    || !key.executor_references.includes(envelope.executor_reference) || !key.reviewer_references.includes(envelope.reviewer_reference)
    || !key.approval_references.includes(envelope.approval_reference) || envelope.approval_reference !== plan.approval_reference) reject('source_not_authorized');
  const validFrom = Date.parse(key.valid_from); const validUntil = Date.parse(key.valid_until);
  const start = Date.parse(envelope.started_at); const end = Date.parse(envelope.ended_at);
  if (![validFrom, validUntil, start, end, now].every(Number.isFinite) || validFrom >= validUntil
    || validFrom > start || validUntil < end || now < validFrom || now >= validUntil
    || now - end > trust.max_age_seconds * 1000) reject('trust_window_invalid');
  if (!verify(null, evidenceSignedBytes(entry, trust.expected_plan_sha256), signatureKey(key.public_key_base64url), canonicalBase64(entry.signature_base64url, 64))) reject('signature_invalid');
}

export async function verifyModelTokenEvidence({ planPath, bundlePath, rootPath, trustPath, now = Date.now() }) {
  if (!Number.isFinite(now)) reject('time_invalid');
  let root; let trustFile; let bundleFile;
  try { root = await realpath(rootPath); trustFile = await realpath(trustPath); bundleFile = await realpath(bundlePath); } catch { reject('input_unavailable'); }
  if (!(await lstat(root)).isDirectory() || within(root, trustFile)) reject('external_trust_required');
  if (!within(root, bundleFile)) reject('bundle_outside_root');
  const [planBytes, bundleBytes, trustBytes] = await Promise.all([
    boundedFile(path.resolve(planPath), JSON_LIMIT), boundedFile(path.resolve(bundlePath), JSON_LIMIT), boundedFile(trustFile, JSON_LIMIT),
  ]);
  const plan = decodedJSON(planBytes); const bundle = decodedJSON(bundleBytes); const trust = decodedJSON(trustBytes);
  validateModelTokenReleasePlan(plan);
  if (!validators.bundle(bundle) || !validators.trust(trust)) reject('schema_invalid');
  if (sha256(planBytes) !== trust.expected_plan_sha256) reject('plan_hash_invalid');
  if (new Set(trust.keys.map(key => key.key_id)).size !== trust.keys.length) reject('key_duplicate');
  const kinds = new Set(bundle.entries.map(entry => entry.envelope.kind));
  if (kinds.size !== evidenceKinds.length || evidenceKinds.some(kind => !kinds.has(kind))) reject('kind_coverage_invalid');
  let decision = 'EVIDENCE_VERIFIED'; let retention = 'RETAIN_REQUIRED'; let total = 0;
  const consumed = new Map(); const reports = new Set();
  for (const entry of bundle.entries) {
    const envelope = entry.envelope;
    validateModelTokenEvidenceEnvelope(envelope, plan, now);
    validateEntryTrust(entry, trust, plan, now);
    if (reports.has(envelope.report_path) || consumed.has(envelope.report_path)) reject('report_path_duplicate');
    reports.add(envelope.report_path);
    const bytes = await containedFile(root, envelope.report_path, JSON_LIMIT);
    total += bytes.length;
    if (total > TOTAL_LIMIT) reject('total_size_exceeded');
    if (sha256(bytes) !== envelope.report_sha256) reject('report_hash_invalid');
    const report = decodedJSON(bytes);
    if (!validators.report(report)) reject('report_schema_invalid');
    const result = validateReportSemantics(report, envelope, plan, bundle.stage, now);
    for (const artifact of report.artifacts) {
      if (reports.has(artifact.path) || bundle.entries.some(item => item.envelope.report_path === artifact.path)) reject('artifact_report_alias');
      if (consumed.has(artifact.path)) {
        if (consumed.get(artifact.path) !== artifact.sha256) reject('artifact_hash_conflict');
        continue;
      }
      const raw = await containedFile(root, artifact.path, ARTIFACT_LIMIT);
      total += raw.length;
      if (total > TOTAL_LIMIT) reject('total_size_exceeded');
      if (sha256(raw) !== artifact.sha256) reject('artifact_hash_invalid');
      consumed.set(artifact.path, artifact.sha256);
    }
    if (result.decision === 'NO_GO' || decision === 'NO_GO') decision = 'NO_GO';
    else if (result.decision === 'HOLD') decision = 'HOLD';
    if (result.retention) retention = result.retention;
  }
  return Object.freeze({ decision, environment_status: decision === 'EVIDENCE_VERIFIED' ? 'VERIFIED' : decision === 'NO_GO' ? 'FAILED' : 'ENV_PENDING',
    deployment_status: 'NOT_DEPLOYED', environment: plan.environment, stage: bundle.stage,
    candidate: plan.candidate, plan_sha256: trust.expected_plan_sha256, bundle_sha256: sha256(bundleBytes),
    reports_verified: bundle.entries.length, artifacts_verified: consumed.size, legacy_retention: retention });
}

async function main(args) {
  const options = new Map();
  for (let index = 0; index < args.length; index += 2) {
    if (!['--plan', '--bundle', '--root'].includes(args[index]) || !args[index + 1] || options.has(args[index])) reject('usage');
    options.set(args[index], args[index + 1]);
  }
  if (options.size !== 3 || !process.env.PINVOU_MODEL_TOKEN_EVIDENCE_TRUST_FILE) reject('trust_configuration_missing');
  const result = await verifyModelTokenEvidence({ planPath: options.get('--plan'), bundlePath: options.get('--bundle'), rootPath: options.get('--root'), trustPath: process.env.PINVOU_MODEL_TOKEN_EVIDENCE_TRUST_FILE });
  process.stdout.write(`${JSON.stringify(result)}\n`);
  if (result.decision !== 'EVIDENCE_VERIFIED') process.exitCode = 1;
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  const code = /^model_token_(?:evidence|release)_[a-z_]+$/.test(error?.message) ? error.message : 'model_token_evidence_invalid';
  process.stderr.write(`${code}\n`); process.exitCode = 1;
});
