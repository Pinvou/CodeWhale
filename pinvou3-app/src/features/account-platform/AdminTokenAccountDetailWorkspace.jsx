import { secureRequestID } from './request-id.js';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck, requireTokenApprovalImpact } from './admin-high-risk-action-state.js';
import { createAndSubmitFreshGovernedApproval, recoverGovernedApproval } from './admin-governed-approval-workflow.js';
import { openCredentialHandoff } from './admin-credential-handoff.js';
import { settleCredentialLaunch } from './admin-credential-launch-state.js';
import { serverRouteHealth } from './admin-token-resource-state.js';

const TABS = Object.freeze(['overview', 'credentials', 'routes', 'provisionings', 'audit']);
const TERMINAL_LAUNCH = new Set(['SUCCEEDED', 'FAILED', 'REVOKED', 'EXPIRED']);
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const key = (prefix) => `${prefix}-${secureRequestID()}`;
const date = (value) => value ? new Date(value).toLocaleString() : '—';

function asCollection(value) {
  return { items: value ? [value] : [], as_of: value?.as_of || '', next_cursor: '', has_more: false, limit: 1 };
}

function exactKeys(value, required, optional = []) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const allowed = new Set([...required, ...optional]);
  return required.every((name) => Object.prototype.hasOwnProperty.call(value, name)) && Object.keys(value).every((name) => allowed.has(name));
}

function validCredentialLaunch(value, accountID, action) {
  const required = ['launch_id', 'result_ref', 'token_account_id', 'action', 'status', 'expires_at', 'audit_request_id'];
  return exactKeys(value, required) && UUID.test(value.launch_id) && UUID.test(value.result_ref) && value.token_account_id === accountID
    && value.action === action && value.status === 'CREATED' && !Number.isNaN(Date.parse(value.expires_at)) && typeof value.audit_request_id === 'string' && value.audit_request_id.length > 0;
}

function validCredentialResult(value, launch) {
  const required = ['result_ref', 'token_account_id', 'action', 'status', 'audit_request_id', 'window_confirmed', 'updated_at'];
  const optional = ['credential_id', 'credential_version', 'credential_status', 'validation_status', 'fingerprint', 'error_code'];
  if (!exactKeys(value, required, optional) || value.result_ref !== launch.result_ref || value.token_account_id !== launch.token_account_id || value.action !== launch.action || !['PENDING', ...TERMINAL_LAUNCH].includes(value.status) || typeof value.audit_request_id !== 'string' || value.audit_request_id.length < 1 || typeof value.window_confirmed !== 'boolean' || Number.isNaN(Date.parse(value.updated_at))) return false;
  if (value.status === 'SUCCEEDED') return UUID.test(value.credential_id || '') && Number.isInteger(value.credential_version) && value.credential_version >= 1 && value.credential_status === 'ACTIVE' && value.validation_status === 'VALID' && typeof value.fingerprint === 'string' && value.fingerprint.length > 0 && !value.error_code;
  if (value.status === 'PENDING') return !value.credential_id && !value.credential_version && !value.credential_status && !value.validation_status && !value.fingerprint && !value.error_code;
  return !value.credential_id && !value.credential_version && !value.credential_status && !value.validation_status && !value.fingerprint && ['CREDENTIAL_WINDOW_BLOCKED', 'CREDENTIAL_LAUNCH_CANCELLED', 'CREDENTIAL_PERMISSION_REVOKED', 'CREDENTIAL_FEATURE_DISABLED', 'CREDENTIAL_LAUNCH_EXPIRED', 'CREDENTIAL_VALIDATION_FAILED'].includes(value.error_code);
}

function Source({ title, id, state, copy, retry, children }) {
  return <section className="adm-card adm-source-card" aria-labelledby={id}><SourceHeader title={title} titleID={id} state={state} copy={copy} onRetry={retry} /><SourceFeedback state={state} copy={copy} onRetry={retry} />{!state.loading && !state.error && state.items.length > 0 && children}</section>;
}

function Facts({ children }) { return <dl className="adm-model-detail-facts">{children}</dl>; }
function Fact({ label, children }) { return <div><dt>{label}</dt><dd>{children ?? '—'}</dd></div>; }

