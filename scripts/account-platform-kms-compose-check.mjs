// Pure structural checks of normalized Linux Compose output; never reads secrets or connects.
import path from 'node:path';

const consumers = ['token-gateway', 'token-credential-portal'];
const tokenTarget = '/var/run/secrets/pinvou/kms';
const caTarget = '/var/run/secrets/pinvou/kms-ca';
function check(ok, code) { if (!ok) throw new Error(`model_token_deployment_kms_${code}`); }
const overlaps = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`) || a === '/' || b === '/';
function canonicalDirectory(value) {
  check(typeof value === 'string' && value.startsWith('/') && value !== '/'
    && !value.includes('\\') && !/[\x00-\x20\x7f]/.test(value)
    && path.posix.normalize(value) === value && !value.endsWith('/'), 'directory_invalid');
  return value;
}
function endpoint(raw, native) {
  check(typeof raw === 'string', 'endpoint_invalid');
  let url;
  try { url = new URL(raw); } catch { check(false, 'endpoint_invalid'); }
  check(url.protocol === 'https:' && !url.username && !url.password && !url.search && !url.hash
    && (!native || raw === url.origin || raw === `${url.origin}/`), 'endpoint_invalid');
  return url.href;
}

export function checkComposeKMS(config) {
  const services = config.services;
  const values = consumers.map(name => services[name]?.environment || {});
  const provider = values[0].TOKEN_SECRET_PROVIDER;
  check(['remote', 'vault-transit'].includes(provider)
    && values.every(value => value.TOKEN_SECRET_PROVIDER === provider), 'provider_mismatch');
  const native = provider === 'vault-transit';
  const origins = values.map(value => endpoint(value.TOKEN_SECRET_PROVIDER_URL, native));
  check(origins[0] === origins[1] && typeof values[0].TOKEN_SECRET_KEY_ID === 'string' && values[0].TOKEN_SECRET_KEY_ID
    && values[0].TOKEN_SECRET_KEY_ID === values[1].TOKEN_SECRET_KEY_ID, 'key_or_endpoint_mismatch');
  for (const value of values) check(!value.TOKEN_LOCAL_KEK_BASE64, 'local_fallback');
  if (!native) {
    check(values.every(value => typeof value.TOKEN_SECRET_PROVIDER_TOKEN === 'string'
      && value.TOKEN_SECRET_PROVIDER_TOKEN.trim() && !value.TOKEN_SECRET_PROVIDER_TOKEN_FILE
      && !value.TOKEN_SECRET_PROVIDER_CA_FILE), 'remote_credentials_missing');
    check(values[0].TOKEN_SECRET_PROVIDER_TOKEN !== values[1].TOKEN_SECRET_PROVIDER_TOKEN, 'shared_identity');
    return;
  }
  check(/^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(values[0].TOKEN_SECRET_KEY_ID), 'key_invalid');
  const network = config.networks?.['kms-transit'];
  check(network?.external === true && typeof network.name === 'string' && network.name.trim(), 'network_missing');
  const bindings = consumers.map((name, index) => {
    const service = services[name]; const value = values[index];
    check(service.user === '65532:65532' && service.read_only === true && !service.privileged
      && !service.network_mode && !service.pid && !service.volumes_from?.length, 'runtime_isolation');
    check(!value.TOKEN_SECRET_PROVIDER_TOKEN && value.TOKEN_SECRET_PROVIDER_TOKEN_FILE === `${tokenTarget}/token`
      && value.TOKEN_SECRET_PROVIDER_CA_FILE === `${caTarget}/ca.crt`, 'native_identity_invalid');
    check(Object.hasOwn(service.networks || {}, 'kms-transit'), 'network_missing');
    return [tokenTarget, caTarget].map(target => {
      const matches = (service.volumes || []).filter(volume => volume.target === target);
      check(matches.length === 1, 'mount_missing');
      const volume = matches[0];
      check(volume.type === 'bind' && volume.read_only === true && volume.bind?.create_host_path === false, 'mount_unsafe');
      return canonicalDirectory(volume.source);
    });
  });
  const [gateway, credential] = bindings;
  check(!overlaps(gateway[0], credential[0]) && gateway[1] === credential[1]
    && !overlaps(gateway[0], gateway[1]) && !overlaps(credential[0], gateway[1]), 'shared_identity');
  const protectedSources = [gateway[0], credential[0], gateway[1]];
  check(Object.entries(config.networks).every(([name, value]) => name === 'kms-transit' || value.name !== network.name), 'network_alias');
  for (const [name, service] of Object.entries(services)) {
    check(!service.volumes_from?.length, 'identity_leaked');
    const index = consumers.indexOf(name);
    if (index < 0) {
      check(!Object.hasOwn(service.networks || {}, 'kms-transit'), 'network_leaked');
      check(!service.network_mode?.startsWith('service:') || !consumers.includes(service.network_mode.slice(8)), 'network_leaked');
      check(!Object.keys(service.environment || {}).some(key => key.startsWith('TOKEN_SECRET_PROVIDER') && service.environment[key]), 'identity_leaked');
    }
    for (const volume of service.volumes || []) {
      check(typeof volume === 'object', 'mount_not_normalized');
      const slot = index < 0 ? -1 : [tokenTarget, caTarget].indexOf(volume.target);
      if (index >= 0 && slot < 0) check(typeof volume.target === 'string'
        && ![tokenTarget, caTarget].some(target => overlaps(path.posix.normalize(volume.target), target)), 'mount_shadowed');
      if (volume.type !== 'bind') continue;
      const source = canonicalDirectory(volume.source);
      if (slot >= 0 && source === bindings[index][slot]) continue;
      check(!protectedSources.some(protectedSource => overlaps(source, protectedSource)), 'identity_leaked');
    }
  }
}
