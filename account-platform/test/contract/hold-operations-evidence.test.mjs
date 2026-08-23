import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('hold lease and operator evidence are append-only and backfilled', async () => {
  const migration = await read('migrations/billing/000026_hold_operations_evidence.up.sql');
  assert.match(migration, /CREATE TABLE usage_hold_lease_events/);
  assert.match(migration, /SELECT id,id,'CREATED'/);
  assert.match(migration, /CREATE TABLE usage_hold_admin_actions/);
  assert.match(migration, /UNIQUE\(action,idempotency_key\)/);
  assert.match(migration, /usage_hold_lease_events_immutable/);
  assert.match(migration, /usage_hold_admin_actions_immutable/);
});

test('new and extended holds persist the effective lease and amount history', async () => {
  const [personal, member] = await Promise.all([
    read('internal/billing/postgres.go'),
    read('internal/billing/member_quota_holds.go'),
  ]);
  assert.match(personal, /usage_hold_lease_events[^`]+?'CREATED'/s);
  assert.match(member, /usage_hold_lease_events[^`]+?'CREATED'/s);
  assert.match(personal, /previousExpiresAt, previousMaximum/);
  assert.match(personal, /usage_hold_lease_events[^`]+?'EXTENDED'/s);
});

test('hold operations expose component, member, pricing, lease, and action evidence', async () => {
  const admin = await read('internal/billing/admin.go');
  assert.match(admin, /type AdminHoldDetail struct/);
  assert.match(admin, /ComponentAllocations \[\]AdminHoldComponentAllocation/);
  assert.match(admin, /MemberAllocations\s+\[\]AdminHoldMemberAllocation/);
  assert.match(admin, /LeaseEvents\s+\[\]AdminHoldLeaseEvent/);
  assert.match(admin, /AdminActions\s+\[\]AdminHoldAction/);
  assert.match(admin, /component\.component_type,component\.credit_lot_id/);
  assert.match(admin, /grant\.organization_id,grant\.member_user_id/);
});

test('safe release is limited to anomalous holds and is reasoned and idempotent', async () => {
  const admin = await read('internal/billing/admin.go');
  assert.match(admin, /Idempotency-Key/);
  assert.match(admin, /usage-hold\.admin\.safe-release/);
  assert.match(admin, /hold\.Status != "REVIEW" && !\(hold\.Status == "ACTIVE" && !hold\.ExpiresAt\.After\(now\)\)/);
  assert.match(admin, /request_fingerprint/);
  assert.match(admin, /allocation_snapshot/);
});

test('admin contract and portal expose evidence without a balance mutation route', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const client = await read('../pinvou3-app/src/features/account-platform/admin-client.js');
  const view = await read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  assert.ok(contract.paths['/billing/admin/v1/usage-holds/{hold_id}'].get);
  assert.ok(contract.paths['/billing/admin/v1/usage-holds/{hold_id}/release'].post.parameters.some(parameter => parameter.$ref?.endsWith('/IdempotencyKey')));
  assert.ok(contract.components.schemas.UsageHoldDetail.properties.component_allocations);
  assert.match(client, /hold\(holdID, signal\)/);
  assert.match(client, /hold-release-\$\{crypto\.randomUUID\(\)\}/);
  assert.match(view, /holdDetail\.lease_events/);
});
