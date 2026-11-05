import React, { useEffect, useState } from 'react';
import { accountLayoutCopy } from '../../shared/i18n.js';
import { PortalDialog } from './PortalDialog.jsx';
import { maskedAccountMobile } from './account-display-label.js';

export function ProfileSettingsPortal({ copy, language, me, active, busy, onReauthenticate, usernameForm, mobileForm, captchaDialog, error = '', notice = '' }) {
  const words = accountLayoutCopy[language];
  const [editing, setEditing] = useState(null);
  useEffect(() => {
    const update = () => {
      const requested = new URLSearchParams(globalThis.location?.search || '').get('profile');
      setEditing(active && ['username', 'mobile'].includes(requested) ? requested : null);
    };
    update(); globalThis.addEventListener('popstate', update);
    return () => globalThis.removeEventListener('popstate', update);
  }, [active]);
  const change = field => {
    const url = new URL(globalThis.location.href);
    if (field) url.searchParams.set('profile', field);
    else url.searchParams.delete('profile');
    globalThis.history[field ? 'pushState' : 'replaceState']({}, '', url);
    setEditing(field);
  };
  return <section className="ap-profile-rows"><h3>{copy.profileSettings}</h3>
    <div className="ap-profile-row"><span>{copy.currentUsername}</span><strong>{me.username || '—'}</strong><button type="button" disabled={busy} onClick={() => change('username')}>{words.edit}</button></div>
    <div className="ap-profile-row"><span>{copy.currentMobile}</span><strong>{maskedAccountMobile(me.mobile)}</strong><button type="button" disabled={busy} onClick={() => change('mobile')}>{words.edit}</button></div>
    {active && editing && !captchaDialog && <PortalDialog title={editing === 'username' ? copy.changeUsername : copy.changeMobile} closeLabel={words.closeDialog} busy={busy} onClose={() => change(null)}>
      {error && <p className="ap-alert error" role="alert">{error}</p>}{notice && <p className="ap-alert" role="status">{notice}</p>}<p className="ap-muted">{copy.reauthenticateLead}</p><button type="button" disabled={busy} onClick={() => onReauthenticate(editing)}>{copy.reauthenticate}</button>
      {editing === 'username' ? usernameForm : mobileForm}
    </PortalDialog>}
    {active && captchaDialog}
  </section>;
}
