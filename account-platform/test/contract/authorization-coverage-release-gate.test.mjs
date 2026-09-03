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
  assert.deepEqual(await runAuthorizationCoverage(), { routes: 284, tasks: 25, events: 3, cli: 13 });
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
