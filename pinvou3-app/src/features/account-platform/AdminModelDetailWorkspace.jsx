import React, { useEffect, useMemo, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';

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

function PricingTab({ detail, client, copy }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const formula = detail.formula;
  const query = useMemo(() => ({ model_id: detail.model.model_id, protocol_code: detail.protocol.protocol_code, formula_version_id: formula?.formula_version_id || '', credit_unit: 'MICROCREDIT' }), [detail, formula]);
  const load = useMemo(() => async (filters, signal) => asCollection(await client.modelPriceProjection(filters.model_id, filters.protocol_code, filters.formula_version_id, filters.credit_unit, signal)), [client]);
  const state = useAdminSource({ enabled: Boolean(formula), load, query, refreshVersion });
  if (!formula) return <section className="adm-card adm-model-detail-panel"><h2>{copy.detailPricing}</h2><p className="adm-source-lead">{copy.priceFormulaMissing}</p></section>;
  const projection = state.items[0];
  return <DetailSource title={copy.detailPricing} titleID="model-pricing-heading" state={state} copy={copy} onRetry={() => setRefreshVersion((value) => value + 1)}>
    <p className="adm-source-lead">{copy.detailPricingLead}</p>
    {projection && <>
      <div className="adm-billing-authority"><div><strong>{copy.billingAuthority}</strong><span>{copy.independentBillingSnapshot} · {formatDate(projection.as_of)}</span></div><Status value={projection.current?.coverage_status} copy={copy} /></div>
      <Facts>
        <Fact label={copy.protocol}>{projection.protocol_code}</Fact>
        <Fact label={copy.safeFormula}><span className="adm-mono">{projection.formula_version_id}</span></Fact>
        <Fact label={copy.creditUnit}>{projection.credit_unit}</Fact>
        <Fact label={copy.priceVersion}>{projection.current?.price?.price_version_id || '—'}</Fact>
        <Fact label={copy.effectiveFrom}>{formatDate(projection.current?.price?.effective_from)}</Fact>
        <Fact label={copy.nextPriceChange}>{projection.next_change ? formatDate(projection.next_change.effective_at) : copy.noneScheduled}</Fact>
      </Facts>
      {projection.current?.price?.units?.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.unitCode}</th><th>{copy.resolvedPrice}</th><th>{copy.priceSource}</th><th>{copy.roundingRule}</th></tr></thead><tbody>{projection.current.price.units.map((unit) => <tr key={unit.unit_definition_id}><td data-label={copy.unitCode}>{unit.unit_code}</td><td data-label={copy.resolvedPrice}>{unit.resolved_price}</td><td data-label={copy.priceSource}>{unit.price_source_scope}</td><td data-label={copy.roundingRule}>{unit.rounding_rule}</td></tr>)}</tbody></table></div>}
    </>}
  </DetailSource>;
}

function LifecycleTab({ detail, client, copy }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const model = detail.model;
  const target = model.lifecycle_status === 'PUBLISHED' ? 'DEPRECATED' : model.lifecycle_status === 'DEPRECATED' ? 'REMOVED' : '';
  const query = useMemo(() => ({ model_id: model.model_id, target }), [model.model_id, target]);
  const load = useMemo(() => async (filters, signal) => asCollection(await client.modelLifecycleImpact(filters.model_id, filters.target, '', signal)), [client]);
  const state = useAdminSource({ enabled: Boolean(target), load, query, refreshVersion });
  const impact = state.items[0];
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

export function AdminModelDetailWorkspace({ modelID, client, copy, permissions, routeVersion, onRouteChange, onQueryChange, cursorHistory, onTabsKeyDown }) {
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [copied, setCopied] = useState(false);
  const requestedTab = useMemo(() => new URLSearchParams(globalThis.location?.search || '').get('model_detail_tab') || 'overview', [routeVersion]);
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
  useEffect(() => setCopied(false), [modelID]);
  const back = () => onRouteChange({ model_id: null, model_detail_tab: null, ...adminModelTokenQueryUpdates('modelProvisionings', {}), ...adminModelTokenQueryUpdates('modelAudit', {}), ...adminModelTokenQueryUpdates('modelHistory', {}) }, 'push');
  return <article className="adm-model-detail">
    <header className="adm-card adm-model-detail-header">
      <button type="button" className="adm-back" onClick={back}>← {copy.backToModelDirectory}</button>
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
        {tab === 'pricing' && <PricingTab detail={detail} client={client} copy={copy} />}
        {tab === 'lifecycle' && <LifecycleTab detail={detail} client={client} copy={copy} />}
        {tab === 'audit' && <AuditTab modelID={modelID} client={client} copy={copy} query={auditQuery} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
      </div>
    </>}
  </article>;
}
