import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, previousAdminModelTokenCursor, readAdminModelTokenQuery, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';
import { createProtocolFormulaIdempotencyKey, formulaDraftCommand, protocolDraftCommand, safeFormulaInputs, safeFormulaPreviewSummary, safeProtocolFormulaFailure } from './admin-protocol-formula-state.js';
import { classifyHighRiskFailure, governedTokenEnvelope, highRiskPayloadKey, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck } from './admin-high-risk-action-state.js';

function useSource(load, query, refreshVersion) {
  const [state, setState] = useState(initialAdminSourceState);
  const generation = useRef(0);
  const queryKey = JSON.stringify(query);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    void executeAdminSourceRequest({
      requestID: ++generation.current,
      queryKey,
      query,
      load,
      signal: controller.signal,
      transition: (event) => setState((previous) => reduceAdminSourceState(previous, event)),
    });
    return () => controller.abort();
  }, [load, queryKey, refreshVersion]);
  return visibleAdminSourceState(state, queryKey, true);
}

function Filter({ source, query, copy, fields, onChange }) {
  const [draft, setDraft] = useState(query);
  useEffect(() => setDraft(query), [JSON.stringify(query)]);
  const submittedQuery = () => Object.fromEntries(Object.entries(draft).map(([key, value]) => {
    if (!value || !fields.some((field) => field.key === key && field.type === 'datetime-local')) return [key, value];
    const instant = new Date(value);
    return [key, Number.isNaN(instant.getTime()) ? value : instant.toISOString()];
  }));
  const fieldValue = (field) => {
    const value = draft[field.key] || '';
    if (!value || field.type !== 'datetime-local') return value;
    const instant = new Date(value);
    return Number.isNaN(instant.getTime()) ? '' : new Date(instant.getTime() - instant.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
  };
  return <form className="adm-model-token-filters" onSubmit={(event) => { event.preventDefault(); onChange(source, { ...submittedQuery(), cursor: '' }); }}>
    {fields.map((field) => <label key={field.key}><span>{field.label}</span>{field.options
      ? <select value={draft[field.key] || ''} onChange={(event) => setDraft((value) => ({ ...value, [field.key]: event.target.value }))}><option value="">{copy.all}</option>{field.options.map((option) => <option key={option} value={option}>{copy.enumLabels[option] || option}</option>)}</select>
      : <input type={field.type || 'search'} value={fieldValue(field)} placeholder={field.placeholder || ''} onChange={(event) => setDraft((value) => ({ ...value, [field.key]: event.target.value }))} />}</label>)}
    <button type="submit">{copy.search}</button><button type="button" className="secondary" onClick={() => onChange(source, Object.fromEntries(Object.keys(query).map((key) => [key, ''])))}>{copy.clear}</button>
  </form>;
}

function Feedback({ state, copy, retry }) {
  if (state.error) return <div className="adm-source-error" role="alert"><strong>{state.error.status === 410 ? copy.cursorExpired : copy.sourceUnavailable}</strong><span>{state.error.requestID ? `${copy.requestID}: ${state.error.requestID}` : copy.retryHelp}</span><button type="button" onClick={retry}>{copy.retry}</button></div>;
  if (state.loading && !state.items.length) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && !state.items.length) return <div className="adm-source-state">{copy.noResults}</div>;
  return null;
}

function Paging({ source, query, state, copy, cursorHistory, onChange }) {
  if (!state.page || state.error) return null;
  const history = synchronizeAdminModelTokenCursorHistory(cursorHistory.current[source], query);
  cursorHistory.current[source] = history;
  const previous = previousAdminModelTokenCursor(history);
  return <div className="adm-pagination adm-model-token-pagination"><span>{copy.pageCount(state.items.length, state.page.hasMore)}</span>{state.page.asOf && <small>{copy.snapshotAt} {new Date(state.page.asOf).toLocaleString()}</small>}<div className="adm-row-actions">
    <button type="button" disabled={state.loading || !previous.available} onClick={() => { cursorHistory.current[source] = { ...history, currentCursor: previous.cursor }; onChange(source, { ...query, cursor: previous.cursor }); }}>{copy.previous}</button>
    <button type="button" disabled={state.loading || !state.page.nextCursor} onClick={() => { cursorHistory.current[source] = rememberAdminModelTokenNextCursor(history, state.page.nextCursor); onChange(source, { ...query, cursor: state.page.nextCursor }); }}>{copy.next}</button>
  </div></div>;
}

