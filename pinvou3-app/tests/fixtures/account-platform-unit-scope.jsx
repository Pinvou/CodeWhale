import React from 'react';
import { createRoot } from 'react-dom/client';
import { OrganizationWorkspace } from '../../src/features/account-platform/OrganizationWorkspace.jsx';
import { organizationCopy } from '../../src/shared/i18n.js';
import dto from './organization-unit-scope-dto.json';

// Captured from genuine public-migration Account HTTP DTOs in an isolated
// synthetic fixture. Transport and domain effects below are controlled seams.
const clone = value => JSON.parse(JSON.stringify(value));
const root = createRoot(document.querySelector('#root'));
const control = { dto, kind: 'unit', language: 'en', calls: [], changed: 0, revoked: false, delayed: '', pagination: false, failed: 0 };
const listeners = { context: new Set(), authorization: new Set() };
const actor = () => dto[['mixed', 'auditor'].includes(control.kind) ? 'mixed' : 'unit'];
const newID = '99999999-9999-4999-8999-999999999999';
const allUnits = () => [...clone(actor().tree_a.items), ...[[dto.root_c, 'Scope C'], [dto.outside_b, 'Outside B']].map(([unit_id, name]) => ({ unit_id, name, parent_id: null, status: 'ACTIVE', version: 1, organization_id: dto.organization_id }))];
const client = {
 financeSessionEpoch: 1,
 onContextVersion(fn) { listeners.context.add(fn); return () => listeners.context.delete(fn); },
 onAuthorizationVersion(fn) { listeners.authorization.add(fn); return () => listeners.authorization.delete(fn); },
 models: async () => ({ items: [] }),
 async request(area, path, options = {}) {
  const method = options.method || 'GET', url = new URL(path, 'https://fixture.example.test');
  control.calls.push({ area, path, method, body: clone(options.body || null), key: options.idempotencyKey || null, aborted: () => options.signal?.aborted });
  const respond = () => {
   if (url.pathname.endsWith('/authorization-context')) {
    if (control.failed) throw Object.assign(new Error('CONTROLLED_DENIAL'), { status: control.failed });
    const value = clone(actor().context);
    if (['member', 'auditor'].includes(control.kind) || control.expired) value.unit_authorization_bindings = [];
    if (control.revoked) { value.organization_authorization_version++; value.unit_authorization_bindings = []; }
    if (control.mismatch) value[control.mismatch] = control.mismatch === 'organization_id' ? newID : value[control.mismatch] + 1;
    return value;
   }
   if (method === 'GET' && url.pathname.endsWith('/units')) {
    const selected = url.searchParams.get('root_unit_id');
    if (!selected) return { structure_version: 4, items: allUnits() };
    if (selected === dto.root_a) return clone(actor().tree_a);
    if (selected === dto.root_c) return { structure_version: 4, items: allUnits().filter(unit => unit.unit_id === dto.root_c) };
    throw Object.assign(new Error('FORBIDDEN_SCOPE'), { status: 403 });
   }
   if (method === 'GET' && url.pathname.endsWith('/members')) {
    if (url.searchParams.get('unit_id') === dto.root_c) return { items: [], next_cursor: null };
    const value = clone(actor().members_a);
    if (control.forgedCrossMember) value.items.push({ ...value.items[0], user_id: dto.cross_id, display_name: 'Cross subtree member', unit_ids: [dto.child_a, dto.root_c] });
    if (control.pagination) { const second = Boolean(url.searchParams.get('cursor')); value.items = value.items.slice(second ? 1 : 0, second ? 2 : 1); value.next_cursor = second ? null : 'controlled-scope-page-2'; }
    return value;
   }
   if (method === 'GET' && url.pathname.endsWith('/role-bindings')) return { items: [], truncated: false };
   if (method === 'GET' && url.pathname.endsWith('/balance')) return { owner_type: 'ORGANIZATION', owner_id: dto.organization_id, wallet_id: newID, posted_microcredits: '0', reserved_microcredits: '0', available_microcredits: '0' };
   if (method === 'GET' && url.pathname.endsWith('/model-policy')) return { organization_id: dto.organization_id, version: 0, mode: 'ALLOW_ALL', model_ids: [], authorization_version: actor().context.organization_authorization_version };
   if (method === 'GET') return { items: [] };
   if (control.unknown) throw Object.assign(new Error('CONTROLLED_UNKNOWN'), { financeRequestState: 'WRITE_OBSERVED', status: 503 });
   const body = options.body;
   if (method === 'POST' && url.pathname.endsWith('/units')) return { unit: { unit_id: newID, organization_id: dto.organization_id, parent_id: body.parent_id, name: body.name, status: 'ACTIVE', version: 1 }, structure_version: 5 };
   if (url.pathname.endsWith('/invitations')) return { invitation_id: newID, organization_id: dto.organization_id, status: 'PENDING', unit_ids: body.unit_ids, expires_at: '2099-01-01T00:00:00Z', token: 'CONTROLLED_INVITATION' };
   if (url.pathname.endsWith('/units') && method === 'PUT') {
    const user = url.pathname.split('/').slice(-2)[0], member = actor().members_a.items.find(item => item.user_id === user);
    return { organization_id: dto.organization_id, member: { ...clone(member), version: body.expected_version + 1, unit_ids: body.unit_ids }, replayed: false };
   }
   throw new Error(`UNEXPECTED_MUTATION ${path}`);
  };
  if (control.delayed && (control.delayed === 'context' && url.pathname.endsWith('/authorization-context') || control.delayed === 'tree' && url.pathname.endsWith('/units') && url.searchParams.has('root_unit_id') || control.delayed === 'page' && url.searchParams.has('cursor'))) {
   return new Promise((resolve, reject) => { control.release = () => { try { resolve(respond()); } catch (error) { reject(error); } }; });
  }
  return respond();
 },
};
control.client = client; control.listeners = listeners;
control.copy = () => organizationCopy[control.language];
control.me = () => {
 const me = clone(actor().me);
 if (control.kind === 'admin') me.active_context.permissions.push('org.units.read', 'org.members.read', 'org.units.manage', 'org.members.manage');
 if (control.kind === 'member') me.active_context.permissions = ['org.roles.read'];
 return me;
};
control.render = () => {
 if (!control.currentMe || control.profileKind !== control.kind || control.profileEpoch !== client.financeSessionEpoch) { control.currentMe = control.me(); control.profileKind = control.kind; control.profileEpoch = client.financeSessionEpoch; }
 root.render(<OrganizationWorkspace client={client} me={control.currentMe} language={control.language} onChanged={() => { control.changed++; }} />);
};
Object.assign(control, JSON.parse(new URLSearchParams(location.search).get('configuration') || '{}'));
window.unitScope = control; control.render();