function trapCredentialDialogFocus(event, root) {
  if (event.key !== 'Tab') return;
  const controls = [...(root?.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])') || [])];
  if (!controls.length) return;
  if (event.shiftKey && document.activeElement === controls[0]) { event.preventDefault(); controls.at(-1).focus(); }
  else if (!event.shiftKey && document.activeElement === controls.at(-1)) { event.preventDefault(); controls[0].focus(); }
}

async function createAndSubmitCredentialApproval(client, draft, signal) {
  return createAndSubmitFreshGovernedApproval(client, draft, signal);
}

async function recoverCredentialApproval(client, reference, draft, signal) {
  return recoverGovernedApproval(client, reference, draft, signal);
}

function CredentialLaunchDialog({ client, copy, account, action, credential, enabled, disabledReason, onClose, onCompleted }) {
  const root = useRef(null);
  const first = useRef(null);
  const previousFocus = useRef(document.activeElement);
  const completed = useRef(onCompleted);
  const preparation = useRef({ inflight: false, payloadKey: '', idempotencyKey: '', payload: null, controller: null, handoff: null, unknown: false });
  const launchRef = useRef(null);
  const terminal = useRef(false);
  const cancellation = useRef(null);
  const revocationAttempted = useRef(false);
  const disabledReasonRef = useRef(disabledReason);
  const [reason, setReason] = useState('');
  const [launch, setLaunch] = useState(null);
  const [state, setState] = useState('EDITING');
  const [error, setError] = useState('');
  useEffect(() => { completed.current = onCompleted; }, [onCompleted]);
  useEffect(() => { launchRef.current = launch; }, [launch]);
  useEffect(() => { disabledReasonRef.current = disabledReason; }, [disabledReason]);
  const acceptTerminalResult = (result) => {
    if (!result || !TERMINAL_LAUNCH.has(result.status)) return false;
    terminal.current = true;
    setState(result.status === 'SUCCEEDED' ? 'SUCCEEDED' : 'FAILED');
    if (result.status === 'SUCCEEDED') completed.current();
    else setError(result.error_code || 'CREDENTIAL_UPDATE_FAILED');
    return true;
  };
  const cancelLaunch = (reasonCode) => {
    preparation.current.handoff?.close();
    preparation.current.handoff = null;
    const current = launchRef.current;
    if (!current?.result_ref || terminal.current) return Promise.resolve(null);
    if (cancellation.current) return cancellation.current;
    // Revocation can race a heartbeat or completion. Regardless of the POST
    // outcome, the immutable result reference is the only authority: query it
    // and never create/re-submit a launch merely because the response was lost.
    const attemptRevoke = !revocationAttempted.current;
    revocationAttempted.current = true;
    cancellation.current = settleCredentialLaunch(client, current, reasonCode, validCredentialResult, undefined, attemptRevoke)
      .then((result) => { acceptTerminalResult(result); return result; })
      .finally(() => { cancellation.current = null; });
    return cancellation.current;
  };
  useEffect(() => { first.current?.focus(); return () => { cancelLaunch(disabledReasonRef.current || 'CREDENTIAL_LAUNCH_CANCELLED'); preparation.current.controller?.abort(); previousFocus.current?.focus?.(); }; }, []);
  useEffect(() => { if (!enabled) { cancelLaunch(disabledReason); onClose(); } }, [enabled, disabledReason, onClose]);
  useEffect(() => {
    if (!launch?.result_ref || !['WAITING', 'CANCELLING', 'UNKNOWN'].includes(state)) return undefined;
    const controller = new globalThis.AbortController();
    let deadline = state === 'WAITING' ? globalThis.setTimeout(() => { void cancelLaunch('CREDENTIAL_WINDOW_BLOCKED'); }, 10_000) : null;
    const poll = globalThis.setInterval(async () => {
      try {
        const result = await client.credentialIsolationResult(launch.result_ref, controller.signal);
        if (!validCredentialResult(result, launch)) throw Object.assign(new Error('invalid credential result'), { code: 'INVALID_CREDENTIAL_RESULT' });
        if (result.window_confirmed && deadline) {
          globalThis.clearTimeout(deadline);
          deadline = null;
        }
        if (acceptTerminalResult(result)) {
          globalThis.clearInterval(poll); if (deadline) globalThis.clearTimeout(deadline);
        }
      } catch (cause) {
        if (cause?.name !== 'AbortError') setError('CREDENTIAL_RESULT_UNKNOWN');
      }
    }, 2_000);
    return () => { controller.abort(); globalThis.clearInterval(poll); if (deadline) globalThis.clearTimeout(deadline); };
  }, [client, launch?.result_ref, state]);
  const prepare = async (event) => {
    event?.preventDefault();
    if (preparation.current.inflight) return;
    const normalized = reason.trim();
    if (normalized.length < 10 || normalized.length > 2000) { setError('INVALID_REASON'); return; }
    const payload = {
      action, expected_account_version: account.account_version,
      expected_credential_version: action === 'ROTATE' ? Number(credential?.version || 0) : 0,
      slot: 'PRIMARY', reason: normalized,
    };
    const payloadKey = JSON.stringify(payload);
    if (preparation.current.unknown && preparation.current.payloadKey !== payloadKey) { setError('COMMAND_PAYLOAD_LOCKED'); return; }
    if (preparation.current.payloadKey !== payloadKey) preparation.current = { ...preparation.current, payloadKey, payload, idempotencyKey: key(`credential-${action.toLowerCase()}`), unknown: false };
    const controller = new globalThis.AbortController();
    preparation.current = { ...preparation.current, inflight: true, controller };
    setState('PREPARING'); setError('');
    let handoff = null;
    let result = null;
    try {
      handoff = await openCredentialHandoff({ signal: controller.signal });
      preparation.current.handoff = handoff;
      result = await client.createCredentialLaunch(account.token_account_id, action, preparation.current.payload, preparation.current.idempotencyKey, controller.signal);
      if (!validCredentialLaunch(result, account.token_account_id, action)) throw Object.assign(new Error('invalid credential launch'), { code: 'INVALID_CREDENTIAL_LAUNCH_RESPONSE' });
      launchRef.current = result;
      setLaunch(result);
      await handoff.submit(result.launch_id);
      preparation.current = { ...preparation.current, inflight: false, controller: null, handoff, unknown: false };
      setState('WAITING');
    } catch (cause) {
      if (cause?.name === 'AbortError') return;
      handoff?.close();
      if (result?.result_ref) {
        preparation.current = { ...preparation.current, inflight: false, controller: null, handoff: null, unknown: true };
        // Once HANDOFF_SUBMIT may have crossed the MessagePort boundary, an
        // absent ACK is UNKNOWN. Keep the frozen result_ref and poll only.
        if (cause?.outcomeUnknown) {
          setError('CREDENTIAL_RESULT_UNKNOWN'); setState('UNKNOWN');
          void cancelLaunch('CREDENTIAL_LAUNCH_CANCELLED');
        } else {
          setState('CANCELLING');
          void cancelLaunch('CREDENTIAL_LAUNCH_CANCELLED');
        }
        return;
      }
      const provenNoWrite = [[400, 'INVALID_CREDENTIAL_LAUNCH'], [401, 'RECENT_AUTH_REQUIRED'], [403, 'PERMISSION_DENIED'], [404, 'NOT_FOUND'], [409, 'IDEMPOTENCY_KEY_REUSED'], [409, 'CATALOG_VERSION_CONFLICT']].some(([status, code]) => cause?.status === status && cause?.code === code);
      const handoffFailure = ['CREDENTIAL_WINDOW_BLOCKED', 'CREDENTIAL_HANDOFF_INVALID', 'CREDENTIAL_HANDOFF_UNAVAILABLE'].includes(cause?.code);
      preparation.current = { ...preparation.current, inflight: false, controller: null, handoff: null, unknown: !provenNoWrite && !handoffFailure };
      setError(provenNoWrite ? cause.code : 'COMMAND_RESULT_UNKNOWN'); setState(provenNoWrite ? 'EDITING' : 'UNKNOWN');
      if (handoffFailure) { setError(cause.code); setState('EDITING'); }
    }
  };
  const locked = state === 'CANCELLING' || state === 'PREPARING';
  if (!enabled) return null;
  const closeSafely = () => {
    if (!launchRef.current?.result_ref || terminal.current) { onClose(); return; }
    setState('CANCELLING');
    void cancelLaunch('CREDENTIAL_LAUNCH_CANCELLED');
  };
  return <div className="adm-protocol-dialog-backdrop"><div ref={root} className="adm-protocol-dialog" role="dialog" aria-modal="true" aria-labelledby="credential-launch-title" onKeyDown={(event) => { trapCredentialDialogFocus(event, root.current); if (event.key === 'Escape' && !locked) closeSafely(); }}>
    <header><div><h3 id="credential-launch-title">{action === 'ENROLL' ? copy.enrollCredential : copy.rotateCredential}</h3><p>{copy.credentialIsolationLead}</p></div><button ref={first} type="button" disabled={locked} onClick={closeSafely} aria-label={copy.close}>×</button></header>
    {error && <div className="adm-inline-message error" role="alert">{copy.commandErrors?.[error] || copy.credentialOperationFailed}</div>}
    {!launch && <form onSubmit={prepare}><label>{copy.reason}<textarea required minLength="10" maxLength="2000" disabled={state === 'PREPARING' || state === 'UNKNOWN'} value={reason} onChange={(event) => setReason(event.target.value)} /></label><footer><button type="button" disabled={state === 'PREPARING' || state === 'UNKNOWN'} onClick={closeSafely}>{copy.cancel}</button><button type="submit" disabled={state === 'PREPARING'}>{state === 'PREPARING' ? copy.submitting : state === 'UNKNOWN' ? copy.actionRecoverSameKey : copy.prepareSecureWindow}</button></footer></form>}
    {['WAITING', 'UNKNOWN', 'CANCELLING'].includes(state) && <><div className="adm-source-state" role="status">{copy.waitingCredentialResult}</div>{state !== 'CANCELLING' && <footer><button type="button" onClick={closeSafely}>{copy.cancel}</button></footer>}</>}
    {state === 'SUCCEEDED' && <><div className="adm-inline-message success" role="status">{copy.credentialUpdated}</div><footer><button type="button" onClick={onClose}>{copy.close}</button></footer></>}
    {state === 'FAILED' && <footer><button type="button" onClick={onClose}>{copy.close}</button></footer>}
  </div></div>;
}

