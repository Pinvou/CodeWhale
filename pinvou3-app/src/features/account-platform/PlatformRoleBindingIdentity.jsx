import React from 'react';

// The binding's user ID remains the operation target even when a profile is
// unavailable, repeated, renamed, or has the same display name as another user.
export function PlatformRoleBindingIdentity({ userID, entry, words }) {
  const user = entry?.status === 'ready' ? entry.user : null;
  const name = [user?.display_name, user?.username].find(value => typeof value === 'string' && value);
  const code = typeof user?.user_code === 'string' && user.user_code ? user.user_code : '';
  const hint = entry?.status === 'notPermitted' ? words.bindingUserNoPermission
    : entry?.status === 'unavailable' ? words.bindingUserUnavailable
      : entry?.status === 'loading' ? words.bindingUserLoading : entry?.status === 'retired' ? words.bindingUserRetired : '';
  return <div className="adm-binding-identity">
    <strong>{name || code || words.bindingUserFallback}</strong>
    {code && name && <small>{words.bindingUserCode}: <code>{code}</code></small>}
    <small>{words.bindingUserID}: <code>{userID}</code></small>
    {hint && <small role={entry?.status === 'loading' ? 'status' : undefined}>{hint}</small>}
  </div>;
}
