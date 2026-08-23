#!/usr/bin/env node
import { execFileSync } from 'node:child_process';

const base = String(process.env.PINVOU_CLIENT_MIGRATION_BASE_SHA || '').trim();
if (!/^[0-9a-f]{7,40}$/i.test(base)) {
  throw new Error('client_migration_scope_base_missing: set PINVOU_CLIENT_MIGRATION_BASE_SHA');
}
const changed = execFileSync('git', ['diff', '--name-only', `${base}...HEAD`], { encoding: 'utf8' })
  .split(/\r?\n/).map(value => value.trim()).filter(Boolean);
const allowedDocs = new Set([
  'account-platform/docs/architecture.md',
  'account-platform/docs/call-flows.md',
  'account-platform/docs/deployment.md',
  'account-platform/docs/implementation-status.md',
  'account-platform/docs/implementation-tasks.md',
]);
const denied = changed.filter(file => !(
  file.startsWith('pinvou3-app/')
  || file === 'scripts/account-platform-release-gate.mjs'
  || file === 'scripts/check-client-migration-scope.mjs'
  || allowedDocs.has(file)
));
if (denied.length) {
  process.stderr.write(`client_migration_scope_violation:\n${denied.join('\n')}\n`);
  process.exit(1);
}
process.stdout.write(`${JSON.stringify({ decision:'PASS', base, changed_files:changed.length })}\n`);
