import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { hasPermission } from './authorization-ui.js';
import { organizationID, organizationUnitIDs, organizationVersion } from './organization-client.js';
import { createOrganizationScopeFailureGuard, hasUnitScopePermission, memberWithinUnitScope, organizationScopeRoots, requireScopedMemberMutation, scopedUnitIDs } from './organization-scope.js';
import { useOrganizationScope } from './useOrganizationScope.js';
import { useOrganizationMemberPages } from './useOrganizationMemberPages.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';

const input = () => { throw new Error('INVALID_ORGANIZATION_INPUT'); };
const requiredText = value => value.trim() || input();
const retired = () => { throw Object.assign(new Error('ORGANIZATION_SCOPE_CHANGED'), { status: 403, financeRequestState: 'NOT_DISPATCHED' }); };

// Kept alongside the existing organization-wide view: an Auditor+Unit Admin
// can still read the organization, but writes remain in one explicit subtree.
export function OrganizationUnitWorkspace({ api, me, owner, language, onChanged, scopeFailureGuard }) {
 const copy = organizationCopy[language];
 const [selection, setSelection] = useState({ owner: '', root: '' });
 const failures = useRef(null);
 if (!failures.current) failures.current = createOrganizationScopeFailureGuard();
 const failureGuard = scopeFailureGuard || failures.current;
 const [, refreshFailure] = useState(0);
 const failureIdentity = JSON.stringify([owner, me.user_id, me.active_context.organization_id, me.active_context.context_version, me.active_context.organization_authorization_version, api.client.financeSessionEpoch || 0]);
 const scope = useOrganizationScope(api, me, owner, onChanged, failureGuard, failureIdentity);
 const roots = organizationScopeRoots(scope.context);
 const root = selection.owner === owner && roots.includes(selection.root) ? selection.root : roots[0] || '';
 const selected = useRef(root); selected.current = root;
 const context = scope.context;
 const assertCurrent = () => { if (!scope.isCurrent(context) || selected.current !== root || failureGuard.isDenied(failureIdentity, root)) retired(); };
 const permissionDenied = failureGuard.isDenied(failureIdentity, root);
 const onPermissionDenied = () => {
  if (!scope.isCurrent(context) || selected.current !== root) return;
  const refreshAuthority = failureGuard.deny(failureIdentity, root);
  refreshFailure(value => value + 1);
  if (refreshAuthority) { void scope.refresh(); void onChanged(); }
 };
 const retryScope = () => { failureGuard.retry(failureIdentity, root); refreshFailure(value => value + 1); void scope.refresh(true); };
 if (!roots.length && !scope.error) return null;
 return <section className="ap-org-unit-scope"><h3>{copy.unitScopeTitle}</h3><p>{copy.unitScopeHint}</p>
  <button type="button" disabled={scope.loading} onClick={retryScope}>{copy.refresh}</button>
  {(scope.error || permissionDenied) && <p role="alert">{copy[scope.error || 'permissions']}</p>}
  {root && <><label>{copy.unitScope}<select value={root} onChange={event => { selected.current = event.target.value; setSelection({ owner, root: event.target.value }); }}>
   {roots.map(id => <option key={id} value={id}>{id}</option>)}
  </select></label>
   {!permissionDenied && <ScopedDirectory key={`${owner}:${root}:${scope.revision}`} api={api} me={me} owner={`${owner}:unit:${root}`} root={root} context={context} copy={copy} assertCurrent={assertCurrent} onPermissionDenied={onPermissionDenied} onChanged={() => { void scope.refresh(); void onChanged(); }} />}
  </>}
 </section>;
}

