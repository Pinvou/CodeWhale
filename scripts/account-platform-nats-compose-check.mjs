// Pure checks of normalized Compose configuration. No file, credential or network access.
import path from 'node:path';

const consumers = ['billing-service', 'account-event-relay', 'billing-event-relay', 'token-event-relay', 'token-command-executor'];
const legacyConsumers = ['account-service', 'token-gateway', ...consumers];
const seedTarget = '/var/run/secrets/pinvou/nats';
const caTarget = '/var/run/secrets/pinvou/nats-ca';
const toolName = 'event-resources';
const check = (value, code) => { if (!value) throw new Error(`model_token_deployment_nats_${code}`); };
const overlaps = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`) || a === '/' || b === '/';

function directory(value) {
  check(typeof value === 'string' && value.startsWith('/') && value !== '/' && !value.endsWith('/')
    && !/[\\\x00-\x20\x7f]/.test(value) && path.posix.normalize(value) === value, 'directory_invalid');
  return value;
}

function cluster(value) {
  check(typeof value === 'string' && !/[\s,]/.test(value), 'cluster_invalid');
  let parsed;
  try { parsed = new URL(value); } catch { check(false, 'cluster_invalid'); }
  check(parsed.protocol === 'tls:' && parsed.hostname && parsed.port && !parsed.username && !parsed.password
    && !parsed.pathname && !parsed.search && !parsed.hash && parsed.href === value, 'cluster_invalid');
  return value;
}

function binding(service, target) {
  const matches = (service.volumes || []).filter(item => item.target === target);
  check(matches.length === 1, 'mount_missing');
  const item = matches[0];
  check(item.type === 'bind' && item.read_only === true && item.bind?.create_host_path === false, 'mount_unsafe');
  return directory(item.source);
}

function volumeSource(config, volume) {
  if (volume.type === 'bind') return directory(volume.source);
  if (volume.type === 'tmpfs') {
    check(volume.source == null, 'volume_source_unproven');
    return null;
  }
  check(volume.type === 'volume' && typeof volume.source === 'string', 'volume_source_unproven');
  const declared = config.volumes?.[volume.source];
  const options = declared?.driver_opts;
  // Named/external volumes with opaque prior contents cannot establish a file-source boundary.
  check(declared && !declared.external && declared.driver === 'local' && options?.type === 'none'
    && ['bind', 'bind,ro', 'bind,rw'].includes(options.o)
    && Object.keys(options).length === 3 && Object.hasOwn(options, 'device'), 'volume_source_unproven');
  return directory(options.device);
}

function fileResource(config, kind, resource) {
  const item = typeof resource === 'string' ? { source: resource } : resource;
  check(item && typeof item.source === 'string', 'file_source_unproven');
  const declared = config[kind]?.[item.source];
  check(declared && !declared.external && declared.file && !declared.environment && declared.content == null
    && !declared.template_driver, 'file_source_unproven');
  const prefix = kind === 'secrets' ? '/run/secrets/' : '/';
  const rawTarget = item.target || `${prefix}${item.source}`;
  check(typeof rawTarget === 'string', 'file_source_unproven');
  return { source: directory(declared.file), target: rawTarget.startsWith('/') ? rawTarget : `${prefix}${rawTarget}` };
}

function checkOperator(services) {
  const tool = services[toolName];
  if (!tool) return;
  check(Array.isArray(tool.profiles) && tool.profiles.length === 1 && tool.profiles[0] === 'event-operations'
    && tool.restart === 'no' && Array.isArray(tool.entrypoint) && tool.entrypoint.length === 1 && tool.entrypoint[0] === '/service'
    && Array.isArray(tool.command) && tool.command.length === 0 && tool.healthcheck?.disable === true
    && Object.keys(tool.healthcheck).length === 1 && !tool.post_start?.length && !tool.pre_stop?.length, 'operator_auto_execution');
  for (const service of Object.values(services)) {
    const dependencies = Array.isArray(service.depends_on) ? service.depends_on : Object.keys(service.depends_on || {});
    check(!dependencies.includes(toolName)
      && ![service.network_mode, service.pid, service.ipc].includes(`service:${toolName}`)
      && !(service.links || []).some(value => value.split(':')[0] === toolName)
      && !(service.volumes_from || []).some(value => value === toolName || value.startsWith(`${toolName}:`)), 'operator_dependency');
  }
}

export function checkComposeNATS(config) {
  const services = config.services || {};
  const native = Object.values(services).some(service => Object.hasOwn(service.environment || {}, 'NATS_NKEY_SEED_FILE'));
  if (!native) {
    const endpoint = services['token-command-executor']?.environment?.NATS_URL;
    if (!(/^tls:\/\//.test(endpoint) && legacyConsumers.every(name => services[name]?.environment?.NATS_URL === endpoint
      && services[name]?.environment?.NATS_CREDENTIALS_FILE))) throw new Error('model_token_deployment_event_cluster_or_credentials_mismatch');
    return;
  }
  checkOperator(services);
  const names = services[toolName] ? [...consumers, toolName] : consumers;
  const endpoint = cluster(services['token-command-executor']?.environment?.NATS_URL);
  const bindings = names.map(name => {
    const service = services[name]; const environment = service?.environment || {};
    check(service && environment.NATS_URL === endpoint && !environment.NATS_CREDENTIALS_FILE
      && environment.NATS_NKEY_SEED_FILE === `${seedTarget}/nkey.seed`
      && environment.NATS_CA_FILE === `${caTarget}/ca.crt`, 'native_identity_invalid');
    check(service.user === '65532:65532' && service.read_only === true && !service.privileged
      && !service.volumes_from?.length && !service.network_mode && !service.pid && !service.ipc, 'runtime_isolation');
    return { name, seed: binding(service, seedTarget), ca: binding(service, caTarget) };
  });
  for (const [index, item] of bindings.entries()) {
    check(!bindings.some((other, at) => at !== index && overlaps(item.seed, other.seed))
      && bindings.every(other => !overlaps(item.seed, other.ca))
      && item.ca === bindings[0].ca, 'shared_identity');
  }
  for (const [name, service] of Object.entries(services)) {
    const own = bindings.find(item => item.name === name);
    const environment = service.environment || {};
    if (!own) check(!environment.NATS_URL && !environment.NATS_CA_FILE && !environment.NATS_CREDENTIALS_FILE
      && !Object.hasOwn(environment, 'NATS_NKEY_SEED_FILE'), 'identity_leaked');
    check(!service.volumes_from?.length, 'identity_leaked');
    for (const namespace of [service.network_mode, service.pid, service.ipc]) {
      check(!names.some(consumer => namespace === `service:${consumer}`), 'identity_leaked');
    }
    const mounted = (service.volumes || []).map(volume => {
      check(volume && typeof volume === 'object', 'mount_not_normalized');
      return { source: volumeSource(config, volume), target: volume.target, directBind: volume.type === 'bind' };
    });
    check(service.tmpfs == null || Array.isArray(service.tmpfs), 'mount_not_normalized');
    for (const value of service.tmpfs || []) {
      check(typeof value === 'string', 'mount_not_normalized');
      mounted.push({ source: null, target: value.split(':', 1)[0] });
    }
    for (const kind of ['secrets', 'configs']) {
      for (const resource of service[kind] || []) mounted.push(fileResource(config, kind, resource));
    }
    for (const mount of mounted) {
      const authorized = own && mount.directBind && (mount.target === seedTarget && mount.source === own.seed
        || mount.target === caTarget && mount.source === own.ca);
      if (authorized) continue;
      if (own) {
        check(typeof mount.target === 'string' && ![seedTarget, caTarget].some(target => overlaps(path.posix.normalize(mount.target), target)), 'mount_shadowed');
      }
      if (mount.source != null) check(!bindings.some(item => overlaps(mount.source, item.seed)), 'identity_leaked');
    }
  }
}
