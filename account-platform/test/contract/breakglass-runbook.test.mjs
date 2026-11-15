import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const source = readFileSync(new URL('../../deploy/runbooks/breakglass-retirement.md', import.meta.url), 'utf8');
const blocks = [...source.matchAll(/```bash\r?\n([\s\S]*?)```/g)].map(x => x[1]);
const retirement = blocks.filter(x => x.includes('enabled=false'));
assert.equal(retirement.length, 1, 'one explicit, independently selected identity-retirement operation');
const bash = process.env.PINVOU_TEST_BASH || (process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash');
const shellPath = value => process.platform === 'win32' ? value.replace(/\\/g, '/').replace(/^([A-Za-z]):/, (_, drive) => '/' + drive.toLowerCase()) : value;

function execute(code, input = {}) {
  const parent = path.resolve(tmpdir());
  const directory = mkdtempSync(path.join(parent, 'pinvou-breakglass-runbook-'));
  try {
    const cli = path.join(directory, 'controlled-cli.sh');
    const log = path.join(directory, 'calls.txt');
    writeFileSync(cli, '#!/usr/bin/env bash\nprintf "%s\\n" "$@" >> "$BG_TEST_LOG"\n', { encoding: 'utf8', mode: 0o700 });
    chmodSync(cli, 0o700);
    const env = { ...process.env, KCADM: shellPath(cli), BG_TEST_LOG: shellPath(log),
      USER_REALM: 'controlled-user-realm', TARGET_SUBJECT: 'controlled-subject', KC_ADMIN_CONFIG: '/controlled/private-config',
      BG_IDENTITY_CLASS: '', CONFIRMED_RETIRE_SUBJECT: '', ...input };
    const result = spawnSync(bash, ['--noprofile', '--norc', '-c', code], { env, encoding: 'utf8', timeout: 5000, windowsHide: true });
    assert.equal(result.error, undefined);
    return { ...result, calls: existsSync(log) ? readFileSync(log, 'utf8').trimEnd().split('\n') : [] };
  } finally {
    // Only this freshly created private fixture directory, after sync child exit.
    assert.equal(path.dirname(directory), parent);
    assert.ok(path.basename(directory).startsWith('pinvou-breakglass-runbook-'));
    rmSync(directory, { recursive: true, force: true });
  }
}

test('every published Bash operation parses without executing a business command', () => {
  assert.equal(blocks.length, 4);
  for (const block of blocks) {
    const result = spawnSync(bash, ['--noprofile', '--norc', '-n'], { input: block, encoding: 'utf8', timeout: 5000, windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
  }
});

test('published retirement refuses normal, unknown, mismatched and missing targets before any write', () => {
  for (const input of [
    {}, { BG_IDENTITY_CLASS: 'EXISTING_NORMAL', CONFIRMED_RETIRE_SUBJECT: 'controlled-subject' },
    { BG_IDENTITY_CLASS: 'INCIDENT_ONLY', CONFIRMED_RETIRE_SUBJECT: 'other-subject' },
    { BG_IDENTITY_CLASS: 'INCIDENT_ONLY', CONFIRMED_RETIRE_SUBJECT: 'controlled-subject', TARGET_SUBJECT: '' },
    { BG_IDENTITY_CLASS: 'INCIDENT_ONLY', CONFIRMED_RETIRE_SUBJECT: 'controlled-subject', USER_REALM: '' },
  ]) {
    const result = execute(retirement[0], input);
    assert.notEqual(result.status, 0);
    assert.deepEqual(result.calls, [], 'an unverified target must not reach the CLI');
  }
});

test('explicitly confirmed dedicated target produces one narrowly scoped update', () => {
  const result = execute(retirement[0], { BG_IDENTITY_CLASS: 'INCIDENT_ONLY', CONFIRMED_RETIRE_SUBJECT: 'controlled-subject' });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, ['update', 'users/controlled-subject', '-r', 'controlled-user-realm', '--config', '/controlled/private-config', '-s', 'enabled=false']);
});
