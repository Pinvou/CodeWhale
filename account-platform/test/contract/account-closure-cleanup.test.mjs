import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('identity cleanup is lifecycle-versioned, irreversible, and releases identifiers only after all cleanup proofs', async () => {
  const [migration, cleanup, worker, commerce] = await Promise.all([
    read('migrations/account/000028_account_closure_cleanup.up.sql'),
    read('internal/account/account_closure_cleanup.go'),
    read('internal/account/account_closure.go'),
    read('internal/account/commerce_mapping.go'),
  ]);
  assert.match(migration, /subject_lifecycle_version/);
  assert.match(migration, /account_subject_retention_records/);
  assert.match(migration, /CANCELLED_ACCOUNT_CLOSURE/);
  assert.match(cleanup, /PrepareAccountClosureCleanup/);
  assert.match(cleanup, /Delete FROM identities|DELETE FROM identities/);
  assert.match(cleanup, /status='DELETED'/);
  assert.match(cleanup, /stage='ANONYMIZED'/);
  assert.match(cleanup, /retained_until/);
  assert.match(cleanup, /account\.user\.anonymized\.v1/);
  assert.match(cleanup, /published_at IS NOT NULL/);
  assert.match(worker, /DeleteUser/);
  assert.match(worker, /IsolateGrants/);
  assert.match(commerce, /subject_lifecycle_version/);
  for (const callback of ['CompleteCommerceCustomerSyncJob', 'RetryCommerceCustomerSyncJob']) {
    const body = commerce.match(new RegExp(`func \\(r \\*PostgresRepository\\) ${callback}\\([^\\n]*\\) error \\{([\\s\\S]*?)\\n\\}`))?.[1];
    assert.ok(body, `${callback} is installed`);
    const subjectLock = body.indexOf('lockCommerceSubjectTx');
    const generationFence = body.indexOf('lifecycle != job.SubjectLifecycleVersion');
    const mappingWrite = body.indexOf('UPDATE commerce_customer_mappings');
    assert.ok(subjectLock >= 0 && generationFence > subjectLock && mappingWrite > generationFence, `${callback} checks the locked subject generation before a mapping write`);
    assert.match(body, /(?:userStatus|status) == "DELETED"/);
    assert.match(body, /(?:userStatus|status) == "ANONYMIZED"/);
  }
});

test('account-closed organization grants remain isolated and only full eligible remainder can be reclaimed', async () => {
  const [migration, lifecycle, grants, tokenService, portal] = await Promise.all([
    read('migrations/billing/000030_account_closed_grants.up.sql'),
    read('internal/billing/organization_member_lifecycle.go'),
    Promise.all([read('internal/billing/account_closed_grants.go'), read('internal/billing/account_closure_grant_isolation.go')]).then(parts => parts.join('\n')),
    Promise.all([read('internal/token/service.go'), read('internal/token/organization_finance_routes.go')]).then(parts => parts.join('\n')),
    Promise.all([read('internal/portal/service.go'), read('internal/portal/organization_proxy.go')]).then(parts => parts.join('\n')),
  ]);
  assert.match(migration, /ACCOUNT_CLOSED_ISOLATED/);
  assert.match(migration, /RECLAIMED_ACCOUNT_CLOSED/);
  assert.match(migration, /account_closed_grant_reclaim_commands/);
  assert.match(lifecycle, /suspension_reason='MEMBER_REMOVED'/);
  assert.doesNotMatch(lifecycle, /suspension_reason='ACCOUNT_CLOSED'.*status='ACTIVE'/s);
  assert.match(grants, /status='SUSPENDED',suspension_reason=\$2/);
  assert.match(grants, /status='RECLAIMED_ACCOUNT_CLOSED'/);
  assert.match(grants, /suspended_grant_microcredits=suspended_grant_microcredits-\$2,available_microcredits=available_microcredits\+\$2/);
  assert.match(grants, /RequireAnonymized/);
  assert.match(grants, /ReservedMicrocredits != 0/);
  assert.match(tokenService, /account-closed-grants/);
  assert.match(portal, /account-closed-grants/);
});

test('public contract exposes single and independently committed batch reclaim without an amount field', async () => {
  const contract = YAML.parse(await read('contracts/openapi/account-closed-grants.yaml'));
  const single = contract.paths['/v1/organizations/{organization_id}/account-closed-grants/{grant_id}/reclaim'].post;
  const batch = contract.paths['/v1/organizations/{organization_id}/account-closed-grants:reclaim-batch'].post;
  assert.ok(single);
  assert.ok(batch);
  const singleSchema = single.requestBody.content['application/json'].schema;
  assert.deepEqual(singleSchema.required, ['reason']);
  assert.equal(singleSchema.properties.amount_microcredits, undefined);
  assert.deepEqual(batch.requestBody.content['application/json'].schema.required, ['grant_ids', 'reason']);
  assert.ok(batch.responses['207']);
});
