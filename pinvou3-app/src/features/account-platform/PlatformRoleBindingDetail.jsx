import React from 'react';
import { PortalDialog } from './PortalDialog.jsx';
import { platformRoleName } from './platform-role-presentation.js';
import { PlatformRoleBindingIdentity } from './PlatformRoleBindingIdentity.jsx';
import { usePlatformBindingUsers } from './platform-binding-users.js';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';

export function PlatformRoleBindingDetail({ binding, words, copy, busy, onClose, onRevoke, client, me, acceptedSessionEpoch, routeScope }) {
  const active = usePortalDialogScope();
  const users = usePlatformBindingUsers({ client, me, acceptedSessionEpoch, routeScope, userIDs: [binding.user_id], active });
  const entry = users.lookup(binding.user_id);
  const date = value => value ? new Date(value).toLocaleString() : words.forever;
  return <PortalDialog title={words.bindingDetail} closeLabel={words.cancel} onClose={onClose} busy={busy} drawer>
    <div className="adm-role-grant"><h3>{platformRoleName(words, binding.role_code)}</h3><dl className="adm-role-preview">
      <div><dt>{words.user}</dt><dd><PlatformRoleBindingIdentity userID={binding.user_id} {...{ entry, words }} />{entry.status === 'unavailable' && <button type="button" onClick={users.retry}>{words.reloadBindingUsers}</button>}</dd></div>
      <div><dt>{words.role}</dt><dd>{platformRoleName(words, binding.role_code)}<code>{binding.role_code}</code></dd></div>
      <div><dt>{words.scope}</dt><dd>{words.scopes.PLATFORM}</dd></div>
      <div><dt>{copy.status}</dt><dd>{words.statuses[binding.status]}</dd></div>
      <div><dt>{words.validity}</dt><dd><div>{words.validFrom}: {date(binding.starts_at)}</div><div>{words.validUntil}: {binding.expires_at == null ? words.noExpiry : date(binding.expires_at)}</div></dd></div>
      <div><dt>{words.source}</dt><dd>{typeof words.sources[binding.grant_source] === 'string' ? words.sources[binding.grant_source] : binding.grant_source}</dd></div>
      <div><dt>{words.bindingID}</dt><dd><code>{binding.binding_id}</code></dd></div>
    </dl>{onRevoke && <div className="adm-role-drawer-foot"><button type="button" className="adm-binding-danger" disabled={busy} onClick={onRevoke}>{copy.revoke}</button></div>}</div>
  </PortalDialog>;
}
