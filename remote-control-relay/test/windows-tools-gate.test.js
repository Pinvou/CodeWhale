import assert from 'node:assert/strict';
import { test } from 'node:test';
import { assertTestSummary } from '../../scripts/windows-relay-tools-test-summary.mjs';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdtempSync, mkdirSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { requireWindowsPowerShell51, POWERSHELL_PREFLIGHT_TIMEOUT_MS } from '../../scripts/windows-relay-tools-preflight.mjs';

test('PowerShell preflight has a bounded cold-start budget and fails closed on every probe failure', () => {
  assert.equal(POWERSHELL_PREFLIGHT_TIMEOUT_MS, 60_000);
  let calls = 0;
  requireWindowsPowerShell51((command, args, options) => {
    calls++;
    assert.equal(command, 'powershell.exe');
    assert.deepEqual(args, ['-NoProfile', '-NonInteractive', '-Command', '$PSVersionTable.PSVersion.ToString()']);
    assert.equal(options.timeout, POWERSHELL_PREFLIGHT_TIMEOUT_MS);
    assert.equal(options.windowsHide, true);
    return { status: 0, stdout: '5.1.26100.7462\r\n' };
  });
  assert.equal(calls, 1);
  for (const [result, reason] of [
    [{ error: Object.assign(new Error('timeout'), { code: 'ETIMEDOUT' }), status: null, stdout: '5.1.26100.7462' }, /preflight timed out/],
    [{ error: Object.assign(new Error('missing'), { code: 'ENOENT' }) }, /could not start/],
    [{ status: 1, stdout: '5.1.26100.7462' }, /exited unsuccessfully/],
    [{ status: null, stdout: '5.1.26100.7462' }, /exited unsuccessfully/],
    ...['', '7.4.0', '5.1.garbage', '5.1.26100.7462\n7.4.0'].map((stdout) => [{ status: 0, stdout }, /unexpected version/]),
  ]) assert.throws(() => requireWindowsPowerShell51(() => result), reason);
});

