import React from 'react';
import { formatFen, integerMoney } from './finance-contract.js';

const supportedLocales = ['zh_cn', 'en', 'ja'];
const formatLocale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' };
const text = value => typeof value === 'string' ? value.trim() : '';
const sourceInteger = value => {
  if (!['number', 'string'].includes(typeof value)) return null;
  try { return integerMoney(value); } catch { return null; }
};

export function catalogDisplayName(item, language) {
  const preferred = language === 'zh' ? 'zh_cn' : language;
  for (const key of new Set([preferred, ...supportedLocales])) {
    const name = text(item?.['display_name_' + key]);
    if (name) return name;
  }
  return '';
}

function ProductAmount({ mapping, language, copy }) {
  const amount = sourceInteger(mapping?.amount_fen), currency = text(mapping?.currency);
  if (amount === null) return copy.catalogValueMissing;
  if (!currency) return <><code dir="ltr">{amount.toString()}</code>{' '}{copy.amountFen}{' · '}{copy.catalogCurrencyMissing}</>;
  return <bdi dir="ltr" className="adm-product-number">{currency} {formatFen(amount, language)}</bdi>;
}

function ProductPoints({ item, language, copy }) {
  const locale = formatLocale[language] || 'en-US';
  const value = points => sourceInteger(points)?.toLocaleString(locale) ?? copy.catalogValueMissing;
  return <dl className="adm-product-points">
    <div><dt>{copy.base}</dt><dd>{value(item.base_points)}</dd></div>
    <div><dt>{copy.bonus}</dt><dd>{value(item.bonus_points)}</dd></div>
  </dl>;
}

export function AdminProductCatalogTable({ page, language, copy, disabled, onOpen }) {
  // biome-ignore lint/a11y/noNoninteractiveTabindex: This named region provides keyboard access to the wide table horizontal scroll.
  return <div className="adm-product-table-scroll" role="region" aria-label={copy.productAdmin} tabIndex={0}>
    <table className="adm-product-catalog-table">
      <thead><tr><th scope="col">{copy.name}</th><th scope="col">{copy.catalogConfigAmount}</th><th scope="col">{copy.catalogEntitlement}</th><th scope="col">{copy.validityDays}</th><th scope="col">{copy.status}</th><th scope="col">{copy.actions}</th></tr></thead>
      <tbody>{page.items.map(item => {
        const name = catalogDisplayName(item, language), id = text(item.product_id);
        const state = text(item.lifecycle_status);
        const status = copy.catalogLifecycle && Object.prototype.hasOwnProperty.call(copy.catalogLifecycle, state) ? copy.catalogLifecycle[state] : state || copy.catalogValueMissing;
        return <tr key={item.catalog_item_id}>
          <th scope="row"><div className="adm-product-identity"><bdi>{name || copy.catalogNameMissing}</bdi><code dir="ltr">{id || copy.catalogValueMissing}</code></div></th>
          <td><ProductAmount mapping={item.mapping} language={language} copy={copy} /></td>
          <td><ProductPoints item={item} language={language} copy={copy} /></td>
          <td>{Number.isInteger(item.validity_days) && item.validity_days >= 0 ? item.validity_days : copy.catalogValueMissing}</td>
          <td><div className="adm-product-lifecycle"><span className="adm-product-state" data-state={item.lifecycle_status}>{status}</span><small>{copy.version}: {item.version ?? copy.catalogValueMissing}</small></div></td>
          <td><button type="button" disabled={disabled} aria-label={[copy.details, name, id].filter(Boolean).join(' · ')} onClick={() => void onOpen(item)}>{copy.details}</button></td>
        </tr>;
      })}</tbody>
    </table>
  </div>;
}
