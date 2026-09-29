import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('finance payment product and approval actions use validated structured panels', async () => {
  const [view, product, productStyles, approvals, approvalStyles, copy, built] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('internal/portal/static/assets/product-admin.js'),
    read('internal/portal/static/assets/product-admin.css'),
    read('internal/portal/static/assets/approval-requests.js'),
    read('internal/portal/static/assets/approval-requests.css'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('internal/portal/static/assets/admin.js'),
  ]);

  const billing = view.slice(view.indexOf('function Billing('), view.indexOf('function BillingDetail('));
  assert.doesNotMatch(billing, /window\.(prompt|confirm)\s*\(/);
  assert.match(view, /function StructuredActionPanel/);
  assert.match(billing, /minimumLength\(field\.minLength\)/);
  assert.match(billing, /field\.future/);
  assert.match(billing, /executeStructuredFinanceAction/);
  assert.match(billing, /actionPanel\.execute\(values, \{ idempotencyKey: actionPanel\.idempotencyKey \}\)/);
  assert.match(billing, /writeSucceeded: true/);
  assert.match(billing, /outcomeUnknown: unknown/);
  assert.match(view, /const refreshRequired = financeActionRequiresRefresh\(operation\)/);
  assert.match(view, /busy \|\| refreshRequired \? \(\) => \{\} : onClose/);
  assert.match(view, /disabled=\{busy \|\| refreshRequired\}/);
  assert.match(billing, /const renderWithActionPanel/);
  assert.equal((billing.match(/return renderWithActionPanel\(/g) || []).length, 7);
  assert.match(billing, /normalRefundCreationEnabled && item\.status === 'ACTIVE'/);
  assert.match(billing, /canReadUserDirectory &&/);
  assert.match(billing, /type: 'directory-search'/);
  assert.match(billing, /client\.users\(String\(query \|\| ''\)\.trim\(\)\)/);
  assert.match(billing, /submit: false/);
  assert.match(billing, /sequence !== ownerSearchSequence\.current/);
  assert.match(billing, /idempotencyPrefix: 'target-refund'/);
  assert.match(billing, /idempotencyPrefix: 'hold-release'/);
  assert.match(billing, /idempotencyPrefix: 'reconciliation-case'/);
  assert.match(billing, /idempotencyPrefix: 'payment-anomaly-resolve'/);
  assert.match(view, /aria-live="assertive"/);
  for (const action of ['releaseHold', 'repairPaymentOrder', 'updateReconciliationCase', 'resolvePaymentAnomaly', 'scheduleLatePaymentReview', 'escalateProviderBlind']) {
    assert.match(billing, new RegExp(`client\\.${action}`));
  }

  assert.doesNotMatch(product, /\bprompt\s*\(/);
  assert.match(product, /id="product-action-dialog"/);
  assert.match(product, /minlength="10"/);
  assert.match(product, /expected_version:pendingAction\.version/);
  assert.match(product, /if\(\[\.\.\.reason\]\.length<10\)/);
  assert.match(product, /command\(path,body,pendingAction\.idempotencyKey\)/);
  assert.match(product, /pendingAction\.idempotencyKey=draft\.idempotencyKey/);
  assert.match(product, /command\(request\.path,request\.body,createIdempotencyKey,request\.method\)/);
  assert.match(product, /actionSubmit\.disabled\|\|pendingAction\?\.outcomeUnknown/);
  assert.match(product, /setActionCloseLocked\(true\)/);
  assert.match(product, /if\(pendingAction\.outcomeUnknown\)/);
  assert.match(product, /role="alert" aria-live="assertive"/);
  assert.match(product, /actionDraftKey="pinvou\.product-admin\.action-draft"/);
  assert.match(product, /sessionStorage\.setItem\(actionDraftKey/);
  assert.match(product, /oc\.createConfirmation/);
  assert.match(productStyles, /\.product-action-dialog/);

  assert.doesNotMatch(approvals, /\bprompt\s*\(/);
  assert.match(approvals, /id="approval-decision-dialog"/);
  assert.match(approvals, /minlength="3"/);
  assert.match(approvals, /observed_versions:item\.critical_resource_versions\|\|\{\}/);
  assert.match(approvals, /pendingDecision=\{\.\.\.pendingDecision,outcomeUnknown:true\}/);
  assert.match(approvals, /uncertainDecision=\{\.\.\.pendingDecision\}/);
  assert.match(approvals, /if\(uncertainDecision\)return/);
  assert.match(approvals, /const actions=uncertainDecision\?\[\]/);
  assert.match(approvals, /request\(`\/admin\/v1\/approval-requests\/\$\{encodeURIComponent\(requestID\)\}`\)/);
  assert.match(approvals, /latest\?\.approval_request_id!==requestID/);
  assert.match(approvals, /latest\.version<frozenVersion/);
  assert.match(approvals, /approvalStatuses\.has\(latest\?\.status\)/);
  assert.match(approvals, /if\(!uncertainDecision\)\{pendingDecision=null;render\(\);\}/);
  assert.match(approvals, /submit\?\.disabled\|\|pendingDecision\?\.outcomeUnknown/);
  assert.match(approvals, /if\(pendingDecision\.outcomeUnknown\)/);
  assert.match(approvals, /role="alert" aria-live="assertive"/);
  assert.match(approvalStyles, /\.approval-decision-dialog/);

  for (const marker of ['adjustmentConfirmation', 'refundTitle', 'releaseTitle', 'anomalyTitle', 'providerBlindTitle', 'caseConfirmations', 'futureTime']) {
    assert.match(copy, new RegExp(marker));
  }
  assert.match(built, /adm-action-panel/);
  assert.match(built, /actionPanel/);
});
