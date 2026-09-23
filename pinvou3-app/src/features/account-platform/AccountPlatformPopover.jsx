import { secureRequestID } from './request-id.js';
import React, { useCallback, useEffect, useRef, useState } from 'react';
import { ArrowLeft, CheckCircle2, Plus, RefreshCw, User, X } from '../../components/icons.jsx';
import { formatPoints } from './credit-units.js';
import { rechargePollDelay } from './context-guard.js';
import { authorizationVersions, personalSurfaceAccess } from './authorization-ui.js';
import { hasSelectablePaymentChannels, isPayablePaymentOrder, orderSummaryStatus, paymentScanPrompt } from './order-view.js';
import { localizedAccountPlatformError } from './error-localization.js';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'EXPIRED', 'MANUAL_REVIEW']);
const paidOrderStates = new Set(['PAID', 'PAID_LATE']);
const fallbackChannels = ['ALIPAY'];

function localizedError(copy, reason) {
  return localizedAccountPlatformError(copy, reason, {
    USERNAME_ALREADY_REGISTERED: copy.usernameAlreadyRegistered,
    USERNAME_INVALID: copy.usernameInvalid,
    PASSWORD_INVALID: copy.passwordInvalid,
    PASSWORD_EDGE_SPACE_NOT_ALLOWED: copy.passwordEdgeSpace,
    PASSWORD_MISMATCH: copy.passwordMismatch,
    PASSWORD_REFERENCE_INVALID: copy.registrationPrecheckExpired,
    PASSWORD_REFERENCE_CONFLICT: copy.registrationPrecheckExpired,
    MOBILE_ALREADY_REGISTERED: copy.mobileAlreadyRegistered,
    MOBILE_INVALID: copy.mobileInvalid,
    CAPTCHA_INVALID: copy.captchaInvalid,
    SMS_RATE_LIMITED: copy.smsRateLimited,
    SMS_UNAVAILABLE: copy.smsUnavailable,
    SMS_CODE_INVALID: copy.mobileAuthFailed,
    mobile_auth_code_invalid: copy.mobileAuthFailed,
    mobile_auth_failed: copy.mobileAuthFailed,
    REGISTRATION_IDEMPOTENCY_CONFLICT: copy.registrationConflict,
    REGISTRATION_RESTART_REQUIRED: copy.registrationRestartRequired,
    registration_recovery_unavailable: copy.registrationRecoveryUnavailable,
  });
}

function retryAfterFromError(reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const match = raw.match(/SMS_RATE_LIMITED:(\d+)/);
  return match ? Math.max(1, Number(match[1])) : 0;
}

function registrationInputError(copy, username, mobile, password, passwordConfirmation) {
  if (!/^[A-Za-z][A-Za-z0-9._-]{2,31}$/.test(username.trim())) return copy.usernameInvalid;
  if (!/^1[3-9]\d{9}$/.test(mobile.trim().replace(/^\+?86/, ''))) return copy.mobileInvalid;
  if (password.startsWith(' ') || password.endsWith(' ')) return copy.passwordEdgeSpace;
  if (password.length < 8 || password.length > 128 || !/[A-Za-z]/.test(password) || !/\d/.test(password) || !/^[ -~]+$/.test(password)) return copy.passwordInvalid;
  if (password !== passwordConfirmation) return copy.passwordMismatch;
  return '';
}

function SliderCaptcha({ challenge, offset, busy, copy, cardTheme, mutedText, onOffset, onConfirm, onRefresh, onClose }) {
  const [dragOffset, setDragOffset] = useState(null);
  const [dragging, setDragging] = useState(false);
  const width = Number(challenge?.width || 280);
  const height = Number(challenge?.height || 120);
  const pieceWidth = Number(challenge?.piece_width || 44);
  const pieceHeight = Number(challenge?.piece_height || 44);
  const maximum = Math.max(1, width - pieceWidth);
  const challengeID = challenge?.captcha_id || challenge?.id || '';
  const shownOffset = dragOffset ?? Number(offset || 0);

  useEffect(() => {
    setDragOffset(null);
    setDragging(false);
  }, [challengeID]);

  const updateOffset = (value) => {
    setDragOffset(value);
    onOffset(value);
  };

  const releaseSlider = () => {
    if (!dragging || busy) return;
    setDragging(false);
    onConfirm(shownOffset);
  };

  return (
    <div className={`w-full space-y-2 rounded-xl border p-2 shadow-xl ${cardTheme}`}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] font-medium">{copy.captchaTitle}</span>
        <div className="flex items-center gap-2">
          <button type="button" disabled={busy} onClick={onRefresh} className={`text-[11px] ${mutedText}`}>
            {copy.captchaRefresh}
          </button>
          <button type="button" disabled={busy} onClick={onClose} aria-label={copy.captchaClose} title={copy.captchaClose} className={`flex h-6 w-6 items-center justify-center rounded-full ${mutedText}`}>
            <X size={13} />
          </button>
        </div>
      </div>
      <div className="relative w-full overflow-hidden rounded-lg" style={{ aspectRatio: `${width}/${height}` }}>
        <img src={challenge.background_image} alt="" draggable="false" className="absolute inset-0 h-full w-full select-none" />
        <img
          src={challenge.piece_image}
          alt=""
          draggable="false"
          className="pointer-events-none absolute select-none drop-shadow-md"
          style={{
            left: `${(shownOffset / width) * 100}%`,
            top: `${(Number(challenge.piece_y || 0) / height) * 100}%`,
            width: `${(pieceWidth / width) * 100}%`,
            height: `${(pieceHeight / height) * 100}%`,
          }}
        />
      </div>
      <input
        type="range"
        min="0"
        max={maximum}
        step="1"
        value={shownOffset}
        disabled={busy}
        onChange={(event) => updateOffset(Number(event.target.value))}
        onPointerDown={() => setDragging(true)}
        onPointerUp={releaseSlider}
        onPointerCancel={() => setDragging(false)}
        aria-label={copy.captchaSlider}
        className="h-6 w-full cursor-grab accent-[#10B981] disabled:cursor-not-allowed"
      />
      <button type="button" disabled={busy} onClick={onConfirm} className="h-8 w-full rounded-lg bg-[#10B981] text-[12px] font-semibold text-white disabled:opacity-40">
        {busy ? copy.sendingCode : copy.captchaConfirm}
      </button>
    </div>
  );
}

