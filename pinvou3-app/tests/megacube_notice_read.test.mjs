import test from 'node:test';
import assert from 'node:assert/strict';
import { cmsNotice, cmsNoticeObservations, isCmsNoticeRead, markCmsNoticeRead, modelIssueNotice,
  noticeReadKey, readCmsNotices, reconcileCmsNotices, saveCmsNotices, splitCmsTasks, taskIssueNotice } from '../src/features/megacube/cms-notice-read.js';

const issue = modelIssueNotice('ornith-9b', 'runtime_missing: container missing');
const status = { degraded: [], activeModel: 'qwen' };
const models = { models: [{ card: 'ornith-9b', status: 'stopped', degradedReason: issue.detail },
  { card: 'qwen', status: 'running', healthy: true }] };
function storage() {
  const values = new Map();
  return { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
}

test('read receipt survives reopening and is isolated by profile/socket recovery scope', () => {
  const s = storage(), a = noticeReadKey('a'.repeat(64)), b = noticeReadKey('b'.repeat(64));
  const read = markCmsNoticeRead([], issue);
  assert.equal(saveCmsNotices(s, a, read), true);
  assert.equal(isCmsNoticeRead(readCmsNotices(s, a), issue), true);
  assert.equal(isCmsNoticeRead(readCmsNotices(s, b), issue), false);
  assert.equal(noticeReadKey(''), null);
  assert.equal(saveCmsNotices(s, null, read), false);
  assert.deepEqual(models.models[0], { card: 'ornith-9b', status: 'stopped', degradedReason: issue.detail });
});

test('polls retain same error, changed reason returns, confirmed recovery rearms recurrence', () => {
  let read = markCmsNoticeRead([], issue);
  const observed = cmsNoticeObservations({ status, models, engines: [] });
  read = reconcileCmsNotices(read, observed);
  assert.equal(isCmsNoticeRead(read, issue), true);
  const changed = modelIssueNotice(issue.id, 'runtime_missing: new deployment removed');
  assert.equal(isCmsNoticeRead(read, changed), false);
  read = reconcileCmsNotices(read, cmsNoticeObservations({ status,
    models: { models: models.models.map(m => ({ ...m, degradedReason: null })) }, engines: [] }));
  assert.equal(isCmsNoticeRead(read, issue), false);
});

test('offline and partial queries do not falsely clear model acknowledgements', () => {
  const read = markCmsNoticeRead([], issue);
  for (const observed of [cmsNoticeObservations({ unavailable: 'socket_missing' }),
    cmsNoticeObservations({ status, models: null, partial: 'models' }), cmsNoticeObservations({})]) {
    assert.deepEqual(reconcileCmsNotices(read, observed), read);
  }
});

test('model health unknown is not a confirmed recovery; genuine health restores notification', () => {
  const unhealthy = cmsNotice('model-health', 'qwen', 'unhealthy'), read = markCmsNoticeRead([], unhealthy);
  const observations = healthy => cmsNoticeObservations({ status,
    models: { models: [{ card: 'qwen', status: 'running', healthy }] } });
  assert.deepEqual(reconcileCmsNotices(read, observations(null)), read);
  assert.deepEqual(reconcileCmsNotices(read, observations(false)), read);
  assert.deepEqual(reconcileCmsNotices(read, observations(true)), []);
});

test('status summary and model detail share one read identity without acknowledging other models', () => {
  const read = markCmsNoticeRead([], issue), another = modelIssueNotice('other', issue.detail);
  const observed = cmsNoticeObservations({ status: { ...status, degraded: [{ card: issue.id, reason: issue.detail }] }, models });
  assert.equal(observed['model-issue'].filter(n => isCmsNoticeRead(read, n)).length, 2);
  assert.equal(isCmsNoticeRead(read, another), false);
});

test('blocked/corrupt storage is reported and read records stay bounded', () => {
  const blocked = { getItem() { throw Error('blocked'); }, setItem() { throw Error('blocked'); } };
  const key = noticeReadKey('a'.repeat(64));
  assert.equal(readCmsNotices(blocked, key), null);
  assert.equal(saveCmsNotices(blocked, key, [issue]), false);
  const s = storage(); s.setItem(key, '{broken');
  assert.equal(readCmsNotices(s, key), null);
  let read = [];
  for (let i = 0; i < 200; i++) read = markCmsNoticeRead(read, modelIssueNotice(`model-${i}`, 'fault'));
  assert.equal(read.length, 128);
  assert.equal(saveCmsNotices(s, key, read), true);
  assert.equal(saveCmsNotices(s, key, [modelIssueNotice('oversized', 'x'.repeat(16385))]), false);
});

const failedTask = { id: 'run-1', source: 'cmsd', status: 'failed', error: 'deployment failed', created_at: 100 };
test('acknowledged failures move to history while active tasks remain visible', () => {
  const receipt = taskIssueNotice(failedTask), read = markCmsNoticeRead([], receipt);
  const active = { ...failedTask, status: 'running' }, success = { id: 'done', status: 'succeeded' };
  const cancelled = { id: 'cancel', status: 'cancelled' }, unknown = { id: 'uncertain', status: 'unknown' };
  const result = splitCmsTasks([failedTask, active, success, cancelled, unknown], n => isCmsNoticeRead(read, n));
  assert.deepEqual(result.attention, [active, unknown]);
  assert.deepEqual(result.history, [failedTask, success, cancelled]);
  assert.equal(failedTask.status, 'failed');
  assert.deepEqual(splitCmsTasks([failedTask], () => false).attention, [failedTask]);
});

test('task source, attempt generation and reason prevent unrelated suppression', () => {
  const read = markCmsNoticeRead([], taskIssueNotice(failedTask));
  for (const task of [{ ...failedTask, source: 'store' }, { ...failedTask, error: 'new failure' },
    { ...failedTask, created_at: 101 }, { ...failedTask, id: 'run-2' }]) {
    assert.equal(isCmsNoticeRead(read, taskIssueNotice(task)), false);
  }
  assert.equal(isCmsNoticeRead(read, taskIssueNotice({ ...failedTask, updated_at: 900, duration: 50 })), true);
});

test('task receipts survive truncated lists and query failure, but a confirmed retry rearms failures', () => {
  const read = markCmsNoticeRead([], taskIssueNotice(failedTask));
  for (const snapshot of [{ tasks: [] }, { tasks: null }, { tasks: [{ ...failedTask, id: 'another' }] }]) {
    const groups = cmsNoticeObservations({ status, ...snapshot });
    assert.deepEqual(reconcileCmsNotices(read, groups), read);
  }
  assert.deepEqual(reconcileCmsNotices(read, cmsNoticeObservations({ unavailable: 'offline' })), read);
  assert.deepEqual(reconcileCmsNotices(read, cmsNoticeObservations({ status, tasks: [failedTask] })), read);
  assert.deepEqual(reconcileCmsNotices(read, cmsNoticeObservations({ status,
    tasks: [{ ...failedTask, status: 'running' }] })), []);
});

test('acknowledging an active or unknown task warning never hides the task itself', () => {
  for (const taskStatus of ['running', 'queued', 'unknown']) {
    const task = { ...failedTask, status: taskStatus };
    const read = markCmsNoticeRead([], taskIssueNotice(task));
    assert.deepEqual(splitCmsTasks([task], n => isCmsNoticeRead(read, n)).attention, [task]);
    assert.deepEqual(reconcileCmsNotices(read, cmsNoticeObservations({ status, tasks: [task] })), read);
    assert.deepEqual(reconcileCmsNotices(read, cmsNoticeObservations({ status,
      tasks: [{ ...task, error: null }] })), []);
  }
});
