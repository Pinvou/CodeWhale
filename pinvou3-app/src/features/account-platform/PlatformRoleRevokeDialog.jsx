import React from 'react';
import { PortalDialog } from './PortalDialog.jsx';
import { PlatformRoleBindingIdentity } from './PlatformRoleBindingIdentity.jsx';
import { platformRoleName } from './platform-role-presentation.js';
import { usePlatformBindingUsers } from './platform-binding-users.js';

export function PlatformRoleRevokeDialog({ review, words, copy, client, me, acceptedSessionEpoch, routeScope, busy, onCancel, onConfirm }) {
  const { binding } = review;
  const users = usePlatformBindingUsers({ client, me, acceptedSessionEpoch, routeScope, userIDs: [binding.user_id], active: true });
  const entry = users.lookup(binding.user_id);
  return <PortalDialog title={words.confirmRevokeTitle} closeLabel={words.cancel} onClose={onCancel} busy={busy}>
    <div className="adm-role-grant adm-role-revoke-review">
      <p>{words.confirmRevokeLead}</p><dl className="adm-role-preview">
        <div><dt>{words.user}</dt><dd><PlatformRoleBindingIdentity userID={binding.user_id} {...{ entry, words }} />{entry.status === 'unavailable' && <button type="button" onClick={users.retry}>{words.reloadBindingUsers}</button>}</dd></div>
        <div><dt>{words.role}</dt><dd>{platformRoleName(words, binding.role_code)}<code>{binding.role_code}</code></dd></div>
        <div><dt>{words.scope}</dt><dd>{words.scopes.PLATFORM}</dd></div>
        <div><dt>{words.bindingID}</dt><dd><code>{binding.binding_id}</code></dd></div>
      </dl><p className="adm-role-risk-note">{words.revokeBindingImpact}</p>
      <div className="adm-role-drawer-foot"><button type="button" disabled={busy} onClick={onCancel}>{words.cancel}</button><button type="button" className="adm-binding-danger" disabled={busy} onClick={onConfirm}>{copy.revoke}</button></div>
    </div>
  </PortalDialog>;
}