const summary = '# tests 2\n# pass 2\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n';
test('Windows tools CLI rejects an unreadable authoritative probe error before executing tools', () => {
  const entry = new URL('../../scripts/run-windows-relay-tools-tests.mjs', import.meta.url).href;
  const script = `
    import assert from 'node:assert/strict';
    import child from 'node:child_process';
    import { syncBuiltinESMExports } from 'node:module';
    Object.defineProperty(process, 'platform', { value: 'win32' });
    let calls=0, reads=0, observed;
    console.error=value=>{observed=value;};
    const probe={status:0,stdout:'5.1.999'};
    Object.defineProperty(probe,'error',{get(){reads++;throw new Error('PRIVATE_SENTINEL');}});
    child.spawnSync=()=>{calls++;return calls===1?probe:{status:0,stdout:${JSON.stringify(summary)},stderr:''};};
    syncBuiltinESMExports();
    await assert.rejects(import(${JSON.stringify(entry)}),e=>e.message==='Windows PowerShell 5.1 required: could not start' && e.cause===undefined);
    assert.equal(calls,1);assert.equal(reads,1);
    assert.ok(!observed.includes('PRIVATE_SENTINEL'));
    const diagnostic=JSON.parse(observed.slice('windows_relay_tools_version_failure '.length));
    assert.equal(diagnostic.reason,'spawn_error');assert.equal(diagnostic.code,'unknown');
  `;
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script], { encoding: 'utf8', timeout: 15_000 });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 0, result.stderr);
});
test('Windows tools version probe emits only cached closed metadata and preserves its CLI rejection', () => {
  const entry = new URL('../../scripts/run-windows-relay-tools-tests.mjs', import.meta.url).href;
  for (const mode of ['timeout', 'missing', 'exit', 'version', 'changing', 'getter_throw', 'boxed', 'observer_throw', 'spawn_throw', 'success']) {
    const script = `
      import assert from 'node:assert/strict';
      import child from 'node:child_process';
      import { syncBuiltinESMExports } from 'node:module';
      Object.defineProperty(process, 'platform', { value: 'win32' });
      const mode = ${JSON.stringify(mode)};
      let calls = 0, reads = {}, forbidden = 0, emitted;
      const error = {};
      for (const key of ['message','stack','stdout','stderr','cmd','path','args','env'])
        Object.defineProperty(error,key,{get(){forbidden++;throw new Error('PRIVATE_SENTINEL');}});
      for (const [key, first] of [['code',mode==='missing'?'ENOENT':'ETIMEDOUT'],['errno',-1]])
        Object.defineProperty(error,key,{get(){reads[key]=(reads[key]??0)+1;
          if(mode==='getter_throw')throw new Error('PRIVATE_SENTINEL');
          if(mode==='boxed')return Object(first);
          return reads[key]===1?first:'PRIVATE_SENTINEL';}});
      const result={error:['timeout','missing','changing','getter_throw','boxed','observer_throw'].includes(mode)?error:undefined,
        stdout:mode==='version'?'7.4.0':'5.1.999'};
      Object.defineProperty(result,'status',{get(){reads.status=(reads.status??0)+1;return mode==='exit'?3:0;}});
      Object.defineProperty(result,'signal',{get(){reads.signal=(reads.signal??0)+1;return reads.signal===1?'SIGTERM':'PRIVATE_SENTINEL';}});
      const observer=console.error;
      console.error=(value)=>{emitted=value;if(mode==='observer_throw')throw new Error('PRIVATE_SENTINEL');};
      child.spawnSync=(executable,args,options)=>{
        calls++;
        if(calls===1){assert.equal(executable,'powershell.exe');assert.equal(options.timeout,60000);assert.equal(options.windowsHide,true);
          assert.deepEqual(args,['-NoProfile','-NonInteractive','-Command','$PSVersionTable.PSVersion.ToString()']);
          if(mode==='spawn_throw')throw error;return result;}
        assert.equal(mode,'success');return {status:0,stdout:${JSON.stringify(summary)},stderr:''};
      };
      syncBuiltinESMExports();
      try {
        if(mode==='success')await import(${JSON.stringify(entry)});
        else await assert.rejects(import(${JSON.stringify(entry)}),e=>e.message.startsWith('Windows PowerShell 5.1 required: ')
          && !e.message.includes('PRIVATE_SENTINEL') && e.cause===undefined);
      } finally {console.error=observer;}
      assert.equal(forbidden,0);assert.equal(calls,mode==='success'?4:1);
      if(mode==='success'){assert.equal(emitted,undefined);}
      else {
        assert.ok(!emitted.includes('PRIVATE_SENTINEL'));
        const payload=JSON.parse(emitted.slice('windows_relay_tools_version_failure '.length));
        assert.deepEqual(Object.keys(payload),['stage','reason','code','errno','status','signal']);
        assert.equal(payload.stage,'version_probe');
        assert.equal(payload.reason,mode==='exit'?'unsuccessful_exit':mode==='version'?'version_mismatch':'spawn_error');
        assert.equal(payload.code,['exit','version','getter_throw','boxed'].includes(mode)?'unknown':mode==='missing'?'ENOENT':'ETIMEDOUT');
        assert.equal(reads.signal,mode==='spawn_throw'?undefined:1);
        if(result.error||mode==='spawn_throw'){assert.equal(reads.code,1);assert.equal(reads.errno,1);assert.equal(reads.status,undefined);}
        else assert.equal(reads.status,1);
      }
    `;
    const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script], { encoding: 'utf8', timeout: 15_000 });
    assert.equal(result.error, undefined, mode);
    assert.equal(result.status, 0, `${mode}: ${result.stderr}`);
  }
});

test('Windows tools gate requires real successful execution with a complete summary', () => {
  assert.doesNotThrow(() => assertTestSummary(summary));
  for (const output of ['', summary.replace('# tests 2', '# tests 0'),
    summary.replace('# pass 2', '# pass 1'), summary + summary,
    ...['fail', 'cancelled', 'skipped', 'todo'].map((name) => summary.replace(`# ${name} 0`, `# ${name} 1`)),
    ...['tests', 'pass', 'fail', 'cancelled', 'skipped', 'todo'].map((name) => summary.replace(new RegExp(`^# ${name} .*\\n`, 'm'), ''))]) {
    assert.throws(() => assertTestSummary(output));
  }
});

test('Windows tools CLI cannot silently succeed through a linked directory', (t) => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-windows-tools-cli-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const scripts = join(directory, 'scripts');
  const alias = join(directory, 'linked-scripts');
  mkdirSync(scripts);
  for (const file of ['run-windows-relay-tools-tests.mjs', 'windows-relay-tools-test-summary.mjs', 'windows-relay-tools-preflight.mjs']) {
    copyFileSync(fileURLToPath(new URL('../../scripts/' + file, import.meta.url)), join(scripts, file));
  }
  symlinkSync(scripts, alias, process.platform === 'win32' ? 'junction' : 'dir');
  // No test files exist in this isolated root. Both entries MUST run and fail;
  // never execute the host's real suite or accept a lexical-entry no-op.
  for (const entry of [scripts, alias]) {
    const result = spawnSync(process.execPath, [join(entry, 'run-windows-relay-tools-tests.mjs')],
      { encoding: 'utf8', timeout: 90_000 });
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Native Windows execution required|Windows relay regression failed/);
  }
});
