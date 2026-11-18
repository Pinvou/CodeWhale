import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { encodeWindowsAncestorPaths } from '../windows-ancestor-paths.js';
import { verifyWindowsAncestors } from '../relay-state-store.js';

test('ancestor envelope preserves literal Unicode, metacharacters and line boundaries', () => {
  const paths = ['C:\\目录 空格\\[x] $literal;quote\'', 'C:\\another\\a\nb'];
  const encoded = encodeWindowsAncestorPaths(paths);
  assert.deepEqual(encoded.split('\n').map(value => Buffer.from(value, 'base64').toString('utf8')), paths);
  for (const value of [[], null, [''], [7], ['a\0b']]) assert.throws(() => encodeWindowsAncestorPaths(value), TypeError);
});

test('native module-free ancestor check rejects junctions, dangling paths and a junction ancestor', {
  skip: process.platform !== 'win32',
}, async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'ancestor-native-'));
  try {
    const target = path.join(root, '目录 [literal] $x');
    await fs.mkdir(target);
    const file = path.join(target, 'plain.txt');
    await fs.writeFile(file, 'owned test');
    await verifyWindowsAncestors([root, target, file]);
    const link = path.join(root, 'junction');
    await fs.symlink(target, link, 'junction');
    await assert.rejects(verifyWindowsAncestors([link]));
    await assert.rejects(verifyWindowsAncestors([root, link, path.join(link, 'plain.txt')]));
    await assert.rejects(verifyWindowsAncestors([path.join(root, 'absent')]));
    const empty = path.join(root, 'empty');
    await fs.mkdir(empty);
    const dangling = path.join(root, 'dangling');
    await fs.symlink(empty, dangling, 'junction');
    await fs.rmdir(empty);
    await assert.rejects(verifyWindowsAncestors([dangling]));
    // No cmdlet/module dependency: execute the real verifier with autoload
    // disabled in the script scope, rather than merely inspecting source text.
    const { WINDOWS_REPARSE_VERIFY_SCRIPT } = await import('../windows-ancestor-paths.js');
    const script = "$PSModuleAutoLoadingPreference='None'\n" + WINDOWS_REPARSE_VERIFY_SCRIPT;
    const output = execFileSync(path.join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe'),
      ['-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', Buffer.from(script, 'utf16le').toString('base64')],
      { encoding: 'utf8', windowsHide: true, timeout: 15_000,
        env: { ...process.env, PINVOU_ANCESTOR_DIAGNOSTIC: '', PINVOU_PATHS_B64_LINES: encodeWindowsAncestorPaths([root, target, file]) } });
    assert.equal(output, 'ok');
  } finally {
    await fs.rm(root, { recursive: true, force: true });
  }
});
