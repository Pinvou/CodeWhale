import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = name => readFileSync(path.join(root, 'deploy/production', name), 'utf8');

test('production NATS container only receives its leaf, public ACL and isolated store', () => {
  const config = YAML.parse(read('compose.nats.yaml'));
  assert.deepEqual(Object.keys(config.services), ['nats']);
  assert.equal(config.name, YAML.parse(read('compose.dependencies.yaml')).name);
  const server = config.services.nats;
  assert.equal(server.user, '65532:65532');
  assert.equal(server.read_only, true);
  assert.deepEqual(server.cap_drop, ['ALL']);
  assert.equal(server.ports, undefined);
  assert.equal(server.environment, undefined);
  assert.equal(server.pull_policy, 'never');
  assert.equal(config.networks.data.internal, true);
  assert.equal(server.volumes.length, 4);
  for (const volume of server.volumes) {
    assert.equal(volume.type, 'bind');
    assert.equal(volume.bind.create_host_path, false);
    assert.equal(volume.read_only ?? false, volume.target !== '/data');
    assert.doesNotMatch(volume.source, /seed|custody|kms|ca-private|identities.age/);
  }
  assert.match(server.healthcheck.test.join(' '), /127.0.0.1:8222/);
});

test('NATS configuration closes public monitoring and requires TLS and external fixed ACL', () => {
  const conf = read('nats.conf');
  assert.match(conf, /http: "127.0.0.1:8222"/);
  assert.match(conf, /min_version: "1.2"/);
  assert.match(conf, /^include authorization.conf$/m);
  assert.match(conf, /store_dir: "\/data"/);
  assert.match(conf, /max_file_store: 2GB/);
  assert.match(conf, /debug: false/);
  assert.match(conf, /trace: false/);
  assert.doesNotMatch(conf, /no_auth_user|password:|token:|allow_non_tls|verify_and_map/);
});
