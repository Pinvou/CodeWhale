const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const path = require('node:path');
const helper = path.resolve(__dirname, '../ci-artifact-upload-diagnostics.cjs');

test('passive receipt/network evidence preserves bytes and excludes signed URLs, headers and local paths', () => {
  const source = `
    const http=require('node:http'),fs=require('node:fs'),os=require('node:os'),path=require('node:path');
    const root=fs.mkdtempSync(path.join(os.tmpdir(),'artifact-trace-'));
    const file=path.join(root,'private-filename.json');fs.writeFileSync(file,'private-file-payload');
    let actual='';const server=http.createServer((req,res)=>{req.on('data',b=>actual+=b);req.on('end',()=>res.end('ok'));});
    server.listen(0,'127.0.0.1',()=>{
      const req=http.request({hostname:'127.0.0.1',port:server.address().port,path:'/private-path?sig=private-signature',method:'PUT',headers:{Authorization:'Bearer private-token'}},res=>{
        res.resume();res.on('end',()=>{if(actual!=='private-file-payload')process.exitCode=3;server.close(()=>{fs.rmSync(root,{recursive:true});console.log('unchanged-body');});});
      });req.on('error',e=>{console.error(e.code);process.exitCode=2;server.close();});fs.createReadStream(file).pipe(req);
    });`;
  const result = spawnSync(process.execPath, ['--require', helper, '-e', source], {
    env: { ...process.env, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1' }, encoding: 'utf8', timeout: 10000
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'unchanged-body');
  for (const secret of ['private-filename', 'private-file-payload', 'private-path', 'private-signature', 'private-token', 'Bearer', osTemp()]) {
    assert.ok(!result.stderr.includes(secret), secret);
  }
  const events = result.stderr.split('\n').filter(x => x.startsWith('[artifact-transport] ')).map(x => JSON.parse(x.slice(21)));
  assert.ok(events.some(x => x.kind === 'receipt_read' && x.phase === 'end' && x.bytes === 20));
  assert.ok(events.some(x => x.phase === 'response_headers' && x.status === 200));
  assert.ok(events.some(x => x.phase === 'exit' && x.pending === 0));
  assert.ok(events.length <= 160);
});

function osTemp() { return require('node:os').tmpdir(); }

test('many receipt reads retain the later network phases within the bounded log', () => {
  const source = `
    const fs=require('node:fs'),http=require('node:http'),os=require('node:os'),path=require('node:path');
    const root=fs.mkdtempSync(path.join(os.tmpdir(),'artifact-many-'));
    (async()=>{
      for(let i=0;i<64;i++){
        const file=path.join(root,'private-bulk-'+i+'.json');fs.writeFileSync(file,'{}');
        await new Promise((resolve,reject)=>{const s=fs.createReadStream(file);s.once('error',reject);s.once('close',resolve);s.resume();});
      }
      const server=http.createServer((req,res)=>res.end('ok'));
      server.listen(0,'127.0.0.1',()=>{
        const req=http.request({hostname:'127.0.0.1',port:server.address().port,path:'/private-bulk?sig=private-token'},res=>{
          res.resume();res.once('end',()=>server.close(()=>{fs.rmSync(root,{recursive:true});console.log('completed-64');}));
        });req.once('error',()=>{process.exitCode=2;server.close();});req.end();
      });
    })().catch(()=>{process.exitCode=3;});`;
  const result = spawnSync(process.execPath, ['--require', helper, '-e', source], {
    env: { ...process.env, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1' }, encoding: 'utf8', timeout: 15000
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'completed-64');
  assert.ok(!result.stderr.includes('private-bulk'));
  assert.ok(!result.stderr.includes('private-token'));
  const events = result.stderr.split('\n').filter(x => x.startsWith('[artifact-transport] ')).map(x => JSON.parse(x.slice(21)));
  assert.ok(events.some(x => x.phase === 'response_headers' && x.status === 200));
  const final = events.findLast(x => x.phase === 'exit');
  assert.deepEqual(final.receipt_reads, { created: 64, opened: 64, ended: 64, closed: 64, bytes: 128, errors: 0, active: 0 });
  assert.ok(events.length <= 160);
});

test('generated NODE_OPTIONS really preloads a path with spaces and preserves existing options', () => {
  const fs = require('node:fs');
  const { preloadOptions } = require(helper);
  const root = fs.mkdtempSync(path.join(osTemp(), 'artifact options '));
  try {
    const copy = path.join(root, 'trace module.cjs');
    fs.copyFileSync(helper, copy);
    const result = spawnSync(process.execPath, ['-e', 'console.log("preload-completed")'], {
      env: { ...process.env, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1', NODE_OPTIONS: preloadOptions('--max-old-space-size=4096', copy) },
      encoding: 'utf8', timeout: 10000
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), 'preload-completed');
    assert.match(result.stderr, /"phase":"enabled"/);
    assert.throws(() => preloadOptions('bad\noption', copy));
  } finally { fs.rmSync(root, { recursive: true }); }
});

test('error observation does not consume an unhandled stream error or make it successful', () => {
  const result = spawnSync(process.execPath, ['--require', helper, '-e', `require('node:fs').createReadStream('absent-owned-receipt.json')`], {
    env: { ...process.env, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1' }, encoding: 'utf8', timeout: 10000
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /"code":"ENOENT"/);
  assert.match(result.stderr, /Unhandled 'error' event/);
});

test('closed diagnostic stderr does not make a successful receipt read fail', () => {
  const source = `const fs=require('node:fs'),os=require('node:os'),path=require('node:path');
    const root=fs.mkdtempSync(path.join(os.tmpdir(),'artifact-closed-stderr-'));const file=path.join(root,'receipt.json');fs.writeFileSync(file,'{}');
    fs.closeSync(2);let ended=false;const stream=fs.createReadStream(file);
    stream.on('end',()=>{ended=true;}).on('close',()=>{if(!ended)throw new Error('read ended without data completion');fs.rmSync(root,{recursive:true});console.log('read-completed');});stream.resume();`;
  const result = spawnSync(process.execPath, ['--require', helper, '-e', source], {
    env: { ...process.env, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1' }, encoding: 'utf8', timeout: 10000
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'read-completed');
});
