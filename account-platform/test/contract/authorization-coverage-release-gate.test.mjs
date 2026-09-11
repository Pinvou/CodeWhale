import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { runAuthorizationCoverage } from '../../../scripts/check-account-platform-authorization-coverage.mjs';

const catalogURL = new URL('../../deploy/authorization-entrypoints.json', import.meta.url);

async function rejectedMutation(mutate, expected) {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-authorization-coverage-'));
  try {
    const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
    mutate(catalog);
    const target = path.join(directory, 'authorization-entrypoints.json');
    await writeFile(target, `${JSON.stringify(catalog, null, 2)}\n`);
    await assert.rejects(runAuthorizationCoverage(target), expected);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

test('authorization release gate classifies every HTTP task event callback and CLI entrypoint', async () => {
  assert.deepEqual(await runAuthorizationCoverage(), { routes: 342, tasks: 27, events: 6, cli: 20 });
});

test('fixed NATS resource operator has an exact required authorization entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'event-resources'), [{
    name: 'event-resources', source: 'account-platform/cmd/event-resources/main.go',
    strategy: 'protocol.offline-fixed-nats-authorization-or-workload-fixed-resources',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'event-resources');
  }, /unclassified_cli:event-resources/);
});

test('single-client workload token projector has an exact required authorization entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'workload-token-agent'), [{
    name: 'workload-token-agent', source: 'account-platform/cmd/workload-token-agent/main.go',
    strategy: 'workload.single-client-verified-token-projection',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'workload-token-agent');
  }, /unclassified_cli:workload-token-agent/);
});

test('authorization release gate fails closed for removed unknown and scope-mismatched policies', async () => {
  await rejectedMutation((catalog) => catalog.http_policies.shift(), /unclassified_http_entrypoint/);
  await rejectedMutation((catalog) => {
    const policy = catalog.http_policies.find((item) => item.scope === 'organization');
    policy.permissions = ['org.unknown.injected'];
  }, /unknown_human_permission/);
  await rejectedMutation((catalog) => {
    const policy = catalog.http_policies.find((item) => item.scope === 'organization');
    policy.scope = 'platform';
  }, /permission_scope_mismatch/);
});

test('authorization release gate rejects unregistered tasks events and command surfaces', async () => {
  await rejectedMutation((catalog) => catalog.tasks.pop(), /unclassified_task/);
  await rejectedMutation((catalog) => catalog.events.pop(), /unclassified_event/);
  await rejectedMutation((catalog) => catalog.cli.pop(), /unclassified_cli/);
});

test('authorization gate discovers all three unresolved Token approval subscriptions', async () => {
  for (const durable of ['token-model-lifecycle-executor-v1', 'token-catalog-rollback-executor-v1', 'token-credential-revoke-executor-v1']) {
    await rejectedMutation((catalog) => {
      catalog.events = catalog.events.filter((entry) => entry.durable !== durable);
    }, new RegExp(`unclassified_event:.*${durable}`));
  }
});
