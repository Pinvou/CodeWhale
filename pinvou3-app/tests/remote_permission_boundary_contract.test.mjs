import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const rust = new URL('../src-tauri/src/', import.meta.url);
const adapter = readFileSync(new URL('features/assistant/remote_turn.rs', rust), 'utf8');
const pool = readFileSync(new URL('features/assistant/engine_pool.rs', rust), 'utf8');
const composition = readFileSync(new URL('app/commands/desktop_remote_task.rs', rust), 'utf8');

function assertPermissionBoundary(source, engine, root) {
  const start = source.indexOf('impl RemoteChatEnginePort for EnginePoolChatEngine');
  assert.ok(start >= 0);
  const plainStart = source.indexOf('async fn send_user_message(', start);
  const attachmentStart = source.indexOf('async fn send_user_message_with_attachments(', plainStart);
  const end = source.indexOf('fn is_turn_active(', attachmentStart);
  assert.ok(plainStart > start && attachmentStart > plainStart && end > attachmentStart);
  const branches = [source.slice(plainStart, attachmentStart), source.slice(attachmentStart, end)];
  for (const [index, branch] of branches.entries()) {
    const check = branch.indexOf('let mode = remote_chat_permission_mode(&self.store, session_id)?;');
    assert.ok(check >= 0, 'both real adapter paths must enforce B policy');
    const model = branch.indexOf('runtime_models_for_task(task_id, lease)');
    assert.ok(model >= 0 && check > branch.indexOf('.await', model), 'policy check must follow model preparation');
    if (index === 1) {
      const ingest = branch.indexOf('tokio::task::spawn_blocking');
      assert.ok(ingest >= 0 && check > branch.indexOf('.await', ingest), 'policy check must follow attachment ingestion');
    }
    assert.ok(check < branch.indexOf('RemoteChatReservation::take_turn'), 'validate before handing off the occupied turn');
    assert.match(branch.slice(check), /\.send_remote_user_message(?:_with_attachments)?\(\s*session_id,\s*content,\s*mode,/);
  }
  assert.match(source, /fn remote_chat_permission_mode\(store: &SessionStore, session_id: &str\) -> Result<AppMode, String>\s*\{\s*for_session\(store, session_id, false\)\.map\(\|mode\| mode\.to_app_mode\(\)\)/);
  assert.doesNotMatch(engine, /remote_control::|task_permissions::/);
  const plainEngine = engine.indexOf('pub(crate) async fn send_remote_user_message(');
  const attachmentEngine = engine.indexOf('pub(crate) async fn send_remote_user_message_with_attachments(');
  const engineEnd = engine.indexOf('pub(crate) async fn send_eval_user_message(', attachmentEngine);
  assert.ok(plainEngine >= 0 && attachmentEngine > plainEngine && engineEnd > attachmentEngine);
  for (const method of ['send_remote_user_message', 'send_remote_user_message_with_attachments']) {
    const header = engine.slice(engine.indexOf(`pub(crate) async fn ${method}(`));
    assert.match(header.slice(0, header.indexOf(') -> Result<()>')), /mode: AppMode,/);
  }
  assert.match(engine.slice(plainEngine, attachmentEngine), /display_message,\s*mode,/);
  assert.match(engine.slice(attachmentEngine, engineEnd), /user_display_message\(display_content\),\s*mode,/);
  assert.match(root, /EnginePoolChatEngine::new\(\s*app\.clone\(\),\s*Arc::new\(pool\),\s*store\.clone\(\),/);
}

test('real chat and attachment adapters validate B policy after preparation and forward the frozen mode', () => {
  assertPermissionBoundary(adapter, pool, composition);
});

test('permission wiring contract rejects a missing check or silently substituted mode in either branch', () => {
  const check = 'let mode = remote_chat_permission_mode(&self.store, session_id)?;';
  const first = adapter.indexOf(check);
  const second = adapter.indexOf(check, first + check.length);
  assert.ok(first >= 0 && second > first);
  for (const offset of [first, second]) {
    const omitted = adapter.slice(0, offset) + adapter.slice(offset + check.length);
    assert.throws(() => assertPermissionBoundary(omitted, pool, composition));
  }
  assert.throws(() => assertPermissionBoundary(adapter.replace(/\bcontent,\s*mode,/g, 'content, AppMode::Agent,'), pool, composition));
  assert.throws(() => assertPermissionBoundary(adapter, pool.replace(/(pub\(crate\) async fn send_remote_user_message\([\s\S]*?display_message,)\s*mode,/, '$1 AppMode::Agent,'), composition));
  assert.throws(() => assertPermissionBoundary(adapter, pool, composition.replace(/store\.clone\(\),\s*temporary_models,/, 'SessionStore::default(), temporary_models,')));
});
