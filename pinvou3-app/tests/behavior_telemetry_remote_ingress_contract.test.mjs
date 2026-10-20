import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const read = name => readFileSync(new URL(`../src-tauri/src/app/commands/${name}.rs`, import.meta.url), 'utf8');
const feature = name => readFileSync(new URL(`../src-tauri/src/features/${name}.rs`, import.meta.url), 'utf8');

function requiredIndex(source, needle, start = 0) {
  const index = source.indexOf(needle, start);
  assert(index >= 0, `missing contract callsite: ${needle}`);
  return index;
}

function section(source, start, end) {
  const begin = requiredIndex(source, start);
  return source.slice(begin, requiredIndex(source, end, begin + start.length));
}

function assertBefore(source, first, second) {
  assert(requiredIndex(source, first) < requiredIndex(source, second), `${first} must precede ${second}`);
}

test('snapshot receipts are recorded only after authenticated projection succeeds', () => {
  const journal = read('desktop_remote_journal');
  const snapshot = section(journal, 'fn publish_snapshot(', '#[cfg(test)]\nmod tests');
  assertBefore(snapshot, 'verify_digest()', 'self.project_frame(');
  assertBefore(snapshot, 'AuthenticatedProxyEvent::verify', 'if result.is_ok()');
  assertBefore(snapshot, 'if result.is_ok()', 'track_remote_result_received(');
  assert(snapshot.includes('for event in &frame.snapshot().events'));
  assert(snapshot.includes('event.terminal_status.as_deref()'));
});

test('B ingress rejection observes the ledger guard and never invents a B-origin submission', () => {
  const business = read('desktop_remote_controller_business');
  const ingress = section(business, 'async fn submit_task(', 'async fn task_status(');
  assertBefore(ingress, 'task_rejection::is_definite(code)', 'RemoteStage::TaskRejected');
  assertBefore(ingress, 'ledger.record(', 'RemoteStage::TaskRejected');
  assertBefore(ingress, 'return "task_request_conflict"', 'RemoteStage::TaskRejected');
  assert(ingress.includes('true, payload.get("mode")') || /true,\s*payload.get\("mode"\)/.test(ingress));
  assert(!ingress.includes('RemoteStage::TaskSubmit'));
});

test('recovery telemetry uses the injected ledger before consulting live runtime state', () => {
  const journal = read('desktop_remote_journal');
  const beforeTelemetry = section(journal, 'impl RemoteTaskEventSink for DesktopTaskEventSink', 'self.track_task_once(event, &record)');
  assert(beforeTelemetry.includes('task_telemetry_record(&self.ledger, event)'));
  assert(!beforeTelemetry.includes('.state::<'));
  const composition = readFileSync(new URL('../src-tauri/src/lib.rs', import.meta.url), 'utf8');
  const sink = section(composition, 'DesktopTaskEventSink::new(', 'let recovered = desktop_task_runtime.recover_after_restart()');
  assert(sink.includes('desktop_task_runtime.ledger()'));
});

test('B local rejection is gated by absence from the durable admission ledger', () => {
  const task = read('desktop_remote_task');
  const submission = section(task, 'fn submit_prepared_bound_task_inner(', '#[cfg(test)]\nfn admit_bound_task_with_ref');
  assert.match(submission, /if should_report_local_task_rejection\(local_submission, &result, &service\.ledger\(\), task_id\)/);
  const guard = submission.slice(requiredIndex(submission, 'fn should_report_local_task_rejection('));
  assert(guard.includes('local_submission'));
  assert(guard.includes('result.is_err()'));
  assert.match(guard, /matches!\(ledger\.record\(task_id\), Err\(lookup\) if lookup\.code == "task_not_found"\)/);
});

function assertDurableAdmission(task, journal) {
  const dispatch = section(task, 'fn admit_and_dispatch(', 'let (outcome, mut admission_cleanup)');
  assertBefore(dispatch, 'capture_remote_execution_lease(app)', 'let install = |ticket: &RemoteTaskTicket|');
  const install = dispatch.slice(requiredIndex(dispatch, 'let install = |ticket: &RemoteTaskTicket|'));
  assertBefore(install, 'register_remote_execution_with_lease(', '.claim_for_admission(');
  assertBefore(install, 'register_remote_execution_with_lease(', '.install(package,');
  assertBefore(install, 'register_remote_execution_with_lease(', 'authorizations.authorize(');
  assert(dispatch.includes('.prepare_bound_admission(lease, validated, now_b, expected_remote_ref, install)'));
  assert(!/\bregister_remote_execution(?:_with_lease)?\(/.test(journal), 'late admission publication cannot mint a new consent lease');
}

function assertRemoteTurnBinding({ engine, forwarder, chat, code }) {
  const send = section(engine, 'async fn send_reserved_turn_op(', 'pub(crate) async fn finish_reclaimed_turn(');
  assertBefore(send, 'reservation.bind_remote_submission(submission_id.as_deref())?', 'self.handle.send(op)');
  const started = forwarder.slice(requiredIndex(forwarder, 'Event::TurnStarted {'));
  assertBefore(started, 'take_remote_task_for_submission(submission_id.as_deref())', 'bind_remote_execution_turn(');
  assertBefore(started, 'bind_remote_execution_turn(', 'behavior_telemetry::admit_task(');
  assert.equal(chat.match(/reservation\.take_remote_turn\(task_id\)/g)?.length, 2, 'plain and attachment sends bind their reservation');
  const codeSubmit = code.slice(requiredIndex(code, 'let mut turn_relay = AcpTurnEventRelay::start('));
  assertBefore(codeSubmit, 'bind_remote_execution_turn(', '.submit(&actual_content');
  assert(codeSubmit.includes('reservation.turn_id()'));
}

function bindingSources() {
  return {
    engine: feature('assistant/engine'),
    forwarder: feature('assistant/forwarder'),
    chat: feature('assistant/remote_turn'),
    code: feature('codex_acp/remote_turn'),
  };
}

test('durable admission registers its original consent before fallible resource installation', () => {
  assertDurableAdmission(read('desktop_remote_task'), read('desktop_remote_journal'));
});

test('remote turn binding precedes generic telemetry and uses exact engine ownership', () => {
  assertRemoteTurnBinding(bindingSources());
});

test('admission and turn contracts reject missing registration or binding callsites', () => {
  const task = read('desktop_remote_task');
  const journal = read('desktop_remote_journal');
  const withoutRegistration = task.replace('register_remote_execution_with_lease(', 'removed_registration(');
  assert.notEqual(withoutRegistration, task);
  assert.throws(() => assertDurableAdmission(withoutRegistration, journal), /missing contract callsite: register_remote_execution_with_lease/);
  const sources = bindingSources();
  for (const [file, needle] of [
    ['engine', 'reservation.bind_remote_submission(submission_id.as_deref())?'],
    ['forwarder', 'take_remote_task_for_submission(submission_id.as_deref())'],
    ['forwarder', 'bind_remote_execution_turn('],
    ['code', 'bind_remote_execution_turn('],
  ]) {
    const changed = sources[file].replace(needle, 'removed_binding');
    assert.notEqual(changed, sources[file]);
    assert.throws(
      () => assertRemoteTurnBinding({ ...sources, [file]: changed }),
      /missing contract callsite:/,
      `${file}: missing ${needle} must fail`,
    );
  }
});
