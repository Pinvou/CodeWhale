import React, { useEffect, useState } from 'react';
import { localizedAccountPlatformError } from './error-localization.js';
import { profileCaptchaSubmissionOffset } from './profile-captcha-state.js';
import { useAccountOwner } from './useAccountOwner.js';
function Field({ label, children }) {
  return (
    <label className="ap-field">
      <span>{label}</span>
      {children}
    </label>
  );
}

function localizedError(copy, reason) {
  return localizedAccountPlatformError(copy, reason, {
    RECENT_AUTHENTICATION_REQUIRED: copy.recentAuthenticationRequired,
    USERNAME_INVALID: copy.usernameInvalid,
    USERNAME_ALREADY_REGISTERED: copy.usernameAlreadyRegistered,
    MOBILE_INVALID: copy.mobileInvalid,
    MOBILE_ALREADY_REGISTERED: copy.mobileAlreadyRegistered,
    CAPTCHA_INVALID: copy.captchaInvalid,
    SMS_RATE_LIMITED: copy.smsRateLimited,
    SMS_UNAVAILABLE: copy.smsUnavailable,
    SMS_CODE_INVALID: copy.mobileAuthFailed,
    PROFILE_CHANGE_CONFLICT: copy.profileChangeConflict,
    MOBILE_VERIFICATION_REQUIRED: copy.mobileVerificationRequired,
    PROFILE_CHANGE_NOT_FOUND: copy.profileChangeNotFound,
  });
}

export function ProfileCaptcha({ challenge, offset, busy, error = '', copy, onOffset, onConfirm, onRefresh, onClose }) {
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
    <div className="ap-captcha">
      <header>
        <strong>{copy.captchaTitle}</strong>
        <div>
          <button type="button" disabled={busy} onClick={onRefresh}>
            {copy.captchaRefresh}
          </button>
          <button type="button" disabled={busy} onClick={onClose} aria-label={copy.captchaClose}>
            ×
          </button>
        </div>
      </header>
      {error && (
        <div role="alert" className="ap-alert error">
          {error}
        </div>
      )}
      <div className="ap-captcha-image" style={{ aspectRatio: `${width}/${height}` }}>
        <img src={challenge.background_image} alt="" draggable="false" />
        <img
          src={challenge.piece_image}
          alt=""
          draggable="false"
          className="ap-captcha-piece"
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
      />
      <button type="button" className="ap-primary" disabled={busy} onClick={onConfirm}>
        {busy ? copy.sendingCode : copy.captchaConfirm}
      </button>
    </div>
  );
}

