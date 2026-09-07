import { secureRequestID } from './request-id.js';
import React, { useEffect, useMemo, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck, requireTokenApprovalImpact, validApprovalRequest } from './admin-high-risk-action-state.js';
import { PricingLifecycleWorkspace } from './PricingLifecycleWorkspace.jsx';

const MODEL_DETAIL_TABS = Object.freeze(['overview', 'capabilities', 'operations', 'pricing', 'lifecycle', 'audit']);

function asCollection(value, asOf = '') {
  return { items: value == null ? [] : [value], as_of: asOf || value?.as_of || value?.token_observed_at || '', next_cursor: '', has_more: false, limit: 1 };
}

function formatDate(value) {
  return value ? new Date(value).toLocaleString() : '—';
}

function Facts({ children }) {
  return <dl className="adm-model-detail-facts">{children}</dl>;
}

function Fact({ label, children }) {
  return <div><dt>{label}</dt><dd>{children ?? '—'}</dd></div>;
}

function DetailSource({ title, titleID, state, copy, onRetry, children }) {
  return <section className="adm-card adm-source-card adm-model-detail-source" aria-labelledby={titleID}>
    <SourceHeader title={title} titleID={titleID} state={state} copy={copy} onRetry={onRetry} />
    <SourceFeedback state={state} copy={copy} onRetry={onRetry} />
    {!state.error && !state.loading && state.items.length > 0 && children}
  </section>;
}

function OverviewTab({ detail, copy }) {
  const model = detail.model;
  return <section className="adm-card adm-model-detail-panel" aria-labelledby="model-overview-heading">
    <h2 id="model-overview-heading">{copy.detailOverview}</h2>
    <p className="adm-source-lead">{copy.detailOverviewLead}</p>
    <Facts>
      <Fact label={copy.lifecycle}><Status value={model.lifecycle_status} copy={copy} /></Fact>
      <Fact label={copy.catalogVersion}>{model.catalog_version}</Fact>
      <Fact label={copy.capabilityVersion}>{model.capability_version}</Fact>
      <Fact label={copy.protocol}>{model.protocol_code} · v{model.protocol_version}</Fact>
      <Fact label={copy.contextLength}>{Number(model.context_length || 0).toLocaleString()}</Fact>
      <Fact label={copy.maximumOutputTokens}>{Number(model.maximum_output_tokens || 0).toLocaleString()}</Fact>
      <Fact label={copy.modalities}>{(model.supported_modalities || []).join(', ') || '—'}</Fact>
      <Fact label={copy.tokenizerVersion}>{model.tokenizer_version || '—'}</Fact>
      <Fact label={copy.source}><Status value={model.source_available ? 'ACTIVE' : 'UNAVAILABLE'} copy={copy} /></Fact>
    </Facts>
    {model.description && <p className="adm-model-detail-description">{model.description}</p>}
  </section>;
}

function ModelHistory({ modelID, client, copy, query, onQueryChange, cursorHistory }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.modelVersions(modelID, filters, signal), [client, modelID]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.modelHistory;
      onQueryChange('modelHistory', { cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="model-history-heading">
    <SourceHeader title={copy.versionHistory} titleID="model-history-heading" state={state} copy={copy} onRetry={retry} />
    <p className="adm-source-lead">{copy.versionHistoryLead}</p>
    {(state.loading || state.error) && <SourceFeedback state={state} copy={copy} onRetry={retry} />}
    {!state.loading && !state.error && state.items.length === 0 && <p className="adm-source-lead">{copy.noHistoricalVersions}</p>}
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.catalogVersion}</th><th>{copy.lifecycle}</th><th>{copy.capabilityVersion}</th><th>{copy.protocol}</th><th>{copy.schemaSummary}</th><th>{copy.publishedAt}</th></tr></thead><tbody>
      {state.items.map((item) => <tr key={item.catalog_version}><td data-label={copy.catalogVersion}>{item.catalog_version}</td><td data-label={copy.lifecycle}><Status value={item.lifecycle_status} copy={copy} /></td><td data-label={copy.capabilityVersion}>v{item.capability_version}</td><td data-label={copy.protocol}>{item.protocol_code} · v{item.protocol_version}</td><td data-label={copy.schemaSummary}>{copy.schemaFields(item.schema_field_count)}</td><td data-label={copy.publishedAt}>{formatDate(item.published_at)}</td></tr>)}
    </tbody></table></div>}
    <Pagination source="modelHistory" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
  </section>;
}