function CredentialsTab({ client, copy, account, query, onQueryChange, cursorHistory, actionsEnabled, permissions }) {
  const [refresh, setRefresh] = useState(0);
  const [launch, setLaunch] = useState(null);
  const [action, setAction] = useState(null);
  const canManage = actionsEnabled && permissions.includes('platform.tokens.manage');
  const canValidate = actionsEnabled && permissions.includes('platform.tokens.credentials.validate');
  const canRevoke = actionsEnabled && permissions.includes('platform.tokens.credentials.revoke.request');
  const load = useMemo(() => (filters, signal) => client.tokenAccountCredentials(account.token_account_id, filters, signal), [client, account.token_account_id]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion: refresh });
  useEffect(() => { if (!canValidate && action?.type === 'VALIDATE' || !canRevoke && action?.type === 'REVOKE') setAction(null); }, [canValidate, canRevoke, action?.type]);
  const retry = () => setRefresh((value) => value + 1);
  const credential = action?.credential;
  const validateCommand = credential ? { token_account_id: account.token_account_id, credential_id: credential.credential_id } : null;
  return <>
    <section className="adm-card adm-source-card" aria-labelledby="token-credentials-heading"><SourceHeader title={copy.credentialMetadata} titleID="token-credentials-heading" state={state} copy={copy} onRetry={retry} actions={canManage && <button type="button" onClick={() => setLaunch({ action: state.items.some((item) => item.status === 'ACTIVE') ? 'ROTATE' : 'ENROLL', credential: state.items.find((item) => item.status === 'ACTIVE') })}>{state.items.some((item) => item.status === 'ACTIVE') ? copy.rotateCredential : copy.enrollCredential}</button>} />
      <p className="adm-source-lead">{copy.credentialSafety}</p><FilterForm source="tokenCredentials" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'status', label: copy.status, options: ['CANDIDATE', 'ACTIVE', 'RETIRED', 'REVOKED'] }]} /><SourceFeedback state={state} copy={copy} onRetry={retry} />
      {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.credentialType}</th><th>{copy.version}</th><th>{copy.status}</th><th>{copy.validationStatus}</th><th>{copy.fingerprint}</th><th>{copy.responsibleParty}</th><th>{copy.updatedAt}</th>{(canValidate || canRevoke) && <th>{copy.actions}</th>}</tr></thead><tbody>{state.items.map((item) => <tr key={item.credential_id}><td data-label={copy.credentialType}>{item.credential_type}</td><td data-label={copy.version}>v{item.version}</td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.validationStatus}><Status value={item.validation_status} copy={copy} /></td><td data-label={copy.fingerprint}><span className="adm-mono">{item.fingerprint}</span></td><td data-label={copy.responsibleParty}><span className="adm-mono">{item.responsible_actor_id}</span></td><td data-label={copy.updatedAt}>{date(item.updated_at)}</td>{(canValidate || canRevoke) && <td data-label={copy.actions}><div className="adm-row-actions">{canValidate && item.status === 'ACTIVE' && <button type="button" onClick={() => setAction({ type: 'VALIDATE', credential: item })}>{copy.validateCredential}</button>}{canRevoke && ['ACTIVE', 'RETIRED'].includes(item.status) && <button type="button" className="danger" onClick={() => setAction({ type: 'REVOKE', credential: item })}>{copy.revokeCredential}</button>}</div></td>}</tr>)}</tbody></table></div>}
      <Pagination source="tokenCredentials" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
    </section>
    {launch && <CredentialLaunchDialog client={client} copy={copy} account={account} action={launch.action} credential={launch.credential} enabled={canManage} disabledReason={actionsEnabled ? 'CREDENTIAL_PERMISSION_REVOKED' : 'CREDENTIAL_FEATURE_DISABLED'} onClose={() => setLaunch(null)} onCompleted={retry} />}
    {action?.type === 'VALIDATE' && credential && canValidate && <AdminHighRiskActionDialog action={{ key: `credential-validate-${credential.credential_id}`, impactAction: 'TOKEN_CREDENTIAL_VALIDATE', title: copy.validateCredential, lead: copy.validateCredentialLead }} copy={copy} target={{ label: `${copy.credentialType} ${credential.credential_type}`, version: `v${credential.version}` }} policy="AUDIT_ONLY" recentAuth impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_CREDENTIAL_VALIDATE', target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credential.credential_id, command: validateCommand }, signal), { commandType: 'TOKEN_CREDENTIAL_VALIDATE', targetResourceType: 'PROVIDER_CREDENTIAL', targetResourceID: credential.credential_id })} payloadDetails={validateCommand} onClose={() => setAction(null)} execute={async (payload, idempotencyKey, signal) => requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_CREDENTIAL_VALIDATE', targetResourceType: 'PROVIDER_CREDENTIAL', targetResourceID: credential.credential_id, reason: payload.reason, impact: payload.precheck, command: validateCommand }), idempotencyKey, signal), { commandType: 'TOKEN_CREDENTIAL_VALIDATE', targetResourceType: 'PROVIDER_CREDENTIAL', targetResourceID: credential.credential_id })} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_CREDENTIAL_VALIDATE', targetResourceType: 'PROVIDER_CREDENTIAL', targetResourceID: credential.credential_id })} onCompleted={retry} />}
    {action?.type === 'REVOKE' && credential && canRevoke && <CredentialRevokeDialog client={client} copy={copy} account={account} credential={credential} onClose={() => setAction(null)} onCompleted={retry} onNavigate={onNavigate} />}
  </>;
}

