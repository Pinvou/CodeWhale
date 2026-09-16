import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('Token authorization consumers use raw delivery and never enter Billing attempts or DLQ', async () => {
  const [main, supervisor, delivery] = await Promise.all([
    read('cmd/token-command-executor/main.go'),
    read('internal/token/approval_consumer_supervisor.go'),
    read('internal/token/approval_delivery.go'),
  ]);
  assert.match(main, /RunTokenApprovalConsumers\(ctx, broker, reader/);
  assert.match(supervisor, /ConsumeAuthorization\(ctx, "token-model-lifecycle-executor-v1", lifecycle\.Handle\)/);
  assert.match(supervisor, /ConsumeAuthorization\(ctx, "token-catalog-rollback-executor-v1", catalogRollback\.Handle\)/);
  assert.match(supervisor, /ConsumeAuthorization\(ctx, "token-credential-revoke-executor-v1", credentialRevoke\.Handle\)/);
  assert.doesNotMatch(supervisor, /ConsumeUntilResolved|\.Consume\(/);
  assert.match(delivery, /ResolveApprovalAuthorizationRoute|\/route/);
  assert.match(delivery, /route\.CanonicalDurable != consumer\.durable[\s\S]*DoubleAck/);
  assert.doesNotMatch(delivery, /PublishDeadLetter|GrantAuthorizationAttempt|RecordAuthorizationFailure|DOMAIN_INFLIGHT|DLQ_PUBLISHED/);
});
