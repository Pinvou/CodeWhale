import assert from 'node:assert/strict';
import fs from 'node:fs';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { freshLatePaymentQuery, latePaymentQueryAuthority } from '../../src/features/account-platform/late-payment-query-contract.js';

// The PostgreSQL contract supplies an owned local HTTP receiver and real case.
const input = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const client = new AdminPlatformClient(input.url);
client.csrfToken = 'PUBLIC_SYNTHETIC_QUERY_CSRF';
const authority = latePaymentQueryAuthority({ user_id: input.actor_id, active_context: { platform_authorization_version: 1 } });
const facts = freshLatePaymentQuery(input.item, input.actor_id, authority);
const original = await client.queryLatePaymentReview(input.item.case_id, facts, input.key);
const recovered = await client.queryLatePaymentReviewResult(input.item.case_id, facts, input.key);
assert.deepEqual(recovered, original);
console.log(JSON.stringify({ case_id: original.case_id, last_query_result: original.last_query_result }));
