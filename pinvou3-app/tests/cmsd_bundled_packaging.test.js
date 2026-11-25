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

const { cmsdSourceTarget, prepareBundledCmsd } = require('../scripts/tauri/cmsd-component.js');

function sourceFixture(t, architecture = 'aarch64') {
  const f = fixture(t, architecture);
  const builder = path.join(f.root, 'components', 'cmsd', 'scripts', 'build-linux-component.py');
  fs.mkdirSync(path.dirname(builder), { recursive: true });
  fs.writeFileSync(builder, '# fixture; never executed');
  const calls = [];
  const options = { command: 'build', platform: 'linux', architecture: architecture === 'aarch64' ? 'arm64' : 'x64',
    environment: {}, repositoryRoot: f.root, tauriRoot: path.join(f.root, 'tauri'),
    spawn: (program, args, settings) => {
      calls.push({ program, args, settings });
      if (program === 'python3') {
        const output = args[args.indexOf('--output') + 1];
        fs.cpSync(f.source, output, { recursive: true });
        fs.writeFileSync(path.join(output, 'source.json'), JSON.stringify({
          repository_revision: 'a'.repeat(40), source_directory: 'components/cmsd',
          source_sha256: 'b'.repeat(64), dirty: false, build_revision: 'a'.repeat(40) }));
      }
      return { status: 0, stdout: 'a'.repeat(40) + '\n' };
    } };
  return { ...f, builder, calls, options };
}

test('build and standalone bundle compile the in-repository component and stage verified resources', t => {
  for (const [architecture, command] of [['aarch64', 'build'], ['x86_64', 'bundle']]) {
    const f = sourceFixture(t, architecture);
    const result = prepareBundledCmsd({ ...f.options, command });
    assert.equal(result.manifest.architecture, architecture);
    assert.deepEqual(f.calls.map(call => call.program), ['python3', 'git', 'bash']);
    assert.equal(f.calls[0].args[0], f.builder);
    assert.equal(f.calls[0].settings.cwd, f.root);
    assert.equal(f.calls[0].args.includes('--allow-dirty'), false);
    assert.equal(fs.readFileSync(path.join(result.directory, 'cmsd')).length, f.manifest.size);
    const sourceRoot = path.join(f.root, 'tauri', 'target', 'cmsd-source');
    assert.deepEqual(fs.readdirSync(sourceRoot), [`source-${f.options.architecture}.json`]);
    assert.deepEqual(fs.readdirSync(result.directory).sort(), ['cmsd', 'manifest.json']);
  }
});

test('dev, unrelated commands, non-Linux targets and unsupported LoongArch never compile CMS', t => {
  const f = sourceFixture(t);
  for (const override of [{ command: 'dev' }, { command: 'info' }, { command: undefined },
    { platform: 'darwin' }, { platform: 'win32' }, { architecture: 'loong64' },
    { targetTriple: 'loongarch64-unknown-linux-gnu' }, { targetTriple: 'aarch64-apple-darwin' },
    { targetTriple: 'x86_64-pc-windows-msvc' }]) {
    assert.equal(prepareBundledCmsd({ ...f.options, ...override }), null);
  }
  assert.deepEqual(f.calls, []);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri')), false);
});

test('Linux source build rejects wrong hosts, unsupported ABI and external package overrides before spawning', t => {
  const f = sourceFixture(t);
  for (const override of [
    { targetTriple: 'x86_64-unknown-linux-gnu' },
    { targetTriple: 'aarch64-unknown-linux-musl' },
    { targetTriple: 'riscv64gc-unknown-linux-gnu' },
    { platform: 'darwin', targetTriple: 'aarch64-unknown-linux-gnu' },
    { environment: { PINVOU3_CMSD_PACKAGE: f.source } },
  ]) assert.throws(() => prepareBundledCmsd({ ...f.options, ...override }), /cmsd|Linux/);
  assert.deepEqual(f.calls, []);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri')), false);
});

test('CLI target wins over Cargo defaults; caller sysroot and toolchain environment reach the source builder', t => {
  const f = sourceFixture(t);
  const environment = { CARGO_BUILD_TARGET: 'x86_64-unknown-linux-gnu', PINVOU3_CMSD_SYSROOT: '/fixture/sysroot',
    CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER: '/fixture/linker', CARGO_NET_OFFLINE: 'true' };
  prepareBundledCmsd({ ...f.options, targetTriple: 'aarch64-unknown-linux-gnu', environment });
  assert.deepEqual(f.calls[0].settings.env, environment);
  assert.deepEqual(f.calls[0].args.slice(-2), ['--sysroot', '/fixture/sysroot']);
  assert.throws(() => cmsdSourceTarget({ platform: 'linux', architecture: 'arm64', environment }), /native Linux/);
});