function Status({ value, copy }) {
  const tone = value === 'PUBLISHED' ? 'ok' : value === 'DRAFT' ? 'warn' : '';
  return <span className={`adm-status ${tone}`}>{copy.enumLabels[value] || value}</span>;
}

function SourceHead({ title, titleID, state, copy, retry, action }) {
  return <div className="adm-source-head"><div><h2 id={titleID}>{title}</h2><p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p></div><div className="adm-row-actions">{action}<button type="button" disabled={state.loading} onClick={retry}>{state.loading ? copy.loading : copy.refreshSource}</button></div></div>;
}

function keepDialogFocus(event, root) {
  if (event.key !== 'Tab') return;
  const controls = [...(root?.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])') || [])];
  if (!controls.length) return;
  const first = controls[0];
  const last = controls[controls.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}

function Dialog({ action, copy, units, busy, onClose, onSubmit }) {
  const dialogRef = useRef(null);
  const firstRef = useRef(null);
  const previousFocus = useRef(document.activeElement);
  const [reason, setReason] = useState('');
  const [protocolCode, setProtocolCode] = useState('');
  const [requestSchema, setRequestSchema] = useState('{\n  "type": "object",\n  "additionalProperties": false,\n  "properties": {}\n}');
  const [rows, setRows] = useState([{ unitCode: '', unitDefinitionID: '', input: 'request_count' }]);
  const [errorKey, setErrorKey] = useState('');
  useEffect(() => { firstRef.current?.focus(); return () => previousFocus.current?.focus?.(); }, []);
  const title = copy.actionTitles[action.type];
  const submit = async (event) => {
    event.preventDefault();
    let command = { value: {} };
    if (action.type === 'PROTOCOL_DRAFT') command = protocolDraftCommand({ protocolCode, requestSchema, expectedHeadVersion: action.expectedHeadVersion, reason });
    if (action.type === 'FORMULA_DRAFT') command = formulaDraftCommand({ rows, expectedProtocolVersion: action.item.version, expectedFormulaHeadVersion: action.expectedHeadVersion, reason });
    if (command.error) { setErrorKey(command.error); return; }
    if (!command.value.reason && reason.trim().length < 10) { setErrorKey('reasonTooShort'); return; }
    await onSubmit({ ...action, body: command.value, reason: reason.trim() });
  };
  const updateRow = (index, patch) => setRows((current) => current.map((row, rowIndex) => rowIndex === index ? { ...row, ...patch } : row));
  const terminal = action.failure?.terminal;
  const locked = busy || terminal;
	const closeLocked = busy || action.failure?.unknown;
	const fieldsLocked = busy || terminal || action.failure?.unknown;
  return <div className="adm-protocol-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !closeLocked) onClose(); }}><form ref={dialogRef} className="adm-protocol-dialog" role="dialog" aria-modal="true" aria-labelledby="protocol-action-title" onSubmit={submit} onKeyDown={(event) => { keepDialogFocus(event, dialogRef.current); if (event.key === 'Escape' && !closeLocked) onClose(); }}>
    <header><div><h3 id="protocol-action-title">{title}</h3><p>{copy.actionLead}</p></div><button ref={firstRef} type="button" disabled={closeLocked} onClick={onClose} aria-label={copy.close}>×</button></header>
		{action.failure && <div className="adm-inline-message error" role="alert" data-error-code={action.failure.code}>{copy.commandErrors[action.failure.code] || copy.commandFailed}{action.failure.requestID && <small>{copy.requestID}: {action.failure.requestID}</small>}{action.failure.commandReference && <code>{copy.commandReference}: {action.failure.commandReference}</code>}</div>}
		{action.precheck && <dl className="adm-command-target adm-high-risk-impact"><div><dt>{copy.status}</dt><dd>{action.precheck.evidence.current_status}</dd></div><div><dt>{copy.actionRoutes}</dt><dd>{action.precheck.evidence.dependencies.routes}</dd></div><div><dt>{copy.actionProvisionings}</dt><dd>{action.precheck.evidence.dependencies.provisionings}</dd></div><div><dt>{copy.actionInflight}</dt><dd>{action.precheck.evidence.dependencies.inflight_requests}</dd></div></dl>}
		{action.precheck?.blockers?.length > 0 && <div className="adm-inline-message error" role="alert">{copy.actionBlocked}</div>}
    {action.type === 'PROTOCOL_DRAFT' && <><label>{copy.protocolCode}<input required disabled={fieldsLocked} maxLength="80" value={protocolCode} onChange={(event) => setProtocolCode(event.target.value)} /></label><label>{copy.requestSchema}<textarea required disabled={fieldsLocked} rows="8" value={requestSchema} onChange={(event) => setRequestSchema(event.target.value)} /></label></>}
    {action.type === 'FORMULA_DRAFT' && <fieldset disabled={fieldsLocked}><legend>{copy.safeMappings}</legend>{rows.map((row, index) => <div className="adm-formula-row" key={`${index}-${row.unitCode}`}><label>{copy.meteringUnit}<select required value={row.unitDefinitionID} onChange={(event) => { const unit = units.find((item) => item.unit_definition_id === event.target.value); updateRow(index, { unitDefinitionID: event.target.value, unitCode: unit?.unit_code || '' }); }}><option value="">{copy.chooseUnit}</option>{units.filter((item) => item.status === 'ACTIVE').map((unit) => <option key={unit.unit_definition_id} value={unit.unit_definition_id}>{unit.unit_code} · v{unit.version}</option>)}</select></label><label>{copy.safeInput}<select value={row.input} onChange={(event) => updateRow(index, { input: event.target.value })}>{safeFormulaInputs.map((input) => <option key={input} value={input}>{copy.safeInputLabels[input]}</option>)}</select></label><button type="button" disabled={rows.length === 1} onClick={() => setRows((current) => current.filter((_, rowIndex) => rowIndex !== index))}>{copy.removeMapping}</button></div>)}<button type="button" className="adm-add-mapping" disabled={rows.length >= 32} onClick={() => setRows((current) => [...current, { unitCode: '', unitDefinitionID: '', input: 'request_count' }])}>{copy.addMapping}</button><small>{copy.safeFormulaHelp}</small></fieldset>}
    <dl className="adm-command-target">
      <div><dt>{copy.policy}</dt><dd>{copy.auditOnly}</dd></div><div><dt>{copy.idempotency}</dt><dd>{copy.fixedIdempotency}</dd></div>
      {action.item && <div><dt>{copy.target}</dt><dd>{action.item.protocol_code} · v{action.item.version}</dd></div>}
      <div><dt>{copy.casEvidence}</dt><dd>{action.item ? `v${action.item.version} · ETag ${String(action.item.etag || '').slice(0, 12)}…` : copy.serverResolvedBeforeSubmit}</dd></div>
      <div><dt>{copy.impact}</dt><dd>{copy.actionImpacts[action.type]}</dd></div>
      {action.type === 'FORMULA_PUBLISH' && <div><dt>{copy.billingSync}</dt><dd>{copy.billingSyncRequired}</dd></div>}
    </dl>
    {['PROTOCOL_PUBLISH', 'FORMULA_PUBLISH'].includes(action.type) && <p className="adm-safe-preview-note">{copy.recentAuthRequired}</p>}
    <label>{copy.reason}<textarea required disabled={fieldsLocked} minLength="10" maxLength="2000" rows="4" value={reason} onChange={(event) => setReason(event.target.value)} /></label>
    {errorKey && <div className="adm-field-error" role="alert">{copy.validationErrors[errorKey]}</div>}
		<footer><button type="button" disabled={closeLocked} onClick={onClose}>{terminal ? copy.close : copy.cancel}</button>{!terminal && <button type="submit" disabled={locked || Boolean(action.failure && !action.failure.unknown) || Boolean(action.precheck?.blockers?.length)}>{busy ? copy.submitting : action.failure?.unknown ? copy.actionRecoverSameKey : action.precheck ? copy.confirm : copy.actionLoadingImpact}</button>}</footer>
  </form></div>;
}

function Preview({ preview, copy, onClose }) {
  const root = useRef(null);
  const close = useRef(null);
  const previousFocus = useRef(document.activeElement);
  useEffect(() => { close.current?.focus(); return () => previousFocus.current?.focus?.(); }, []);
  return <div className="adm-protocol-dialog-backdrop"><section ref={root} className="adm-protocol-dialog adm-formula-preview" role="dialog" aria-modal="true" aria-labelledby="formula-preview-title" onKeyDown={(event) => { keepDialogFocus(event, root.current); if (event.key === 'Escape') onClose(); }}><header><div><h3 id="formula-preview-title">{copy.previewTitle}</h3><p>{copy.previewLead}</p></div><button ref={close} type="button" onClick={onClose} aria-label={copy.close}>×</button></header><dl className="adm-command-target"><div><dt>{copy.protocol}</dt><dd>{preview.protocolCode} · v{preview.version}</dd></div><div><dt>{copy.deterministic}</dt><dd>{preview.deterministic ? copy.yes : copy.no}</dd></div><div><dt>{copy.externalAccess}</dt><dd>{preview.externalAccess ? copy.yes : copy.no}</dd></div><div><dt>{copy.units}</dt><dd>{preview.unitCodes.join(', ') || '—'}</dd></div></dl><p className="adm-safe-preview-note">{copy.previewSafety}</p><footer><button type="button" onClick={onClose}>{copy.close}</button></footer></section></div>;
}

function PreviewFailure({ failure, copy, onClose }) {
  const root = useRef(null);
  const close = useRef(null);
  const previousFocus = useRef(document.activeElement);
  useEffect(() => { close.current?.focus(); return () => previousFocus.current?.focus?.(); }, []);
  return <div className="adm-protocol-dialog-backdrop"><section ref={root} className="adm-protocol-dialog adm-formula-preview" role="dialog" aria-modal="true" aria-labelledby="formula-preview-error-title" onKeyDown={(event) => { keepDialogFocus(event, root.current); if (event.key === 'Escape') onClose(); }}><div className="adm-source-error" role="alert"><strong id="formula-preview-error-title">{copy.previewFailed}</strong>{failure.requestID && <span>{copy.requestID}: {failure.requestID}</span>}<button ref={close} type="button" onClick={onClose}>{copy.close}</button></div></section></div>;
}

function protocolFormulaDescriptor(next) {
	if (next.type === 'PROTOCOL_DRAFT') return {
		commandType: 'TOKEN_PROTOCOL_DRAFT_CREATE', targetResourceType: 'PROTOCOL', targetResourceID: next.body.protocol_code,
		command: { protocol_code: next.body.protocol_code, request_schema: next.body.request_schema },
	};
	if (next.type === 'PROTOCOL_PUBLISH') return {
		commandType: 'TOKEN_PROTOCOL_PUBLISH', targetResourceType: 'PROTOCOL', targetResourceID: next.item.protocol_definition_id,
		command: { expected_protocol_version: next.item.version, draft_etag: next.item.etag },
	};
	if (next.type === 'FORMULA_DRAFT') return {
		commandType: 'TOKEN_FORMULA_DRAFT_CREATE', targetResourceType: 'FORMULA', targetResourceID: next.item.protocol_definition_id,
		command: { required_unit_versions: next.body.required_unit_versions, expression: next.body.expression },
	};
	if (next.type === 'FORMULA_PUBLISH') return {
		commandType: 'TOKEN_FORMULA_PUBLISH', targetResourceType: 'FORMULA', targetResourceID: next.item.formula_version_id,
		command: { expected_formula_version: next.item.version, draft_etag: next.item.etag },
	};
	throw new Error('unsupported protocol/formula action');
}

export function AdminProtocolFormulaWorkspace({ client, copy, canManage, routeVersion, onRouteChange, cursorHistory }) {
  const protocolQuery = useMemo(() => readAdminModelTokenQuery('protocols'), [routeVersion]);
  const formulaQuery = useMemo(() => readAdminModelTokenQuery('formulas'), [routeVersion]);
  const [protocolRefresh, setProtocolRefresh] = useState(0);
  const [formulaRefresh, setFormulaRefresh] = useState(0);
  const [units, setUnits] = useState({ items: [], loading: canManage, error: null, updatedAt: null });
  const [action, setAction] = useState(null);
  const [preview, setPreview] = useState(null);
  const [receipt, setReceipt] = useState(null);
  const [busy, setBusy] = useState(false);
  const mutationInFlight = useRef(false);
  const mutationController = useRef(null);
  const protocolLoad = useMemo(() => (query, signal) => client.protocols(query, signal), [client]);
  const formulaLoad = useMemo(() => (query, signal) => client.formulas(query, signal), [client]);
  const protocols = useSource(protocolLoad, protocolQuery, protocolRefresh);
  const formulas = useSource(formulaLoad, formulaQuery, formulaRefresh);
  const onChange = (source, query, mode = 'push') => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  const retry = (source, state, query, setter) => {
    if (state.error?.status === 410) { delete cursorHistory.current[source]; onChange(source, { ...query, cursor: '' }, 'replace'); } else setter((value) => value + 1);
  };
  const loadUnits = async () => {
    setUnits((current) => ({ ...current, loading: true, error: null }));
    try { const payload = await client.meteringUnits(); setUnits({ items: payload.items || [], loading: false, error: null, updatedAt: new Date().toISOString() }); }
    catch (error) { setUnits((current) => ({ ...current, loading: false, error: safeProtocolFormulaFailure(error) })); }
  };
  useEffect(() => {
    if (!canManage) { setUnits({ items: [], loading: false, error: null, updatedAt: null }); return; }
    void loadUnits();
  }, [client, canManage]);
  useEffect(() => {
    if (canManage) return;
    mutationController.current?.abort();
    setAction(null);
    setBusy(false);
  }, [canManage]);
  const openFormulaDraft = async (item) => {
    if (units.error || units.loading || !units.items.some((unit) => unit.status === 'ACTIVE')) return;
    setAction({ type: 'FORMULA_DRAFT', item, expectedHeadVersion: Math.max(0, ...formulas.items.filter((formula) => formula.protocol_definition_id === item.protocol_definition_id).map((formula) => Number(formula.version || 0))), idempotencyKey: createProtocolFormulaIdempotencyKey('formula-draft') });
  };
  const runAction = async (next) => {
    if (mutationInFlight.current) return;
    mutationInFlight.current = true;
    setBusy(true); setReceipt(null);
    const controller = new globalThis.AbortController();
    mutationController.current = controller;
    try {
			const descriptor = protocolFormulaDescriptor(next);
			const expected = { commandType: descriptor.commandType, targetResourceType: descriptor.targetResourceType, targetResourceID: descriptor.targetResourceID };
			if (next.failure?.unknown) {
				const payload = requireGovernedTokenCommandResult(await client.governedTokenCommandResult(next.failure.commandReference, controller.signal), expected);
				setReceipt({ commandReference: payload.command_reference, auditRequestID: payload.audit_request_id, commandType: payload.command_type, status: payload.status, billingSync: payload.redacted_result?.billing_sync || '', verifiedUnitCount: Number(payload.redacted_result?.verified_unit_count || 0) });
				setAction(null);
				if (next.type.startsWith('PROTOCOL')) setProtocolRefresh((value) => value + 1); else setFormulaRefresh((value) => value + 1);
				return;
			}
			const precheckKey = highRiskPayloadKey(descriptor);
			if (!next.precheck || next.precheckKey !== precheckKey) {
				const precheck = requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: descriptor.commandType, target_resource_type: descriptor.targetResourceType, target_resource_id: descriptor.targetResourceID, command: descriptor.command }, controller.signal), expected);
				setAction((current) => ({ ...current, precheck, precheckKey, failure: null }));
				return;
			}
			const envelope = await governedTokenEnvelope({ ...descriptor, reason: next.reason, impact: next.precheck });
			const payload = requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(envelope, next.idempotencyKey, controller.signal), expected);
			setReceipt({ commandReference: payload.command_reference, auditRequestID: payload.audit_request_id, commandType: payload.command_type, status: payload.status, billingSync: payload.redacted_result?.billing_sync || '', verifiedUnitCount: Number(payload.redacted_result?.verified_unit_count || 0) });
      setAction(null);
      if (next.type.startsWith('PROTOCOL')) setProtocolRefresh((value) => value + 1);
      else setFormulaRefresh((value) => value + 1);
		} catch (error) {
			if (!controller.signal.aborted && error?.name !== 'AbortError') {
				const dispatched = Boolean(next.precheck && next.precheckKey === highRiskPayloadKey(protocolFormulaDescriptor(next)));
				const classified = dispatched ? classifyHighRiskFailure({}, error) : null;
				const failure = classified
					? { ...classified.error, unknown: classified.unknown, terminal: classified.terminal, locked: classified.locked }
					: { ...safeProtocolFormulaFailure(error), terminal: false };
				setAction((current) => ({ ...current, failure }));
			}
		}
    finally { if (mutationController.current === controller) { mutationController.current = null; mutationInFlight.current = false; setBusy(false); } }
  };
  const openPreview = async (item) => {
    try { setPreview({ loading: true }); const payload = await client.previewFormula(item.formula_version_id); const summary = safeFormulaPreviewSummary(payload); setPreview(summary.deterministic && !summary.externalAccess ? summary : { error: { code: 'UNSAFE_PREVIEW', requestID: '' } }); }
    catch (error) { setPreview({ error: safeProtocolFormulaFailure(error) }); }
  };
  return <div className="adm-protocol-formula-workspace">
    {receipt && <div className="adm-inline-message success" role="status"><strong>{copy.commandCompleted}</strong><span>{copy.commandReference}: {receipt.commandReference}</span><span>{copy.actionAuditReference}: {receipt.auditRequestID}</span>{receipt.billingValidation && <span>{copy.billingValidation}: {copy.verifiedUnits(receipt.verifiedUnitCount)}</span>}{receipt.billingSync && <span>{copy.billingSync}: {copy.enumLabels[receipt.billingSync] || receipt.billingSync} · {copy.verifiedUnits(receipt.verifiedUnitCount)}</span>}</div>}
    {canManage && <div className={`adm-billing-authority ${units.error ? 'error' : ''}`} role={units.error ? 'alert' : 'status'}><div><strong>{copy.billingAuthority}</strong><span>{units.error ? copy.billingUnitsUnavailable : units.loading ? copy.loading : copy.billingUnitsReady(units.items.filter((unit) => unit.status === 'ACTIVE').length)}</span></div>{units.error && <button type="button" onClick={loadUnits}>{copy.retry}</button>}</div>}
    <section className="adm-card adm-source-card" aria-labelledby="protocols-title"><SourceHead title={copy.protocolDefinitions} titleID="protocols-title" state={protocols} copy={copy} retry={() => retry('protocols', protocols, protocolQuery, setProtocolRefresh)} action={canManage && <button type="button" onClick={() => setAction({ type: 'PROTOCOL_DRAFT', expectedHeadVersion: 0, idempotencyKey: createProtocolFormulaIdempotencyKey('protocol-draft') })}>{copy.createProtocolDraft}</button>} />
      <Filter source="protocols" query={protocolQuery} copy={copy} onChange={onChange} fields={[{ key: 'q', label: copy.protocolSearch, placeholder: copy.protocolSearchPlaceholder }, { key: 'status', label: copy.status, options: ['DRAFT', 'PUBLISHED', 'DEPRECATED'] }, { key: 'protocol_code', label: copy.protocolCode }, { key: 'published', label: copy.publishedFilter, options: ['true', 'false'] }, { key: 'created_from', label: copy.createdFrom, type: 'datetime-local' }, { key: 'created_to', label: copy.createdTo, type: 'datetime-local' }]} />
      <Feedback state={protocols} copy={copy} retry={() => retry('protocols', protocols, protocolQuery, setProtocolRefresh)} />
      {protocols.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.protocol}</th><th>{copy.version}</th><th>{copy.status}</th><th>{copy.schemaSummary}</th><th>{copy.createdAt}</th><th>{copy.actions}</th></tr></thead><tbody>{protocols.items.map((item) => <tr key={item.protocol_definition_id}><td data-label={copy.protocol}><strong>{item.protocol_code}</strong><small className="adm-secondary-id">{item.protocol_definition_id}</small></td><td data-label={copy.version}>v{item.version}</td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.schemaSummary}>{copy.schemaFields(Object.keys(item.request_schema?.properties || {}).length)}</td><td data-label={copy.createdAt}>{new Date(item.created_at).toLocaleString()}</td><td data-label={copy.actions}><div className="adm-row-actions">{canManage && item.status === 'DRAFT' && <button type="button" onClick={() => setAction({ type: 'PROTOCOL_PUBLISH', item, idempotencyKey: createProtocolFormulaIdempotencyKey('protocol-publish') })}>{copy.publish}</button>}{canManage && item.status === 'PUBLISHED' && <button type="button" disabled={units.loading || Boolean(units.error)} onClick={() => openFormulaDraft(item)}>{copy.createFormulaDraft}</button>}</div></td></tr>)}</tbody></table></div>}
      <Paging source="protocols" query={protocolQuery} state={protocols} copy={copy} cursorHistory={cursorHistory} onChange={onChange} />
    </section>
    <section className="adm-card adm-source-card" aria-labelledby="formulas-title"><SourceHead title={copy.safeFormulas} titleID="formulas-title" state={formulas} copy={copy} retry={() => retry('formulas', formulas, formulaQuery, setFormulaRefresh)} />
      <Filter source="formulas" query={formulaQuery} copy={copy} onChange={onChange} fields={[{ key: 'q', label: copy.formulaSearch, placeholder: copy.formulaSearchPlaceholder }, { key: 'status', label: copy.status, options: ['DRAFT', 'PUBLISHED', 'DEPRECATED'] }, { key: 'protocol_definition_id', label: copy.protocolID }, { key: 'metering_unit_code', label: copy.unitCode }, { key: 'published', label: copy.publishedFilter, options: ['true', 'false'] }, { key: 'created_from', label: copy.createdFrom, type: 'datetime-local' }, { key: 'created_to', label: copy.createdTo, type: 'datetime-local' }]} />
      <Feedback state={formulas} copy={copy} retry={() => retry('formulas', formulas, formulaQuery, setFormulaRefresh)} />
      {formulas.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.protocol}</th><th>{copy.version}</th><th>{copy.status}</th><th>{copy.units}</th><th>{copy.safety}</th><th>{copy.billingSync}</th><th>{copy.actions}</th></tr></thead><tbody>{formulas.items.map((item) => <tr key={item.formula_version_id}><td data-label={copy.protocol}><strong>{item.protocol_code}</strong><small className="adm-secondary-id">{item.formula_version_id}</small></td><td data-label={copy.version}>v{item.version}</td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.units}>{Object.keys(item.required_unit_versions || {}).sort().join(', ')}</td><td data-label={copy.safety}>{copy.safeEvaluator}</td><td data-label={copy.billingSync}><Status value={item.status === 'PUBLISHED' ? 'SYNCHRONIZED' : 'PENDING'} copy={copy} /></td><td data-label={copy.actions}><div className="adm-row-actions"><button type="button" onClick={() => openPreview(item)}>{copy.preview}</button>{canManage && item.status === 'DRAFT' && <button type="button" onClick={() => setAction({ type: 'FORMULA_PUBLISH', item, idempotencyKey: createProtocolFormulaIdempotencyKey('formula-publish') })}>{copy.publish}</button>}</div></td></tr>)}</tbody></table></div>}
      <Paging source="formulas" query={formulaQuery} state={formulas} copy={copy} cursorHistory={cursorHistory} onChange={onChange} />
    </section>
    {action && canManage && <Dialog action={action} copy={copy} units={units.items} busy={busy} onClose={() => setAction(null)} onSubmit={runAction} />}
    {preview?.loading && <div className="adm-protocol-dialog-backdrop"><div className="adm-protocol-dialog" role="status">{copy.loading}</div></div>}
    {preview?.error && <PreviewFailure failure={preview.error} copy={copy} onClose={() => setPreview(null)} />}
    {preview && !preview.loading && !preview.error && <Preview preview={preview} copy={copy} onClose={() => setPreview(null)} />}
  </div>;
}
