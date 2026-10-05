import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ChevronRight, RefreshCw, User, X } from '../../components/icons.jsx';
import { formatPoints } from './credit-units.js';
import { AccountPlatformDesktopClient } from './desktop-client.js';
import { accountErrorMessage, needsAccountLogin } from './errors.js';

function localizedError(copy, reason) {
  return accountErrorMessage(typeof reason === 'string' ? { code: reason } : reason, copy);
}

export function AccountPlatformPopover({ open, client, copy, language, theme, collapsed, onClose, onOpenCenter }) {
  const panelRef = useRef(null);
  const requestRef = useRef({ generation: 0, controller: null });
  const lifecycleRef = useRef({ generation: 0, active: false });
  const mutationRef = useRef(false);
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [balance, setBalance] = useState(null);
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [loading, setLoading] = useState(false);
  const [mutating, setMutating] = useState(false);
  const [error, setError] = useState('');
  const busy = loading || mutating;
  const isDark = theme === 'dark';

  useEffect(() => {
    const life = lifecycleRef.current;
    life.generation += 1;
    life.active = open;
    if (!open) setPassword('');
    return () => {
      life.active = false;
      life.generation += 1;
      requestRef.current.controller?.abort();
      requestRef.current.generation += 1;
    };
  }, [open, client]);

  const clearProtected = useCallback(() => {
    setAuth('guest'); setMe(null); setBalance(null);
  }, []);

  const loadAccount = useCallback(async () => {
    requestRef.current.controller?.abort();
    const controller = new window.AbortController();
    const generation = requestRef.current.generation + 1;
    requestRef.current = { generation, controller };
    const isCurrent = () => requestRef.current.generation === generation && !controller.signal.aborted;
    setLoading(true); setError('');
    try {
      const session = await client.session(controller.signal);
      if (!isCurrent()) return;
      if (!session.authenticated) {
        clearProtected();
        if (session.errorCode && session.errorCode !== 'account_platform_disabled') setError(localizedError(copy, session.errorCode));
        return;
      }
      const account = await client.bootstrap(controller.signal);
      if (!isCurrent()) return;
      const results = await Promise.allSettled([client.balance(controller.signal)]);
      if (!isCurrent()) return;
      const denied = results.find(result => result.status === 'rejected' && needsAccountLogin(result.reason));
      if (denied) { clearProtected(); setError(localizedError(copy, denied.reason)); return; }
      setMe(account);
      setBalance(results[0].status === 'fulfilled' ? results[0].value : null);
      setAuth('ready');
      if (results[0].status === 'rejected') setError(copy.billingUnavailable);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) {
        setMe(null); setBalance(null);
        setAuth(needsAccountLogin(reason) ? 'guest' : 'error');
        setError(localizedError(copy, reason));
      }
    } finally {
      if (isCurrent()) setLoading(false);
    }
  }, [client, copy, clearProtected]);

  useEffect(() => {
    if (open) {
      setMutating(mutationRef.current); setAuth('loading');
      void loadAccount();
    } else {
      requestRef.current.controller?.abort();
      requestRef.current.generation += 1;
    }
    return () => { requestRef.current.controller?.abort(); requestRef.current.generation += 1; };
  }, [open, loadAccount]);

  useEffect(() => {
    if (!open) return undefined;
    const handleMouseDown = event => { if (panelRef.current && !panelRef.current.contains(event.target)) onClose?.(); };
    const handleKeyDown = event => { if (event.key === 'Escape') onClose?.(); };
    window.addEventListener('mousedown', handleMouseDown);
    window.addEventListener('keydown', handleKeyDown);
    return () => { window.removeEventListener('mousedown', handleMouseDown); window.removeEventListener('keydown', handleKeyDown); };
  }, [open, onClose]);

  const runMutation = async action => {
    if (mutationRef.current || loading) return;
    mutationRef.current = true;
    const generation = lifecycleRef.current.generation;
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === generation;
    requestRef.current.controller?.abort(); requestRef.current.generation += 1;
    setLoading(false); setMutating(true); setError('');
    try { await action(isCurrent); }
    catch (reason) {
      if (isCurrent()) {
        if (needsAccountLogin(reason)) clearProtected();
        setError(localizedError(copy, reason));
      }
    } finally {
      mutationRef.current = false;
      if (lifecycleRef.current.active) setMutating(false);
    }
  };

  const submitLogin = async event => {
    event.preventDefault();
    if (!account.trim() || !password || mutationRef.current || loading) return;
    const loginAccount = account.trim(); const loginPassword = password;
    setPassword('');
    await runMutation(async isCurrent => {
      await client.login(loginAccount, loginPassword);
      if (isCurrent()) await loadAccount();
    });
  };
  const logout = () => runMutation(async isCurrent => {
    await client.logout();
    if (!isCurrent()) return;
    requestRef.current.controller?.abort(); requestRef.current.generation += 1;
    clearProtected();
  });

  if (!open) return null;
  const displayName = me?.display_name || copy.user;
  const panelPosition = collapsed ? 'left-full bottom-0 ml-3' : 'left-0 bottom-full mb-2';
  const panelTheme = isDark ? 'bg-[#202124]/95 border-white/10 text-[#E3E3E3]' : 'bg-white/95 border-black/10 text-[#1F1F1F]';
  const cardTheme = isDark ? 'bg-[#131314] border-white/10' : 'bg-[#F8FAFD] border-black/5';
  const mutedText = isDark ? 'text-[#9AA0A6]' : 'text-[#6F7378]';
  const controlTheme = isDark ? 'bg-[#202124] border-white/10 text-[#E3E3E3] placeholder:text-[#6F7378]' : 'bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]';

  return (
    <div
      ref={panelRef}
      data-testid="account-platform-popover"
      className={`absolute ${panelPosition} z-[90] w-[320px] max-w-[calc(100vw-24px)] rounded-[22px] border p-3 shadow-2xl backdrop-blur-xl ${panelTheme}`}
    >
      <div className="flex items-center gap-3 px-1.5 pb-3">
        <div className="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-gradient-to-br from-[#19D3A2] to-[#0B57D0] text-white shadow-lg shadow-emerald-500/20"><User size={20} /></div>
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold">{auth === 'ready' ? displayName : copy.title}</div>
          {auth !== 'ready' && <div className={`truncate text-[11px] ${mutedText}`}>PINVOU ACCOUNT PLATFORM</div>}
        </div>
        <button type="button" onClick={onClose} className={`flex h-8 w-8 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.close}><X size={16} /></button>
      </div>

      {auth === 'loading' && <div className={`flex min-h-36 items-center justify-center gap-2 rounded-2xl border text-[13px] ${cardTheme} ${mutedText}`}><RefreshCw size={15} className="animate-spin" />{copy.loading}</div>}

      {(auth === 'guest' || auth === 'error') && (
        <form className={`space-y-3 rounded-2xl border p-3 ${cardTheme}`} onSubmit={event => void submitLogin(event)}>
          <input
            value={account}
            onChange={event => setAccount(event.target.value)}
            disabled={busy}
            placeholder={copy.account}
            aria-label={copy.account}
            autoComplete="username"
            className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
          />
          <input
            value={password}
            onChange={event => setPassword(event.target.value)}
            disabled={busy}
            placeholder={copy.password}
            aria-label={copy.password}
            type="password"
            autoComplete="current-password"
            className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
          />
          {error && <div role="alert" className="break-words text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          {auth === 'error' && <button type="button" disabled={busy} onClick={() => void loadAccount()}>{copy.retry}</button>}
          <button type="submit" disabled={busy || !account.trim() || !password} className="h-9 w-full rounded-xl bg-[#10B981] text-[13px] font-semibold text-white transition-opacity disabled:opacity-40">{busy ? copy.signingIn : copy.login}</button>
        </form>
      )}

      {auth === 'ready' && (
        <div className="space-y-2.5">
          {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <div className={`rounded-2xl border p-3 ${cardTheme}`}>
            <div className="flex items-end justify-between gap-3"><div><div className={`text-[12px] ${mutedText}`}>{copy.balance}</div><div className="mt-1 text-[22px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div></div><div className={`text-right text-[11px] ${mutedText}`}>{copy.reservedCredits}<div className="mt-1 text-[13px] font-medium tabular-nums">{formatPoints(balance?.reserved_microcredits, language)}</div></div></div>
          </div>

          {onOpenCenter && <button type="button" disabled={busy} onClick={onOpenCenter} className="h-9 w-full rounded-xl text-[12px] font-medium">{copy.title}</button>}

          <button type="button" onClick={() => void logout()} disabled={busy} className={`h-9 w-full rounded-xl text-[12px] font-medium transition-colors disabled:opacity-40 ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>{copy.logout}</button>
        </div>
      )}
    </div>
  );
}

