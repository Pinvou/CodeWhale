import { useCallback, useEffect, useRef, useState } from 'react';
import { secureRequestID } from './request-id.js';

const MICROCREDITS_PER_POINT = 10_000;
const EMPTY_PERMISSIONS = Object.freeze([]);
const REGISTRATION_BONUS_DRAFT_KEY = 'pinvou.admin.registration-bonus.reauthentication-draft';
const REGISTRATION_BONUS_STEPUP_QUERY = 'registration_bonus_stepup';
const REGISTRATION_BONUS_DRAFT_TTL_MS = 10 * 60 * 1000;
const REQUEST_ID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function policyForm(policy = {}) {
  return {
    enabled: policy.enabled === true,
    amount_points: String(Math.trunc(Number(policy.amount_microcredits || 0) / MICROCREDITS_PER_POINT)),
    validity_days: String(policy.validity_days || 30),
    reason: '',
  };
}

function reauthenticationAttemptID() {
  try {
    return new URLSearchParams(globalThis.location.search).get(REGISTRATION_BONUS_STEPUP_QUERY) || '';
  } catch {
    return '';
  }
}

function storedReauthenticationDraft() {
  try {
    const value = JSON.parse(globalThis.sessionStorage.getItem(REGISTRATION_BONUS_DRAFT_KEY) || 'null');
    const attemptID = typeof value?.attempt_id === 'string' ? value.attempt_id : '';
    const userID = typeof value?.user_id === 'string' ? value.user_id : '';
    const createdAt = Number(value?.created_at);
    const expiresAt = Number(value?.expires_at);
    const expectedRevision = Number(value?.expected_revision);
    const amount = Number(value?.amount_points);
    const validity = Number(value?.validity_days);
    const reason = typeof value?.reason === 'string' ? value.reason : '';
    if (value?.version !== 1 || !REQUEST_ID_PATTERN.test(attemptID) || !userID
      || !Number.isSafeInteger(createdAt) || !Number.isSafeInteger(expiresAt)
      || expiresAt <= createdAt || expiresAt - createdAt > REGISTRATION_BONUS_DRAFT_TTL_MS || expiresAt <= Date.now()
      || !Number.isSafeInteger(expectedRevision) || expectedRevision < 1
      || typeof value?.enabled !== 'boolean' || !Number.isSafeInteger(amount) || amount < 0
      || !Number.isInteger(validity) || validity < 1 || validity > 3650
      || reason.trim().length < 10 || reason.length > 500) {
      globalThis.sessionStorage.removeItem(REGISTRATION_BONUS_DRAFT_KEY);
      return null;
    }
    return {
      attemptID,
      userID,
      expiresAt,
      expectedRevision,
      form: { enabled: value.enabled, amount_points: String(amount), validity_days: String(validity), reason },
    };
  } catch {
    return null;
  }
}

function clearReauthenticationDraft() {
  try {
    globalThis.sessionStorage.removeItem(REGISTRATION_BONUS_DRAFT_KEY);
  } catch {
    // Storage can be unavailable in hardened browsers.
  }
  try {
    const url = new URL(globalThis.location.href);
    url.searchParams.delete(REGISTRATION_BONUS_STEPUP_QUERY);
    globalThis.history.replaceState(globalThis.history.state, '', `${url.pathname}${url.search}${url.hash}`);
  } catch {
    // History can be unavailable in embedded browsers; the server policy remains authoritative.
  }
}

