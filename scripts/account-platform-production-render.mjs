import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, realpathSync } from 'node:fs';
import { resolve, isAbsolute, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { spawnSync } from 'node:child_process';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';

const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const YAML = require('yaml');
export const repositoryRoot = fileURLToPath(new URL('../', import.meta.url));
export const requireGate = (condition, code) => {
  if (!condition) throw new Error(`production_render_${code}`);
};
export function stableJSON(value) {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(',')}]`;
  if (value && typeof value === 'object') return `{${Object.keys(value).sort().map(k => `${JSON.stringify(k)}:${stableJSON(value[k])}`).join(',')}}`;
  return JSON.stringify(value);
}
export const digest = value => createHash('sha256').update(stableJSON(value)).digest('hex');
export const bytesDigest = value => createHash('sha256').update(value).digest('hex');
export function readGateFile(path, limit = 4 * 1024 * 1024) {
  const stat = lstatSync(path);
  requireGate(stat.isFile() && !stat.isSymbolicLink() && stat.size > 0 && stat.size <= limit, 'input_file');
  const bytes = readFileSync(path);
  requireGate(bytes.length <= limit, 'input_size');
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
}
export function readGateJSON(path) { return parseReleaseJSON(readGateFile(path, 1024 * 1024)); }
function hostSourcePath(source) {
  let path = source;
  if (process.platform === 'win32' && /^\/[a-zA-Z]\//.test(path)) path = `${path[1]}:${path.slice(2)}`;
  requireGate(isAbsolute(path), 'source_path');
  for (let current = resolve(path);; current = dirname(current)) {
    requireGate(!lstatSync(current).isSymbolicLink(), 'source_symlink');
    if (dirname(current) === current) break;
  }
  return path;
}
export function physicalSourcePath(source) {
  const physical = realpathSync(hostSourcePath(source)).replaceAll('\\', '/');
  return process.platform === 'win32' ? physical.toLowerCase() : physical;
}
export function readSourceBytes(source) {
  const path = hostSourcePath(source), stat = lstatSync(path);
  requireGate(stat.isFile() && stat.size > 0 && stat.size <= 1024 * 1024, 'input_file');
  const bytes = readFileSync(path);
  requireGate(bytes.length <= 1024 * 1024, 'input_size');
  return bytes;
}
export function readSourceFile(source) { return new TextDecoder('utf-8', { fatal: true }).decode(readSourceBytes(source)); }
export function mountedSource(workload, path) {
  const mounts = workload.mounts.filter(v => path === v.target || path.startsWith(`${v.target}/`));
  requireGate(mounts.length === 1, 'credential_mount');
  const mount = mounts[0];
  requireGate(mount.type === 'bind' && mount.read_only === true && mount.bind?.create_host_path === false, 'credential_mount');
  return `${mount.source}${path.slice(mount.target.length)}`;
}
export function parseHelmRender(text) {
  requireGate(Buffer.byteLength(text) <= 4 * 1024 * 1024, 'input_size');
  const documents = YAML.parseAllDocuments(text, { uniqueKeys: true, maxAliasCount: 0 });
  requireGate(documents.length > 0 && documents.every(d => d.errors.length === 0), 'yaml_invalid');
  const resources = documents.map(d => d.toJS({ maxAliasCount: 0 })).filter(d => d !== null);
  requireGate(resources.every(d => d && typeof d === 'object' && !Array.isArray(d)), 'resource_invalid');
  const keys = resources.map(resourceKey);
  requireGate(new Set(keys).size === keys.length, 'resource_duplicate');
  return resources.sort((a, b) => resourceKey(a).localeCompare(resourceKey(b)));
}
export const resourceKey = resource => `${resource.apiVersion}/${resource.kind}/${resource.metadata?.namespace || ''}/${resource.metadata?.name || ''}`;

// These are current modular compositions, not the removed twelve-service stack.
const base = ['personal', 'personal-workload'];
const model = [...base, 'finance', 'reconciliation', 'models'];
export const composeCompositions = Object.freeze({ personal: base, models: model });
const extraOverlays = new Set(['finance', 'reconciliation', 'signup', 'organizations', 'invocation', 'closure', 'approval-recovery',
  'receivable-write-off', 'payment-financial-repair', 'ledger-repair', 'entitlement-correction', 'audit-export']);

function runRenderer(binary, args, root, environment) {
  const rendered = spawnSync(binary, args, { cwd: root, env: environment, encoding: 'buffer',
    timeout: 30000, maxBuffer: 4 * 1024 * 1024, windowsHide: true, shell: false });
  // Renderer diagnostics and stdout can contain private env-file contents.
  requireGate(!rendered.error && rendered.status === 0, 'renderer_failed');
  return new TextDecoder('utf-8', { fatal: true }).decode(rendered.stdout);
}
export function renderCurrentProduction(recipe, options = {}) {
  requireGate(recipe && ['compose', 'helm'].includes(recipe.kind), 'recipe');
  const root = options.root || repositoryRoot;
  const binary = options.binary;
  requireGate(typeof binary === 'string' && isAbsolute(binary), 'renderer_path');
  requireGate(/^[a-f0-9]{64}$/.test(options.binarySHA256 || '')
    && bytesDigest(readFileSync(binary)) === options.binarySHA256, 'renderer_digest');
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP', 'DOCKER_CONFIG'].includes(key.toUpperCase())));
  if (recipe.kind === 'compose') {
    requireGate(Object.hasOwn(composeCompositions, recipe.profile), 'composition');
    const extra = recipe.overlays || [];
    requireGate(Array.isArray(extra) && new Set(extra).size === extra.length && extra.every(v => extraOverlays.has(v)), 'composition');
    requireGate(new Set([...composeCompositions[recipe.profile], ...extra]).size === composeCompositions[recipe.profile].length + extra.length, 'composition');
    requireGate(recipe.profile === 'models' || !extra.some(v => ['organizations', 'invocation'].includes(v)), 'composition_profile');
    requireGate(typeof recipe.envFile === 'string' && isAbsolute(recipe.envFile), 'env_file');
    readGateFile(recipe.envFile, 1024 * 1024);
    const version = runRenderer(binary, ['version', '--short'], root, environment).trim();
    requireGate(version === '5.4.0' || version === 'v5.4.0', 'renderer_version');
    const args = ['--env-file', recipe.envFile];
    for (const name of [...composeCompositions[recipe.profile], ...extra]) args.push('-f',
      resolve(root, `account-platform/deploy/production/compose.${name}.yaml`));
    // Config only: include operator declarations in the closed inventory, never run them.
    args.push('--profile', '*', 'config', '--format', 'json');
    const env = { ...environment, COMPOSE_CONVERT_WINDOWS_PATHS: '1' };
    const template = parseReleaseJSON(runRenderer(binary, [...args, '--no-env-resolution'], root, env));
    return { kind: 'compose', recipe, template,
      config: parseReleaseJSON(runRenderer(binary, args, root, env)) };
  }
  requireGate(/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(recipe.namespace || '')
    && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(recipe.release || ''), 'helm_scope');
  requireGate(typeof recipe.valuesFile === 'string' && isAbsolute(recipe.valuesFile), 'values_file');
  readGateFile(recipe.valuesFile, 1024 * 1024);
  requireGate(typeof recipe.secretSnapshotFile === 'string' && isAbsolute(recipe.secretSnapshotFile), 'secret_snapshot');
  const secrets = readGateJSON(recipe.secretSnapshotFile);
  requireGate(runRenderer(binary, ['version', '--short'], root, environment).trim().startsWith('v3.16.4+'), 'renderer_version');
  const args = ['template', recipe.release, resolve(root, 'account-platform/deploy/helm/account-platform'),
    '--namespace', recipe.namespace, '--values', resolve(root, 'account-platform/deploy/helm/account-platform/values-production-new.yaml'),
    '--values', recipe.valuesFile];
  const config = parseHelmRender(runRenderer(binary, args, root, environment));
  return { kind: 'helm', recipe, template: structuredClone(config), secrets, config };
}

function secretValue(ref, secrets, namespace) {
  requireGate(ref && typeof ref.name === 'string' && typeof ref.key === 'string', 'secret_reference');
  const key = `${namespace}/${ref.name}/${ref.key}`;
  if (ref.optional === true && !Object.hasOwn(secrets, key)) return '';
  requireGate(Object.hasOwn(secrets, key) && typeof secrets[key] === 'string', 'secret_missing');
  return secrets[key];
}
export function workloads(render, secrets = {}) {
  if (render.kind === 'compose') {
    requireGate(render.config?.services && !Array.isArray(render.config.services), 'services');
    return Object.entries(render.config.services).map(([name, service]) => ({ id: name, name, service,
      image: service.image, environment: service.environment || {}, mounts: service.volumes || [], kind: 'compose' }));
  }
  requireGate(Array.isArray(render.config), 'resources');
  const result = [];
  for (const resource of render.config) {
    const pod = ['Deployment', 'StatefulSet', 'DaemonSet', 'Job'].includes(resource.kind) ? resource.spec?.template?.spec
      : resource.kind === 'CronJob' ? resource.spec?.jobTemplate?.spec?.template?.spec
        : resource.kind === 'Pod' ? resource.spec : null;
    if (!pod) continue;
    for (const [type, containers] of [['init', pod.initContainers || []], ['container', pod.containers || []]]) {
      requireGate(Array.isArray(containers), 'containers');
      const names = containers.map(c => c.name);
      requireGate(new Set(names).size === names.length, 'container_duplicate');
      for (const container of containers) {
        requireGate(!container.envFrom && Array.isArray(container.env || []), 'environment_scope');
        const environment = {}, refs = {};
        for (const env of container.env || []) {
          requireGate(typeof env.name === 'string' && !Object.hasOwn(environment, env.name), 'environment_duplicate');
          if (Object.hasOwn(env, 'value')) {
            requireGate(!env.valueFrom && typeof env.value === 'string', 'environment_value');
            environment[env.name] = env.value;
          } else if (env.valueFrom?.secretKeyRef) {
            requireGate(Object.keys(env.valueFrom).length === 1, 'environment_reference');
            refs[env.name] = env.valueFrom.secretKeyRef;
            environment[env.name] = secretValue(env.valueFrom.secretKeyRef, secrets, resource.metadata?.namespace || render.recipe.namespace);
          } else if (env.valueFrom?.fieldRef) {
            requireGate(['metadata.name', 'metadata.namespace', 'status.podIP'].includes(env.valueFrom.fieldRef.fieldPath), 'environment_reference');
            environment[env.name] = `field:${env.valueFrom.fieldRef.fieldPath}`;
          } else requireGate(false, 'environment_reference');
        }
        result.push({ id: `${resourceKey(resource)}/${type}/${container.name}`, name: resource.metadata.name,
          resource, pod, service: container, image: container.image, environment, refs,
          mounts: container.volumeMounts || [], kind: 'helm', type });
      }
    }
  }
  return result;
}

export function approvedRender(render, inventory) {
  requireGate(inventory?.version === 1 && inventory.kind === render.kind, 'inventory');
  requireGate(inventory.images && typeof inventory.images === 'object', 'inventory');
  requireGate(render.template, 'template_missing');
  if (render.kind === 'compose') {
    const actual = structuredClone(render.config), expected = structuredClone(render.template);
    for (const config of [actual, expected]) for (const service of Object.values(config.services || {})) {
      delete service.environment; delete service.env_file;
    }
    requireGate(stableJSON(actual) === stableJSON(expected), 'template_changed');
    for (const [name, service] of Object.entries(render.template.services || {})) {
      for (const [key, value] of Object.entries(service.environment || {}))
        requireGate(render.config.services[name].environment?.[key] === value, 'literal_changed');
    }
  } else requireGate(stableJSON(render.config) === stableJSON(render.template), 'template_changed');
  const actual = workloads(render, render.secrets || {});
  requireGate(stableJSON(actual.map(w => w.id).sort()) === stableJSON(Object.keys(inventory.images).sort()), 'workload_inventory');
  for (const workload of actual) {
    requireGate(inventory.images[workload.id] === workload.image
      && /^(?:sha256:[a-f0-9]{64}|[^\s@]+@sha256:[a-f0-9]{64})$/.test(workload.image), 'image');
  }
  return actual;
}

export function gateCLI(check, argv = process.argv.slice(2)) {
  try {
    requireGate(argv.length === 6 && argv[0] === '--recipe' && argv[2] === '--inventory' && argv[4] === '--renderer', 'usage');
    const recipe = readGateJSON(argv[1]), inventory = readGateJSON(argv[3]);
    const render = renderCurrentProduction(recipe, { binary: resolve(argv[5]), binarySHA256: inventory.rendererSHA256 });
    process.stdout.write(JSON.stringify(check(render, inventory)) + '\n');
  } catch (error) {
    process.stderr.write((/^production_render_[a-z_]+$/.test(error?.message) ? error.message : 'production_render_input_rejected') + '\n');
    process.exitCode = 1;
  }
}
