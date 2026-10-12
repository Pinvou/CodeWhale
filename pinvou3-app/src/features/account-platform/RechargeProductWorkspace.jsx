import React, { useEffect, useState } from 'react';
import { encodeFinanceJSON, requireFinanceCollection } from './finance-contract.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { useFinanceOperation } from './useFinanceOperation.js';

const empty = { product_id: '', display_name_zh_cn: '', display_name_en: '', display_name_ja: '', display_order: '0', base_points: '', bonus_points: '0', validity_days: '', invoice_product_name: '', mapping: { provider_id: '', provider_version: '', sku_bom: '', amount_fen: '', currency: 'CNY', channels: [], admission_class: 'FORMAL' } };
function safeInteger(value, minimum = 0) { const parsed = Number(value); if (!/^\d+$/.test(String(value)) || !Number.isSafeInteger(parsed) || parsed < minimum) throw new Error('INVALID_FINANCE_ACK'); return parsed; }
export function RechargeProductWorkspace({ client, me, copy, language }) {
  const [page, setPage] = useState(null);
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState([]);
  const [options, setOptions] = useState(null);
  const [selected, setSelected] = useState(null);
  const [input, setInput] = useState(empty);
  const [reason, setReason] = useState('');
  const [validationError, setValidationError] = useState('');
  const [verifications, setVerifications] = useState([]);
  const permissions = me.platform_permissions || [];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const operation = useFinanceOperation(owner, copy, client, 'product-catalog');
  const canCreate = permissions.includes('platform.products.create');
  const canPublish = permissions.includes('platform.products.publish');
  const load = async (next = cursor) => {
    const result = await operation.read(async () => {
      const page = await client.rechargeProducts({ cursor: next }); requireFinanceCollection(page);
      const options = canCreate ? await client.rechargeProductCreateOptions() : null;
      if (options && (!Array.isArray(options.adapters) || !Array.isArray(options.invoice_product_names))) throw new Error('INVALID_FINANCE_ACK');
      return { page, options };
    });
    if (result) { setPage(result.page); setOptions(result.options); setCursor(next); }
  };
  useEffect(() => { void load(''); }, [client, owner]); // eslint-disable-line react-hooks/exhaustive-deps
  const open = async item => {
    const result = await operation.read(() => client.productAdmissionVerifications(item.catalog_item_id));
    if (result) { setSelected(item); setInput({ ...item, display_order: String(item.display_order), validity_days: String(item.validity_days), mapping: { ...item.mapping } }); setVerifications(requireFinanceCollection(result)); setReason(''); }
  };
  const execute = (facts, key) => {
    if (facts.action === 'create') return client.createRechargeProduct(facts.body, key);
    if (facts.action === 'update') return client.updateRechargeProduct(facts.id, facts.body, key);
    if (['verify', 'retry', 'revoke'].includes(facts.action)) return client.productAdmissionAction(facts.id, facts.action, facts.body, key, facts.verificationID);
    return client.transitionRechargeProduct(facts.id, facts.action, facts.body, key);
  };
  const submit = async (action, verification) => {
    let facts = operation.pendingFacts;
    if (!facts) {
      setValidationError('');
      try {
      const body = { expected_version: selected?.version || 0, reason: reason.trim() };
      if (!body.reason) return;
      if (action === 'create' || action === 'update') {
        Object.assign(body, { display_name_zh_cn: input.display_name_zh_cn.trim(), display_name_en: input.display_name_en.trim(), display_name_ja: input.display_name_ja.trim(), display_order: safeInteger(input.display_order), base_points: input.base_points, bonus_points: input.bonus_points, validity_days: safeInteger(input.validity_days, 1), invoice_product_name: input.invoice_product_name, mapping: { ...input.mapping } });
        if (action === 'create') body.product_id = input.product_id.trim();
        else body.expected_draft_version = selected.mapping.draft_version;
      }
      if (action === 'display-names') Object.assign(body, { display_name_zh_cn: input.display_name_zh_cn.trim(), display_name_en: input.display_name_en.trim(), display_name_ja: input.display_name_ja.trim() });
      if (action === 'publish') {
        const current = verifications.find(item => item.current && item.active && item.status === 'VERIFIED' && item.evidence_id);
        if (!current) return;
        body.evidence_id = current.evidence_id;
      }
      facts = { action, id: selected?.catalog_item_id, body, verificationID: verification?.verification_id || '' };
      // Validate the exact int64 wire representation before retaining an intent.
      encodeFinanceJSON(body);
      if (!globalThis.confirm(copy.confirmAction)) return;
      } catch { setValidationError('inputInvalid'); return; }
    }
    const value = await operation.mutate(facts, execute);
    if (value) { await load(); setSelected(null); setInput(empty); setVerifications([]); setReason(''); }
  };
  const field = (key, label, numeric = false) => <label key={key}>{label}<input disabled={operation.busy || operation.unknown || !(canCreate || (canPublish && key.startsWith('display_name_')))} value={input[key] ?? ''} inputMode={numeric ? 'numeric' : undefined} onChange={event => setInput(value => ({ ...value, [key]: event.target.value }))} /></label>;
  const mappingField = (key, label) => <label key={key}>{label}<input disabled={operation.busy || operation.unknown || !canCreate} value={input.mapping[key]} onChange={event => setInput(value => ({ ...value, mapping: { ...value.mapping, [key]: event.target.value } }))} /></label>;
  return <section><h3>{copy.productAdmin}</h3><button disabled={operation.busy || operation.unknown} onClick={() => void load()}>{copy.refresh}</button>{validationError && <p role="alert">{copy[validationError]}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}{operation.busy && <p role="status">{copy.loading}</p>}
    {operation.unknown && <button disabled={operation.busy} onClick={() => void submit('')}>{copy.retry}</button>}
    {page && <table><thead><tr><th>{copy.name}</th><th>{copy.status}</th><th>{copy.version}</th><th>{copy.actions}</th></tr></thead><tbody>{page.items.map(item => <tr key={item.catalog_item_id}><td>{localizedProductDisplayName(item, language, 'product_id')}</td><td>{item.lifecycle_status}</td><td>{item.version}</td><td><button disabled={operation.busy || operation.unknown} onClick={() => void open(item)}>{copy.details}</button></td></tr>)}</tbody></table>}
    <button disabled={operation.busy || operation.unknown || !previous.length} onClick={() => { const values = [...previous]; const value = values.pop(); setPrevious(values); void load(value); }}>{copy.previous}</button><button disabled={operation.busy || operation.unknown || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); void load(page.next_cursor); }}>{copy.next}</button>
    {(canCreate || (selected && canPublish)) && <form onSubmit={event => { event.preventDefault(); if (canCreate && (!selected || selected.lifecycle_status === 'DRAFT')) void submit(selected ? 'update' : 'create'); }}><h4>{selected ? copy.details : copy.createProduct}</h4>
      {!selected && field('product_id', copy.productID)}{field('display_name_zh_cn', copy.nameZh)}{field('display_name_en', copy.nameEn)}{field('display_name_ja', copy.nameJa)}{field('display_order', copy.displayOrder, true)}{field('base_points', copy.base, true)}{field('bonus_points', copy.bonus, true)}{field('validity_days', copy.validityDays, true)}
      <label>{copy.invoiceName}<select disabled={operation.busy || operation.unknown || !canCreate} value={input.invoice_product_name} onChange={event => setInput(value => ({ ...value, invoice_product_name: event.target.value }))}><option value="">{copy.invoiceName}</option>{!canCreate && <option value={input.invoice_product_name}>{input.invoice_product_name}</option>}{options?.invoice_product_names.map(value => <option key={value} value={value}>{value}</option>)}</select></label>
      <label>{copy.provider}<select disabled={operation.busy || operation.unknown || !canCreate} value={`${input.mapping.provider_id}/${input.mapping.provider_version}`} onChange={event => { const adapter = options?.adapters.find(value => `${value.provider_id}/${value.provider_version}` === event.target.value); if (adapter) setInput(value => ({ ...value, mapping: { ...value.mapping, provider_id: adapter.provider_id, provider_version: adapter.provider_version, channels: [] } })); }}><option value="/">{copy.provider}</option>{!canCreate && <option value={`${input.mapping.provider_id}/${input.mapping.provider_version}`}>{input.mapping.provider_id} / {input.mapping.provider_version}</option>}{options?.adapters.map(value => <option key={`${value.provider_id}/${value.provider_version}`} value={`${value.provider_id}/${value.provider_version}`}>{value.provider_id} / {value.provider_version}</option>)}</select></label>
      {mappingField('sku_bom', copy.sku)}{mappingField('amount_fen', copy.amountFen)}<label>{copy.channel}{(options?.adapters.find(value => value.provider_id === input.mapping.provider_id && value.provider_version === input.mapping.provider_version)?.channels || (!canCreate ? input.mapping.channels : [])).map(value => <label key={value}><input type="checkbox" disabled={operation.busy || operation.unknown || !canCreate} checked={input.mapping.channels.includes(value)} onChange={event => setInput(item => ({ ...item, mapping: { ...item.mapping, channels: event.target.checked ? [...item.mapping.channels, value] : item.mapping.channels.filter(channel => channel !== value) } }))} />{value}</label>)}</label>
      <label>{copy.admissionClass}<select disabled={operation.busy || operation.unknown || !canCreate} value={input.mapping.admission_class} onChange={event => setInput(value => ({ ...value, mapping: { ...value.mapping, admission_class: event.target.value } }))}><option value="FORMAL">{copy.formal}</option><option value="TEST_ONLY">{copy.testOnly}</option></select></label>
      <label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} value={reason} onChange={event => setReason(event.target.value)} /></label><button disabled={operation.busy || operation.unknown || !canCreate || (selected && selected.lifecycle_status !== 'DRAFT') || !reason.trim() || !input.mapping.provider_id || !input.invoice_product_name}>{selected ? copy.saveDraft : copy.createProduct}</button>
      {selected && canPublish && <button type="button" disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('display-names')}>{copy.saveNames}</button>}
      {selected && <button type="button" disabled={operation.busy || operation.unknown} onClick={() => { setSelected(null); setInput(empty); setVerifications([]); }}>{copy.newInvocation}</button>}
    </form>}
    {selected && <article><h4>{copy.admission}</h4>{!canCreate && !canPublish && <label>{copy.reason}<textarea disabled={operation.busy || operation.unknown} value={reason} onChange={event => setReason(event.target.value)} /></label>}
      {canPublish && <><button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('verify')}>{copy.verify}</button><button disabled={operation.busy || operation.unknown || !reason.trim() || !verifications.some(item => item.current && item.active && item.status === 'VERIFIED' && item.evidence_id)} onClick={() => void submit('publish')}>{copy.publish}</button></>}
      {permissions.includes('platform.products.unpublish') && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('unpublish')}>{copy.unpublish}</button>}{permissions.includes('platform.products.delete') && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('delete')}>{copy.deleteProduct}</button>}
      <ul>{verifications.map(value => <li key={value.verification_id}>{value.status} / {value.requested_at}{canPublish && value.status === 'FAILED' && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('retry', value)}>{copy.retry}</button>}{canPublish && value.current && value.active && value.evidence_id && <button disabled={operation.busy || operation.unknown || !reason.trim()} onClick={() => void submit('revoke', value)}>{copy.revoke}</button>}</li>)}</ul>
    </article>}
  </section>;
}