function newIdempotencyKey() {
  return `desktop-recharge-${secureRequestID()}`;
}

function orderRemainingSeconds(order, now) {
  const expiresAt = new Date(order?.expires_at).getTime();
  if (!Number.isFinite(expiresAt)) return 0;
  return Math.max(0, Math.ceil((expiresAt - now) / 1000));
}

function formatCountdown(seconds) {
  return `${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}`;
}

function RechargePanel({ copy, language, products, balance, order, qrData, qrError, loading, busy, error, notice, cardTheme, controlTheme, mutedText, isDark, onCreate, onRetryLoad, onRetryQR, onRefreshBalance, onResetOrder, onDone }) {
  const [productId, setProductId] = useState('');
  const [channel, setChannel] = useState('ALIPAY');
  const [now, setNow] = useState(Date.now());
  const selected = products.find((product) => product.product_id === productId);
  const selectedAvailable = selected?.availability_status === 'AVAILABLE';
  const allPaused = products.length > 0 && !products.some((product) => product.availability_status === 'AVAILABLE');
  const channels = selectedAvailable && selected?.channels?.length ? selected.channels : fallbackChannels;
  const remainingSeconds = orderRemainingSeconds(order, now);
  const paid = paidOrderStates.has(order?.status);
  const displayStatus = orderSummaryStatus(order);
  const canScan = isPayablePaymentOrder(order, now);
  const scanApps = products.find((product) => product.product_id === order?.product_id)?.qr_scan_apps || [];
  const inactiveHint = copy.orderNoLongerPayable;

  useEffect(() => {
    if (!selectedAvailable) setProductId(products.find((product) => product.availability_status === 'AVAILABLE')?.product_id || '');
  }, [products, selectedAvailable]);

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
    return (
      <div className={`flex min-h-56 items-center justify-center gap-2 rounded-2xl border text-[13px] ${cardTheme} ${mutedText}`}>
        <RefreshCw size={15} className="animate-spin" />
        {copy.loading}
      </div>
    );
  }

  if (paid) {
    return (
      <div className="space-y-2.5">
        {error && (
          <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">
            {error}
          </div>
        )}
        <div className={`flex flex-col items-center rounded-2xl border px-4 py-5 text-center ${cardTheme}`}>
          <CheckCircle2 size={38} className="text-[#10B981]" />
          <div className="mt-3 text-[15px] font-semibold">{copy.orderStatuses[order.status]}</div>
          <div className={`mt-1 text-[12px] leading-relaxed ${mutedText}`}>{notice || (error ? copy.paymentConfirmed : copy.paymentReceived)}</div>
          <div className="mt-4 text-[24px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
          <div className={`text-[11px] ${mutedText}`}>{copy.balance}</div>
        </div>
        {error && (
          <button type="button" disabled={busy} onClick={onRefreshBalance} className="flex h-9 w-full items-center justify-center gap-2 rounded-xl bg-[#10B981]/10 text-[12px] font-semibold text-[#07966F] disabled:opacity-40">
            {busy && <RefreshCw size={13} className="animate-spin" />}
            {copy.retry}
          </button>
        )}
        <div className="grid grid-cols-2 gap-2">
          <button type="button" onClick={onResetOrder} className={`h-9 rounded-xl text-[12px] font-medium transition-colors ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>
            {copy.rechargeAgain}
          </button>
          <button type="button" onClick={onDone} className="h-9 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">
            {copy.done}
          </button>
        </div>
      </div>
    );
  }

  if (order) {
    return (
      <div className="space-y-2.5">
        {error && (
          <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">
            {error}
          </div>
        )}
        <div className={`rounded-2xl border p-3 ${cardTheme}`}>
          <div className="flex items-center justify-between gap-3">
            <span className={`rounded-full px-2 py-1 text-[11px] font-medium ${canScan ? 'bg-[#10B981]/10 text-[#07966F]' : 'bg-[#EA4335]/10 text-[#D93025]'}`}>{copy.orderSummaryStatuses?.[displayStatus] || copy.orderStatuses[displayStatus] || displayStatus}</span>
            <strong className="text-[15px] tabular-nums">¥{(order.amount_fen / 100).toFixed(2)}</strong>
          </div>
          {canScan ? (
            <>
              <div className="mt-3 flex min-h-[190px] items-center justify-center rounded-2xl bg-white p-2">
                {qrData && <img src={qrData} alt={copy.paymentQR} className="h-[180px] w-[180px]" />}
                {!qrData && !qrError && (
                  <div className="flex items-center gap-2 text-[12px] text-[#6F7378]">
                    <RefreshCw size={14} className="animate-spin" />
                    {copy.qrGenerating}
                  </div>
                )}
                {qrError && (
                  <div className="px-4 text-center text-[12px] text-[#D93025]">
                    <p>{copy.qrFailed}</p>
                    <button type="button" onClick={onRetryQR} className="mt-2 rounded-lg bg-[#D93025]/10 px-3 py-1.5 font-medium">
                      {copy.retry}
                    </button>
                  </div>
                )}
              </div>
              <div className="mt-3 text-center text-[13px] font-medium">{paymentScanPrompt(copy, scanApps)}</div>
              <div className={`mt-1 text-center text-[11px] tabular-nums ${mutedText}`}>{remainingSeconds > 0 ? copy.countdown(formatCountdown(remainingSeconds)) : copy.orderStatuses.PENDING}</div>
            </>
          ) : (
            <div className={`py-8 text-center text-[12px] ${mutedText}`}>{inactiveHint}</div>
          )}
        </div>
        {!canScan && (
          <button type="button" onClick={onResetOrder} className="h-9 w-full rounded-xl bg-[#10B981] text-[12px] font-semibold text-white">
            {copy.chooseAgain}
          </button>
        )}
      </div>
    );
  }

  if (!products.length) {
    return (
      <div className={`rounded-2xl border p-4 text-center ${cardTheme}`}>
        <p className={`text-[12px] leading-relaxed ${mutedText}`}>{error || copy.rechargeUnavailable}</p>
        <button type="button" onClick={onRetryLoad} className="mt-3 h-8 rounded-xl bg-[#10B981] px-4 text-[12px] font-semibold text-white">
          {copy.retry}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-2.5">
      {error && (
        <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">
          {error}
        </div>
      )}
      {allPaused && (
        <div role="status" className="break-words rounded-xl bg-[#F59E0B]/10 px-3 py-2 text-[12px] leading-snug text-[#B45309]">
          {copy.paymentChannelMaintenance}
        </div>
      )}
      <div className={`rounded-2xl border p-3 ${cardTheme}`}>
        <div className="flex items-end justify-between gap-3">
          <div>
            <div className={`text-[11px] ${mutedText}`}>{copy.balance}</div>
            <div className="mt-0.5 text-[20px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
          </div>
          <div className={`text-[11px] ${mutedText}`}>{copy.points}</div>
        </div>
      </div>
      <div>
        <div className={`mb-1.5 px-1 text-[11px] font-medium ${mutedText}`}>{copy.selectAmount}</div>
        <div className="grid grid-cols-2 gap-2">
          {products.map((product) => (
            <button key={product.product_id} type="button" disabled={product.availability_status !== 'AVAILABLE'} aria-pressed={product.product_id === productId} onClick={() => setProductId(product.product_id)} className={`rounded-xl border px-2 py-2.5 text-left transition-colors disabled:cursor-not-allowed disabled:opacity-55 ${product.product_id === productId ? 'border-[#10B981] bg-[#10B981]/10' : cardTheme}`}>
              <strong className="block text-[15px] tabular-nums">¥{(product.amount_fen / 100).toFixed(2)}</strong>
              <span className={`mt-0.5 block text-[11px] ${mutedText}`}>
                {product.display_points.toLocaleString()} {copy.points}
              </span>
              {product.availability_status !== 'AVAILABLE' && <span className="mt-1 block text-[10px] text-[#B45309]">{copy.paymentChannelMaintenance}</span>}
            </button>
          ))}
        </div>
      </div>
      {hasSelectablePaymentChannels(channels) && (
        <label className="block">
          <span className={`mb-1.5 block px-1 text-[11px] font-medium ${mutedText}`}>{copy.paymentMethod}</span>
          <select value={channel} onChange={(event) => setChannel(event.target.value)} className={`h-9 w-full rounded-xl border px-3 text-[12px] outline-none ${controlTheme}`}>
            {channels.map((item) => (
              <option key={item} value={item}>
                {copy.channels[item] || item}
              </option>
            ))}
          </select>
        </label>
      )}
      <button type="button" disabled={busy || !selectedAvailable} onClick={() => onCreate(productId, channel)} className="flex h-9 w-full items-center justify-center gap-2 rounded-xl bg-[#10B981] text-[12px] font-semibold text-white transition-opacity disabled:opacity-40">
        {busy && <RefreshCw size={13} className="animate-spin" />}
        {copy.createRecharge}
      </button>
    </div>
  );
}

export function AccountPlatformPopover({ open, client, copy, language, theme, collapsed, onClose }) {
  const panelRef = useRef(null);
  const requestRef = useRef({ generation: 0, controller: null });
  const contextSyncRef = useRef({
    versions: {},
    loading: false,
    pending: false,
  });
  const rechargeRequestRef = useRef({ generation: 0, controller: null });
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [balance, setBalance] = useState(null);
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [passwordConfirmation, setPasswordConfirmation] = useState('');
  const [loginMode, setLoginMode] = useState('password');
  const [mobile, setMobile] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [smsChallenge, setSmsChallenge] = useState('');
  const [smsCooldown, setSmsCooldown] = useState(0);
  const [captcha, setCaptcha] = useState(null);
  const [captchaOffset, setCaptchaOffset] = useState(0);
  const [registrationCheck, setRegistrationCheck] = useState('idle');
  const [passwordReference, setPasswordReference] = useState(null);
  const [registrationRecovery, setRegistrationRecovery] = useState(null);
  const [registrationProgress, setRegistrationProgress] = useState(null);
  const registrationCheckRef = useRef(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
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
    contextSyncRef.current.loading = true;
    setBusy(true);
    setError('');
    try {
      const session = await client.session(controller.signal);
      if (!isCurrent()) return;
      if (!session.authenticated) {
        setAuth('guest');
        setMe(null);
        setBalance(null);
        contextSyncRef.current.versions = {};
        contextSyncRef.current.pending = false;
        if (session.errorCode && session.errorCode !== 'account_platform_disabled') setError(localizedError(copy, session.errorCode));
        return;
      }
      const nextAccount = await client.bootstrap(controller.signal);
      if (!isCurrent()) return;
      contextSyncRef.current.versions = authorizationVersions(nextAccount);
      const nextAccess = personalSurfaceAccess(nextAccount);
      if (nextAccount?.status === 'PROVISIONING' || nextAccount?.status === 'REPAIR_REQUIRED') {
        setMe(nextAccount);
        setBalance(null);
        setAuth('ready');
        return;
      }
      const results = await Promise.allSettled([nextAccess.billingRead ? client.balance(controller.signal) : Promise.resolve(null)]);
      if (!isCurrent()) return;
      setMe(nextAccount);
      setBalance(results[0].status === 'fulfilled' ? results[0].value : null);
      setAuth('ready');
      if (results.some((result) => result.status === 'rejected')) setError(copy.requestFailed);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) {
        setAuth('error');
        setError(localizedError(copy, reason));
      }
    } finally {
      if (isCurrent()) {
        setBusy(false);
        contextSyncRef.current.loading = false;
        if (contextSyncRef.current.pending) {
          contextSyncRef.current.pending = false;
          void loadAccount();
        } else {
          contextSyncRef.current.pending = false;
        }
      }
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
      const results = await Promise.allSettled([client.products(controller.signal), client.balance(controller.signal)]);
      if (!isCurrent()) return;
      const value = (index) => (results[index].status === 'fulfilled' ? results[index].value : null);
      setProducts(value(0)?.items || []);
      setOrder(null);
      if (value(1)) setBalance(value(1));
      if (results.some((result) => result.status === 'rejected')) setRechargeError(copy.billingUnavailable);
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
    if (!open) return undefined;
    const refreshAuthorization = () => {
      if (contextSyncRef.current.loading) {
        contextSyncRef.current.pending = true;
        return;
      }
      void loadAccount();
    };
    const unsubscribeContext = client.onContextVersion?.(refreshAuthorization);
    const unsubscribeAuthorization = client.onAuthorizationVersion?.(refreshAuthorization);
    const refreshOnFocus = () => void loadAccount();
    window.addEventListener('focus', refreshOnFocus);
    return () => {
      unsubscribeContext?.();
      unsubscribeAuthorization?.();
      window.removeEventListener('focus', refreshOnFocus);
    };
  }, [client, loadAccount, open]);

  useEffect(() => {
    if (!open || (auth !== 'guest' && auth !== 'error')) return undefined;
    let stopped = false;
    let timer;
    const poll = async () => {
      try {
        const status = await client.registrationStatus();
        if (stopped || !status) return;
        setRegistrationProgress(status);
        if (status.registration_operation_id) setRegistrationRecovery((current) => (current?.registration_operation_id === status.registration_operation_id ? current : { registration_operation_id: status.registration_operation_id }));
        if (status.username) setAccount(status.username);
        if (status.status === 'COMPENSATED' || status.status === 'EXPIRED') {
          setRegistrationRecovery(null);
          return;
        }
        if (status.status === 'ACTIVE') {
          setLoginMode('password');
          return;
        }
        const suggested = Date.parse(status.next_retry_at || '') - Date.now();
        timer = window.setTimeout(poll, Math.min(30000, Math.max(2000, Number.isFinite(suggested) ? suggested : 5000)));
      } catch (reason) {
        if (!stopped) {
          setRegistrationProgress((current) => current || { status: 'RECOVERY_UNAVAILABLE' });
          timer = window.setTimeout(poll, 10000);
        }
      }
    };
    void poll();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [auth, client, open, registrationRecovery?.registration_operation_id]);

  useEffect(() => {
    if (smsCooldown <= 0) return undefined;
    const timer = window.setInterval(() => setSmsCooldown((current) => Math.max(0, current - 1)), 1000);
    return () => window.clearInterval(timer);
  }, [smsCooldown]);

  useEffect(() => {
    const paymentContent = isPayablePaymentOrder(order) ? order?.qr_data || '' : '';
    if (!paymentContent) {
      setQrData('');
      setQrError(false);
      return undefined;
    }
    setQrData(paymentContent);
    setQrError(!paymentContent.startsWith('data:image/png;base64,'));
    return undefined;
  }, [order?.qr_data, order?.status, qrRetry]);

  useEffect(() => {
    if (!open || auth !== 'ready' || !order?.order_id || terminalOrderStates.has(order.status)) return undefined;
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
        if (!terminalOrderStates.has(latest.status)) timer = window.setTimeout(poll, rechargePollDelay(attempt++));
      } catch {
        if (!stopped) {
          setRechargeError(copy.paymentStatusUnavailable);
          timer = window.setTimeout(poll, rechargePollDelay(attempt++));
        }
      }
    };
    timer = window.setTimeout(poll, rechargePollDelay(attempt++));
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [auth, client, copy, open, order?.order_id, order?.status]);

  useEffect(() => {
    if (!open) return undefined;
    const handleMouseDown = (event) => {
      if (panelRef.current && !panelRef.current.contains(event.target)) onClose?.();
    };
    const handleKeyDown = (event) => {
      if (event.key !== 'Escape') return;
      if (captcha && !busy) {
        setCaptcha(null);
        setCaptchaOffset(0);
        return;
      }
      onClose?.();
    };
    window.addEventListener('mousedown', handleMouseDown);
    window.addEventListener('keydown', handleKeyDown);
    return () => {
      window.removeEventListener('mousedown', handleMouseDown);
      window.removeEventListener('keydown', handleKeyDown);
    };
  }, [busy, captcha, open, onClose]);

  useEffect(() => {
    if (view === 'recharge' && !personalSurfaceAccess(me).rechargeCreate) {
      setView('account');
    }
  }, [me, view]);

  if (!open) return null;

  const access = personalSurfaceAccess(me);
  const displayName = me?.display_name || copy.user;
  const panelPosition = collapsed ? 'left-full bottom-0 ml-3' : 'left-0 bottom-full mb-2';
  const panelTheme = isDark ? 'bg-[#202124]/95 border-white/10 text-[#E3E3E3]' : 'bg-white/95 border-black/10 text-[#1F1F1F]';
  const cardTheme = isDark ? 'bg-[#131314] border-white/10' : 'bg-[#F8FAFD] border-black/5';
  const mutedText = isDark ? 'text-[#9AA0A6]' : 'text-[#6F7378]';
  const controlTheme = isDark ? 'bg-[#202124] border-white/10 text-[#E3E3E3] placeholder:text-[#6F7378]' : 'bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]';
  const registrationBlocksRestart = Boolean(registrationProgress && !registrationProgress.user_id && ['REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING', 'IDENTITY_CREATED', 'ACCOUNT_COMMITTING', 'REPAIR_REQUIRED', 'RECOVERY_UNAVAILABLE'].includes(registrationProgress.status));
  const registrationProgressMessage = registrationProgress?.status === 'REPAIR_REQUIRED' ? copy.registrationRepairing : registrationProgress?.status === 'RECOVERY_UNAVAILABLE' ? copy.registrationRecoveryUnavailable : copy.registrationAccountCreating;

  const resetSMSVerification = () => {
    setSmsChallenge('');
    setSmsCode('');
    setCaptcha(null);
    setCaptchaOffset(0);
  };

  const invalidateRegistrationPrecheck = () => {
    setRegistrationCheck('idle');
    setPasswordReference(null);
  };

  const precheckRegistration = async () => {
    const localError = registrationInputError(copy, account, mobile, password, passwordConfirmation);
    if (localError) {
      setRegistrationCheck('error');
      setError(localError);
      return false;
    }
    const generation = registrationCheckRef.current + 1;
    registrationCheckRef.current = generation;
    setRegistrationCheck('checking');
    setError('');
    try {
      const result = await client.precheckMobileRegistration(account.trim(), mobile.trim(), password);
      if (registrationCheckRef.current !== generation) return false;
      if (result?.username) setAccount(result.username);
      setPasswordReference({
        password_handle: result?.password_handle,
        password_policy_version: result?.password_policy_version,
        expires_at: result?.expires_at,
      });
      setRegistrationCheck('ready');
      return true;
    } catch (reason) {
      if (registrationCheckRef.current !== generation) return false;
      setRegistrationCheck('error');
      setError(localizedError(copy, reason));
      return false;
    }
  };

  const openSliderCaptcha = async () => {
    if (busy || !mobile.trim() || smsCooldown > 0) return;
    setBusy(true);
    setError('');
    try {
      if (loginMode === 'register' && registrationCheck !== 'ready' && !(await precheckRegistration())) return;
      const purpose = loginMode === 'register' ? 'REGISTER' : loginMode === 'forgot' ? 'PASSWORD_RESET' : 'LOGIN';
      const result = await client.createSliderCaptcha(mobile.trim(), purpose);
      setCaptcha(result);
      setCaptchaOffset(0);
    } catch (reason) {
      setError(localizedError(copy, reason));
    } finally {
      setBusy(false);
    }
  };

  const submitLogin = async (event) => {
    event.preventDefault();
    if (busy) return;
    if (loginMode === 'register') {
      const localError = registrationInputError(copy, account, mobile, password, passwordConfirmation);
      if (localError) {
        setRegistrationCheck('error');
        setError(localError);
        return;
      }
    }
    setBusy(true);
    setError('');
    try {
      if (loginMode === 'forgot') {
        if (password !== passwordConfirmation) throw new Error('PASSWORD_MISMATCH');
        await client.resetPassword(mobile.trim(), smsChallenge, smsCode.trim(), password);
        setLoginMode('password');
        resetSMSVerification();
        setNotice(copy.passwordResetComplete);
      } else if (loginMode === 'sms') await client.loginSms(mobile.trim(), smsChallenge, smsCode.trim());
      else if (loginMode === 'register') {
        const registration = await client.registerMobile(account.trim(), mobile.trim(), smsChallenge, smsCode.trim(), passwordReference, registrationRecovery);
        setRegistrationProgress(registration);
        if (!registration?.user_id) {
          setPassword('');
          setPasswordConfirmation('');
          setAuth('guest');
          setBusy(false);
          return;
        }
        const registeredUsername = registration?.username || account.trim();
        try {
          await client.login(registeredUsername, password);
        } catch {
          setAccount(registeredUsername);
          setLoginMode('password');
          setPassword('');
          setPasswordConfirmation('');
          setAuth('guest');
          resetSMSVerification();
          setRegistrationCheck('idle');
          setError(copy.registrationCompletedLoginFailed);
          setBusy(false);
          return;
        }
      } else await client.login(account.trim().replace(/^\+?86(?=1\d{10}$)/, ''), password);
      setPassword('');
      setPasswordConfirmation('');
      if (loginMode !== 'forgot') await loadAccount();
    } catch (reason) {
      setPassword('');
      setPasswordConfirmation('');
      setAuth('guest');
      setError(localizedError(copy, reason));
      setBusy(false);
    }
  };

  const sendSms = async () => {
    if (busy || !mobile.trim() || !captcha?.captcha_id || smsCooldown > 0) return;
    if (loginMode === 'register') {
      const localError = registrationInputError(copy, account, mobile, password, passwordConfirmation);
      if (localError) {
        setRegistrationCheck('error');
        setError(localError);
        return;
      }
    }
    setBusy(true);
    setError('');
    try {
      const purpose = loginMode === 'register' ? 'REGISTER' : loginMode === 'forgot' ? 'PASSWORD_RESET' : 'LOGIN';
      const result = await client.sendSms(loginMode === 'register' ? account.trim() : '', mobile.trim(), loginMode === 'register' ? passwordReference : null, purpose, captcha.captcha_id, captchaOffset);
      setSmsChallenge(result.challenge_id || result.challengeId || '');
      if (loginMode === 'register') {
        setRegistrationRecovery({
          registration_operation_id: result.registration_operation_id,
          registration_query_token: result.registration_query_token || '',
        });
        setRegistrationProgress({
          registration_operation_id: result.registration_operation_id,
          status: 'SMS_PENDING',
          next_retry_at: result.expires_at,
        });
      }
      setSmsCooldown(Math.max(1, Number(result.retry_after_seconds || 60)));
      setCaptcha(null);
      setCaptchaOffset(0);
    } catch (reason) {
      const retryAfter = retryAfterFromError(reason);
      if (retryAfter > 0) setSmsCooldown(retryAfter);
      setCaptcha(null);
      setCaptchaOffset(0);
      setError(localizedError(copy, reason));
    } finally {
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
    if (!personalSurfaceAccess(me).rechargeCreate) {
      setError(copy.permissionDenied);
      return;
    }
    setOrder(null);
    setQrData('');
    setQrError(false);
    setRechargeError('');
    setRechargeNotice('');
    setView('recharge');
    void loadRecharge();
  };

  const createRecharge = async (productId, channel) => {
    if (rechargeBusy || !personalSurfaceAccess(me).rechargeCreate) return;
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
          <button type="button" onClick={() => setView('account')} className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.backToAccount}>
            <ArrowLeft size={17} />
          </button>
        ) : (
          <div className="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-gradient-to-br from-[#19D3A2] to-[#0B57D0] text-white shadow-lg shadow-emerald-500/20">
            <User size={20} />
          </div>
        )}
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold">{view === 'recharge' && auth === 'ready' ? copy.rechargeTitle : auth === 'ready' ? displayName : copy.title}</div>
          {auth === 'ready' && view !== 'recharge' && (
            <div className={`truncate font-mono text-[11px] ${mutedText}`} title={me?.user_code || ''}>
              {me?.user_code || '—'}
            </div>
          )}
          {auth !== 'ready' && <div className={`truncate text-[11px] ${mutedText}`}>PINVOU ACCOUNT PLATFORM</div>}
        </div>
        <button type="button" onClick={onClose} className={`flex h-8 w-8 items-center justify-center rounded-full transition-colors ${isDark ? 'hover:bg-white/10' : 'hover:bg-black/5'}`} title={copy.close}>
          <X size={16} />
        </button>
      </div>

      {auth === 'loading' && (
        <div className={`flex min-h-36 items-center justify-center gap-2 rounded-2xl border text-[13px] ${cardTheme} ${mutedText}`}>
          <RefreshCw size={15} className="animate-spin" />
          {copy.loading}
        </div>
      )}

      {(auth === 'guest' || auth === 'error') && registrationBlocksRestart && (
        <div className={`rounded-2xl border p-4 text-center ${cardTheme}`}>
          <RefreshCw size={22} className="mx-auto animate-spin text-[#10B981]" />
          <div className="mt-3 text-[13px] font-semibold">{registrationProgressMessage}</div>
          <div className={`mt-1 text-[11px] leading-relaxed ${mutedText}`}>{copy.registrationDoNotRepeat}</div>
        </div>
      )}

      {(auth === 'guest' || auth === 'error') && !registrationBlocksRestart && (
        <form className={`space-y-3 rounded-2xl border p-3 ${cardTheme}`} onSubmit={(event) => void submitLogin(event)}>
          <div className="grid grid-cols-4 gap-1 rounded-xl bg-black/5 p-1">
            {['password', 'sms', 'register', 'forgot'].map((mode) => (
              <button
                key={mode}
                type="button"
                onClick={() => {
                  setLoginMode(mode);
                  setPasswordConfirmation('');
                  resetSMSVerification();
                  invalidateRegistrationPrecheck();
                  setError('');
                  setNotice('');
                }}
                className={`h-7 rounded-lg text-[11px] ${loginMode === mode ? 'bg-white shadow-sm' : mutedText}`}
              >
                {copy.authModes[mode]}
              </button>
            ))}
          </div>
          {loginMode !== 'sms' && loginMode !== 'forgot' && (
            <input
              value={account}
              onChange={(event) => {
                setAccount(event.target.value);
                invalidateRegistrationPrecheck();
                resetSMSVerification();
              }}
              onBlur={() => {
                if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration();
              }}
              disabled={busy}
              placeholder={loginMode === 'register' ? copy.registrationUsername : copy.mobileOrAccount}
              aria-label={loginMode === 'register' ? copy.username : copy.mobileOrAccount}
              autoComplete="username"
              className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
            />
          )}
          {loginMode !== 'password' && (
            <input
              value={mobile}
              onChange={(event) => {
                setMobile(event.target.value);
                invalidateRegistrationPrecheck();
                resetSMSVerification();
              }}
              onBlur={() => {
                if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration();
              }}
              disabled={busy}
              placeholder={copy.mobile}
              aria-label={copy.mobile}
              inputMode="tel"
              autoComplete="tel-national"
              className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
            />
          )}
          {loginMode !== 'sms' && (
            <input
              value={password}
              onChange={(event) => {
                setPassword(event.target.value);
                invalidateRegistrationPrecheck();
                setError('');
                if (loginMode === 'register') resetSMSVerification();
              }}
              onBlur={() => {
                if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration();
              }}
              disabled={busy}
              placeholder={copy.password}
              aria-label={copy.password}
              type="password"
              autoComplete={loginMode === 'register' ? 'new-password' : 'current-password'}
              className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
            />
          )}
          {(loginMode === 'register' || loginMode === 'forgot') && (
            <input
              value={passwordConfirmation}
              onChange={(event) => {
                setPasswordConfirmation(event.target.value);
                invalidateRegistrationPrecheck();
                setError('');
                resetSMSVerification();
              }}
              onBlur={() => {
                if (loginMode === 'register' && account.trim() && mobile.trim() && password && passwordConfirmation) void precheckRegistration();
              }}
              disabled={busy}
              placeholder={copy.confirmPassword}
              aria-label={copy.confirmPassword}
              type="password"
              autoComplete="new-password"
              className={`h-9 w-full rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`}
            />
          )}
          {loginMode === 'register' && registrationCheck === 'checking' && <div className={`text-[11px] ${mutedText}`}>{copy.registrationPrechecking}</div>}
          {loginMode === 'register' && registrationCheck === 'ready' && <div className="text-[11px] text-[#07966F]">{copy.registrationReady}</div>}
          {loginMode !== 'password' && (
            <div className="flex gap-2">
              <input value={smsCode} onChange={(event) => setSmsCode(event.target.value)} disabled={busy} placeholder={copy.smsCode} aria-label={copy.smsCode} inputMode="numeric" autoComplete="one-time-code" className={`h-9 min-w-0 flex-1 rounded-xl border px-3 text-[13px] outline-none ${controlTheme}`} />
              <button type="button" disabled={busy || registrationCheck === 'checking' || smsCooldown > 0 || !mobile.trim() || (loginMode === 'register' && (!account.trim() || !password || !passwordConfirmation || password !== passwordConfirmation)) || (loginMode === 'forgot' && (!password || password !== passwordConfirmation))} onClick={() => void openSliderCaptcha()} className="h-9 shrink-0 rounded-xl bg-[#10B981]/10 px-3 text-[11px] font-semibold text-[#07966F] disabled:opacity-40">
                {smsCooldown > 0 ? copy.resendCountdown(smsCooldown) : smsChallenge ? copy.resendCode : copy.sendCode}
              </button>
            </div>
          )}
          {error && (
            <div role="alert" className="break-words text-[12px] leading-snug text-[#EA4335]">
              {error}
            </div>
          )}
          {notice && <div className="break-words text-[12px] leading-snug text-[#07966F]">{notice}</div>}
          <button type="submit" disabled={busy || (loginMode === 'password' ? !account.trim() || !password : !mobile.trim() || !smsChallenge || !smsCode.trim() || (loginMode === 'register' && (!account.trim() || !password || !passwordConfirmation || password !== passwordConfirmation || registrationCheck !== 'ready')) || (loginMode === 'forgot' && (!password || password !== passwordConfirmation)))} className="h-9 w-full rounded-xl bg-[#10B981] text-[13px] font-semibold text-white transition-opacity disabled:opacity-40">
            {busy ? copy.signingIn : loginMode === 'register' ? copy.register : loginMode === 'forgot' ? copy.resetPassword : copy.login}
          </button>
        </form>
      )}

      {captcha && (
        <div role="dialog" aria-modal="true" aria-label={copy.captchaTitle} className={`absolute inset-0 z-20 flex items-center justify-center rounded-[22px] p-3 backdrop-blur-xl ${isDark ? 'bg-[#202124]/95' : 'bg-white/95'}`}>
          <SliderCaptcha
            challenge={captcha}
            offset={captchaOffset}
            busy={busy}
            copy={copy}
            cardTheme={cardTheme}
            mutedText={mutedText}
            onOffset={setCaptchaOffset}
            onConfirm={() => void sendSms()}
            onRefresh={() => void openSliderCaptcha()}
            onClose={() => {
              if (busy) return;
              setCaptcha(null);
              setCaptchaOffset(0);
            }}
          />
        </div>
      )}

      {auth === 'ready' && view === 'account' && (
        <div className="space-y-2.5">
          {error && (
            <div role="alert" className="break-words rounded-xl bg-[#EA4335]/10 px-3 py-2 text-[12px] leading-snug text-[#EA4335]">
              {error}
            </div>
          )}
          {me?.status !== 'ACTIVE' ? (
            <div className={`rounded-2xl border p-4 text-center ${cardTheme}`}>
              <RefreshCw size={22} className="mx-auto text-[#10B981]" />
              <div className="mt-3 text-[13px] font-semibold">{me?.status === 'REPAIR_REQUIRED' ? copy.accountProvisioningRepair : copy.accountProvisioning}</div>
              <div className={`mt-1 text-[11px] leading-relaxed ${mutedText}`}>{copy.accountProvisioningCapabilities}</div>
            </div>
          ) : (
            <>
              <div className={`rounded-2xl border p-3 ${cardTheme}`}>
                <div className="flex items-center justify-between gap-3">
                  <div className={`text-[12px] ${mutedText}`}>{copy.balance}</div>
                  <button type="button" onClick={openRecharge} disabled={!access.rechargeCreate} title={!access.rechargeCreate ? copy.permissionDenied : undefined} className="flex h-7 items-center gap-1 rounded-lg bg-[#10B981]/10 px-2 text-[11px] font-semibold text-[#07966F] transition-colors hover:bg-[#10B981]/20">
                    <Plus size={13} />
                    {copy.recharge}
                  </button>
                </div>
                <div className="mt-1 flex items-end justify-between gap-3">
                  <div className="text-[22px] font-semibold tabular-nums">{formatPoints(balance?.available_microcredits, language)}</div>
                  <div className={`text-right text-[11px] ${mutedText}`}>
                    {copy.reservedCredits}
                    <div className="mt-1 text-[13px] font-medium tabular-nums">{formatPoints(balance?.reserved_microcredits, language)}</div>
                  </div>
                </div>
              </div>
            </>
          )}
          <button type="button" onClick={() => void client.openPortal(language).catch((reason) => setError(localizedError(copy, reason)))} disabled={busy} className="h-9 w-full rounded-xl bg-[#10B981]/10 text-[12px] font-semibold text-[#07966F] transition-colors hover:bg-[#10B981]/20 disabled:opacity-40">
            {copy.openWebAccount}
          </button>
          <button type="button" onClick={() => void logout()} disabled={busy} className={`h-9 w-full rounded-xl text-[12px] font-medium transition-colors disabled:opacity-40 ${isDark ? 'bg-white/5 hover:bg-white/10' : 'bg-black/5 hover:bg-black/10'}`}>
            {copy.logout}
          </button>
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
          onRetryQR={() => setQrRetry((value) => value + 1)}
          onRefreshBalance={() => void refreshRechargeBalance()}
          onResetOrder={() => {
            setOrder(null);
            setQrData('');
            setQrError(false);
            setRechargeError('');
            setRechargeNotice('');
          }}
          onDone={() => setView('account')}
        />
      )}
    </div>
  );
}
