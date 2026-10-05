import React, { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { ArrowLeft, CheckCircle2, ChevronRight, Plus, RefreshCw, User, X } from '../../components/icons.jsx';
import { safePNGImage } from './bff-client.js';
import { formatMoney, formatPoints } from './credit-units.js';
import { AccountPlatformDesktopClient } from './desktop-client.js';
import { accountErrorMessage, needsAccountLogin } from './errors.js';
import { rechargePollDelay } from './context-guard.js';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'EXPIRED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW']);
const paidOrderStates = new Set(['PAID', 'PAID_LATE']);
const fallbackChannels = ['UNIONPAY', 'ALIPAY', 'WECHAT'];

function localizedError(copy, reason) {
  if (!reason) return '';
  if (reason.copyKey) return copy[reason.copyKey] || copy.requestFailed;
  const code = typeof reason === 'string' ? reason : reason.code;
  const mobile = {
    USERNAME_INVALID: 'usernameInvalid', USERNAME_ALREADY_REGISTERED: 'usernameAlreadyRegistered',
    PASSWORD_INVALID: 'passwordInvalid', MOBILE_INVALID: 'mobileInvalid',
    MOBILE_ALREADY_REGISTERED: 'mobileAlreadyRegistered', CAPTCHA_INVALID: 'captchaInvalid',
    SMS_RATE_LIMITED: 'smsRateLimited', SMS_UNAVAILABLE: 'smsUnavailable',
    SMS_CODE_INVALID: 'mobileAuthFailed', mobile_auth_code_invalid: 'mobileAuthFailed',
    mobile_auth_failed: 'mobileAuthFailed', AUTH_INTERNAL_ERROR: 'requestFailed', INVALID_REQUEST: 'invalidInput',
  };
  if (Object.prototype.hasOwnProperty.call(mobile, code)) return copy[mobile[code]] || copy.requestFailed;
  return accountErrorMessage(typeof reason === 'string' ? { code: reason } : reason, copy);
}

function registrationInputError(username, mobile, password, confirmation) {
  if (!/^[A-Za-z][A-Za-z0-9._-]{2,31}$/u.test(username.trim())) return 'usernameInvalid';
  if (!/^1[3-9]\d{9}$/u.test(mobile.trim().replace(/^\+?86/u, ''))) return 'mobileInvalid';
  if (password.length < 8 || password.length > 128 || !/[A-Za-z]/u.test(password) || !/\d/u.test(password) || !/^[!-~]+$/u.test(password)) return 'passwordInvalid';
  if (password !== confirmation) return 'passwordMismatch';
  return '';
}

function SliderCaptcha({ challenge, offset, busy, copy, cardTheme, mutedText, onOffset, onConfirm, onRefresh, onClose }) {
  const width = Number(challenge?.width || 280);
  const height = Number(challenge?.height || 120);
  const pieceWidth = Number(challenge?.piece_width || 44);
  const pieceHeight = Number(challenge?.piece_height || 44);
  const maximum = Math.max(1, width - pieceWidth);
  return (
    <div className={`w-full space-y-2 rounded-xl border p-2 shadow-xl ${cardTheme}`}>
      <div className="flex items-center justify-between gap-2"><span className="text-[11px] font-medium">{copy.captchaTitle}</span><div className="flex items-center gap-2"><button type="button" disabled={busy} onClick={onRefresh} className={`text-[11px] ${mutedText}`}>{copy.captchaRefresh}</button><button type="button" disabled={busy} onClick={onClose} aria-label={copy.captchaClose} title={copy.captchaClose} className={`flex h-6 w-6 items-center justify-center rounded-full ${mutedText}`}><X size={13} /></button></div></div>
      <div className="relative w-full overflow-hidden rounded-lg" style={{ aspectRatio:`${width}/${height}` }}>
        <img src={challenge.background_image} alt="" draggable="false" className="absolute inset-0 h-full w-full select-none" />
        <img src={challenge.piece_image} alt="" draggable="false" className="pointer-events-none absolute select-none drop-shadow-md" style={{ left:`${Number(offset || 0) / width * 100}%`, top:`${Number(challenge.piece_y || 0) / height * 100}%`, width:`${pieceWidth / width * 100}%`, height:`${pieceHeight / height * 100}%` }} />
      </div>
      <input type="range" min="0" max={maximum} step="1" value={offset} disabled={busy} onChange={event => onOffset(Number(event.target.value))} aria-label={copy.captchaSlider} className="h-6 w-full cursor-grab accent-[#10B981] disabled:cursor-not-allowed" />
      <button type="button" disabled={busy} onClick={onConfirm} className="h-8 w-full rounded-lg bg-[#10B981] text-[12px] font-semibold text-white disabled:opacity-40">{busy ? copy.sendingCode : copy.captchaConfirm}</button>
    </div>
  );
}

