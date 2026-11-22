export function canOpenPersonalRecharge(userError) {
  return walletRechargeTarget(userError)?.contextType === 'PERSONAL';
}

export function walletRechargeTarget(userError) {
  if (userError?.kind !== 'wallet_insufficient') return null;
  if (userError.contextType === 'PERSONAL' && userError.action === 'OPEN_RECHARGE') {
    return { contextType: 'PERSONAL', view: 'recharge' };
  }
  const organizationId = String(userError.organizationId || '').trim().toLowerCase();
  if (userError.contextType === 'ORGANIZATION' && userError.action === 'OPEN_ORGANIZATION_RECHARGE'
    && (!userError.errorCode || userError.errorCode === 'WALLET_INSUFFICIENT')
    && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(organizationId)) {
    return { contextType: 'ORGANIZATION', organizationId, view: 'organizationRecharge' };
  }
  return null;
}
