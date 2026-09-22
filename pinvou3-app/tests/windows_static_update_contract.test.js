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
const windowsUpdaterProduction = windowsUpdater.split('\n#[cfg(test)]', 1)[0];
const platformModule = readRepo(
  'pinvou3-app',
  'src-tauri',
  'src',
  'features',
  'updater',
  'platform',
  'mod.rs',
);
const linuxUpdater = readRepo(
  'pinvou3-app', 'src-tauri', 'src', 'features', 'updater', 'platform', 'linux.rs',
);
const macosUpdater = readRepo(
  'pinvou3-app', 'src-tauri', 'src', 'features', 'updater', 'platform', 'macos.rs',
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
assert.match(windowsUpdater, /-EncodedCommand','\{encoded_script\}'\) -Verb RunAs/);
assert.match(windowsUpdater, /\[IO\.FileShare\]::Read/);
assert.match(windowsUpdater, /installer SHA-256 does not match manifest/);
assert.match(windowsUpdater, /@\('\/P','\/UPDATE','\/R'\)/);
assert.match(windowsUpdater, /static APPROVED_ASSET: Mutex<Option<ApprovedAsset>>/);
assert.match(windowsUpdater, /approval_receipt/);
assert.match(windowsUpdater, /mark_approved_downloaded/);
assert.match(windowsUpdater, /GetSystemDirectoryW/);
assert.match(windowsUpdater, /Move-Item -LiteralPath \{pending_state\} -Destination \{claimed_state\}/);
assert.match(windowsUpdater, /marker_payload\(&markers\.started, &markers\.nonce\)/);
assert.doesNotMatch(windowsUpdater, /HiddenCommand::new\("powershell\.exe"\)/);
assert.doesNotMatch(windowsUpdater, /Start-Process -FilePath 'msiexec\.exe'/);
assert.doesNotMatch(windowsUpdaterProduction, /Start-Process -FilePath \$exe/);
assert.doesNotMatch(
  windowsUpdaterProduction,
  /Microsoft\.Win32\.Registry|InstallLocation|current_install_dir|ProgramFiles/,
);
assert.doesNotMatch(windowsUpdater, /Get-AuthenticodeSignature/);
assert.doesNotMatch(windowsUpdater, /package\/upgrade\/check|OtaInfo\.json|package_md5\.to_ascii_lowercase/);
const updaterModule = readRepo(
  'pinvou3-app',
  'src-tauri',
  'src',
  'features',
  'updater',
  'mod.rs',
);
assert.match(updaterModule, /static UPDATE_OPERATION: tokio::sync::Mutex/);
assert.match(updaterModule, /DOWNLOAD_CANCEL\.store\(false, Ordering::SeqCst\)/);
assert.match(updaterModule, /const UPDATE_MANIFEST_URL: &str = "https:\/\/pinvou\.com\/pinvou3\/latest\.json"/);
assert.match(
  updaterModule,
  /fn manifest_url\(\).*?#\[cfg\(any\(debug_assertions, test\)\)\].*?std::env::var\("PINVOU3_UPDATE_URL"\).*?is_loopback_update_url.*?UPDATE_MANIFEST_URL\.to_string\(\)/s,
);
assert.match(updaterModule, /Policy::custom/);
assert.match(updaterModule, /redirect_chain_allowed\(boundary, attempt\.previous\(\), attempt\.url\(\)\)/);
assert.match(updaterModule, /update_client_builder\(&manifest_url\)/);
for (const source of [windowsUpdater, linuxUpdater, macosUpdater]) {
  assert.ok(
    source.match(/wait_download_step/g)?.length >= 2,
    'each platform must cancel/timeout both response headers and response chunks',
  );
  assert.doesNotMatch(source, /cancel\.store\(false, Ordering::SeqCst\)/);
  assert.match(source, /update_client_builder\(/);
  assert.doesNotMatch(source, /reqwest::Client::builder\(\)/);
}
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
