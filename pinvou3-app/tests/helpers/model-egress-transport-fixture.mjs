// Own listeners, temporary test keys and the child command; no user credentials.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { once } from 'node:events';
import http from './http-fixture.mjs';
import https from './https-fixture.mjs';

const command = process.argv.slice(process.argv.indexOf('--') + 1);
assert.ok(process.argv.includes('--') && command.length, 'child command required after --');
const root = await mkdtemp(join(tmpdir(), 'pinvou-egress-tls-'));
const servers = [];
const counts = { primary: 0, vision: 0, redirect: 0, trap: 0, proxy: 0 };
const listen = async server => {
  servers.push(server); server.listen(0, '127.0.0.1'); await once(server, 'listening');
  return server.address().port;
};
try {
  let openssl = 'openssl';
  if (process.platform === 'win32') {
    const candidate = resolve(execFileSync('git', ['--exec-path'], {encoding: 'utf8'}).trim(), '../../../usr/bin/openssl.exe');
    if (existsSync(candidate)) openssl = candidate;
  }
  execFileSync(openssl, ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
    '-subj', '/CN=isolated-model-transport-fixture', '-addext', 'basicConstraints=critical,CA:TRUE',
    '-keyout', join(root, 'test-root.key'), '-out', join(root, 'test-root.pem')], {stdio: 'ignore'});
  execFileSync(openssl, ['req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=primary.invalid',
    '-keyout', join(root, 'test-only.key'), '-out', join(root, 'test-only.csr')], {stdio: 'ignore'});
  await writeFile(join(root, 'leaf.ext'), 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:primary.invalid,DNS:vision.invalid,IP:127.0.0.1\n');
  execFileSync(openssl, ['x509', '-req', '-in', join(root, 'test-only.csr'), '-CA', join(root, 'test-root.pem'),
    '-CAkey', join(root, 'test-root.key'), '-CAcreateserial', '-days', '1', '-extfile', join(root, 'leaf.ext'),
    '-out', join(root, 'test-only.pem')], {stdio: 'ignore'});
  const tls = {key: await readFile(join(root, 'test-only.key')), cert: await readFile(join(root, 'test-only.pem'))};
  const trapPort = await listen(https.createServer(tls, (_req, res) => {counts.trap++; res.end('unexpected redirect');}));
  const proxy = http.createServer((_req, res) => {counts.proxy++; res.writeHead(502).end();});
  proxy.on('connect', (_req, socket) => {counts.proxy++; socket.destroy();});
  const proxyPort = await listen(proxy);
  const model = role => https.createServer(tls, (req, res) => {
    if (req.url.endsWith('/redirect')) {
      counts.redirect++; res.writeHead(302, {Location: `https://127.0.0.1:${trapPort}/trap`}).end();
    } else {counts[role]++; res.end('owned TLS response');}
  });
  const primaryPort = await listen(model('primary'));
  const visionPort = await listen(model('vision'));
  const manifest = join(root, 'fixture.json');
  await writeFile(manifest, JSON.stringify({primary_url: `https://primary.invalid:${primaryPort}/v1`,
    vision_url: `https://vision.invalid:${visionPort}/v1`, ca_pem: await readFile(join(root, 'test-root.pem'), 'utf8')}));
  const proxyUrl = `http://127.0.0.1:${proxyPort}`;
  const child = spawn(command[0], command.slice(1), {stdio: 'inherit', shell: false,
    env: {...process.env, PINVOU_EGRESS_TRANSPORT_FIXTURE: manifest,
      HTTP_PROXY: proxyUrl, HTTPS_PROXY: proxyUrl, ALL_PROXY: proxyUrl,
      http_proxy: proxyUrl, https_proxy: proxyUrl, all_proxy: proxyUrl, NO_PROXY: '', no_proxy: ''}});
  const [code, signal] = await once(child, 'exit');
  assert.equal(signal, null, 'child must exit normally');
  assert.equal(code, 0, 'transport validation child failed');
  assert.deepEqual(counts, {primary: 2, vision: 2, redirect: 4, trap: 0, proxy: 0});
  process.stdout.write(`${JSON.stringify({result: 'isolated TLS transport passed', counts})}\n`);
} finally {
  await Promise.all(servers.map(server => new Promise((resolve, reject) => {
    server.closeAllConnections(); server.close(error => error ? reject(error) : resolve());
  })));
  await rm(root, {recursive: true, force: true});
}