/** Native client stays behind the account popover's lazy boundary. */
export function AccountPlatformDesktopPopover({ transport, ...props }) {
  const client = useMemo(() => new AccountPlatformDesktopClient(transport), [transport]);
  return <AccountPlatformPopover {...props} client={client} />;
}

/** The sidebar account entry owns its presentation and native session restoration. */
export function AccountPlatformSidebarEntry({ transport, copy, language, theme, collapsed, onOpen, onOpenCenter }) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let active = true;
    Promise.resolve().then(() => active && transport.session()).catch(() => {});
    return () => { active = false; };
  }, [transport]);
  return <div className={`relative ${collapsed ? '' : 'w-full'}`}>
    <button type="button" data-testid="account-platform-button" aria-expanded={open} title={copy.title}
      onMouseDown={event => event.stopPropagation()}
      onClick={() => { if (!open) onOpen?.(); setOpen(value => !value); }}
      className={`flex items-center rounded-xl transition-colors ${collapsed ? 'justify-center w-10 h-10' : 'w-full px-2 py-1.5 gap-3 text-left'} ${theme === 'dark' ? 'text-[#E3E3E3] hover:bg-[#333537]' : 'text-[#444746] hover:bg-[#E1E5EA]'}`}>
      <User size={18} />{!collapsed && <span className="min-w-0 flex-1 truncate">{copy.title}</span>}
      {!collapsed && <ChevronRight size={16} className={`shrink-0 opacity-60 ${open ? '-rotate-90' : ''}`} />}
    </button>
    <AccountPlatformDesktopPopover open={open} transport={transport} copy={copy} language={language}
      theme={theme} collapsed={collapsed} onClose={() => setOpen(false)}
      onOpenCenter={onOpenCenter ? () => { setOpen(false); onOpenCenter(); } : undefined} />
  </div>;
}
