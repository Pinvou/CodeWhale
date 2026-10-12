import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { OrganizationClient, organizationID, organizationUnitIDs, organizationVersion } from './organization-client.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { OrganizationFinanceWorkspace } from './OrganizationFinanceWorkspace.jsx';
import { OrganizationModelPolicyWorkspace } from './OrganizationModelPolicyWorkspace.jsx';
import { PersonalUsageWorkspace } from './PersonalUsageWorkspace.jsx';

const roleOptions = ['MEMBER', 'ORG_ADMIN', 'BILLING_ADMIN', 'AUDITOR', 'UNIT_ADMIN'].map(value => ({ value, label: value }));
const optionalID = value => value ? organizationID(value) : null;
const time = value => { if (!value) return null; const parsed = new Date(value); if (!Number.isFinite(parsed.getTime())) throw new Error('INVALID_ORGANIZATION_INPUT'); return parsed.toISOString(); };
const text = value => { if (!value.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return value.trim(); };
export function OrganizationWorkspace({ client, me, language, onChanged }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${client.financeSessionEpoch || 0}`;
 const [data, setData] = useState({ units: [], members: [], bindings: [], structure: 0 });
 const [error, setError] = useState(''), [loading, setLoading] = useState(false), [invitation, setInvitation] = useState(null);
 const generation = useRef(0);
 const can = permission => hasPermission(me, permission);
 const load = async (stamp, signal) => {
  const [units, members, bindings] = await Promise.all([
   can('org.units.read') ? api.units(org, signal) : { items: [], structure_version: 0 },
   can('org.members.read') ? api.members(org, signal) : [],
   can('org.roles.read') ? api.bindings(org, signal) : [],
  ]);
  if (stamp === generation.current) setData({ units: units.items, structure: units.structure_version, members, bindings });
 };
 const refresh = async () => { const stamp = ++generation.current; setLoading(true); setError(''); try { await load(stamp); } catch { if (stamp === generation.current) setError(copy.unavailable); } finally { if (stamp === generation.current) setLoading(false); } };
 useEffect(() => { const stamp = ++generation.current, controller = new AbortController(); setData({ units: [], members: [], bindings: [], structure: 0 }); setInvitation(null); setError(''); setLoading(true); void load(stamp, controller.signal).catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError(copy.unavailable); }).finally(() => { if (stamp === generation.current) setLoading(false); }); return () => { generation.current++; controller.abort(); }; }, [api, owner]); // eslint-disable-line react-hooks/exhaustive-deps
 const units = data.units.filter(unit => unit.status === 'ACTIVE').map(unit => ({ value: unit.unit_id, label: unit.name }));
 const members = data.members.filter(member => member.status === 'ACTIVE').map(member => ({ value: member.user_id, label: member.display_name || member.user_code || member.user_id }));
 const action = (scope, title, fields, prepare, execute, confirm = false, confirmed) => <OrganizationActionForm key={scope} client={client} owner={owner} copy={copy} scope={scope} title={title} fields={fields} prepare={prepare} action={execute} confirm={confirm} onConfirmed={result => { confirmed?.(result); void refresh(); void onChanged(); }} />;
 return <section><h2>{copy.title}</h2><p>{org}</p><button disabled={loading} onClick={() => void refresh()}>{copy.refresh}</button>{error && <p role="alert">{error}</p>}{loading && <p role="status">{copy.loading}</p>}
  {can('org.units.read') && <section><h3>{copy.units}</h3><ul>{data.units.map(unit => <li key={unit.unit_id}>{unit.name} · {unit.parent_id || copy.root} · {unit.status}</li>)}</ul></section>}
  {can('org.units.manage') && <div>{action('create-unit', copy.createUnit, [{ name: 'name' }, { name: 'parent', optional: true, options: units }], values => ({ name: text(values.name), parent_id: optionalID(values.parent) }), (facts, key) => api.createUnit(org, facts, key))}
   {action('move-unit', copy.moveUnit, [{ name: 'unit', options: units }, { name: 'parent', optional: true, options: units }], values => ({ unit: organizationID(values.unit), parent_id: optionalID(values.parent), expected_structure_version: organizationVersion(data.structure) }), (facts, key) => api.moveUnit(org, facts.unit, { parent_id: facts.parent_id, expected_structure_version: facts.expected_structure_version }, key))}</div>}
  {can('org.members.read') && <section><h3>{copy.members}</h3><table><thead><tr><th>{copy.user}</th><th>{copy.status}</th><th>{copy.roles}</th><th>{copy.memberUnits}</th><th>{copy.version}</th></tr></thead><tbody>{data.members.map(member => <tr key={member.user_id}><td>{member.display_name || member.user_code || member.user_id}</td><td>{member.status}</td><td>{member.roles.join(', ')}</td><td>{member.unit_ids.map(id => data.units.find(unit => unit.unit_id === id)?.name || id).join(', ') || copy.unscopedUnits}</td><td>{member.version}</td></tr>)}</tbody></table></section>}
  {can('org.members.manage') && <div>{action('invite', copy.invite, [{ name: 'targetType', options: ['USERNAME', 'PHONE', 'EMAIL', 'LINK'].map(value => ({ value, label: value })) }, { name: 'target', optional: true }, { name: 'role', options: roleOptions.filter(item => item.value !== 'UNIT_ADMIN' && (item.value === 'MEMBER' || can('org.roles.assign'))) }, { name: 'memberUnits', multiple: true, options: units }, { name: 'hours', type: 'number', initial: '24' }], values => { const hours = Number(values.hours); if (!Number.isSafeInteger(hours) || hours < 1 || hours > 720 || !values.targetType || !values.role) throw new Error('INVALID_ORGANIZATION_INPUT'); return { target_type: values.targetType, target: values.targetType === 'LINK' ? '' : text(values.target), roles: [values.role], unit_ids: organizationUnitIDs(values.memberUnits), expires_in_hours: hours }; }, (facts, key) => api.invite(org, facts, key), false, setInvitation)}
   {invitation && <div><p>{invitation.invitation_id} · {invitation.expires_at}</p>{invitation.token && <label>{copy.link}<input type="password" autoComplete="off" readOnly value={invitation.token} /></label>}{action('revoke-invitation', copy.revoke, [], () => ({ invitation: invitation.invitation_id }), (facts, key) => api.revokeInvitation(org, facts.invitation, key), true)}</div>}
   {action('replace-member-units', copy.replaceMemberUnits, [{ name: 'user', options: members }, { name: 'memberUnits', multiple: true, options: units }], values => { const user = organizationID(values.user), member = data.members.find(item => item.user_id === user); return { user, unit_ids: organizationUnitIDs(values.memberUnits), expected_version: organizationVersion(member?.version) }; }, (facts, key) => api.replaceUnits(org, facts.user, facts.unit_ids, facts.expected_version, key), true)}
   {action('remove-member', copy.remove, [{ name: 'user', options: members }], values => ({ user: organizationID(values.user) }), (facts, key) => api.removeMember(org, facts.user, key), true)}</div>}
  {can('org.roles.assign') && action('replace-roles', copy.replaceRoles, [{ name: 'user', options: members }, { name: 'role', options: roleOptions.filter(item => item.value !== 'UNIT_ADMIN') }], values => ({ user: organizationID(values.user), roles: [text(values.role)] }), (facts, key) => api.replaceRoles(org, facts.user, facts.roles, key), true)}
  {can('org.roles.read') && <section><h3>{copy.bindings}</h3><ul>{data.bindings.map(binding => <li key={binding.binding_id}>{binding.user_id} · {binding.role_code} · {binding.unit_id || copy.root} · {binding.status} · {binding.expires_at || '—'}{can('org.roles.assign') && binding.status === 'ACTIVE' && binding.role_code !== 'OWNER' && action(`revoke-role:${binding.binding_id}`, copy.revoke, [], () => ({ id: binding.binding_id }), (facts, key) => api.revokeRole(org, facts.id, key), true)}</li>)}</ul></section>}
  {can('org.roles.assign') && action('grant-role', copy.grantRole, [{ name: 'user', options: members }, { name: 'role', options: roleOptions }, { name: 'unit', optional: true, options: units }, { name: 'starts', type: 'datetime-local', optional: true }, { name: 'expires', type: 'datetime-local', optional: true }, { name: 'reason' }], values => { if ((values.role === 'UNIT_ADMIN') !== Boolean(values.unit)) throw new Error('INVALID_ORGANIZATION_INPUT'); return { user_id: organizationID(values.user), role_code: text(values.role), unit_id: optionalID(values.unit), starts_at: time(values.starts), expires_at: time(values.expires), reason: text(values.reason) }; }, (facts, key) => api.grantRole(org, facts, key))}
  {can('org.owner.transfer') && action('transfer-owner', copy.transfer, [{ name: 'user', label: 'newOwner', options: members }], values => ({ user: organizationID(values.user) }), (facts, key) => api.transfer(org, facts.user, key), true)}
  <OrganizationFinanceWorkspace client={client} me={me} language={language} />
  <OrganizationModelPolicyWorkspace client={client} me={me} language={language} />
  <PersonalUsageWorkspace client={client} me={me} language={language} />
 </section>;
}
