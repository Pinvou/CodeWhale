import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = (relative) => readFileSync(path.join(root, relative), 'utf8');

test('Account approval pagination freezes bounded membership and a closed signed cursor', () => {
  const source = read('internal/account/approval_pagination.go');
  const migration = read('migrations/account/000061_approval_pagination_snapshots.up.sql');
  const keyBinding = read('migrations/account/000062_approval_pagination_key_binding.up.sql');
  const rollbackFence = read('migrations/account/000063_approval_pagination_rollback_fence.down.sql');
  const forwardOnly = read('migrations/account/000064_approval_pagination_forward_only.down.sql');

  for (const contract of [
    /ApprovalSnapshotTTL\s+= 15 \* time\.Minute/,
    /ApprovalSnapshotTombstoneTTL\s+= 20 \* time\.Minute/,
    /ApprovalSnapshotKeyRetention\s+= ApprovalSnapshotTTL \+ ApprovalSnapshotTombstoneTTL/,
    /ApprovalSnapshotBuildBudget\s+= 10 \* time\.Second/,
    /ApprovalSnapshotReadBudget\s+= 3 \* time\.Second/,
    /ApprovalSnapshotMaxMembers\s+= 1_000_000/,
    /ApprovalSnapshotMaxGlobal\s+= 64/,
    /ApprovalSnapshotMaxGlobalMembers\s+= 20_000_000/,
    /ApprovalSnapshotMaxStorageBytes\s+= int64\(8 \* 1024 \* 1024 \* 1024\)/,
    /ApprovalSnapshotSubjectPerMinute\s+= 6/,
    /ApprovalSnapshotGlobalPerMinute\s+= 60/,
    /ApprovalReadSubjectPerMinute\s+= 60/,
    /ApprovalReadIPPerMinute\s+= 120/,
  ]) assert.match(source, contract);

  const cursor = source.slice(source.indexOf('type approvalPaginationCursor struct'), source.indexOf('type approvalPaginationRateError struct'));
  const keys = [...cursor.matchAll(/`json:"([^"]+)"`/g)].map((match) => match[1]);
  assert.deepEqual(keys, ['v', 'kid', 'snapshot_id', 'subject_id', 'authorization_version', 'resource', 'scope_digest', 'filter_digest', 'sort', 'limit', 'position', 'position_key', 'as_of', 'expires_at']);
  assert.match(source, /hmac\.New\(sha256\.New, key\)/);
  assert.match(source, /if !hmac\.Equal\(signature, mac\.Sum\(nil\)\)/);
  assert.match(source, /parseApprovalPaginationCursor\(cursor, r\.approvalPagination, r\.now\(\)\)[\s\S]*userIDByPrincipal\(operationCtx, principal\)/);
  assert.match(source, /parsedCursor\.AuthorizationVersion != version \|\| parsedCursor\.ScopeDigest != scopeDigest[\s\S]*ErrApprovalPaginationCursorExpired/);
  assert.match(source, /ORDER BY ar\.updated_at DESC,ar\.id DESC LIMIT \$13/);
  assert.match(source, /ORDER BY m\.position LIMIT \$3/);
  assert.match(source, /FROM approval_pagination_snapshot_members m[\s\S]*role_assignments ra JOIN role_permissions rp/);

  assert.match(migration, /accepting_new_snapshots BOOLEAN NOT NULL DEFAULT FALSE/);
  assert.match(migration, /UNIQUE INDEX approval_pagination_subject_resource_idx/);
  assert.match(migration, /CHECK\(member_count BETWEEN 0 AND 1000000\)/);
  assert.match(migration, /CREATE TABLE approval_pagination_snapshot_tombstones/);
  assert.match(migration, /CREATE TABLE approval_pagination_signing_keys/);
  assert.match(keyBinding, /ADD COLUMN key_digest TEXT CHECK/);
  assert.match(rollbackFence, /ACCOUNT_APPROVAL_PAGINATION_CURSOR_ACTIVE/);
  assert.match(rollbackFence, /ACCOUNT_APPROVAL_PAGINATION_TOMBSTONE_ACTIVE/);
  assert.match(rollbackFence, /ACCOUNT_APPROVAL_PAGINATION_KEY_ACTIVE/);
  assert.match(forwardOnly, /ACCOUNT_APPROVAL_PAGINATION_FORWARD_ONLY/);
  assert.match(source, /approval_pagination_signing_keys\.key_digest=EXCLUDED\.key_digest/);
  assert.match(migration, /REVOKE ALL[\s\S]*FROM PUBLIC/);
});

test('Account approval pagination publishes stable failures, watermarks, and cleanup ownership', () => {
  const source = read('internal/account/approval_pagination.go');
  const metrics = read('internal/account/approval_pagination_metrics.go');
  const main = read('cmd/account-service/main.go');
  const rules = read('deploy/helm/account-platform/templates/prometheus-rules.yaml');

  for (const [status, code] of [
    [400, 'INVALID_LIMIT'], [400, 'INVALID_FILTER'], [400, 'INVALID_CURSOR'],
    [410, 'CURSOR_EXPIRED'], [422, 'PAGINATION_FILTER_REQUIRED'],
    [429, 'PAGINATION_SNAPSHOT_LIMITED'],
    [504, 'QUERY_TIMEOUT'], [503, 'PAGINATION_SNAPSHOT_UNAVAILABLE'],
  ]) assert.match(source, new RegExp(`Status: ${status}, Code: "${code}"`));
  assert.match(source, /code := "RATE_LIMITED"[\s\S]*Status: 429, Code: code/);
  assert.match(source, /peak >= 70 \|\| result\.OldestCleanupLag > 5\*time\.Minute/);
  assert.match(source, /result\.Critical = peak >= 85/);
  assert.match(metrics, /pinvou_account_approval_pagination_capacity_level/);
  assert.match(metrics, /pinvou_account_approval_pagination_cleanup_lag_seconds/);
  assert.match(main, /approval-pagination-cleanup[\s\S]*NewApprovalPaginationCleanupWorker\(repository, time\.Minute\)/);
  assert.match(main, /RunWithApplicationAndCollectors[\s\S]*repository\.ApprovalPaginationCollectors\(\)/);
  assert.match(rules, /PinvouAccountApprovalPaginationCapacityWarning/);
  assert.match(rules, /PinvouAccountApprovalPaginationCapacityCritical/);
  assert.match(rules, /PinvouAccountApprovalPaginationCleanupBacklog/);
});