function CredentialRevokeDialog({ client, copy, account, credential, onClose, onCompleted, onNavigate }) {
  const command = { token_account_id: account.token_account_id, expected_account_version: account.account_version, credential_id: credential.credential_id, credential_version: credential.version };
  const draft = (payload, idempotencyKey) => ({ command_type: 'TOKEN_CREDENTIAL_REVOKE', idempotency_key: idempotencyKey, normalized_command: command, target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credential.credential_id, reason: payload.reason, redacted_evidence: { precheck_fingerprint: payload.precheck.precheck_fingerprint, token_observed_at: payload.precheck.token_observed_at, billing_observed_at: payload.precheck.billing_observed_at || null }, expected_impact: { blockers: payload.precheck.blockers, dependencies: payload.precheck.evidence.dependencies }, critical_resource_versions: payload.precheck.critical_resource_versions, financial_snapshot: payload.precheck.financial_snapshot });
  return <AdminHighRiskActionDialog action={{ key: `credential-revoke-${credential.credential_id}`, impactAction: 'TOKEN_CREDENTIAL_REVOKE', title: copy.revokeCredential, lead: copy.revokeCredentialLead }} copy={copy} target={{ label: credential.credential_type, version: `v${credential.version}` }} policy="APPROVAL_REQUIRED" recentAuth dangerous payloadDetails={command} impactLoader={async (signal) => requireTokenApprovalImpact(await client.tokenCredentialImpact(account.token_account_id, 'REVOKE', credential.credential_id, signal), { action: 'TOKEN_CREDENTIAL_REVOKE', commandType: 'TOKEN_CREDENTIAL_REVOKE', targetResourceID: credential.credential_id })} onClose={onClose} execute={async (payload, idempotencyKey, signal) => createAndSubmitCredentialApproval(client, draft(payload, idempotencyKey), signal)} recover={(reference, signal, payload) => recoverCredentialApproval(client, reference, draft(payload, 'credential-recovery'), signal)} onCompleted={onCompleted} onOpenApproval={(id) => onNavigate?.('approvals', { approval_command_type: 'TOKEN_CREDENTIAL_REVOKE', approval_target: credential.credential_id, approval_request_id: id })} />;
}

