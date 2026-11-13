import React, { useEffect, useState } from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import { formatBalance } from './balance-view.js';
import { integerMoney } from './finance-contract.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { AdminSystemStatusWorkspace } from './AdminSystemStatusWorkspace.jsx';

const definitions = {
  account: [['users', 'platform.users.read'], ['active_users', 'platform.users.read'], ['disabled_users', 'platform.users.read'], ['organizations', 'platform.organizations.read'], ['active_memberships', 'platform.organizations.read'], ['pending_invitations', 'platform.organizations.read']],
  billing: [['wallets', 'platform.billing.wallets.read'], ['posted_microcredits', 'platform.billing.wallets.read'], ['reserved_microcredits', 'platform.billing.wallets.read'], ['receivable_microcredits', 'platform.billing.wallets.read'], ['pending_payment_orders', 'platform.billing.wallets.read'], ['review_payment_orders', 'platform.billing.wallets.read'], ['pending_adjustments', 'platform.billing.wallets.read']],
  token: [['token_accounts', 'platform.tokens.read'], ['active_accounts', 'platform.tokens.read'], ['active_credentials', 'platform.tokens.read'], ['routes', 'platform.tokens.read'], ['healthy_routes', 'platform.tokens.read'], ['usage_requests', 'platform.usage.read'], ['review_usage_requests', 'platform.usage.read'], ['reconciliation_pending', ['platform.usage.read', 'platform.system.status.read']], ['reconciliation_stopped', ['platform.usage.read', 'platform.system.status.read']]],
};
function DomainOverview({ domain, client, copy, language, permissions, navigation }) {
  const fields = definitions[domain].filter(([, permission]) => (Array.isArray(permission) ? permission.some(value => permissions.includes(value)) : permissions.includes(permission))).map(([key]) => key);
  const signature = fields.join(',');
  const [state, setState] = useState({ owner: null, signature: '', value: null, error: null, updatedAt: null }), [revision, setRevision] = useState(0);
  const words = adminLayoutCopy[language];
  useEffect(() => {
    const controller = new AbortController();
    setState({ owner: client, signature, value: null, error: null });
    if (fields.length) void client.request(domain, '/admin/v1/overview', controller.signal).then(value => {
      for (const key of fields) { const number = integerMoney(value?.[key]); if (!key.endsWith('_microcredits') && number < 0n) throw new Error('INVALID_OVERVIEW'); }
      if (!controller.signal.aborted) setState({ owner: client, signature, value, error: null, updatedAt: Date.now() });
    }).catch(error => { if (!controller.signal.aborted) setState({ owner: client, signature, value: null, error }); });
    return () => controller.abort();
  }, [client, domain, signature, revision]); // fields are exactly represented by signature.
  const current = state.owner === client && state.signature === signature ? state : { value: null, error: null };
  const targets = { account: ['users', 'organizations'], billing: ['billing', 'payments'], token: ['tokens', 'models', 'tokenResources', 'modelOperations'] }[domain].filter(key => navigation.available.includes(key));
  if (!targets.length && !fields.length) return null;
  return <article className="adm-panel" data-overview-domain={domain}><header><h3>{words.domains[domain]}</h3><button type="button" disabled={fields.length === 0} onClick={() => setRevision(value => value + 1)}>{copy.refresh}</button></header>{current.updatedAt && <small className="adm-muted">{new Date(current.updatedAt).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language])}</small>}
    {!fields.length && <p className="adm-muted">{words.readOnly}</p>}
    {current.error && <p role="alert">{localizedAccountPlatformError(copy, current.error)}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}
    {fields.length > 0 && !current.value && !current.error && <p role="status">{copy.loading}</p>}
    {current.value && <dl>{fields.map(key => <React.Fragment key={key}><dt>{words.metrics[key]}</dt><dd>{key.endsWith('_microcredits') ? formatBalance(current.value[key], language) : integerMoney(current.value[key]).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language])}</dd></React.Fragment>)}</dl>}
    <nav>{targets.map(key => <button key={key} onClick={() => navigation.navigate(key)}>{words.nav[key]}</button>)}</nav>
  </article>;
}
export function AdminOverviewWorkspace(props) {
  return <section><div className="adm-overview-grid">{Object.keys(definitions).map(domain => <DomainOverview key={domain} domain={domain} {...props} />)}</div>
    {props.permissions.includes('platform.system.status.read') && <AdminSystemStatusWorkspace client={props.client} copy={props.copy} language={props.language} permissions={props.permissions} onOpenReconciliation={props.onOpenReconciliation} compact />}
  </section>;
}
