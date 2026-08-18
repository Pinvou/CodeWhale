import React, { useCallback, useEffect, useRef, useState } from 'react';
import { RefreshCw, User, X } from '../../components/icons.jsx';
import { formatPoints } from './credit-units.js';

function localizedError(copy, reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const code = raw.split(':', 1)[0].trim();
  return copy.errors?.[code] || (reason?.message && typeof reason.message === 'string' ? reason.message : copy.requestFailed);
}

export function AccountPlatformPopover({ open, client, copy, language, theme, collapsed, onClose }) {
  const panelRef = useRef(null);
  const requestRef = useRef({ generation: 0, controller: null });
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [balance, setBalance] = useState(null);
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const isDark = theme === 'dark';

  const loadAccount = useCallback(async () => {
    requestRef.current.controller?.abort();
    const controller = new window.AbortController();
    const generation = requestRef.current.generation + 1;
    requestRef.current = { generation, controller };
    const isCurrent = () => requestRef.current.generation === generation && !controller.signal.aborted;
    setBusy(true);
    setError('');
    try {
      const session = await client.session(controller.signal);
      if (!isCurrent()) return;
      if (!session.authenticated) {
        setAuth('guest');
        setMe(null);
        setBalance(null);
        if (session.errorCode && session.errorCode !== 'account_platform_disabled') {
          setError(localizedError(copy, session.errorCode));
        }
        return;
      }

      const account = await client.bootstrap(controller.signal);
      if (!isCurrent()) return;
      const results = await Promise.allSettled([client.balance(controller.signal)]);
      if (!isCurrent()) return;
      setMe(account);
      setBalance(results[0].status === 'fulfilled' ? results[0].value : null);
      setAuth('ready');
      if (results.some(result => result.status === 'rejected')) setError(copy.requestFailed);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) {
        setAuth('error');
        setError(localizedError(copy, reason));
      }
    } finally {
      if (isCurrent()) setBusy(false);
    }
  }, [client, copy]);

  useEffect(() => {
    if (open) {
      setAuth('loading');
      void loadAccount();
    } else {
      requestRef.current.controller?.abort();
      requestRef.current.generation += 1;
    }
    return () => requestRef.current.controller?.abort();
  }, [open, loadAccount]);

  useEffect(() => {
    if (!open) return undefined;
    const handleMouseDown = event => {
      if (panelRef.current && !panelRef.current.contains(event.target)) onClose?.();
    };
    const handleKeyDown = event => {
      if (event.key === 'Escape') onClose?.();
    };
    window.addEventListener('mousedown', handleMouseDown);
    window.addEventListener('keydown', handleKeyDown);
    return () => {
      window.removeEventListener('mousedown', handleMouseDown);
      window.removeEventListener('keydown', handleKeyDown);
    };
  }, [open, onClose]);

  if (!open) return null;

  const displayName = me?.display_name || copy.user;
  const panelPosition = collapsed ? 'left-full bottom-0 ml-3' : 'left-0 bottom-full mb-2';
  const panelTheme = isDark
    ? 'bg-[#202124]/95 border-white/10 text-[#E3E3E3]'
    : 'bg-white/95 border-black/10 text-[#1F1F1F]';
  const cardTheme = isDark ? 'bg-[#131314] border-white/10' : 'bg-[#F8FAFD] border-black/5';
  const mutedText = isDark ? 'text-[#9AA0A6]' : 'text-[#6F7378]';
  const controlTheme = isDark
    ? 'bg-[#202124] border-white/10 text-[#E3E3E3] placeholder:text-[#6F7378]'
    : 'bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]';

  const submitLogin = async event => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await client.login(account.trim(), password);
      setPassword('');
      await loadAccount();
    } catch (reason) {
      setPassword('');
      setAuth('guest');
      setError(localizedError(copy, reason));
      setBusy(false);
    }
  };

  const logout = async () => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await client.logout();
      setMe(null);
      setBalance(null);
      setAuth('guest');
    } catch (reason) {
      setError(localizedError(copy, reason));
    } finally {
      setBusy(false);
    }
  };

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
          <button type="submit" disabled={busy || !account.trim() || !password} className="h-9 w-full rounded-xl bg-[#10B981] text-[13px] font-semibold text-white transition-opacity disabled:opacity-40">{busy ? copy.signingIn : copy.login}</button>
        </form>
      )}

      {auth === 'ready' && (
        <div className="space-y-2.5">
          {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <div className={`rounded-2xl border p-3 ${cardTheme}`}>
            <div className="flex items-end justify-between gap-3"><div><div className={`text-[12px] ${mutedText}`}>{copy.balance}</div><div className="mt-1 text-[22px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div></div><div className={`text-right text-[11px] ${mutedText}`}>{copy.reservedCredits}<div className="mt-1 text-[13px] font-medium tabular-nums">{formatPoints(balance?.reserved_microcredits, language)}</div></div></div>
          </div>

          <button type="button" onClick={() => void logout()} disabled={busy} className={`h-9 w-full rounded-xl text-[12px] font-medium transition-colors disabled:opacity-40 ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>{copy.logout}</button>
        </div>
      )}
    </div>
  );
}
