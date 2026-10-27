import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { currentRenderFixture } from '../fixtures/production-render.mjs';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const services = ['account-service', 'identity-service', 'billing-service', 'token-gateway', 'token-portal'];
const admission = environment => environment.HTTP_TRUSTED_PROXY_IPS;
const composeEnabled = process.env.PINVOU_TEST_KMS_COMPOSE === '1';
const helmEnabled = process.env.PINVOU_TEST_KMS_HELM === '1';

test('personal NGINX overwrites admission source at inherited server scope', () => {
  const text = readFileSync(new URL('../../deploy/nginx/personal.conf.template', import.meta.url), 'utf8');
  const locations = text.indexOf('location ');
  assert.match(text.slice(0, locations), /proxy_set_header X-Pinvou-RateLimit-Source \$remote_addr;/);
  assert.doesNotMatch(text.slice(locations), /proxy_set_header/);
  assert.doesNotMatch(text, /proxy_set_header X-Pinvou-RateLimit-Source \$http_/);
});

test('retained development Compose renders direct defaults and exact per-service overrides', { skip: !composeEnabled }, () => {
  const files = ['deploy/compose/.env.example', 'deploy/compose/model-token.env.example', 'test/fixtures/compose-render.env'];
  const args = files.flatMap(path => ['--env-file', fileURLToPath(new URL(`../../${path}`, import.meta.url))]);
  args.push('-f', fileURLToPath(new URL('../../deploy/compose/compose.yaml', import.meta.url)),
    '-f', fileURLToPath(new URL('../../deploy/compose/compose.model-token.yaml', import.meta.url)),
    '--profile', 'model-token', '--profile', 'governance-operations', '--profile', 'real-newapi', 'config', '--format', 'json');
  for (const value of ['', '192.0.2.10,2001:db8::10']) {
    const env = { ...process.env, COMPOSE_CONVERT_WINDOWS_PATHS: '1' };
    for (const name of services) env[`${name.toUpperCase().replaceAll('-', '_')}_HTTP_TRUSTED_PROXY_IPS`] = value;
    const result = spawnSync(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY, args, { cwd: root, env,
      encoding: 'utf8', windowsHide: true, timeout: 30000, maxBuffer: 4 * 1024 * 1024 });
    assert.equal(result.status, 0, 'public development Compose config failed');
    const actual = JSON.parse(result.stdout);
    for (const name of services) assert.equal(admission(actual.services[name].environment), value, name);
  }
});

for (const profile of ['personal', 'models']) test(`current ${profile} Compose pins only actual edge and Portal peers`, { skip: !composeEnabled }, () => {
  const fixture = currentRenderFixture('compose', profile);
  try {
    const actual = fixture.render.config.services;
    const edge = actual['personal-edge'].networks.backend.ipv4_address;
    const portal = actual['token-portal'].networks.backend.ipv4_address;
    for (const name of services) assert.equal(admission(actual[name].environment),
      ['identity-service', 'token-portal'].includes(name) ? edge : `${edge},${portal}`, name);
  } finally { fixture.dispose(); }
});

test('Helm renders safe defaults and exact per-service trust without changing external controllers', { skip: !helmEnabled }, () => {
  const fixture = currentRenderFixture('helm', 'models');
  const environments = () => Object.fromEntries(fixture.render.config.filter(resource => resource.kind === 'Deployment')
    .flatMap(resource => resource.spec.template.spec.containers.filter(container => services.includes(container.name))
      .map(container => [container.name, Object.fromEntries(container.env.filter(env => Object.hasOwn(env, 'value')).map(env => [env.name, env.value]))])));
  try {
    const defaults = environments();
    assert.equal(Object.keys(defaults).length, services.length);
    for (const name of services) assert.equal(admission(defaults[name]), '', name);
    const values = JSON.parse(readFileSync(fixture.recipe.valuesFile, 'utf8'));
    values.security = { httpTrustedProxyIPs: Object.fromEntries(services.map((name, index) => [name, [`192.0.2.${index + 1}`, '2001:db8::10']])) };
    writeFileSync(fixture.recipe.valuesFile, JSON.stringify(values));
    fixture.rerender();
    const configured = environments();
    for (const [index, name] of services.entries()) assert.equal(admission(configured[name]), `192.0.2.${index + 1},2001:db8::10`, name);
    assert.ok(fixture.render.config.every(resource => resource.metadata?.namespace !== 'ingress-nginx'));
    for (const resource of fixture.render.config) assert.ok(!Object.hasOwn(resource.metadata?.annotations || {}, 'nginx.ingress.kubernetes.io/proxy-set-headers'));
  } finally { fixture.dispose(); }
});
