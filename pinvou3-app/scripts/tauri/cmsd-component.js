// Source builds and verified resource staging are kept outside pure Tauri config derivation.
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { APP_ROOT } = require('./platform-config.js');

function prepareCmsdComponent({ platform = process.platform, architecture = process.arch,
  environment = process.env, packageDirectory = environment.PINVOU3_CMSD_PACKAGE,
  tauriRoot = path.join(APP_ROOT, 'src-tauri'), spawn = spawnSync } = {}) {
  const input = packageDirectory;
  if (!input) return null;
  if (platform !== 'linux') throw new Error('Bundled cmsd requires a Linux target');
  const arch = { arm64: ['aarch64', 183], x64: ['x86_64', 62] }[architecture];
  if (!arch) throw new Error(`Unsupported cmsd target architecture: ${architecture}`);
  const source = path.resolve(input);
  if (!fs.lstatSync(source).isDirectory() || fs.lstatSync(source).isSymbolicLink()) throw new Error('Invalid cmsd package directory');
  for (const name of ['cmsd', 'manifest.json']) {
    const stat = fs.lstatSync(path.join(source, name));
    if (!stat.isFile() || stat.isSymbolicLink()) throw new Error(`Linked or invalid cmsd package file: ${name}`);
  }
  if (fs.statSync(path.join(source, 'manifest.json')).size > 16384) throw new Error('Invalid cmsd manifest size');
  const raw = fs.readFileSync(path.join(source, 'manifest.json'));
  if (raw.length > 16384) throw new Error('Invalid cmsd manifest size');
  const manifest = JSON.parse(raw);
  const keys = ['format', 'component', 'version', 'architecture', 'os', 'api', 'state_format', 'sha256', 'size'];
  if (Object.keys(manifest).some(key => !keys.includes(key)) || keys.some(key => !(key in manifest)) ||
      manifest.format !== 'megacube-cmsd-component-v1' || manifest.component !== 'cmsd' ||
      manifest.os !== 'linux' || manifest.architecture !== arch[0] || manifest.api !== 'v1' ||
      manifest.state_format !== 'cmsd-json-v1' || !/^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(manifest.version) ||
      !/^[a-f0-9]{64}$/.test(manifest.sha256) || !Number.isSafeInteger(manifest.size) || manifest.size < 20 || manifest.size > 256 * 1024 * 1024) {
    throw new Error('Incompatible cmsd component manifest');
  }
  if (fs.statSync(path.join(source, 'cmsd')).size !== manifest.size) throw new Error('cmsd binary size mismatch');
  const binary = fs.readFileSync(path.join(source, 'cmsd'));
  if (binary.length !== manifest.size || crypto.createHash('sha256').update(binary).digest('hex') !== manifest.sha256 ||
      !binary.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46])) || binary[4] !== 2 || binary[5] !== 1 || binary.readUInt16LE(18) !== arch[1]) {
    throw new Error('cmsd binary checksum, size or ELF architecture mismatch');
  }
  const policy = spawn('bash', [path.join(APP_ROOT, '..', 'scripts', 'check-linux-elf-policy.sh'),
    path.join(source, 'cmsd'), architecture === 'arm64' ? 'arm64' : 'amd64'], { cwd: APP_ROOT, env: { ...environment }, stdio: 'inherit' });
  if (policy.error || policy.status !== 0) throw new Error('Bundled cmsd failed Linux ELF compatibility policy');
  const directory = path.join(tauriRoot, 'target', 'cmsd-component', arch[0]);
  fs.mkdirSync(path.dirname(directory), { recursive: true });
  // A complete private staging directory prevents stale files from entering a later package.
  const temporary = fs.mkdtempSync(directory + '.stage-');
  try {
    fs.writeFileSync(path.join(temporary, 'cmsd'), binary, { mode: 0o755 });
    fs.writeFileSync(path.join(temporary, 'manifest.json'), raw, { mode: 0o644 });
    fs.rmSync(directory, { recursive: true, force: true });
    fs.renameSync(temporary, directory);
  } finally { fs.rmSync(temporary, { recursive: true, force: true }); }
  return { directory, manifest, configSpec: JSON.stringify({ bundle: { resources: {
    [`target/cmsd-component/${arch[0]}/cmsd`]: 'runtime/cmsd/cmsd',
    [`target/cmsd-component/${arch[0]}/manifest.json`]: 'runtime/cmsd/manifest.json',
  } } }) };
}
/** Resolve the component from the requested target, never from host architecture alone. */
function cmsdSourceTarget({ platform = process.platform, architecture = process.arch,
  targetTriple = null, environment = process.env } = {}) {
  const triple = targetTriple || environment.CARGO_BUILD_TARGET || null;
  if (triple && !triple.includes('-linux-')) return null;
  if (!triple && platform !== 'linux') return null;
  // Pinvou supports LoongArch, but CMS does not yet ship on that platform.
  if ((triple && triple.startsWith('loongarch64-')) || (!triple && architecture === 'loong64')) return null;
  const targetArchitecture = triple
    ? { 'x86_64-unknown-linux-gnu': 'x64', 'aarch64-unknown-linux-gnu': 'arm64' }[triple]
    : architecture;
  if (!['x64', 'arm64'].includes(targetArchitecture)) {
    throw new Error(`Unsupported cmsd Linux target: ${triple || targetArchitecture}; only GNU x86_64/aarch64 are supported`);
  }
  if (platform !== 'linux' || targetArchitecture !== architecture) {
    throw new Error(`cmsd source builds require native Linux ${targetArchitecture}; host is ${platform}/${architecture}`);
  }
  return { architecture: targetArchitecture,
    triple: triple || (architecture === 'arm64' ? 'aarch64-unknown-linux-gnu' : 'x86_64-unknown-linux-gnu') };
}

