import React from 'react';
import { organizationCopy, accountPlatformCopy, accountCenterCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { integerMoney } from './finance-contract.js';
import { PortalBenefits } from './PortalBenefits.jsx';

export function OrganizationCreditOverview({ me, data, language, ready, error, now, onPurchase }) {
  const copy = organizationCopy[language], account = accountPlatformCopy[language];
  const center = accountCenterCopy[language];
  const wallet = hasPermission(me, 'org.wallet.read');
  const quota = hasPermission(me, 'org.quota.read.self');
  const own = quota ? data.grants.filter(item => item.member_user_id === me.user_id && item.status === 'ACTIVE') : [];
  const available = wallet ? data.balance?.available_microcredits : own.reduce((sum, item) => sum + integerMoney(item.available_microcredits), 0n).toString();
  const reserved = wallet ? data.balance?.reserved_microcredits : own.reduce((sum, item) => sum + integerMoney(item.reserved_microcredits), 0n).toString();
  if (!ready) return error ? null : <p role="status">{copy.loading}</p>;
  return <>
    {(wallet || quota) && <section className="ap-balance-hero ap-balance-columns"><div><span>{wallet ? copy.balance : copy.quota}</span><strong>{formatBalance(available, language)}</strong><small>{center.available}</small></div><div className="ap-reserved-summary"><span>{account.reservedCredits}</span><strong>{formatBalance(reserved, language)}</strong><p>{center.reservedHint}</p></div></section>}
    {hasPermission(me, 'org.entitlements.purchase') && onPurchase && <div className="ap-action-group"><button type="button" className="ap-primary" onClick={onPurchase}>{center.buyOrganization}</button></div>}
    {!error && (wallet || quota) && available != null && integerMoney(available) === 0n && !hasPermission(me, 'org.entitlements.purchase') && <p className="ap-empty-state" role="status">{center.contactOrganizationAdmin}</p>}
     {wallet && <PortalBenefits items={data.packages} language={language} now={now} />}
  </>;
}
