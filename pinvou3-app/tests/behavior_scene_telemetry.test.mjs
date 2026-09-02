import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { behaviorTelemetryRust } from './behavior_telemetry_source.mjs';

const chatView = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
const desktopChat = readFileSync(new URL('../src/platform/tauri/bridge/chat.js', import.meta.url), 'utf8');
const webChat = readFileSync(new URL('../src/platform/web/bridge.js', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const telemetry = behaviorTelemetryRust;

test('scene entries are recorded only after becoming visible and once per view', () => {
  assert.match(chatView, /new window\.IntersectionObserver/);
  assert.match(chatView, /entry\.intersectionRatio < 0\.5/);
  assert.match(chatView, /sceneImpressionsRef\.current\.has\(key\)/);
  assert.match(chatView, /trackSceneEntry\('scene_impression'/);
  assert.match(chatView, /viewId: sceneViewIdRef\.current/);
  assert.match(chatView, /onImpression=\{trackWorkSceneImpression\}/);
  assert.match(chatView, /onImpression=\{trackDesignSceneImpression\}/);
});

test('scene selection is separate from an admitted scene submission', () => {
  assert.match(chatView, /trackSceneEntry\('scene_selected', 'work', nextSubtab\)/);
  assert.match(chatView, /trackSceneEntry\('scene_selected', 'design', nextSubtab\)/);
  assert.match(desktopChat, /\.then\(function \(\) \{[^]*?trackSceneSubmitted\(sid, meta\.pinvouScene\)/);
  assert.match(webChat, /\.then\(function \(\) \{[^]*?trackSceneSubmitted\(sid, meta\.pinvouScene\)/);
  assert.match(webChat, /trackSceneSubmitted\(metadata\.id, submission\.pinvouScene\)/);
});

test('the backend binds scene submissions to its authoritative attempt', () => {
  assert.match(command, /"scene_submitted" => \{[^]*?track_scene_submitted/);
  assert.match(telemetry, /BehaviorEvent::new\("scene_submitted"\)[^]*?\.string\("attempt_id", attempt\.attempt_id\)/);
  assert.match(telemetry, /let dedup_key = format!\("scene:\{\}", attempt\.attempt_id\)/);
  assert.match(telemetry, /pub fn scene_registry_pair/);
  assert.match(telemetry, /_ => \("unknown", "unknown"\)/);
  assert.doesNotMatch(desktopChat, /scene_triggered/);
});