/** Build the committed in-repository crate for both build and standalone bundle. */
function prepareBundledCmsd({ command, platform = process.platform, architecture = process.arch,
  targetTriple = null, environment = process.env, repositoryRoot = path.join(APP_ROOT, '..'),
  tauriRoot = path.join(APP_ROOT, 'src-tauri'), spawn = spawnSync } = {}) {
  if (!['build', 'bundle'].includes(command)) return null;
  const target = cmsdSourceTarget({ platform, architecture, targetTriple, environment });
  if (!target) return null;
  if (environment.PINVOU3_CMSD_PACKAGE) {
    throw new Error('PINVOU3_CMSD_PACKAGE cannot override a Pinvou build: cmsd must be built from components/cmsd in this repository');
  }
  const builder = path.join(repositoryRoot, 'components', 'cmsd', 'scripts', 'build-linux-component.py');
  if (!fs.existsSync(builder)) throw new Error(`cmsd source builder is missing: ${builder}`);
  const stagingRoot = path.join(tauriRoot, 'target', 'cmsd-source');
  fs.mkdirSync(stagingRoot, { recursive: true });
  const temporary = fs.mkdtempSync(path.join(stagingRoot, 'package-'));
  const output = path.join(temporary, 'component');
  const args = [builder, '--output', output, '--target-dir', path.join(stagingRoot, 'cargo')];
  if (environment.PINVOU3_CMSD_SYSROOT) args.push('--sysroot', environment.PINVOU3_CMSD_SYSROOT);
  try {
    const build = spawn('python3', args, { cwd: repositoryRoot, env: { ...environment }, stdio: 'inherit' });
    if (build.error || build.status !== 0) {
      throw new Error(`Failed to build bundled cmsd from repository source (exit ${build.status ?? 'unknown'}): ${build.error?.message || builder}`);
    }
    const provenance = path.join(output, 'source.json');
    if (!fs.existsSync(provenance)) throw new Error('Source-built cmsd is missing source.json provenance');
    const stat = fs.lstatSync(provenance);
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 16384) {
      throw new Error('Invalid cmsd source provenance file');
    }
    const receipt = fs.readFileSync(provenance);
    let source;
    try { source = JSON.parse(receipt); } catch { throw new Error('Invalid cmsd source provenance JSON'); }
    const keys = ['repository_revision', 'source_directory', 'source_sha256', 'dirty', 'build_revision'];
    if (receipt.length > 16384 || !source || typeof source !== 'object' || Array.isArray(source) ||
        Object.keys(source).length !== keys.length || keys.some(key => !(key in source)) ||
        source.source_directory !== 'components/cmsd' || source.dirty !== false ||
        !/^[a-f0-9]{40}(?:[a-f0-9]{24})?$/.test(source.repository_revision) ||
        source.build_revision !== source.repository_revision || !/^[a-f0-9]{64}$/.test(source.source_sha256)) {
      throw new Error('Invalid or uncommitted cmsd source provenance');
    }
    const revision = spawn('git', ['rev-parse', '--verify', 'HEAD'], { cwd: repositoryRoot,
      env: { ...environment }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    if (revision.error || revision.status !== 0 || typeof revision.stdout !== 'string' ||
        revision.stdout.trim() !== source.repository_revision) {
      throw new Error('cmsd source provenance does not match the current Pinvou repository HEAD');
    }
    const staged = prepareCmsdComponent({ platform: 'linux', architecture: target.architecture,
      packageDirectory: output, environment, tauriRoot, spawn });
    // Retain the verified receipt without shipping local build metadata as an app resource.
    fs.writeFileSync(path.join(stagingRoot, `source-${target.architecture}.json`), receipt);
    return staged;
  } finally { fs.rmSync(temporary, { recursive: true, force: true }); }
}
module.exports = { cmsdSourceTarget, prepareBundledCmsd, prepareCmsdComponent };
