import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

// The recovery worker may investigate and (through the documented confirmation
// endpoint) converge a persisted original success, but it must never gain a
// general "apply / retry / invalidate" switch: a financial terminal transition
// needs its own governed surface. This locks the CLI's read-only shape so a
// later edit cannot quietly turn the investigator into an executor.
test('the approval recovery CLI stays a bounded read-only investigator', async () => {
  const source = await readFile(path.join(root, 'cmd/approval-recovery/main.go'), 'utf8');

  // Exactly two options, both bounded: a candidate limit and a wall-clock
  // deadline. No action selector may appear.
  const options = [...source.matchAll(/flags\.\w+\("([a-z-]+)"/g)].map((match) => match[1]).sort();
  assert.deepEqual(options, ['deadline', 'limit'], 'the CLI must only expose the bounded investigation options');
  for (const forbidden of ['apply', 'invalidate', 'retry', 'execute', 'force', 'complete']) {
    assert.doesNotMatch(source, new RegExp(`flags\\.\\w+\\("${forbidden}`), `the CLI must not accept a ${forbidden} option`);
  }

  // It never calls a domain writer and never issues a mutating HTTP method.
  assert.doesNotMatch(source,
    /Apply(LedgerRepair|ReceivableWriteOff|PaymentFinancialRepair|RefundExceptionRepair|ChargebackFinancialRepair|ReconciliationFinancialRepair|UsageSettlementRepair)|BeginApprovalExecution|CompleteApprovalExecution|ApplyReviewedForcedRefund/,
    'the CLI must not call any domain writer');
  assert.doesNotMatch(source, /http\.Method(Post|Put|Patch|Delete)/, 'the CLI must not send a mutating HTTP request');

  // It must run as the dedicated NOLOGIN recovery role and refuse any other
  // session identity, so a misconfigured deployment cannot inherit write rights.
  assert.match(source, /role != "account_approval_recovery"/, 'the CLI must verify its dedicated database role');
  assert.match(source, /APPROVAL_RECOVERY_DATABASE_URL/, 'the CLI must require its own database URL');

  // Its machine output is closed: statuses, reasons and counts only.
  const summary = source.slice(source.indexOf('type commandSummary struct'), source.indexOf('type investigateAction'));
  const fields = [...summary.matchAll(/^\t([A-Z]\w+)\s+(\S+)/gm)].map((match) => `${match[1]}:${match[2]}`);
  assert.deepEqual(fields, [
    'Status:string', 'Reason:string', 'Candidates:int', 'Observed:int', 'Confirmed:int',
    'Converged:int', 'Unavailable:int', 'MoreCandidates:bool',
  ], 'the CLI summary must stay a closed set of counts and statuses');
  assert.doesNotMatch(summary, /json\.RawMessage|uuid\.UUID|\[\]byte/, 'the summary must not carry domain payloads');

  // Candidate output is bounded and redacted by construction: the limit is
  // validated before any read and the deadline caps the whole investigation.
  assert.match(source, /\*limit < 1 \|\| \*limit > 200/, 'the candidate limit must stay bounded');
  assert.match(source, /\*deadline < time\.Second \|\| \*deadline > 30\*time\.Second/, 'the investigation deadline must stay bounded');
});