function RoutesTab({ client, copy, account, query, onQueryChange, cursorHistory }) {
  const [refresh, setRefresh] = useState(0);
  const load = useMemo(() => (filters, signal) => client.tokenAccountRoutes(account.token_account_id, filters, signal), [client, account.token_account_id]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion: refresh });
  return <Source title={copy.routeHealth} id="token-routes-heading" state={state} copy={copy} retry={() => setRefresh((v) => v + 1)}><FilterForm source="tokenRoutes" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'health_status', label: copy.health, options: ['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'] }]} /><div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.protocol}</th><th>{copy.health}</th><th>{copy.healthEvidence}</th><th>{copy.routable}</th></tr></thead><tbody>{state.items.map((route) => { const health = serverRouteHealth(route, state.page?.asOf); return <tr key={route.route_id}><td data-label={copy.model}><span className="adm-mono">{route.model_id}</span></td><td data-label={copy.protocol}>{route.protocol_code}</td><td data-label={copy.health}><Status value={health.status} copy={copy} /></td><td data-label={copy.healthEvidence}>{date(health.observedAt)}</td><td data-label={copy.routable}>{health.routable ? copy.yes : copy.no}</td></tr>; })}</tbody></table></div><Pagination source="tokenRoutes" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} /></Source>;
}

