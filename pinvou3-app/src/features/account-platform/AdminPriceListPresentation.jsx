import React from 'react';
import { AdminIdentifier } from './AdminIdentifier.jsx';
import './admin-price-list-presentation.css';

const dictionaryValue = (dict, key) => Object.prototype.hasOwnProperty.call(dict, key) ? dict[key] : '';
const locales = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' };
const formatters = new Map();
const text = value => typeof value === 'string' && value.trim() ? value.trim() : '';
const integer = (value) => {
  if (typeof value === 'string' && /^(0|[1-9]\d*)$/.test(value)) return value;
  if (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0) return String(value);
  if (typeof value === 'bigint' && value >= 0n) return String(value);
  return '';
};

export function PricingDate({ value, language, missing }) {
  const date = value ? new Date(value) : null;
  if (!date || !Number.isFinite(date.getTime())) return missing;
  let formatter = formatters.get(language);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(locales[language], {
      timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
    });
    formatters.set(language, formatter);
  }
  return <time dateTime={value}>{formatter.format(date)} <small>UTC+08:00</small></time>;
}

function UnitLabel({ code, version, words }) {
  return <>
    <strong>{dictionaryValue(words.charges, code) || code || words.unitUnknown}</strong>
    {code && dictionaryValue(words.charges, code) && <small>{code}</small>}
    {integer(version) && <small>{words.unitVersion(integer(version))}</small>}
  </>;
}

function ExactPrice({ value, creditUnit, denominator, words }) {
  const price = integer(value);
  const divisor = integer(denominator);
  if (price === '') return words.unavailable;
  return <span className="adm-price-exact">
    <strong>{price}</strong> {creditUnit || words.creditUnknown}
    <small>{divisor && divisor !== '0' ? words.perDenominator(divisor) : words.denominatorUnknown}</small>
  </span>;
}

export function ConfiguredPriceUnits({ item, formula, unitByID, words }) {
  const entries = item.units || [];
  if (!entries.length) return <small>{words.unavailable}</small>;
  return <ul className="adm-price-unit-lines" aria-label={words.configuredColumn}>
    {entries.map(unit => {
      const id = unit.unit_definition_id;
      const definition = unitByID.get(id);
      const code = definition?.unit_code || Object.entries(formula?.required_unit_versions || {}).find(([, value]) => value === id)?.[0];
      return <li key={id}>
        <span><UnitLabel code={code} version={definition?.version} words={words} />
          {!definition && <small className="adm-price-id">{id}</small>}</span>
        <span>{unit.config_type === 'FIXED_PRICE'
          ? <ExactPrice value={unit.fixed_price_per_denominator} creditUnit={item.credit_unit} denominator={definition?.price_denominator} words={words} />
          : unit.config_type === 'INHERIT_GLOBAL' ? <small>{words.inherited}</small> : <small>{words.unavailable}</small>}</span>
      </li>;
    })}
  </ul>;
}

export function PricingIdentifiers({ identifiers, label, target, scope, client, owner, language }) {
  const fields = identifiers.filter(([, value]) => text(value));
  if (!fields.length) return null;
  return <details className="adm-price-identifiers">
    <summary>{label}</summary>
    <dl>{fields.map(([title, value]) => <div key={title}>
      <dt>{title}</dt>
      <dd><AdminIdentifier client={client} owner={owner} routeScope={scope} value={value} label={title} targetLabel={target} language={language} alwaysExpanded /></dd>
    </div>)}</dl>
  </details>;
}

export function ResolvedPriceEvidence({ preview, language, labels }) {
  const words = labels.listPresentation;
  return <>
    <p className="adm-price-evidence-note">{words.previewBoundary}</p>
    <div className="adm-detail-grid">
      <div><small>{labels.scope}</small><strong>{dictionaryValue(words.scopes, preview.scope_type) || preview.scope_type} {preview.model}</strong></div>
      <div><small>{labels.protocolFormula}</small><strong>{preview.protocol_code}</strong><code>{preview.formula_version_id}</code></div>
      <div><small>{labels.resolvedAt}</small><strong><PricingDate value={preview.resolved_at} language={language} missing={words.unavailable} /></strong></div>
    </div>
    <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th scope="col">{labels.unitCode}</th><th scope="col">{labels.configurationType}</th><th scope="col">{labels.unitPrice}</th><th scope="col">{labels.priceSource}</th></tr></thead>
      <tbody>{(preview.units || []).map(unit => <tr key={unit.unit_definition_id}>
        <th scope="row"><UnitLabel code={unit.unit_code} version={unit.unit_version} words={words} /></th>
        <td>{unit.configuration_type === 'INHERIT_GLOBAL' ? words.inheritedConfiguration : unit.configuration_type === 'FIXED_PRICE' ? words.fixedConfiguration : unit.configuration_type || words.unavailable}</td>
        <td><ExactPrice value={unit.resolved_price} creditUnit={preview.credit_unit} denominator={unit.price_denominator} words={words} /></td>
        <td>{dictionaryValue(words.scopes, unit.price_source_scope) || unit.price_source_scope || words.unavailable}<br /><code>{unit.source_price_version_id}</code></td>
      </tr>)}</tbody>
    </table></div>
  </>;
}
