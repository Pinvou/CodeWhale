import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { hasPermission } from './authorization-ui.js';
import { OrganizationClient, organizationID, organizationUnitIDs, organizationVersion } from './organization-client.js';
import { useOrganizationMemberPages } from './useOrganizationMemberPages.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { OrganizationUnitWorkspace } from './OrganizationUnitWorkspace.jsx';
import { OrganizationFinanceWorkspace } from './OrganizationFinanceWorkspace.jsx';
import { OrganizationClosedGrantsWorkspace } from './OrganizationClosedGrantsWorkspace.jsx';
import { OrganizationModelPolicyWorkspace } from './OrganizationModelPolicyWorkspace.jsx';
import { PersonalUsageWorkspace } from './PersonalUsageWorkspace.jsx';

const optionalID = value => value ? organizationID(value) : null;
const time = value => { if (!value) return null; const parsed = new Date(value); if (!Number.isFinite(parsed.getTime())) throw new Error('INVALID_ORGANIZATION_INPUT'); return parsed.toISOString(); };
const text = value => { if (!value.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return value.trim(); };
export function OrganizationWorkspace({ client, me, language, onChanged, scopeFailureGuard, includeFinance = true, includeUsage = true, dialogActions = false, purchaseFocusRequest }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const roleOptions = ['MEMBER', 'ORG_ADMIN', 'BILLING_ADMIN', 'AUDITOR', 'UNIT_ADMIN'].map(value => ({ value, label: organizationLabel(copy, 'Role', value) }));
 const grantRoleOptions = [...roleOptions, { value: 'ORG_MODEL_ADMIN', label: organizationLabel(copy, 'Role', 'ORG_MODEL_ADMIN') }];
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${client.financeSessionEpoch || 0}`;
 const [data, setData] = useState({ units: [], bindings: [], bindingsTruncated: false, structure: 0 });
 const memberPage = useOrganizationMemberPages({ api, org, owner, enabled: hasPermission(me, 'org.members.read') });
 const [error, setError] = useState(''), [loading, setLoading] = useState(false), [invitation, setInvitation] = useState(null);
 const generation = useRef(0);
 const can = permission => hasPermission(me, permission);
 const load = async (stamp, signal) => {
  const [units, bindings] = await Promise.all([
   can('org.units.read') ? api.units(org, signal) : { items: [], structure_version: 0 },
   can('org.roles.read') ? api.bindings(org, signal) : [],
  ]);
  if (stamp === generation.current) setData({ units: units.items, structure: units.structure_version, bindings, bindingsTruncated: Boolean(bindings.truncated) });
 };
 const refresh = async () => { void memberPage.refresh(); const stamp = ++generation.current; setLoading(true); setError(''); try { await load(stamp); } catch (reason) {
  if (stamp === generation.current) {
   setData(current => ({ ...current, bindings: [], bindingsTruncated: false }));
   setError(reason.status === 403 ? copy.permissions : copy.unavailable);
  }
 } finally { if (stamp === generation.current) setLoading(false); } };
 useEffect(() => { const stamp = ++generation.current, controller = new AbortController(); setData({ units: [], bindings: [], bindingsTruncated: false, structure: 0 }); setInvitation(null); setError(''); setLoading(true); void load(stamp, controller.signal).catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError(reason.status === 403 ? copy.permissions : copy.unavailable); }).finally(() => { if (stamp === generation.current) setLoading(false); }); return () => { generation.current++; controller.abort(); }; }, [api, owner]); // eslint-disable-line react-hooks/exhaustive-deps
 const units = data.units.filter(unit => unit.status === 'ACTIVE').map(unit => ({ value: unit.unit_id, label: unit.name }));
 const members = memberPage.items.filter(member => member.status === 'ACTIVE').map(member => ({ value: member.user_id, label: member.display_name || member.user_code || member.user_id }));
 const action = (scope, title, fields, prepare, execute, confirm = false, confirmed, target = org) => <OrganizationActionForm dialog={dialogActions} key={scope} client={client} owner={owner} copy={copy} scope={scope} title={title} fields={fields} prepare={prepare} action={execute} confirm={confirm} confirmationTarget={target} onConfirmed={result => { confirmed?.(result); void refresh(); void onChanged(); }} />;
 return <section><h2>{copy.title}</h2><p>{org}</p><button disabled={loading} onClick={() => void refresh()}>{copy.refresh}</button>{error && <p role="alert">{error}</p>}{loading && <p role="status">{copy.loading}</p>}
  {can('org.units.read') && <section><h3>{copy.units}</h3><ul>{data.units.map(unit => <li key={unit.unit_id}>{unit.name} · {unit.parent_id || copy.root} · {organizationLabel(copy, 'Status', unit.status)}</li>)}</ul></section>}
  {can('org.units.manage') && <div>{action('create-unit', copy.createUnit, [{ name: 'name', nameRule: 'unit' }, { name: 'parent', optional: true, options: units }], values => ({ name: text(values.name), parent_id: optionalID(values.parent) }), (facts, key) => api.createUnit(org, facts, key))}
   {action('move-unit', copy.moveUnit, [{ name: 'unit', options: units }, { name: 'parent', optional: true, options: units }], values => ({ unit: organizationID(values.unit), parent_id: optionalID(values.parent), expected_structure_version: organizationVersion(data.structure) }), (facts, key) => api.moveUnit(org, facts.unit, { parent_id: facts.parent_id, expected_structure_version: facts.expected_structure_version }, key))}</div>}
  {can('org.members.read') && <section><h3>{copy.members}</h3><p>{copy.membersPageHint}</p>{memberPage.loading && <p role="status">{copy.loading}</p>}{memberPage.error && <p role="alert">{copy[memberPage.error]}</p>}<button type="button" disabled={memberPage.loading} onClick={() => void memberPage.refresh()}>{copy.refresh}</button><button type="button" disabled={memberPage.loading || !memberPage.nextCursor} onClick={() => void memberPage.loadMore()}>{copy.loadMoreMembers}</button><table><thead><tr><th>{copy.user}</th><th>{copy.status}</th><th>{copy.roles}</th><th>{copy.memberUnits}<small className="ap-member-units-help">{copy.unscopedUnits}</small></th><th>{copy.version}</th></tr></thead><tbody>{memberPage.items.map(member => <tr key={member.user_id}><td>{member.display_name || member.user_code || member.user_id}</td><td>{organizationLabel(copy, 'Status', member.status)}</td><td>{member.roles.map(role => organizationLabel(copy, 'Role', role)).join(', ')}</td><td>{member.unit_ids.map(id => data.units.find(unit => unit.unit_id === id)?.name || id).join(', ') || copy.unassignedUnits}</td><td>{member.version}</td></tr>)}</tbody></table></section>}
  {can('org.members.manage') && <div>{action('invite', copy.invite, [{ name: 'targetType', options: ['USERNAME', 'PHONE', 'EMAIL', 'LINK'].map(value => ({ value, label: organizationLabel(copy, 'Target', value) })) }, { name: 'target', optional: true }, { name: 'role', options: roleOptions.filter(item => item.value !== 'UNIT_ADMIN' && (item.value === 'MEMBER' || can('org.roles.assign'))) }, { name: 'memberUnits', multiple: true, options: units }, { name: 'hours', type: 'number', initial: '24' }], values => { const hours = Number(values.hours); if (!Number.isSafeInteger(hours) || hours < 1 || hours > 720 || !values.targetType || !values.role) throw new Error('INVALID_ORGANIZATION_INPUT'); return { target_type: values.targetType, target: values.targetType === 'LINK' ? '' : text(values.target), roles: [values.role], unit_ids: organizationUnitIDs(values.memberUnits), expires_in_hours: hours }; }, (facts, key) => api.invite(org, facts, key), false, setInvitation)}
   {invitation && <div><p>{invitation.invitation_id} · {invitation.expires_at}</p>{invitation.token && <label>{copy.link}<input type="password" autoComplete="off" readOnly value={invitation.token} /></label>}{action('revoke-invitation', copy.revoke, [], () => ({ invitation: invitation.invitation_id }), (facts, key) => api.revokeInvitation(org, facts.invitation, key), true, undefined, invitation.invitation_id)}</div>}
   {action('replace-member-units', copy.replaceMemberUnits, [{ name: 'user', options: members }, { name: 'memberUnits', multiple: true, options: units }], values => { const user = organizationID(values.user), member = memberPage.items.find(item => item.user_id === user); return { user, unit_ids: organizationUnitIDs(values.memberUnits), expected_version: organizationVersion(member?.version) }; }, (facts, key) => api.replaceUnits(org, facts.user, facts.unit_ids, facts.expected_version, key), true)}
   {action('remove-member', copy.remove, [{ name: 'user', options: members }], values => ({ user: organizationID(values.user) }), (facts, key) => api.removeMember(org, facts.user, key), true)}</div>}
  {can('org.roles.assign') && action('replace-roles', copy.replaceRoles, [{ name: 'user', options: members }, { name: 'role', options: roleOptions.filter(item => item.value !== 'UNIT_ADMIN') }], values => ({ user: organizationID(values.user), roles: [text(values.role)] }), (facts, key) => api.replaceRoles(org, facts.user, facts.roles, key), true)}
  {can('org.roles.read') && <section><h3>{copy.bindings}</h3>{data.bindingsTruncated && <p role="status">{copy.bindingsTruncated}</p>}<ul>{data.bindings.map(binding => <li key={binding.binding_id}>{binding.user_id} · {organizationLabel(copy, 'Role', binding.role_code)} · {binding.unit_id || copy.root} · {organizationLabel(copy, 'Status', binding.status)} · {binding.expires_at || '—'}{can('org.roles.assign') && ['ACTIVE', 'SCHEDULED'].includes(binding.status) && binding.role_code !== 'OWNER' && action(`revoke-role:${binding.binding_id}`, copy.revoke, [], () => ({ id: binding.binding_id }), (facts, key) => api.revokeRole(org, facts.id, key), true, undefined, `${binding.user_id} · ${binding.role_code} · ${binding.binding_id}`)}</li>)}</ul></section>}
  {can('org.roles.assign') && action('grant-role', copy.grantRole, [{ name: 'user', options: members }, { name: 'role', options: grantRoleOptions }, { name: 'unit', optional: true, options: units }, { name: 'starts', type: 'datetime-local', optional: true }, { name: 'expires', type: 'datetime-local', optional: true }, { name: 'reason' }], values => { if ((values.role === 'UNIT_ADMIN') !== Boolean(values.unit)) throw new Error('INVALID_ORGANIZATION_INPUT'); return { user_id: organizationID(values.user), role_code: text(values.role), unit_id: optionalID(values.unit), starts_at: time(values.starts), expires_at: time(values.expires), reason: text(values.reason) }; }, (facts, key) => api.grantRole(org, facts, key))}
  {can('org.owner.transfer') && action('transfer-owner', copy.transfer, [{ name: 'user', label: 'newOwner', options: members }], values => ({ user: organizationID(values.user) }), (facts, key) => api.transfer(org, facts.user, key), true)}
  {(!can('org.units.manage') || !can('org.members.manage')) && <OrganizationUnitWorkspace api={api} me={me} owner={owner} language={language} onChanged={onChanged} scopeFailureGuard={scopeFailureGuard} />}
  {includeFinance && <OrganizationFinanceWorkspace client={client} me={me} language={language} purchaseFocusRequest={purchaseFocusRequest} />}
  <OrganizationClosedGrantsWorkspace key={owner} client={client} me={me} language={language} />
  <OrganizationModelPolicyWorkspace client={client} me={me} language={language} />
  {includeUsage && <PersonalUsageWorkspace client={client} me={me} language={language} />}
 </section>;
}