test('missing source and failed source compilation cannot fall back to an old bundled payload', t => {
  const f = sourceFixture(t);
  fs.rmSync(f.builder);
  assert.throws(() => prepareBundledCmsd(f.options), /source builder is missing/);
  assert.deepEqual(f.calls, []);
  fs.writeFileSync(f.builder, '# restored fixture');
  assert.throws(() => prepareBundledCmsd({ ...f.options, spawn: () => ({ status: 1 }) }), /Failed to build/);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri', 'target', 'cmsd-component')), false);
  assert.deepEqual(fs.readdirSync(path.join(f.root, 'tauri', 'target', 'cmsd-source')), []);
});

test('source build still rejects a tampered binary through the shared checksum and ELF policy', t => {
  const f = sourceFixture(t);
  fs.appendFileSync(path.join(f.source, 'cmsd'), 'tampered');
  assert.throws(() => prepareBundledCmsd(f.options), /size mismatch/);
  assert.deepEqual(f.calls.map(call => call.program), ['python3', 'git']);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri', 'target', 'cmsd-component')), false);
});

test('missing source provenance rejects before a resource candidate is staged', t => {
  const f = sourceFixture(t);
  const spawn = (program, args, settings) => {
    const result = f.options.spawn(program, args, settings);
    if (program === 'python3') fs.rmSync(path.join(args[args.indexOf('--output') + 1], 'source.json'));
    return result;
  };
  assert.throws(() => prepareBundledCmsd({ ...f.options, spawn }), /missing source.json/);
  assert.equal(fs.existsSync(path.join(f.root, 'tauri', 'target', 'cmsd-component')), false);
});

test('actual Tauri build and bundle orchestration include the source component overlay', async () => {
  const vm = require('node:vm');
  const { EventEmitter } = require('node:events');
  const { createRequire } = require('node:module');
  const buildScript = path.resolve(__dirname, '../scripts/tauri/build.js');
  const localRequire = createRequire(buildScript);
  for (const command of ['build', 'bundle']) {
    const calls = [];
    const componentConfig = JSON.stringify({ bundle: { resources: { 'cms-source-fixture': 'runtime/cmsd/cmsd' } } });
    const requireFixture = name => {
      if (name === './cmsd-component.js') return { prepareBundledCmsd: options => {
        calls.push(options); return { configSpec: componentConfig };
      } };
      if (name === './effective-config.js') return { writeEffectiveArtifacts: configs => {
        assert.ok(configs.includes(componentConfig));
        return { effectiveConfigPath: 'fixture', resourceManifestPath: 'fixture', resourceManifest: { resourceFileCount: 2 } };
      } };
      if (name === './codex-bridge.js') return { prepareCodexBridge: () => {}, prepareWindowsCodexBridge: () => {} };
      if (name === './linux-asr-runtime.js') return { prepareLinuxAsrRuntime: () => {} };
      if (name === './knowledge-host.js') return { prepareKnowledgeHost: () => {} };
      if (name === 'node:child_process') return { spawn: (_program, args) => {
        assert.ok(args.includes(componentConfig));
        const child = new EventEmitter();
        queueMicrotask(() => child.emit('exit', 0, null));
        return child;
      } };
      return localRequire(name);
    };
    requireFixture.resolve = localRequire.resolve;
    const exported = { exports: {} };
    vm.runInNewContext(fs.readFileSync(buildScript, 'utf8'), { require: requireFixture, module: exported,
      process: { argv: ['node', buildScript, command, '--target', 'x86_64-unknown-linux-gnu'],
        platform: 'linux', arch: 'x64', env: {}, execPath: process.execPath },
      console: { log: () => {} }, setInterval, clearInterval });
    assert.equal(await exported.exports.main(), 0);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].command, command);
    assert.equal(calls[0].targetTriple, 'x86_64-unknown-linux-gnu');
  }
});

test('source receipt must identify a committed current repository revision before resource staging', t => {
  const f = sourceFixture(t);
  const invalid = [
    '', '{}', 'null', '[]', '{',
    { dirty: true }, { dirty: 'false' }, { repository_revision: 'short' },
    { repository_revision: 'c'.repeat(40), build_revision: 'c'.repeat(40) },
    { build_revision: 'development:uncommitted' }, { source_sha256: 'bad' },
    { source_directory: '../another-cmsd' }, { extra: true },
  ];
  for (const change of invalid) {
    const spawn = (program, args, settings) => {
      const result = f.options.spawn(program, args, settings);
      if (program === 'python3') {
        const file = path.join(args[args.indexOf('--output') + 1], 'source.json');
        const receipt = JSON.parse(fs.readFileSync(file));
        fs.writeFileSync(file, typeof change === 'string' ? change : JSON.stringify({ ...receipt, ...change }));
      }
      return result;
    };
    assert.throws(() => prepareBundledCmsd({ ...f.options, spawn }), /source provenance/);
    assert.equal(fs.existsSync(path.join(f.root, 'tauri', 'target', 'cmsd-component')), false);
  }
  for (const failure of [{ status: 128, stdout: '' }, { status: 0, stdout: 'stale' }, { error: new Error('missing git') }]) {
    const spawn = (program, args, settings) => program === 'git' ? failure : f.options.spawn(program, args, settings);
    assert.throws(() => prepareBundledCmsd({ ...f.options, spawn }), /current Pinvou repository HEAD/);
  }
});
