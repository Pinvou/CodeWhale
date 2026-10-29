import React, { useId, useState } from 'react';

import { platformRoleName, platformRoleDescription, platformPermissionGroups } from './platform-role-presentation.js';

export function PlatformRoleCatalog({ roles, words, authorityCopy, canGrant, onGrant }) {
  const [query, setQuery] = useState(''), [selectedCode, setSelectedCode] = useState('');
  const heading = useId();
  const search = query.trim().toLowerCase();
  const visible = roles.filter(role => [role.code, platformRoleName(words, role.code), platformRoleDescription(words, role), ...role.permissions].some(text => text.toLowerCase().includes(search)));
  const selected = visible.find(role => role.code === selectedCode) || visible[0];
  return <>
    <div className="adm-role-toolbar"><label><span className="adm-visually-hidden">{words.roleSearchPlaceholder}</span><input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder={words.roleSearchPlaceholder} /></label><span>{visible.length} / {roles.length}</span></div>
    <div className="adm-role-catalog-layout">
      <div className="adm-table-wrap" tabIndex={0}><table className="adm-table adm-role-table"><thead><tr><th>{words.role}</th><th>{words.permissionCount}</th></tr></thead><tbody>{visible.map(role => <tr key={role.code} className={selected?.code === role.code ? 'selected' : ''} onClick={() => setSelectedCode(role.code)}>
        <td><button type="button" className="adm-name-link" aria-pressed={selected?.code === role.code} onClick={() => setSelectedCode(role.code)}>{platformRoleName(words, role.code)}</button><code>{role.code}</code></td><td>{role.permissions.length}</td>
      </tr>)}</tbody></table>{!visible.length && <p className="adm-empty">{words.noRoles}</p>}</div>
      {selected ? <aside className="adm-role-detail" tabIndex={0} aria-labelledby={heading}><header><div><h3 id={heading}>{platformRoleName(words, selected.code)}</h3><code>{selected.code}</code></div><span className="adm-role-scope platform">{words.scopes.PLATFORM}</span></header>
        <p>{platformRoleDescription(words, selected)}</p>
        <dl className="adm-role-facts"><div><dt>{words.type}</dt><dd>{words.builtIn}</dd></div><div><dt>{words.permissionCount}</dt><dd>{selected.permissions.length}</dd></div></dl>
        <div className="adm-role-permission-title"><strong>{words.includedPermissions}</strong><span>{words.groupedByDomain}</span></div>
        <div className="adm-role-permission-groups">{platformPermissionGroups(words, selected.permissions).map(group => <details key={group.domain} open><summary>{group.label}<span>{group.items.length}</span></summary>{group.items.map(permission => <div className="adm-role-permission" key={permission.code}><strong>{permission.label}</strong><code>{permission.code}</code></div>)}</details>)}</div>
        {canGrant(selected) ? onGrant && <button type="button" className="adm-role-primary" onClick={() => onGrant(selected.code)}>{words.grantThisRole}</button> : <p className="adm-role-lifecycle-note">{authorityCopy.protected}</p>}
      </aside> : <div className="adm-role-detail-empty">{words.chooseRole}</div>}
    </div>
  </>;
}
