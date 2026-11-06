import test from 'node:test';
import assert from 'node:assert/strict';
import { organizationNameInputError, acceptedRenamedUnit, acceptedRenamedOrganization } from '../src/features/account-platform/organization-name-feedback.js';

test('Name validation counts the actual account owner rune limit, without Unicode normalization', () => {
  assert.equal(organizationNameInputError(''), 'nameRequired');
  assert.equal(organizationNameInputError(' \t\n\u0085\u3000 '), 'nameRequired');
  assert.equal(organizationNameInputError('名'.repeat(120)), '');
  assert.equal(organizationNameInputError('名'.repeat(121)), 'nameTooLong');
  assert.equal(organizationNameInputError('😀'.repeat(120)), '');
  assert.equal(organizationNameInputError('😀'.repeat(121)), 'nameTooLong');
  assert.equal(organizationNameInputError('e\u0301'.repeat(60)), '');
  assert.equal(organizationNameInputError('e\u0301'.repeat(61)), 'nameTooLong');
});

test('Name measurement preserves established JS trim then Go TrimSpace edge semantics', () => {
  assert.equal(organizationNameInputError('\u0085' + '😀'.repeat(120) + '\u0085'), '');
  assert.equal(organizationNameInputError('\ufeff' + '名'.repeat(120) + '\ufeff'), '');
  // Go trims NEL after the existing frontend trim, revealing FEFF which Go
  // does not trim. Repeated mixed trim or a generic JS-only trim is incorrect.
  assert.equal(organizationNameInputError('\u0085\ufeff\u0085'), '');
  assert.equal(organizationNameInputError('\u0085\ufeff' + '名'.repeat(120) + '\ufeff\u0085'), 'nameTooLong');
});

test('Saved unit display is selected by canonical organization/unit and acknowledged version, never draft/name/position', () => {
  const org = '00000000-0000-4000-8000-000000000001';
  const id = '00000000-0000-4000-8000-000000000002';
  const unit = { organization_id: org, unit_id: id, parent_id: null, name: 'Accepted name', status: 'ACTIVE', version: 2 };
  const change = { organizationID: org, target: id, result: { unit, structure_version: 3, replayed: false } };
  const newer = { ...unit, name: 'Later accepted name', version: 3 };
  const other = { ...unit, unit_id: '00000000-0000-4000-8000-000000000003' };
  assert.strictEqual(acceptedRenamedUnit(change, { items: [other, newer] }), newer);
  assert.equal(acceptedRenamedUnit(change, { items: [other] }), undefined);
  assert.equal(acceptedRenamedUnit(change, { items: [{ ...unit, version: 1 }] }), undefined);
  assert.equal(acceptedRenamedUnit(change, { items: [{ ...unit, organization_id: other.unit_id }] }), undefined);
  assert.equal(acceptedRenamedUnit({ ...change, result: { ...change.result, unit: other } }, { items: [newer] }), undefined);
  assert.equal(acceptedRenamedUnit(change, undefined), undefined);
});

test('Organization204 acknowledgement needs the current canonical read after the acknowledgement; an older cached page or later failed attempt is not confirmation', () => {
  const org = '00000000-0000-4000-8000-000000000001';
  const current = { organization_id: org, name: 'Actual read name', status: 'ACTIVE', version: 3 };
  const change = { operation: 'rename', organizationID: org, requiredReadGeneration: 7, result: null };
  assert.equal(acceptedRenamedOrganization(change, current, 6, 6), undefined);
  assert.strictEqual(acceptedRenamedOrganization(change, current, 7, 7), current);
  assert.equal(acceptedRenamedOrganization(change, current, 7, 8), undefined);
  const newer = { ...current, name: 'Later authoritative name', version: 4 };
  assert.strictEqual(acceptedRenamedOrganization(change, newer, 8, 8), newer);
  assert.equal(acceptedRenamedOrganization(change, { ...current, organization_id: '00000000-0000-4000-8000-000000000002' }, 7, 7), undefined);
  assert.equal(acceptedRenamedOrganization(change, undefined, 7, 7), undefined);
  assert.equal(acceptedRenamedOrganization({ ...change, operation: 'renameUnit' }, current, 7, 7), undefined);
  assert.equal(acceptedRenamedOrganization({ ...change, requiredReadGeneration: undefined }, current, 7, 7), undefined);
});
