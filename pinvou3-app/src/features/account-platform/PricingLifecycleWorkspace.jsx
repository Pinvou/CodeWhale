import { integerMoney } from './finance-contract.js';
import { pricingUnknownStorageKey, replayPricingMutation } from './pricing-mutation.js';
import { secureRequestID } from './request-id.js';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { buildPriceDraftInput, matchingContinuationDrafts, newModelPriceAllowed, priceDraftForm, priceSpaceQuery, priceTransitionModes, pricingLifecycleMutationLock, publishedPricingFormulas } from './pricing-lifecycle.js';

const shortID = (value) => {
  const text = String(value || '');
  return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—';
};
const formatDate = (value) => value ? new Date(value).toLocaleString() : '—';
const mutationKey = (prefix) => `${prefix}:${secureRequestID()}`;
const outcomeUnknown = (error) => !Number.isFinite(error?.status) || error.status >= 500;
const stableUUID = (value) => /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(String(value || ''));
const readRetainedUnknown = (unknownStorageKey) => {
  try {
    const value = JSON.parse(globalThis.sessionStorage?.getItem(unknownStorageKey) || 'null');
    if (value?.phase === 'PREPARING') { globalThis.sessionStorage?.removeItem(unknownStorageKey); return null; }
    return ['DRAFT_CREATE', 'DRAFT_UPDATE', 'UNIT_CREATE', 'UNIT_DEACTIVATE', 'PRICE_PUBLISH', 'PRICE_TRANSITION'].includes(value?.kind) ? value : null;
  } catch { return null; }
};
const storeUnknown = (unknownStorageKey, value) => {
  try { if (value) globalThis.sessionStorage?.setItem(unknownStorageKey, JSON.stringify(value)); else globalThis.sessionStorage?.removeItem(unknownStorageKey); } catch { /* storage is optional */ }
  return value;
};

function PricingStatus({ value }) {
  const normalized = String(value || '—');
  const tone = ['ACTIVE', 'PUBLISHED'].includes(normalized) ? 'ok' : normalized === 'DRAFT' ? 'warn' : 'neutral';
  return <span className={`adm-status ${tone}`} aria-label={normalized}>{normalized}</span>;
}

function fieldError(labels, value) {
  if (!value) return '';
  if (value === 'range') return labels.dateRangeError;
  if (value === 'nonnegative_integer') return labels.integerError;
  return labels.required;
}

