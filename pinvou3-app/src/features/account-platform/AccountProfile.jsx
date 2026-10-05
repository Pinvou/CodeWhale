import React, { useEffect, useRef, useState } from 'react';
import { safePNGImage } from './bff-client.js';
import { accountErrorMessage } from './errors.js';
import { createContextRequestGuard } from './context-guard.js';

const recovering = status => ['VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'].includes(status);
function validID(value) { return typeof value === 'string' && /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(value) && value !== '00000000-0000-0000-0000-000000000000'; }
function profileChange(value, expected) { if (!value || !validID(value.profile_change_request_id) || (expected && value.profile_change_request_id !== expected) || !['USERNAME','MOBILE'].includes(value.field) || typeof value.new_value !== 'string' || !['PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','COMPLETED','RETRY','REPAIR_REQUIRED'].includes(value.status)) throw new TypeError('invalid profile response'); return value; }
export function MyRoleBindings({ client, copy:allCopy, onError }) {
  const copy = allCopy.myRoleBindings;
  const [items,setItems] = useState([]); const [failed,setFailed] = useState(false); const [loading,setLoading] = useState(true);
  useEffect(() => {
    const controller = new globalThis.AbortController(); setItems([]); setFailed(false); setLoading(true);
    client.myRoleBindings(controller.signal).then(value => { if (!Array.isArray(value?.items)) throw new TypeError('invalid roles'); if (!controller.signal.aborted) setItems(value.items); }).catch(error => { if (!controller.signal.aborted && error?.name !== 'AbortError') { setFailed(true); onError(error); } }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  },[client,onError]);
  return <section><h2>{copy.title}</h2><p>{copy.lead}</p>{failed && <p role="alert">{copy.failed}</p>}{loading ? <p>{allCopy.loading}</p> : !items.length ? <p>{copy.none}</p> : <div className="ap-order-list">{items.map(item => <article className="ap-card" key={item.binding_id}><h3>{item.role_code}</h3><dl><dt>{copy.scope}</dt><dd>{item.scope} {item.organization_id} {item.unit_id}</dd><dt>{copy.validity}</dt><dd>{item.starts_at ? new Date(item.starts_at).toLocaleString() : '?'} / {item.expires_at ? new Date(item.expires_at).toLocaleString() : copy.forever}</dd><dt>{copy.source}</dt><dd>{item.grant_source}</dd><dt>{copy.status}</dt><dd>{item.status}</dd></dl></article>)}</div>}</section>;
}

export function AccountProfile({ client, copy, me, portal, busy, run, onSignedOut, onError }) {
  const [field,setField] = useState('USERNAME'); const [value,setValue] = useState(''); const [password,setPassword] = useState('');
  const [change,setChange] = useState(null); const [captcha,setCaptcha] = useState(null); const [offset,setOffset] = useState(0); const [code,setCode] = useState(''); const [challenge,setChallenge] = useState(null);
  const [error,setError] = useState(null); const [cooldown,setCooldown] = useState(0); const [now,setNow] = useState(Date.now());
  const alive = useRef(false); const guard = useRef(createContextRequestGuard());
  useEffect(() => { alive.current = true; return () => { alive.current = false; guard.current.cancel(); }; },[client]);
  const action = work => run(async parentCurrent => { setError(null); const current = () => alive.current && parentCurrent(); try { await work(current); } catch (reason) { if (current()) setError(reason); throw reason; } });
  const completed = async current => {
    if (!current()) return; setPassword(''); setCode(''); setCaptcha(null);
    try { if (portal) await client.logout(); }
    catch (reason) { if (current()) setChange(previous => ({ ...previous, status:'COMPLETED' })); throw reason; }
    if (current()) onSignedOut();
  };
  const begin = event => {
    event.preventDefault(); if (busy) return;
    void action(async current => {
      const currentPassword = password; setPassword('');
      if (!portal) { await client.login(me.username, currentPassword); if (!current()) return; }
      const result = profileChange(await client.createProfileChange(field,value.trim()));
      if (current()) { setChange(result); setChallenge(null); setCode(''); }
    });
  };
  useEffect(() => {
    if (cooldown <= Date.now()) return undefined;
    const timer = window.setInterval(() => setNow(Date.now()),1000); return () => window.clearInterval(timer);
  },[cooldown]);
  useEffect(() => {
    if (!change || !recovering(change.status)) return undefined;
    const ticket = guard.current.begin(); let timer;
    const poll = async () => {
      try { const result = profileChange(await client.profileChangeStatus(change.profile_change_request_id,ticket.signal),change.profile_change_request_id);
        if (!alive.current || !guard.current.isCurrent(ticket.generation)) return;
        if (result.status === 'COMPLETED') { await completed(() => alive.current && guard.current.isCurrent(ticket.generation)); return; }
        setChange(result); if (recovering(result.status)) timer = window.setTimeout(poll,3000);
      } catch (reason) { if (alive.current && guard.current.isCurrent(ticket.generation) && reason?.name !== 'AbortError') { setError(reason); onError(reason); } }
    };
    timer = window.setTimeout(poll,3000); return () => { ticket.controller?.abort(); guard.current.cancel(); window.clearTimeout(timer); };
  },[client,change?.profile_change_request_id,change?.status]);
  const openCaptcha = () => { if (busy) return; void action(async current => {
    const result = await client.createSliderCaptcha(change.new_value,'PROFILE_MOBILE');
    if (!validID(result?.captcha_id) || !safePNGImage(result.background_image) || !safePNGImage(result.piece_image)
      || !['width','height','piece_width','piece_height'].every(key => Number.isInteger(result[key]) && result[key] > 0 && result[key] <= 4096)
      || result.piece_width >= result.width || result.piece_height > result.height || !Number.isInteger(result.piece_y) || result.piece_y < 0 || result.piece_y + result.piece_height > result.height || !Number.isFinite(Date.parse(result.expires_at))) throw new TypeError('invalid captcha');
    if (current()) { setCaptcha(result); setOffset(0); }
  }); };
  const send = () => { if (busy || !captcha || Date.parse(captcha.expires_at) <= Date.now()) return; void action(async current => {
    const result = await client.sendProfileMobileChallenge(change.profile_change_request_id,captcha.captcha_id,offset);
    if (!validID(result?.challenge_id) || !Number.isInteger(result.retry_after_seconds) || result.retry_after_seconds < 1 || result.retry_after_seconds > 86400 || !Number.isFinite(Date.parse(result.expires_at))) throw new TypeError('invalid challenge');
    if (current()) { setChallenge(result); setCaptcha(null); setCooldown(Date.now()+result.retry_after_seconds*1000); setNow(Date.now()); }
  }); };
  const finish = () => { if (busy) return; void action(async current => {
    const result = profileChange(await client.completeProfileChange(change.profile_change_request_id,code),change.profile_change_request_id);
    if (!current()) return;
    if (result.status === 'COMPLETED') await completed(current); else setChange(result);
  }); };
  const remaining = Math.max(0,Math.ceil((cooldown-now)/1000));
  return <section className="ap-card"><h2>{copy.profileSettings}</h2>{error && <p role="alert">{accountErrorMessage(error,copy)}</p>}{!change ? <form className="ap-stack" onSubmit={begin}><label>{copy.profileSettings}<select disabled={busy} value={field} onChange={event => { setField(event.target.value); setValue(''); }}><option value="USERNAME">{copy.changeUsername}</option><option value="MOBILE">{copy.changeMobile}</option></select></label><label>{field === 'USERNAME' ? copy.username : copy.mobile}<input required disabled={busy} value={value} onChange={event => setValue(event.target.value)} /></label>{portal ? <a href="/auth/login?return_to=/account.html">{copy.recentAuthenticationRequired}</a> : <label>{copy.currentPassword}<input required disabled={busy} type="password" autoComplete="current-password" value={password} onChange={event => setPassword(event.target.value)} /></label>}<button className="ap-primary" disabled={busy || !value.trim() || (!portal && !password)}>{copy.continueChange}</button></form> : <div className="ap-stack"><p>{change.new_value}</p><p>{recovering(change.status) ? copy.profileChangeRecovering : copy.profileChangeStates[change.status]}</p>{change.status === 'COMPLETED' ? <button disabled={busy} onClick={() => { void action(completed); }}>{copy.logout}</button> : !recovering(change.status) && <>{change.field === 'MOBILE' && <><button disabled={busy || remaining > 0} onClick={openCaptcha}>{remaining ? copy.resendCountdown(remaining) : copy.sendCode}</button>{captcha && <div><div className="relative overflow-hidden" style={{width:'100%',aspectRatio:`${captcha.width}/${captcha.height}`}}><img src={captcha.background_image} alt="" className="absolute inset-0 h-full w-full" /><img src={captcha.piece_image} alt="" className="absolute" style={{left:`${offset/captcha.width*100}%`,top:`${captcha.piece_y/captcha.height*100}%`,width:`${captcha.piece_width/captcha.width*100}%`,height:`${captcha.piece_height/captcha.height*100}%`}} /></div><input aria-label={copy.captchaSlider} disabled={busy} type="range" min="0" max={captcha.width-captcha.piece_width} value={offset} onChange={event => setOffset(Number(event.target.value))} /><button disabled={busy} onClick={send}>{copy.captchaConfirm}</button></div>}<input aria-label={copy.smsCode} disabled={busy || !challenge} value={code} onChange={event => setCode(event.target.value)} autoComplete="one-time-code" /></>}<button className="ap-primary" disabled={busy || (change.field === 'MOBILE' && (!challenge || !code.trim()))} onClick={finish}>{copy.confirmChange}</button></>}</div>}</section>;
}
