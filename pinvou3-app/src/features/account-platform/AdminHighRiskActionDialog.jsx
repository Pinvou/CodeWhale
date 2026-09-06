import React, { useEffect, useRef, useState } from 'react';
import { executeHighRiskCommand, resetHighRiskCommandAfterSafeFailure, validImpactPrecheck } from './admin-high-risk-action-state.js';
import { validateCatalogDifference } from './admin-model-operations-overview.js';

function trapFocus(event, root) {
  if (event.key !== 'Tab') return;
  const controls = [...(root?.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])') || [])];
  if (!controls.length) return;
  if (event.shiftKey && document.activeElement === controls[0]) { event.preventDefault(); controls.at(-1).focus(); }
  else if (!event.shiftKey && document.activeElement === controls.at(-1)) { event.preventDefault(); controls[0].focus(); }
}

function ImpactFacts({ impact, copy }) {
  const dependencies = impact?.evidence?.dependencies || {};
  const facts = [
    [copy.actionRoutes, dependencies.routes], [copy.actionProvisionings, dependencies.provisionings],
    [copy.actionInflight, dependencies.inflight_requests], [copy.actionSessions, dependencies.active_sessions],
    [copy.actionCoverageGaps, dependencies.coverage_gaps], [copy.actionAlternatives, Number(dependencies.alternative_credentials || 0) + Number(dependencies.alternative_accounts || 0)],
  ];
  const catalogDiff = validateCatalogDifference(impact?.catalog_diff);
  const catalogDiffUnavailable = Object.hasOwn(impact || {}, 'catalog_diff') && !catalogDiff;
  const providerObservation = catalogDiff?.target_observed_version !== null;
  return <><dl className="adm-command-target adm-high-risk-impact">{facts.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{Number(value || 0).toLocaleString()}</dd></div>)}</dl>
    {catalogDiff && <section className="adm-catalog-diff" aria-labelledby="catalog-diff-title"><h4 id="catalog-diff-title">{copy.catalogDiffTitle}</h4><dl><div><dt>{copy.catalogAdded}</dt><dd>{catalogDiff.added_count.toLocaleString()}</dd></div><div><dt>{copy.catalogModified}</dt><dd>{catalogDiff.modified_count.toLocaleString()}</dd></div><div><dt>{providerObservation ? copy.catalogMissing : copy.catalogRemoved}</dt><dd>{catalogDiff.removed_count.toLocaleString()}</dd></div></dl>{catalogDiff.total_count === 0 ? <p>{copy.catalogDiffEmpty}</p> : <ul>{catalogDiff.items.map((item, index) => <li key={item.model_id || `${item.change_type}-${index}`}><span>{item.change_type === 'REMOVED' && providerObservation ? copy.catalogMissing : copy.catalogChangeTypes[item.change_type]}</span><strong>{item.display_name || copy.catalogUnnamedModel}</strong>{item.model_id && <code>{item.model_id}</code>}</li>)}</ul>}{catalogDiff.truncated && <small>{copy.catalogDiffTruncated(catalogDiff.total_count, catalogDiff.items.length)}</small>}<small>{catalogDiff.target_catalog_version ? copy.catalogVersionPair(catalogDiff.baseline_catalog_version, catalogDiff.target_catalog_version) : copy.catalogObservedTarget(catalogDiff.baseline_catalog_version)}</small></section>}
    {catalogDiffUnavailable && <div className="adm-inline-message error" role="alert">{copy.catalogDiffUnavailable}</div>}
    {impact.blockers.length > 0 && <div className="adm-inline-message error" role="alert"><strong>{copy.actionBlocked}</strong><ul>{impact.blockers.map((blocker) => <li key={blocker}>{copy.impactBlockers?.[blocker] || blocker}</li>)}</ul></div>}
  </>;
}

