'use strict';
// Compilation and caching use portable aliases. Each container still owns its
// original toolchain/cache directories; no mutable home is shared across jobs.
const path = require('node:path');
const os = require('node:os');
const fs = require('node:fs');
const crypto = require('node:crypto');

function prepare(env = process.env, base = '/tmp/pinvou-rust-cache-v1') {
  if (process.platform !== 'linux') throw new Error('portable Rust cache is Linux-only');
  const root = path.resolve(env.GITHUB_WORKSPACE || process.cwd());
  const cargo = path.resolve(env.CARGO_HOME || path.join(os.homedir(), '.cargo'));
  const rustup = path.resolve(env.RUSTUP_HOME || path.join(os.homedir(), '.rustup'));
  if ([root, cargo, rustup, base].some(value => /[\r\n\0]/.test(value))) throw new Error('invalid Rust cache path');
  // Self-hosted hosts may run several native listeners. A fixed alias is safe
  // only in the deployed one-listener containers, or a disposable hosted VM.
  const isolated = env.RUNNER_ENVIRONMENT !== 'self-hosted' || fs.existsSync('/.dockerenv');
  const aliases = isolated ? { cargo: path.join(base, 'cargo'), rustup: path.join(base, 'rustup') } : { cargo, rustup };
  const homes = { cargo, rustup };
  // Normalize only our known compiler wrapper. Forward to its real pathname so
  // its argv[0], sibling lookups and any future path-dependent behavior survive.
  const wrapper = path.join(root, 'pinvou3-app/src-tauri/scripts/rustc-stack-wrapper');
  const knownWrapper = (env.RUSTC_WRAPPER === wrapper || env.RUSTC_WRAPPER === path.join(base, 'rustc-stack-wrapper'))
    && fs.existsSync(wrapper) && fs.statSync(wrapper).isFile();
  if (knownWrapper && isolated) {
    aliases.wrapper = path.join(base, 'rustc-stack-wrapper');
  }
  if (isolated) {
    try { fs.mkdirSync(base, { mode: 0o700 }); } catch (error) { if (error.code !== 'EEXIST') throw error; }
    const stat = fs.lstatSync(base);
    if (stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== process.getuid() || (stat.mode & 0o022)) {
      throw new Error('cache alias root must be a real private owned directory');
    }
  }
  for (const [name, original] of isolated ? Object.entries(homes) : []) {
    if (name !== 'wrapper' && !fs.statSync(original).isDirectory()) throw new Error('Rust home must already exist');
    const alias = aliases[name];
    if (fs.existsSync(alias) || (() => { try { fs.lstatSync(alias); return true; } catch { return false; } })()) {
      if (!fs.lstatSync(alias).isSymbolicLink() || fs.realpathSync(alias) !== fs.realpathSync(original)) {
        throw new Error('cache alias belongs to a different Rust home');
      }
    } else fs.symlinkSync(original, alias, 'dir');
  }
  if (aliases.wrapper) {
    try {
      const stat = fs.lstatSync(aliases.wrapper);
      if (!stat.isFile() || stat.uid !== process.getuid() || (stat.mode & 0o022)) {
        throw new Error('compiler wrapper alias must be a private owned regular file');
      }
    } catch (error) { if (error.code !== 'ENOENT') throw error; }
    const temporary = path.join(base, `.wrapper-${crypto.randomBytes(12).toString('hex')}`);
    try {
      const quoted = "'" + wrapper.replaceAll("'", "'\\''") + "'";
      fs.writeFileSync(temporary, `#!/bin/sh\nexec ${quoted} "$@"\n`, { flag: 'wx', mode: 0o700 });
      fs.renameSync(temporary, aliases.wrapper);
    } finally { if (fs.existsSync(temporary)) fs.unlinkSync(temporary); }
  }
  const workspaces = (env.CACHE_WORKSPACES || '.').trim().split(/\r?\n/).filter(Boolean);
  const targets = workspaces.map(value => {
    const [source, target = 'target'] = value.split('->').map(x => x.trim());
    const relative = path.relative(root, path.resolve(root, source, target));
    if (relative.startsWith('../') || path.isAbsolute(relative) || !relative) {
      throw new Error('workspace cache target must stay inside the checkout');
    }
    return relative;
  });
  const values = {
    'cargo-home': aliases.cargo,
    'rustup-home': aliases.rustup,
    'portable': String(isolated),
    'rustc-wrapper': aliases.wrapper || env.RUSTC_WRAPPER || '',
    'wrapper-sha256': knownWrapper ? crypto.createHash('sha256').update(fs.readFileSync(wrapper)).digest('hex') : '',
    'cmd-format': env.RUSTC_WRAPPER === undefined ? 'env -u RUSTC_WRAPPER {0}' : '{0}',
    'config-sha256': (() => {
      const hash = crypto.createHash('sha256');
      for (const name of ['config', 'config.toml']) {
        const file = path.join(cargo, name);
        if (fs.existsSync(file)) hash.update(name + '\0').update(fs.readFileSync(file));
      }
      return hash.digest('hex');
    })(),
    // actions/cache stores home paths relative to its checkout cwd in tar.
    // Different depths must not consume the same archive even with equal ABI.
    'layout': `${env.RUNNER_ENVIRONMENT || 'unknown'}-depth-${root.split(path.sep).filter(Boolean).length}`,
    'targets': env.CACHE_TARGETS === 'false' ? '' : targets.join('\n'),
  };
  for (const value of Object.values(values)) {
    if (value.includes('\r') || value.includes('\0') || value.split('\n').includes('PINVOU_CACHE_EOF')) {
      throw new Error('invalid portable cache output');
    }
  }
  return values;
}

if (require.main === module) {
  const values = prepare();
  for (const [key, value] of Object.entries(values)) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, `${key}<<PINVOU_CACHE_EOF\n${value}\nPINVOU_CACHE_EOF\n`);
  }
  if (values.portable === 'true') {
    for (const [key, value] of Object.entries({ CARGO_HOME: values['cargo-home'], RUSTUP_HOME: values['rustup-home'],
      ...(values['wrapper-sha256'] ? { RUSTC_WRAPPER: values['rustc-wrapper'] } : {}) })) {
      fs.appendFileSync(process.env.GITHUB_ENV, `${key}=${value}\n`);
    }
  }
  console.log(`[ci-rust-cache] portable=${values.portable}; instance-owned homes preserved`);
}
module.exports = { prepare };
