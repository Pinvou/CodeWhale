import React, { useId } from 'react';
import { organizationID } from './organization-client.js';
import { organizationLabel } from './organization-labels.js';

export function OrganizationAdminLifecycle({ current, ownerMember, ownerReadFailed = false, can, action, copy, words }) {
  const heading = useId();
  const status = ['ACTIVE', 'DISABLED', 'DELETED'].includes(current?.status) ? current.status : '';
  const active = status === 'ACTIVE', disabled = status === 'DISABLED';
  const canManageStatus = can('status.manage'), canTransfer = can('owner.transfer');
  const statusCanStart = canManageStatus && (active || disabled);
  const statusCondition = !canManageStatus ? words.noStatusPermission : disabled ? words.enableCondition : active ? words.statusCondition : words.statusUnavailable;
  const transferCondition = !canTransfer ? words.noTransferPermission : active ? words.transferCondition : disabled ? words.transferDisabled : words.transferUnavailable;
  return <div className="adm-organization-lifecycle">
    <section className="adm-organization-lifecycle-context" aria-labelledby={heading + '-context'}>
      <div><h4 id={heading + '-context'}>{words.contextTitle}</h4>
        <dl>
          <dt>{words.currentStatus}</dt><dd>{status ? organizationLabel(copy, 'Status', status) : words.unknownStatus}</dd>
          <dt>{words.contextOwner}</dt><dd>{ownerMember ? <>
            {(ownerMember.display_name || ownerMember.user_code) && <bdi>{ownerMember.display_name || ownerMember.user_code}</bdi>}<code dir="ltr">{ownerMember.user_id}</code>
          </> : ownerReadFailed ? words.contextOwnerReadFailed : words.contextOwnerUnverified}</dd>
        </dl>
      </div>
      <p>{words.contextHistory}</p>
    </section>
    <div className="adm-organization-lifecycle-tasks">
      <section className="adm-organization-lifecycle-task" aria-labelledby={heading + '-status'}>
        <h4 id={heading + '-status'}>{words.statusTitle}</h4>
        <p>{words.statusPurpose}</p>
        <dl>
          <dt>{words.conditions}</dt><dd>{statusCondition}</dd>
          {(active || disabled) && <>
          <dt>{words.effect}</dt><dd>{active ? words.disableEffect : words.enableEffect}</dd>
          <dt>{words.usageEffect}</dt><dd>{active ? words.disableUsageEffect : words.enableUsageEffect}</dd>
        </>}
        </dl>
        {statusCanStart && <div className="adm-organization-lifecycle-action">
          {action('status', active ? copy.disable : copy.enable, [], () => ({ status: active ? 'DISABLED' : 'ACTIVE' }), 'status', '', true, active ? { confirmationContext: [{ name: 'currentOrganizationStatus', label: words.currentStatus, value: organizationLabel(copy, 'Status', status) }, { name: 'organizationPaymentImpact', label: words.disablePaymentLabel, value: words.disablePaymentEffect }, { name: 'organizationUsageImpact', label: words.usageEffect, value: words.disableUsageEffect }] } : undefined)}
        </div>}
      </section>
      <section className="adm-organization-lifecycle-task" aria-labelledby={heading + '-transfer'}>
        <h4 id={heading + '-transfer'}>{copy.transfer}</h4>
        <p>{words.transferPurpose}</p>
        <dl>
          <dt>{words.conditions}</dt><dd>{transferCondition}</dd>
          <dt>{words.effect}</dt><dd>{words.transferEffect}</dd>
        </dl>
        {canTransfer && status !== 'DELETED' && <div className="adm-organization-lifecycle-action">
          {/* The existing form stays mounted for recovery even when a new transfer is unavailable. */}
          {action('transfer', copy.transfer, [{ name: 'newOwner', lookup: true }], values => ({ new_owner_user_id: organizationID(values.newOwner) }), 'transfer', '', true, { disabled: !active })}
        </div>}
      </section>
    </div>
  </div>;
}
