import path from 'node:path';

const reject = () => { throw new Error('DEV_NATS_EXISTING_STORAGE_UNPROVEN: preserve existing NATS data before changing its mount; see account-platform/deploy/runbooks/development-nats.md'); };
const samePath = (left, right) => typeof left === 'string' && typeof right === 'string' && path.isAbsolute(left) && path.isAbsolute(right) && path.normalize(left) === path.normalize(right);

// Read-only preflight. In particular, do not treat an old named volume as an
// empty/new bind directory or infer identity from its name, size or contents.
export function guardExistingDevelopmentNATSStorage({ args, environment, capture, dockerHost }) {
  try {
    if (dockerHost) reject();
    const endpoint = JSON.parse(capture('docker', ['context', 'inspect', '--format', '{{json .Endpoints.docker.Host}}']));
    if (typeof endpoint !== 'string' || !(endpoint.startsWith('unix:///') || endpoint.startsWith('npipe:////./pipe/'))) reject();
    const configuration = JSON.parse(capture('docker', [...args, 'config', '--format', 'json']));
    if (!configuration || !/^[a-z0-9][a-z0-9_-]*$/.test(configuration.name || '')) reject();
    const configured = configuration.services?.nats?.volumes?.filter(mount => mount.target === '/data');
    if (configured?.length !== 1 || configured[0].type !== 'bind' || !samePath(configured[0].source, environment.DEV_NATS_DATA_DIRECTORY)) reject();
    const oldVolumes = capture('docker', ['volume', 'ls', '-q', '--filter', `label=com.docker.compose.project=${configuration.name}`, '--filter', 'label=com.docker.compose.volume=nats-data']);
    if (oldVolumes.trim() !== '') reject();
    const oldNamed = capture('docker', ['volume', 'ls', '-q', '--filter', `name=${configuration.name}_nats-data`]);
    if (oldNamed.trim().split(/\s+/).includes(`${configuration.name}_nats-data`)) reject();
    const ids = capture('docker', [...args, 'ps', '-aq', 'nats']).trim().split(/\s+/).filter(Boolean);
    if (ids.length > 1 || ids.some(id => !/^[a-f0-9]{12,64}$/.test(id))) reject();
    for (const id of ids) {
      const mounts = JSON.parse(capture('docker', ['inspect', '--format', '{{json .Mounts}}', id]));
      if (!Array.isArray(mounts)) reject();
      const actual = mounts.filter(mount => mount.Destination === '/data');
      if (actual.length !== 1 || actual[0].Type !== 'bind' || !samePath(actual[0].Source, configured[0].source)) reject();
    }
  } catch { reject(); }
}
