// Config wiring only. Public file contents, cutover evidence and live broker
// resources remain verified by Billing's unchanged bootstrap/readiness reader.
import { lstatSync, readdirSync, readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { physicalSourcePath } from './account-platform-production-render.mjs';
const prefix = 'BILLING_APPROVAL_CONSUMER_';
const artifacts = [
  ['PUBLIC_KEYS', '/run/pinvou/approval-identities/current.json', 'current-v2.json', 'public-keys'],
  ['MANIFEST', '/run/pinvou/approval-manifest/manifest.json', 'manifest.json', 'manifest'],
  ['RECEIPT', '/run/pinvou/approval-receipt/receipt.json', 'receipt.json', 'receipt'],
];
const equal = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const overlaps = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`) || a === '/';
function check(value, code) { if (!value) throw new Error(`billing_approval_artifacts_${code}`); }
export function checkHelmBillingApprovalArtifacts(items, secrets, namespace) {
  const deployments = items.filter(item => item.kind === 'Deployment'
    && item.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'] === 'billing-service');
  check(deployments.length === 1, 'billing_missing_or_duplicate');
  const item = deployments[0], pod = item.spec.template.spec, main = pod.containers[0];
  const environment = main.env || [], mounts = main.volumeMounts || [], volumes = pod.volumes || [];
  const runtimeSecret = environment.find(v => v.name === 'DATABASE_URL')?.valueFrom?.secretKeyRef?.name;
  check(runtimeSecret, 'runtime_secret_missing');
  const name = 'billing-approval-public-artifacts';
  const found = volumes.filter(v => v.name === name);
  check(found.length === 1 && Object.keys(found[0]).sort().join(',') === 'configMap,name', 'volume');
  const publicMap = found[0].configMap;
  check(typeof publicMap.name === 'string' && publicMap.name.length > 0 && publicMap.defaultMode === 292
    && !publicMap.optional && equal(publicMap.items, artifacts.map(([, , key]) => ({ key, path: key }))), 'public_projection');
  check(environment.filter(v => v.name.startsWith(prefix)).length === artifacts.length * 2, 'environment');
  for (const [suffix, file, key, digestKey] of artifacts) {
    const paths = environment.filter(v => v.name === `${prefix}${suffix}_FILE`);
    check(paths.length === 1 && equal(paths[0], { name: `${prefix}${suffix}_FILE`, value: file }), 'file_path');
    const digest = environment.filter(v => v.name === `${prefix}${suffix}_SHA256`);
    const ref = { name: runtimeSecret, key: `billing-approval-consumer-${digestKey}-sha256` };
    check(digest.length === 1 && equal(digest[0], { name: `${prefix}${suffix}_SHA256`, valueFrom: { secretKeyRef: ref } }), 'digest_reference');
    if (secrets !== undefined) {
      const value = secrets[`${item.metadata.namespace || namespace}/${ref.name}/${ref.key}`];
      check(typeof value === 'string' && /^[a-f0-9]{64}$/.test(value), 'digest_value');
    }
    const projected = mounts.filter(v => v.name === name && v.mountPath === file);
    check(projected.length === 1 && equal(projected[0], { name, mountPath: file, subPath: key, readOnly: true }), 'file_mount');
    const directory = file.slice(0, file.lastIndexOf('/'));
    for (const mount of mounts) if (mount !== projected[0]) check(!overlaps(mount.mountPath, directory), 'path_alias');
  }
  check(mounts.filter(v => v.name === name).length === artifacts.length, 'file_mount');
  for (const container of [...pod.containers.slice(1), ...(pod.initContainers || []), ...(pod.ephemeralContainers || [])]) {
    check(!(container.volumeMounts || []).some(v => v.name === name), 'public_scope');
  }
}

export function checkComposeBillingApprovalArtifacts(config) {
  const service = config.services?.['billing-service'];
  check(service, 'billing_missing_or_duplicate');
  const env = service.environment || {}, mounted = service.volumes || [], directories = [];
  check(typeof env.NATS_URL === 'string' && /^tls:\/\//.test(env.NATS_URL)
    && env.NATS_CA_FILE === '/run/pinvou/trust/ca.crt'
    && env.NATS_NKEY_SEED_FILE === '/run/pinvou/nats/nkey.seed', 'broker_configuration');
  for (const [suffix, file] of artifacts) {
    check(env[`${prefix}${suffix}_FILE`] === file, 'file_path');
    check(/^[a-f0-9]{64}$/.test(env[`${prefix}${suffix}_SHA256`] || ''), 'digest_value');
    const target = file.slice(0, file.lastIndexOf('/')), filename = file.slice(file.lastIndexOf('/') + 1);
    const matches = mounted.filter(v => v.target === target);
    check(matches.length === 1 && matches[0].type === 'bind' && matches[0].read_only === true
      && matches[0].bind?.create_host_path === false, 'public_bind');
    const source = physicalSourcePath(matches[0].source);
    check(lstatSync(source).isDirectory(), 'public_directory');
    check(!directories.some(other => overlaps(source, other)), 'path_alias');
    directories.push(source);
    for (const other of mounted) if (other !== matches[0]) check(!overlaps(other.target, target), 'path_alias');
    if (suffix === 'PUBLIC_KEYS') {
      const names = readdirSync(source);
      check(names.length === 1 && names[0] === filename, 'identity_directory');
    }
    const path = physicalSourcePath(`${source}/${filename}`), info = lstatSync(path);
    check(info.isFile() && info.size > 0 && info.size <= (32 << 10)
      && (process.platform === 'win32' || (info.mode & 0o022) === 0), 'public_file');
    check(createHash('sha256').update(readFileSync(path)).digest('hex') === env[`${prefix}${suffix}_SHA256`], 'digest_mismatch');
  }
  for (const other of Object.values(config.services || {})) for (const mount of other.volumes || []) {
    if (!/(?:^|\/)(?:database|bootstrap|kms|nats|provisioner|handoff|workload|projected)(?:\/|$)/.test(mount.target || '')) continue;
    const source = physicalSourcePath(mount.source);
    check(directories.every(directory => !overlaps(source, directory) && !overlaps(directory, source)), 'private_directory_alias');
  }
}