function ScopedDirectory({ api, me, owner, root, context, copy, assertCurrent, onChanged, onPermissionDenied }) {
 const org = me.active_context.organization_id;
 const [tree, setTree] = useState(null), [error, setError] = useState(''), [invitation, setInvitation] = useState(null);
 const live = useRef(true);
 useEffect(() => {
  const abort = new AbortController(); live.current = true;
  void api.units(org, abort.signal, root).then(value => {
   assertCurrent(); scopedUnitIDs(value, root); if (live.current) setTree(value);
  }).catch(reason => { if (live.current && reason.name !== 'AbortError') { setTree(null); setError(reason.status === 403 ? 'permissions' : 'unavailable'); } });
  return () => { live.current = false; abort.abort(); };
 }, [api, org, root]); // eslint-disable-line react-hooks/exhaustive-deps
 const ids = useMemo(() => tree ? scopedUnitIDs(tree, root) : new Set(), [tree, root]);
 const can = permission => hasUnitScopePermission(context, root, permission);
 const page = useOrganizationMemberPages({ api, org, owner, unitID: root, enabled: Boolean(tree) && can('org.members.read') });
 useEffect(() => { if (page.error === 'permissions') onPermissionDenied(); }, [page.error]); // eslint-disable-line react-hooks/exhaustive-deps
 const units = (tree?.items || []).map(unit => ({ value: unit.unit_id, label: unit.name }));
 const writableMembers = page.items.filter(member => can('org.members.manage') && memberWithinUnitScope(member, ids));
 const members = writableMembers.map(member => ({ value: member.user_id, label: member.display_name || member.user_code || member.user_id }));
 const target = user => {
  const member = page.items.find(item => item.user_id === organizationID(user));
  requireScopedMemberMutation(context, root, ids, member); return member;
 };
 const guarded = fn => (...args) => { assertCurrent(); if (!live.current || !tree) retired(); return fn(...args); };
 const mutate = execute => guarded(async (...args) => { try { return await execute(...args); } catch (reason) { if (reason.status === 403) onPermissionDenied(); throw reason; } });
 const action = (name, title, fields, prepare, execute, confirm = false, confirmed) => <OrganizationActionForm key={name} client={api.client} owner={owner} copy={{ ...copy, unscopedUnits: name === 'invite' ? copy.unitScopeInviteUnitsHint : copy.unitScopeAssignmentHint }} scope={`unit:${root}:${name}`} title={title} fields={fields} prepare={guarded(prepare)} action={mutate(execute)} confirm={confirm} confirmationTarget={root} onConfirmed={value => { confirmed?.(value); if (!confirmed) onChanged(); }} />;
 return <div>{error && <p role="alert">{copy[error]}</p>}{!tree && !error && <p role="status">{copy.loading}</p>}
  {tree && <><h4>{copy.units}</h4><ul>{tree.items.map(unit => <li key={unit.unit_id}>{unit.name} · {unit.unit_id === root ? copy.unitScopeRoot : unit.parent_id}</li>)}</ul>
   {!hasPermission(me, 'org.units.manage') && can('org.units.manage') && <>
    {action('create', copy.createUnit, [{ name: 'name', nameRule: 'unit' }, { name: 'parent', options: units }], values => { if (!ids.has(values.parent)) input(); return { name: requiredText(values.name), parent_id: organizationID(values.parent) }; }, (facts, key) => api.createUnit(org, facts, key))}
    {action('move', copy.moveUnit, [{ name: 'unit', options: units.filter(item => item.value !== root) }, { name: 'parent', options: units }], values => { if (!ids.has(values.unit) || values.unit === root || !ids.has(values.parent)) input(); return { unit: organizationID(values.unit), parent_id: organizationID(values.parent), expected_structure_version: organizationVersion(tree.structure_version) }; }, (facts, key) => api.moveUnit(org, facts.unit, { parent_id: facts.parent_id, expected_structure_version: facts.expected_structure_version }, key))}
   </>}
   {can('org.members.read') && <section><h4>{copy.members}</h4><p>{copy.unitScopeMembersHint}</p>
    {page.loading && <p role="status">{copy.loading}</p>}{page.error && <p role="alert">{copy[page.error]}</p>}
    <button type="button" disabled={page.loading} onClick={() => void page.refresh()}>{copy.refresh}</button><button type="button" disabled={page.loading || !page.nextCursor} onClick={() => void page.loadMore()}>{copy.loadMoreMembers}</button>
    <ul>{page.items.map(member => <li key={member.user_id}>{member.display_name || member.user_code || member.user_id} · {member.unit_ids.map(id => tree.items.find(unit => unit.unit_id === id)?.name || id).join(', ')} · {member.roles.map(role => organizationLabel(copy, 'Role', role)).join(', ')}</li>)}</ul>
   </section>}
   {!hasPermission(me, 'org.members.manage') && can('org.members.manage') && <>
    {action('invite', copy.invite, [{ name: 'targetType', options: ['USERNAME', 'PHONE', 'EMAIL', 'LINK'].map(value => ({ value, label: organizationLabel(copy, 'Target', value) })) }, { name: 'target', optional: true }, { name: 'memberUnits', multiple: true, options: units }, { name: 'hours', type: 'number', initial: '24' }], values => {
     const assigned = organizationUnitIDs(values.memberUnits), hours = Number(values.hours);
     if (!assigned.length || assigned.some(id => !ids.has(id)) || !Number.isSafeInteger(hours) || hours < 1 || hours > 720 || !['USERNAME', 'PHONE', 'EMAIL', 'LINK'].includes(values.targetType)) input();
     return { target_type: values.targetType, target: values.targetType === 'LINK' ? '' : requiredText(values.target), roles: ['MEMBER'], unit_ids: assigned, expires_in_hours: hours };
    }, (facts, key) => api.invite(org, facts, key), false, setInvitation)}
    {invitation && <div><p>{invitation.invitation_id} · {invitation.expires_at}</p><p>{copy.unitScopeInvitationHint}</p>{invitation.token && <label>{copy.link}<input type="password" autoComplete="off" readOnly value={invitation.token} /></label>}</div>}
    {action('replace-member-units', copy.replaceMemberUnits, [{ name: 'user', options: members }, { name: 'memberUnits', multiple: true, options: units }], values => { const member = target(values.user), assigned = organizationUnitIDs(values.memberUnits); requireScopedMemberMutation(context, root, ids, member, assigned); return { user: member.user_id, unit_ids: assigned, expected_version: organizationVersion(member.version) }; }, (facts, key) => api.replaceUnits(org, facts.user, facts.unit_ids, facts.expected_version, key), true)}
    {action('remove-member', copy.remove, [{ name: 'user', options: members.filter(option => !writableMembers.find(member => member.user_id === option.value)?.roles.includes('OWNER')) }], values => { const member = target(values.user); if (member.roles.includes('OWNER')) input(); return { user: member.user_id }; }, (facts, key) => api.removeMember(org, facts.user, key), true)}
   </>}
  </>}
 </div>;
}
