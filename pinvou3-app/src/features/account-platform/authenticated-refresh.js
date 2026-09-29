export function createAuthenticatedRefreshCoordinator() {
  let generation = 0;
  let attempted = false;
  let pending = null;

  return {
    reset() {
      generation += 1;
      attempted = false;
      pending = null;
    },
    invalidate() {
      generation += 1;
      attempted = true;
      pending = null;
    },
    run(refresh) {
      if (attempted) return pending || Promise.resolve(false);
      attempted = true;
      const activeGeneration = generation;
      pending = Promise.resolve()
        .then(refresh)
        .then(
          (result) => activeGeneration === generation && result === true,
          () => false,
        )
        .finally(() => {
          if (activeGeneration === generation) pending = null;
        });
      return pending;
    },
  };
}
