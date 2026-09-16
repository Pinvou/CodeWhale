import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { adminSectionURL } from '../../../pinvou3-app/src/features/account-platform/admin-navigation.js';
import { adminPlatformCopy } from '../../../pinvou3-app/src/shared/admin-platform-i18n.js';

const root = path.resolve(import.meta.dirname, '../..');
const read = (relative) => readFileSync(path.join(root, relative), 'utf8');

test('approval center mounts one governed four-view reader without the legacy direct-v1 script', () => {
  const html = read('internal/portal/static/admin.html');
  const view = read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  const workspace = read('../pinvou3-app/src/features/account-platform/AdminApprovalWorkspace.jsx');
  const client = read('../pinvou3-app/src/features/account-platform/admin-client.js');
  const built = read('internal/portal/static/assets/admin.js');
  assert.doesNotMatch(html, /<script[^>]*src="\/assets\/approval-requests\.js"/);
  assert.match(view, /<AdminApprovalWorkspace/);
  assert.match(view, /approval_request_id: null/);
  assert.match(view, /approval_request_id: approval\?\.approval_request_id \|\| null/);
  assert.match(view, /approval_request_id: adjustmentApprovalByTarget\.get\(item\.adjustment_id\)\?\.approval_request_id \|\| item\.approval_request_id \|\| null/);
  assert.match(workspace, /\['mine', 'pending', 'execution', 'directory'\]/);
  assert.match(workspace, /client\.governedApprovalDirectory/);
  assert.match(workspace, /client\.governedApprovalRequests/);
  assert.match(workspace, /client\.governedApprovalRequest/);
  assert.match(workspace, /client\.decideGovernedApproval/);
  assert.match(workspace, /const decision = approvalDecisionForAction\(action\)/);
  assert.match(workspace, /client\.decideGovernedApproval\(route\.requestID, detail, decision, reason\.trim\(\)\)/);
  assert.match(workspace, /else if \(action === 'SUBMIT'\)/);
  assert.match(workspace, /client\.withdrawGovernedApprovalRequest/);
  assert.match(workspace, /client\.submitGovernedApprovalRequest/);
  assert.match(workspace, /approvalBusinessRoute\(detail/);
  assert.match(workspace, /visibleSections\.includes\(businessRoute\.section\)/);
  assert.match(workspace, /addEventListener\?\.\('popstate'/);
  assert.match(workspace, /route\.cursor && <button[^>]+/);
  assert.doesNotMatch(workspace, /client\.approvalRequests\(/);
  assert.doesNotMatch(workspace, /allowed_actions/);
  assert.match(built, /approval_view/);
});

test('approval center labels and error states exist in all three management locales', () => {
  for (const locale of ['zh', 'en', 'ja']) {
    for (const key of ['mine', 'pending', 'execution', 'directory', 'listUnavailable', 'detailUnavailable', 'historicalMinimum', 'firstPage', 'openBusinessArea']) {
      assert.ok(adminPlatformCopy[locale].approvalCenter[key], `${locale}.${key}`);
    }
  }
});

test('approval detail deep link replaces stale cursor and preserves an unrelated hash', () => {
  const location = { pathname: '/admin.html', search: '?section=approvals&approval_cursor=old&approval_request_id=old-id', hash: '#work' };
  const result = adminSectionURL('approvals', location, {
    approval_cursor: null, approval_request_id: 'request-id',
    approval_command_type: 'WALLET_ADJUSTMENT', approval_target: 'adjustment-id',
  });
  assert.equal(result, '/admin.html?section=approvals&approval_request_id=request-id&approval_command_type=WALLET_ADJUSTMENT&approval_target=adjustment-id#work');
});