export function AdminHighRiskActionDialog({ action, copy, target, policy = 'AUDIT_ONLY', recentAuth = false, impactLoader, precheckEnabled = true, impactRequired = true, payloadDetails = {}, execute, recover, recoverByIdempotencyKey, onClose, onCompleted, onManualReview, reasonMin = 10, reasonMax = 2000, dangerous = false, initialReason = '', children }) {
  const root = useRef(null);
  const first = useRef(null);
  const previousFocus = useRef(document.activeElement);
  const command = useRef({ command: null, inflight: false });
  const impactLoaderRef = useRef(impactLoader);
  const executionController = useRef(null);
  const [reason, setReason] = useState(initialReason);
  const [confirmed, setConfirmed] = useState(!dangerous);
  const [impact, setImpact] = useState(null);
  const [impactGeneration, setImpactGeneration] = useState(0);
  const [state, setState] = useState({ loading: Boolean(impactRequired && impactLoader && precheckEnabled), busy: false, error: null, commandError: false, unknown: false, success: '' });
  impactLoaderRef.current = impactLoader;
  useEffect(() => {
    first.current?.focus();
    return () => {
      executionController.current?.abort();
      previousFocus.current?.focus?.();
    };
  }, []);
  useEffect(() => {
    if (!impactRequired) { setImpact(null); setState((current) => ({ ...current, loading: false, error: null })); return undefined; }
    if (!impactLoaderRef.current || !precheckEnabled) { setImpact(null); setState((current) => ({ ...current, loading: false, error: { code: 'PRECHECK_UNAVAILABLE', requestID: '' } })); return undefined; }
    const controller = new globalThis.AbortController();
    setState((current) => ({ ...current, loading: true, error: null }));
    void impactLoaderRef.current(controller.signal).then((value) => {
      if (controller.signal.aborted) return;
      if (!validImpactPrecheck(value, action.impactAction)) throw Object.assign(new Error('invalid impact'), { code: 'INVALID_IMPACT_RESPONSE' });
      setImpact(value);
      setState((current) => ({ ...current, loading: false }));
    }).catch((error) => {
      if (!controller.signal.aborted) setState((current) => ({ ...current, loading: false, commandError: false, error: { code: error.code || 'PRECHECK_UNAVAILABLE', requestID: error.requestID || '' } }));
    });
    return () => controller.abort();
  }, [action.key, impactGeneration, impactRequired, precheckEnabled]);
  const normalizedReason = reason.trim();
  const reasonLength = [...normalizedReason].length;
  const frozenReference = command.current?.command?.unknown ? command.current.command.error?.commandReference : '';
  const recoveryBlocked = state.unknown && (!(recover && frozenReference) && !recoverByIdempotencyKey);
  const malformedCatalogDiff = Object.hasOwn(impact || {}, 'catalog_diff') && !validateCatalogDifference(impact.catalog_diff);
  const blocked = (impactRequired && (!impact || impact.blockers?.length > 0 || malformedCatalogDiff)) || state.loading || state.busy || state.terminal || recoveryBlocked || reasonLength < reasonMin || reasonLength > reasonMax || !confirmed;
  const closeDialog = () => {
    const completed = state.success;
    onClose();
    if (completed) void onCompleted?.(completed);
  };
  const submit = async (event) => {
    event.preventDefault();
    if (blocked) return;
    setState((current) => ({ ...current, busy: true, error: null, success: '' }));
    const payload = { reason: normalizedReason, precheck: impact, details: payloadDetails };
    executionController.current = new globalThis.AbortController();
    const commandReference = frozenReference;
    if (state.unknown && (!(commandReference && recover) && !recoverByIdempotencyKey)) return;
    const recovering = state.unknown;
    const send = recovering
      ? async () => {
					try {
						if (commandReference && recover) return await recover(commandReference, executionController.current.signal, command.current?.command?.payload);
						return await recoverByIdempotencyKey(command.current?.command?.idempotencyKey, executionController.current.signal, command.current?.command?.payload);
					}
          catch (error) {
            if (error?.finalResult === true) throw error;
						if (error?.status === 404 && error?.code === 'NOT_FOUND') throw error;
            throw Object.assign(new Error('command recovery remains unknown'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', requestID: error?.requestID || '', commandReference });
          }
        }
      : (input, key) => execute(input, key, executionController.current.signal);
    const result = await executeHighRiskCommand(command, payload, send, undefined, { recovery: recovering });
    if (result.duplicate) return;
    if (result.ok) {
      setReason('');
      setState({ loading: false, busy: false, error: null, commandError: false, unknown: false, terminal: true, success: result.value || {} });
      return;
    }
		if (result.error?.commandResult?.recovery_state === 'MANUAL_REVIEW') {
			onManualReview?.(result.error.commandResult);
			return;
		}
    setState({ loading: false, busy: false, error: result.error, commandError: true, unknown: result.command.unknown, terminal: result.command.terminal, success: '' });
  };
  const retryPrecheck = () => {
    resetHighRiskCommandAfterSafeFailure(command, { preserveKey: action.impactAction === 'TOKEN_ACCOUNT_CREATE' });
    setImpact(null);
    setState((current) => ({ ...current, error: null, loading: Boolean(impactLoaderRef.current) }));
    setImpactGeneration((value) => value + 1);
  };
  return <div className="adm-protocol-dialog-backdrop"><form ref={root} className="adm-protocol-dialog adm-high-risk-dialog" role="dialog" aria-modal="true" aria-labelledby={`${action.key}-dialog-title`} onSubmit={submit} onKeyDown={(event) => { trapFocus(event, root.current); if (event.key === 'Escape' && !state.busy && !state.unknown) closeDialog(); }}>
    <header><div><h3 id={`${action.key}-dialog-title`}>{action.title}</h3><p>{action.lead || copy.actionLead}</p></div><button ref={first} type="button" disabled={state.busy || state.unknown} onClick={closeDialog} aria-label={copy.close}>×</button></header>
    <dl className="adm-command-target"><div><dt>{copy.target}</dt><dd>{target.label}</dd></div><div><dt>{copy.actionResourceVersion}</dt><dd>{target.version || copy.serverResolvedBeforeSubmit}</dd></div><div><dt>{copy.policy}</dt><dd>{policy === 'APPROVAL_REQUIRED' ? copy.approvalRequired : copy.auditOnly}</dd></div><div><dt>{copy.actionEffectiveAt}</dt><dd>{target.effectiveAt || copy.actionImmediate}</dd></div><div><dt>{copy.idempotency}</dt><dd>{copy.fixedIdempotency}</dd></div></dl>
    {recentAuth && <div className="adm-inline-message info" role="status">{copy.recentAuthRequired}</div>}
    {state.loading && <div className="adm-source-state" role="status">{copy.actionLoadingImpact}</div>}
    {state.error && <div className={`adm-inline-message error${state.unknown ? ' adm-command-unknown' : ''}`} role="alert"><strong>{state.unknown ? copy.actionUnknown : state.commandError ? copy.actionSubmitFailed : copy.actionPrecheckFailed}</strong>{state.error.commandReference && <small>{copy.commandReference}: {state.error.commandReference}</small>}{state.error.requestID && <small>{copy.requestID}: {state.error.requestID}</small>}</div>}
    {state.success && <div className="adm-inline-message success" role="status"><strong>{copy.actionAccepted}</strong>{state.success.command_reference && <small>{copy.commandReference}: {state.success.command_reference}</small>}{state.success.approval_request_id && <small>{copy.actionApprovalReference}: {state.success.approval_request_id}</small>}{state.success.audit_request_id && <small>{copy.actionAuditReference}: {state.success.audit_request_id}</small>}</div>}
    {impact && <ImpactFacts impact={impact} copy={copy} />}
    {children && <fieldset className="adm-high-risk-fields" disabled={state.busy || state.unknown || state.terminal}>{children}</fieldset>}
    <label>{copy.reason}<textarea required minLength={reasonMin} maxLength={reasonMax} rows="4" disabled={state.busy || state.unknown || state.terminal} value={reason} onChange={(event) => { setReason(event.target.value); if (state.error && !state.unknown) { resetHighRiskCommandAfterSafeFailure(command); setState((current) => ({ ...current, error: null })); } }} /></label>
    {dangerous && <label className="adm-danger-confirm"><input type="checkbox" checked={confirmed} disabled={state.busy || state.unknown || state.terminal} onChange={(event) => setConfirmed(event.target.checked)} /><span>{copy.actionDangerConfirm}</span></label>}
    <p className="adm-safe-preview-note">{copy.actionAuditNotice}</p>
    <footer><button type="button" disabled={state.busy || state.unknown} onClick={closeDialog}>{state.terminal ? copy.close : copy.cancel}</button>{state.error && !state.unknown && !state.terminal && <button type="button" onClick={retryPrecheck}>{copy.actionReload}</button>}{!state.terminal && <button className={dangerous ? 'danger' : ''} type="submit" disabled={blocked}>{state.busy ? copy.submitting : state.unknown && recoveryBlocked ? copy.actionRecoveryUnavailable : state.unknown ? copy.actionRecoverSameKey : policy === 'APPROVAL_REQUIRED' ? copy.actionRequestApproval : copy.confirm}</button>}</footer>
  </form></div>;
}
