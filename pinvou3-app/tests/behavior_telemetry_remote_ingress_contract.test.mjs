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

test('recovery telemetry uses the injected ledger before consulting live runtime state', () => {
  const journal = read('desktop_remote_journal');
  const publication = journal.slice(journal.indexOf('impl RemoteTaskEventSink for DesktopTaskEventSink'));
  const beforeTelemetry = publication.slice(0, publication.indexOf('self.track_task_once(event, &record)'));
  assert(beforeTelemetry.includes('task_telemetry_record(&self.ledger, event)'));
  assert(!beforeTelemetry.includes('.state::<'));
  const composition = readFileSync(new URL('../src-tauri/src/lib.rs', import.meta.url), 'utf8');
  const sink = composition.slice(composition.indexOf('DesktopTaskEventSink::new('), composition.indexOf('let recovered = desktop_task_runtime.recover_after_restart()'));
  assert(sink.includes('desktop_task_runtime.ledger()'));
});

test('B local rejection is gated by absence from the durable admission ledger', () => {
  const task = read('desktop_remote_task');
  const submission = task.slice(task.indexOf('fn submit_prepared_bound_task_inner('), task.indexOf('#[cfg(test)]\nfn admit_bound_task_with_ref'));
  assert.match(submission, /if should_report_local_task_rejection\(local_submission, &result, &service\.ledger\(\), task_id\)/);
  const guard = submission.slice(submission.indexOf('fn should_report_local_task_rejection('));
  assert(guard.includes('local_submission'));
  assert(guard.includes('result.is_err()'));
  assert.match(guard, /matches!\(ledger\.record\(task_id\), Err\(lookup\) if lookup\.code == "task_not_found"\)/);
});

test('durable admission registers its original consent before fallible resource installation', () => {
  const task = read('desktop_remote_task');
  const dispatch = task.slice(task.indexOf('fn admit_and_dispatch('), task.indexOf('let (outcome, mut admission_cleanup)'));
  assert(dispatch.indexOf('capture_remote_execution_lease(app)') < dispatch.indexOf('let install = |ticket: &RemoteTaskTicket|'));
  const install = dispatch.slice(dispatch.indexOf('let install = |ticket: &RemoteTaskTicket|'));
  assert(install.indexOf('register_remote_execution_with_lease(') < install.indexOf('.claim_for_admission('));
  assert(install.indexOf('register_remote_execution_with_lease(') < install.indexOf('.install(package,'));
  assert(install.indexOf('register_remote_execution_with_lease(') < install.indexOf('authorizations.authorize('));
  assert(dispatch.includes('.prepare_bound_admission(lease, validated, now_b, expected_remote_ref, install)'));
  assert(!/\bregister_remote_execution(?:_with_lease)?\(/.test(read('desktop_remote_journal')), 'late admission publication cannot mint a new consent lease');
});

test('remote turn binding precedes generic telemetry and uses exact engine ownership', () => {
  const feature = name => readFileSync(new URL(`../src-tauri/src/features/${name}.rs`, import.meta.url), 'utf8');
  const engine = feature('assistant/engine');
  const send = engine.slice(engine.indexOf('async fn send_reserved_turn_op('), engine.indexOf('pub(crate) async fn finish_reclaimed_turn('));
  assert(send.indexOf('reservation.bind_remote_submission(submission_id.as_deref())?') < send.indexOf('self.handle.send(op)'));
  const forwarder = feature('assistant/forwarder');
  const started = forwarder.slice(forwarder.indexOf('Event::TurnStarted {'));
  assert(started.indexOf('take_remote_task_for_submission(submission_id.as_deref())') < started.indexOf('bind_remote_execution_turn('));
  assert(started.indexOf('bind_remote_execution_turn(') < started.indexOf('behavior_telemetry::admit_task('));
  const chat = feature('assistant/remote_turn');
  assert.equal(chat.match(/reservation\.take_remote_turn\(task_id\)/g)?.length, 2, 'plain and attachment sends bind their reservation');
  const code = feature('codex_acp/remote_turn');
  const codeSubmit = code.slice(code.indexOf('let mut turn_relay = AcpTurnEventRelay::start('));
  assert(codeSubmit.indexOf('bind_remote_execution_turn(') < codeSubmit.indexOf('.submit(&actual_content'));
  assert(codeSubmit.includes('reservation.turn_id()'));
});