function CapabilitiesTab({ detail, client, copy, historyQuery, onQueryChange, cursorHistory }) {
  const { model, protocol, formula } = detail;
  return <div className="adm-model-detail-stack">
    <section className="adm-card adm-model-detail-panel" aria-labelledby="model-current-capability-heading">
      <h2 id="model-current-capability-heading">{copy.currentPublishedVersion}</h2>
      <p className="adm-source-lead">{copy.capabilityProtocolLead}</p>
      <Facts>
        <Fact label={copy.capabilityVersion}>{model.capability_version}</Fact>
        <Fact label={copy.capabilityVersionID}><span className="adm-mono">{model.model_capability_version_id}</span></Fact>
        <Fact label={copy.protocol}>{protocol.protocol_code} · v{protocol.version}</Fact>
        <Fact label={copy.protocolID}><span className="adm-mono">{protocol.protocol_definition_id}</span></Fact>
        <Fact label={copy.schemaSummary}>{copy.schemaFields(protocol.schema_field_count)}</Fact>
        <Fact label={copy.status}><Status value={protocol.status} copy={copy} /></Fact>
        <Fact label={copy.safeFormula}>{formula ? `${formula.protocol_code} · v${formula.version}` : copy.notConfigured}</Fact>
        <Fact label={copy.safeEvaluator}>{formula?.evaluator_version || '—'}</Fact>
        <Fact label={copy.units}>{formula ? Object.keys(formula.required_unit_versions || {}).sort().join(', ') : '—'}</Fact>
      </Facts>
    </section>
    <section className="adm-card adm-model-detail-panel" aria-labelledby="model-draft-heading">
      <h2 id="model-draft-heading">{copy.draftVersion}</h2>
      {model.draft ? <Facts>
        <Fact label={copy.version}>{model.draft.draft_version}</Fact>
        <Fact label={copy.catalogVersion}>{model.draft.base_catalog_version}</Fact>
        <Fact label={copy.protocol}>{model.draft.protocol_code} · v{model.draft.protocol_version}</Fact>
        <Fact label={copy.updatedAt}>{formatDate(model.draft.updated_at)}</Fact>
      </Facts> : <p className="adm-source-lead">{copy.noDraft}</p>}
    </section>
    <ModelHistory modelID={model.model_id} client={client} copy={copy} query={historyQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />
  </div>;
}

function OperationsTab({ modelID, client, copy, query, onQueryChange, cursorHistory }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.provisionings({ ...filters, model_id: modelID }, signal), [client, modelID]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.modelProvisionings;
      onQueryChange('modelProvisionings', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="model-runtime-heading">
    <SourceHeader title={copy.detailOperations} titleID="model-runtime-heading" state={state} copy={copy} onRetry={retry} />
    <p className="adm-source-lead">{copy.detailOperationsLead}</p>
    <FilterForm source="modelProvisionings" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.provisioningSearch, placeholder: copy.identifierPlaceholder },
      { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.provisioning}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th></tr></thead><tbody>
      {state.items.map((item) => <tr key={item.provisioning_id}><td data-label={copy.provisioning}><span className="adm-mono">{item.provisioning_id}</span></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.stage}>{copy.enumLabels[item.stage] || item.stage}</td><td data-label={copy.generation}>{item.actual_generation ?? '—'} / {item.desired_generation}</td><td data-label={copy.updatedAt}>{formatDate(item.updated_at)}</td></tr>)}
    </tbody></table></div>}
    <Pagination source="modelProvisionings" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
  </section>;
}

function ProjectionUnits({ point, copy }) {
  if (!point?.price?.units?.length) return null;
  return <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.unitCode}</th><th>{copy.resolvedPrice}</th><th>{copy.priceSource}</th><th>{copy.roundingRule}</th></tr></thead><tbody>{point.price.units.map((unit) => <tr key={unit.unit_definition_id}><td data-label={copy.unitCode}>{unit.unit_code} · v{unit.unit_version}</td><td data-label={copy.resolvedPrice}>{unit.resolved_price} / {unit.price_denominator}</td><td data-label={copy.priceSource}>{copy.enumLabels[unit.price_source_scope] || unit.price_source_scope}<small className="adm-secondary-id">{unit.source_price_version_id}</small></td><td data-label={copy.roundingRule}>{copy.enumLabels[unit.rounding_rule] || unit.rounding_rule}</td></tr>)}</tbody></table></div>;
}

function PricingTab({ detail, client, copy, pricingCopy, permissions, pricingEditEnabled, manageRequested, onRouteChange }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const formula = detail.formula;
  const canManage = pricingEditEnabled && permissions.includes('platform.pricing.manage');
  const query = useMemo(() => ({ model_id: detail.model.model_id, protocol_code: detail.protocol.protocol_code, formula_version_id: formula?.formula_version_id || '', credit_unit: 'MICROCREDIT' }), [detail, formula]);
  const load = useMemo(() => async (filters, signal) => asCollection(await client.modelPriceProjection(filters.model_id, filters.protocol_code, filters.formula_version_id, filters.credit_unit, signal)), [client]);
  const state = useAdminSource({ enabled: Boolean(formula), load, query, refreshVersion });
  if (!formula) return <section className="adm-card adm-model-detail-panel"><h2>{copy.detailPricing}</h2><p className="adm-source-lead">{copy.priceFormulaMissing}</p></section>;
  const projection = state.items[0];
  return <div className="adm-model-detail-stack">
    <DetailSource title={copy.detailPricing} titleID="model-pricing-heading" state={state} copy={copy} onRetry={() => setRefreshVersion((value) => value + 1)}>
      <div className="adm-pricing-context-head"><p className="adm-source-lead">{copy.detailPricingLead}</p>{canManage && <button type="button" onClick={() => onRouteChange({ model_price_manage: manageRequested ? null : '1' }, 'push')}>{manageRequested ? copy.closePricingManager : copy.manageModelPricing}</button>}</div>
      {projection && <>
      <div className="adm-billing-authority"><div><strong>{copy.billingAuthority}</strong><span>{copy.independentBillingSnapshot} · {formatDate(projection.as_of)}</span></div><Status value={projection.current?.coverage_status} copy={copy} /></div>
      <Facts>
        <Fact label={copy.protocol}>{projection.protocol_code}</Fact>
        <Fact label={copy.safeFormula}><span className="adm-mono">{projection.formula_version_id}</span></Fact>
        <Fact label={copy.creditUnit}>{projection.credit_unit}</Fact>
        <Fact label={copy.priceVersion}>{projection.current?.price?.price_version_id || '—'}</Fact>
        <Fact label={copy.priceSpaceETag}>{projection.current?.price?.space_etag ?? '—'}</Fact>
        <Fact label={copy.effectiveFrom}>{formatDate(projection.current?.price?.effective_from)}</Fact>
        <Fact label={copy.nextPriceChange}>{projection.next_change ? formatDate(projection.next_change.effective_at) : copy.noneScheduled}</Fact>
        <Fact label={copy.nextCoverage}>{projection.next_change ? <Status value={projection.next_change.coverage_status} copy={copy} /> : copy.noneScheduled}</Fact>
      </Facts>
      <h3>{copy.currentPriceUnits}</h3>
      <ProjectionUnits point={projection.current} copy={copy} />
      {projection.current?.coverage_status !== 'AVAILABLE' && <p className="adm-source-lead">{copy.noResolvedPrice}</p>}
      {projection.next_change && <><h3>{copy.futurePriceUnits}</h3><ProjectionUnits point={projection.next_change} copy={copy} />{projection.next_change.coverage_status !== 'AVAILABLE' && <p className="adm-source-lead">{copy.futurePriceUnavailable}</p>}</>}
      </>}
    </DetailSource>
    {manageRequested && canManage && <section className="adm-card adm-model-pricing-editor" aria-labelledby="model-pricing-editor-heading"><h2 id="model-pricing-editor-heading">{copy.sharedPricingEditor}</h2><p className="adm-source-lead">{copy.sharedPricingEditorLead}</p><PricingLifecycleWorkspace key={`${detail.model.model_id}:${formula.formula_version_id}`} copy={pricingCopy} client={client} permissions={permissions} contextModel={detail.model} contextFormula={formula} onExit={() => onRouteChange({ model_price_manage: null }, 'push')} /></section>}
    {!pricingEditEnabled && permissions.includes('platform.pricing.manage') && <section className="adm-card adm-model-detail-panel"><p className="adm-source-lead">{copy.pricingEditorGateOff}</p></section>}
  </div>;
}

