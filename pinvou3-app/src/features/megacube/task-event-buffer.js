/** Bounded event-time snapshots bridge shell startup and the deferred controller. */
export function createTaskEventBuffer(limit = 50) {
  const pending = [];
  let consumer = null;
  return {
    publish(method, payload, snapshot) {
      if (consumer) consumer(method, payload, snapshot);
      else {
        if (pending.length >= limit) pending.shift();
        pending.push([method, payload, snapshot]);
      }
    },
    subscribe(next) {
      consumer = next;
      for (const args of pending.splice(0)) next(...args);
      return () => { if (consumer === next) consumer = null; };
    },
  };
}
