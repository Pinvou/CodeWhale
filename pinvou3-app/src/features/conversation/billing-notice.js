export function canOpenPersonalRecharge(userError) {
  return walletRechargeTarget(userError)?.contextType === 'PERSONAL';
}

export function walletRechargeTarget(userError) {
  if (userError?.kind !== 'wallet_insufficient') return null;
  if (userError.contextType === 'PERSONAL' && userError.action === 'OPEN_RECHARGE') {
    return { contextType: 'PERSONAL', view: 'recharge' };
  }
  return null;
}
