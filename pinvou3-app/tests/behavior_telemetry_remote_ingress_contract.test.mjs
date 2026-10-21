import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

// Normalize once at the read boundary so all callsite offsets and slices use
// the same source text, regardless of Git's checkout line endings.
const readRust = (url, reader = readFileSync) => reader(url, 'utf8').replace(/\r\n/g, '\n');
const commandUrl = name => new URL(`../src-tauri/src/app/commands/${name}.rs`, import.meta.url);
const read = name => readRust(commandUrl(name));
const feature = name => readRust(new URL(`../src-tauri/src/features/${name}.rs`, import.meta.url));

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

function assertSnapshotReceipts(journal) {
  const snapshot = section(journal, 'fn publish_snapshot(', '#[cfg(test)]\nmod tests');
  assertBefore(snapshot, 'verify_digest()', 'self.project_frame(');
  assertBefore(snapshot, 'AuthenticatedProxyEvent::verify', 'if result.is_ok()');
  assertBefore(snapshot, 'if result.is_ok()', 'track_remote_result_received(');
  assert(snapshot.includes('for event in &frame.snapshot().events'));
  assert(snapshot.includes('event.terminal_status.as_deref()'));
}

test('snapshot receipts are recorded only after authenticated projection succeeds', () => {
  assertSnapshotReceipts(read('desktop_remote_journal'));
});

test('B ingress rejection observes the ledger guard and never invents a B-origin submission', () => {
  const business = read('desktop_remote_controller_business');
  const ingress = section(business, 'async fn submit_task(', 'async fn task_status(');
  assertBefore(ingress, 'task_rejection::is_definite(code)', 'RemoteStage::TaskRejected');
  assertBefore(ingress, 'ledger.confirms_absence(', 'RemoteStage::TaskRejected');
  assertBefore(ingress, 'return "task_request_conflict"', 'RemoteStage::TaskRejected');
  assert(/true,\s*payload.get\("mode"\)/.test(ingress));
  assert(!ingress.includes('RemoteStage::TaskSubmit'));
});

test('recovery telemetry uses the injected ledger before consulting live runtime state', () => {
  const journal = read('desktop_remote_journal');
  const beforeTelemetry = section(journal, 'impl RemoteTaskEventSink for DesktopTaskEventSink', 'self.track_task_once(event, &record)');
  assert(beforeTelemetry.includes('task_telemetry_record(&self.ledger, event)'));
  assert(!beforeTelemetry.includes('.state::<'));
  const composition = readRust(new URL('../src-tauri/src/lib.rs', import.meta.url));
  const sink = section(composition, 'DesktopTaskEventSink::new(', 'let recovered = desktop_task_runtime.recover_after_restart()');
  assert(sink.includes('desktop_task_runtime.ledger()'));
});

function assertLocalRejection(task) {
  const submission = section(task, 'fn submit_prepared_bound_task_inner(', '#[cfg(test)]\nfn admit_bound_task_with_ref');
  assert.match(submission, /if should_report_local_task_rejection\(local_submission, &result, &service\.ledger\(\), task_id\)/);
  const guard = submission.slice(requiredIndex(submission, 'fn should_report_local_task_rejection('));
  assert(guard.includes('local_submission'));
  assert(guard.includes('result.is_err()'));
  assert(guard.includes('ledger.confirms_absence(task_id)'));
  const ledger = feature('remote_control/controller/task');
  const absence = section(ledger, 'pub(crate) fn confirms_absence(', 'pub(crate) fn records(');
  assert.match(absence, /matches!\(self\.record\(task_id\), Err\(lookup\) if lookup\.code == "task_not_found"\)/);
}

test('B local rejection is gated by absence from the durable admission ledger', () => {
  assertLocalRejection(read('desktop_remote_task'));
});

for (const [name, newline] of [['LF', '\n'], ['CRLF', '\r\n']]) {
  test(`section contracts accept real Rust source read with ${name} line endings`, () => {
    for (const [file, check] of [
      ['desktop_remote_journal', assertSnapshotReceipts],
      ['desktop_remote_task', assertLocalRejection],
    ]) {
      const url = commandUrl(file);
      const input = readFileSync(url, 'utf8').replace(/\r?\n/g, newline);
      assert(input.includes(newline));
      assert.equal(input.includes('\r\n'), name === 'CRLF');
      check(readRust(url, () => input));
    }
  });

  test(`section contracts reject missing boundaries with ${name} line endings`, () => {
    for (const [file, check, start, end] of [
      ['desktop_remote_journal', assertSnapshotReceipts, 'fn publish_snapshot(', `#[cfg(test)]${newline}mod tests`],
      ['desktop_remote_task', assertLocalRejection, 'fn submit_prepared_bound_task_inner(', `#[cfg(test)]${newline}fn admit_bound_task_with_ref`],
    ]) {
      const url = commandUrl(file);
      const input = readFileSync(url, 'utf8').replace(/\r?\n/g, newline);
      for (const marker of [start, end]) {
        const missing = input.replace(marker, 'removed_section_boundary');
        assert.notEqual(missing, input, `${file}: negative control must remove ${marker}`);
        assert.throws(
          () => check(readRust(url, () => missing)),
          /missing contract callsite:/,
          `${file}: missing section boundary must fail`,
        );
      }
    }
  });
}

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

