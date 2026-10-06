import React, { useCallback, useEffect, useState } from 'react';
import { useAdminOperation } from './use-admin-operation.js';
import { requireAdminItems, requireAdminRecord, requireAdminFacts, invalidAdminAcknowledgement } from './admin-acknowledgements.js';
import { parseScaledAmount } from './credit-units.js';

const emptyPrice = { scope_type:'MODEL', scope_id:'', formula_version_id:'', effective_from:'', effective_to:'', expected_etag:'1', units:{} };
const integer = (value, positive = false) => {
  const result = parseScaledAmount(String(value), 0);
  if (result === null || (positive && result < 1)) throw invalidAdminAcknowledgement();
  return result;
};
const objectJSON = value => {
  const result = JSON.parse(value);
  if (!result || typeof result !== 'object' || Array.isArray(result)) throw invalidAdminAcknowledgement();
  return result;
};

export function AdminPricing({ client, copy, onAuthorizationError }) {
  const billing = useAdminOperation(onAuthorizationError, 'billing');
  const token = useAdminOperation(onAuthorizationError, 'token');
  const busy = billing.busy || token.busy;
  const [resourceSnapshot, setResources] = useState(null);
  // Responses and previews belong to the client that authorized their reads.
  const resources = resourceSnapshot?.client === client ? resourceSnapshot.value : { units:[], prices:[], protocols:[], formulas:[] };
  const [errors, setErrors] = useState({});
  const [unit, setUnit] = useState({ unit_code:'', minimum_precision:'1', price_denominator:'1000000', rounding_rule:'CEILING' });
  const [protocol, setProtocol] = useState({ protocol_code:'CHAT', request_schema:'{"parameters":["estimated_input_tokens","cached_input_tokens","max_output_tokens"]}' });
  const [formula, setFormula] = useState({ protocolID:'', required:'{}', expression:'{}' });
  const [price, setPrice] = useState(emptyPrice);
  const [editing, setEditing] = useState(null);
  const [preview, setPreview] = useState(null);
  const load = useCallback(async ({ signal, isCurrent }) => {
    const requests = { units:client.meteringUnits(signal), prices:client.managedPrices(signal), protocols:client.protocols(signal) };
    const entries = await Promise.all(Object.entries(requests).map(async ([key, request]) => {
      try {
        const value = await request;
        if (key === 'protocols') {
          if (!Array.isArray(value.protocols) || !Array.isArray(value.formulas)) throw invalidAdminAcknowledgement();
          return { protocols:value.protocols, formulas:value.formulas };
        }
        return { [key]:requireAdminItems(value) };
      } catch (error) {
        if (error.status === 401 || error.code === 'CSRF_REJECTED') throw error;
        return { [key]:[], ...(key === 'protocols' ? { formulas:[] } : {}), errors:{ [key]:error.status === 403 ? 'denied' : 'failed' } };
      }
    }));
    if (isCurrent()) {
      const next = Object.assign({}, ...entries.map(({ errors:_errors, ...items }) => items));
      const nextErrors = Object.assign({}, ...entries.map(value => value.errors || {}));
      setResources({ client, value:next });
      setErrors(nextErrors);
      if (Object.keys(nextErrors).length) setPreview(null);
      if (nextErrors.protocols) setFormula(current => ({ ...current, protocolID:'' }));
    }
  }, [client]);
  useEffect(() => {
    token.invalidate(); setPreview(null); setErrors({});
    setEditing(null); setPrice(emptyPrice);
    setFormula({ protocolID:'', required:'{}', expression:'{}' });
    void billing.run(load);
  }, [load, token.invalidate]);
  const mutate = (ops, action) => ops.run(async context => {
    await action(context);
    if (!context.isCurrent()) return;
    await load(context);
    return 'completed';
  }, true);
  const selectedFormula = resources.formulas.find(item => item.formula_version_id === price.formula_version_id);
  const savePrice = event => {
    event.preventDefault();
    void mutate(billing, async ({ isCurrent }) => {
      if (!selectedFormula || selectedFormula.status !== 'PUBLISHED') throw invalidAdminAcknowledgement();
      const units = Object.entries(selectedFormula.required_unit_versions).map(([code, id]) => {
        const configuration = price.units[code] || {};
        return configuration.type === 'INHERIT_GLOBAL'
          ? { unit_definition_id:id, config_type:'INHERIT_GLOBAL' }
          : { unit_definition_id:id, config_type:'FIXED_PRICE', fixed_price_per_denominator:integer(configuration.rate || '') };
      });
      const input = { scope_type:price.scope_type, scope_id:price.scope_type === 'MODEL' ? price.scope_id.trim() : null,
        protocol_code:selectedFormula.protocol_code, formula_version_id:selectedFormula.formula_version_id, credit_unit:'MICROCREDIT',
        effective_from:`${price.effective_from}T00:00:00+08:00`, effective_to:price.effective_to ? `${price.effective_to}T00:00:00+08:00` : null,
        expected_etag:integer(price.expected_etag, true), units };
      const value = editing ? await client.updatePriceDraft(editing, input) : await client.createPriceDraft(input);
      requireAdminFacts(requireAdminRecord(value, 'price_version_id', editing), { scope_type:input.scope_type, scope_id:input.scope_id, formula_version_id:input.formula_version_id, lifecycle_status:'DRAFT' });
      if (isCurrent()) { setEditing(null); setPrice(emptyPrice); }
    });
  };
  const edit = item => {
    billing.invalidate(); token.invalidate(); setPreview(null); setEditing(item.price_version_id);
    const chosen = resources.formulas.find(value => value.formula_version_id === item.formula_version_id);
    const units = {};
    for (const [code, id] of Object.entries(chosen?.required_unit_versions || {})) {
      const value = item.units.find(unit => unit.unit_definition_id === id);
      units[code] = { type:value?.config_type || 'FIXED_PRICE', rate:String(value?.fixed_price_per_denominator ?? '') };
    }
    const localDay = value => value ? new Date(new Date(value).getTime() + 8 * 3600000).toISOString().slice(0, 10) : '';
    setPrice({ scope_type:item.scope_type, scope_id:item.scope_id || '', formula_version_id:item.formula_version_id, expected_etag:String(item.space_etag), effective_from:localDay(item.effective_from), effective_to:localDay(item.effective_to), units });
  };
  const transition = (item, action, mode) => {
    const reason = String(window.prompt(copy.billing.reason) || '').trim();
    if (!reason) return;
    let replacement;
    if (mode === 'CONTINUATION') {
      replacement = String(window.prompt(copy.pricing.replacement) || '').trim();
      if (!resources.prices.some(candidate => candidate.price_version_id === replacement && candidate.lifecycle_status === 'DRAFT'
        && candidate.scope_type === item.scope_type && candidate.scope_id === item.scope_id
        && candidate.protocol_code === item.protocol_code && candidate.formula_version_id === item.formula_version_id)) return;
    }
    void mutate(billing, async () => { requireAdminRecord(await client.transitionPrice(item.price_version_id, { action, mode, reason, ...(replacement ? { replacement_price_version_id:replacement } : {}) }), 'price_version_id', item.price_version_id); });
  };
  const field = (key, state, setter, type = 'text') => <label>{copy.pricing[key]}<input type={type} required value={state[key]} onChange={event => setter(current => ({ ...current, [key]:event.target.value }))} /></label>;
  return <section className="adm-panel">
    <header className="adm-section-head"><div><h2>{copy.pricing.title}</h2><p>{copy.pricing.lead}</p></div><button disabled={busy} onClick={() => billing.run(load)}>{copy.refresh}</button></header>
    {(billing.failed || token.failed) && <p role="alert">{copy.billing.actionFailed}</p>}
    {(billing.noticeKey || token.noticeKey) && <p role="status">{copy.billing.completed}</p>}
    {Object.keys(errors).map(key => <p role="alert" key={key}>{copy.pricing[key]}: {errors[key] === 'denied' ? copy.denied : copy.failed}</p>)}
    {!errors.units && <article><h3>{copy.pricing.units}</h3><form className="adm-ops-form" onSubmit={event => { event.preventDefault(); void mutate(billing, async () => {
      const input = { ...unit, unit_code:unit.unit_code.trim().toUpperCase(), minimum_precision:integer(unit.minimum_precision, true), price_denominator:integer(unit.price_denominator, true) };
      requireAdminFacts(requireAdminRecord(await client.createMeteringUnit(input), 'unit_definition_id'), input);
    }); }}>
      {field('unit_code', unit, setUnit)}{field('minimum_precision', unit, setUnit, 'number')}{field('price_denominator', unit, setUnit, 'number')}
      <label>{copy.pricing.rounding_rule}<select value={unit.rounding_rule} onChange={event => setUnit(current => ({ ...current, rounding_rule:event.target.value }))}>{['CEILING','FLOOR','HALF_UP'].map(value => <option key={value}>{value}</option>)}</select></label>
      <button disabled={busy}>{copy.pricing.createVersion}</button>
    </form>{resources.units.map(item => <article key={item.unit_definition_id}><code>{item.unit_code} · v{item.version}</code> · <code>{item.unit_definition_id}</code> · {item.minimum_precision} / {item.price_denominator} · {item.rounding_rule} · {item.status} {item.status === 'ACTIVE' && <button disabled={busy} onClick={() => mutate(billing, async () => { requireAdminFacts(requireAdminRecord(await client.deactivateMeteringUnit(item.unit_definition_id), 'unit_definition_id', item.unit_definition_id), { status:'INACTIVE' }); })}>{copy.pricing.deactivate}</button>}</article>)}</article>}
    {!errors.protocols && <article><h3>{copy.pricing.protocols}</h3>
      <form className="adm-ops-form" onSubmit={event => { event.preventDefault(); void mutate(token, () => client.createProtocol({ protocol_code:protocol.protocol_code.trim().toUpperCase(), request_schema:objectJSON(protocol.request_schema) })); }}>
        {field('protocol_code', protocol, setProtocol)}<label>{copy.pricing.request_schema}<textarea required value={protocol.request_schema} onChange={event => setProtocol(current => ({ ...current, request_schema:event.target.value }))} /></label><button disabled={busy}>{copy.pricing.createProtocol}</button>
      </form>
      {resources.protocols.map(item => <article key={item.protocol_definition_id}><code>{item.protocol_code} · v{item.version} · {item.protocol_definition_id}</code> · {item.status} {item.status === 'DRAFT' && <button disabled={busy} onClick={() => mutate(token, () => client.publishProtocol(item.protocol_definition_id))}>{copy.pricing.publish}</button>}</article>)}
      <form className="adm-ops-form" onSubmit={event => { event.preventDefault(); void mutate(token, () => client.createFormula(formula.protocolID, { required_unit_versions:objectJSON(formula.required), expression:objectJSON(formula.expression) })); }}>
        <label>{copy.pricing.protocol}<select required value={formula.protocolID} onChange={event => setFormula(current => ({ ...current, protocolID:event.target.value }))}><option value="">—</option>{resources.protocols.filter(item => item.status === 'PUBLISHED').map(item => <option key={item.protocol_definition_id} value={item.protocol_definition_id}>{item.protocol_code} · v{item.version}</option>)}</select></label>
        <label>{copy.pricing.required}<textarea required value={formula.required} onChange={event => setFormula(current => ({ ...current, required:event.target.value }))} /></label><label>{copy.pricing.expression}<textarea required value={formula.expression} onChange={event => setFormula(current => ({ ...current, expression:event.target.value }))} /></label><button disabled={busy || !formula.protocolID}>{copy.pricing.createFormula}</button>
      </form>
      {resources.formulas.map(item => <article key={item.formula_version_id}><code>{item.protocol_code} · v{item.version} · {item.formula_version_id}</code> · {item.status} {item.status === 'DRAFT' && <button disabled={busy} onClick={() => mutate(token, () => client.publishFormula(item.formula_version_id))}>{copy.pricing.publish}</button>}<button disabled={busy} onClick={() => token.run(async ({ signal, isCurrent }) => { const value = await client.previewFormula(item.formula_version_id, signal); requireAdminRecord(value.formula, 'formula_version_id', item.formula_version_id); requireAdminFacts(value, { deterministic:true, external_access:false }); if (isCurrent()) setPreview({ client, value }); })}>{copy.pricing.preview}</button></article>)}
    </article>}
    {!errors.prices && <article><h3>{copy.pricing.prices}</h3><form className="adm-ops-form" onSubmit={savePrice}>
      <label>{copy.pricing.scope}<select disabled={Boolean(editing)} value={price.scope_type} onChange={event => setPrice(current => ({ ...current, scope_type:event.target.value }))}><option>MODEL</option><option>GLOBAL</option></select></label>
      {price.scope_type === 'MODEL' && <label>{copy.tokens.model}<input disabled={Boolean(editing)} required value={price.scope_id} onChange={event => setPrice(current => ({ ...current, scope_id:event.target.value }))} /></label>}
      <label>{copy.pricing.formula}<select disabled={Boolean(editing)} required value={price.formula_version_id} onChange={event => setPrice(current => ({ ...current, formula_version_id:event.target.value, units:{} }))}><option value="">—</option>{resources.formulas.filter(item => item.status === 'PUBLISHED').map(item => <option key={item.formula_version_id} value={item.formula_version_id}>{item.protocol_code} · v{item.version}</option>)}</select></label>
      {field('effective_from', price, setPrice, 'date')}<label>{copy.pricing.effective_to}<input type="date" value={price.effective_to} onChange={event => setPrice(current => ({ ...current, effective_to:event.target.value }))} /></label>{field('expected_etag', price, setPrice, 'number')}
      {Object.entries(selectedFormula?.required_unit_versions || {}).map(([code, id]) => { const unit = resources.units.find(item => item.unit_definition_id === id); const config = price.units[code] || {}; return <label key={id}>{code} · {copy.pricing.rateUnit} / {unit?.price_denominator ?? '—'}<select value={config.type || 'FIXED_PRICE'} onChange={event => setPrice(current => ({ ...current, units:{ ...current.units, [code]:{ ...config, type:event.target.value } } }))}><option>FIXED_PRICE</option>{price.scope_type === 'MODEL' && <option>INHERIT_GLOBAL</option>}</select>{config.type !== 'INHERIT_GLOBAL' && <input required type="number" min="0" step="1" value={config.rate || ''} onChange={event => setPrice(current => ({ ...current, units:{ ...current.units, [code]:{ ...config, rate:event.target.value } } }))} />}</label>; })}
      <button disabled={busy || !selectedFormula || Boolean(errors.protocols) || Boolean(errors.units)}>{editing ? copy.accounts.save : copy.pricing.createDraft}</button>{editing && <button type="button" disabled={busy} onClick={() => { setEditing(null); setPrice(emptyPrice); }}>{copy.accounts.cancel}</button>}
    </form>{resources.prices.map(item => <article key={item.price_version_id}><code>{item.scope_type} · {item.scope_id || 'GLOBAL'} · {item.protocol_code} · v{item.version} · ETag {item.space_etag}</code> · {item.derived_status}<div className="adm-row-actions">
      {item.lifecycle_status === 'DRAFT' && <><button disabled={busy} onClick={() => edit(item)}>{copy.pricing.editDraft}</button><button disabled={busy} onClick={() => mutate(billing, () => client.publishPrice(item.price_version_id, item.space_etag))}>{copy.pricing.publish}</button></>}
      <button disabled={busy} onClick={() => billing.run(async ({ signal, isCurrent }) => { const value = requireAdminRecord(await client.previewPrice(item.price_version_id, signal), 'price_version_id', item.price_version_id); if (isCurrent()) setPreview({ client, value }); })}>{copy.pricing.preview}</button>
      {item.derived_status === 'PUBLISHED' && <button disabled={busy} onClick={() => transition(item, 'CANCEL', '')}>{copy.accounts.cancel}</button>}
      {item.derived_status === 'ACTIVE' && <>{item.scope_type === 'MODEL' && <><button disabled={busy} onClick={() => transition(item, 'END', 'FALLBACK_GLOBAL')}>{copy.pricing.endFallback}</button><button disabled={busy} onClick={() => transition(item, 'END', 'BLOCK')}>{copy.pricing.endBlock}</button></>}<button disabled={busy} onClick={() => transition(item, 'END', 'CONTINUATION')}>{copy.pricing.continuation}</button></>}
    </div></article>)}</article>}
    {preview?.client === client && <article><h3>{copy.pricing.preview}</h3><pre>{JSON.stringify(preview.value, null, 2)}</pre></article>}
  </section>;
}
