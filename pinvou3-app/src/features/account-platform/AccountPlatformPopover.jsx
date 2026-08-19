import React, { useCallback, useEffect, useRef, useState } from 'react';
import { ArrowLeft, CheckCircle2, Plus, RefreshCw, User, X } from '../../components/icons.jsx';
import { formatPoints } from './credit-units.js';
import { rechargePollDelay } from './context-guard.js';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'EXPIRED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW']);
const paidOrderStates = new Set(['PAID', 'PAID_LATE']);
const fallbackChannels = ['UNIONPAY', 'ALIPAY', 'WECHAT'];

function localizedError(copy, reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const code = raw.split(':', 1)[0].trim();
  return copy.errors?.[code] || (reason?.message && typeof reason.message === 'string' ? reason.message : copy.requestFailed);
}

function newIdempotencyKey() {
  return `desktop-recharge-${globalThis.crypto.randomUUID()}`;
}

function orderRemainingSeconds(order, now) {
  const expiresAt = new Date(order?.expires_at).getTime();
  if (!Number.isFinite(expiresAt)) return 0;
  return Math.max(0, Math.ceil((expiresAt - now) / 1000));
}

function formatCountdown(seconds) {
  return `${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}`;
}

function RechargePanel({
  copy, language, products, balance, order, qrData, qrError, loading, busy, error, notice,
  cardTheme, controlTheme, mutedText, isDark, onCreate, onRetryLoad, onRetryQR, onRefreshBalance, onResetOrder, onDone,
}) {
  const [productId, setProductId] = useState('');
  const [channel, setChannel] = useState('ALIPAY');
  const [now, setNow] = useState(Date.now());
  const selected = products.find(product => product.product_id === productId);
  const channels = selected?.channels?.length ? selected.channels : fallbackChannels;
  const remainingSeconds = orderRemainingSeconds(order, now);
  const paid = paidOrderStates.has(order?.status);
  const terminal = terminalOrderStates.has(order?.status);
  const displayStatus = order && !terminal && remainingSeconds === 0 ? 'EXPIRED' : order?.status;
  const canScan = Boolean(order && !terminal && remainingSeconds > 0);
  const inactiveHint = displayStatus === 'EXPIRED'
    ? copy.orderExpires(new Date(order?.expires_at).toLocaleTimeString())
    : copy.orderNoLongerPayable;

  useEffect(() => {
    if (!selected && products[0]) setProductId(products[0].product_id);
  }, [products, selected]);

  useEffect(() => {
    if (channels.length && !channels.includes(channel)) setChannel(channels[0]);
  }, [channel, channels]);

  useEffect(() => {
    if (!canScan) return undefined;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [canScan, order?.order_id]);

  if (loading) {
    return <div className={`flex min-h-56 items-center justify-center gap-2 rounded-2xl border text-[13px] ${cardTheme} ${mutedText}`}><RefreshCw size={15} className="animate-spin" />{copy.loading}</div>;
  }

  if (paid) {
    return (
      <div className="space-y-2.5">
        {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
        <div className={`flex flex-col items-center rounded-2xl border px-4 py-5 text-center ${cardTheme}`}>
          <CheckCircle2 size={38} className="text-[#10B981]" />
          <div className="mt-3 text-[15px] font-semibold">{copy.orderStatuses[order.status]}</div>
          <div className={`mt-1 text-[12px] leading-relaxed ${mutedText}`}>{notice || (error ? copy.paymentConfirmed : copy.paymentReceived)}</div>
          <div className="mt-4 text-[24px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
          <div className={`text-[11px] ${mutedText}`}>{copy.balance}</div>
        </div>
        {error && <button type="button" disabled={busy} onClick={onRefreshBalance} className="flex h-9 w-full items-center justify-center gap-2 rounded-xl bg-[#10B981]/10 text-[12px] font-semibold text-[#07966F] disabled:opacity-40">{busy && <RefreshCw size={13} className="animate-spin" />}{copy.retry}</button>}
        <div className="grid grid-cols-2 gap-2">
          <button type="button" onClick={onResetOrder} className={`h-9 rounded-xl text-[12px] font-medium transition-colors ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>{copy.rechargeAgain}</button>
          <button type="button" onClick={onDone} className="h-9 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">{copy.done}</button>
        </div>
      </div>
    );
  }

  if (order) {
    return (
      <div className="space-y-2.5">
        {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
        <div className={`rounded-2xl border p-3 ${cardTheme}`}>
          <div className="flex items-center justify-between gap-3">
            <span className={`rounded-full px-2 py-1 text-[11px] font-medium ${canScan ? 'bg-[#10B981]/10 text-[#07966F]' : 'bg-[#EA4335]/10 text-[#D93025]'}`}>{copy.orderStatuses[displayStatus] || displayStatus}</span>
            <strong className="text-[15px] tabular-nums">¥{(order.amount_fen / 100).toFixed(2)}</strong>
          </div>
          {canScan ? (
            <>
              <div className="mt-3 flex min-h-[190px] items-center justify-center rounded-2xl bg-white p-2">
                {qrData && <img src={qrData} alt={copy.paymentQR} className="h-[180px] w-[180px]" />}
                {!qrData && !qrError && <div className="flex items-center gap-2 text-[12px] text-[#6F7378]"><RefreshCw size={14} className="animate-spin" />{copy.qrGenerating}</div>}
                {qrError && <div className="px-4 text-center text-[12px] text-[#D93025]"><p>{copy.qrFailed}</p><button type="button" onClick={onRetryQR} className="mt-2 rounded-lg bg-[#D93025]/10 px-3 py-1.5 font-medium">{copy.retry}</button></div>}
              </div>
              <div className="mt-3 text-center text-[13px] font-medium">{copy.scanToPay}</div>
              <div className={`mt-1 text-center text-[11px] tabular-nums ${mutedText}`}>{copy.countdown(formatCountdown(remainingSeconds))}</div>
            </>
          ) : (
            <div className={`py-8 text-center text-[12px] ${mutedText}`}>{inactiveHint}</div>
          )}
        </div>
        {!canScan && <button type="button" onClick={onResetOrder} className="h-9 w-full rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">{copy.chooseAgain}</button>}
      </div>
    );
  }

  if (!products.length) {
    return (
      <div className={`rounded-2xl border p-4 text-center ${cardTheme}`}>
        <p className={`text-[12px] leading-relaxed ${mutedText}`}>{error || copy.rechargeUnavailable}</p>
        <button type="button" onClick={onRetryLoad} className="mt-3 h-8 rounded-xl bg-[#10B981] px-4 text-[12px] font-semibold text-white">{copy.retry}</button>
      </div>
    );
  }

  return (
    <div className="space-y-2.5">
      {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
      <div className={`rounded-2xl border p-3 ${cardTheme}`}>
        <div className="flex items-end justify-between gap-3">
          <div><div className={`text-[11px] ${mutedText}`}>{copy.balance}</div><div className="mt-0.5 text-[20px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div></div>
          <div className={`text-[11px] ${mutedText}`}>{copy.points}</div>
        </div>
      </div>
      <div>
        <div className={`mb-1.5 px-1 text-[11px] font-medium ${mutedText}`}>{copy.selectAmount}</div>
        <div className="grid grid-cols-2 gap-2">
          {products.map(product => (
            <button
              key={product.product_id}
              type="button"
              aria-pressed={product.product_id === productId}
              onClick={() => setProductId(product.product_id)}
              className={`rounded-xl border px-2 py-2.5 text-left transition-colors ${product.product_id === productId ? 'border-[#10B981] bg-[#10B981]/10' : cardTheme}`}
            >
              <strong className="block text-[15px] tabular-nums">¥{(product.amount_fen / 100).toFixed(2)}</strong>
              <span className={`mt-0.5 block text-[11px] ${mutedText}`}>{product.display_points.toLocaleString()} {copy.points}</span>
            </button>
          ))}
        </div>
      </div>
      <label className="block">
        <span className={`mb-1.5 block px-1 text-[11px] font-medium ${mutedText}`}>{copy.paymentMethod}</span>
        <select value={channel} onChange={event => setChannel(event.target.value)} className={`h-9 w-full rounded-xl border px-3 text-[12px] outline-none ${controlTheme}`}>
          {channels.map(item => <option key={item} value={item}>{copy.channels[item] || item}</option>)}
        </select>
      </label>
      <button type="button" disabled={busy || !selected} onClick={() => onCreate(productId, channel)} className="flex h-9 w-full items-center justify-center gap-2 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white transition-opacity disabled:opacity-40">{busy && <RefreshCw size={13} className="animate-spin" />}{copy.createRecharge}</button>
    </div>
  );
}

export function AccountPlatformPopover({ open, client, copy, language, theme, collapsed, onClose }) {
  const panelRef = useRef(null);
  const requestRef = useRef({ generation: 0, controller: null });
  const rechargeRequestRef = useRef({ generation: 0, controller: null });
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [balance, setBalance] = useState(null);
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [view, setView] = useState('account');
  const [products, setProducts] = useState([]);
  const [order, setOrder] = useState(null);
  const [qrData, setQrData] = useState('');
  const [qrError, setQrError] = useState(false);
  const [qrRetry, setQrRetry] = useState(0);
  const [rechargeLoading, setRechargeLoading] = useState(false);
  const [rechargeBusy, setRechargeBusy] = useState(false);
  const [rechargeError, setRechargeError] = useState('');
  const [rechargeNotice, setRechargeNotice] = useState('');
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
        if (session.errorCode && session.errorCode !== 'account_platform_disabled') setError(localizedError(copy, session.errorCode));
        return;
      }
      const nextAccount = await client.bootstrap(controller.signal);
      if (!isCurrent()) return;
      const results = await Promise.allSettled([client.balance(controller.signal)]);
      if (!isCurrent()) return;
      setMe(nextAccount);
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

  const loadRecharge = useCallback(async () => {
    rechargeRequestRef.current.controller?.abort();
    const controller = new window.AbortController();
    const generation = rechargeRequestRef.current.generation + 1;
    rechargeRequestRef.current = { generation, controller };
    const isCurrent = () => rechargeRequestRef.current.generation === generation && !controller.signal.aborted;
    setRechargeLoading(true);
    setRechargeError('');
    setRechargeNotice('');
    try {
      const results = await Promise.allSettled([
        client.products(controller.signal),
        client.balance(controller.signal),
      ]);
      if (!isCurrent()) return;
      const value = index => results[index].status === 'fulfilled' ? results[index].value : null;
      setProducts(value(0)?.items || []);
      setOrder(null);
      if (value(1)) setBalance(value(1));
      if (results.some(result => result.status === 'rejected')) setRechargeError(copy.billingUnavailable);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) setRechargeError(localizedError(copy, reason));
    } finally {
      if (isCurrent()) setRechargeLoading(false);
    }
  }, [client, copy]);

  useEffect(() => {
    if (open) {
      setView('account');
      setAuth('loading');
      void loadAccount();
    } else {
      requestRef.current.controller?.abort();
      requestRef.current.generation += 1;
      rechargeRequestRef.current.controller?.abort();
      rechargeRequestRef.current.generation += 1;
    }
    return () => {
      requestRef.current.controller?.abort();
      rechargeRequestRef.current.controller?.abort();
    };
  }, [open, loadAccount]);

  useEffect(() => {
    const paymentContent = order?.payment_url || order?.qr_content || '';
    if (!paymentContent) {
      setQrData('');
      setQrError(false);
      return undefined;
    }
    let active = true;
    setQrData('');
    setQrError(false);
    import('qrcode')
      .then(({ default: QRCode }) => QRCode.toDataURL(paymentContent, { width: 220, margin: 2, errorCorrectionLevel: 'M' }))
      .then(value => { if (active) setQrData(value); })
      .catch(() => { if (active) setQrError(true); });
    return () => { active = false; };
  }, [order?.payment_url, order?.qr_content, qrRetry]);

  useEffect(() => {
    if (!open || auth !== 'ready' || !order?.order_id || terminalOrderStates.has(order.status) || orderRemainingSeconds(order, Date.now()) === 0) return undefined;
    let stopped = false;
    let timer;
    let attempt = 0;
    const poll = async () => {
      try {
        const latest = await client.rechargeOrder(order.order_id);
        if (stopped) return;
        if (paidOrderStates.has(latest.status)) {
          let refreshedBalance = null;
          let refreshFailed = false;
          try {
            refreshedBalance = await client.balance();
          } catch {
            refreshFailed = true;
          }
          if (stopped) return;
          setOrder(latest);
          setRechargeNotice(refreshFailed ? '' : copy.paymentReceived);
          setRechargeError(refreshFailed ? copy.balanceRefreshFailed : '');
          if (refreshedBalance) setBalance(refreshedBalance);
          return;
        }
        setOrder(latest);
        setRechargeError('');
        if (!terminalOrderStates.has(latest.status) && orderRemainingSeconds(latest, Date.now()) > 0) timer = window.setTimeout(poll, rechargePollDelay(attempt++));
      } catch {
        if (!stopped) {
          setRechargeError(copy.paymentStatusUnavailable);
          timer = window.setTimeout(poll, rechargePollDelay(attempt++));
        }
      }
    };
    timer = window.setTimeout(poll, rechargePollDelay(attempt++));
    return () => { stopped = true; window.clearTimeout(timer); };
  }, [auth, client, copy, open, order?.order_id, order?.status]);

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
  const panelTheme = isDark ? 'bg-[#202124]/95 border-white/10 text-[#E3E3E3]' : 'bg-white/95 border-black/10 text-[#1F1F1F]';
  const cardTheme = isDark ? 'bg-[#131314] border-white/10' : 'bg-[#F8FAFD] border-black/5';
  const mutedText = isDark ? 'text-[#9AA0A6]' : 'text-[#6F7378]';
  const controlTheme = isDark ? 'bg-[#202124] border-white/10 text-[#E3E3E3] placeholder:text-[#6F7378]' : 'bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]';

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
      setProducts([]);
      setOrder(null);
      setView('account');
      setAuth('guest');
    } catch (reason) {
      setError(localizedError(copy, reason));
    } finally {
      setBusy(false);
    }
  };

  const openRecharge = () => {
    setOrder(null);
    setQrData('');
    setQrError(false);
    setRechargeError('');
    setRechargeNotice('');
    setView('recharge');
    void loadRecharge();
  };

  const createRecharge = async (productId, channel) => {
    if (rechargeBusy) return;
    setRechargeBusy(true);
    setRechargeError('');
    setRechargeNotice('');
    try {
      const nextOrder = await client.createRecharge(productId, channel, newIdempotencyKey());
      setOrder(nextOrder);
    } catch (reason) {
      setRechargeError(localizedError(copy, reason));
    } finally {
      setRechargeBusy(false);
    }
  };

  const refreshRechargeBalance = async () => {
    if (rechargeBusy) return;
    setRechargeBusy(true);
    try {
      setBalance(await client.balance());
      setRechargeError('');
      setRechargeNotice(copy.paymentReceived);
    } catch {
      setRechargeError(copy.balanceRefreshFailed);
      setRechargeNotice('');
    } finally {
      setRechargeBusy(false);
    }
  };

  return (
    <div ref={panelRef} data-testid="account-platform-popover" className={`absolute ${panelPosition} z-[90] max-h-[calc(100vh-24px)] w-[336px] max-w-[calc(100vw-24px)] overflow-y-auto rounded-[22px] border p-3 shadow-2xl backdrop-blur-xl ${panelTheme}`}>
      <div className="flex items-center gap-3 px-1.5 pb-3">
        {view === 'recharge' && auth === 'ready' ? (
          <button type="button" onClick={() => setView('account')} className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.backToAccount}><ArrowLeft size={17} /></button>
        ) : (
          <div className="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-gradient-to-br from-[#19D3A2] to-[#0B57D0] text-white shadow-lg shadow-emerald-500/20"><User size={20} /></div>
        )}
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold">{view === 'recharge' && auth === 'ready' ? copy.rechargeTitle : auth === 'ready' ? displayName : copy.title}</div>
          {auth !== 'ready' && <div className={`truncate text-[11px] ${mutedText}`}>PINVOU ACCOUNT PLATFORM</div>}
        </div>
        <button type="button" onClick={onClose} className={`flex h-8 w-8 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.close}><X size={16} /></button>
      </div>

      {auth === 'loading' && <div className={`flex min-h-36 items-center justify-center gap-2 rounded-2xl border text-[13px] ${cardTheme} ${mutedText}`}><RefreshCw size={15} className="animate-spin" />{copy.loading}</div>}

      {(auth === 'guest' || auth === 'error') && (
        <form className={`space-y-3 rounded-2xl border p-3 ${cardTheme}`} onSubmit={event => void submitLogin(event)}>
          <input value={account} onChange={event => setAccount(event.target.value)} disabled={busy} placeholder={copy.account} aria-label={copy.account} autoComplete="username" className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />
          <input value={password} onChange={event => setPassword(event.target.value)} disabled={busy} placeholder={copy.password} aria-label={copy.password} type="password" autoComplete="current-password" className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />
          {error && <div role="alert" className="break-words text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <button type="submit" disabled={busy || !account.trim() || !password} className="h-9 w-full rounded-xl bg-[#10B981] text-[13px] font-semibold text-white transition-opacity disabled:opacity-40">{busy ? copy.signingIn : copy.login}</button>
        </form>
      )}

      {auth === 'ready' && view === 'account' && (
        <div className="space-y-2.5">
          {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <div className={`rounded-2xl border p-3 ${cardTheme}`}>
            <div className="flex items-center justify-between gap-3">
              <div className={`text-[12px] ${mutedText}`}>{copy.balance}</div>
              <button type="button" onClick={openRecharge} className="flex h-7 items-center gap-1 rounded-lg bg-[#10B981]/10 px-2 text-[11px] font-semibold text-[#07966F] transition-colors hover:bg-[#10B981]/20"><Plus size={13} />{copy.recharge}</button>
            </div>
            <div className="mt-1 flex items-end justify-between gap-3">
              <div className="text-[22px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
              <div className={`text-right text-[11px] ${mutedText}`}>{copy.reservedCredits}<div className="mt-1 text-[13px] font-medium tabular-nums">{formatPoints(balance?.reserved_microcredits, language)}</div></div>
            </div>
          </div>
          <button type="button" onClick={() => void logout()} disabled={busy} className={`h-9 w-full rounded-xl text-[12px] font-medium transition-colors disabled:opacity-40 ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>{copy.logout}</button>
        </div>
      )}

      {auth === 'ready' && view === 'recharge' && (
        <RechargePanel
          copy={copy}
          language={language}
          products={products}
          balance={balance}
          order={order}
          qrData={qrData}
          qrError={qrError}
          loading={rechargeLoading}
          busy={rechargeBusy}
          error={rechargeError}
          notice={rechargeNotice}
          cardTheme={cardTheme}
          controlTheme={controlTheme}
          mutedText={mutedText}
          isDark={isDark}
          onCreate={(productId, channel) => void createRecharge(productId, channel)}
          onRetryLoad={() => void loadRecharge()}
          onRetryQR={() => setQrRetry(value => value + 1)}
          onRefreshBalance={() => void refreshRechargeBalance()}
          onResetOrder={() => { setOrder(null); setQrData(''); setQrError(false); setRechargeError(''); setRechargeNotice(''); }}
          onDone={() => setView('account')}
        />
      )}
    </div>
  );
}
