const assert = require('assert');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const repoRoot = path.resolve(__dirname, '..', '..');
const readRepo = (...segments) => fs.readFileSync(path.join(repoRoot, ...segments), 'utf8');

const windowsUpdater = readRepo(
  'pinvou3-app',
  'src-tauri',
  'src',
  'features',
  'updater',
  'platform',
  'windows.rs',
);
const platformModule = readRepo(
  'pinvou3-app',
  'src-tauri',
  'src',
  'features',
  'updater',
  'platform',
  'mod.rs',
);
const prepareScript = path.join(repoRoot, 'scripts', 'prepare-windows-release.ps1');
const onlineVerifyScript = path.join(
  repoRoot,
  'scripts',
  'verify-windows-release-online.ps1',
);

assert.match(windowsUpdater, /latest\.json 尚未发布 \{key\} 更新资产/);
assert.match(windowsUpdater, /Windows 更新资产 sha256 必须是 64 位十六进制/);
assert.match(windowsUpdater, /\.create_new\(true\)/);
assert.match(windowsUpdater, /Start-Process -FilePath .* -Verb RunAs -Wait -PassThru/);
assert.doesNotMatch(windowsUpdater, /package\/upgrade\/check|OtaInfo\.json|package_md5\.to_ascii_lowercase/);
assert.doesNotMatch(platformModule, /mod windows_domain_bootstrap/);
assert.ok(!fs.existsSync(path.join(
  repoRoot,
  'pinvou3-app',
  'src-tauri',
  'src',
  'features',
  'updater',
  'platform',
  'windows_domain_bootstrap.rs',
)));
assert.ok(fs.existsSync(prepareScript));
assert.ok(fs.existsSync(onlineVerifyScript));
const onlineVerifier = fs.readFileSync(onlineVerifyScript, 'utf8');
assert.match(onlineVerifier, /Invoke-WebRequest/);
assert.match(onlineVerifier, /Get-FileHash/);
assert.match(onlineVerifier, /Remove-Item -LiteralPath \$tempDirectory -Recurse -Force/);

if (process.platform === 'win32') {
  const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-windows-release-'));
  try {
    const version = fs.readFileSync(path.join(repoRoot, 'VERSION'), 'utf8').trim();
    const installerName = `pinvou3_${version}_x64-setup.exe`;
    const installerPath = path.join(tempRoot, installerName);
    const installerBytes = Buffer.from('signed-installer-fixture');
    fs.writeFileSync(installerPath, installerBytes);
    const expectedSha256 = crypto.createHash('sha256').update(installerBytes).digest('hex');

    const currentManifestPath = path.join(tempRoot, 'latest.json');
    fs.writeFileSync(currentManifestPath, JSON.stringify({
      version: '0.8.0',
      url: 'https://example.com/linux.deb',
      sha256: 'b'.repeat(64),
      platforms: {
        'macos-universal': {
          version: '0.8.0',
          url: 'https://example.com/macos.dmg',
          format: 'dmg',
          sha256: 'c'.repeat(64),
          size: 42,
        },
        'windows-x86_64': {
          version: '0.0.1',
          url: 'https://example.com/old.exe',
          format: 'nsis',
          sha256: 'd'.repeat(64),
          size: 10,
        },
      },
    }, null, 2));

    const outputManifest = path.join(tempRoot, 'latest.json.new');
    const outputInfo = path.join(tempRoot, 'release-info.json');
    const result = spawnSync('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-ExecutionPolicy',
      'Bypass',
      '-File',
      prepareScript,
      '-InstallerPath',
      installerPath,
      '-Version',
      version,
      '-GitCommit',
      '0123456789abcdef',
      '-ExpectedSha256',
      expectedSha256,
      '-Notes',
      'Windows static update contract',
      '-CurrentManifest',
      currentManifestPath,
      '-BaseUrl',
      'https://updates.example.com/pinvou3',
      '-OutputManifest',
      outputManifest,
      '-OutputReleaseInfo',
      outputInfo,
    ], { encoding: 'utf8' });
    assert.strictEqual(result.status, 0, result.stderr || result.stdout);

    const manifestBytes = fs.readFileSync(outputManifest);
    assert.notDeepStrictEqual([...manifestBytes.subarray(0, 3)], [0xef, 0xbb, 0xbf]);
    const manifest = JSON.parse(manifestBytes.toString('utf8'));
    assert.strictEqual(manifest.version, '0.8.0');
    assert.strictEqual(manifest.platforms['macos-universal'].version, '0.8.0');
    const windows = manifest.platforms['windows-x86_64'];
    assert.strictEqual(windows.version, version);
    assert.strictEqual(windows.format, 'nsis');
    assert.strictEqual(windows.size, installerBytes.length);
    assert.strictEqual(windows.sha256, expectedSha256);
    assert.strictEqual(
      windows.url,
      `https://updates.example.com/pinvou3/${installerName}`,
    );

    const info = JSON.parse(fs.readFileSync(outputInfo, 'utf8'));
    assert.strictEqual(info.git_commit, '0123456789abcdef');
    assert.strictEqual(info.sha256, windows.sha256);

    const rejectedManifest = path.join(tempRoot, 'rejected.json');
    const mismatch = spawnSync('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-ExecutionPolicy',
      'Bypass',
      '-File',
      prepareScript,
      '-InstallerPath',
      installerPath,
      '-Version',
      version,
      '-GitCommit',
      '0123456789abcdef',
      '-ExpectedSha256',
      '0'.repeat(64),
      '-Notes',
      'Rejected release',
      '-CurrentManifest',
      currentManifestPath,
      '-OutputManifest',
      rejectedManifest,
    ], { encoding: 'utf8' });
    assert.notStrictEqual(mismatch.status, 0);
    assert.ok(!fs.existsSync(rejectedManifest));
  } finally {
    fs.rmSync(tempRoot, { recursive: true, force: true });
  }
}

console.log('windows_static_update_contract: ok');