function ProvisioningsTab({ client, copy, accountID, query, onQueryChange, cursorHistory }) {
  const [refresh, setRefresh] = useState(0);
  const load = useMemo(() => (filters, signal) => client.provisionings({ ...filters, token_account_id: accountID }, signal), [client, accountID]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion: refresh });
  return <Source title={copy.provisionings} id="token-provisionings-heading" state={state} copy={copy} retry={() => setRefresh((v) => v + 1)}><FilterForm source="tokenProvisionings" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'q', label: copy.provisioningSearch }, { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] }]} /><div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.provisioning}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.provisioning_id}><td data-label={copy.provisioning}><span className="adm-mono">{item.provisioning_id}</span></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.stage}>{copy.enumLabels[item.stage] || item.stage}</td><td data-label={copy.generation}>{item.actual_generation ?? '—'} / {item.desired_generation}</td><td data-label={copy.updatedAt}>{date(item.updated_at)}</td></tr>)}</tbody></table></div><Pagination source="tokenProvisionings" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} /></Source>;
}

function AuditTab({ client, copy, accountID, query, onQueryChange, cursorHistory }) {
  const [refresh, setRefresh] = useState(0);
  const load = useMemo(() => (filters, signal) => client.tokenResourceAudit({ ...filters, resource_type: 'TOKEN_ACCOUNT', resource_id: accountID }, signal), [client, accountID]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion: refresh });
  return <Source title={copy.detailAudit} id="token-audit-heading" state={state} copy={copy} retry={() => setRefresh((v) => v + 1)}><FilterForm source="tokenAudit" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'action', label: copy.auditAction }, { key: 'actor_id', label: copy.auditActor }, { key: 'occurred_from', label: copy.createdFrom, type: 'datetime-local' }, { key: 'occurred_to', label: copy.createdTo, type: 'datetime-local' }]} /><div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.auditAction}</th><th>{copy.auditActor}</th><th>{copy.occurredAt}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.audit_id}><td data-label={copy.auditAction}>{item.action}</td><td data-label={copy.auditActor}><span className="adm-mono">{item.actor_id}</span></td><td data-label={copy.occurredAt}>{date(item.occurred_at)}</td></tr>)}</tbody></table></div><Pagination source="tokenAudit" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} /></Source>;
}

