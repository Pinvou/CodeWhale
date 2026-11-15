import React from 'react';
import './admin-pricing-action-impact.css';

export function PricingActionImpact({ action, prices, formulas, labels }) {
  const words = labels.riskPresentation;
  const { item } = action;
  if (action.type === 'DEACTIVATE') {
    const id = item.unit_definition_id;
    const linkedPrices = prices.filter(price => price.units?.some(unit => unit.unit_definition_id === id));
    const linkedFormulas = formulas.filter(formula => Object.values(formula.required_unit_versions || {}).includes(id));
    return <div className="adm-pricing-impact">
      <dl><div><dt>{labels.unitCode}</dt><dd>{item.unit_code} · v{item.version}</dd></div><div><dt>{words.currentStatus}</dt><dd>{item.status}</dd></div></dl>
      <p>{words.deactivateImpact}</p>
      <h3>{words.knownReferences}</h3>
      <p>{words.referencesBoundary}</p>
      {!linkedPrices.length && !linkedFormulas.length ? <p>{words.noneConfirmed}</p> : <>
        <p>{words.visibleTotals(linkedPrices.length, linkedFormulas.length)}</p>
        <ul>{linkedPrices.slice(0, 3).map(price => <li key={price.price_version_id}>
          <strong>{labels.priceVersions} · v{price.version} · {price.derived_status}</strong> <code>{price.price_version_id}</code>
        </li>)}{linkedFormulas.slice(0, 3).map(formula => <li key={formula.formula_version_id}>
          <strong>{labels.protocolFormula} · {formula.protocol_code} v{formula.version} · {formula.status}</strong> <code>{formula.formula_version_id}</code>
        </li>)}</ul>
        {(linkedPrices.length > 3 || linkedFormulas.length > 3) && <p>{words.sampleLimit}</p>}
      </>}
    </div>;
  }
  if (action.type !== 'TRANSITION' || item.derived_status !== 'ACTIVE') return null;
  const isModel = item.scope_type === 'MODEL';
  return <div className="adm-pricing-impact">
    <dl><div><dt>{labels.scope}</dt><dd>{isModel ? labels.listPresentation.scopes.MODEL : labels.listPresentation.scopes.GLOBAL} {isModel && <code>{item.scope_id}</code>}</dd></div>
      <div><dt>{labels.protocolFormula}</dt><dd>{item.protocol_code} · <code>{item.formula_version_id}</code></dd></div>
      <div><dt>{labels.version}</dt><dd>v{item.version}</dd></div>
      <div><dt>{words.currentStatus}</dt><dd>{item.derived_status}</dd></div></dl>
    <p>{words.endImpact}</p>
    <p>{words.modeHelp[action.mode]}</p>
  </div>;
}
