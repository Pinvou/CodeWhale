import { useCallback, useEffect, useState } from 'react';

const MICROCREDITS_PER_POINT = 10_000;
const EMPTY_PERMISSIONS = Object.freeze([]);

function policyForm(policy = {}) {
  return {
    enabled: policy.enabled === true,
    amount_points: String(Math.trunc(Number(policy.amount_microcredits || 0) / MICROCREDITS_PER_POINT)),
    validity_days: String(policy.validity_days || 30),
    reason: '',
  };
}

export function AdminOperationsWorkspace({ client, copy, permissions = EMPTY_PERMISSIONS }) {
  const labels = copy.operations;
  const permissionSet = new Set(permissions);
  const canManage = permissionSet.has('platform.registration-bonus.manage');
  const [policy, setPolicy] = useState(null);
  const [form, setForm] = useState(() => policyForm());
  const [state, setState] = useState({ loading: true, saving: false, error: '', notice: '' });

  const load = useCallback(async (signal) => {
    setState((value) => ({ ...value, loading: true, error: '', notice: '' }));
    try {
      const result = await client.registrationBonusPolicy(signal);
      setPolicy(result);
      setForm(policyForm(result));
      setState({ loading: false, saving: false, error: '', notice: '' });
    } catch (error) {
      if (error?.name !== 'AbortError') setState({ loading: false, saving: false, error: labels.loadFailed, notice: '' });
    }
  }, [client, labels.loadFailed]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const save = async (event) => {
    event.preventDefault();
    if (!canManage || !policy) return;
    setState((value) => ({ ...value, saving: true, error: '', notice: '' }));
    try {
      const updated = await client.updateRegistrationBonusPolicy({
        enabled: form.enabled,
        amount_points: Number(form.amount_points),
        validity_days: Number(form.validity_days),
        expected_revision: Number(policy.revision),
        reason: form.reason.trim(),
      });
      setPolicy(updated);
      setForm(policyForm(updated));
      setState({ loading: false, saving: false, error: '', notice: labels.saved });
    } catch (error) {
      const message = error?.code === 'REGISTRATION_BONUS_CONFLICT'
        ? labels.conflict
        : error?.code === 'REGISTRATION_BONUS_ELIGIBILITY_NOT_READY'
          ? labels.eligibilityNotReady
        : error?.code === 'RECENT_AUTHENTICATION_REQUIRED'
          ? labels.recentAuthenticationRequired
          : labels.saveFailed;
      setState({ loading: false, saving: false, error: message, notice: '' });
    }
  };

  const valid = Number.isSafeInteger(Number(form.amount_points)) && Number(form.amount_points) >= 0
    && Number.isInteger(Number(form.validity_days)) && Number(form.validity_days) >= 1 && Number(form.validity_days) <= 3650
    && form.reason.trim().length >= 10 && form.reason.trim().length <= 500;
  const eligibilityReady = policy?.eligibility_status === 'READY';

  return (
    <section className="adm-panel adm-settings-panel adm-registration-bonus" data-testid="registration-bonus-policy">
      <header className="adm-section-head"><div><h2>{labels.title}</h2><p>{labels.lead}</p></div></header>
      <div className="adm-inline-message warn" role="note">{labels.lifetimeWarning}</div>
      {state.error && <div className="adm-inline-message error" role="alert"><span>{state.error}</span><button type="button" onClick={() => void load()} disabled={state.loading || state.saving}>{copy.retry}</button></div>}
      {state.notice && <div className="adm-inline-message ok" role="status">{state.notice}</div>}
      {state.loading && <p className="adm-muted">{copy.loading}</p>}
      {!state.loading && policy && (
        <form className="adm-sms-form" onSubmit={save}>
          <div className="adm-sms-summary"><div><small>{labels.revision}</small><strong>{policy.revision}</strong></div><div><small>{labels.updatedAt}</small><strong>{new Date(policy.updated_at).toLocaleString()}</strong></div></div>
          <div className={`adm-inline-message ${eligibilityReady ? 'ok' : 'warn'}`} role="status" data-testid="registration-bonus-readiness">{eligibilityReady ? labels.eligibilityReady : labels.eligibilityBlocked.replace('{count}', String(policy.unrecoverable_registration_count || 0))}</div>
          <label className="adm-sms-switch"><input type="checkbox" checked={form.enabled} disabled={!canManage || state.saving || (!eligibilityReady && !form.enabled)} onChange={(event) => setForm((value) => ({ ...value, enabled: event.target.checked }))} /><span>{labels.enabled}</span></label>
          <div className="adm-sms-fields">
            <label><span>{labels.amount}</span><input required type="number" min="0" step="1" value={form.amount_points} disabled={!canManage || state.saving} onChange={(event) => setForm((value) => ({ ...value, amount_points: event.target.value }))} /></label>
            <label><span>{labels.validityDays}</span><input required type="number" min="1" max="3650" step="1" value={form.validity_days} disabled={!canManage || state.saving} onChange={(event) => setForm((value) => ({ ...value, validity_days: event.target.value }))} /></label>
          </div>
          {canManage && <label className="adm-sms-reason"><span>{labels.reason}</span><textarea required minLength="10" maxLength="500" rows="3" value={form.reason} disabled={state.saving} onChange={(event) => setForm((value) => ({ ...value, reason: event.target.value }))} placeholder={labels.reasonPlaceholder} /></label>}
          {!canManage && <div className="adm-inline-message info" role="status">{labels.readOnly}</div>}
          {canManage && <button type="submit" className="adm-primary-button" disabled={state.saving || !valid || (form.enabled && !eligibilityReady)}>{state.saving ? labels.saving : labels.save}</button>}
        </form>
      )}
    </section>
  );
}
