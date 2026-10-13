import { useCallback, useEffect, useRef, useState } from 'react';
import { secureRequestID } from './request-id.js';
import { integerMoney } from './finance-contract.js';
import { bonusRevision, MAX_BONUS_POINTS } from './registration-bonus-contract.js';
import { readBonusPending, persistBonusPending, clearBonusPending, bonusAttemptProvesNoWrite } from './registration-bonus-recovery.js';

const MICROCREDITS_PER_POINT = 10_000n;
const EMPTY_PERMISSIONS = Object.freeze([]);
const REGISTRATION_BONUS_DRAFT_KEY = 'pinvou.admin.registration-bonus.reauthentication-draft';
const REGISTRATION_BONUS_STEPUP_QUERY = 'registration_bonus_stepup';
const REGISTRATION_BONUS_DRAFT_TTL_MS = 10 * 60 * 1000;
const REQUEST_ID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const REGISTRATION_BONUS_DRAFT_TOMBSTONE = '{"version":0}';

function policyForm(policy = {}) {
  policy = policy || {};
  return {
    enabled: policy.enabled === true,
    amount_points: (integerMoney(policy.amount_microcredits || 0) / MICROCREDITS_PER_POINT).toString(),
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
    let expectedRevision;
    try { expectedRevision = bonusRevision(value?.expected_revision); } catch { return null; }
    const amount = Number(value?.amount_points);
    const validity = Number(value?.validity_days);
    const reason = typeof value?.reason === 'string' ? value.reason : '';
    if (value?.version !== 1 || !REQUEST_ID_PATTERN.test(attemptID) || !userID
      || !Number.isSafeInteger(createdAt) || !Number.isSafeInteger(expiresAt)
      || expiresAt <= createdAt || expiresAt - createdAt > REGISTRATION_BONUS_DRAFT_TTL_MS || expiresAt <= Date.now()
      || !expectedRevision
      || typeof value?.enabled !== 'boolean' || !Number.isSafeInteger(amount) || amount < 0 || amount > MAX_BONUS_POINTS
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

function reauthenticationDraftIsFresh(draft) {
  return Number.isSafeInteger(draft?.expiresAt) && draft.expiresAt > Date.now();
}

function clearReauthenticationDraft() {
  let storageCleared = false;
  try {
    globalThis.sessionStorage.removeItem(REGISTRATION_BONUS_DRAFT_KEY);
    storageCleared = globalThis.sessionStorage.getItem(REGISTRATION_BONUS_DRAFT_KEY) === null;
  } catch {
    // Fall through to an invalid tombstone when deletion alone is unavailable.
  }
  if (!storageCleared) {
    try {
      globalThis.sessionStorage.setItem(REGISTRATION_BONUS_DRAFT_KEY, REGISTRATION_BONUS_DRAFT_TOMBSTONE);
      storageCleared = globalThis.sessionStorage.getItem(REGISTRATION_BONUS_DRAFT_KEY) === REGISTRATION_BONUS_DRAFT_TOMBSTONE;
    } catch {
      storageCleared = false;
    }
  }
  try {
    const url = new URL(globalThis.location.href);
    url.searchParams.delete(REGISTRATION_BONUS_STEPUP_QUERY);
    globalThis.history.replaceState(globalThis.history.state, '', `${url.pathname}${url.search}${url.hash}`);
  } catch {
    // History can be unavailable in embedded browsers; the server policy remains authoritative.
  }
  return storageCleared;
}

export function AdminOperationsWorkspace({ client, copy, language = 'en', permissions = EMPTY_PERMISSIONS, currentUserID = '', authorizationRefreshing = false, authorizationRefreshGuard = null }) {
  const labels = copy.operations;
  const permissionSet = new Set(permissions);
  const canManage = permissionSet.has('platform.registration-bonus.manage');
  const canRead = permissionSet.has('platform.registration-bonus.read');
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || 'en-US';
  const returnAttemptID = useRef(reauthenticationAttemptID());
  const initialStoredDraft = useRef(storedReauthenticationDraft());
  const workspaceUserID = useRef(currentUserID);
  const currentUserIDRef = useRef(currentUserID);
  const formOwnerUserID = useRef(currentUserID);
  const mountedRef = useRef(true);
  const saveInFlight = useRef(false);
  const loadGeneration = useRef(0);
  currentUserIDRef.current = currentUserID;
  const trustedStoredDraft = initialStoredDraft.current?.userID === currentUserID && canManage ? initialStoredDraft.current : null;
  const returnedDraft = useRef(returnAttemptID.current && trustedStoredDraft?.attemptID === returnAttemptID.current ? trustedStoredDraft : null);
  const [pendingDraft, setPendingDraft] = useState(() => !returnAttemptID.current ? trustedStoredDraft : null);
  const pendingWrite = useRef(null);
  const [recoveryBlocked, setRecoveryBlocked] = useState(false);
  const [unknownWrite, setUnknownWrite] = useState(false);
  useEffect(() => {
    try {
      pendingWrite.current = readBonusPending(currentUserID);
      setUnknownWrite(Boolean(pendingWrite.current)); setRecoveryBlocked(false);
    } catch { pendingWrite.current = null; setRecoveryBlocked(true); }
  }, [currentUserID]);
  const [policy, setPolicy] = useState(null);
  const [form, setForm] = useState(() => policyForm());
  const [state, setState] = useState({ loading: true, saving: false, error: '', errorCode: '', notice: '' });

  const load = useCallback(async (signal, { fresh = false } = {}) => {
    const request = ++loadGeneration.current;
    setState((value) => ({ ...value, loading: true, error: '', errorCode: '', notice: '' }));
    try {
      const result = await client.registrationBonusPolicy(signal, { fresh });
      if (signal?.aborted || !mountedRef.current || request !== loadGeneration.current) return false;
      const draft = returnedDraft.current;
      const draftExpired = Boolean(draft && !reauthenticationDraftIsFresh(draft));
      const draftCleared = !returnAttemptID.current || clearReauthenticationDraft();
      const usableDraft = draftCleared && !draftExpired ? draft : null;
      const draftConflict = Boolean(usableDraft && usableDraft.expectedRevision !== bonusRevision(result.revision));
      setPolicy(result);
      setForm(usableDraft?.form || policyForm(result));
      formOwnerUserID.current = usableDraft?.userID || currentUserIDRef.current;
      if (returnAttemptID.current) {
        returnAttemptID.current = '';
        returnedDraft.current = null;
      }
      setState({
        loading: false,
        saving: false,
        error: !draftCleared ? labels.reauthenticationDraftCleanupFailed : draftConflict ? labels.reauthenticationDraftConflict : '',
        errorCode: !draftCleared ? 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED' : draftConflict ? 'REGISTRATION_BONUS_DRAFT_CONFLICT' : '',
        notice: draftCleared && draftExpired ? labels.reauthenticationDraftExpired : '',
      });
      return true;
    } catch (error) {
      if (mountedRef.current && request === loadGeneration.current && currentUserID === currentUserIDRef.current) setState({ loading: false, saving: false, error: labels.loadFailed, errorCode: error?.code || '', notice: '' });
      return false;
    }
  }, [client, currentUserID, labels.loadFailed, labels.reauthenticationDraftCleanupFailed, labels.reauthenticationDraftConflict, labels.reauthenticationDraftExpired]);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; loadGeneration.current++; };
  }, []);

  useEffect(() => {
    if (authorizationRefreshing || (policy && workspaceUserID.current === currentUserID)) return;
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load, authorizationRefreshing]); // Policy is deliberately not a reload trigger for an unsaved same-actor draft.

  useEffect(() => {
    const draft = initialStoredDraft.current;
    const identityChanged = workspaceUserID.current !== currentUserID;
    if (identityChanged || !canManage || draft && draft.userID !== currentUserID) {
      workspaceUserID.current = currentUserID;
      initialStoredDraft.current = null;
      returnedDraft.current = null;
      setPendingDraft(null);
      const draftCleared = clearReauthenticationDraft();
      formOwnerUserID.current = currentUserID;
      if (policy) setForm(policyForm(policy));
      setState((value) => ({
        ...value,
        error: draftCleared ? '' : labels.reauthenticationDraftCleanupFailed,
        errorCode: draftCleared ? '' : 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED',
        notice: draftCleared && identityChanged ? labels.reauthenticationDraftIdentityChanged : '',
      }));
    }
  }, [canManage, currentUserID, labels.reauthenticationDraftCleanupFailed, labels.reauthenticationDraftIdentityChanged, policy]);

  const save = async (event) => {
    event?.preventDefault();
    if (saveInFlight.current || state.saving || recoveryBlocked) return;
    const refreshGeneration = Number(authorizationRefreshGuard?.current?.generation || 0);
    const refreshInterrupted = () => Boolean(authorizationRefreshGuard?.current?.active) || Number(authorizationRefreshGuard?.current?.generation || 0) !== refreshGeneration;
    const stopForAuthorizationRefresh = () => setState((value) => ({ ...value, saving: false }));
    const reconcileAfterAuthorizationRefresh = async (confirmed = false) => {
      let loaded = false;
      while (mountedRef.current) {
        while (authorizationRefreshGuard?.current?.active) {
          await authorizationRefreshGuard.current.settled;
        }
        if (!mountedRef.current) return;
        const reconciliationGeneration = Number(authorizationRefreshGuard?.current?.generation || 0);
        loaded = await load(undefined, { fresh: true });
        if (!authorizationRefreshGuard?.current?.active && Number(authorizationRefreshGuard?.current?.generation || 0) === reconciliationGeneration) break;
      }
      if (loaded && mountedRef.current && confirmed) setState((value) => ({ ...value, notice: labels.saveReconciled }));
    };
    if (!canManage || (!policy && !pendingWrite.current) || authorizationRefreshing || refreshInterrupted() || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_IDENTITY_CHANGED') return;
    if (formOwnerUserID.current !== currentUserID) {
      formOwnerUserID.current = '';
      setForm(policyForm(policy));
      setState((value) => ({ ...value, error: labels.reauthenticationDraftIdentityChanged, errorCode: 'REGISTRATION_BONUS_DRAFT_IDENTITY_CHANGED', notice: '' }));
      return;
    }
    saveInFlight.current = true;
    let submittedCommand = null;
    const replayingUnknown = Boolean(pendingWrite.current);
    setState((value) => ({ ...value, saving: true, error: '', errorCode: '', notice: '' }));
    try {
      const freshSession = await client.session();
      if (refreshInterrupted()) { stopForAuthorizationRefresh(); return; }
      const freshAuthorization = freshSession.authenticated ? await client.authorization() : null;
      if (refreshInterrupted()) { stopForAuthorizationRefresh(); return; }
      const freshUserID = String(freshAuthorization?.user_id || '');
      const freshPermissions = new Set(freshAuthorization?.platform_permissions || []);
      if (!freshSession.authenticated || freshUserID !== currentUserID || freshUserID !== formOwnerUserID.current || !freshPermissions.has('platform.registration-bonus.manage')) {
        formOwnerUserID.current = '';
        setForm(policyForm(policy));
        setState({ loading: false, saving: false, error: labels.reauthenticationDraftIdentityChanged, errorCode: 'REGISTRATION_BONUS_DRAFT_IDENTITY_CHANGED', notice: '' });
        return;
      }
      if (refreshInterrupted()) { stopForAuthorizationRefresh(); return; }
      const command = pendingWrite.current || { actor: currentUserID, key: `registration-bonus-${secureRequestID()}`, body: {
        enabled: form.enabled,
        amount_points: Number(form.amount_points),
        validity_days: Number(form.validity_days),
        expected_revision: bonusRevision(policy.revision),
        reason: form.reason.trim(),
      } };
      if (command.actor !== freshUserID) throw new Error('INVALID_REGISTRATION_BONUS_RECOVERY');
      try { persistBonusPending(command); } catch {
        setState(value => ({ ...value, saving: false, error: labels.pendingStorageFailed, errorCode: 'REGISTRATION_BONUS_PENDING_STORAGE', notice: '' })); return;
      }
      pendingWrite.current = command; setUnknownWrite(true);
      submittedCommand = command;
      const updated = await client.updateRegistrationBonusPolicy(command.body, command.key);
      if (BigInt(updated.revision) !== BigInt(command.body.expected_revision) + 1n || updated.enabled !== command.body.enabled
        || integerMoney(updated.amount_microcredits) !== BigInt(command.body.amount_points) * MICROCREDITS_PER_POINT
        || updated.validity_days !== command.body.validity_days) throw new Error('INVALID_REGISTRATION_BONUS_ACK');
      clearBonusPending(command.actor);
      if (pendingWrite.current?.key === command.key) { pendingWrite.current = null; setUnknownWrite(false); }
      if (refreshInterrupted()) { await reconcileAfterAuthorizationRefresh(true); return; }
      if (replayingUnknown) {
        // A replay receipt is the original result, which may no longer be the
        // active policy. Confirm it, then show current authoritative facts.
        const loaded = await load(undefined, { fresh: true });
        if (refreshInterrupted()) { await reconcileAfterAuthorizationRefresh(true); return; }
        if (loaded) setState(value => ({ ...value, notice: labels.saveReconciled }));
        return;
      }
      setPolicy(updated);
      setForm(policyForm(updated));
      setState({ loading: false, saving: false, error: '', errorCode: '', notice: labels.saved });
    } catch (error) {
      const safeNoWrite = bonusAttemptProvesNoWrite(error, replayingUnknown);
      // A rejected replay proves only that this attempt did not write. It does
      // not resolve the original unknown outcome or permit a replacement key.
      if (submittedCommand && safeNoWrite) {
        try {
          clearBonusPending(submittedCommand.actor);
          if (pendingWrite.current?.key === submittedCommand.key) { pendingWrite.current = null; setUnknownWrite(false); }
        } catch { setRecoveryBlocked(true); }
      }
      if (refreshInterrupted()) { await reconcileAfterAuthorizationRefresh(); return; }
      if (pendingWrite.current && error?.code !== 'RECENT_AUTHENTICATION_REQUIRED') {
        setState({ loading: false, saving: false, error: labels.outcomeUnknown, errorCode: 'REGISTRATION_BONUS_RESULT_UNKNOWN', notice: '' }); return;
      }
      const authorizationChanged = (error?.status === 401 || error?.status === 403) && error?.code !== 'RECENT_AUTHENTICATION_REQUIRED';
      if (authorizationChanged) formOwnerUserID.current = '';
      const message = authorizationChanged
        ? labels.reauthenticationDraftIdentityChanged
        : error?.code === 'REGISTRATION_BONUS_CONFLICT'
        ? labels.conflict
        : error?.code === 'REGISTRATION_BONUS_ELIGIBILITY_NOT_READY'
          ? labels.eligibilityNotReady
        : error?.code === 'RECENT_AUTHENTICATION_REQUIRED'
          ? labels.recentAuthenticationRequired
          : labels.saveFailed;
      setState({ loading: false, saving: false, error: message, errorCode: authorizationChanged ? 'REGISTRATION_BONUS_DRAFT_IDENTITY_CHANGED' : error?.code || '', notice: '' });
    } finally { saveInFlight.current = false; }
  };

  const valid = Number.isSafeInteger(Number(form.amount_points)) && Number(form.amount_points) >= 0 && Number(form.amount_points) <= MAX_BONUS_POINTS
    && Number.isInteger(Number(form.validity_days)) && Number(form.validity_days) >= 1 && Number(form.validity_days) <= 3650
    && form.reason.trim().length >= 10 && form.reason.trim().length <= 500;
  const eligibilityReady = policy?.eligibility_status === 'READY';
  const reauthenticate = () => {
    if (pendingWrite.current?.actor === currentUserID && canManage) {
      // The durable original command already retains its actor/key/body. A
      // policy draft must not replace it with the latest policy revision.
      client.reauthenticate('/admin.html?section=operations');
      return;
    }
    const attemptID = secureRequestID();
    const createdAt = Date.now();
    const draft = JSON.stringify({
      version: 1,
      attempt_id: attemptID,
      user_id: currentUserID,
      created_at: createdAt,
      expires_at: createdAt + REGISTRATION_BONUS_DRAFT_TTL_MS,
      ...form,
      expected_revision: bonusRevision(policy?.revision),
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
    if (!policy) return;
    if (!pendingDraft || pendingDraft.userID !== currentUserID || !canManage) {
      setPendingDraft(null);
      clearReauthenticationDraft();
      return;
    }
    if (!reauthenticationDraftIsFresh(pendingDraft)) {
      if (!clearReauthenticationDraft()) {
        setState((value) => ({ ...value, error: labels.reauthenticationDraftCleanupFailed, errorCode: 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED', notice: '' }));
        return;
      }
      setPendingDraft(null);
      setForm(policyForm(policy));
      formOwnerUserID.current = currentUserID;
      setState((value) => ({ ...value, error: '', errorCode: '', notice: labels.reauthenticationDraftExpired }));
      return;
    }
    if (!clearReauthenticationDraft()) {
      setState((value) => ({ ...value, error: labels.reauthenticationDraftCleanupFailed, errorCode: 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED', notice: '' }));
      return;
    }
    const draftConflict = pendingDraft.expectedRevision !== bonusRevision(policy.revision);
    setForm(pendingDraft.form);
    formOwnerUserID.current = pendingDraft.userID;
    setPendingDraft(null);
    setState({
      loading: false,
      saving: false,
      error: draftConflict ? labels.reauthenticationDraftConflict : '',
      errorCode: draftConflict ? 'REGISTRATION_BONUS_DRAFT_CONFLICT' : '',
      notice: draftConflict ? '' : labels.reauthenticationDraftRecovered,
    });
  };
  const discardPendingDraft = () => {
    if (!clearReauthenticationDraft()) {
      setState((value) => ({ ...value, error: labels.reauthenticationDraftCleanupFailed, errorCode: 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED', notice: '' }));
      return;
    }
    setPendingDraft(null);
    setForm(policyForm(policy));
    formOwnerUserID.current = currentUserID;
    setState((value) => ({ ...value, error: '', errorCode: '', notice: labels.reauthenticationDraftDiscarded }));
  };
  const retryDraftCleanup = () => {
    if (!clearReauthenticationDraft()) {
      setState((value) => ({ ...value, error: labels.reauthenticationDraftCleanupFailed, errorCode: 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED', notice: '' }));
      return;
    }
    setPendingDraft(null);
    setForm(policyForm(policy));
    formOwnerUserID.current = currentUserID;
    setState((value) => ({ ...value, error: '', errorCode: '', notice: labels.reauthenticationDraftDiscarded }));
  };
  const errorAction = unknownWrite && state.errorCode !== 'RECENT_AUTHENTICATION_REQUIRED'
    ? { label: labels.retryUnknown, run: () => void save() }
    : state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT'
    ? { label: labels.loadLatestPolicy, run: () => void load() }
    : state.errorCode === 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED'
      ? { label: labels.retryDraftCleanup, run: retryDraftCleanup }
    : state.errorCode === 'RECENT_AUTHENTICATION_REQUIRED' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_STORAGE_UNAVAILABLE'
      ? { label: state.errorCode === 'RECENT_AUTHENTICATION_REQUIRED' ? labels.reauthenticate : labels.retryReauthentication, run: reauthenticate }
      : { label: copy.retry, read: true, run: () => void load() };

  return (
    <section className="adm-panel adm-settings-panel adm-registration-bonus" data-testid="registration-bonus-policy">
      <header className="adm-section-head"><div><h2>{labels.title}</h2><p>{labels.lead}</p></div></header>
      <div className="adm-inline-message warn" role="note">{labels.lifetimeWarning}</div>
      {pendingDraft && <div className="adm-inline-message warn adm-registration-bonus-draft-prompt" role="status"><span>{labels.unfinishedReauthenticationDraft}</span><div><button type="button" onClick={recoverPendingDraft} disabled={state.loading || !policy}>{labels.recoverDraft}</button><button type="button" onClick={discardPendingDraft}>{labels.discardDraft}</button></div></div>}
      {(unknownWrite || recoveryBlocked) && !state.error && <div className="adm-inline-message error" role="alert"><span>{recoveryBlocked ? labels.pendingStorageFailed : labels.outcomeUnknown}</span>{unknownWrite && <button type="button" disabled={state.saving || authorizationRefreshing || !canManage || recoveryBlocked} onClick={() => void save()}>{labels.retryUnknown}</button>}</div>}
      {state.error && <div className="adm-inline-message error" role="alert"><span>{state.error}</span><button type="button" onClick={errorAction.run} disabled={state.loading || state.saving || authorizationRefreshing || (errorAction.read ? !canRead : !canManage) || recoveryBlocked}>{errorAction.label}</button></div>}
      {state.notice && <div className="adm-inline-message ok" role="status">{state.notice}</div>}
      {state.loading && <p className="adm-muted">{copy.loading}</p>}
      {!state.loading && policy && (
        <form className="adm-sms-form" onSubmit={save}>
          <dl className="adm-registration-bonus-summary">
            <div><dt>{labels.revision}</dt><dd>{policy.revision}</dd></div>
            <div><dt>{labels.updatedAt}</dt><dd>{new Date(policy.updated_at).toLocaleString(locale)}</dd></div>
          </dl>
          <div className={`adm-inline-message ${eligibilityReady ? 'ok' : 'warn'}`} role="status" data-testid="registration-bonus-readiness">{eligibilityReady ? labels.eligibilityReady : labels.eligibilityBlocked.replace('{count}', String(policy.unrecoverable_registration_count || 0))}</div>
          <div className="adm-registration-bonus-editor">
            <label className="adm-sms-switch"><input type="checkbox" checked={form.enabled} disabled={!canManage || unknownWrite || recoveryBlocked || state.saving || (!eligibilityReady && !form.enabled)} onChange={(event) => setForm((value) => ({ ...value, enabled: event.target.checked }))} /><span>{labels.enabled}</span></label>
            <div className="adm-sms-fields">
              <label><span>{labels.amount}</span><input required type="number" min="0" step="1" value={form.amount_points} disabled={!canManage || unknownWrite || recoveryBlocked || state.saving} onChange={(event) => setForm((value) => ({ ...value, amount_points: event.target.value }))} /></label>
              <label><span>{labels.validityDays}</span><input required type="number" min="1" max="3650" step="1" value={form.validity_days} disabled={!canManage || unknownWrite || recoveryBlocked || state.saving} onChange={(event) => setForm((value) => ({ ...value, validity_days: event.target.value }))} /></label>
            </div>
            {canManage && <label className="adm-sms-reason"><span>{labels.reason}</span><textarea required minLength="10" maxLength="500" rows="4" value={form.reason} disabled={unknownWrite || recoveryBlocked || state.saving} onChange={(event) => setForm((value) => ({ ...value, reason: event.target.value }))} placeholder={labels.reasonPlaceholder} /></label>}
            {!canManage && <div className="adm-inline-message info" role="status">{labels.readOnly}</div>}
            {canManage && <div className="adm-registration-bonus-actions"><button type="submit" className="adm-primary-button" disabled={unknownWrite || recoveryBlocked || state.saving || authorizationRefreshing || !valid || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CONFLICT' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_CLEANUP_FAILED' || state.errorCode === 'REGISTRATION_BONUS_DRAFT_IDENTITY_CHANGED' || (form.enabled && !eligibilityReady)}>{state.saving ? labels.saving : labels.save}</button></div>}
          </div>
        </form>
      )}
    </section>
  );
}
