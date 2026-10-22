// Index only report-owned immutable snapshots, never callers' mutable raw logs.
// Named views keep unrelated remote projections out of local-only metric scans.
const views = new WeakMap();

export function prepareBehaviorEventView(events) {
  if (!views.has(events)) views.set(events, { names: null, windows: new Map(), endDay: null });
  return events;
}

export const isBehaviorEventView = events => views.has(events);

export function namedBehaviorEvents(events, name) {
  const view = views.get(events);
  if (!view) return events.filter(event => event.event_name === name);
  if (!view.names) {
    view.names = new Map();
    for (const event of events) {
      let rows = view.names.get(event.event_name);
      if (!rows) view.names.set(event.event_name, rows = []);
      rows.push(event);
    }
  }
  return prepareBehaviorEventView(view.names.get(name) || []);
}

export function windowedBehaviorEvents(events, window, statsDay) {
  const view = views.get(events);
  if (view) {
    // Date windows are calendar-day based. Do not cache wall-clock-dependent
    // receipt, maturity or hourly results; discard old-day window references.
    if (view.endDay !== window.endDay) {
      view.windows.clear();
      view.endDay = window.endDay;
    }
    const cached = view.windows.get(window.startDay);
    if (cached) return cached;
  }
  let result = events.filter(event => {
    const day = statsDay(event.occurred_at);
    return day >= window.startDay && day <= window.endDay;
  });
  if (view) {
    // Reuse the immutable source when the whole snapshot is in range. This
    // also shares its named index instead of retaining an identical second one.
    if (result.length === events.length) result = events;
    // Public range keys normalize to today/7/30; keep custom internal uses
    // bounded too, rather than retaining every requested historical window.
    if (view.windows.size >= 3) view.windows.delete(view.windows.keys().next().value);
    prepareBehaviorEventView(result);
    view.windows.set(window.startDay, result);
  }
  return result;
}
