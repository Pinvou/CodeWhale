import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { createServer } from './helpers/vite-fixture.mjs';

const root = fileURLToPath(new URL('..', import.meta.url));
const config = { root, configFile: false, logLevel: 'error', optimizeDeps: { noDiscovery: true } };

test('two middleware fixtures have private optimizer output and no default WebSocket listener', async () => {
  const servers = [];
  try {
    servers.push(await createServer({ ...config, server: { middlewareMode: true } }));
    servers.push(await createServer({ ...config, server: { middlewareMode: true } }));
    assert.notEqual(servers[0].config.cacheDir, servers[1].config.cacheDir);
    for (const server of servers) {
      assert.equal(server.config.server.hmr, false);
      assert.equal(server.config.server.ws, false);
      assert.ok(fs.existsSync(server.config.cacheDir));
    }
    await servers[0].close();
    await servers[0].close();
    assert.equal(fs.existsSync(servers[0].config.cacheDir), false);
    assert.equal(fs.existsSync(servers[1].config.cacheDir), true);
  } finally {
    const results = await Promise.allSettled(servers.map(server => server.close()));
    assert.ok(results.every(result => result.status === 'fulfilled'));
  }
});

test('two HTTP fixtures bind distinct dynamic ports and closing one leaves the other alive', async () => {
  const servers = [];
  try {
    for (let index = 0; index < 2; index++) {
      const server = await createServer(config);
      servers.push(server);
      await server.listen();
    }
    const ports = servers.map(server => server.httpServer.address().port);
    assert.notEqual(ports[0], ports[1]);
    await servers[0].close();
    assert.equal(servers[0].httpServer.listening, false);
    assert.equal(servers[1].httpServer.listening, true);
    const response = await fetch(`http://127.0.0.1:${ports[1]}/__fixture_lifecycle_ping__`);
    assert.equal(response.status, 404);
  } finally {
    const results = await Promise.allSettled(servers.map(server => server.close()));
    assert.ok(results.every(result => result.status === 'fulfilled'));
  }
});

test('native setup failure preserves its primary error and containment retires the private workspace', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-vite-setup-probe-'));
  try {
    const receipt = path.join(directory, 'probe.json');
    const module = new URL('./helpers/vite-fixture.mjs', import.meta.url).href;
    const source = `import { createServer } from ${JSON.stringify(module)};
      try { await createServer({configFile:false,server:{middlewareMode:true},plugins:[{name:'controlled-failure',configureServer(){throw new Error('controlled setup failure')}}]}); }
      catch (error) { console.error('PRIMARY_SETUP_PROBE:'+error.message); process.exit(7); }`;
    const python = process.env.PINVOU_CI_PYTHON || (process.platform === 'win32' ? 'python' : 'python3');
    // Includes interpreter/module startup and the bounded native-close probe.
    const result = spawnSync(python, [fileURLToPath(new URL('../../scripts/ci-fixture-process.py', import.meta.url)), '--receipt', receipt, '--', process.execPath, '--input-type=module', '-e', source], { encoding: 'utf8', timeout: 60_000, windowsHide: true });
    assert.equal(result.status, 7, result.stderr);
    assert.match(result.stderr, /PRIMARY_SETUP_PROBE:controlled setup failure/);
    const retired = JSON.parse(fs.readFileSync(receipt, 'utf8'));
    assert.equal(retired.state, 'released');
    assert.equal(retired.cleanup_failed, true);
    assert.equal(fs.existsSync(retired.workspace.path), false);
  } finally { fs.rmSync(directory, { recursive: true }); }
});