function lifecycleApprovalDraft(plan, reason, impact, idempotencyKey) {
  return {
    command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', idempotency_key: idempotencyKey,
    normalized_command: { model_id: plan.model_id, lifecycle_plan_id: plan.lifecycle_plan_id, lifecycle_plan_version: plan.lifecycle_plan_version, target_lifecycle: plan.target_lifecycle, effective_at: plan.effective_at, existing_session_cutoff_at: plan.existing_session_cutoff_at, replacement_model_id: plan.replacement_model_id },
    target_resource_type: 'MODEL', target_resource_id: plan.model_id, reason,
    redacted_evidence: { precheck_fingerprint: impact.precheck_fingerprint, token_observed_at: impact.token_observed_at, billing_observed_at: impact.billing_observed_at || null },
    expected_impact: { blockers: impact.blockers, dependencies: impact.evidence.dependencies },
    critical_resource_versions: impact.critical_resource_versions, financial_snapshot: impact.financial_snapshot,
  };
}

async function submitLifecycleApproval(client, draft, signal) {
  const created = await client.createApprovalDraft(draft, signal);
  if (!validApprovalRequest(created, draft, ['DRAFT'])) throw Object.assign(new Error('invalid lifecycle approval draft'), { code: 'INVALID_APPROVAL_RESPONSE' });
  try {
    const submitted = await client.submitApprovalRequest(created.approval_request_id, created.version, signal);
    if (!validApprovalRequest(submitted, draft, ['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'])) throw Object.assign(new Error('invalid lifecycle approval submit'), { code: 'INVALID_APPROVAL_RESPONSE' });
    return submitted;
  } catch (error) {
    throw Object.assign(error instanceof Error ? error : new Error('lifecycle approval submit failed'), { commandReference: created.approval_request_id });
  }
}

async function recoverLifecycleApproval(client, reference, draft, signal) {
  const value = await client.approvalRequest(reference, signal);
  const statuses = ['DRAFT', 'SUBMITTED', 'APPROVED', 'REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED', 'EXECUTING', 'EXECUTED', 'EXECUTION_FAILED'];
  if (!validApprovalRequest(value, draft, statuses)) throw Object.assign(new Error('invalid lifecycle approval recovery'), { code: 'INVALID_APPROVAL_RESPONSE', commandReference: reference });
  if (['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'].includes(value.status)) return value;
  if (value.status === 'DRAFT') throw Object.assign(new Error('approval submission outcome remains unknown'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: reference });
  throw Object.assign(new Error('approval reached terminal state'), { code: `APPROVAL_${value.status}`, commandReference: reference, finalResult: true });
}

function localDateTime(instant) {
  const value = new Date(instant);
  value.setMinutes(value.getMinutes() - value.getTimezoneOffset());
  return value.toISOString().slice(0, 16);
}

