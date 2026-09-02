import { readFileSync } from 'node:fs';

const behaviorTelemetryModules = [
  '../src-tauri/src/features/behavior_telemetry.rs',
  '../src-tauri/src/features/behavior_telemetry/runtime.rs',
  '../src-tauri/src/features/behavior_telemetry/events.rs',
  '../src-tauri/src/features/behavior_telemetry_tests.rs',
];

export const behaviorTelemetryRust = behaviorTelemetryModules
  .map(relativePath => readFileSync(new URL(relativePath, import.meta.url), 'utf8'))
  .join('\n');
