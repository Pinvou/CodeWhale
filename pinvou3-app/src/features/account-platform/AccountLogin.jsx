import React, { useEffect, useRef, useState } from 'react';
import { PasswordInput } from './PasswordInput.jsx';
import { ProfileCaptcha } from './ProfileSettings.jsx';
import { localizedAccountPlatformError } from './error-localization.js';
import { profileCaptchaSubmissionOffset } from './profile-captcha-state.js';

export function AccountLogin({ client, copy, onAuthenticated }) {
  const [mode, setMode] = useState('password');
  const [account, setAccount] = useState('');
  const [mobile, setMobile] = useState('');
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [code, setCode] = useState('');
  const [captcha, setCaptcha] = useState(null);
  const [offset, setOffset] = useState(0);
  const [challenge, setChallenge] = useState('');
  const [reference, setReference] = useState(null);
  const [recovery, setRecovery] = useState(null);
  const [progress, setProgress] = useState(null);
  const [cooldown, setCooldown] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const owner = useRef(0);
  useEffect(() => { owner.current++; return () => { owner.current++; }; }, [client]);
  useEffect(() => {
    if (cooldown <= 0) return;
    const timer = setTimeout(() => setCooldown(value => Math.max(0, value - 1)), 1000);
    return () => clearTimeout(timer);
  }, [cooldown]);
  useEffect(() => {
    let stopped = false;
    let timer;
    const poll = async () => {
      try {
        const result = await client.registrationStatus();
        if (stopped || !result) return;
        setProgress(result);
        if (result.username) setAccount(result.username);
        if (result.registration_operation_id) setRecovery(value => value?.registration_operation_id === result.registration_operation_id ? value : { registration_operation_id: result.registration_operation_id });
        if (['ACTIVE', 'COMPENSATED', 'EXPIRED'].includes(result.status)) { if (result.user_id) setMode('password'); return; }
        const suggested = Date.parse(result.next_retry_at || '') - Date.now();
        timer = setTimeout(poll, Math.min(30000, Math.max(2000, Number.isFinite(suggested) ? suggested : 5000)));
      } catch {
        if (!stopped) { setProgress(value => value || { status: 'RECOVERY_UNAVAILABLE' }); timer = setTimeout(poll, 10000); }
      }
    };
    void poll();
    return () => { stopped = true; clearTimeout(timer); };
  }, [client, recovery?.registration_operation_id]);
  const pending = Boolean(progress && !progress.user_id && ['REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING', 'IDENTITY_CREATED', 'ACCOUNT_COMMITTING', 'REPAIR_REQUIRED', 'RECOVERY_UNAVAILABLE'].includes(progress.status));
  const run = async action => {
    const generation = owner.current;
    const current = () => owner.current === generation;
    setBusy(true); setError(''); setNotice('');
    try { await action(current); }
    catch (reason) {
      if (current()) {
        setError(localizedAccountPlatformError(copy, reason));
        if (Number.isFinite(reason?.retryAfterSeconds) && reason.retryAfterSeconds > 0) setCooldown(Math.ceil(reason.retryAfterSeconds));
      }
    } finally { if (current()) setBusy(false); }
  };
  const registrationError = () => {
    if (!/^[A-Za-z][A-Za-z0-9._-]{2,31}$/.test(account.trim())) return copy.usernameInvalid;
    if (!/^1[3-9]\d{9}$/.test(mobile.trim().replace(/^\+?86/, ''))) return copy.mobileInvalid;
    if (password.startsWith(' ') || password.endsWith(' ')) return copy.passwordEdgeSpace;
    if (password.length < 8 || password.length > 128 || !/[A-Za-z]/.test(password) || !/\d/.test(password) || !/^[ -~]+$/.test(password)) return copy.passwordInvalid;
    return password !== confirmation ? copy.passwordMismatch : '';
  };
  const openCaptcha = () => {
    if (mode === 'register' && registrationError()) { setError(registrationError()); return; }
    void run(async current => {
      let checked = reference;
      if (mode === 'register' && !checked) {
        checked = await client.precheckMobileRegistration(account.trim(), mobile.trim(), password);
        if (!current()) return;
        setReference(checked);
      }
      const result = await client.createSliderCaptcha(mobile.trim(), mode === 'register' ? 'REGISTER' : mode === 'forgot' ? 'PASSWORD_RESET' : 'LOGIN');
      if (current()) { setCaptcha(result); setOffset(0); }
    });
  };
  const sendCode = selectedOffset => void run(async current => {
    const purpose = mode === 'register' ? 'REGISTER' : mode === 'forgot' ? 'PASSWORD_RESET' : 'LOGIN';
    const result = await client.sendSms(mode === 'register' ? account.trim() : '', mobile.trim(), mode === 'register' ? reference : null, purpose, captcha.captcha_id, profileCaptchaSubmissionOffset(selectedOffset, offset));
    if (!current()) return;
    if (!result?.challenge_id) throw new Error('INVALID_SMS_ACK');
    setChallenge(result.challenge_id); setCaptcha(null); setOffset(0);
    setCooldown(Math.max(1, Number(result.retry_after_seconds || 60)));
    if (mode === 'register') {
      setRecovery({ registration_operation_id: result.registration_operation_id, registration_query_token: result.registration_query_token || '' });
    }
  });
  const recover = () => void run(async current => {
    const result = await client.registrationStatus();
    if (!current()) return;
    setProgress(result);
    if (result?.user_id) { setMode('password'); setNotice(copy.registrationCompletedLoginFailed); }
  });
  const submit = event => {
    event.preventDefault();
    if (mode === 'register' && registrationError()) { setError(registrationError()); return; }
    void run(async current => {
      if (mode === 'forgot') {
        if (password !== confirmation) { setError(copy.passwordMismatch); return; }
        await client.resetPassword(mobile.trim(), challenge, code.trim(), password);
        if (!current()) return;
        setMode('password'); setPassword(''); setConfirmation(''); setChallenge(''); setCode(''); setNotice(copy.passwordResetComplete);
        return;
      }
      if (mode === 'register') {
        let result;
        try { result = await client.registerMobile(account.trim(), mobile.trim(), challenge, code.trim(), reference, recovery); }
        catch (reason) {
          if (!current()) return;
          try { const saved = await client.registrationStatus(); if (current() && saved) setProgress(saved); }
          catch { if (current()) setProgress({ status: 'RECOVERY_UNAVAILABLE' }); }
          throw reason;
        }
        if (!current()) return;
        setProgress(result);
        if (!result?.user_id) return;
        try { await client.login(result.username || account.trim(), password); }
        catch { if (current()) { setMode('password'); setPassword(''); setConfirmation(''); setError(copy.registrationCompletedLoginFailed); } return; }
      } else if (mode === 'sms') await client.loginSms(mobile.trim(), challenge, code.trim());
      else await client.login(account.trim().replace(/^\+?86(?=1\d{10}$)/, ''), password);
      if (current()) { setPassword(''); setConfirmation(''); await onAuthenticated(); }
    });
  };
  const changeMode = value => {
    if (busy || pending) return;
    owner.current++; setMode(value); setPassword(''); setConfirmation(''); setCode(''); setChallenge(''); setReference(null); setRecovery(null); setProgress(null); setCaptcha(null); setError(''); setNotice('');
  };
  const changeFacts = setter => event => {
    owner.current++; setter(event.target.value); setReference(null); setChallenge(''); setCode(''); setCaptcha(null); setRecovery(null);
  };
  return <section className="ap-stack">
    <h1>{copy.title}</h1><p>{copy.desktopLoginIntro}</p>
    <nav>{[['password', copy.login], ['sms', copy.authModes.sms], ['register', copy.register], ['forgot', copy.authModes.forgot]].map(([value, label]) => <button key={value} type="button" disabled={busy || pending} aria-pressed={mode === value} onClick={() => changeMode(value)}>{label}</button>)}</nav>
    {error && <div className="ap-alert error" role="alert">{error}</div>}{notice && <div className="ap-alert" role="status">{notice}</div>}
    {pending ? <div role="status"><p>{copy.registrationDoNotRepeat}</p><p>{progress.status}</p><button disabled={busy} onClick={recover}>{copy.retry}</button></div> : <form className="ap-stack" onSubmit={submit}>
      {(mode === 'password' || mode === 'register') && <label>{mode === 'register' ? copy.registrationUsername : copy.account}<input required value={account} autoComplete="username" disabled={busy} onChange={changeFacts(setAccount)} /></label>}
      {mode !== 'password' && <label>{copy.mobile}<input required value={mobile} inputMode="tel" autoComplete="tel-national" disabled={busy || Boolean(challenge)} onChange={changeFacts(setMobile)} /></label>}
      {mode !== 'sms' && <label>{copy.password}<PasswordInput resetKey={mode} showLabel={copy.showPassword} hideLabel={copy.hidePassword} ariaLabel={copy.password} autoComplete={mode === 'password' ? 'current-password' : 'new-password'} maxLength={128} value={password} disabled={busy || mode === 'register' && Boolean(challenge)} onValueChange={value => { owner.current++; setPassword(value); setReference(null); setChallenge(''); setCode(''); setCaptcha(null); setRecovery(null); }} /></label>}
      {(mode === 'register' || mode === 'forgot') && <label>{copy.confirmPassword}<PasswordInput resetKey={mode} showLabel={copy.showPasswordConfirmation} hideLabel={copy.hidePasswordConfirmation} ariaLabel={copy.confirmPassword} autoComplete="new-password" value={confirmation} maxLength={128} disabled={busy || mode === 'register' && Boolean(challenge)} onValueChange={value => { owner.current++; setConfirmation(value); setReference(null); setChallenge(''); setCode(''); setCaptcha(null); setRecovery(null); }} /></label>}
      {mode !== 'password' && <><button type="button" disabled={busy || cooldown > 0 || !mobile.trim()} onClick={openCaptcha}>{cooldown > 0 ? copy.resendCountdown(cooldown) : copy.sendCode}</button><label>{copy.smsCode}<input value={code} required autoComplete="one-time-code" inputMode="numeric" disabled={busy} onChange={event => setCode(event.target.value)} /></label></>}
      <button disabled={busy || mode !== 'password' && !challenge}>{busy ? copy.signingIn : mode === 'register' ? copy.register : mode === 'forgot' ? copy.resetPassword : copy.login}</button>
    </form>}
    {captcha && <div className="ap-captcha-backdrop" role="dialog" aria-modal="true" aria-label={copy.captchaTitle}><ProfileCaptcha challenge={captcha} offset={offset} busy={busy} copy={copy} onOffset={setOffset} onConfirm={sendCode} onRefresh={openCaptcha} onClose={() => { if (!busy) setCaptcha(null); }} /></div>}
  </section>;
}