function LifecycleTab({ detail, client, copy, permissions, actionsEnabled, query, candidateQuery, onQueryChange, cursorHistory }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [planRefreshVersion, setPlanRefreshVersion] = useState(0);
  const [planAction, setPlanAction] = useState(null);
  const initialTarget = detail.model.lifecycle_status === 'DRAFT' ? 'PUBLISHED' : detail.model.lifecycle_status === 'PUBLISHED' ? 'DEPRECATED' : 'REMOVED';
  const [form, setForm] = useState(() => ({ target_lifecycle: initialTarget, effective_at: localDateTime(Date.now() + 10 * 60 * 1000), existing_session_cutoff_at: '', replacement_model_id: '', reason: '' }));
	const [candidateDraft, setCandidateDraft] = useState(candidateQuery.q || '');
	const [selectedCandidate, setSelectedCandidate] = useState(null);
  const [formState, setFormState] = useState({ busy: false, error: null });
  const createKey = React.useRef({ signature: '', key: '' });
	const createInflight = React.useRef(false);
  const model = detail.model;
	const canApprovalPlan = actionsEnabled && permissions.includes('platform.models.lifecycle.request') && (model.lifecycle_status === 'PUBLISHED' || model.lifecycle_status === 'DEPRECATED');
	const canPublishPlan = actionsEnabled && permissions.includes('platform.models.manage') && model.lifecycle_status === 'DRAFT';
	const canPlan = canApprovalPlan || canPublishPlan;
  const target = model.lifecycle_status === 'DRAFT' ? 'PUBLISHED' : model.lifecycle_status === 'PUBLISHED' ? 'DEPRECATED' : model.lifecycle_status === 'DEPRECATED' ? 'REMOVED' : '';
  const impactQuery = useMemo(() => ({ model_id: model.model_id, target }), [model.model_id, target]);
  const load = useMemo(() => async (filters, signal) => asCollection(await client.modelLifecycleImpact(filters.model_id, filters.target, '', signal)), [client]);
  const state = useAdminSource({ enabled: Boolean(target) && target !== 'PUBLISHED', load, query: impactQuery, refreshVersion });
  const planLoad = useMemo(() => (filters, signal) => client.modelLifecyclePlans(model.model_id, filters, signal), [client, model.model_id]);
  const plans = useAdminSource({ enabled: true, load: planLoad, query, refreshVersion: planRefreshVersion });
  // The write CAS must not depend on the operator's visible list filters.
  // The newest plan is authoritative because rescheduling always freezes the
  // previous active version before inserting the next monotonically versioned row.
  const activePlans = useAdminSource({ enabled: canPlan, load: planLoad, query: { limit: 1 }, refreshVersion: planRefreshVersion });
  const candidateLoad = useMemo(() => (filters, signal) => client.catalogModels({ ...filters, lifecycle_status: 'PUBLISHED' }, signal), [client]);
  const [candidateRefreshVersion, setCandidateRefreshVersion] = useState(0);
  const candidates = useAdminSource({ enabled: canPlan && form.target_lifecycle === 'REMOVED', load: candidateLoad, query: candidateQuery, refreshVersion: candidateRefreshVersion });
  useEffect(() => { if (!canPlan) { setPlanAction(null); setFormState({ busy: false, error: null }); } }, [canPlan]);
	useEffect(() => setCandidateDraft(candidateQuery.q || ''), [candidateQuery.q]);
  const impact = state.items[0];
	const retryPlans = () => {
		if (plans.error?.status === 410) { delete cursorHistory.current.modelLifecyclePlans; onQueryChange('modelLifecyclePlans', { ...query, cursor: '' }, 'replace'); }
		else setPlanRefreshVersion((value) => value + 1);
	};
	const retryCandidates = () => {
		if (candidates.error?.status === 410) { delete cursorHistory.current.modelLifecycleCandidates; onQueryChange('modelLifecycleCandidates', { ...candidateQuery, cursor: '' }, 'replace'); }
		else setCandidateRefreshVersion((value) => value + 1);
	};
	const createPlan = async (event) => {
		event.preventDefault();
		if (!canPlan || formState.busy || createInflight.current) return;
		const latestPlan = activePlans.items[0];
		const activePlan = activePlans.items.find((item) => ['PENDING_APPROVAL', 'SCHEDULED'].includes(item.status));
		const effectiveAt = new Date(form.effective_at).toISOString();
		if (form.target_lifecycle === 'PUBLISHED') {
			setPlanAction({ type: 'SCHEDULE_PUBLISH', command: { model_id: model.model_id, publish_mode: 'SCHEDULED', lifecycle_plan_id: secureRequestID(), lifecycle_plan_version: Number(latestPlan?.lifecycle_plan_version || 0) + 1, effective_at: effectiveAt }, reason: form.reason.trim() });
			return;
		}
		const input = { expected_catalog_version: model.catalog_version, expected_plan_version: Number(activePlan?.lifecycle_plan_version || 0), target_lifecycle: form.target_lifecycle, effective_at: effectiveAt, existing_session_cutoff_at: form.target_lifecycle === 'REMOVED' ? effectiveAt : form.target_lifecycle === 'DEPRECATED' && form.existing_session_cutoff_at ? new Date(form.existing_session_cutoff_at).toISOString() : null, replacement_model_id: form.target_lifecycle === 'REMOVED' ? form.replacement_model_id || null : null, force_cancel_inflight: false, reason: form.reason.trim() };
		const signature = JSON.stringify(input);
		if (createKey.current.signature !== signature) createKey.current = { signature, key: `model-lifecycle-plan-${secureRequestID()}` };
		createInflight.current = true;
		setFormState({ busy: true, error: null });
		try {
			const plan = await client.createModelLifecyclePlan(model.model_id, input, createKey.current.key);
			createKey.current = { signature: '', key: '' };
			setFormState({ busy: false, error: null }); setPlanAction({ type: 'APPROVE', plan }); setPlanRefreshVersion((value) => value + 1);
		} catch (error) { setFormState({ busy: false, error: { code: error.code || 'LIFECYCLE_PLAN_UNAVAILABLE', requestID: error.requestID || '' } }); }
		finally { createInflight.current = false; }
	};
  return <div className="adm-model-detail-stack">
    <section className="adm-card adm-model-detail-panel" aria-labelledby="model-lifecycle-heading"><h2 id="model-lifecycle-heading">{copy.detailLifecycle}</h2><p className="adm-source-lead">{copy.detailLifecycleLead}</p><Facts>
      <Fact label={copy.lifecycle}><Status value={model.lifecycle_status} copy={copy} /></Fact>
      <Fact label={copy.deprecationEffectiveAt}>{formatDate(model.deprecation_effective_at)}</Fact>
      <Fact label={copy.newSessionCutoff}>{formatDate(model.new_session_cutoff_at)}</Fact>
      <Fact label={copy.continuationCutoff}>{formatDate(model.continuation_cutoff_at)}</Fact>
      <Fact label={copy.replacementModel}>{model.replacement_model_id ? <span className="adm-mono">{model.replacement_model_id}</span> : '—'}</Fact>
    </Facts></section>
    {target && <DetailSource title={copy.dependencySnapshot} titleID="model-dependencies-heading" state={state} copy={copy} onRetry={() => setRefreshVersion((value) => value + 1)}>
      {impact && <><p className="adm-source-lead">{copy.dependencySnapshotLead}</p>
        <Facts>
          {Object.keys(copy.dependencyLabels).map((key) => <Fact key={key} label={copy.dependencyLabels[key]}>{Number(impact.evidence.dependencies[key]).toLocaleString()}</Fact>)}
          <Fact label={copy.tokenObservedAt}>{formatDate(impact.token_observed_at)}</Fact>
          <Fact label={copy.billingObservedAt}>{formatDate(impact.billing_observed_at)}</Fact>
        </Facts>
        {impact.price_coverage && <section className="adm-model-detail-subsection" aria-labelledby="model-price-coverage-heading"><h3 id="model-price-coverage-heading">{copy.priceCoverage}</h3><Facts>
          <Fact label={copy.modelsChecked}>{impact.price_coverage.models_checked}</Fact>
          <Fact label={copy.modelsCovered}>{impact.price_coverage.models_covered}</Fact>
          <Fact label={copy.fullyCovered}><Status value={impact.price_coverage.fully_covered ? 'COVERED' : 'MISSING'} copy={copy} /></Fact>
          <Fact label={copy.financialFactRevision}>{impact.price_coverage.financial_fact_revision}</Fact>
          <Fact label={copy.priceSetFingerprint}><span className="adm-mono">{impact.price_coverage.price_set_fingerprint}</span></Fact>
          <Fact label={copy.billingObservedAt}>{formatDate(impact.price_coverage.billing_observed_at)}</Fact>
        </Facts>
        {impact.price_coverage.versions?.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.protocol}</th><th>{copy.creditUnit}</th><th>{copy.coverageStatus}</th><th>{copy.priceVersion}</th><th>{copy.effectiveFrom}</th></tr></thead><tbody>{impact.price_coverage.versions.map((version) => <tr key={`${version.model_id}-${version.protocol_code}-${version.credit_unit}`}><td data-label={copy.protocol}>{version.protocol_code}</td><td data-label={copy.creditUnit}>{version.credit_unit}</td><td data-label={copy.coverageStatus}><Status value={version.covered ? 'COVERED' : 'MISSING'} copy={copy} /></td><td data-label={copy.priceVersion}>{version.price_version ?? '—'}</td><td data-label={copy.effectiveFrom}>{formatDate(version.effective_from)}</td></tr>)}</tbody></table></div>}
        </section>}
        {impact.blockers?.length > 0 && <div className="adm-inline-message warning" role="status">{impact.blockers.map((item) => copy.enumLabels[item] || item).join(' · ')}</div>}
      </>}
    </DetailSource>}
		<section className="adm-card adm-source-card" aria-labelledby="model-lifecycle-plans-heading">
			<SourceHeader title={copy.lifecyclePlans} titleID="model-lifecycle-plans-heading" state={plans} copy={copy} onRetry={retryPlans} />
			<p className="adm-source-lead">{copy.lifecyclePlansLead}</p>
			<FilterForm source="modelLifecyclePlans" query={query} copy={copy} onQueryChange={onQueryChange} fields={[{ key: 'status', label: copy.status, options: ['PENDING_APPROVAL','SCHEDULED','EXECUTING','APPLIED','CANCELLED','EXPIRED','INVALIDATED','FAILED','UNKNOWN'] }, { key: 'target_lifecycle', label: copy.targetLifecycle, options: ['PUBLISHED','DEPRECATED','REMOVED'] }]} />
			<SourceFeedback state={plans} copy={copy} onRetry={retryPlans} />
			{plans.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.planVersion}</th><th>{copy.targetLifecycle}</th><th>{copy.actionEffectiveAt}</th><th>{copy.existingSessionCutoff}</th><th>{copy.replacementModel}</th><th>{copy.status}</th><th>{copy.actions}</th></tr></thead><tbody>{plans.items.map((plan) => <tr key={plan.lifecycle_plan_id}><td data-label={copy.planVersion}>v{plan.lifecycle_plan_version}</td><td data-label={copy.targetLifecycle}><Status value={plan.target_lifecycle} copy={copy} /></td><td data-label={copy.actionEffectiveAt}>{formatDate(plan.effective_at)}</td><td data-label={copy.existingSessionCutoff}>{formatDate(plan.existing_session_cutoff_at)}</td><td data-label={copy.replacementModel}>{plan.replacement_model_id ? <span className="adm-mono">{plan.replacement_model_id}</span> : '—'}</td><td data-label={copy.status}><Status value={plan.status} copy={copy} /></td><td data-label={copy.actions}><div className="adm-row-actions">{canApprovalPlan && plan.status === 'PENDING_APPROVAL' && <button type="button" onClick={() => setPlanAction({ type: 'APPROVE', plan })}>{copy.requestApproval}</button>}{((plan.target_lifecycle === 'PUBLISHED' && canPublishPlan) || (plan.target_lifecycle !== 'PUBLISHED' && canApprovalPlan)) && ['PENDING_APPROVAL','SCHEDULED'].includes(plan.status) && <button type="button" onClick={() => setPlanAction({ type: 'CANCEL', plan })}>{copy.cancelPlan}</button>}</div></td></tr>)}</tbody></table></div>}
			<Pagination source="modelLifecyclePlans" query={query} state={plans} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
		</section>
		{canPlan && form.target_lifecycle === 'REMOVED' && <section className="adm-card adm-source-card" aria-labelledby="model-lifecycle-candidates-heading"><SourceHeader title={copy.replacementCandidates} titleID="model-lifecycle-candidates-heading" state={candidates} copy={copy} onRetry={retryCandidates} /><p className="adm-source-lead">{copy.replacementCandidatesLead}</p><div className="adm-model-token-filters"><label><span>{copy.search}</span><input type="search" value={candidateDraft} placeholder={copy.modelSearch} onChange={(event) => setCandidateDraft(event.target.value)} /></label><button type="button" onClick={() => onQueryChange('modelLifecycleCandidates', { q: candidateDraft, cursor: '' })}>{copy.search}</button><button type="button" className="secondary" onClick={() => { setCandidateDraft(''); onQueryChange('modelLifecycleCandidates', { q: '', cursor: '' }); }}>{copy.clear}</button></div><SourceFeedback state={candidates} copy={copy} onRetry={retryCandidates} /><Pagination source="modelLifecycleCandidates" query={candidateQuery} state={candidates} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} /></section>}
		{canPlan && <form className="adm-card adm-model-detail-panel adm-lifecycle-plan-form" onSubmit={createPlan} aria-labelledby="model-lifecycle-plan-create-heading"><h2 id="model-lifecycle-plan-create-heading">{copy.createLifecyclePlan}</h2><p className="adm-source-lead">{copy.lifecyclePlanTimeHint}</p>
			{activePlans.error && <SourceFeedback state={activePlans} copy={copy} onRetry={() => setPlanRefreshVersion((value) => value + 1)} />}
			<div className="adm-lifecycle-plan-fields"><label>{copy.targetLifecycle}<select value={form.target_lifecycle} onChange={(event) => setForm((value) => ({ ...value, target_lifecycle: event.target.value, existing_session_cutoff_at: '', replacement_model_id: '' }))}>{model.lifecycle_status === 'DRAFT' && <option value="PUBLISHED">{copy.enumLabels.PUBLISHED}</option>}{model.lifecycle_status === 'PUBLISHED' && <><option value="DEPRECATED">{copy.enumLabels.DEPRECATED}</option><option value="REMOVED">{copy.enumLabels.REMOVED}</option></>}{model.lifecycle_status === 'DEPRECATED' && <option value="REMOVED">{copy.enumLabels.REMOVED}</option>}</select></label><label>{copy.actionEffectiveAt}<input type="datetime-local" required value={form.effective_at} onChange={(event) => setForm((value) => ({ ...value, effective_at: event.target.value }))} /></label>{form.target_lifecycle === 'DEPRECATED' && <label>{copy.existingSessionCutoff}<input type="datetime-local" min={form.effective_at} value={form.existing_session_cutoff_at} onChange={(event) => setForm((value) => ({ ...value, existing_session_cutoff_at: event.target.value }))} /></label>}{form.target_lifecycle === 'REMOVED' && <label>{copy.replacementModel}<select required value={form.replacement_model_id} onChange={(event) => { const item = candidates.items.find((candidate) => candidate.model_id === event.target.value); if (item) setSelectedCandidate(item); setForm((value) => ({ ...value, replacement_model_id: event.target.value })); }}><option value="">{copy.chooseReplacement}</option>{selectedCandidate && !candidates.items.some((item) => item.model_id === selectedCandidate.model_id) && <option value={selectedCandidate.model_id}>{selectedCandidate.display_name}</option>}{candidates.items.filter((item) => item.model_id !== model.model_id).map((item) => <option key={item.model_id} value={item.model_id}>{item.display_name}</option>)}</select></label>}</div>
			{form.target_lifecycle === 'REMOVED' && <p className="adm-source-lead">{copy.removedCutoffLocked}</p>}<label>{copy.reason}<textarea minLength="10" maxLength="2000" required rows="3" value={form.reason} onChange={(event) => setForm((value) => ({ ...value, reason: event.target.value }))} /></label>{formState.error && <div className="adm-inline-message error" role="alert">{copy.commandErrors?.[formState.error.code] || copy.lifecyclePlanFailed}{formState.error.requestID && <small>{copy.requestID}: {formState.error.requestID}</small>}</div>}<button type="submit" disabled={formState.busy || (form.target_lifecycle === 'REMOVED' && (candidates.loading || Boolean(candidates.error))) || activePlans.loading || Boolean(activePlans.error)}>{formState.busy ? copy.submitting : copy.prepareLifecyclePlan}</button>
		</form>}
		{planAction?.type === 'SCHEDULE_PUBLISH' && canPublishPlan && <AdminHighRiskActionDialog action={{ key: `schedule-model-publish-${planAction.command.lifecycle_plan_id}`, impactAction: 'TOKEN_MODEL_PUBLISH', title: copy.publishModel, lead: copy.lifecyclePlanTimeHint }} copy={copy} target={{ label: model.display_name, version: `${copy.planVersion}: ${planAction.command.lifecycle_plan_version}`, effectiveAt: formatDate(planAction.command.effective_at) }} policy="AUDIT_ONLY" recentAuth initialReason={planAction.reason} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_MODEL_PUBLISH', target_resource_type: 'MODEL', target_resource_id: model.model_id, command: planAction.command }, signal), { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: model.model_id })} onClose={() => setPlanAction(null)} execute={async (payload, key, signal) => requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: model.model_id, reason: payload.reason, impact: payload.precheck, command: planAction.command }), key, signal), { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: model.model_id })} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: model.model_id })} onCompleted={() => setPlanRefreshVersion((value) => value + 1)} />}
		{planAction?.type === 'APPROVE' && canApprovalPlan && <AdminHighRiskActionDialog action={{ key: `approve-lifecycle-plan-${planAction.plan.lifecycle_plan_id}`, impactAction: `MODEL_LIFECYCLE_${planAction.plan.target_lifecycle}`, title: copy.requestLifecycleApproval, lead: copy.lifecycleApprovalLead }} copy={copy} target={{ label: model.display_name, version: `${copy.planVersion}: ${planAction.plan.lifecycle_plan_version}`, effectiveAt: formatDate(planAction.plan.effective_at) }} policy="APPROVAL_REQUIRED" recentAuth dangerous={planAction.plan.target_lifecycle === 'REMOVED'} initialReason={planAction.plan.reason} impactLoader={async (signal) => requireTokenApprovalImpact(await client.modelLifecycleImpact(model.model_id, planAction.plan.target_lifecycle, planAction.plan.replacement_model_id || '', signal, planAction.plan.lifecycle_plan_id), { action: `MODEL_LIFECYCLE_${planAction.plan.target_lifecycle}`, commandType: 'TOKEN_MODEL_LIFECYCLE_CHANGE', targetResourceID: model.model_id, targetStatus: planAction.plan.target_lifecycle })} onClose={() => setPlanAction(null)} execute={(payload, key, signal) => submitLifecycleApproval(client, lifecycleApprovalDraft(planAction.plan, payload.reason, payload.precheck, key), signal)} recover={(reference, signal, payload) => recoverLifecycleApproval(client, reference, lifecycleApprovalDraft(planAction.plan, payload.reason, payload.precheck, ''), signal)} onCompleted={() => setPlanRefreshVersion((value) => value + 1)} />}
		{planAction?.type === 'CANCEL' && canPlan && <AdminHighRiskActionDialog action={{ key: `cancel-lifecycle-plan-${planAction.plan.lifecycle_plan_id}`, impactAction: `MODEL_LIFECYCLE_${planAction.plan.target_lifecycle}`, title: copy.cancelLifecyclePlan, lead: copy.cancelLifecyclePlanLead }} copy={copy} target={{ label: model.display_name, version: `${copy.planVersion}: ${planAction.plan.lifecycle_plan_version}`, effectiveAt: formatDate(planAction.plan.effective_at) }} policy="AUDIT_ONLY" initialReason={copy.cancelLifecyclePlanDefaultReason} precheckEnabled={false} impactRequired={false} onClose={() => setPlanAction(null)} execute={async (payload, key, signal) => { try { return await client.cancelModelLifecyclePlan(model.model_id, planAction.plan.lifecycle_plan_id, { expected_plan_version: planAction.plan.lifecycle_plan_version, reason: payload.reason }, key, signal); } catch (error) { throw Object.assign(error, { commandReference: planAction.plan.lifecycle_plan_id }); } }} recover={async (_, signal) => { const current = await client.modelLifecyclePlan(model.model_id, planAction.plan.lifecycle_plan_id, signal); if (current.status !== 'CANCELLED') throw Object.assign(new Error('cancellation outcome remains unknown'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: planAction.plan.lifecycle_plan_id }); return current; }} onCompleted={() => setPlanRefreshVersion((value) => value + 1)} />}
  </div>;
}

