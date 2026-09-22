import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = name => readFileSync(path.join(root, 'deploy/production', name), 'utf8');

test('Redis instances isolate data, TLS and ACL without host ports or password environment', () => {
  const config = YAML.parse(read('compose.redis.yaml'), { merge: true });
  assert.deepEqual(Object.keys(config.services), ['redis', 'newapi-redis']);
  assert.equal(config.networks.data.internal, true);
  for (const server of Object.values(config.services)) {
    assert.equal(server.user, '65532:65532');
    assert.equal(server.read_only, true);
    assert.deepEqual(server.cap_drop, ['ALL']);
    assert.equal(server.ports, undefined);
    assert.equal(server.environment, undefined);
    assert.equal(server.pull_policy, 'never');
    assert.equal(server.mem_limit, '384m');
    assert.equal(server.volumes.length, 5);
    for (const v of server.volumes) {
      assert.equal(v.type, 'bind');
      assert.equal(v.bind.create_host_path, false);
      assert.equal(v.read_only ?? false, v.target !== '/data');
    }
  }
  for (let i = 0; i < 3; i++) assert.notEqual(config.services.redis.volumes[i].source, config.services['newapi-redis'].volumes[i].source);
});

test('Redis ACL is closed by default and never grants shared-cache enumeration or admin categories', () => {
  const accounts = read('redis-accounts.acl.template');
  const model = read('redis-newapi.acl.template');
  for (const acl of [accounts, model]) {
    assert.match(acl, /^user default reset off$/m);
    assert.doesNotMatch(acl, /\+@|\+acl|\+config|\+flush|\+module|\+replica|\+shutdown|nopass/);
    assert.doesNotMatch(acl, /(?:^| )>\S/m);
  }
  assert.doesNotMatch(accounts, /\+scan|\+keys|~\*/);
  assert.match(accounts, /user portal .*~portal:approval-wire:\*/);
  assert.match(accounts, /user portal .*~portal:session-handoff:\*/);
  assert.match(accounts, /~portal:flow:\*/);
  assert.match(accounts, /~pinvou:auth-admission:\*/);
  assert.match(model, /\+scan/);
  assert.match(model, /\+script\|load/);
  const conf = read('redis.conf');
  for (const line of ['port 0', 'tls-port 6379', 'tls-protocols "TLSv1.2 TLSv1.3"', 'maxmemory-policy noeviction', 'appendonly yes', 'appendfsync everysec', 'aof-load-truncated no', 'databases 1']) assert.ok(conf.split(/\r?\n/).includes(line), line);
  const health = read('redis-health.sh');
  assert.match(health, /\[ "\$result" = 'NOAUTH Authentication required\.' \]/);
  assert.doesNotMatch(health, /--pass|--user|rediss?:/);
});
