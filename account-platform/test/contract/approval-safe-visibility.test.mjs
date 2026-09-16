import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = (relative) => readFileSync(path.join(root, relative), 'utf8');

test('approval directory and request reads preserve the non-leaking authorization boundary', () => {
  const service = read('internal/account/service.go');
  const visibility = read('internal/account/approval_visibility.go');
  const approvals = read('internal/account/approvals.go');
  const openapi = read('contracts/openapi/admin.yaml');
  const catalog = JSON.parse(read('deploy/authorization-entrypoints.json'));

  assert.match(service, /admin\.GET\("\/approval-policies", s\.requirePermission\(PermissionPlatformRolesRead\)/);
  assert.match(service, /adminV2\.GET\("\/approval-directory", httpserver\.AdaptFunc\(s\.adminApprovalDirectory\)\)/);
  assert.match(service, /"APPROVAL_REQUEST_NOT_FOUND", "the approval request was not found"/);
  assert.match(visibility, /ErrApprovalRequestNotFound/);
  assert.match(visibility, /approvalRequestVisibilityForActor[\s\S]*SELECT ar\.requester_user_id=\$2,[\s\S]*role_permissions[\s\S]*ErrApprovalRequestNotFound/);
  assert.match(approvals, /approvalRequestVisibilityForActor\(ctx, r\.pool, actorID, requestID\)[\s\S]*approvalRequestByID\(ctx, r\.pool, requestID\)/);
  assert.match(approvals, /item\.RequesterUserID != actorID && !canApprove[\s\S]*continue/);

  const policy = catalog.http_policies.find((entry) => entry.receiver === 'adminV2' && entry.method === 'GET' && entry.path === '/approval-directory');
  assert.deepEqual(policy, {
    source: 'account-platform/internal/account/service.go',
    receiver: 'adminV2',
    method: 'GET',
    path: '/approval-directory',
    scope: 'platform',
    strategy: 'domain.approval-policy-v2-directory',
  });

  const directorySchema = openapi.slice(openapi.indexOf('    ApprovalDirectoryProjection:'), openapi.indexOf('    ApprovalPolicy:'));
  assert.match(directorySchema, /additionalProperties: false/g);
  assert.match(directorySchema, /required: \[command_type, policy_version, snapshot_schema_version, risk_class, visible, requestable\]/);
  assert.doesNotMatch(directorySchema, /approvable|requester_permission|approver_permission|allowed_actions/);
});