function AuditTab({ modelID, client, copy, query, onQueryChange, cursorHistory }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.tokenResourceAudit({ ...filters, resource_type: 'MODEL', resource_id: modelID }, signal), [client, modelID]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.modelAudit;
      onQueryChange('modelAudit', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="model-audit-heading">
    <SourceHeader title={copy.detailAudit} titleID="model-audit-heading" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="modelAudit" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'action', label: copy.auditAction }, { key: 'actor_id', label: copy.auditActor, placeholder: copy.identifierPlaceholder },
      { key: 'occurred_from', label: copy.createdFrom, type: 'datetime-local' }, { key: 'occurred_to', label: copy.createdTo, type: 'datetime-local' },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.auditAction}</th><th>{copy.auditChange}</th><th>{copy.auditActor}</th><th>{copy.occurredAt}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.audit_id}><td data-label={copy.auditAction}>{item.action}</td><td data-label={copy.auditChange}>{[item.summary?.before_status, item.summary?.after_status].filter(Boolean).join(' → ') || `${copy.catalogVersion}: ${item.summary?.catalog_version ?? '—'}`}</td><td data-label={copy.auditActor}><span className="adm-mono">{item.actor_id}</span></td><td data-label={copy.occurredAt}>{formatDate(item.occurred_at)}</td></tr>)}</tbody></table></div>}
    <Pagination source="modelAudit" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
  </section>;
}

