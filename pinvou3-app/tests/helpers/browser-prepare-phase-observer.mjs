// Test-only, opt-in observation. Values are sampled states, never phase durations.
const PHASES = Object.freeze(['starting', 'idle', 'scan', 'read', 'write', 'control', 'cdp', 'failed', 'exists', 'directory', 'audit', 'native-readdir', 'unknown']);
const PHASE_SET = new Set(PHASES);
const LIMIT = 32;
const INTERVAL_MS = 100;
const MAX_NUMBER = 1_000_000_000;
const DISABLED = Object.freeze({ stop() {}, snapshot: () => ({ enabled: false }) });

export function observeBrowserPreparePhases(host, { enabled = false } = {}) {
  if (enabled !== true) return DISABLED;
  let active = true;
  let timer;
  let started;
  let samples = 0;
  let invalidSamples = 0;
  let droppedChanges = 0;
  let lastPhase;
  const changes = [];
  const maxima = Object.fromEntries(PHASES.map(phase => [phase, null]));
  const bounded = value => Number.isSafeInteger(value) && value >= 0
    ? Math.min(value, MAX_NUMBER) : 0;
  const sample = () => {
    if (!active) return;
    try {
      const current = host.diagnostic();
      const phase = PHASE_SET.has(current?.phase) ? current.phase : 'unknown';
      const elapsed = performance.now() - started;
      const frame = {
        phase,
        elapsed_ms: Number.isFinite(elapsed) ? bounded(Math.max(0, Math.round(elapsed))) : 0,
        seq: bounded(current?.sequence),
        observed_age_ms: bounded(current?.ageMs),
      };
      if (typeof current?.exited === 'boolean') frame.exited = current.exited;
      samples = bounded(samples + 1);
      maxima[phase] = Math.max(maxima[phase] ?? 0, frame.observed_age_ms);
      if (phase !== lastPhase) {
        lastPhase = phase;
        if (changes.length === LIMIT) { changes.shift(); droppedChanges = bounded(droppedChanges + 1); }
        changes.push(frame);
      }
    } catch { invalidSamples = bounded(invalidSamples + 1); }
  };
  try {
    started = performance.now();
    sample();
    timer = setInterval(sample, INTERVAL_MS);
    timer.unref?.();
  } catch {
    active = false;
    if (timer !== undefined) clearInterval(timer);
    return DISABLED;
  }
  return Object.freeze({
    stop() {
      if (!active) return;
      sample();
      active = false;
      try { clearInterval(timer); } catch { /* no diagnostic may replace the primary result */ }
    },
    snapshot() {
      return {
        enabled: true,
        interval_ms: INTERVAL_MS,
        max_changes: LIMIT,
        samples,
        invalid_samples: invalidSamples,
        dropped_changes: droppedChanges,
        changes: changes.map(frame => ({ ...frame })),
        observed_max_age_ms: { ...maxima },
      };
    },
  });
}
