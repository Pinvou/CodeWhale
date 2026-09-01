import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const view = readFileSync(new URL('../src/features/tools/ToolStoreView.jsx', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const telemetry = [
  '../src-tauri/src/features/behavior_telemetry.rs',
  '../src-tauri/src/features/behavior_telemetry_tests.rs',
].map(path => readFileSync(new URL(path, import.meta.url), 'utf8')).join('\n');

test('tool cards emit deduplicated visibility impressions and explicit detail opens', () => {
  assert.match(view, /import \{ can, isWeb \} from '..\/..\/shared\/platform\.js'/);
  assert.doesNotMatch(view, /window\.PinvouPlatform/);
  assert.match(view, /new window\.IntersectionObserver/);
  assert.match(view, /entry\.intersectionRatio < 0\.25/);
  assert.match(view, /behaviorImpressionsRef\.current\.has\(backendId\)/);
  assert.match(view, /trackToolBehavior\('tool_impression'[^]*?viewId: behaviorViewIdRef\.current/);
  assert.match(view, /trackToolBehavior\('tool_detail_opened'[^]*?viewId: behaviorViewIdRef\.current/);
  assert.match(view, /data-tool-analytics-key=\{tool\.backendId \|\| undefined\}/);
  assert.match(view, /if \(index < 3\) return '1_3'/);
  assert.match(view, /if \(index < 10\) return '4_10'/);
  assert.match(view, /return '11_plus'/);
});

test('each configuration operation has one correlated terminal event', () => {
  assert.match(view, /trackToolBehavior\('tool_configuration_started'[^]*?operationId: attempt\.operationId[^]*?attemptIndex/);
  assert.match(view, /configurationAttemptsRef\.current\.delete\(backendId\)/);
  assert.match(view, /trackToolBehavior\('tool_configuration_finished'[^]*?operationId: attempt\.operationId[^]*?success[^]*?durationMs/);
  assert.match(view, /Array\.from\(configurationAttemptsRef\.current\.keys\(\)\)[^]*?finishToolConfiguration\(backendId, false\)/);
  for (const connector of ['feishu', 'wecom', 'dingtalk', 'eip', 'zhidao', 'tmeet', 'ima']) {
    assert.match(view, new RegExp(`finishToolConfiguration\\('${connector}', true\\)|finishToolConfiguration\\('${connector}', succeeded\\)`));
    assert.match(view, new RegExp(`finishToolConfiguration\\('${connector}', false\\)|finishToolConfiguration\\('${connector}', succeeded\\)`));
  }
});

test('tool identity is reduced to a controlled backend registry', () => {
  for (const event of [
    'tool_impression',
    'tool_detail_opened',
    'tool_configuration_started',
    'tool_configuration_finished',
  ]) {
    assert.match(command, new RegExp(`"${event}" => "${event}"`));
  }
  assert.match(command, /tool_registry_key\(raw_tool\)/);
  assert.match(command, /event\.tool\([^]*?"",[^]*?tool_type/);
  assert.match(telemetry, /"canva-mcp" => "canva"/);
  assert.match(telemetry, /_ => "custom"/);
  assert.doesNotMatch(view, /trackToolBehavior\([^]*?toolName:/);
});
