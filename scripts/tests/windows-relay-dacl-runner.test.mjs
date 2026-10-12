import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const runner = new URL('../run-windows-relay-dacl.mjs', import.meta.url).href;
function invoke(platform, result, { temporary, verifyFails = false, verifyFailureAt,
  failure = {}, spawnThrows = false, accessorMode } = {}) {
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
    let verification=0;
    const reads={code:0,errno:0,killed:0,signal:0};
    const accessorMode=${JSON.stringify(accessorMode ?? null)};
    const failure=()=>{
      const error=Object.assign(new Error('private-path-token-sentinel'),${JSON.stringify(failure)},
        {stack:'private-path-token-sentinel',stdout:'private-path-token-sentinel',stderr:'private-path-token-sentinel'});
      if(accessorMode)for(const [field,value]of Object.entries({code:'ETIMEDOUT',errno:-110,killed:true,signal:'SIGTERM'})){
        Object.defineProperty(error,field,{get(){
          reads[field]++;
          if(accessorMode==='throws')throw new Error('private-path-token-sentinel');
          if(accessorMode==='boxed')return {toJSON(){return 'private-path-token-sentinel';}};
          return reads[field]===1?value:'private-path-token-sentinel';
        }});
      }
      return error;
    };
    let owned;
    process.on('exit',()=>{
      if(owned){assert.equal(existsSync(owned),false);process.stdout.write('owned_cleaned');}
      if(accessorMode)assert.deepEqual(reads,{code:1,errno:1,killed:1,signal:1});
    });
    child.spawnSync=(_file,_args,options)=>{
      owned=options.env.TEMP;
      assert.equal(options.env.TMP,owned);
      assert.equal(dirname(owned),realpathSync(originalTemp));
      assert.match(basename(owned),/^pinvou-relay-dacl-[A-Za-z0-9]+$/u);
      assert.equal(realpathSync(owned),owned);
      assert.ok(lstatSync(owned).isDirectory());
      assert.equal(process.env.TEMP,originalTemp);assert.equal(process.env.TMP,originalTmp);
      process.stdout.write('child_called');
      ${spawnThrows ? 'throw failure();' : `return (${JSON.stringify(result)});`}
    };
    child.execFile=(...args)=>{ const done=args.at(-1); done(${verifyFails ? 'new Error("injected preflight rejection")' : 'null'},'ok',''); };
    child.execFile[Symbol.for('nodejs.util.promisify.custom')]=async(...args)=>{
      verification++;
      const paths=JSON.parse(Buffer.from(args[2].env.PINVOU_PATHS_B64,'base64').toString('utf8'));
      const last=paths.at(-1);
      if(last&&/^pinvou-relay-dacl-[A-Za-z0-9]+$/u.test(basename(last)))owned=last;
      if(verification===${JSON.stringify(verifyFailureAt ?? null)})throw failure();
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
  assert.doesNotMatch(accepted.stderr, /windows_relay_dacl_stage_failure/);
});

test('failure diagnostics identify bounded stages without exception payloads', () => {
  const valid = { status: 0, stdout: '# tests 1\n# pass 1\n# fail 0\n# skipped 0\n' };
  for (const [options, expected] of [
    [{ verifyFailureAt: 1, failure: { code: 'ETIMEDOUT', errno: -110, killed: true, signal: 'SIGTERM' } },
      { stage: 'base_reparse', code: 'ETIMEDOUT', errno: -110, killed: true, signal: 'SIGTERM' }],
    [{ verifyFailureAt: 2, failure: { code: 'EPERM', errno: -1 } },
      { stage: 'owned_reparse', code: 'EPERM', errno: -1 }],
    [{ spawnThrows: true, failure: { code: 'private-path-token-sentinel', errno: 1e30,
      killed: 'private-path-token-sentinel', signal: 'private-path-token-sentinel' } },
      { stage: 'child_spawn', code: 'unknown' }],
  ]) {
    const result = invoke('win32', valid, options);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /windows_relay_dacl_temporary_unsafe/);
    const lines = result.stderr.split(/\r?\n/u).filter((line) => line.startsWith('windows_relay_dacl_stage_failure '));
    assert.equal(lines.length, 1);
    assert.deepEqual(JSON.parse(lines[0].slice('windows_relay_dacl_stage_failure '.length)), expected);
    assert.doesNotMatch(result.stdout + result.stderr, /private-path-token-sentinel|AssertionError/);
    if (options.spawnThrows) assert.match(result.stdout, /child_called.*owned_cleaned/s);
    else assert.doesNotMatch(result.stdout, /child_called/);
    if (options.verifyFailureAt === 2) assert.match(result.stdout, /owned_cleaned/);
  }
});

test('diagnostic fields read each accessor once and reject boxed or throwing values', () => {
  for (const accessorMode of ['changing', 'throws', 'boxed']) {
    const result = invoke('win32', {}, { verifyFailureAt: 1, accessorMode });
    assert.equal(result.status, 1);
    const lines = result.stderr.split(/\r?\n/u).filter((line) => line.startsWith('windows_relay_dacl_stage_failure '));
    assert.equal(lines.length, 1);
    assert.deepEqual(JSON.parse(lines[0].slice('windows_relay_dacl_stage_failure '.length)),
      accessorMode === 'changing'
        ? { stage: 'base_reparse', code: 'ETIMEDOUT', errno: -110, killed: true, signal: 'SIGTERM' }
        : { stage: 'base_reparse', code: 'unknown' });
    assert.doesNotMatch(result.stdout + result.stderr, /private-path-token-sentinel|AssertionError|child_called/);
  }
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
