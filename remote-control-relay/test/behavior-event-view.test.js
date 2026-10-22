import assert from 'node:assert/strict';
import test from 'node:test';
import { prepareBehaviorEventView, namedBehaviorEvents, windowedBehaviorEvents } from '../behavior-event-view.js';

test('named report views reuse their index without rescanning unrelated event types', () => {
  let reads = 0;
  const unrelated = Array.from({ length: 1000 }, () => ({ get event_name() { reads++; return 'task_finished'; } }));
  const voice = { event_name: 'voice_started' };
  const view = prepareBehaviorEventView([...unrelated, voice]);
  assert.deepEqual(namedBehaviorEvents(view, 'voice_started'), [voice]);
  reads = 0;
  assert.deepEqual(namedBehaviorEvents(view, 'voice_started'), [voice]);
  assert.deepEqual(namedBehaviorEvents(view, 'analytics_cohort_started'), []);
  assert.equal(reads, 0);
});

test('raw caller arrays remain uncached after replacement, mutation and append', () => {
  const rows = [{ event_name: 'task_finished', occurred_at: 3 }];
  const window = { startDay: 1, endDay: 3 };
  assert.equal(windowedBehaviorEvents(rows, window, Number).length, 1);
  assert.equal(namedBehaviorEvents(rows, 'task_finished').length, 1);
  rows[0].occurred_at = 4;
  rows[0].event_name = 'voice_started';
  assert.equal(windowedBehaviorEvents(rows, window, Number).length, 0);
  assert.equal(namedBehaviorEvents(rows, 'task_finished').length, 0);
  rows[0] = { event_name: 'task_finished', occurred_at: 2 };
  rows.push({ event_name: 'task_finished', occurred_at: 1 });
  assert.equal(windowedBehaviorEvents(rows, window, Number).length, 2);
  assert.equal(namedBehaviorEvents(rows, 'task_finished').length, 2);
});

test('calendar windows preserve order, roll over dates and bound retained views', () => {
  const rows = prepareBehaviorEventView([4, 1, 3, 2].map(occurred_at => ({ occurred_at })));
  assert.equal(windowedBehaviorEvents(rows, { startDay: 1, endDay: 4 }, Number), rows,
    'a full calendar selection should reuse its immutable source and named index');
  const first = windowedBehaviorEvents(rows, { startDay: 1, endDay: 3 }, Number);
  assert.deepEqual(first.map(row => row.occurred_at), [1, 3, 2]);
  assert.equal(windowedBehaviorEvents(rows, { startDay: 1, endDay: 3 }, Number), first);
  for (const startDay of [0, -1, -2]) windowedBehaviorEvents(rows, { startDay, endDay: 3 }, Number);
  assert.notEqual(windowedBehaviorEvents(rows, { startDay: 1, endDay: 3 }, Number), first);
  assert.deepEqual(windowedBehaviorEvents(rows, { startDay: 4, endDay: 4 }, Number).map(row => row.occurred_at), [4]);
  assert.notEqual(windowedBehaviorEvents(rows, { startDay: 1, endDay: 3 }, Number), first);
});