export function PricingLifecycleWorkspace({ copy, client, permissions, owner, contextModel = null, contextFormula = null, onExit = null, onOpenModel = null }) {
  const unknownStorageKey = pricingUnknownStorageKey(owner, client.financeSessionEpoch);
  const retainUnknown = (value) => storeUnknown(unknownStorageKey, value);
  const labels = copy.billing.pricingLifecycle;
  const contextual = Boolean(contextModel?.model_id && contextFormula?.formula_version_id);
  const canManage = new Set(permissions || []).has('platform.pricing.manage');
  const canReadModels = new Set(permissions || []).has('platform.models.read');
  const [prices, setPrices] = useState([]);
  const [units, setUnits] = useState([]);
  const [protocols, setProtocols] = useState(() => contextual ? { protocols: [], formulas: [contextFormula] } : { protocols: [], formulas: [] });
  const [models, setModels] = useState(() => contextual ? [contextModel] : []);
  const [loading, setLoading] = useState({});
  const [errors, setErrors] = useState({});
  const [updatedAt, setUpdatedAt] = useState({});
  const [notice, setNotice] = useState('');
  const [editor, setEditor] = useState(null);
  const [editorErrors, setEditorErrors] = useState({});
  const [unitForm, setUnitForm] = useState(null);
  const [preview, setPreview] = useState(null);
  const [pendingAction, setPendingAction] = useState(null);
  const [unknownMutation, setUnknownMutationState] = useState(() => readRetainedUnknown(unknownStorageKey));
  const setUnknownMutation = (value) => { retainUnknown(value); if (mounted.current) setUnknownMutationState(value); };
  const generations = useRef({ prices: 0, units: 0, formulas: 0, models: 0 });
  const mounted = useRef(true);
  const preflights = useRef(new Set());
  const sourceControllers = useRef(new Map());
  const transientControllers = useRef(new Map());
  const transientGenerations = useRef(new Map());
  const abortTransientReads = useCallback(() => {
    for (const controller of transientControllers.current.values()) controller.abort();
    transientControllers.current.clear();
  }, []);
  const formulas = useMemo(() => publishedPricingFormulas(protocols), [protocols]);
  const latestFacts = useRef({ canManage, canReadModels, models, formulas });
  latestFacts.current = { canManage, canReadModels, models, formulas };
  useEffect(() => () => {
    mounted.current = false;
    for (const controller of preflights.current) controller.abort();
    preflights.current.clear();
    for (const controller of sourceControllers.current.values()) controller.abort();
    sourceControllers.current.clear();
    abortTransientReads();
  }, [abortTransientReads]);
  const unitByID = useMemo(() => new Map(units.map((unit) => [unit.unit_definition_id, unit])), [units]);
  const editorFormula = formulas.find((item) => item.formula_version_id === editor?.formula_version_id);
  const compatibleModels = models.filter((item) => item.protocol_code === editorFormula?.protocol_code);

  const loadResource = useCallback(async (key, request, apply, message) => {
    const generation = (generations.current[key] || 0) + 1;
    generations.current[key] = generation;
    sourceControllers.current.get(key)?.abort();
    const controller = new globalThis.AbortController();
    sourceControllers.current.set(key, controller);
    setLoading((current) => ({ ...current, [key]: true }));
    try {
      const payload = await request(controller.signal);
      if (generations.current[key] !== generation) throw Object.assign(new Error('STALE_RESOURCE_RESPONSE'), { code: 'STALE_RESOURCE_RESPONSE' });
      apply(payload);
      setUpdatedAt((current) => ({ ...current, [key]: new Date() }));
      setErrors((current) => { const next = { ...current }; delete next[key]; return next; });
      return payload;
    } catch (error) {
      if (generations.current[key] === generation && error?.name !== 'AbortError') setErrors((current) => ({ ...current, [key]: message }));
      throw error;
    } finally {
      if (generations.current[key] === generation) {
        sourceControllers.current.delete(key);
        setLoading((current) => ({ ...current, [key]: false }));
      }
    }
  }, []);
  const readTransient = useCallback(async (key, request) => {
    const generation = (transientGenerations.current.get(key) || 0) + 1;
    transientGenerations.current.set(key, generation);
    transientControllers.current.get(key)?.abort();
    const controller = new globalThis.AbortController();
    transientControllers.current.set(key, controller);
    try {
      const payload = await request(controller.signal);
      if (!mounted.current || transientGenerations.current.get(key) !== generation) {
        throw Object.assign(new Error('STALE_TRANSIENT_RESPONSE'), { name: 'AbortError', code: 'STALE_TRANSIENT_RESPONSE' });
      }
      return payload;
    } finally {
      if (transientGenerations.current.get(key) === generation) transientControllers.current.delete(key);
    }
  }, []);
  const loadPrices = useCallback(() => loadResource('prices', (signal) => contextual
    ? client.managedPriceSpace({ scope_type: 'MODEL', scope_id: contextModel.model_id, protocol_code: contextFormula.protocol_code, formula_version_id: contextFormula.formula_version_id, credit_unit: 'MICROCREDIT' }, signal)
    : client.managedPrices(signal), (payload) => setPrices(payload.items || []), labels.loadPricesFailed), [client, contextFormula, contextModel, contextual, labels.loadPricesFailed, loadResource]);
  const loadUnits = useCallback(() => loadResource('units', (signal) => client.meteringUnits(signal), (payload) => setUnits(payload.items || []), labels.loadUnitsFailed), [client, labels.loadUnitsFailed, loadResource]);
  const loadProtocols = useCallback(() => {
    if (contextual) {
      generations.current.formulas += 1;
      setProtocols({ protocols: [], formulas: [contextFormula] });
      setLoading((current) => ({ ...current, formulas: false }));
      setErrors((current) => { const next = { ...current }; delete next.formulas; return next; });
      return Promise.resolve({ protocols: [], formulas: [contextFormula] });
    }
    return loadResource('formulas', (signal) => client.pricingProtocols(signal), (payload) => setProtocols(payload || { protocols: [], formulas: [] }), labels.loadFormulasFailed);
  }, [client, contextFormula, contextual, labels.loadFormulasFailed, loadResource]);
  const loadModels = useCallback(() => {
    if (contextual) {
      generations.current.models += 1;
      setModels([contextModel]);
      setLoading((current) => ({ ...current, models: false }));
      setErrors((current) => { const next = { ...current }; delete next.models; return next; });
      return Promise.resolve({ items: [contextModel] });
    }
    if (!canReadModels) {
      generations.current.models += 1;
      setModels([]);
      setLoading((current) => ({ ...current, models: false }));
      setErrors((current) => { const next = { ...current }; delete next.models; return next; });
      return Promise.resolve({ items: [] });
    }
    return loadResource('models', (signal) => client.catalogModels({}, signal), (payload) => setModels((payload.items || []).filter((item) => item.lifecycle_status !== 'REMOVED')), labels.loadModelsFailed);
  }, [canReadModels, client, contextModel, contextual, labels.loadModelsFailed, loadResource]);

  useEffect(() => { void loadPrices().catch(() => {}); void loadUnits().catch(() => {}); void loadProtocols().catch(() => {}); void loadModels().catch(() => {}); }, [loadModels, loadPrices, loadProtocols, loadUnits]);
  useEffect(() => {
    if (!canManage) abortTransientReads();
    if (!canManage && !unknownMutation) {
      setEditor(null);
      setUnitForm(null);
      setPendingAction(null);
    }
  }, [abortTransientReads, canManage, unknownMutation]);
  useEffect(() => {
    setEditor((current) => current && !newModelPriceAllowed(current, canReadModels, models, formulas.find((item) => item.formula_version_id === current.formula_version_id)) ? null : current);
  }, [canReadModels, formulas, models]);

  const openCreate = () => {
    const form = priceDraftForm();
    const formula = formulas[0];
    const compatibleModels = models.filter((item) => item.protocol_code === formula?.protocol_code);
    form.scope_type = contextual ? 'MODEL' : compatibleModels.length ? 'MODEL' : 'GLOBAL';
    form.scope_id = contextual ? contextModel.model_id : compatibleModels[0]?.model_id || '';
    if (formula) {
      form.formula_version_id = formula.formula_version_id;
      form.protocol_code = formula.protocol_code;
      form.units = Object.fromEntries(Object.values(formula.required_unit_versions || {}).map((id) => [id, { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '' }]));
    }
    setEditorErrors({});
    setEditor(form);
  };

  const openEdit = (item) => {
    setEditorErrors({});
    setEditor(priceDraftForm(item));
  };

  const changeFormula = (formulaID) => {
    if (contextual) return;
    const formula = formulas.find((item) => item.formula_version_id === formulaID);
    const compatibleModels = models.filter((item) => item.protocol_code === formula?.protocol_code);
    setEditor((current) => ({
      ...current,
      formula_version_id: formulaID,
      protocol_code: formula?.protocol_code || '',
      scope_id: current.scope_type === 'MODEL' && compatibleModels.some((item) => item.model_id === current.scope_id) ? current.scope_id : (compatibleModels[0]?.model_id || ''),
      units: Object.fromEntries(Object.values(formula?.required_unit_versions || {}).map((id) => [id, current.units?.[id] || { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '' }])),
    }));
  };

  const submitDraft = async (event) => {
    event.preventDefault();
    if (!editor || loading.mutation || unknownMutation || !pricingLifecycleMutationLock.tryAcquire()) return;
    retainUnknown({ phase: 'PREPARING', kind: editor.price_version_id ? 'DRAFT_UPDATE' : 'DRAFT_CREATE' });
    if (!canManage) { setEditorErrors({ form: labels.permissionLost }); retainUnknown(null); pricingLifecycleMutationLock.release(); return; }
    setLoading((current) => ({ ...current, mutation: true }));
    const formula = formulas.find((item) => item.formula_version_id === editor.formula_version_id);
    if (!newModelPriceAllowed(editor, canReadModels, models, formula)) {
      setEditorErrors({ form: labels.modelSelectionInvalid });
      retainUnknown(null);
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
      return;
    }
    let currentETag = 1;
    let spaceBeforeIDs = [];
    const preflightController = new globalThis.AbortController();
    preflights.current.add(preflightController);
    if (!editor.price_version_id && formula) {
      try {
        const projection = await client.managedPriceSpace(priceSpaceQuery(editor, formula), preflightController.signal);
        currentETag = projection.current_space_etag;
        spaceBeforeIDs = (projection.items || []).map((item) => item.price_version_id);
      } catch {
        preflights.current.delete(preflightController);
        if (mounted.current) { setEditorErrors({ form: labels.loadPriceSpaceFailed }); setLoading((current) => ({ ...current, mutation: false })); }
        retainUnknown(null);
        pricingLifecycleMutationLock.release();
        return;
      }
    }
    preflights.current.delete(preflightController);
    const { errors: validation, input } = buildPriceDraftInput(editor, formula, currentETag);
    if (Object.keys(validation).length) {
      setEditorErrors(validation);
      retainUnknown(null);
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
      return;
    }
    const currentFacts = latestFacts.current;
    const currentFormula = currentFacts.formulas.find((item) => item.formula_version_id === editor.formula_version_id);
    if (!mounted.current || !currentFacts.canManage || !currentFormula || !newModelPriceAllowed(editor, currentFacts.canReadModels, currentFacts.models, currentFormula)) {
      if (mounted.current) { setEditorErrors({ form: currentFacts.canManage ? labels.modelSelectionInvalid : labels.permissionLost }); setLoading((current) => ({ ...current, mutation: false })); }
      retainUnknown(null);
      pricingLifecycleMutationLock.release();
      return;
    }
    const dispatched = { phase: 'DISPATCHED', kind: editor.price_version_id ? 'DRAFT_UPDATE' : 'DRAFT_CREATE', targetID: editor.price_version_id, input, idempotencyKey: mutationKey('price-draft'), beforeIDs: editor.price_version_id ? [] : spaceBeforeIDs };
    setUnknownMutation(dispatched);
    setNotice('');
    try {
      await replayPricingMutation(client, dispatched);
      if (!mounted.current) return;
      setUnknownMutation(null);
      setEditor(null);
      setNotice(labels.draftSaved);
      void loadPrices().catch(() => {});
    } catch (error) {
      setEditorErrors({ form: labels.errors[error?.code] || labels.actionFailed });
      if (!outcomeUnknown(error)) setUnknownMutation(null);
    } finally {
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
    }
  };

  const submitUnit = async (event) => {
    event.preventDefault();
    if (!unitForm || loading.mutation || unknownMutation || !pricingLifecycleMutationLock.tryAcquire()) return;
    retainUnknown({ phase: 'PREPARING', kind: 'UNIT_CREATE' });
    if (!canManage) { setErrors((current) => ({ ...current, unitMutation: labels.permissionLost })); retainUnknown(null); pricingLifecycleMutationLock.release(); return; }
    setLoading((current) => ({ ...current, mutation: true }));
    let input;
    try { input = {
      unit_code: unitForm.unit_code.trim().toUpperCase(),
      minimum_precision: integerMoney(unitForm.minimum_precision).toString(),
      price_denominator: integerMoney(unitForm.price_denominator).toString(),
      rounding_rule: unitForm.rounding_rule,
    }; } catch {
      setErrors(current => ({ ...current, unitMutation: labels.integerError }));
      retainUnknown(null); pricingLifecycleMutationLock.release();
      setLoading(current => ({ ...current, mutation: false })); return;
    }
    if (!input.unit_code || !input.minimum_precision || BigInt(input.minimum_precision) < 1n || !input.price_denominator || BigInt(input.price_denominator) < 1n) {
      setErrors(current => ({ ...current, unitMutation: labels.integerError }));
      retainUnknown(null);
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
      return;
    }
    let baseline;
    const baselineController = new globalThis.AbortController();
    preflights.current.add(baselineController);
    try {
      baseline = await client.meteringUnits(baselineController.signal);
      preflights.current.delete(baselineController);
      if (!mounted.current || !latestFacts.current.canManage) {
        retainUnknown(null);
        pricingLifecycleMutationLock.release();
        if (mounted.current) setLoading((current) => ({ ...current, mutation: false }));
        return;
      }
      setUnits(baseline.items || []);
    } catch {
      preflights.current.delete(baselineController);
      if (mounted.current) setErrors((current) => ({ ...current, unitMutation: labels.unitBaselineFailed }));
      retainUnknown(null);
      pricingLifecycleMutationLock.release();
      if (mounted.current) setLoading((current) => ({ ...current, mutation: false }));
      return;
    }
    const dispatched = { phase: 'DISPATCHED', kind: 'UNIT_CREATE', input, idempotencyKey: mutationKey('metering-unit'), beforeIDs: (baseline.items || []).map((item) => item.unit_definition_id) };
    setUnknownMutation(dispatched);
    setNotice('');
    try {
      await replayPricingMutation(client, dispatched);
      if (!mounted.current) return;
      setUnknownMutation(null);
      setUnitForm(null);
      setNotice(labels.unitCreated);
      void loadUnits().catch(() => {});
    } catch (error) {
      setErrors((current) => ({ ...current, unitMutation: labels.errors[error?.code] || labels.actionFailed }));
      if (!outcomeUnknown(error)) setUnknownMutation(null);
    } finally {
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
    }
  };

  const showPreview = async (item) => {
    setLoading((current) => ({ ...current, preview: true }));
    setNotice('');
    try {
      const payload = await readTransient('preview', (signal) => client.previewPrice(item.price_version_id, signal));
      setPreview(payload);
    } catch (error) {
      if (error?.name !== 'AbortError') setErrors((current) => ({ ...current, preview: labels.errors[error?.code] || labels.previewFailed }));
    } finally {
      if (mounted.current) setLoading((current) => ({ ...current, preview: false }));
    }
  };

  const runPendingAction = async (event) => {
    event.preventDefault();
    if (!pendingAction || loading.mutation || unknownMutation || !pricingLifecycleMutationLock.tryAcquire()) return;
    if (!canManage) { setPendingAction((current) => ({ ...current, error: labels.permissionLost })); pricingLifecycleMutationLock.release(); return; }
    const { type, item } = pendingAction;
    const reason = String(pendingAction.reason || '').trim();
    if (type === 'TRANSITION' && !reason || type === 'TRANSITION' && pendingAction.mode === 'CONTINUATION' && !pendingAction.replacementID) {
      pricingLifecycleMutationLock.release();
      return;
    }
    setLoading((current) => ({ ...current, mutation: true }));
    setNotice('');
    const dispatched = { phase: 'DISPATCHED', kind: type === 'PUBLISH' ? 'PRICE_PUBLISH' : type === 'TRANSITION' ? 'PRICE_TRANSITION' : 'UNIT_DEACTIVATE',
      targetID: type === 'DEACTIVATE' ? item.unit_definition_id : item.price_version_id,
      idempotencyKey: pendingAction.idempotencyKey,
      input: type === 'PUBLISH' ? { expected_etag: item.space_etag } : type === 'TRANSITION' ? {
        action: item.derived_status === 'PUBLISHED' ? 'CANCEL' : 'END',
        ...(item.derived_status === 'ACTIVE' ? { mode: pendingAction.mode } : {}), reason,
        ...(pendingAction.mode === 'CONTINUATION' ? { replacement_price_version_id: pendingAction.replacementID } : {}),
      } : {} };
    setUnknownMutation(dispatched);
    try {
      await replayPricingMutation(client, dispatched);
      if (!mounted.current) return;
      setUnknownMutation(null);
      setPendingAction(null);
      setNotice(labels.actionCompleted);
      if (type === 'DEACTIVATE') void loadUnits().catch(() => {}); else void loadPrices().catch(() => {});
    } catch (error) {
      setPendingAction((current) => ({ ...current, error: labels.errors[error?.code] || labels.actionFailed }));
      if (!outcomeUnknown(error)) { setUnknownMutation(null); setPendingAction(current => ({ ...current, idempotencyKey: mutationKey('price-action') })); }
    } finally {
      pricingLifecycleMutationLock.release();
      setLoading((current) => ({ ...current, mutation: false }));
    }
  };

  const openPublish = (item) => setPendingAction({ type: 'PUBLISH', item, idempotencyKey: mutationKey('price-publish'), error: '' });
  const openTransition = async (item) => {
    const modes = priceTransitionModes(item);
    let candidates = [];
    if (item.derived_status === 'ACTIVE') {
      try {
        const projection = await readTransient('transition', (signal) => client.managedPriceSpace(priceSpaceQuery(item, item), signal));
        if (projection.has_more) {
          setErrors((current) => ({ ...current, prices: labels.continuationTruncated }));
          return;
        }
        candidates = matchingContinuationDrafts(item, projection.items || []);
      } catch (error) {
        if (error?.name !== 'AbortError') setErrors((current) => ({ ...current, prices: labels.loadPriceSpaceFailed }));
        return;
      }
    }
    setPendingAction({ type: 'TRANSITION', item, candidates, mode: modes[0] || '', reason: '', replacementID: '', idempotencyKey: mutationKey('price-transition'), error: '' });
  };
  const openDeactivate = (item) => setPendingAction({ type: 'DEACTIVATE', item, idempotencyKey: mutationKey('unit-deactivate'), error: '' });

  const verifyUnknown = async () => {
    if (!unknownMutation || loading.recovery || !pricingLifecycleMutationLock.tryAcquire()) return;
    if (!latestFacts.current.canManage) { setNotice(labels.permissionLost); pricingLifecycleMutationLock.release(); return; }
    setLoading((current) => ({ ...current, recovery: true }));
    try {
      // The original key recovers the durable receipt even after a lost response.
      await replayPricingMutation(client, unknownMutation);
      if (!mounted.current) return;
      setUnknownMutation(null);
      setNotice(labels.recoveryConfirmed);
      setEditor(null); setUnitForm(null); setPendingAction(null);
      void loadPrices().catch(() => {}); void loadUnits().catch(() => {});
    } catch (error) {
      if (mounted.current && error?.name !== 'AbortError') setNotice(labels.errors[error?.code] || labels.recoveryFailed);
    } finally {
      pricingLifecycleMutationLock.release();
      if (mounted.current) setLoading((current) => ({ ...current, recovery: false }));
    }
  };

  return (
    <div className="adm-pricing-workspace">
      <div className="adm-section-tools">
        <span>{contextual ? labels.modelContextBoundary(contextModel.display_name) : labels.lifecycleBoundary}</span>
        <div className="adm-row-actions">
          {canManage && <button type="button" onClick={openCreate} disabled={loading.mutation || unknownMutation || !formulas.length}>{labels.createDraft}</button>}
          {onExit && <button type="button" onClick={onExit}>{labels.readOnlySummary}</button>}
          <button type="button" onClick={() => { void loadPrices().catch(() => {}); void loadUnits().catch(() => {}); void loadProtocols().catch(() => {}); void loadModels().catch(() => {}); }} disabled={Boolean(loading.recovery || loading.prices || loading.units || loading.formulas || loading.models)}>{copy.refresh}</button>
        </div>
      </div>
      {unknownMutation && <div className="adm-inline-message error" role="alert">{labels.resultUnknown}<button type="button" onClick={() => void verifyUnknown()} disabled={loading.recovery}>{loading.recovery ? labels.verifying : labels.verifyResult}</button></div>}
      {notice && <div className="adm-inline-message ok" role="status">{notice}</div>}
      {Object.entries(errors).map(([key, error]) => <div key={key} className="adm-inline-message error" role="alert">{error}</div>)}
      {!formulas.length && !errors.formulas && <div className="adm-inline-message">{labels.noPublishedFormula}</div>}

      {editor && (
        <form className="adm-pricing-editor" onSubmit={submitDraft} noValidate>
          <fieldset className="adm-pricing-fieldset" disabled={Boolean(loading.mutation || unknownMutation || !canManage)}>
          <header><div><strong>{editor.price_version_id ? labels.editDraft : labels.createDraft}</strong><small>{labels.draftContract}</small></div><button type="button" disabled={Boolean(unknownMutation)} onClick={() => setEditor(null)}>{copy.accounts.cancel}</button></header>
          {editorErrors.form && <div className="adm-inline-message error" role="alert">{editorErrors.form}</div>}
          <div className="adm-pricing-fields">
            <label>{labels.scope}<select value={editor.scope_type} disabled={contextual || Boolean(editor.price_version_id)} onChange={(event) => setEditor((current) => ({ ...current, scope_type: event.target.value, scope_id: event.target.value === 'GLOBAL' ? '' : (compatibleModels[0]?.model_id || ''), units: Object.fromEntries(Object.entries(current.units).map(([id, value]) => [id, { ...value, config_type: event.target.value === 'GLOBAL' ? 'FIXED_PRICE' : value.config_type }])) }))}><option value="MODEL" disabled={!compatibleModels.length}>{labels.modelScope}</option><option value="GLOBAL">{labels.global}</option></select></label>
            {editor.scope_type === 'MODEL' && <label>{labels.model}<select value={editor.scope_id} disabled={contextual || Boolean(editor.price_version_id)} onChange={(event) => setEditor((current) => ({ ...current, scope_id: event.target.value }))}>{editor.price_version_id && !compatibleModels.some((item) => item.model_id === editor.scope_id) && <option value={editor.scope_id}>{editor.scope_id}</option>}{compatibleModels.map((item) => <option key={item.model_id} value={item.model_id}>{item.display_name} · {shortID(item.model_id)}</option>)}</select>{!compatibleModels.length && <small>{labels.modelDirectoryRequired}</small>}{editorErrors.scope_id && <small className="adm-field-error">{fieldError(labels, editorErrors.scope_id)}</small>}</label>}
            <label>{labels.formula}<select value={editor.formula_version_id} disabled={contextual || Boolean(editor.price_version_id)} onChange={(event) => changeFormula(event.target.value)}><option value="">{labels.chooseFormula}</option>{formulas.map((formula) => <option key={formula.formula_version_id} value={formula.formula_version_id}>{formula.protocol_code} · v{formula.version} · {shortID(formula.formula_version_id)}</option>)}</select></label>
            <label>{labels.effectiveFrom}<input type="date" value={editor.effective_from} onChange={(event) => setEditor((current) => ({ ...current, effective_from: event.target.value }))} />{editorErrors.effective_from && <small className="adm-field-error">{fieldError(labels, editorErrors.effective_from)}</small>}</label>
            <label>{labels.effectiveTo}<input type="date" value={editor.effective_to} onChange={(event) => setEditor((current) => ({ ...current, effective_to: event.target.value }))} />{editorErrors.effective_to && <small className="adm-field-error">{fieldError(labels, editorErrors.effective_to)}</small>}</label>
            <label>{labels.etag}<input value={editor.price_version_id ? editor.expected_etag : labels.serverDerived} readOnly /></label>
          </div>
          <strong>{labels.unitConfiguration}</strong>
          <div className="adm-pricing-unit-configs">
            {Object.entries(formulas.find((item) => item.formula_version_id === editor.formula_version_id)?.required_unit_versions || {}).map(([code, unitID]) => {
              const value = editor.units?.[unitID] || { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '' };
              const definition = unitByID.get(unitID);
              return <div key={unitID}><div><strong>{code}</strong><small>v{definition?.version || '—'} · {shortID(unitID)}</small></div>{editor.scope_type === 'MODEL' && <select aria-label={`${code} ${labels.configurationType}`} value={value.config_type} onChange={(event) => setEditor((current) => ({ ...current, units: { ...current.units, [unitID]: { ...value, config_type: event.target.value, fixed_price_per_denominator: event.target.value === 'INHERIT_GLOBAL' ? '' : value.fixed_price_per_denominator } } }))}><option value="FIXED_PRICE">{labels.fixedPrice}</option><option value="INHERIT_GLOBAL">{labels.inheritGlobal}</option></select>}{(editor.scope_type === 'GLOBAL' || value.config_type !== 'INHERIT_GLOBAL') && <input type="number" min="0" step="1" aria-label={`${code} ${labels.unitPrice}`} value={value.fixed_price_per_denominator} onChange={(event) => setEditor((current) => ({ ...current, units: { ...current.units, [unitID]: { ...value, fixed_price_per_denominator: event.target.value } } }))} />}{editorErrors[unitID] && <small className="adm-field-error">{fieldError(labels, editorErrors[unitID])}</small>}</div>;
            })}
          </div>
          <footer><button type="submit" disabled={loading.mutation || unknownMutation || !canManage}>{copy.accounts.save}</button><button type="button" disabled={Boolean(unknownMutation)} onClick={() => setEditor(null)}>{copy.accounts.cancel}</button></footer>
          </fieldset>
        </form>
      )}

      <section className="adm-finance-subpanel">
        <header className="adm-pricing-panel-head"><div><h3>{labels.priceVersions}</h3><p>{contextual ? labels.modelPriceVersionsLead : labels.priceVersionsLead}</p></div><div className="adm-row-actions"><span>{labels.returnedCount(prices.length)}</span>{updatedAt.prices && <small>{copy.lastUpdated} {updatedAt.prices.toLocaleString()}</small>}<button type="button" onClick={() => void loadPrices().catch(() => {})} disabled={loading.prices || loading.recovery}>{copy.refresh}</button></div></header>
        <div className="adm-table-wrap" tabIndex="0"><table className="adm-table"><thead><tr><th>{labels.scope}</th><th>{labels.protocolFormula}</th><th>{labels.version}</th><th>{copy.accounts.status}</th><th>{labels.effectivity}</th><th>{labels.etag}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>
          {prices.map((item) => <tr key={item.price_version_id}><td><strong>{item.scope_type}</strong><br />{onOpenModel && item.scope_type === 'MODEL' && stableUUID(item.scope_id) ? <button type="button" className="adm-name-link" onClick={() => onOpenModel(item.scope_id)}>{item.scope_id}</button> : <small>{item.scope_id || labels.global}</small>}</td><td>{item.protocol_code}<br /><code title={item.formula_version_id}>{shortID(item.formula_version_id)}</code></td><td>v{item.version}</td><td><PricingStatus value={item.derived_status} /></td><td>{formatDate(item.effective_from)}<br /><small>{item.effective_to ? formatDate(item.effective_to) : labels.openEnded}</small></td><td>{item.lifecycle_status === 'DRAFT' ? item.space_etag : item.current_space_etag || item.space_etag}</td><td><div className="adm-row-actions"><button type="button" onClick={() => void showPreview(item)} disabled={loading.preview}>{labels.preview}</button>{canManage && item.lifecycle_status === 'DRAFT' && <button type="button" onClick={() => openEdit(item)} disabled={loading.mutation || unknownMutation}>{labels.editDraft}</button>}{canManage && item.lifecycle_status === 'DRAFT' && <button type="button" onClick={() => openPublish(item)} disabled={loading.mutation || unknownMutation}>{labels.publish}</button>}{canManage && ['PUBLISHED', 'ACTIVE'].includes(item.derived_status) && <button type="button" className={item.derived_status === 'ACTIVE' ? 'danger' : ''} onClick={() => void openTransition(item)} disabled={loading.mutation || unknownMutation}>{item.derived_status === 'PUBLISHED' ? labels.cancelFuture : labels.endActive}</button>}</div></td></tr>)}
          {!prices.length && !loading.prices && <tr><td colSpan="7" className="adm-empty">{contextual ? labels.noModelPrices : labels.noPrices}</td></tr>}
        </tbody></table></div>
      </section>

      {!contextual && <section className="adm-finance-subpanel">
        <header className="adm-pricing-panel-head"><div><h3>{labels.meteringUnits}</h3><p>{labels.meteringUnitsLead}</p></div><div className="adm-row-actions"><button type="button" onClick={() => void loadUnits().catch(() => {})} disabled={loading.units || loading.recovery}>{copy.refresh}</button>{canManage && <button type="button" onClick={() => setUnitForm({ unit_code: '', minimum_precision: '1', price_denominator: '1000000', rounding_rule: 'CEILING' })} disabled={loading.mutation || unknownMutation}>{labels.createUnitVersion}</button>}</div></header>
        {unitForm && <form className="adm-pricing-unit-form" onSubmit={submitUnit}><fieldset className="adm-pricing-fieldset" disabled={Boolean(loading.mutation || unknownMutation || !canManage)}><label>{labels.unitCode}<input maxLength="80" value={unitForm.unit_code} onChange={(event) => setUnitForm((current) => ({ ...current, unit_code: event.target.value }))} /></label><label>{labels.minimumPrecision}<input type="number" min="1" step="1" value={unitForm.minimum_precision} onChange={(event) => setUnitForm((current) => ({ ...current, minimum_precision: event.target.value }))} /></label><label>{labels.priceDenominator}<input type="number" min="1" step="1" value={unitForm.price_denominator} onChange={(event) => setUnitForm((current) => ({ ...current, price_denominator: event.target.value }))} /></label><label>{labels.roundingRule}<select value={unitForm.rounding_rule} onChange={(event) => setUnitForm((current) => ({ ...current, rounding_rule: event.target.value }))}><option>CEILING</option><option>FLOOR</option><option>HALF_UP</option></select></label><button disabled={loading.mutation || unknownMutation || !canManage}>{labels.createUnitVersion}</button><button type="button" disabled={Boolean(unknownMutation)} onClick={() => setUnitForm(null)}>{copy.accounts.cancel}</button></fieldset></form>}
        <div className="adm-table-wrap" tabIndex="0"><table className="adm-table"><thead><tr><th>{labels.unitCode}</th><th>{labels.version}</th><th>{labels.minimumPrecision}</th><th>{labels.priceDenominator}</th><th>{labels.roundingRule}</th><th>{copy.accounts.status}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>
          {units.map((item) => <tr key={item.unit_definition_id}><td><strong>{item.unit_code}</strong><br /><code title={item.unit_definition_id}>{shortID(item.unit_definition_id)}</code></td><td>v{item.version}</td><td>{item.minimum_precision}</td><td>{item.price_denominator}</td><td>{item.rounding_rule}</td><td><PricingStatus value={item.status} /></td><td>{canManage && item.status === 'ACTIVE' ? <button type="button" className="danger" onClick={() => openDeactivate(item)} disabled={loading.mutation || unknownMutation}>{labels.deactivate}</button> : '—'}</td></tr>)}
          {!units.length && !loading.units && <tr><td colSpan="7" className="adm-empty">{labels.noUnits}</td></tr>}
        </tbody></table></div>
      </section>}

      {preview && <aside className="adm-pricing-preview" role="dialog" aria-modal="true" aria-label={labels.preview}><header><div><strong>{labels.resolvedPreview}</strong><code>{preview.price_version_id}</code></div><button type="button" onClick={() => setPreview(null)} aria-label={copy.accounts.cancel}>×</button></header><div className="adm-detail-grid"><div><small>{labels.scope}</small><strong>{preview.scope_type} · {preview.model}</strong></div><div><small>{labels.protocolFormula}</small><strong>{preview.protocol_code} · {shortID(preview.formula_version_id)}</strong></div><div><small>{labels.resolvedAt}</small><strong>{formatDate(preview.resolved_at)}</strong></div></div><div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{labels.unitCode}</th><th>{labels.configurationType}</th><th>{labels.unitPrice}</th><th>{labels.priceSource}</th><th>{labels.priceDenominator}</th></tr></thead><tbody>{(preview.units || []).map((unit) => <tr key={unit.unit_definition_id}><td>{unit.unit_code} v{unit.unit_version}</td><td>{unit.configuration_type}</td><td>{unit.resolved_price}</td><td>{unit.price_source_scope}<br /><code>{shortID(unit.source_price_version_id)}</code></td><td>{unit.price_denominator}</td></tr>)}</tbody></table></div></aside>}

      {pendingAction && <div className="adm-pricing-action-backdrop"><form className="adm-pricing-action" role="dialog" aria-modal="true" aria-label={labels.confirmAction} onSubmit={runPendingAction}><header><div><strong>{pendingAction.type === 'PUBLISH' ? labels.publish : pendingAction.type === 'DEACTIVATE' ? labels.deactivate : pendingAction.item.derived_status === 'PUBLISHED' ? labels.cancelFuture : labels.endActive}</strong><code>{pendingAction.item.price_version_id || pendingAction.item.unit_definition_id}</code></div><button type="button" disabled={Boolean(unknownMutation)} onClick={() => setPendingAction(null)} aria-label={copy.accounts.cancel}>×</button></header>{pendingAction.error && <div className="adm-inline-message error" role="alert">{pendingAction.error}</div>}<p>{pendingAction.type === 'PUBLISH' ? labels.publishConfirmation : pendingAction.type === 'DEACTIVATE' ? labels.deactivateConfirmation : labels.transitionConfirmation}</p>{pendingAction.type === 'TRANSITION' && <>{pendingAction.item.derived_status === 'ACTIVE' && <label>{labels.transitionMode}<select disabled={Boolean(loading.mutation || unknownMutation || !canManage)} value={pendingAction.mode} onChange={(event) => setPendingAction((current) => ({ ...current, mode: event.target.value, replacementID: event.target.value === 'CONTINUATION' ? current.replacementID : '' }))}>{priceTransitionModes(pendingAction.item).map((mode) => <option key={mode}>{mode}</option>)}</select></label>}{pendingAction.mode === 'CONTINUATION' && <label>{labels.replacementDraft}<select disabled={Boolean(loading.mutation || unknownMutation || !canManage)} required value={pendingAction.replacementID} onChange={(event) => setPendingAction((current) => ({ ...current, replacementID: event.target.value }))}><option value="">{labels.chooseReplacement}</option>{(pendingAction.candidates || []).map((item) => <option key={item.price_version_id} value={item.price_version_id}>v{item.version} · {shortID(item.price_version_id)}</option>)}</select></label>}<label>{copy.billing.reason}<textarea disabled={Boolean(loading.mutation || unknownMutation || !canManage)} required maxLength="500" value={pendingAction.reason} onChange={(event) => setPendingAction((current) => ({ ...current, reason: event.target.value }))} /></label></>}<footer><button type="submit" className={pendingAction.type === 'DEACTIVATE' || pendingAction.type === 'TRANSITION' ? 'danger' : ''} disabled={loading.mutation || unknownMutation || !canManage}>{labels.confirmAction}</button><button type="button" disabled={Boolean(unknownMutation)} onClick={() => setPendingAction(null)}>{copy.accounts.cancel}</button></footer></form></div>}
    </div>
  );
}
