import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const desktopAttachments = readFileSync(new URL('../src/platform/tauri/bridge/artifacts.js', import.meta.url), 'utf8');
const desktopArtifacts = readFileSync(new URL('../src/platform/tauri/bridge/artifact-tracker.js', import.meta.url), 'utf8');
const web = readFileSync(new URL('../src/platform/web/bridge.js', import.meta.url), 'utf8');
const knowledge = readFileSync(new URL('../src/features/knowledge/KnowledgeView.jsx', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const telemetry = [
  '../src-tauri/src/features/behavior_telemetry.rs',
  '../src-tauri/src/features/behavior_telemetry_tests.rs',
].map(path => readFileSync(new URL(path, import.meta.url), 'utf8')).join('\n');

test('desktop and web attachments emit a correlated, idempotent processing terminal', () => {
  for (const source of [desktopAttachments, web]) {
    assert.match(source, /eventName: ["']attachment_added["']/);
    assert.match(source, /eventName: ["']attachment_processing_completed["']/);
    assert.match(source, /attempt\.finished/);
    assert.match(source, /fileTypeBucket/);
    assert.match(source, /sizeBucket/);
    assert.match(source, /["']composer["']/);
    assert.match(source, /errorCategory: success \? undefined/);
  }
  const webPathAttachment = web.match(/async function addAttachmentByPath[\s\S]*?\n  }/)?.[0] || '';
  assert.ok(webPathAttachment.indexOf('beginAttachmentTelemetry') < webPathAttachment.indexOf('await invoke'));
});

test('knowledge imports preserve one operation across polling and terminal phases', () => {
  assert.match(knowledge, /knowledgeImportTelemetryAttempt/);
  assert.match(knowledge, /eventName: 'knowledge_import_started'/);
  assert.match(knowledge, /eventName: 'knowledge_import_completed'/);
  assert.match(knowledge, /\['done', 'done_with_errors', 'cancelled', 'interrupted'\]\.includes\(next\.phase\)/);
  assert.match(knowledge, /sourceType/);
  assert.match(knowledge, /itemCountBucket: countBucket\(list\.length\)/);
});

test('artifact creation is bound to an authoritative turn and export reuses its anonymous id', () => {
  for (const source of [desktopArtifacts, web]) {
    assert.match(source, /eventName: ["']artifact_created["']/);
    assert.match(source, /artifactId:/);
    assert.match(source, /artifactType:/);
  }
  for (const source of [desktopAttachments, web]) {
    assert.match(source, /eventName: ["']artifact_exported["']/);
    assert.match(source, /exportFormat:/);
  }
  assert.match(command, /"artifact_created" => \{[^]*?track_artifact_created/);
  assert.match(command, /"artifact_exported" => \{[^]*?track_artifact_exported/);
  assert.match(telemetry, /track_artifact_created[^]*?attempt\.turn_id\.clone\(\)/);
  assert.match(telemetry, /track_artifact_created[^]*?latest_turns/);
  assert.match(telemetry, /struct ArtifactTelemetryRecord/);
  assert.match(telemetry, /track_artifact_exported[^]*?artifacts\s*\.lock/);
});

test('behavior payloads never include content, path, filename, title, or query fields', () => {
  for (const source of [desktopAttachments, desktopArtifacts, web, knowledge]) {
    const telemetryRequests = source.match(/(?:track_behavior_event|web_access_track_behavior_event)[^]*?request:\s*\{[^}]*\}/g) || [];
    for (const request of telemetryRequests) {
      assert.doesNotMatch(request, /\b(path|filename|fileName|title|content|query)\s*:/);
    }
  }
});
