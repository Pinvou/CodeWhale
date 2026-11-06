import React from 'react';
import { PortalDialog } from './PortalDialog.jsx';
import { platformRoleName } from './platform-role-presentation.js';

export function PlatformRoleBindingDetail({ binding, words, copy, busy, onClose, onRevoke }) {
  const date = value => value ? new Date(value).toLocaleString() : words.forever;
  return <PortalDialog title={words.bindingDetail} closeLabel={words.cancel} onClose={onClose} busy={busy} drawer>
    <div className="adm-role-grant"><h3>{platformRoleName(words, binding.role_code)}</h3><dl className="adm-role-preview">
      <div><dt>{words.user}</dt><dd><code>{binding.user_id}</code></dd></div>
      <div><dt>{words.role}</dt><dd>{platformRoleName(words, binding.role_code)}<code>{binding.role_code}</code></dd></div>
      <div><dt>{words.scope}</dt><dd>{words.scopes.PLATFORM}</dd></div>
      <div><dt>{copy.status}</dt><dd>{words.statuses[binding.status]}</dd></div>
      <div><dt>{words.validity}</dt><dd>{date(binding.starts_at)} → {date(binding.expires_at)}</dd></div>
      <div><dt>{words.source}</dt><dd>{typeof words.sources[binding.grant_source] === 'string' ? words.sources[binding.grant_source] : binding.grant_source}</dd></div>
      <div><dt>{words.bindingID}</dt><dd><code>{binding.binding_id}</code></dd></div>
    </dl>{onRevoke && <div className="adm-role-drawer-foot"><button type="button" disabled={busy} onClick={onRevoke}>{copy.revoke}</button></div>}</div>
  </PortalDialog>;
}
