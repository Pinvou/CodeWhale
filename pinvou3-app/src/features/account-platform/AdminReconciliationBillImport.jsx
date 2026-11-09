import React, { useEffect, useId, useRef, useState } from 'react';
import { RECONCILIATION_BILL_MAX_BYTES, reconciliationImportPreview, requireReconciliationDate } from './reconciliation-contract.js';
import './admin-reconciliation-bill.css';

function template(channel, date) {
  const example = [{
    entry_type: 'PAYMENT', merchant_order_no: 'SYNTHETIC-ORDER-001',
    channel_transaction_id: 'SYNTHETIC-TRANSACTION-001', channel,
    amount_fen: 100, currency: 'CNY', status: 'SUCCESS', paid_at: date + 'T10:00:00+08:00',
  }];
  return 'data:application/json;charset=utf-8,' + encodeURIComponent(JSON.stringify(example, null, 2) + '\n');
}

export function AdminReconciliationBillImport({ channel, date, copy, disabled, onImport }) {
  const [mode, setMode] = useState('file'), [source, setSource] = useState(''), [fileName, setFileName] = useState('');
  const [preview, setPreview] = useState(null), [error, setError] = useState(null), [reading, setReading] = useState(false);
  const generation = useRef(0), submitting = useRef(false);
  const headingID = useId(), helpID = useId(), errorID = useId(), inputAreaID = useId();
  useEffect(() => () => { generation.current++; }, []);
  let validDate = false;
  try { requireReconciliationDate(date); validDate = true; } catch { /* The existing date field is still the authority. */ }
  const inspect = (text, current) => {
    if (current !== generation.current) return;
    try { setPreview(reconciliationImportPreview(text)); setError(null); }
    catch (reason) { setPreview(null); setError({ kind: reason?.kind || 'billInvalid', row: reason?.row, field: reason?.field }); }
  };
  const pickFile = async event => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    const current = ++generation.current;
    setPreview(null); setError(null); setFileName(file.name);
    if (file.size > RECONCILIATION_BILL_MAX_BYTES) {
      setError({ kind: 'billTooLarge' }); setReading(false); return;
    }
    setReading(true);
    try { inspect(await file.text(), current); }
    catch { if (current === generation.current) setError({ kind: 'billReadError' }); }
    finally { if (current === generation.current) setReading(false); }
  };
  const clear = () => {
    generation.current++;
    setSource(''); setFileName(''); setPreview(null); setError(null); setReading(false);
  };
  const errorText = error ? (copy[error.kind] || copy.billInvalid).replace('{row}', String(error.row ?? '')).replace('{field}', error.field ?? '') : '';
  const description = error ? helpID + ' ' + errorID : helpID;
  const canImport = Boolean(preview && validDate && !reading && !disabled);
  return <section className="adm-panel adm-reconciliation-bill" aria-labelledby={headingID}>
    <header><h3 id={headingID}>{copy.billTitle}</h3><p id={helpID}>{copy.billHelp}</p></header>
    <div className="adm-reconciliation-bill-source">
      {validDate && <a href={template(channel, date)} download="provider-bill-template.json">{copy.billTemplate}</a>}
      <button type="button" aria-expanded={mode === 'paste'} aria-controls={inputAreaID} disabled={disabled} onClick={() => {
        clear(); setMode(mode === 'file' ? 'paste' : 'file');
      }}>{mode === 'file' ? copy.billAdvanced : copy.billUseFile}</button>
    </div>
    <div id={inputAreaID} className="adm-reconciliation-bill-input">
      {mode === 'file' ? <label>{copy.billFile}<input type="file" accept=".json,application/json" disabled={disabled} aria-describedby={description} onChange={pickFile} /></label> : <>
        <label>{copy.importedItems}<textarea disabled={disabled} value={source} aria-describedby={description} onChange={event => {
          generation.current++; setSource(event.target.value); setPreview(null); setError(null);
        }} /></label>
        <button type="button" disabled={disabled || !source.trim()} onClick={() => inspect(source, generation.current)}>{copy.billPreview}</button>
      </>}
      {fileName && <small>{copy.billSelectedFile}: <bdi>{fileName}</bdi></small>}
      {(fileName || source) && <button type="button" disabled={disabled} onClick={clear}>{copy.billClear}</button>}
    </div>
    <div className="adm-reconciliation-bill-preview" aria-busy={reading}>
      <dl><dt>{copy.channel}</dt><dd>{channel}</dd><dt>{copy.businessDate}</dt><dd>{validDate ? date : copy.billInvalidDate}</dd>
        {preview && <><dt>{copy.rows}</dt><dd>{preview.items.length}</dd></>}
      </dl>
      <p role="status">{reading ? copy.billReading : preview ? copy.billReady : !error ? copy.billPending : ''}</p>
      {preview && <dl className="adm-reconciliation-bill-totals">{preview.totals.map(({ currency, fen }) => <React.Fragment key={currency}>
        <dt><bdi>{currency}</bdi></dt><dd><bdi>{fen}</bdi> {copy.billFen}</dd>
      </React.Fragment>)}</dl>}
      {error && <p id={errorID} role="alert" className="adm-inline-message error">{errorText}</p>}
    </div>
    <footer><button type="button" disabled={!canImport} onClick={async () => {
      if (!canImport || submitting.current) return;
      submitting.current = true;
      try { await onImport(preview.items); } finally { submitting.current = false; }
    }}>{copy.import}</button></footer>
  </section>;
}
