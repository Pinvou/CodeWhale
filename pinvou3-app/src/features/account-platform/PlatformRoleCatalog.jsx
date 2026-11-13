import React, { useId, useLayoutEffect, useRef, useState } from 'react';

import { AdminIdentifier } from './AdminIdentifier.jsx';
import { platformRoleName, platformRoleDescription, platformPermissionGroups } from './platform-role-presentation.js';

export function PlatformRoleCatalog({ client, me, language, acceptedSessionEpoch, roles, words, authorityCopy, canGrant, onGrant }) {
  const [query, setQuery] = useState(''), [selectedCode, setSelectedCode] = useState('');
  const heading = useId(), headingNode = useRef(null), roleControls = useRef(new Map()), selectionFocus = useRef(false);
  const search = query.trim().toLowerCase();
  const visible = roles.filter(role => [role.code, platformRoleName(words, role.code), platformRoleDescription(words, role), ...role.permissions].some(text => text.toLowerCase().includes(search)));
  const selected = visible.find(role => role.code === selectedCode) || visible[0];
  const focusDetail = () => { const node = headingNode.current; if (node?.isConnected && !node.closest('[hidden]')) node.focus(); };
  const chooseRole = code => {
    if (selected?.code === code) { setSelectedCode(code); focusDetail(); }
    else { selectionFocus.current = true; setSelectedCode(code); }
  };
  useLayoutEffect(() => { if (selectionFocus.current) { selectionFocus.current = false; focusDetail(); } });
  const returnToList = () => { const node = roleControls.current.get(selected?.code); if (node?.isConnected && !node.closest('[hidden]')) node.focus(); };
  const sessionEpoch = acceptedSessionEpoch === undefined ? client?.financeSessionEpoch || 0 : acceptedSessionEpoch;
  const platformVersion = me?.platform_authorization_version || me?.active_context?.platform_authorization_version || me?.authorization_version || 0;
  const identifierOwner = `${me?.user_id || ''}:${platformVersion}:${sessionEpoch}:${selected?.code || ''}`;
  return <>
    <div className="adm-role-toolbar"><label><span className="adm-visually-hidden">{words.roleSearchPlaceholder}</span><input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder={words.roleSearchPlaceholder} /></label><span>{visible.length} / {roles.length}</span></div>
    <div className="adm-role-catalog-layout">
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: This named overflow region needs keyboard focus for native horizontal table scrolling. */}
      <div className="adm-table-wrap" role="region" aria-label={words.catalog} tabIndex={0}><table className="adm-table adm-role-table"><thead><tr><th>{words.role}</th><th>{words.permissionCount}</th></tr></thead><tbody>{visible.map(role => <tr key={role.code} className={selected?.code === role.code ? 'selected' : ''} onClick={() => chooseRole(role.code)}>
        <td><button type="button" ref={node => { if (node) roleControls.current.set(role.code, node); else roleControls.current.delete(role.code); }} className="adm-name-link" aria-pressed={selected?.code === role.code} aria-controls={heading} onClick={event => { event.stopPropagation(); chooseRole(role.code); }}>{platformRoleName(words, role.code)}</button><code>{role.code}</code></td><td>{role.permissions.length}</td>
      </tr>)}</tbody></table>{!visible.length && <p className="adm-empty">{words.noRoles}</p>}</div>
      {selected ? <aside className="adm-role-detail" aria-labelledby={heading}><header><div><h3 id={heading} ref={headingNode} tabIndex={-1}>{platformRoleName(words, selected.code)}</h3><code>{selected.code}</code></div><span className="adm-role-scope platform">{words.scopes.PLATFORM}</span></header>
        <button type="button" className="adm-role-catalog-back" onClick={returnToList}>{words.returnToRoleList}</button>
        <p>{platformRoleDescription(words, selected)}</p>
        <dl className="adm-role-facts"><div><dt>{words.type}</dt><dd>{words.builtIn}</dd></div><div><dt>{words.permissionCount}</dt><dd>{selected.permissions.length}</dd></div></dl>
        <div className="adm-role-permission-title"><strong>{words.includedPermissions}</strong><span>{words.groupedByDomain}</span></div>
        <div className="adm-role-permission-groups">{platformPermissionGroups(words, selected.permissions).map(group => <details key={group.domain} open><summary><svg className="adm-role-permission-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><path d="m9 5 7 7-7 7" /></svg><span className="adm-role-permission-group-name">{group.label}</span><span>{group.items.length}</span></summary>{group.items.map(permission => <div className="adm-role-permission" key={permission.code}><strong>{permission.label}</strong><AdminIdentifier client={client} owner={identifierOwner} routeScope={`${globalThis.location?.href || 'roles'}:${selected.code}`} value={permission.code} label={words.permissionCodeLabel} targetLabel={`${platformRoleName(words, selected.code)} · ${permission.label}`} language={language} sessionEpoch={sessionEpoch} platformAuthorizationVersion={platformVersion} alwaysExpanded /></div>)}</details>)}</div>
        {canGrant(selected) ? onGrant && <button type="button" className="adm-role-primary" onClick={() => onGrant(selected.code)}>{words.grantThisRole}</button> : <p className="adm-role-lifecycle-note">{authorityCopy.protected}</p>}
      </aside> : <div className="adm-role-detail-empty">{words.chooseRole}</div>}
    </div>
  </>;
}
