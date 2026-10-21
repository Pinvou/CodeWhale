const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const { prepareCmsdComponent } = require('../scripts/tauri/cmsd-component.js');

function fixture(t, architecture = 'aarch64') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-cmsd-package-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const source = path.join(root, 'package'); fs.mkdirSync(source);
  const binary = Buffer.alloc(32); binary.set([0x7f, 0x45, 0x4c, 0x46, 2, 1]);
  binary.writeUInt16LE(architecture === 'aarch64' ? 183 : 62, 18);
  fs.writeFileSync(path.join(source, 'cmsd'), binary);
  const manifest = { format: 'megacube-cmsd-component-v1', component: 'cmsd', version: '0.1.0',
    architecture, os: 'linux', api: 'v1', state_format: 'cmsd-json-v1', size: binary.length,
    sha256: crypto.createHash('sha256').update(binary).digest('hex') };
  const save = () => fs.writeFileSync(path.join(source, 'manifest.json'), JSON.stringify(manifest)); save();
  const invocations = [];
  return { root, source, manifest, save, invocations, options: { platform: 'linux', architecture: architecture === 'aarch64' ? 'arm64' : 'x64',
    environment: { PINVOU3_CMSD_PACKAGE: source }, tauriRoot: path.join(root, 'tauri'),
    spawn: (...args) => { invocations.push(args); return { status: 0 }; } } };
}

test('cmsd is an explicit Linux packaging input, never implicitly copied from a developer machine', () => {
  assert.equal(prepareCmsdComponent({ environment: {} }), null);
  assert.throws(() => prepareCmsdComponent({ platform: 'darwin', environment: { PINVOU3_CMSD_PACKAGE: '/unused' } }), /Linux/);
});

test('target architecture and Linux ELF policy gate staging without executing the payload', t => {
  for (const arch of ['aarch64', 'x86_64']) {
    const f = fixture(t, arch); const staged = prepareCmsdComponent(f.options);
    assert.equal(staged.manifest.architecture, arch);
    assert.equal(f.invocations.length, 1); assert.equal(f.invocations[0][0], 'bash');
    assert.match(f.invocations[0][1][0], /check-linux-elf-policy.sh$/);
    assert.equal(f.invocations[0][1][2], arch === 'aarch64' ? 'arm64' : 'amd64');
    const config = JSON.parse(staged.configSpec);
    assert.equal(config.bundle.resources[`target/cmsd-component/${arch}/cmsd`], 'runtime/cmsd/cmsd');
    assert.equal(fs.readFileSync(path.join(staged.directory, 'cmsd')).length, f.manifest.size);
    fs.writeFileSync(path.join(staged.directory, 'stale-private-file'), 'must not ship');
    prepareCmsdComponent(f.options);
    assert.deepEqual(fs.readdirSync(staged.directory).sort(), ['cmsd', 'manifest.json']);
  }
});

test('wrong target, altered content and linked files reject before staging', t => {
  const f = fixture(t);
  assert.throws(() => prepareCmsdComponent({ ...f.options, architecture: 'x64' }), /Incompatible/);
  fs.appendFileSync(path.join(f.source, 'cmsd'), 'changed');
  assert.throws(() => prepareCmsdComponent(f.options), /size mismatch/);
  fs.unlinkSync(path.join(f.source, 'cmsd')); fs.symlinkSync('/not-executed', path.join(f.source, 'cmsd'));
  assert.throws(() => prepareCmsdComponent(f.options), /Linked/);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri')), false);
});

test('failed ABI policy cannot create a candidate resource directory', t => {
  const f = fixture(t);
  assert.throws(() => prepareCmsdComponent({ ...f.options, spawn: () => ({ status: 1 }) }), /ELF compatibility/);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri')), false);
});

test('invalid or oversized manifests reject without trusting their own platform claims', t => {
  const f = fixture(t);
  for (const change of [{ size: 2 ** 32 }, { sha256: 'bad' }, { os: 'windows' }, { unexpected: true }, { api: 'v2' }]) {
    fs.writeFileSync(path.join(f.source, 'manifest.json'), JSON.stringify({ ...f.manifest, ...change }));
    assert.throws(() => prepareCmsdComponent(f.options), /manifest/);
  }
  fs.writeFileSync(path.join(f.source, 'manifest.json'), ' '.repeat(16385));
  assert.throws(() => prepareCmsdComponent(f.options), /manifest size/);
});
