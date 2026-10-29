import React from 'react';
import { organizationCopy, accountPlatformCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance, formatWholeBalance } from './balance-view.js';
import { integerMoney } from './finance-contract.js';
import { PortalBenefits } from './PortalBenefits.jsx';

export function OrganizationCreditOverview({ me, data, language, ready, error, now }) {
  const copy = organizationCopy[language], account = accountPlatformCopy[language];
  const wallet = hasPermission(me, 'org.wallet.read');
  const quota = hasPermission(me, 'org.quota.read.self');
  const own = quota ? data.grants.filter(item => item.member_user_id === me.user_id && item.status === 'ACTIVE') : [];
  const available = wallet ? data.balance?.available_microcredits : own.reduce((sum, item) => sum + integerMoney(item.available_microcredits), 0n).toString();
  const reserved = wallet ? data.balance?.reserved_microcredits : own.reduce((sum, item) => sum + integerMoney(item.reserved_microcredits), 0n).toString();
  if (!ready) return error ? null : <p role="status">{copy.loading}</p>;
  return <>
    {(wallet || quota) && <section className="ap-balance-hero"><span>{wallet ? copy.balance : copy.quota}</span><strong>{formatWholeBalance(available, language)}</strong><small>{account.reservedCredits}: {formatBalance(reserved, language)}</small></section>}
    {wallet && <PortalBenefits items={data.packages} language={language} now={now} />}
  </>;
}
