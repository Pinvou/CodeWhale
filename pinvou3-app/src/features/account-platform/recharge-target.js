import { personalSurfaceAccess } from './authorization-ui.js';

export function rechargeTargetAllowed(account, target) {
  if (target?.contextType === 'PERSONAL' && target?.view === 'recharge') {
    return !account?.active_context?.organization_id && personalSurfaceAccess(account).rechargeCreate;
  }
  if (target?.contextType !== 'ORGANIZATION' || target?.view !== 'organizationRecharge') return false;
  const active = account?.active_context || {};
  return Boolean(target.organizationId)
    && active.organization_id === target.organizationId
    && new Set(active.permissions || []).has('org.entitlements.purchase');
}

export async function loadRechargeTarget(client, account, target, signal) {
  if (!rechargeTargetAllowed(account, target)) throw new Error('PERMISSION_DENIED');
  const productsPromise = client.products(signal);
  if (target.contextType === 'ORGANIZATION') {
    const canReadBalance = new Set(account?.active_context?.permissions || []).has('org.wallet.read');
    const [products, balance] = await Promise.all([
      productsPromise,
      canReadBalance ? client.organizationBalance(target.organizationId, signal) : Promise.resolve(null),
    ]);
    return { products, balance };
  }
  const [products, balance] = await Promise.all([productsPromise, client.balance(signal)]);
  return { products, balance };
}

export function createRechargeForTarget(client, account, target, productId, channel, key) {
  if (!rechargeTargetAllowed(account, target)) return Promise.reject(new Error('PERMISSION_DENIED'));
  if (target?.contextType === 'ORGANIZATION') {
    return client.purchaseOrganizationEntitlements(target.organizationId, productId, channel, key);
  }
  return client.createRecharge(productId, channel, key);
}

export function rechargeOrderForTarget(client, account, target, orderId, signal) {
  if (!rechargeTargetAllowed(account, target)) return Promise.reject(new Error('PERMISSION_DENIED'));
  if (target?.contextType === 'ORGANIZATION') {
    return client.organizationRechargeOrder(target.organizationId, orderId, signal);
  }
  return client.rechargeOrder(orderId, signal);
}

export function rechargeBalanceForTarget(client, account, target, signal) {
  if (!rechargeTargetAllowed(account, target)) return Promise.reject(new Error('PERMISSION_DENIED'));
  if (target?.contextType === 'ORGANIZATION') {
    if (!new Set(account?.active_context?.permissions || []).has('org.wallet.read')) return Promise.resolve(null);
    return client.organizationBalance(target.organizationId, signal);
  }
  return client.balance(signal);
}
