import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const read = name => readFileSync(new URL(`../src-tauri/src/app/commands/${name}.rs`, import.meta.url), 'utf8');

test('snapshot receipts are recorded only after authenticated projection succeeds', () => {
  const journal = read('desktop_remote_journal');
  const snapshot = journal.slice(journal.indexOf('fn publish_snapshot('), journal.indexOf('#[cfg(test)]\nmod tests'));
  assert(snapshot.indexOf('verify_digest()') < snapshot.indexOf('self.project_frame('));
  assert(snapshot.indexOf('AuthenticatedProxyEvent::verify') < snapshot.indexOf('if result.is_ok()'));
  assert(snapshot.indexOf('if result.is_ok()') < snapshot.indexOf('track_remote_result_received('));
  assert(snapshot.includes('for event in &frame.snapshot().events'));
  assert(snapshot.includes('event.terminal_status.as_deref()'));
});

test('B ingress rejection observes the ledger guard and never invents a B-origin submission', () => {
  const business = read('desktop_remote_controller_business');
  const ingress = business.slice(business.indexOf('async fn submit_task('), business.indexOf('async fn task_status('));
  assert(ingress.indexOf('task_rejection::is_definite(code)') < ingress.indexOf('RemoteStage::TaskRejected'));
  assert(ingress.indexOf('ledger.record(') < ingress.indexOf('RemoteStage::TaskRejected'));
  assert(ingress.indexOf('return "task_request_conflict"') < ingress.indexOf('RemoteStage::TaskRejected'));
  assert(ingress.includes('true, payload.get("mode")') || /true,\s*payload.get\("mode"\)/.test(ingress));
  assert(!ingress.includes('RemoteStage::TaskSubmit'));
});
