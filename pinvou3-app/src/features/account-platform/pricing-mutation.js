// Replaying an uncertain operation must keep both its identity and its facts.
// An empty read projection cannot prove that a delayed write did not commit.
export function replayPricingMutation(client, operation) {
  if (!operation?.idempotencyKey || operation.phase !== 'DISPATCHED') {
    throw Object.assign(new Error('pricing operation identity is unavailable'), { code: 'COMMAND_RESULT_UNKNOWN' });
  }
  const { input, targetID, idempotencyKey } = operation;
  switch (operation.kind) {
    case 'DRAFT_CREATE': return client.createPriceDraft(input, idempotencyKey);
    case 'DRAFT_UPDATE': return client.updatePriceDraft(targetID, input, idempotencyKey);
    case 'UNIT_CREATE': return client.createMeteringUnit(input, idempotencyKey);
    case 'UNIT_DEACTIVATE': return client.deactivateMeteringUnit(targetID, idempotencyKey);
    case 'PRICE_PUBLISH': return client.publishPrice(targetID, input.expected_etag, idempotencyKey);
    case 'PRICE_TRANSITION': return client.transitionPrice(targetID, input, idempotencyKey);
    default: throw Object.assign(new Error('pricing operation is unavailable'), { code: 'COMMAND_RESULT_UNKNOWN' });
  }
}

export function pricingUnknownStorageKey(owner, sessionEpoch) {
  return `pinvou.admin.pricing-unknown-mutation:${encodeURIComponent(owner)}:${sessionEpoch || 0}`;
}
