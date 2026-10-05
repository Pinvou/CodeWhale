// A view change cancels reads and fences late replies. A submitted mutation
// stays locked until it settles: changing views does not cancel server work.
export function createAdminMutationOwner() {
  const pending = new Map();
  const listeners = new Set();
  const notify = (area, completed) => { for (const listener of listeners) listener({ area, completed }); };
  return {
    begin(area) {
      if (pending.has(area)) return null;
      const owner = {};
      pending.set(area, owner); notify(area, false);
      return owner;
    },
    finish(area, owner) {
      if (pending.get(area) !== owner) return;
      pending.delete(area); notify(area, true);
    },
    isBusy(area) { return pending.has(area); },
    subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); },
  };
}

export function createAdminOperationScope(mutationOwner = createAdminMutationOwner(), area = 'default') {
  let generation = 0;
  let active = null;
  return {
    begin(mutation = false) {
      if (active || mutationOwner.isBusy(area)) return null;
      const ticket = { generation, mutation, controller: new AbortController() };
      if (mutation) {
        ticket.owner = mutationOwner.begin(area);
        if (!ticket.owner) return null;
      }
      active = ticket;
      return ticket;
    },
    isCurrent(ticket) { return ticket.generation === generation && !ticket.controller.signal.aborted; },
    finish(ticket) {
      if (active === ticket) active = null;
      if (ticket.mutation) mutationOwner.finish(area, ticket.owner);
    },
    invalidate() {
      generation += 1;
      if (active && !active.mutation) { active.controller.abort(); active = null; }
    },
    get busy() { return active !== null || mutationOwner.isBusy(area); },
  };
}