export function ProfileSettings({ copy, client, me, onError, onChanged, onRecovering, embedded = false }) {
  const [username, setUsername] = useState(me?.username || '');
  const [mobile, setMobile] = useState(me?.mobile || '');
  const [requestId, setRequestId] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [captcha, setCaptcha] = useState(null);
  const [captchaOffset, setCaptchaOffset] = useState(0);
  const [challengeSent, setChallengeSent] = useState(false);
  const [deliveryUncertain, setDeliveryUncertain] = useState(false);
  const [busy, setBusy] = useState(false);
  const [captchaError, setCaptchaError] = useState('');

  const owner = useAccountOwner(client);
  const perform = async (action, { captchaScoped = false } = {}) => {
    const token = owner.capture();
    setBusy(true);
    if (captchaScoped) setCaptchaError('');
    try {
      await action(() => owner.current(token));
    } catch (reason) {
      if (!owner.current(token)) return;
      if (captchaScoped) setCaptchaError(localizedError(copy, reason));
      else onError(reason);
    } finally {
      if (owner.current(token)) setBusy(false);
    }
  };
  const finish = (result) => (result?.status === 'COMPLETED' ? onChanged(result) : onRecovering());
  const changeUsername = (event) => {
    event.preventDefault();
    void perform(async (current) => {
      const created = await client.createProfileChange('USERNAME', username.trim());
      if (!current()) return;
      const result = await client.completeProfileChange(created.profile_change_request_id);
      if (current()) await finish(result);
    });
  };
  const openMobileCaptcha = () =>
    void perform(async (current) => {
      const result = await client.createProfileSliderCaptcha(mobile.trim());
      if (!current()) return;
      setCaptcha(result);
      setCaptchaOffset(0);
      setCaptchaError('');
    }, { captchaScoped: Boolean(captcha) });
  const sendMobileCode = (selectedOffset) =>
    void perform(async (current) => {
      let id = requestId;
      if (!id) {
        const created = await client.createProfileChange('MOBILE', mobile.trim());
        if (!current()) return;
        id = created.profile_change_request_id;
        setRequestId(id);
      }
      const delivery = await client.sendProfileMobileChallenge(id, captcha.captcha_id, profileCaptchaSubmissionOffset(selectedOffset, captchaOffset));
      if (!current()) return;
      if (delivery?.delivery_status === 'UNCERTAIN') {
        setCaptcha(null);
        setCaptchaOffset(0);
        setDeliveryUncertain(true);
        setChallengeSent(true);
        setCaptchaError('');
        return;
      }
      if (delivery?.delivery_status !== 'SENT') {
        setCaptchaError(copy.smsDeliveryPending);
        return;
      }
      setCaptcha(null);
      setCaptchaOffset(0);
      setCaptchaError('');
      setChallengeSent(true);
      setDeliveryUncertain(false);
    }, { captchaScoped: true });
  const completeMobile = (event) => {
    event.preventDefault();
    void perform(async (current) => {
      const result = await client.completeProfileChange(requestId, smsCode.trim());
      if (current()) await finish(result);
    });
  };
  const reauthenticate = () => client.reauthenticate('/?section=accountSettings');

  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.profile}</p>}
      <Heading>{copy.profileSettings}</Heading>
      <p className="ap-lead">{copy.profileSettingsLead}</p>
      <article className="ap-card ap-profile-reauth">
        <div>
          <h3>{copy.reauthenticateTitle}</h3>
          <p className="ap-muted">{copy.reauthenticateLead}</p>
        </div>
        <button type="button" className="ap-primary" onClick={reauthenticate}>
          {copy.reauthenticate}
        </button>
      </article>
      <div className="ap-grid">
        <article className="ap-card">
          <h3>{copy.changeUsername}</h3>
          <p className="ap-muted">
            {copy.currentUsername}: {me?.username || '—'}
          </p>
          <form className="ap-stack" onSubmit={changeUsername}>
            <Field label={copy.newUsername}>
              <input value={username} onChange={(event) => setUsername(event.target.value)} disabled={busy} autoComplete="username" />
            </Field>
            <button disabled={busy || !username.trim() || username.trim() === (me?.username || '')}>{copy.confirmChange}</button>
          </form>
        </article>
        <article className="ap-card">
          <h3>{copy.changeMobile}</h3>
          <p className="ap-muted">
            {copy.currentMobile}: {me?.mobile || '—'}
          </p>
          <form className="ap-stack" onSubmit={completeMobile}>
            <Field label={copy.newMobile}>
              <input
                value={mobile}
                onChange={(event) => {
                  setMobile(event.target.value);
                  setRequestId('');
                  setChallengeSent(false);
                  setDeliveryUncertain(false);
                  setSmsCode('');
                  setCaptcha(null);
                  setCaptchaError('');
                }}
                disabled={busy || challengeSent}
                inputMode="tel"
                autoComplete="tel-national"
              />
            </Field>
            {challengeSent && (
              <>
                <Field label={copy.smsCode}>
                  <input value={smsCode} onChange={(event) => setSmsCode(event.target.value)} disabled={busy} inputMode="numeric" autoComplete="one-time-code" />
                </Field>
                {deliveryUncertain && <p className="ap-field-hint error">{copy.smsDeliveryUncertain}</p>}
              </>
            )}
            {challengeSent ? (
              <button disabled={busy || !smsCode.trim()}>{copy.confirmChange}</button>
            ) : (
              <button type="button" disabled={busy || !mobile.trim() || mobile.trim() === (me?.mobile || '')} onClick={openMobileCaptcha}>
                {copy.sendCode}
              </button>
            )}
          </form>
        </article>
      </div>
      {captcha && (
        <div className="ap-captcha-backdrop" role="dialog" aria-modal="true" aria-label={copy.captchaTitle}>
          <ProfileCaptcha
            challenge={captcha}
            offset={captchaOffset}
            busy={busy}
            error={captchaError}
            copy={copy}
            onOffset={setCaptchaOffset}
            onConfirm={sendMobileCode}
            onRefresh={openMobileCaptcha}
            onClose={() => {
              if (!busy) {
                setCaptcha(null);
                setCaptchaOffset(0);
                setCaptchaError('');
              }
            }}
          />
        </div>
      )}
    </section>
  );
}
