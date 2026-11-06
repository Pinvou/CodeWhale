import { OrganizationAdminPortalWorkspace } from './OrganizationAdminPortalWorkspace.jsx';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy, adminLayoutCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { OrganizationAdminClient, organizationID, organizationUnitIDs, organizationVersion } from './organization-client.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { hasPermission } from './authorization-ui.js';

const text = value => { if (!value.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return value.trim(); };
const optionalID = value => value ? organizationID(value) : null;
export function OrganizationAdminWorkspace({ client, me, language, route }) {
 const copy = { ...organizationCopy[language], confirmationName: adminLayoutCopy[language].organization.confirmationName, notFound: adminLayoutCopy[language].organization.notFound, conflict: adminLayoutCopy[language].organization.conflict }, api = useMemo(() => new OrganizationAdminClient(client), [client]);
 const requested = new URLSearchParams(route?.search || '');
 const routeID = requested.get('organization_id') || '', routeQuery = requested.get('organization_q') || '', routeStatus = requested.get('organization_status') || '', routeCursor = requested.get('organization_cursor') || '';
 const memberQuery = requested.get('member_q') || '', memberRole = requested.get('member_role') || '', routeMemberCursor = requested.get('member_cursor') || '';
 const roles = ['MEMBER', 'ORG_ADMIN', 'BILLING_ADMIN', 'AUDITOR'].map(value => ({ value, label: organizationLabel(copy, 'Role', value) }));
 const [page, setPage] = useState(null), [selected, setSelected] = useState(routeID), [query, setQuery] = useState(routeQuery), [cursor, setCursor] = useState(''), [previous, setPrevious] = useState([]);
 const [detail, setDetail] = useState(null), [memberCursor, setMemberCursor] = useState(''), [memberPrevious, setMemberPrevious] = useState([]);
 const [error, setError] = useState(''), [listBusy, setListBusy] = useState(false), [detailBusy, setDetailBusy] = useState(false);
 const generation = useRef(0), selectionGeneration = useRef(0), confirmedRef = useRef(null);
 const busy = listBusy || detailBusy;
 const owner = `${me.user_id}:${me.active_context?.platform_authorization_version || 0}:${client.financeSessionEpoch || 0}`;
 const can = permission => hasPermission(me, `platform.organizations.${permission}`);
 const canRenameOrganization = can('profile.manage'), canRenameUnit = can('units.manage');
 // Unsubmitted name drafts survive a confirmed peer action's detail reload,
 // but never an actor, authority, client or selected-organization change.
 const nameDrafts = useMemo(() => new Map(), [api, owner, selected, canRenameOrganization, canRenameUnit]);
 const load = async (target = '', search = query) => { const stamp = ++generation.current; setListBusy(true); setError(''); try { const value = await api.list({ cursor: target, q: search, ...(route ? { status: routeStatus } : {}) }); if (stamp === generation.current) { setPage(value); setCursor(target); } } catch (reason) { if (stamp === generation.current) setError(reason.status === 403 ? copy.permissions : copy.unavailable); } finally { if (stamp === generation.current) setListBusy(false); } };
 const loadDetail = async (org = selected, target = '') => {
  if (!org) return;
  const stamp = ++selectionGeneration.current; setDetail(null); setError(''); setDetailBusy(true);
  if (route) {
   try {
    const summary = await api.summary(org);
    if (stamp !== selectionGeneration.current) return;
    if (summary.status === 'DELETED') { setDetail({ summary, units: { items: [], structure_version: 0 }, members: { items: [] }, readiness: null }); setMemberCursor(''); return; }
    const results = await Promise.allSettled([api.read(org, 'units'), api.members(org, target, undefined, { q: memberQuery, role: memberRole }), can('delete') && summary.status === 'DISABLED' ? api.read(org, 'deletion-readiness') : null]);
    if (stamp !== selectionGeneration.current) return;
    const [u,m,r] = results;
    setDetail({ summary, units: u.status === 'fulfilled' ? u.value : { items: [], structure_version: 0 }, unitsError: u.status === 'rejected' ? u.reason : null, members: m.status === 'fulfilled' ? m.value : { items: [] }, membersError: m.status === 'rejected' ? m.reason : null, readiness: r.status === 'fulfilled' ? r.value : null, readinessError: r.status === 'rejected' ? r.reason : null }); setMemberCursor(target);
   } catch (reason) { if (stamp === selectionGeneration.current) setError(reason.status === 403 ? copy.permissions : reason.status === 404 ? copy.notFound : reason.status === 409 ? copy.conflict : copy.unavailable); }
   finally { if (stamp === selectionGeneration.current) setDetailBusy(false); }
   return;
  }
  try { const [units, members, readiness] = await Promise.all([api.read(org, 'units'), api.members(org, target), can('delete') ? api.read(org, 'deletion-readiness') : null]); if (stamp === selectionGeneration.current) { setDetail({ units, members, readiness }); setMemberCursor(target); } }
  catch (reason) { if (stamp === selectionGeneration.current) setError(reason.status === 403 ? copy.permissions : copy.unavailable); }
  finally { if (stamp === selectionGeneration.current) setDetailBusy(false); }
 };
 useEffect(() => { setPage(null); if (!route) { setSelected(''); setDetail(null); } setQuery(route ? routeQuery : ''); void load(route ? routeCursor : '', route ? routeQuery : ''); return () => { generation.current++; }; }, [api, owner, routeQuery, routeStatus, routeCursor]); // eslint-disable-line react-hooks/exhaustive-deps
 useEffect(() => { if (route) setSelected(routeID); }, [routeID, Boolean(route)]);
 useEffect(() => { setMemberPrevious([]); }, [selected, memberQuery, memberRole]);
 useEffect(() => { setPrevious([]); }, [routeQuery, routeStatus]);
 useEffect(() => { if (selected) void loadDetail(selected, route ? routeMemberCursor : ''); return () => { selectionGeneration.current++; }; }, [selected, memberQuery, memberRole, routeMemberCursor]); // eslint-disable-line react-hooks/exhaustive-deps
 const confirmed = () => { void load(cursor); if (selected) void loadDetail(selected, memberCursor); };
 confirmedRef.current = confirmed;
 const action = (scope, title, fields, prepare, operation, target = '', confirm = false, presentation = {}) => {
  const organization = current?.name ? `${current.name} (${selected})` : selected;
  const unit = ['renameUnit', 'moveUnit', 'deleteUnit'].includes(operation) ? detail?.units.items.find(item => item.unit_id === target) : null;
  const member = ['roles', 'units', 'remove'].includes(operation) ? detail?.members.items.find(item => item.user_id === target) : null;
  const targetName = unit?.name || member?.display_name || member?.user_code;
  const actionTarget = targetName ? `${targetName} (${target})` : target;
  const description = operation === 'deleteUnit' ? copy.deleteUnitDescription : operation === 'status' ? current?.status === 'ACTIVE' ? copy.disableOrganizationDescription : copy.enableOrganizationDescription : '';
  return <OrganizationActionForm dialog={Boolean(route)} appearance={route ? 'admin' : 'default'} dangerous={['delete', 'deleteUnit', 'remove', 'transfer'].includes(operation) || operation === 'status' && current?.status === 'ACTIVE'} primaryTrigger={['create', 'unit', 'add'].includes(operation)} submitLabel={operation === 'rename' || operation === 'renameUnit' ? copy.saveChanges : ''} key={scope === 'create' ? scope : `${selected}:${scope}`} client={client} owner={scope === 'create' ? owner : `${owner}:${selected}`} copy={copy} scope={scope} editBeforeSubmit={operation === 'rename' || operation === 'renameUnit'} editDrafts={operation === 'rename' || operation === 'renameUnit' ? nameDrafts : undefined} confirmationTarget={scope === 'create' ? '' : [organization, actionTarget].filter(Boolean).join(' · ')} description={description} title={title} fields={fields.map(field => ({ ...field, lookup: Boolean(route && field.lookup && hasPermission(me, 'platform.users.read')) }))} prepare={prepare} action={(facts, key) => api.write(operation === 'create' ? '' : selected, operation, facts, key, target)} onConfirmed={() => confirmedRef.current?.()} confirm={confirm} {...presentation} />;
 };
 const current = route && detail?.summary?.organization_id === selected ? detail.summary : page?.items.find(item => item.organization_id === selected);
 const units = detail?.units.items.filter(item => item.status === 'ACTIVE').map(item => ({ value: item.unit_id, label: item.name })) || [];
 if (route) return <OrganizationAdminPortalWorkspace {...{ client, me, language, copy, route, page, query, setQuery, selected, current, detail, error, listBusy, detailBusy, roles, units, memberCursor, memberPrevious, setMemberPrevious, previous, setPrevious, can, action, memberQuery, memberRole }} onRefresh={() => { if (route && detail?.membersError?.status === 410 && routeMemberCursor) route.navigate('organizations', { member_cursor: null }, 'replace'); else if (selected) void loadDetail(selected, memberCursor); else void load(route ? routeCursor : cursor, route ? routeQuery : query); }} />;
 return <section className="adm-panel"><h2>{copy.title}</h2><form onSubmit={event => { event.preventDefault(); setPrevious([]); void load('', query); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><button disabled={busy}>{copy.refresh}</button></form>{error && <p role="alert">{error}</p>}{busy && <p role="status">{copy.loading}</p>}
  {page?.items.map(org => <button key={org.organization_id} disabled={busy} aria-pressed={org.organization_id === selected} onClick={() => setSelected(org.organization_id)}>{org.name} · {organizationLabel(copy, 'Status', org.status)}</button>)}
  <nav><button disabled={busy || !previous.length} onClick={() => { const next = [...previous], target = next.pop(); setPrevious(next); void load(target); }}>{copy.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => { setPrevious(value => [...value, cursor]); void load(page.next_cursor); }}>{copy.next}</button></nav>
  {can('create') && action('create', copy.create, [{ name: 'name' }, { name: 'owner' }], values => ({ name: text(values.name), owner_user_id: organizationID(values.owner) }), 'create')}
  {current && <div key={`${owner}:${selected}`}><h3>{current.name}</h3><p>{current.organization_id} · {organizationLabel(copy, 'Status', current.status)} · {current.version}</p>
   {canRenameOrganization && action('rename', copy.rename, [{ name: 'name', initial: current.name }], values => ({ name: text(values.name) }), 'rename')}
   {can('status.manage') && ['ACTIVE', 'DISABLED'].includes(current.status) && action('status', current.status === 'ACTIVE' ? copy.disable : copy.enable, [], () => ({ status: current.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE' }), 'status', '', true)}
   {can('delete') && <div><button disabled={busy} onClick={() => void loadDetail()}>{copy.readiness}</button>{detail?.readiness && <ul>{detail.readiness.blockers?.map((item, index) => <li key={index}>{typeof item === 'string' ? item : item.code || item.resource_type}</li>)}</ul>}{action('delete', copy.delete, [{ name: 'reason' }], values => ({ reason: text(values.reason) }), 'delete', '', true)}</div>}
   {detail && <><h4>{copy.units}</h4>{detail.units.items.map(unit => <div key={unit.unit_id}><p>{unit.name} · {unit.parent_id || copy.root} · {organizationLabel(copy, 'Status', unit.status)}</p>{canRenameUnit && <>{action(`rename-unit:${unit.unit_id}`, copy.renameUnit, [{ name: 'name', initial: unit.name }], values => ({ name: text(values.name) }), 'renameUnit', unit.unit_id)}{action(`move-unit:${unit.unit_id}`, copy.moveUnit, [{ name: 'parent', optional: true, options: units }], values => ({ parent_id: optionalID(values.parent), expected_structure_version: organizationVersion(detail.units.structure_version) }), 'moveUnit', unit.unit_id)}{action(`delete-unit:${unit.unit_id}`, copy.deleteUnit, [], () => ({}), 'deleteUnit', unit.unit_id, true)}</>}</div>)}
    {canRenameUnit && action('create-unit', copy.createUnit, [{ name: 'name' }, { name: 'parent', optional: true, options: units }], values => ({ name: text(values.name), parent_id: optionalID(values.parent) }), 'unit')}
    <h4>{copy.members}</h4><p className="ap-member-units-help">{copy.unscopedUnits}</p>{detail.members.items.map(member => <div key={member.user_id}><p>{member.display_name || member.user_code || member.user_id} · {organizationLabel(copy, 'Status', member.status)} · {member.roles.map(role => organizationLabel(copy, 'Role', role)).join(', ')} · {member.version}</p><p>{copy.memberUnits}: {member.unit_ids.map(id => detail.units.items.find(unit => unit.unit_id === id)?.name || id).join(', ') || copy.unassignedUnits}</p>{can('members.manage') && <>{action(`roles:${member.user_id}`, copy.replaceRoles, [{ name: 'role', options: roles }, { name: 'unit', label: 'roleUnit', optional: true, options: units }], values => ({ roles: [text(values.role)], unit_admin_unit_ids: values.unit ? [organizationID(values.unit)] : [] }), 'roles', member.user_id, true)}{member.status === 'ACTIVE' && action(`member-units:${member.user_id}`, copy.replaceMemberUnits, [{ name: 'memberUnits', multiple: true, options: units, initial: member.unit_ids }], values => ({ unit_ids: organizationUnitIDs(values.memberUnits), expected_version: organizationVersion(member.version) }), 'units', member.user_id, true)}{action(`remove:${member.user_id}`, copy.remove, [], () => ({}), 'remove', member.user_id, true)}</>}</div>)}
    <nav><button disabled={busy || !memberPrevious.length} onClick={() => { const next = [...memberPrevious], target = next.pop(); setMemberPrevious(next); void loadDetail(selected, target); }}>{copy.previous}</button><button disabled={busy || !detail.members.next_cursor} onClick={() => { setMemberPrevious(value => [...value, memberCursor]); void loadDetail(selected, detail.members.next_cursor); }}>{copy.next}</button></nav>
   </>}
   {can('members.manage') && action('add-member', copy.addMember, [{ name: 'user' }, { name: 'role', options: roles }, { name: 'unit', label: 'roleUnit', optional: true, options: units }, { name: 'memberUnits', multiple: true, options: units }], values => ({ user_id: organizationID(values.user), roles: [text(values.role)], unit_admin_unit_ids: values.unit ? [organizationID(values.unit)] : [], unit_ids: organizationUnitIDs(values.memberUnits) }), 'add')}
   {can('owner.transfer') && action('transfer', copy.transfer, [{ name: 'newOwner' }], values => ({ new_owner_user_id: organizationID(values.newOwner) }), 'transfer', '', true)}
  </div>}
 </section>;
}
