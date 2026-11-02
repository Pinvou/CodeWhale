import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { offlineAbandonTempBase } from './helpers/offline-abandon-temp.mjs';

const chromeSuffix = 'com.google.Chrome.XXXXXX/SingletonSocket';
const chromiumSuffix = '.org.chromium.Chromium.XXXXXX/SingletonSocket';
const socketBytes = (root, suffix) => Buffer.byteLength(path.posix.join(root, suffix), 'utf8') + 1;

function removeOwnedTestParent(parent, allowedParent) {
  const actual = fs.realpathSync(parent);
  if (path.dirname(actual) !== allowedParent || !path.basename(actual).startsWith('pva-contract-')) throw Error('Cleanup ownership changed');
  fs.rmSync(parent, { recursive: true, force: true });
}

test('Linux long runner temp cannot exhaust the complete singleton socket budget', () => {
  const runner = '/home/github-runner/ci-runners/medium-02/_work/_temp';
  const previous = path.posix.join(runner, 'pinvou-offline-abandon-owner-5O5uDe');
  assert.equal(socketBytes(previous, 'com.google.Chrome.8DTsna/SingletonSocket') - 1, 129);
  assert.ok(socketBytes(previous, chromeSuffix) > 108, 'the observed failing layout exceeds sun_path including NUL');
  const selected = offlineAbandonTempBase('linux', runner + '/very-long-runner-temp'.repeat(12));
  assert.equal(selected, '/tmp');
  const owned = path.posix.join(selected, 'pva-ABCDEF');
  for (const suffix of [chromeSuffix, chromiumSuffix]) assert.ok(socketBytes(owned, suffix) <= 108);
});

test('the socket budget uses UTF-8 bytes rather than JavaScript character count', () => {
  const root = '/tmp/' + '中'.repeat(25);
  assert.ok(path.posix.join(root, chromeSuffix).length + 1 <= 108);
  assert.ok(socketBytes(root, chromeSuffix) > 108);
  assert.equal(offlineAbandonTempBase('linux', root), '/tmp');
});

test('non-Linux owners preserve the inherited temporary directory', () => {
  for (const platform of ['win32', 'darwin']) {
    const inherited = path.join(os.tmpdir(), 'owned-runner-temp');
    assert.equal(offlineAbandonTempBase(platform, inherited), inherited);
  }
});

test('a real child with long runner environment allocates and reaps only its private root', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'pva-contract-'));
  const allowedParent = fs.realpathSync(os.tmpdir());
  assert.equal(path.dirname(fs.realpathSync(parent)), allowedParent);
  const longTemp = path.join(parent, 'long-runner-temp-'.repeat(9));
  fs.mkdirSync(longTemp);
  const sentinel = path.join(longTemp, 'keep.txt'); fs.writeFileSync(sentinel, 'unrelated runner data');
  const moduleURL = new URL('./helpers/offline-abandon-temp.mjs', import.meta.url).href;
  try {
    const child = spawnSync(process.execPath, ['--input-type=module', '-e', `
      import fs from 'node:fs'; import os from 'node:os'; import path from 'node:path';
      const {createOfflineAbandonTempRoot}=await import(process.argv[1]);
      const owned=createOfflineAbandonTempRoot(); let validated=false;
      try {
        const base=fs.realpathSync(process.platform==='linux'?'/tmp':os.tmpdir());
        const actual=fs.realpathSync(owned), stat=fs.statSync(owned);
        if(path.dirname(actual)!==base||!path.basename(actual).startsWith('pva-')||!stat.isDirectory())throw Error('Unexpected owned root');
        validated=true;
        if(process.platform==='linux'&&(stat.mode&0o777)!==0o700)throw Error('Root is not private');
        console.log(JSON.stringify({root:actual,base,platform:process.platform}));
      } finally { if(validated)fs.rmSync(owned,{recursive:true,force:true}); }
    `, moduleURL], { encoding: 'utf8', timeout: 10000,
      env: { ...process.env, RUNNER_TEMP: longTemp, TMPDIR: longTemp, TMP: longTemp, TEMP: longTemp } });
    assert.equal(child.error, undefined); assert.equal(child.status, 0, child.stderr);
    const result = JSON.parse(child.stdout.trim());
    assert.equal(fs.existsSync(result.root), false, 'the child must reap its own allocation');
    assert.equal(fs.readFileSync(sentinel, 'utf8'), 'unrelated runner data');
    if (process.platform === 'linux') {
      assert.equal(result.base, fs.realpathSync('/tmp'));
      for (const suffix of [chromeSuffix, chromiumSuffix]) assert.ok(socketBytes(result.root, suffix) <= 108);
    } else assert.equal(result.base, fs.realpathSync(longTemp));
  } finally {
    removeOwnedTestParent(parent, allowedParent);
  }
});
