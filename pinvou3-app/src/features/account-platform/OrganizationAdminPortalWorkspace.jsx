import React, { useEffect, useId, useState } from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import { adminOrganizationPageLimit, organizationID, organizationUnitIDs, organizationVersion } from './organization-client.js';
import { organizationLabel } from './organization-labels.js';
import { RetainedWorkspace } from './RetainedWorkspace.jsx';
import { OrganizationMemberMenu } from './OrganizationMemberMenu.jsx';
import { OrganizationOverviewSummary } from './OrganizationOverviewSummary.jsx';
import { AdminIdentifier } from './AdminIdentifier.jsx';
import { adminRouteLocation } from './admin-sections.js';

const text = value => { if (!value.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return value.trim(); };
const optionalID = value => value ? organizationID(value) : null;

// Match the Account directory filter whitespace when presenting accepted data.
// This does not rewrite any request, URL, or history entry.
const trimDirectoryValue = value => value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
const directoryStatusLabelValue = value => trimDirectoryValue(value).toUpperCase();

export function OrganizationAdminPortalWorkspace({ client, me, acceptedSessionEpoch, language, copy, route, page, directoryCursor, directoryResultScope, query, setQuery, selected, current, detail, directoryReadError, overviewFeedback, detailReadError, listBusy, detailBusy, roles, units, memberCursor, memberPrevious, setMemberPrevious, previous, setPrevious, can, action, memberQuery, memberRole, onRefresh, nameFeedback, nameFeedbackKind, renamedUnit, renamedUnitNode }) {
  const words = adminLayoutCopy[language], labels = words.organization, params = new URLSearchParams(route.search);
  const directoryPaginationNote = useId();
  const [memberSearch, setMemberSearch] = useState(memberQuery), [role, setRole] = useState(memberRole);
  useEffect(() => { setMemberSearch(memberQuery); setRole(memberRole); }, [memberQuery, memberRole]);
  const status = params.get('organization_status') || '';
  const acceptedCursor = trimDirectoryValue(directoryCursor || '');
  const appliedQuery = directoryResultScope ? trimDirectoryValue(directoryResultScope.query) : '';
  const appliedStatus = directoryResultScope ? directoryStatusLabelValue(directoryResultScope.status) : '';
  const directoryScope = directoryResultScope ? JSON.stringify([appliedQuery, appliedStatus]) : null;
  const requestedScope = JSON.stringify([trimDirectoryValue(params.get('organization_q') || ''), directoryStatusLabelValue(status)]);
  const requestedCursor = trimDirectoryValue(params.get('organization_cursor') || '');
  const previousFrom = page ? acceptedCursor : directoryReadError && directoryScope === requestedScope ? requestedCursor : '';
  const [knownPredecessors, setKnownPredecessors] = useState({ scope: '', cursors: new Map() });
  const previousTarget = previous.at(-1);
  // The existing stack can survive a browser history change. Do not offer
  // its top entry as the previous batch without a known adjacent transition.
  const canShowPrevious = Boolean(directoryScope && previousFrom && previousTarget !== undefined && knownPredecessors.scope === directoryScope
    && knownPredecessors.cursors.has(previousFrom) && knownPredecessors.cursors.get(previousFrom) === trimDirectoryValue(previousTarget));
  const navigate = updates => route.navigate('organizations', updates);
  const requestedID = params.get('organization_id') || '';
  const error = requestedID ? detailReadError : directoryReadError;
  const tabs = ['overview', 'members', 'units', 'lifecycle'];
  const active = tabs.includes(params.get('organization_tab')) ? params.get('organization_tab') : 'overview';
  const busy = listBusy || detailBusy;
  const identifierPlatformVersion = me.platform_authorization_version || me.active_context?.platform_authorization_version || me.authorization_version || 0;
  const identifierSessionEpoch = acceptedSessionEpoch === undefined ? client.financeSessionEpoch || 0 : acceptedSessionEpoch;
  const identifierOwner = `${me.user_id}:${identifierPlatformVersion}:${identifierSessionEpoch}`;
  const organizationLink = id => {
    const updates = { organization_id: id, organization_tab: 'overview', member_cursor: null };
    return {
      href: adminRouteLocation(globalThis.location?.search ?? route.search, 'organizations', updates, globalThis.location?.pathname),
      onClick: event => {
        if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        event.preventDefault(); navigate(updates);
      },
    };
  };
  const directory = <><div className="adm-organization-toolbar adm-section-toolbar"><form className="adm-filter-row" onSubmit={event => { event.preventDefault(); setPrevious([]); if ((params.get('organization_q') || '') === query.trim() && !params.get('organization_cursor')) onRefresh(); else navigate({ organization_q: query.trim() || null, organization_cursor: null, organization_id: null }); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><label>{copy.status}<select value={directoryStatusLabelValue(status)} onChange={event => { setPrevious([]); navigate({ organization_status: event.target.value || null, organization_cursor: null, organization_id: null }); }}><option value="">{labels.allStatuses}</option>{['ACTIVE', 'DISABLED', 'DELETED'].map(value => <option key={value} value={value}>{organizationLabel(copy, 'Status', value)}</option>)}</select></label><button type="submit" disabled={busy}>{copy.search}</button><button type="button" disabled={busy} onClick={() => { setQuery(''); setPrevious([]); if (!(params.get('organization_q') || '') && !status && !params.get('organization_cursor')) onRefresh(); else navigate({ organization_q: null, organization_status: null, organization_cursor: null, organization_id: null }); }}>{labels.resetFilters}</button></form>{can('create') && action('create', copy.create, [{ name: 'name' }, { name: 'owner', lookup: true }], values => ({ name: text(values.name), owner_user_id: organizationID(values.owner) }), 'create')}</div>
    <div className="adm-organization-page-summary">
      {page && <p className="adm-organization-page-count"><strong>{labels.pageCount(page.items.length)}</strong><span>{acceptedCursor ? labels.directoryPagination.later : labels.directoryPagination.first}</span></p>}
      {page && directoryReadError && !listBusy && <p>{labels.directoryPagination.previousResult}</p>}
      {page && directoryResultScope && <p><strong>{labels.directoryPagination.appliedFilters}</strong>{!appliedQuery && !appliedStatus && <span>{labels.directoryPagination.noFilters}</span>}
        {appliedQuery && <span>{labels.directoryPagination.appliedQuery(appliedQuery)}</span>}
        {appliedQuery && appliedStatus && ' · '}{appliedStatus && <span>{labels.directoryPagination.appliedStatus(organizationLabel(copy, 'Status', appliedStatus))}</span>}</p>}
      <p id={directoryPaginationNote}>{labels.directoryPagination.limits(adminOrganizationPageLimit)}</p>
      {(page ? acceptedCursor : directoryReadError && requestedCursor) && !busy && !canShowPrevious && <p>{labels.directoryPagination.previousUnavailable}</p>}
    </div>
    {page && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.name}</th><th>{copy.status}</th><th>{copy.members}</th><th>{labels.identifier}</th></tr></thead><tbody>{page.items.map(org => <tr key={org.organization_id}><td data-label={copy.name}><a className="adm-name-link adm-organization-name-link" {...organizationLink(org.organization_id)}>{org.name}</a></td><td data-label={copy.status}>{organizationLabel(copy, 'Status', org.status)}</td><td data-label={copy.members}>{org.member_count ?? '—'}</td><td data-label={labels.identifier}><AdminIdentifier client={client} owner={identifierOwner} routeScope={globalThis.location?.href || route.search} value={org.organization_id} label={labels.identifier} targetLabel={org.name} language={language} sessionEpoch={identifierSessionEpoch} platformAuthorizationVersion={identifierPlatformVersion} /></td></tr>)}</tbody></table></div>}
    {page?.items.length === 0 && <p>{copy.empty}</p>}
    <nav className="adm-organization-directory-pagination" aria-label={labels.directoryPagination.label} aria-describedby={directoryPaginationNote}><button disabled={busy || !canShowPrevious} onClick={() => { const values = [...previous], cursor = values.pop(); setPrevious(values); navigate({ organization_cursor: cursor || null }); }}>{labels.directoryPagination.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => {
      const nextCursor = page.next_cursor;
      if (directoryScope) setKnownPredecessors(previousKnown => {
        const cursors = previousKnown.scope === directoryScope ? new Map(previousKnown.cursors) : new Map();
        cursors.set(nextCursor, acceptedCursor);
        return { scope: directoryScope, cursors };
      });
      setPrevious(values => [...values, params.get('organization_cursor') || '']); navigate({ organization_cursor: nextCursor });
    }}>{labels.directoryPagination.next}</button></nav>
  </>;
  const available = current?.organization_id === requestedID && detail?.summary?.organization_id === requestedID;
  const unitNames = new Map((detail?.units.items || []).map(unit => [unit.unit_id, unit.name]));
  const readiness = detail?.readiness;
  const mayDelete = current?.status === 'DISABLED' && readiness?.ready === true && readiness.current_status === 'DISABLED' && readiness.account_version === current.version && !detail?.readinessError;
  return <section className="adm-panel adm-organization-directory">
    {error && <p role="alert">{error}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p>}{busy && <p role="status">{copy.loading}</p>}
    {!requestedID ? directory : <><button className="adm-organization-back" onClick={() => navigate({ organization_id: null, organization_tab: null, member_cursor: null })}>{words.backToDirectory}</button>
      {(nameFeedbackKind === 'organization' || active === 'units') && nameFeedback}
      {available && <><header className="adm-organization-header"><div className="adm-organization-identity"><h3>{current.name}</h3><p><span>{organizationLabel(copy, 'Status', current.status)}</span><code>{current.organization_id}</code></p></div><div className="adm-organization-header-actions"><button type="button" disabled={busy} onClick={onRefresh}>{copy.refresh}</button>{can('profile.manage') && current.status !== 'DELETED' && action('rename', copy.rename, [{ name: 'name', initial: current.name }], values => ({ name: text(values.name) }), 'rename')}</div></header>
        <nav className="adm-tabs">{tabs.map(key => <button key={key} aria-pressed={active === key} onClick={() => navigate({ organization_tab: key })}>{labels[key]}</button>)}</nav>
        <RetainedWorkspace key={selected} active={active} available={tabs}>{view => <>
          {view === 'overview' && <OrganizationOverviewSummary organization={current} detail={detail} directoryPage={page} listBusy={listBusy} detailBusy={detailBusy} error={directoryReadError} overviewFeedback={overviewFeedback} labels={labels} navigate={navigate} />}
          {view === 'members' && current.status !== 'DELETED' && <section><div className="adm-member-toolbar adm-section-toolbar"><form className="adm-filter-row" onSubmit={event => { event.preventDefault(); if (memberSearch.trim() === memberQuery && role === memberRole && !params.get('member_cursor')) onRefresh(); else navigate({ member_q: memberSearch.trim() || null, member_role: role || null, member_cursor: null }); }}><label>{copy.search}<input value={memberSearch} onChange={event => setMemberSearch(event.target.value)} /></label><label>{copy.role}<select value={role} onChange={event => setRole(event.target.value)}><option value="">{labels.allRoles}</option>{roles.map(item => <option key={item.value} value={item.value}>{item.label}</option>)}</select></label><button disabled={busy}>{copy.search}</button></form>{can('members.manage') && !detail.unitsError && action('add-member', copy.addMember, [{ name: 'user', lookup: true }, { name: 'role', options: roles }, { name: 'unit', labelKey: 'roleUnit', optional: true, options: units }, { name: 'memberUnits', multiple: true, options: units }], values => ({ user_id: organizationID(values.user), roles: [text(values.role)], unit_admin_unit_ids: values.unit ? [organizationID(values.unit)] : [], unit_ids: organizationUnitIDs(values.memberUnits) }), 'add')}</div>
            {detail.membersError ? <p role="alert">{copy.unavailable}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p> : <><div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.user}</th><th>{copy.role}</th><th>{copy.memberUnits}<small className="ap-member-units-help">{copy.unscopedUnits}</small></th><th>{copy.status}</th><th>{labels.actions}</th></tr></thead><tbody>{detail.members.items.map(member => <tr key={member.user_id}><td data-label={copy.user}>{member.display_name || member.user_code || member.user_id}</td><td data-label={copy.role}>{member.roles.map(value => organizationLabel(copy, 'Role', value)).join(', ')}</td><td data-label={copy.memberUnits}>{member.unit_ids.map(id => detail.units.items.find(unit => unit.unit_id === id)?.name || id).join(', ') || copy.unassignedUnits}</td><td data-label={copy.status}>{organizationLabel(copy, 'Status', member.status)}</td><td data-label={labels.actions}>{can('members.manage') && !detail.unitsError && <OrganizationMemberMenu label={labels.actions} targetLabel={member.display_name || member.user_code || member.user_id}>{action(`roles:${member.user_id}`, copy.replaceRoles, [{ name: 'role', options: roles }, { name: 'unit', labelKey: 'roleUnit', optional: true, options: units }], values => ({ roles: [text(values.role)], unit_admin_unit_ids: values.unit ? [organizationID(values.unit)] : [] }), 'roles', member.user_id, true)}{member.status === 'ACTIVE' && action(`member-units:${member.user_id}`, copy.replaceMemberUnits, [{ name: 'memberUnits', multiple: true, options: units, initial: member.unit_ids }], values => ({ unit_ids: organizationUnitIDs(values.memberUnits), expected_version: organizationVersion(member.version) }), 'units', member.user_id, true)}{action(`remove:${member.user_id}`, copy.remove, [], () => ({}), 'remove', member.user_id, true)}</OrganizationMemberMenu>}</td></tr>)}</tbody></table></div>{!detail.members.items.length && <p>{copy.empty}</p>}
              <nav><button disabled={busy || !memberPrevious.length} onClick={() => { const values = [...memberPrevious], cursor = values.pop(); setMemberPrevious(values); navigate({ member_cursor: cursor || null }); }}>{copy.previous}</button><button disabled={busy || !detail.members.next_cursor} onClick={() => { setMemberPrevious(values => [...values, memberCursor]); navigate({ member_cursor: detail.members.next_cursor }); }}>{copy.next}</button></nav>
            </>}
          </section>}
          {view === 'units' && current.status !== 'DELETED' && <section>{detail.unitsError ? <p role="alert">{copy.unavailable}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p> : <><div className="adm-unit-toolbar adm-section-toolbar"><h4>{labels.units}</h4>{can('units.manage') && action('create-unit', copy.createUnit, [{ name: 'name' }, { name: 'parent', optional: true, options: units }], values => ({ name: text(values.name), parent_id: optionalID(values.parent) }), 'unit')}</div><ul className="adm-organization-units">{detail.units.items.map(unit => <li key={unit.unit_id} ref={renamedUnit?.unit_id === unit.unit_id ? renamedUnitNode : undefined} tabIndex={renamedUnit?.unit_id === unit.unit_id ? -1 : undefined} className={renamedUnit?.unit_id === unit.unit_id ? 'ap-unit-renamed' : undefined}><strong>{unit.name}</strong><span className="adm-unit-parent"><small>{copy.parent}: </small><span>{unit.parent_id ? unitNames.get(unit.parent_id) || unit.parent_id : copy.root}</span></span><span className="adm-unit-status"><small>{copy.status}: </small><span>{organizationLabel(copy, 'Status', unit.status)}</span></span>{can('units.manage') && <div className="adm-row-actions">{action(`rename-unit:${unit.unit_id}`, copy.renameUnit, [{ name: 'name', initial: unit.name }], values => ({ name: text(values.name) }), 'renameUnit', unit.unit_id)}{action(`move-unit:${unit.unit_id}`, copy.moveUnit, [{ name: 'parent', optional: true, options: units }], values => ({ parent_id: optionalID(values.parent), expected_structure_version: organizationVersion(detail.units.structure_version) }), 'moveUnit', unit.unit_id)}{action(`delete-unit:${unit.unit_id}`, copy.deleteUnit, [], () => ({}), 'deleteUnit', unit.unit_id, true)}</div>}</li>)}</ul>{!detail.units.items.length && <p>{copy.empty}</p>}</>}</section>}
          {view === 'lifecycle' && <section>{can('status.manage') && ['ACTIVE', 'DISABLED'].includes(current.status) && action('status', current.status === 'ACTIVE' ? copy.disable : copy.enable, [], () => ({ status: current.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE' }), 'status', '', true)}
            {can('owner.transfer') && current.status !== 'DELETED' && action('transfer', copy.transfer, [{ name: 'newOwner', lookup: true }], values => ({ new_owner_user_id: organizationID(values.newOwner) }), 'transfer', '', true)}
            {can('delete') && current.status === 'DISABLED' && <section><h4>{copy.readiness}</h4><button disabled={busy} onClick={onRefresh}>{copy.refresh}</button>{detail.readinessError && <p role="alert">{copy.unavailable}</p>}{readiness && <ul>{readiness.blockers?.map((item, index) => <li key={index}>{typeof item === 'string' ? item : item.code || item.resource_type}</li>)}</ul>}{mayDelete && action('delete', copy.delete, [{ name: 'confirmationName' }, { name: 'reason' }], values => { if (values.confirmationName !== current.name) throw new Error('INVALID_ORGANIZATION_INPUT'); return { reason: text(values.reason) }; }, 'delete', '', true, { confirmationTarget: `${current.name} · ${current.organization_id}` })}</section>}
          </section>}
        </>}</RetainedWorkspace>
      </>}
    </>}
  </section>;
}