function newIdempotencyKey() {
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  return `desktop-recharge-${Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('')}`;
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
          <button type="button" disabled={busy} onClick={onResetOrder} className={`h-9 rounded-xl text-[12px] font-medium transition-colors ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>{copy.rechargeAgain}</button>
          <button type="button" disabled={busy} onClick={onDone} className="h-9 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">{copy.done}</button>
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
            <strong className="text-[15px] tabular-nums">{formatMoney(order.amount_fen, language)}</strong>
          </div>
          {canScan ? (
            <>
              <div className="mt-3 flex min-h-[190px] items-center justify-center rounded-2xl bg-white p-2">
                {qrData && <img src={qrData} alt={copy.paymentQR} className="h-[180px] w-[180px]" />}
                {!qrData && !qrError && <div className="flex items-center gap-2 text-[12px] text-[#6F7378]"><RefreshCw size={14} className="animate-spin" />{copy.qrGenerating}</div>}
                {qrError && <div className="px-4 text-center text-[12px] text-[#D93025]"><p>{copy.qrFailed}</p><button type="button" disabled={busy} onClick={onRetryQR} className="mt-2 rounded-lg bg-[#D93025]/10 px-3 py-1.5 font-medium">{copy.retry}</button></div>}
              </div>
              <div className="mt-3 text-center text-[13px] font-medium">{copy.scanToPay}</div>
              <div className={`mt-1 text-center text-[11px] tabular-nums ${mutedText}`}>{copy.countdown(formatCountdown(remainingSeconds))}</div>
            </>
          ) : (
            <div className={`py-8 text-center text-[12px] ${mutedText}`}>{inactiveHint}</div>
          )}
        </div>
        {!canScan && <button type="button" disabled={busy} onClick={onResetOrder} className="h-9 w-full rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">{copy.chooseAgain}</button>}
      </div>
    );
  }

  if (!products.length) {
    return (
      <div className={`rounded-2xl border p-4 text-center ${cardTheme}`}>
        <p className={`text-[12px] leading-relaxed ${mutedText}`}>{error || copy.rechargeUnavailable}</p>
        <button type="button" disabled={busy} onClick={onRetryLoad} className="mt-3 h-8 rounded-xl bg-[#10B981] px-4 text-[12px] font-semibold text-white">{copy.retry}</button>
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
              disabled={busy} onClick={() => setProductId(product.product_id)}
              className={`rounded-xl border px-2 py-2.5 text-left transition-colors ${product.product_id === productId ? 'border-[#10B981] bg-[#10B981]/10' : cardTheme}`}
            >
              <strong className="block text-[15px] tabular-nums">{formatMoney(product.amount_fen, language)}</strong>
              <span className={`mt-0.5 block text-[11px] ${mutedText}`}>{formatPoints(String(product.display_points) + '0000', language)} {copy.points}</span>
            </button>
          ))}
        </div>
      </div>
      <label className="block">
        <span className={`mb-1.5 block px-1 text-[11px] font-medium ${mutedText}`}>{copy.paymentMethod}</span>
        <select disabled={busy} value={channel} onChange={event => setChannel(event.target.value)} className={`h-9 w-full rounded-xl border px-3 text-[12px] outline-none ${controlTheme}`}>
          {channels.map(item => <option key={item} value={item}>{copy.channels[item] || item}</option>)}
        </select>
      </label>
      <button type="button" disabled={busy || !selected} onClick={() => onCreate(productId, channel)} className="flex h-9 w-full items-center justify-center gap-2 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white transition-opacity disabled:opacity-40">{busy && <RefreshCw size={13} className="animate-spin" />}{copy.createRecharge}</button>
    </div>
  );
}

export function AccountPlatformPopover({ open, client, copy, language, theme, collapsed, onClose, onOpenCenter }) {
  const panelRef = useRef(null);
  const requestRef = useRef({ generation: 0, controller: null });
  const rechargeRequestRef = useRef({ generation: 0, controller: null });
  const pollRef = useRef({ controller: null, timer: null });
  const lifecycleRef = useRef({ generation: 0, active: false });
  const rechargeRef = useRef({ generation: 0, view: 'account' });
  const mutationRef = useRef(null);
  const currentClientRef = useRef(client);
  const authInputRef = useRef(0);
  const mobileReadRef = useRef({ generation: 0, controller: null });
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [balance, setBalance] = useState(null);
  const [account, setAccount] = useState('');
  const [loginMode, setLoginMode] = useState('password');
  const [mobile, setMobile] = useState('');
  const [passwordConfirmation, setPasswordConfirmation] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [smsChallenge, setSmsChallenge] = useState('');
  const [smsRetryAt, setSmsRetryAt] = useState(0);
  const [smsNow, setSmsNow] = useState(Date.now());
  const [captcha, setCaptcha] = useState(null);
  const [captchaOffset, setCaptchaOffset] = useState(0);
  const [registrationCheck, setRegistrationCheck] = useState('idle');
  const [mobileLoading, setMobileLoading] = useState(false);
  const [password, setPassword] = useState('');
  const [loading, setLoading] = useState(false);
  const [mutating, setMutating] = useState(false);
  const [errorReason, setErrorReason] = useState(null);
  const [view, setView] = useState('account');
  const [products, setProducts] = useState([]);
  const [order, setOrder] = useState(null);
  const [qrData, setQrData] = useState('');
  const [qrError, setQrError] = useState(false);
  const [qrRetry, setQrRetry] = useState(0);
  const [rechargeLoading, setRechargeLoading] = useState(false);
  const [rechargeErrorReason, setRechargeErrorReason] = useState(null);
  const [rechargeNotice, setRechargeNotice] = useState('');
  const busy = loading || mutating || rechargeLoading;
  const rechargeBusy = busy;
  const authBusy = busy || mobileLoading;
  const smsCooldown = Math.max(0, Math.ceil((smsRetryAt - smsNow) / 1000));
  useEffect(() => {
    if (!open || smsRetryAt <= Date.now()) return undefined;
    setSmsNow(Date.now());
    const timer = window.setInterval(() => setSmsNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [open, smsRetryAt]);
  const error = localizedError(copy, errorReason);
  const rechargeError = localizedError(copy, rechargeErrorReason);
  const isDark = theme === 'dark';

  const cancelMobileRead = useCallback(() => {
    mobileReadRef.current.controller?.abort();
    mobileReadRef.current.generation += 1;
    setMobileLoading(false);
  }, []);
  const resetSMSVerification = useCallback(() => {
    authInputRef.current += 1;
    cancelMobileRead();
    setSmsChallenge(''); setSmsCode(''); setCaptcha(null); setCaptchaOffset(0);
    setRegistrationCheck('idle');
  }, [cancelMobileRead]);
  const cancelAccountRead = useCallback(() => {
    requestRef.current.controller?.abort();
    requestRef.current.generation += 1;
  }, []);
  const cancelRechargeReads = useCallback(() => {
    rechargeRequestRef.current.controller?.abort();
    rechargeRequestRef.current.generation += 1;
    pollRef.current.controller?.abort();
    window.clearTimeout(pollRef.current.timer);
  }, []);
  const resetOrder = useCallback(() => {
    rechargeRef.current.generation += 1;
    cancelRechargeReads();
    setRechargeLoading(false); setOrder(null); setQrData(''); setQrError(false);
    setRechargeErrorReason(null); setRechargeNotice('');
  }, [cancelRechargeReads]);
  const clearProtected = useCallback(() => {
    lifecycleRef.current.generation += 1;
    cancelAccountRead(); resetOrder();
    rechargeRef.current.view = 'account';
    setView('account'); setAuth('guest'); setMe(null); setBalance(null);
    setProducts([]); setPassword(''); setPasswordConfirmation(''); resetSMSVerification(); setLoading(false);
  }, [cancelAccountRead, resetOrder, resetSMSVerification]);

  // A closed or replaced client owns no UI writes, even if native work finishes later.
  useLayoutEffect(() => {
    currentClientRef.current = client;
    const life = lifecycleRef.current;
    life.generation += 1; life.active = open;
    cancelAccountRead(); resetOrder();
    rechargeRef.current.view = 'account';
    setView('account'); setMe(null); setBalance(null); setProducts([]);
    setPassword(''); setPasswordConfirmation(''); setMobile(''); resetSMSVerification(); setSmsRetryAt(0); setLoading(false); setErrorReason(null);
    setMutating(Boolean(mutationRef.current));
    if (open) setAuth('loading');
    return () => {
      life.active = false; life.generation += 1;
      rechargeRef.current.generation += 1;
      cancelAccountRead(); cancelRechargeReads(); cancelMobileRead(); authInputRef.current += 1;
    };
  }, [open, client, cancelAccountRead, cancelRechargeReads, cancelMobileRead, resetOrder, resetSMSVerification]);

  // Locale changes render stored reasons with the new copy; they do not restart work.
  const loadAccount = useCallback(async () => {
    if (!lifecycleRef.current.active) return;
    cancelAccountRead();
    const controller = new window.AbortController();
    const generation = requestRef.current.generation + 1;
    const life = lifecycleRef.current.generation;
    requestRef.current = { generation, controller };
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === life &&
      requestRef.current.generation === generation && !controller.signal.aborted;
    setLoading(true); setErrorReason(null);
    try {
      const session = await client.session(controller.signal);
      if (!isCurrent()) return;
      if (!session.authenticated) {
        clearProtected();
        if (session.errorCode && session.errorCode !== 'account_platform_disabled') setErrorReason(session.errorCode);
        return;
      }
      const nextAccount = await client.bootstrap(controller.signal);
      if (!isCurrent()) return;
      const results = await Promise.allSettled([client.balance(controller.signal)]);
      if (!isCurrent()) return;
      const failed = results.find(result => result.status === 'rejected');
      if (failed && needsAccountLogin(failed.reason)) {
        clearProtected(); setErrorReason(failed.reason); return;
      }
      setMe(nextAccount); setBalance(results[0].status === 'fulfilled' ? results[0].value : null);
      setAuth('ready');
      if (failed) setErrorReason({ copyKey: 'billingUnavailable' });
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) {
        if (needsAccountLogin(reason)) clearProtected();
        else { setMe(null); setBalance(null); setAuth('error'); }
        setErrorReason(reason);
      }
    } finally {
      if (isCurrent()) setLoading(false);
    }
  }, [client, clearProtected, cancelAccountRead]);

  useEffect(() => {
    if (open) void loadAccount();
    return cancelAccountRead;
  }, [open, loadAccount, cancelAccountRead]);

  const loadRecharge = useCallback(async () => {
    if (!lifecycleRef.current.active || rechargeRef.current.view !== 'recharge' || mutationRef.current) return;
    cancelRechargeReads();
    const controller = new window.AbortController();
    const generation = rechargeRequestRef.current.generation + 1;
    const life = lifecycleRef.current.generation;
    const recharge = rechargeRef.current.generation;
    rechargeRequestRef.current = { generation, controller };
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === life &&
      rechargeRef.current.view === 'recharge' && rechargeRef.current.generation === recharge &&
      rechargeRequestRef.current.generation === generation && !controller.signal.aborted;
    setRechargeLoading(true); setRechargeErrorReason(null); setRechargeNotice('');
    try {
      const results = await Promise.allSettled([client.products(controller.signal), client.balance(controller.signal)]);
      if (!isCurrent()) return;
      const denied = results.find(result => result.status === 'rejected' && needsAccountLogin(result.reason));
      if (denied) { clearProtected(); setErrorReason(denied.reason); return; }
      setProducts(results[0].status === 'fulfilled' ? results[0].value.items || [] : []);
      if (results[1].status === 'fulfilled') setBalance(results[1].value);
      if (results.some(result => result.status === 'rejected')) setRechargeErrorReason({ copyKey: 'billingUnavailable' });
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) {
        if (needsAccountLogin(reason)) { clearProtected(); setErrorReason(reason); }
        else setRechargeErrorReason(reason);
      }
    } finally {
      if (isCurrent()) setRechargeLoading(false);
    }
  }, [client, cancelRechargeReads, clearProtected]);

  useEffect(() => {
    const paymentContent = order?.payment_url || order?.qr_content || '';
    const embedded = safePNGImage(order?.qr_data);
    if (!open || auth !== 'ready' || view !== 'recharge' || terminalOrderStates.has(order?.status) || orderRemainingSeconds(order, Date.now()) === 0 ) {
      setQrData(''); setQrError(false); return undefined;
    }
    if (order?.qr_data) { setQrData(embedded); setQrError(!embedded); return undefined; }
    if (typeof paymentContent !== 'string' || !paymentContent || paymentContent.length > 8192) { setQrData(''); setQrError(true); return undefined; }
    let active = true;
    const life = lifecycleRef.current.generation;
    const recharge = rechargeRef.current.generation;
    const isCurrent = () => active && lifecycleRef.current.active && lifecycleRef.current.generation === life &&
      rechargeRef.current.view === 'recharge' && rechargeRef.current.generation === recharge;
    setQrData(''); setQrError(false);
    import('qrcode')
      .then(({ default: QRCode }) => QRCode.toDataURL(paymentContent, { width: 220, margin: 2, errorCorrectionLevel: 'M' }))
      .then(value => { if (isCurrent()) setQrData(value); })
      .catch(() => { if (isCurrent()) setQrError(true); });
    return () => { active = false; };
  }, [open, auth, view, client, order?.order_id, order?.status, order?.expires_at, order?.qr_data, order?.payment_url, order?.qr_content, qrRetry]);

  // Both a just-created paid order and a polled paid order need the same balance refresh.
  const acceptOrder = useCallback(async (latest, isCurrent, signal) => {
    if (!isCurrent()) return;
    if (paidOrderStates.has(latest.status)) {
      try {
        const refreshedBalance = await client.balance(signal);
        if (!isCurrent()) return;
        setBalance(refreshedBalance); setRechargeNotice('paymentReceived'); setRechargeErrorReason(null);
      } catch (reason) {
        if (!isCurrent()) return;
        if (needsAccountLogin(reason)) { clearProtected(); setErrorReason(reason); return; }
        setRechargeNotice(''); setRechargeErrorReason({ copyKey: 'balanceRefreshFailed' });
      }
    }
    if (isCurrent()) setOrder(latest);
  }, [client, clearProtected]);

  useEffect(() => {
    if (!open || auth !== 'ready' || view !== 'recharge' || busy || !order?.order_id ||
      terminalOrderStates.has(order.status) || orderRemainingSeconds(order, Date.now()) === 0) return undefined;
    const controller = new window.AbortController();
    const life = lifecycleRef.current.generation;
    const recharge = rechargeRef.current.generation;
    let stopped = false; let attempt = 0;
    const isCurrent = () => !stopped && !controller.signal.aborted && lifecycleRef.current.active &&
      lifecycleRef.current.generation === life && rechargeRef.current.view === 'recharge' &&
      rechargeRef.current.generation === recharge;
    const schedule = () => { pollRef.current.timer = window.setTimeout(poll, rechargePollDelay(attempt++)); };
    const poll = async () => {
      try {
        const latest = await client.rechargeOrder(order.order_id, controller.signal);
        if (!isCurrent()) return;
        await acceptOrder(latest, isCurrent, controller.signal);
        if (!isCurrent() || paidOrderStates.has(latest.status)) return;
        setRechargeErrorReason(null);
        if (!terminalOrderStates.has(latest.status) && orderRemainingSeconds(latest, Date.now()) > 0) schedule();
      } catch (reason) {
        if (!isCurrent()) return;
        if (needsAccountLogin(reason)) { clearProtected(); setErrorReason(reason); return; }
        setRechargeErrorReason({ copyKey: 'paymentStatusUnavailable' });
        if (orderRemainingSeconds(order, Date.now()) > 0) schedule();
      }
    };
    pollRef.current.controller = controller; schedule();
    return () => { stopped = true; controller.abort(); window.clearTimeout(pollRef.current.timer); };
  }, [open, auth, view, busy, client, order?.order_id, order?.status, order?.expires_at, clearProtected, acceptOrder]);

  useEffect(() => {
    if (!open) return undefined;
    const handleMouseDown = event => { if (panelRef.current && !panelRef.current.contains(event.target)) onClose?.(); };
    const handleKeyDown = event => { if (event.key !== 'Escape') return; if (captcha && !authBusy) { setCaptcha(null); setCaptchaOffset(0); } else onClose?.(); };
    window.addEventListener('mousedown', handleMouseDown); window.addEventListener('keydown', handleKeyDown);
    return () => { window.removeEventListener('mousedown', handleMouseDown); window.removeEventListener('keydown', handleKeyDown); };
  }, [open, onClose, captcha, authBusy]);

  const runMutation = async action => {
    if (mutationRef.current || loading || mobileLoading || rechargeLoading || !lifecycleRef.current.active) return;
    const owner = {};
    mutationRef.current = owner;
    const generation = lifecycleRef.current.generation;
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === generation;
    cancelAccountRead(); cancelRechargeReads(); cancelMobileRead();
    setLoading(false); setRechargeLoading(false); setMutating(true); setErrorReason(null);
    try { await action(isCurrent); }
    catch (reason) {
      if (isCurrent()) {
        if (needsAccountLogin(reason)) clearProtected();
        setErrorReason(reason);
      }
    } finally {
      if (mutationRef.current === owner) {
        mutationRef.current = null;
        if (lifecycleRef.current.active) setMutating(false);
      }
    }
  };
  // A registration read belongs to both its input tuple and this popup lifecycle.
  const readMobile = async action => {
    if (!lifecycleRef.current.active || mutationRef.current || loading) return;
    cancelMobileRead();
    const controller = new window.AbortController();
    const generation = mobileReadRef.current.generation;
    const life = lifecycleRef.current.generation; const input = authInputRef.current;
    mobileReadRef.current.controller = controller;
    const isCurrent = () => !controller.signal.aborted && lifecycleRef.current.active &&
      lifecycleRef.current.generation === life && authInputRef.current === input &&
      mobileReadRef.current.generation === generation;
    setMobileLoading(true); setErrorReason(null);
    try { await action(isCurrent, controller.signal); }
    catch (reason) { if (isCurrent() && reason?.name !== 'AbortError') { setRegistrationCheck('error'); setErrorReason(reason); } }
    finally { if (isCurrent()) setMobileLoading(false); }
  };
  const precheck = async (isCurrent, signal) => {
    const inputError = registrationInputError(account, mobile, password, passwordConfirmation);
    if (inputError) { if (isCurrent()) { setRegistrationCheck('error'); setErrorReason({ copyKey: inputError }); } return false; }
    setRegistrationCheck('checking');
    const result = await client.precheckMobileRegistration(account.trim(), mobile.trim(), password, signal);
    if (!isCurrent()) return false;
    setAccount(result.username); setRegistrationCheck('ready');
    return true;
  };
  const precheckRegistration = () => readMobile(precheck);
  const openSliderCaptcha = () => {
    if (authBusy || !mobile.trim() || smsCooldown > 0) return;
    void readMobile(async (isCurrent, signal) => {
      if (loginMode === 'register' && !(await precheck(isCurrent, signal))) return;
      const result = await client.createSliderCaptcha(mobile.trim(), loginMode === 'register' ? 'REGISTER' : 'LOGIN', signal);
      if (isCurrent()) { setCaptcha(result); setCaptchaOffset(0); }
    });
  };
  const sendSms = () => {
    if (authBusy || !captcha?.captcha_id || smsCooldown > 0) return;
    const inputError = loginMode === 'register' ? registrationInputError(account, mobile, password, passwordConfirmation) : '';
    if (inputError) { setErrorReason({ copyKey: inputError }); return; }
    if (new Date(captcha.expires_at).getTime() <= Date.now()) { setCaptcha(null); setErrorReason({ copyKey: 'captchaInvalid' }); return; }
    const input = authInputRef.current;
    void runMutation(async isAlive => {
      const isCurrent = () => isAlive() && authInputRef.current === input;
      try {
        const result = await client.sendSms(loginMode === 'register' ? account.trim() : '', mobile.trim(),
          loginMode === 'register' ? password : '', loginMode === 'register' ? 'REGISTER' : 'LOGIN', captcha.captcha_id, captchaOffset);
        if (!isCurrent()) return;
        setSmsChallenge(result.challenge_id); setSmsCode('');
        setSmsNow(Date.now()); setSmsRetryAt(Date.now() + result.retry_after_seconds * 1000);
        setCaptcha(null); setCaptchaOffset(0);
      } catch (reason) {
        if (!isCurrent()) return;
        if (reason?.code === 'SMS_RATE_LIMITED' && Number.isInteger(reason.retryAfterSeconds) && reason.retryAfterSeconds > 0 && reason.retryAfterSeconds <= 86400) {
          setSmsNow(Date.now()); setSmsRetryAt(Date.now() + reason.retryAfterSeconds * 1000);
        }
        setCaptcha(null); setCaptchaOffset(0); setErrorReason(reason);
      }
    });
  };
  const submitLogin = async event => {
    event.preventDefault();
    if (authBusy || mutationRef.current) return;
    const mode = loginMode;
    if (mode === 'register') {
      const invalid = registrationInputError(account, mobile, password, passwordConfirmation);
      if (invalid) { setRegistrationCheck('error'); setErrorReason({ copyKey: invalid }); return; }
    }
    if (mode === 'password' ? !account.trim() || !password : !mobile.trim() || !smsChallenge || !smsCode.trim()) return;
    const loginAccount = account.trim().replace(/^\+?86(?=1\d{10}$)/u, '');
    const loginMobile = mobile.trim(); const loginPassword = password;
    const challenge = smsChallenge; const code = smsCode.trim();
    setPassword(''); setPasswordConfirmation(''); setSmsCode('');
    await runMutation(async isCurrent => {
      if (mode === 'sms') await client.loginSms(loginMobile, challenge, code);
      else if (mode === 'register') {
        const registration = await client.registerMobile(account.trim(), loginMobile, challenge, code, loginPassword);
        if (currentClientRef.current !== client) return;
        if (registration.password_preserved) {
          // A recovered registration retained the first submitted password.
          // This typed password must not be attempted against that identity.
          if (lifecycleRef.current.active) {
            clearProtected(); setAccount(registration.username); setLoginMode('password');
            setSmsRetryAt(0); setErrorReason({ copyKey: 'registrationRecovered' });
          }
          return;
        }
        try { await client.login(registration.username, loginPassword); }
        catch {
          if (lifecycleRef.current.active && currentClientRef.current === client) {
            clearProtected(); setAccount(registration.username); setLoginMode('password');
            setErrorReason({ copyKey: 'registrationCompletedLoginFailed' });
          }
          return;
        }
      } else await client.login(loginAccount, loginPassword);
      // Login completion changes this client's identity even after close/reopen.
      if (lifecycleRef.current.active && currentClientRef.current === client) {
        resetSMSVerification(); await loadAccount();
      }
    });
  };
  const logout = () => runMutation(async () => {
    await client.logout();
    // A completed logout is authoritative for a reopened view of the same client.
    // A replaced client owns a different identity and must remain untouched.
    if (lifecycleRef.current.active && currentClientRef.current === client) clearProtected();
  });
  const backToAccount = () => { resetOrder(); rechargeRef.current.view = 'account'; setView('account'); };
  const openRecharge = () => {
    if (busy || mutationRef.current || auth !== 'ready') return;
    resetOrder(); rechargeRef.current.view = 'recharge'; setView('recharge'); void loadRecharge();
  };
  const createRecharge = (productId, channel) => {
    const recharge = rechargeRef.current.generation;
    return runMutation(async isAlive => {
      const isCurrent = () => isAlive() && rechargeRef.current.view === 'recharge' && rechargeRef.current.generation === recharge;
      setRechargeErrorReason(null); setRechargeNotice('');
      try {
        const nextOrder = await client.createRecharge(productId, channel, newIdempotencyKey());
        await acceptOrder(nextOrder, isCurrent);
      } catch (reason) {
        if (!isCurrent()) return;
        if (needsAccountLogin(reason)) { clearProtected(); setErrorReason(reason); }
        else setRechargeErrorReason(reason);
      }
    });
  };
  const refreshRechargeBalance = () => {
    const recharge = rechargeRef.current.generation;
    return runMutation(async isAlive => {
      const isCurrent = () => isAlive() && rechargeRef.current.view === 'recharge' && rechargeRef.current.generation === recharge;
      try {
        const nextBalance = await client.balance();
        if (!isCurrent()) return;
        setBalance(nextBalance); setRechargeErrorReason(null); setRechargeNotice('paymentReceived');
      } catch (reason) {
        if (!isCurrent()) return;
        if (needsAccountLogin(reason)) { clearProtected(); setErrorReason(reason); }
        else { setRechargeErrorReason({ copyKey: 'balanceRefreshFailed' }); setRechargeNotice(''); }
      }
    });
  };

  if (!open) return null;
  const displayName = me?.display_name || copy.user;
  const panelPosition = collapsed ? 'left-full bottom-0 ml-3' : 'left-0 bottom-full mb-2';
  const panelTheme = isDark ? 'bg-[#202124]/95 border-white/10 text-[#E3E3E3]' : 'bg-white/95 border-black/10 text-[#1F1F1F]';
  const cardTheme = isDark ? 'bg-[#131314] border-white/10' : 'bg-[#F8FAFD] border-black/5';
  const mutedText = isDark ? 'text-[#9AA0A6]' : 'text-[#6F7378]';
  const controlTheme = isDark ? 'bg-[#202124] border-white/10 text-[#E3E3E3] placeholder:text-[#6F7378]' : 'bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]';

  return (
    <div ref={panelRef} data-testid="account-platform-popover" className={`absolute ${panelPosition} z-[90] max-h-[calc(100vh-24px)] w-[336px] max-w-[calc(100vw-24px)] overflow-y-auto rounded-[22px] border p-3 shadow-2xl backdrop-blur-xl ${panelTheme}`}>
      <div className="flex items-center gap-3 px-1.5 pb-3">
        {view === 'recharge' && auth === 'ready' ? (
          <button type="button" disabled={busy} onClick={backToAccount} className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.backToAccount}><ArrowLeft size={17} /></button>
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
          <div className="grid grid-cols-3 gap-1 rounded-xl bg-black/5 p-1">{['password', 'sms', 'register'].map(mode => <button key={mode} type="button" disabled={busy} onClick={() => { setLoginMode(mode); setPassword(''); setPasswordConfirmation(''); resetSMSVerification(); setErrorReason(null); }} className={`h-7 rounded-lg text-[11px] ${loginMode === mode ? 'bg-white shadow-sm' : mutedText}`}>{copy.authModes[mode]}</button>)}</div>
          {loginMode !== 'sms' && <input value={account} onChange={event => { setAccount(event.target.value); setRegistrationCheck('idle'); resetSMSVerification(); }} onBlur={() => { if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration(); }} disabled={busy} placeholder={loginMode === 'register' ? copy.registrationUsername : copy.mobileOrAccount} aria-label={loginMode === 'register' ? copy.username : copy.mobileOrAccount} autoComplete="username" className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />}
          {loginMode !== 'password' && <input value={mobile} onChange={event => { setMobile(event.target.value); setRegistrationCheck('idle'); resetSMSVerification(); }} onBlur={() => { if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration(); }} disabled={busy} placeholder={copy.mobile} aria-label={copy.mobile} inputMode="tel" autoComplete="tel-national" className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />}
          {loginMode !== 'sms' && <input value={password} onChange={event => { setPassword(event.target.value); setRegistrationCheck('idle'); setErrorReason(null); resetSMSVerification(); }} onBlur={() => { if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration(); }} disabled={busy} placeholder={copy.password} aria-label={copy.password} type="password" autoComplete={loginMode === 'register' ? 'new-password' : 'current-password'} className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />}
          {loginMode === 'register' && <input value={passwordConfirmation} onChange={event => { setPasswordConfirmation(event.target.value); setRegistrationCheck('idle'); setErrorReason(null); resetSMSVerification(); }} onBlur={() => { if (account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration(); }} disabled={busy} placeholder={copy.confirmPassword} aria-label={copy.confirmPassword} type="password" autoComplete="new-password" className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />}
          {loginMode === 'register' && registrationCheck === 'checking' && <div className={`text-[11px] ${mutedText}`}>{copy.registrationPrechecking}</div>}
          {loginMode === 'register' && registrationCheck === 'ready' && <div className="text-[11px] text-[#07966F]">{copy.registrationReady}</div>}
          {loginMode !== 'password' && <div className="flex gap-2"><input value={smsCode} onChange={event => setSmsCode(event.target.value)} disabled={busy} placeholder={copy.smsCode} aria-label={copy.smsCode} inputMode="numeric" autoComplete="one-time-code" className={`h-9 min-w-0 flex-1 rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} /><button type="button" disabled={authBusy || registrationCheck === 'checking' || smsCooldown > 0 || !mobile.trim() || (loginMode === 'register' && (!account.trim() || !password || !passwordConfirmation || password !== passwordConfirmation))} onClick={() => void openSliderCaptcha()} className="h-9 shrink-0 rounded-xl bg-[#10B981]/10 px-3 text-[11px] font-semibold text-[#07966F] disabled:opacity-40">{smsCooldown > 0 ? copy.resendCountdown(smsCooldown) : smsChallenge ? copy.resendCode : copy.sendCode}</button></div>}
          {auth === 'error' && <button type="button" disabled={authBusy} onClick={() => void loadAccount()}>{copy.retry}</button>}
          {error && <div role="alert" className="break-words text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <button type="submit" disabled={authBusy || (loginMode === 'password' ? !account.trim() || !password : !mobile.trim() || !smsChallenge || !smsCode.trim() || (loginMode === 'register' && (!account.trim() || !password || !passwordConfirmation || password !== passwordConfirmation || registrationCheck !== 'ready')))} className="h-9 w-full rounded-xl bg-[#10B981] text-[13px] font-semibold text-white transition-opacity disabled:opacity-40">{authBusy ? copy.signingIn : loginMode === 'register' ? copy.register : copy.login}</button>
        </form>
      )}

      {(auth === 'guest' || auth === 'error') && loginMode !== 'password' && captcha && (
        <div role="dialog" aria-modal="true" aria-label={copy.captchaTitle} className={`absolute inset-0 z-20 flex items-center justify-center rounded-[22px] p-3 backdrop-blur-xl ${isDark ? 'bg-[#202124]/95' : 'bg-white/95'}`}>
          <SliderCaptcha challenge={captcha} offset={captchaOffset} busy={authBusy} copy={copy} cardTheme={cardTheme} mutedText={mutedText} onOffset={setCaptchaOffset} onConfirm={() => void sendSms()} onRefresh={() => void openSliderCaptcha()} onClose={() => { if (authBusy) return; setCaptcha(null); setCaptchaOffset(0); }} />
        </div>
      )}

      {auth === 'ready' && view === 'account' && (
        <div className="space-y-2.5">
          {error && <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">{error}</div>}
          <div className={`rounded-2xl border p-3 ${cardTheme}`}>
            <div className="flex items-center justify-between gap-3">
              <div className={`text-[12px] ${mutedText}`}>{copy.balance}</div>
              <button type="button" disabled={busy} onClick={openRecharge} className="flex h-7 items-center gap-1 rounded-lg bg-[#10B981]/10 px-2 text-[11px] font-semibold text-[#07966F] transition-colors hover:bg-[#10B981]/20"><Plus size={13} />{copy.recharge}</button>
            </div>
            <div className="mt-1 flex items-end justify-between gap-3">
              <div className="text-[22px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
              <div className={`text-right text-[11px] ${mutedText}`}>{copy.reservedCredits}<div className="mt-1 text-[13px] font-medium tabular-nums">{formatPoints(balance?.reserved_microcredits, language)}</div></div>
            </div>
          </div>
          {onOpenCenter && <button type="button" disabled={busy} onClick={onOpenCenter} className="h-9 w-full rounded-xl text-[12px] font-medium">{copy.title}</button>}
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
          notice={rechargeNotice ? copy[rechargeNotice] : ''}
          cardTheme={cardTheme}
          controlTheme={controlTheme}
          mutedText={mutedText}
          isDark={isDark}
          onCreate={(productId, channel) => void createRecharge(productId, channel)}
          onRetryLoad={() => void loadRecharge()}
          onRetryQR={() => setQrRetry(value => value + 1)}
          onRefreshBalance={() => void refreshRechargeBalance()}
          onResetOrder={resetOrder}
          onDone={backToAccount}
        />
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