function assertTrustedDuration(journal) {
  const track = section(journal, 'fn track_task_once(', 'const TASK_STATUS_EVENT');
  assert.equal((track.match(/\.duration_ms\(/g) || []).length, 1);
  assertBefore(track, 'if event.status == "failed"', 'if let Some(duration) = task_telemetry_duration_ms(event, record)');
  assertBefore(track, 'metric = metric.duration_ms(duration)', 'track_remote_execution_event(');
  const duration = section(journal, 'fn task_telemetry_duration_ms(', 'impl DesktopTaskEventSink');
  assert(duration.includes('event.status == "admitted"'));
  assert(duration.includes('record.terminal_reason.as_deref() == Some("process_restart")'));
  assert(duration.includes('record.updated_at_ms.checked_sub(record.admitted_at_ms)'));
}

test('journal omits unmeasured admission/recovery durations after every metric construction branch', () => {
  const journal = read('desktop_remote_journal');
  assertTrustedDuration(journal);
  for (const needle of ['if let Some(duration) = task_telemetry_duration_ms(event, record)', 'record.terminal_reason.as_deref() == Some("process_restart")', 'event.status == "admitted"']) {
    assert.throws(() => assertTrustedDuration(journal.replace(needle, 'removed_guard')));
  }
});

function assertLocalDiagnosticOwnership(task, business) {
  const command = section(task, 'pub async fn desktop_remote_controlled_task_send(', '/// B-local task stop.');
  assertBefore(command, 'submission_delegated = true', '.submit_local(input)');
  assert(command.includes('if result.is_err() && !submission_delegated'));
  const admission = section(business, 'fn admit_and_dispatch(&self, prepared: PreparedControlledTask)', 'fn submit_prevalidated_remote');
  assertBefore(admission, '*self.diagnostic_task.lock() = Some(prepared.task_id.clone())', 'route_prepared_controlled_task(');
  const worker = section(business, 'async fn submit_controlled_on_worker(', 'impl ControlledExecutionPort');
  assertBefore(worker, 'let result = async', 'TurnBudget::from_environment()');
  assertBefore(worker, 'run_controlled_submission_worker(', 'if local && result.is_err()');
  assertBefore(worker, 'should_report_unbound_local_failure(&ledger, observation.lock().as_deref())', 'track_remote_control(');
  assert(worker.includes('task_id.is_none_or(|task_id| ledger.confirms_absence(task_id))'));
}

test('local preparation diagnostics retain ownership through worker errors without reclassifying admitted tasks', () => {
  const task = read('desktop_remote_task');
  const business = read('desktop_remote_controller_business');
  assertLocalDiagnosticOwnership(task, business);
  assert.throws(() => assertLocalDiagnosticOwnership(task.replace('if result.is_err() && !submission_delegated', 'if result.is_err()'), business));
  for (const needle of ['*self.diagnostic_task.lock() = Some(prepared.task_id.clone())', 'should_report_unbound_local_failure(&ledger, observation.lock().as_deref())', 'ledger.confirms_absence(task_id)']) {
    assert.throws(() => assertLocalDiagnosticOwnership(task, business.replace(needle, 'removed_guard')));
  }
});

test('origin registration captures and checks the deletion epoch before persistence', () => {
  const source = feature('behavior_telemetry/remote_execution');
  const registration = section(source, 'pub(crate) fn register_remote_origin', 'pub(crate) fn track_remote_result_received');
  function check(text) {
    assertBefore(text, 'capture_execution_lease(&telemetry)', 'register_origin(&telemetry, task, mode, &token)');
    assertBefore(text, 'Arc::ptr_eq(&contexts.registration_epoch, &token.registration_epoch)', 'insert_origin(');
    assertBefore(text, '!telemetry.lease_is_current(&token.lease)', 'insert_origin(');
  }
  check(registration);
  assert.throws(() => check(registration.replace('Arc::ptr_eq(&contexts.registration_epoch, &token.registration_epoch)', 'true')));
});
