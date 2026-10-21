// Explicit operator-selected artifact; never download or execute it while packaging.
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { APP_ROOT } = require('./platform-config.js');

function prepareCmsdComponent({ platform = process.platform, architecture = process.arch,
  environment = process.env, tauriRoot = path.join(APP_ROOT, 'src-tauri'), spawn = spawnSync } = {}) {
  const input = environment.PINVOU3_CMSD_PACKAGE;
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
module.exports = { prepareCmsdComponent };
