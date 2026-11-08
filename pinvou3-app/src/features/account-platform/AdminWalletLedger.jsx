import React, { useEffect, useState } from 'react';
import { adminLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { requireFinanceCollection, requireFinanceID } from './finance-contract.js';
import { formatBalance } from './balance-view.js';
import { AdminFinanceFilters, useFinanceReadRoute } from './AdminFinanceFilters.jsx';

const columns = ['journal_entry_id', 'posting_id', 'ledger_account_code', 'ledger_account_type', 'entry_type', 'business_reference', 'description', 'delta_microcredits', 'journal_balance_microcredits', 'credit_lot_id', 'entitlement_package_id', 'entitlement_component_id', 'component_type', 'created_at'];

export function AdminWalletLedger({ client, language, route, walletID }) {
  const copy = financeCopy[language], words = adminLayoutCopy[language].financeLists;
  const filters = useFinanceReadRoute('ledger', route), signature = `${walletID}:${filters.signature}`;
  const [state, setState] = useState({ signature: '', client: null, page: null, error: null }), [revision, setRevision] = useState(0), [previous, setPrevious] = useState([]);
  useEffect(() => { setPrevious([]); }, [walletID, filters.filterSignature]);
  useEffect(() => {
    const controller = new AbortController(); setState({ signature, client, page: null, error: null });
    void (async () => { requireFinanceID(walletID); const page = await client.walletLedger(walletID, filters.query, controller.signal); requireFinanceCollection(page); if (!controller.signal.aborted) setState({ signature, client, page, error: null }); })().catch(error => { if (!controller.signal.aborted) setState({ signature, client, page: null, error }); });
    return () => controller.abort();
  }, [client, signature, revision]);
  const visible = state.signature === signature && state.client === client ? state : {};
  const refresh = () => visible.error?.status === 410 ? filters.move({ ...filters.query, cursor: '' }, null, 'replace') : setRevision(value => value + 1);
  const cell = (key, value) => value == null ? '—' : key.endsWith('_microcredits') ? formatBalance(value, language) : key === 'created_at' ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : key === 'entry_type' ? words[value] || String(value) : String(value);
  return <section className="adm-wallet-ledger"><h4>{words.ledger} · {walletID}</h4><AdminFinanceFilters kind="ledger" language={language} value={filters.draft} onChange={filters.setDraft} onSubmit={() => { setPrevious([]); filters.move({ ...filters.draft, cursor: '' }); }} /><button onClick={refresh}>{copy.refresh}</button>
    {visible.error && <p role="alert">{visible.error.status === 403 ? copy.permissions : copy.unavailable}<button onClick={refresh}>{copy.retry}</button></p>}{!visible.page && !visible.error && <p role="status">{copy.loading}</p>}
    {visible.page && <><div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(key => <th key={key}>{words[key] || copy[key]}</th>)}</tr></thead><tbody>{visible.page.items.map(item => <tr key={item.posting_id}>{columns.map(key => <td key={key} data-label={words[key] || copy[key]}>{cell(key, item[key])}</td>)}</tr>)}</tbody></table></div>{visible.page.items.length === 0 && <p>{copy.empty}</p>}
      <button disabled={!previous.length} onClick={() => { const values = [...previous], cursor = values.pop(); setPrevious(values); filters.move({ ...filters.query, cursor }); }}>{copy.previous}</button><button disabled={!visible.page.next_cursor} onClick={() => { setPrevious(values => [...values, filters.query.cursor]); filters.move({ ...filters.query, cursor: visible.page.next_cursor }); }}>{copy.next}</button>
    </>}
  </section>;
}
