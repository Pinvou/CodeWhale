import React, { useState } from 'react';
import { accountLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { PortalDialog } from './PortalDialog.jsx';
import { formatBalance, formatWholeBalance } from './balance-view.js';
import { integerMoney } from './finance-contract.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';

export function PortalBenefits({ items, language, now }) {
  const copy = financeCopy[language], words = accountLayoutCopy[language];
  const [dialog, setDialog] = useState(null);
  const status = value => words.statuses[value] || value;
  const date = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—';
  const active = items.filter(item => item.status === 'ACTIVE' && integerMoney(item.points_remaining_microcredits) > 0n && (!item.valid_until && !item.expires_at || Date.parse(item.valid_until || item.expires_at) > now))
    .sort((a, b) => Date.parse(a.valid_until || a.expires_at || '9999-01-01') - Date.parse(b.valid_until || b.expires_at || '9999-01-01') || a.package_id.localeCompare(b.package_id));
  const remaining = active.reduce((sum, item) => sum + integerMoney(item.points_remaining_microcredits), 0n);
  const card = item => {
    let progress = null;
    try { const used = integerMoney(item.points_used_microcredits), total = used + integerMoney(item.points_remaining_microcredits); if (total > 0n) progress = Math.max(0, Math.min(100, Number(used * 10000n / total) / 100)); } catch { /* Missing optional display facts remain unavailable. */ }
    return <article className="ap-benefit-card" key={item.package_id}><header><strong>{localizedProductDisplayName(item, language, 'plan_id')}</strong><span>{status(item.status)}</span></header>
      <p><strong>{formatWholeBalance(item.points_remaining_microcredits, language)}</strong> <span>{words.remaining}</span></p>
      {progress !== null && <div className="ap-benefit-progress" role="progressbar" aria-label={copy.used} aria-valuenow={progress} aria-valuemin={0} aria-valuemax={100}><i style={{ width: `${progress}%` }} /></div>}
      <footer><small>{date(item.valid_until || item.expires_at)}</small><button type="button" onClick={() => setDialog(item.package_id)}>{copy.details}</button></footer>
    </article>;
  };
  const selected = items.find(item => item.package_id === dialog);
  return <section className="ap-benefits"><div className="ap-section-heading"><h3>{copy.packages}</h3><button type="button" onClick={() => setDialog('all')}>{words.allPackages}</button></div><p className="ap-muted">{words.packagesSummary(active.length, formatWholeBalance(remaining.toString(), language))}</p>
    {!active.length && <p>{copy.empty}</p>}<div className="ap-benefit-grid">{active.slice(0, 5).map(card)}</div>
    {dialog === 'all' && <PortalDialog title={copy.packages} closeLabel={words.closeDialog} onClose={() => setDialog(null)}><div className="ap-benefit-grid">{items.map(card)}</div>{!items.length && <p>{copy.empty}</p>}</PortalDialog>}
    {selected && <PortalDialog title={words.packageDetails} closeLabel={words.closeDialog} onClose={() => setDialog(null)}><h3>{localizedProductDisplayName(selected, language, 'plan_id')}</h3><dl><dt>{copy.remaining}</dt><dd>{formatBalance(selected.points_remaining_microcredits, language)}</dd><dt>{copy.used}</dt><dd>{formatBalance(selected.points_used_microcredits, language)}</dd><dt>{copy.validUntil}</dt><dd>{date(selected.valid_until || selected.expires_at)}</dd><dt>{copy.status}</dt><dd>{status(selected.status)}</dd></dl></PortalDialog>}
  </section>;
}
