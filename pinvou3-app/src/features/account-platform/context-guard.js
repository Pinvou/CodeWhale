export function createContextRequestGuard() {
  let generation = 0;
  let controller = null;
  return {
    begin() {
      generation += 1;
      if (controller) controller.abort();
      controller = new globalThis.AbortController();
      return { generation, signal: controller.signal };
    },
    isCurrent(candidate) {
      return candidate === generation;
    },
    cancel() {
      generation += 1;
      if (controller) controller.abort();
      controller = null;
    },
  };
}

export function rechargePollDelay(attempt, random = Math.random) {
  const base = Math.min(5000, 2000 + Math.max(0, attempt) * 500);
  return Math.round(base * (0.9 + random() * 0.2));
}

export function eligibleForContext(items, organizationId) {
  return (items || []).filter(item => organizationId ? item.owner_type !== 'USER' : item.owner_type === 'USER');
}