export function AdminModelDetailWorkspace({ modelID, client, copy, pricingCopy, permissions, actionsEnabled = false, pricingEditEnabled = false, routeVersion, onRouteChange, onNavigate, onQueryChange, cursorHistory, onTabsKeyDown }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [copied, setCopied] = useState(false);
  const requestedTab = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('model_detail_tab') || 'overview', [routeVersion]);
  const managePricing = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('model_price_manage') === '1', [routeVersion]);
  const priceReturn = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('model_price_return') === 'products', [routeVersion]);
  const tabs = useMemo(() => MODEL_DETAIL_TABS
    .filter((tab) => tab !== 'operations' || permissions.includes('platform.tokens.read'))
    .filter((tab) => tab !== 'pricing' || permissions.includes('platform.pricing.read'))
    .filter((tab) => tab !== 'audit' || permissions.includes('platform.audit.read')), [permissions]);
  const tab = tabs.includes(requestedTab) ? requestedTab : tabs[0];
  const load = useMemo(() => async (_, signal) => {
    const detail = await client.catalogModel(modelID, signal);
    if (detail?.model?.model_id !== modelID) throw Object.assign(new Error('model detail mismatch'), { code: 'INVALID_MODEL_DETAIL' });
    return asCollection(detail, detail.as_of);
  }, [client, modelID]);
  const state = useAdminSource({ enabled: true, load, query: { model_id: modelID }, refreshVersion });
  const detail = state.items[0];
  const provisioningQuery = useMemo(() => readAdminModelTokenQuery('modelProvisionings'), [routeVersion]);
  const auditQuery = useMemo(() => readAdminModelTokenQuery('modelAudit'), [routeVersion]);
  const historyQuery = useMemo(() => readAdminModelTokenQuery('modelHistory'), [routeVersion]);
	const lifecyclePlanQuery = useMemo(() => readAdminModelTokenQuery('modelLifecyclePlans'), [routeVersion]);
	const lifecycleCandidateQuery = useMemo(() => readAdminModelTokenQuery('modelLifecycleCandidates'), [routeVersion]);
  useEffect(() => setCopied(false), [modelID]);
  const back = () => {
    if (priceReturn) { onNavigate?.('products', { finance_tab: 'prices' }); return; }
    onRouteChange({ model_id: null, model_detail_tab: null, model_price_manage: null, model_price_return: null, ...adminModelTokenQueryUpdates('modelProvisionings', {}), ...adminModelTokenQueryUpdates('modelAudit', {}), ...adminModelTokenQueryUpdates('modelHistory', {}), ...adminModelTokenQueryUpdates('modelLifecyclePlans', {}), ...adminModelTokenQueryUpdates('modelLifecycleCandidates', {}) }, 'push');
  };
  return <article className="adm-model-detail">
    <header className="adm-card adm-model-detail-header">
      <button type="button" className="adm-back" onClick={back}>← {priceReturn ? copy.backToPricingGovernance : copy.backToModelDirectory}</button>
      <div><p>{copy.modelDetail}</p><h2>{detail?.model?.display_name || copy.loading}</h2><div className="adm-model-id"><span className="adm-mono">{modelID}</span><button type="button" onClick={async () => { try { await globalThis.navigator?.clipboard?.writeText(modelID); setCopied(true); } catch { setCopied(false); } }}>{copied ? copy.copied : copy.copyID}</button></div></div>
      <div><small>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</small><button type="button" disabled={state.loading} onClick={() => setRefreshVersion((value) => value + 1)}>{copy.refreshSource}</button></div>
    </header>
    <SourceFeedback state={state} copy={copy} onRetry={() => setRefreshVersion((value) => value + 1)} />
    {detail && <>
      <div className="adm-tabs adm-model-detail-tabs" role="tablist" aria-label={copy.modelDetailViews} onKeyDown={onTabsKeyDown}>{tabs.map((item) => <button type="button" role="tab" id={`model-detail-tab-${item}`} aria-controls={`model-detail-panel-${item}`} aria-selected={tab === item} tabIndex={tab === item ? 0 : -1} className={tab === item ? 'active' : ''} key={item} onClick={() => onRouteChange({ model_detail_tab: item }, 'push')}>{copy.modelDetailTabs[item]}</button>)}</div>
      <div role="tabpanel" id={`model-detail-panel-${tab}`} aria-labelledby={`model-detail-tab-${tab}`}>
        {tab === 'overview' && <OverviewTab detail={detail} copy={copy} />}
        {tab === 'capabilities' && <CapabilitiesTab detail={detail} client={client} copy={copy} historyQuery={historyQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
        {tab === 'operations' && <OperationsTab modelID={modelID} client={client} copy={copy} query={provisioningQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
        {tab === 'pricing' && <PricingTab detail={detail} client={client} copy={copy} pricingCopy={pricingCopy} permissions={permissions} pricingEditEnabled={pricingEditEnabled} manageRequested={managePricing} onRouteChange={onRouteChange} />}
        {tab === 'lifecycle' && <LifecycleTab detail={detail} client={client} copy={copy} permissions={permissions} actionsEnabled={actionsEnabled} query={lifecyclePlanQuery} candidateQuery={lifecycleCandidateQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
        {tab === 'audit' && <AuditTab modelID={modelID} client={client} copy={copy} query={auditQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
      </div>
    </>}
  </article>;
}
