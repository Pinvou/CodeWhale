import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const runner = new URL('../run-windows-relay-dacl.mjs', import.meta.url).href;
function invoke(platform, result, { temporary, verifyFails = false } = {}) {
  const originalTemporary = temporary || tmpdir();
  const mockSystemRoot = resolve(tmpdir(), 'pinvou-dacl-mock-system-root');
  const program = `import child from 'node:child_process';
    import assert from 'node:assert/strict';
    import {realpathSync,lstatSync,existsSync} from 'node:fs';
    import {dirname,basename} from 'node:path';
    import { syncBuiltinESMExports } from 'node:module';
    delete process.env.SystemRoot; delete process.env.WINDIR;
    assert.equal(process.env.SystemRoot, undefined); assert.equal(process.env.WINDIR, undefined);
    process.env.SystemRoot=${JSON.stringify(mockSystemRoot)};
    Object.defineProperty(process,'platform',{value:${JSON.stringify(platform)}});
    const originalTemp=process.env.TEMP, originalTmp=process.env.TMP;
    let owned;
    process.on('exit',()=>{if(owned){assert.equal(existsSync(owned),false);process.stdout.write('owned_cleaned');}});
    child.spawnSync=(_file,_args,options)=>{
      owned=options.env.TEMP;
      assert.equal(options.env.TMP,owned);
      assert.equal(dirname(owned),realpathSync(originalTemp));
      assert.match(basename(owned),/^pinvou-relay-dacl-[A-Za-z0-9]+$/u);
      assert.equal(realpathSync(owned),owned);
      assert.ok(lstatSync(owned).isDirectory());
      assert.equal(process.env.TEMP,originalTemp);assert.equal(process.env.TMP,originalTmp);
      process.stdout.write('child_called'); return (${JSON.stringify(result)});
    };
    child.execFile=(...args)=>{ const done=args.at(-1); done(${verifyFails ? 'new Error("injected preflight rejection")' : 'null'},'ok',''); };
    child.execFile[Symbol.for('nodejs.util.promisify.custom')]=async()=>{
      ${verifyFails ? 'throw new Error("injected preflight rejection");' : 'return {stdout:"ok",stderr:""};'}
    };
    syncBuiltinESMExports(); await import(${JSON.stringify(runner)});`;
  return spawnSync(process.execPath, ['--input-type=module', '-e', program], { encoding: 'utf8', windowsHide: true,
    env: { ...process.env, TEMP: originalTemporary, TMP: originalTemporary, TMPDIR: originalTemporary } });
}

// These are runner decision tests; mocked TAP cannot qualify real permissions.
test('native qualification refuses non-Windows, no selected case, skip, and subprocess failure', () => {
  const valid = '# tests 1\n# pass 1\n# fail 0\n# skipped 0\n';
  assert.equal(invoke('linux', { status: 0, stdout: valid }).status, 1);
  for (const result of [
    { status: 0, stdout: '# tests 0\n# pass 0\n# fail 0\n# skipped 0\n' },
    { status: 0, stdout: '# tests 1\n# pass 0\n# fail 0\n# skipped 1\n' },
    { status: 1, stdout: valid },
    { status: null, stdout: valid },
  ]) {
    const invocation = invoke('win32', result);
    assert.equal(invocation.status, 1);
    assert.match(invocation.stdout, /child_called.*owned_cleaned/s);
    assert.doesNotMatch(invocation.stderr, /AssertionError/);
  }
  const accepted = invoke('win32', { status: 0, stdout: valid });
  assert.equal(accepted.status, 0);
  assert.match(accepted.stdout, /child_called.*owned_cleaned/s);
});

test('unsafe original temporary ancestors fail before starting the qualification child', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'pinvou-dacl-preflight-'));
  t.after(() => rm(root, { recursive: true }));
  const target = join(root, 'actual');
  const link = join(root, 'alias');
  const file = join(root, 'file');
  await mkdir(target);
  await symlink(target, link, process.platform === 'win32' ? 'junction' : 'dir');
  await writeFile(file, 'fixture');
  for (const options of [{ temporary: link }, { temporary: file }, { temporary: target, verifyFails: true }]) {
    const result = invoke('win32', { status: 0, stdout: '# tests 1\n# pass 1\n# fail 0\n# skipped 0\n' }, options);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /windows_relay_dacl_temporary_unsafe/);
    assert.doesNotMatch(result.stdout, /child_called/);
  }
});
