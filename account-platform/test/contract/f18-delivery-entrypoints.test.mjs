import assert from 'node:assert/strict';
import { readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('production PostgreSQL dependency mounts resolve to delivered files and keep fresh and existing data separated', async () => {
  const document = YAML.parse(await read('deploy/production/compose.dependencies.yaml'));
  const postgres = document.services.postgres;
  assert.equal(postgres.user, '999:999');
  assert.equal(postgres.read_only, true);
  const required = ['postgres.conf', 'pg_hba.conf', 'postgres-init.sh', 'postgres-entrypoint.sh', 'postgres-common.sh', 'postgres-health.sh'];
  for (const name of required) {
    const binding = postgres.volumes.find(volume => volume.source === `./${name}`);
    assert.ok(binding, `${name} is in the actual production mount graph`);
    assert.equal(binding.read_only, true);
    assert.equal(binding.bind.create_host_path, false);
    assert.ok((await stat(path.join(root, 'deploy/production', name))).isFile());
  }
  assert.deepEqual(postgres.entrypoint, ['/bin/sh', '/opt/pinvou/production/postgres-entrypoint.sh']);
  assert.ok(postgres.healthcheck.test.includes('/opt/pinvou/production/postgres-health.sh'));
  const entry = await read('deploy/production/postgres-entrypoint.sh');
  const existing = entry.match(/if \[ -e "\$PGDATA\/PG_VERSION" \]; then([\s\S]*?)\nelse/);
  assert.ok(existing);
  assert.match(existing[1], /db_ready/);
  assert.doesNotMatch(existing[1], /initdb|init-databases|postgres-init|rm |DROP/i);
  assert.match(entry, /find "\$PGDATA" -mindepth 1 -maxdepth 1 -print -quit/);
  const init = await read('deploy/production/postgres-init.sh');
  const bootstrap = init.indexOf('. /opt/pinvou/init-databases.sh');
  const encodingGate = init.indexOf('db_utf8 "$POSTGRES_USER"');
  const marker = init.indexOf('set -C; printf');
  assert.ok(bootstrap >= 0 && encodingGate > bootstrap && marker > encodingGate);
  const hba = await read('deploy/production/pg_hba.conf');
  assert.match(hba, /^hostnossl all all all reject$/m);
  assert.match(hba, /^host all all all reject$/m);
  assert.doesNotMatch(hba, /\btrust\b/);
  const recovery = hba.split('\n').filter(line => /^hostssl\s+account_platform_account\s+/.test(line));
  assert.equal(recovery.length, 1);
  assert.ok(recovery[0].split(/\s+/)[2].split(',').includes('account_approval_recovery'));
  assert.match(recovery[0], / all scram-sha-256$/);
});

test('all delivered profile callbacks match the installed public Billing ingress rather than an internal route', async () => {
  const ingress = await read('deploy/helm/account-platform/templates/ingress.yaml');
  assert.match(ingress, /host: \{\{ \.Values\.ingress\.portalHost \| quote \}\}/);
  assert.match(ingress, /path: \/api\/v1\/payment-callbacks\s+pathType: Exact/);
  assert.match(ingress, /path: \/api\/v1\/payment-callback\/token\s+pathType: Exact/);
  for (const profile of ['values.yaml', 'values-staging.yaml', 'values-production-new.yaml']) {
    const values = YAML.parse(await read(`deploy/helm/account-platform/${profile}`));
    const callback = new URL(values.public.paymentCallbackUrl);
    assert.equal(callback.protocol, 'https:');
    assert.equal(callback.hostname, values.ingress.portalHost);
    assert.equal(callback.pathname, '/api/v1/payment-callbacks');
    assert.equal(callback.username, '');
    assert.equal(callback.password, '');
    assert.equal(callback.search, '');
    assert.equal(callback.hash, '');
  }
});

test('retained loopback edge offers only the canonical TLS origin and preserves existing private-route and admin deadline guards', async () => {
  const template = await read('deploy/nginx/account-platform.conf.template');
  const listeners = [...template.matchAll(/^\s*listen\s+([^;]+);/gm)].map(match => match[1]);
  assert.equal(listeners.length, 2);
  assert.ok(listeners.every(listener => listener.includes('__PUBLIC_TLS_PORT__') && /\bssl\b/.test(listener)));
  assert.doesNotMatch(template, /^\s*listen\s+(?:\[::\]:)?80(?:\s|;)/m);
  assert.match(template, /ssl_certificate \/run\/pinvou\/public-tls\/server\.crt;/);
  assert.match(template, /ssl_certificate_key \/run\/pinvou\/public-tls\/server\.key;/);
  assert.match(template, /ssl_protocols TLSv1\.2 TLSv1\.3;/);
  assert.match(template, /access_log off;/);
  assert.match(template, /location \^~ \/account\/internal\/ \{\s*return 404;/);
  const locations = [...template.matchAll(/location[^\n]*\{([\s\S]*?)\n    \}/g)].map(match => match[1]);
  const proxies = locations.filter(location => /proxy_pass/.test(location));
  assert.ok(proxies.length > 0);
  for (const proxy of proxies) {
    assert.match(proxy, /proxy_set_header Host "__PUBLIC_HOST__:__PUBLIC_TLS_PORT__";/);
    assert.match(proxy, /proxy_set_header X-Forwarded-Port __PUBLIC_TLS_PORT__;/);
    assert.match(proxy, /proxy_set_header X-Forwarded-Proto https;/);
  }
  assert.match(template, /location \^~ \/api\/admin\/token\//);
  assert.match(template, /proxy_set_header X-Pinvou-Absolute-Deadline "";/);
  assert.match(template, /proxy_set_header X-Pinvou-Deadline-Class "";/);
  assert.match(template, /proxy_read_timeout 14s;/);
});