export function AdminTokenAccountDetailWorkspace({ accountID, client, copy, permissions, routeVersion, onRouteChange, onQueryChange, cursorHistory, onTabsKeyDown, actionsEnabled, onNavigate }) {
  const [refresh, setRefresh] = useState(0);
  const requested = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('token_detail_tab') || 'overview', [routeVersion]);
  const tabs = useMemo(() => TABS.filter((item) => item !== 'audit' || permissions.includes('platform.audit.read')), [permissions]);
  const tab = tabs.includes(requested) ? requested : 'overview';
  const load = useMemo(() => async (_, signal) => { const value = await client.tokenAccountSafeDetail(accountID, signal); if (value.token_account_id !== accountID) throw Object.assign(new Error('account mismatch'), { code: 'INVALID_TOKEN_ACCOUNT_DETAIL' }); return asCollection(value); }, [client, accountID]);
  const state = useAdminSource({ enabled: true, load, query: { token_account_id: accountID }, refreshVersion: refresh });
  const account = state.items[0];
  const queries = { credentials: readAdminModelTokenQuery('tokenCredentials'), routes: readAdminModelTokenQuery('tokenRoutes'), provisionings: readAdminModelTokenQuery('tokenProvisionings'), audit: readAdminModelTokenQuery('tokenAudit') };
  const back = () => onRouteChange({ token_account_id: null, token_detail_tab: null, ...adminModelTokenQueryUpdates('tokenCredentials', {}), ...adminModelTokenQueryUpdates('tokenRoutes', {}), ...adminModelTokenQueryUpdates('tokenProvisionings', {}), ...adminModelTokenQueryUpdates('tokenAudit', {}) }, 'push');
  return <article className="adm-model-detail"><header className="adm-card adm-model-detail-header"><button type="button" className="adm-back" onClick={back}>← {copy.backToAccounts}</button><div><p>{copy.tokenAccountDetail}</p><h2>{account?.name || copy.loading}</h2><span className="adm-mono">{accountID}</span></div><div><small>{state.updatedAt ? `${copy.lastUpdated} ${date(state.updatedAt)}` : copy.notUpdated}</small><button type="button" disabled={state.loading} onClick={() => setRefresh((v) => v + 1)}>{copy.refreshSource}</button></div></header><SourceFeedback state={state} copy={copy} onRetry={() => setRefresh((v) => v + 1)} />
    {account && <><div className="adm-tabs adm-model-detail-tabs" role="tablist" aria-label={copy.tokenAccountViews} onKeyDown={onTabsKeyDown}>{tabs.map((item) => <button type="button" role="tab" id={`token-detail-tab-${item}`} aria-controls={`token-detail-panel-${item}`} aria-selected={tab === item} tabIndex={tab === item ? 0 : -1} className={tab === item ? 'active' : ''} key={item} onClick={() => onRouteChange({ token_detail_tab: item }, 'push')}>{copy.tokenDetailTabs[item]}</button>)}</div><div role="tabpanel" id={`token-detail-panel-${tab}`} aria-labelledby={`token-detail-tab-${tab}`}>
      {tab === 'overview' && <section className="adm-card adm-model-detail-panel" aria-labelledby="token-overview-heading"><h2 id="token-overview-heading">{copy.detailOverview}</h2><Facts><Fact label={copy.status}><Status value={account.status} copy={copy} /></Fact><Fact label={copy.owner}>{copy.enumLabels[account.owner_type] || account.owner_type}</Fact><Fact label={copy.shareScope}>{copy.enumLabels[account.share_scope] || account.share_scope}</Fact><Fact label={copy.accountVersion}>{account.account_version}</Fact><Fact label={copy.credentials}>{account.active_credential_count}</Fact><Fact label={copy.routes}>{account.route_count}</Fact><Fact label={copy.updatedAt}>{date(account.updated_at)}</Fact></Facts></section>}
      {tab === 'credentials' && <CredentialsTab client={client} copy={copy} account={account} query={queries.credentials} onQueryChange={onQueryChange} cursorHistory={cursorHistory} actionsEnabled={actionsEnabled} permissions={permissions} />}
      {tab === 'routes' && <RoutesTab client={client} copy={copy} account={account} query={queries.routes} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
      {tab === 'provisionings' && <ProvisioningsTab client={client} copy={copy} accountID={accountID} query={queries.provisionings} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
      {tab === 'audit' && <AuditTab client={client} copy={copy} accountID={accountID} query={queries.audit} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
    </div></>}
  </article>;
}