export function AdminOperationsWorkspace({ client, copy, permissions = EMPTY_PERMISSIONS, currentUserID = '' }) {
  const labels = copy.operations;
  const permissionSet = new Set(permissions);
  const canManage = permissionSet.has('platform.registration-bonus.manage');
  const returnAttemptID = useRef(reauthenticationAttemptID());
  const initialStoredDraft = useRef(storedReauthenticationDraft());
  const trustedStoredDraft = initialStoredDraft.current?.userID === currentUserID && canManage ? initialStoredDraft.current : null;
  const returnedDraft = useRef(returnAttemptID.current && trustedStoredDraft?.attemptID === returnAttemptID.current ? trustedStoredDraft : null);
  const [pendingDraft, setPendingDraft] = useState(() => !returnAttemptID.current ? trustedStoredDraft : null);
  const [policy, setPolicy] = useState(null);
  const [form, setForm] = useState(() => policyForm());
  const [state, setState] = useState({ loading: true, saving: false, error: '', errorCode: '', notice: '' });

  const load = useCallback(async (signal) => {
    setState((value) => ({ ...value, loading: true, error: '', errorCode: '', notice: '' }));
    try {
      const result = await client.registrationBonusPolicy(signal);
      const draft = returnedDraft.current;
      const draftConflict = Boolean(draft && draft.expectedRevision !== Number(result.revision));
      setPolicy(result);
      setForm(draft?.form || policyForm(result));
      if (returnAttemptID.current) {
        returnAttemptID.current = '';
        returnedDraft.current = null;
        clearReauthenticationDraft();
      }
      setState({
        loading: false,
        saving: false,
        error: draftConflict ? labels.reauthenticationDraftConflict : '',
        errorCode: draftConflict ? 'REGISTRATION_BONUS_DRAFT_CONFLICT' : '',
        notice: '',
      });
    } catch (error) {
      if (error?.name !== 'AbortError') setState({ loading: false, saving: false, error: labels.loadFailed, errorCode: error?.code || '', notice: '' });
    }
  }, [client, labels.loadFailed, labels.reauthenticationDraftConflict]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  useEffect(() => {
    const draft = initialStoredDraft.current;
    if (draft && (draft.userID !== currentUserID || !canManage)) {
      initialStoredDraft.current = null;
      returnedDraft.current = null;
      setPendingDraft(null);
      clearReauthenticationDraft();
    }
  }, [canManage, currentUserID]);

  const save = async (event) => {
    event.preventDefault();
    if (!canManage || !policy || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT') return;
    setState((value) => ({ ...value, saving: true, error: '', errorCode: '', notice: '' }));
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
      setState({ loading: false, saving: false, error: '', errorCode: '', notice: labels.saved });
    } catch (error) {
      const message = error?.code === 'REGISTRATION_BONUS_CONFLICT'
        ? labels.conflict
        : error?.code === 'REGISTRATION_BONUS_ELIGIBILITY_NOT_READY'
          ? labels.eligibilityNotReady
        : error?.code === 'RECENT_AUTHENTICATION_REQUIRED'
          ? labels.recentAuthenticationRequired
          : labels.saveFailed;
      setState({ loading: false, saving: false, error: message, errorCode: error?.code || '', notice: '' });
    }
  };

  const valid = Number.isSafeInteger(Number(form.amount_points)) && Number(form.amount_points) >= 0
    && Number.isInteger(Number(form.validity_days)) && Number(form.validity_days) >= 1 && Number(form.validity_days) <= 3650
    && form.reason.trim().length >= 10 && form.reason.trim().length <= 500;
  const eligibilityReady = policy?.eligibility_status === 'READY';
  const reauthenticate = () => {
    const attemptID = secureRequestID();
    const createdAt = Date.now();
    const draft = JSON.stringify({
      version: 1,
      attempt_id: attemptID,
      user_id: currentUserID,
      created_at: createdAt,
      expires_at: createdAt + REGISTRATION_BONUS_DRAFT_TTL_MS,
      ...form,
      expected_revision: Number(policy?.revision),
    });
    try {
      if (!currentUserID || !canManage) throw new Error('registration bonus draft identity is unavailable');
      globalThis.sessionStorage.setItem(REGISTRATION_BONUS_DRAFT_KEY, draft);
      if (globalThis.sessionStorage.getItem(REGISTRATION_BONUS_DRAFT_KEY) !== draft) throw new Error('registration bonus draft verification failed');
    } catch {
      setState((value) => ({
        ...value,
        error: labels.reauthenticationDraftStorageFailed,
        errorCode: 'REGISTRATION_BONUS_DRAFT_STORAGE_UNAVAILABLE',
        notice: '',
      }));
      return;
    }
    client.reauthenticate(`/admin.html?section=operations&${REGISTRATION_BONUS_STEPUP_QUERY}=${encodeURIComponent(attemptID)}`);
  };
  const recoverPendingDraft = () => {
    if (!pendingDraft || pendingDraft.userID !== currentUserID || !canManage || !policy) {
      setPendingDraft(null);
      clearReauthenticationDraft();
      return;
    }
    const draftConflict = pendingDraft.expectedRevision !== Number(policy.revision);
    setForm(pendingDraft.form);
    setPendingDraft(null);
    clearReauthenticationDraft();
    setState({
      loading: false,
      saving: false,
      error: draftConflict ? labels.reauthenticationDraftConflict : '',
      errorCode: draftConflict ? 'REGISTRATION_BONUS_DRAFT_CONFLICT' : '',
      notice: draftConflict ? '' : labels.reauthenticationDraftRecovered,
    });
  };
  const discardPendingDraft = () => {
    setPendingDraft(null);
    clearReauthenticationDraft();
    setForm(policyForm(policy));
    setState((value) => ({ ...value, error: '', errorCode: '', notice: labels.reauthenticationDraftDiscarded }));
  };
  const errorAction = state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT'
    ? { label: labels.loadLatestPolicy, run: () => void load() }
    : state.errorCode === 'RECENT_AUTHENTICATION_REQUIRED' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_STORAGE_UNAVAILABLE'
      ? { label: state.errorCode === 'RECENT_AUTHENTICATION_REQUIRED' ? labels.reauthenticate : labels.retryReauthentication, run: reauthenticate }
      : { label: copy.retry, run: () => void load() };

  return (
    <section className="adm-panel adm-settings-panel adm-registration-bonus" data-testid="registration-bonus-policy">
      <header className="adm-section-head"><div><h2>{labels.title}</h2><p>{labels.lead}</p></div></header>
      <div className="adm-inline-message warn" role="note">{labels.lifetimeWarning}</div>
      {pendingDraft && <div className="adm-inline-message warn adm-registration-bonus-draft-prompt" role="status"><span>{labels.unfinishedReauthenticationDraft}</span><div><button type="button" onClick={recoverPendingDraft}>{labels.recoverDraft}</button><button type="button" onClick={discardPendingDraft}>{labels.discardDraft}</button></div></div>}
      {state.error && <div className="adm-inline-message error" role="alert"><span>{state.error}</span><button type="button" onClick={errorAction.run} disabled={state.loading || state.saving}>{errorAction.label}</button></div>}
      {state.notice && <div className="adm-inline-message ok" role="status">{state.notice}</div>}
      {state.loading && <p className="adm-muted">{copy.loading}</p>}
      {!state.loading && policy && (
        <form className="adm-sms-form" onSubmit={save}>
          <dl className="adm-registration-bonus-summary">
            <div><dt>{labels.revision}</dt><dd>{policy.revision}</dd></div>
            <div><dt>{labels.updatedAt}</dt><dd>{new Date(policy.updated_at).toLocaleString()}</dd></div>
          </dl>
          <div className={`adm-inline-message ${eligibilityReady ? 'ok' : 'warn'}`} role="status" data-testid="registration-bonus-readiness">{eligibilityReady ? labels.eligibilityReady : labels.eligibilityBlocked.replace('{count}', String(policy.unrecoverable_registration_count || 0))}</div>
          <div className="adm-registration-bonus-editor">
            <label className="adm-sms-switch"><input type="checkbox" checked={form.enabled} disabled={!canManage || state.saving || (!eligibilityReady && !form.enabled)} onChange={(event) => setForm((value) => ({ ...value, enabled: event.target.checked }))} /><span>{labels.enabled}</span></label>
            <div className="adm-sms-fields">
              <label><span>{labels.amount}</span><input required type="number" min="0" step="1" value={form.amount_points} disabled={!canManage || state.saving} onChange={(event) => setForm((value) => ({ ...value, amount_points: event.target.value }))} /></label>
              <label><span>{labels.validityDays}</span><input required type="number" min="1" max="3650" step="1" value={form.validity_days} disabled={!canManage || state.saving} onChange={(event) => setForm((value) => ({ ...value, validity_days: event.target.value }))} /></label>
            </div>
            {canManage && <label className="adm-sms-reason"><span>{labels.reason}</span><textarea required minLength="10" maxLength="500" rows="4" value={form.reason} disabled={state.saving} onChange={(event) => setForm((value) => ({ ...value, reason: event.target.value }))} placeholder={labels.reasonPlaceholder} /></label>}
            {!canManage && <div className="adm-inline-message info" role="status">{labels.readOnly}</div>}
            {canManage && <div className="adm-registration-bonus-actions"><button type="submit" className="adm-primary-button" disabled={state.saving || !valid || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT' || (form.enabled && !eligibilityReady)}>{state.saving ? labels.saving : labels.save}</button></div>}
          </div>
        </form>
      )}
    </section>
  );
}
