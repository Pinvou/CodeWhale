import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import { secureRequestID } from './request-id.js';
import { useAccountOwner } from './useAccountOwner.js';
function SectionHeader({ title, children }) { return <header className="adm-section-head"><h2>{title}</h2>{children}</header>; }
function emptySMSTemplates() {
  return { REGISTER: '', LOGIN: '', PROFILE_MOBILE: '', PASSWORD_RESET: '' };
}

function smsConfigurationForm(configuration = {}) {
  return {
    enabled: Boolean(configuration.enabled),
    provider_id: configuration.provider_id || 'aliyun-managed',
    credential_profile: configuration.credential_profile || 'aliyun-default',
    sign_name: configuration.sign_name || '',
    templates: { ...emptySMSTemplates(), ...(configuration.templates || {}) },
    reason: '',
  };
}

export function SettingsWorkspace({ copy, language, onLanguageChange, client, permissions }) {
  const permissionSet = useMemo(() => new Set(permissions || []), [permissions]);
  const canReadSMS = permissionSet.has('platform.sms-config.read');
  const canManageSMS = permissionSet.has('platform.sms-config.manage');
  const owner = useAccountOwner(client);
  const intent = useRef(null);
  const loadFailed = useRef(copy.settings.sms.loadFailed);
  loadFailed.current = copy.settings.sms.loadFailed;
  const [sms, setSMS] = useState(null);
  const [smsForm, setSMSForm] = useState(() => smsConfigurationForm());
  const [smsState, setSMSState] = useState({ loading: canReadSMS, saving: false, error: '', notice: '' });

  const loadSMSConfiguration = useCallback(async (signal) => {
    if (!canReadSMS) return;
    const token = owner.capture();
    setSMSState((value) => ({ ...value, loading: true, error: '', notice: '' }));
    try {
      const result = await client.smsConfiguration(signal);
      if (!owner.current(token)) return;
      setSMS(result);
      setSMSForm(smsConfigurationForm(result));
      setSMSState({ loading: false, saving: false, error: '', notice: '' });
    } catch (error) {
      if (owner.current(token) && error?.name !== 'AbortError') setSMSState({ loading: false, saving: false, error: loadFailed.current, notice: '' });
    }
  }, [canReadSMS, client, owner]);

  useEffect(() => {
    setSMS(null);
    const controller = new globalThis.AbortController();
    void loadSMSConfiguration(controller.signal);
    return () => controller.abort();
  }, [loadSMSConfiguration]);

  useEffect(() => { intent.current = null; }, [client]);

  const chooseSMSProvider = (providerID) => {
    setSMSForm((value) => ({
      ...value,
      provider_id: providerID,
      credential_profile: providerID === 'aliyun-managed' ? 'aliyun-default' : 'http-default',
      sign_name: providerID === 'aliyun-managed' ? value.sign_name : '',
      templates: providerID === 'aliyun-managed' ? value.templates : emptySMSTemplates(),
    }));
  };

  const saveSMSConfiguration = async (event) => {
    event.preventDefault();
    if (!canManageSMS || !sms) return;
    const token = owner.capture();
    setSMSState((value) => ({ ...value, saving: true, error: '', notice: '' }));
    try {
      const command = {
        expected_revision: sms.revision,
        enabled: smsForm.enabled,
        provider_id: smsForm.provider_id,
        credential_profile: smsForm.credential_profile,
        sign_name: smsForm.provider_id === 'aliyun-managed' ? smsForm.sign_name : '',
        templates: smsForm.provider_id === 'aliyun-managed' ? smsForm.templates : emptySMSTemplates(),
        reason: smsForm.reason.trim(),
      };
      if (!Number.isSafeInteger(command.expected_revision) || command.expected_revision < 0) throw new Error('INVALID_SMS_REVISION');
      const fingerprint = JSON.stringify({ ...command, expected_revision: undefined });
      if (!intent.current || intent.current.fingerprint !== fingerprint) intent.current = { fingerprint, command, key: `sms-configuration-${secureRequestID()}` };
      const attempt = intent.current;
      const result = await client.updateSMSConfiguration(attempt.command, attempt.key);
      if (!owner.current(token)) return;
      if (!result || result.revision !== attempt.command.expected_revision + 1 || result.provider_id !== attempt.command.provider_id || result.credential_profile !== attempt.command.credential_profile || result.enabled !== attempt.command.enabled || Object.keys(emptySMSTemplates()).some(key => result.templates?.[key] !== attempt.command.templates[key]) || (result.sign_name || '') !== attempt.command.sign_name) throw new Error('INVALID_SMS_ACK');
      if (intent.current === attempt) intent.current = null;
      setSMS(result);
      setSMSForm(smsConfigurationForm(result));
      setSMSState({ loading: false, saving: false, error: '', notice: copy.settings.sms.saved });
    } catch (error) {
      if (!owner.current(token)) return;
      const message = error?.code === 'SMS_CONFIGURATION_CONFLICT'
        ? copy.settings.sms.conflict
        : error?.code === 'SMS_PROVIDER_PROFILE_UNAVAILABLE'
          ? copy.settings.sms.profileUnavailable
          : error?.code === 'RECENT_AUTHENTICATION_REQUIRED'
            ? copy.settings.sms.recentAuthenticationRequired
            : copy.settings.sms.saveFailed;
      setSMSState({ loading: false, saving: false, error: message, notice: '' });
    }
  };

  const selectedProfile = sms?.available_profiles?.find((profile) => profile.profile_id === smsForm.credential_profile);
  const formValid = smsForm.reason.trim().length >= 10 && (!smsForm.enabled || selectedProfile?.configured) && (smsForm.provider_id !== 'aliyun-managed' || (
    smsForm.sign_name.trim() && Object.values(smsForm.templates).every((value) => value.trim())
  ));
  return (
    <div className="adm-settings-stack">
      <section className="adm-panel adm-settings-panel">
        <SectionHeader title={copy.settings.title} copy={copy} />
        <div className="adm-detail-grid">
          <label>
            <span>{copy.settings.language}</span>
            <select value={language} onChange={(event) => onLanguageChange(event.target.value)}>
              {supportedLocales.map((locale) => (
                <option key={locale} value={locale}>
                  {localeNames[locale]}
                </option>
              ))}
            </select>
          </label>
        </div>
        <p className="adm-muted">{copy.settings.boundary}</p>
      </section>
      {canReadSMS && (
        <section className="adm-panel adm-settings-panel adm-sms-configuration" data-readiness={sms?.readiness || 'LOADING'}>
          <SectionHeader title={copy.settings.sms.title} copy={copy} />
          <p className="adm-muted">{copy.settings.sms.boundary}</p>
          {smsState.loading && <div className="adm-empty" role="status">{copy.loading}</div>}
          {smsState.error && <div className="adm-inline-message error adm-sms-error" role="alert"><span>{smsState.error}</span><button type="button" disabled={smsState.loading || smsState.saving} onClick={() => void loadSMSConfiguration()}>{copy.retry}</button></div>}
          {smsState.notice && <div className="adm-inline-message ok" role="status">{smsState.notice}</div>}
          {sms && !smsState.loading && (
            <form className="adm-sms-form" onSubmit={saveSMSConfiguration}>
              <div className="adm-detail-grid adm-sms-summary">
                <div><small>{copy.settings.sms.revision}</small><strong>{sms.revision}</strong></div>
                <div><small>{copy.settings.sms.source}</small><strong>{copy.settings.sms.sources[sms.source] || sms.source}</strong></div>
                <div><small>{copy.settings.sms.readiness}</small><strong>{copy.settings.sms.readinessValues[sms.readiness] || sms.readiness}</strong></div>
              </div>
              <label className="adm-sms-switch"><input type="checkbox" checked={smsForm.enabled} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, enabled: event.target.checked }))} /><span>{copy.settings.sms.enabled}</span></label>
              <div className="adm-sms-fields">
                <label><span>{copy.settings.sms.provider}</span><select value={smsForm.provider_id} disabled={!canManageSMS || smsState.saving} onChange={(event) => chooseSMSProvider(event.target.value)}>{(sms.available_profiles || []).map((profile) => <option key={profile.profile_id} value={profile.provider_id}>{copy.settings.sms.providers[profile.provider_id] || profile.provider_id}</option>)}</select></label>
                <label><span>{copy.settings.sms.profile}</span><select value={smsForm.credential_profile} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, credential_profile: event.target.value }))}>{(sms.available_profiles || []).filter((profile) => profile.provider_id === smsForm.provider_id).map((profile) => <option key={profile.profile_id} value={profile.profile_id}>{profile.profile_id} · {profile.configured ? copy.settings.sms.configured : copy.settings.sms.notConfigured}</option>)}</select></label>
                {smsForm.provider_id === 'aliyun-managed' && <>
                  <label><span>{copy.settings.sms.signName}</span><input required maxLength="100" value={smsForm.sign_name} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, sign_name: event.target.value }))} /></label>
                  {Object.entries(copy.settings.sms.templateLabels).map(([purpose, label]) => <label key={purpose}><span>{label}</span><input required maxLength="120" value={smsForm.templates[purpose]} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, templates: { ...value.templates, [purpose]: event.target.value } }))} /></label>)}
                </>}
                {canManageSMS && <label className="adm-sms-reason"><span>{copy.settings.sms.reason}</span><textarea required minLength="10" maxLength="500" rows="3" value={smsForm.reason} disabled={smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, reason: event.target.value }))} placeholder={copy.settings.sms.reasonPlaceholder} /></label>}
              </div>
              {!selectedProfile?.configured && smsForm.enabled && <div className="adm-inline-message error" role="alert">{copy.settings.sms.profileUnavailable}</div>}
              {!canManageSMS && <div className="adm-inline-message info" role="status">{copy.settings.sms.readOnly}</div>}
              {canManageSMS && <button type="submit" className="adm-primary-button" disabled={smsState.saving || !formValid}>{smsState.saving ? copy.settings.sms.saving : copy.settings.sms.save}</button>}
            </form>
          )}
        </section>
      )}
    </div>
  );
}
