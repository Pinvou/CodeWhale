// Screenshot raster-origin tests (Pinvou/pinvou-agent#554): after any
// returned raster, coordinate targets are raster pixels and the server maps
// them to screen points through the bound raster — so a screenshot must bind
// the geometry of the crop actually taken, exactly as the zoom contract does.
// All three backends run here without a desktop: darwin through an injected
// exec, linux and win32 through PATH shims (grim/swaymsg, powershell.exe).
import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import url from "node:url";

const __dirname = path.dirname(url.fileURLToPath(import.meta.url));
const workDir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-shot-origin-"));
process.env.CODEWHALE_CU_RECORDINGS_DIR = path.join(workDir, "rec");
fs.mkdirSync(process.env.CODEWHALE_CU_RECORDINGS_DIR, { recursive: true });

const envSnapshot = { ...process.env };
const managed = ["WAYLAND_DISPLAY", "DISPLAY", "XDG_SESSION_TYPE", "CU_SHIM_LOG", "CU_SHIM_SHOT", "CU_PS_SCRIPT_LOG", "CU_PS_SHOT", "CU_PS_JSON"];

/** Run fn under a scoped env (shim dir on PATH, session vars, shim config). */
async function withEnv(extra, fn) {
  const saved = { ...process.env };
  try {
    for (const k of managed) delete process.env[k];
    for (const [k, v] of Object.entries(extra)) process.env[k] = v;
    // Await inside the try: the env must survive every await in fn.
    return await fn();
  } finally {
    for (const k of managed) delete process.env[k];
    for (const [k, v] of Object.entries(saved)) process.env[k] = v;
  }
}

// ---- darwin: injected exec (no macOS tools needed) ----

function darwinExec({ resolution = "2560x1440", pixels = null, finderBounds = "0, 0, 2560, 1440", sipsSize = null, extraDisplays = [] } = {}) {
  const impl = { resolution, pixels, finderBounds, sipsSize, extraDisplays };
  const calls = [];
  return {
    calls,
    impl,
    async run(cmd, args) {
      calls.push([cmd, ...args]);
      if (cmd === "screencapture") {
        fs.writeFileSync(args[args.length - 1], Buffer.from("89504e470d0a1a0a0000000d49484452", "hex")); // PNG signature stub
        return { code: 0, stdout: "", stderr: "" };
      }
      if (cmd === "sips") {
        // No stubbed size => unparseable probe (binding falls back to metadata).
        if (!impl.sipsSize) return { code: 1, stdout: "", stderr: "no stubbed size" };
        return { code: 0, stdout: `stub.png: pixelWidth: ${impl.sipsSize.w}\n             pixelHeight: ${impl.sipsSize.h}`, stderr: "" };
      }
      if (cmd === "system_profiler") {
        const main = { _name: "Built-In", spdisplays_main: "y", _spdisplays_resolution: impl.resolution };
        if (impl.pixels) main._spdisplays_pixels = impl.pixels;
        return {
          code: 0,
          stdout: JSON.stringify({ SPDisplaysDataType: [{ spdisplays_ndrvs: [main, ...impl.extraDisplays] }] }),
          stderr: "",
        };
      }
      if (cmd === "osascript") {
        if (impl.finderBounds == null) return { code: 1, stdout: "", stderr: "Finder got an error" };
        return { code: 0, stdout: `${impl.finderBounds}\n`, stderr: "" };
      }
      return { code: 0, stdout: "", stderr: "" };
    },
  };
}

async function darwinBackend(impl) {
  const mod = await import("../src/backends/darwin.mjs");
  const exec = darwinExec(impl);
  return { b: mod.create({ exec }), exec };
}

test("darwin: a region screenshot binds the global region origin, not the display bounds", async () => {
  const { b } = await darwinBackend();
  const shot = await b.screenshot({ region: [100, 200, 640, 480], path: path.join(workDir, "d-region.png") });
  // -R crops in global screen points, so the region itself is the raster
  // origin; binding the display's full bounds here is exactly the #554 shift.
  assert.deepEqual(shot.points, { x: 100, y: 200, w: 640, h: 480 });
  assert.deepEqual(shot.pixels, { w: 640, h: 480 });
  assert.equal(shot.scale, 1);
});

test("darwin: the region shot crops in global points and leaves -D off", async () => {
  const { b, exec } = await darwinBackend();
  await b.screenshot({ region: [100, 200, 640, 480], path: path.join(workDir, "d-region-args.png") });
  const cap = exec.calls.find((c) => c[0] === "screencapture");
  assert.ok(cap, "screencapture ran");
  const ri = cap.indexOf("-R");
  assert.ok(ri > 0, "-R present");
  assert.equal(cap[ri + 1], "100,200,640,480");
  assert.equal(cap.indexOf("-D"), -1, "no -D: the global region already pins the crop");
});

test("darwin: a full screenshot keeps -D and binds the display bounds", async () => {
  const { b, exec } = await darwinBackend();
  const shot = await b.screenshot({ path: path.join(workDir, "d-full.png") });
  const cap = exec.calls.find((c) => c[0] === "screencapture");
  assert.ok(cap.indexOf("-D") > 0, "-D present for a full display shot");
  assert.deepEqual(shot.points, { x: 0, y: 0, w: 2560, h: 1440 });
  assert.deepEqual(shot.pixels, { w: 2560, h: 1440 });
});

test("darwin: region pixels scale by the display scale from the pixels field", async () => {
  // _spdisplays_pixels is the backing resolution on Retina panels while
  // _spdisplays_resolution is the point size — the scale must come from the
  // pair, or every region shot binds scale 1 and aims twice as far.
  const { b } = await darwinBackend({ resolution: "1920x1080", pixels: "3840x2160", finderBounds: "0, 0, 1920, 1080" });
  const shot = await b.screenshot({ region: [100, 200, 640, 480], path: path.join(workDir, "d-region-2x.png") });
  assert.equal(shot.scale, 2);
  assert.deepEqual(shot.points, { x: 100, y: 200, w: 640, h: 480 });
  assert.deepEqual(shot.pixels, { w: 1280, h: 960 });
});

test("darwin: the bound scale comes from the PNG actually captured", async () => {
  // The measured pixel size is ground truth (Retina, mixed-DPI, clamping) —
  // here metadata alone would have under- or overstated the real capture.
  const { b, exec } = await darwinBackend({ resolution: "1512x982", pixels: "3024x1964", finderBounds: "0, 0, 1512, 982", sipsSize: { w: 1280, h: 960 } });
  const shot = await b.screenshot({ region: [100, 200, 640, 480], path: path.join(workDir, "d-measured.png") });
  assert.deepEqual(shot.pixels, { w: 1280, h: 960 });
  assert.equal(shot.scale, 2, "measured 1280 px across a 640 pt region = 2");
  exec.impl.sipsSize = { w: 3024, h: 1964 };
  const full = await b.screenshot({ path: path.join(workDir, "d-measured-full.png") });
  assert.deepEqual(full.pixels, { w: 3024, h: 1964 });
  assert.equal(full.scale, 2);
});

test("darwin: a region shot needs no display geometry", async () => {
  // -R is global: the region is its own origin even when no display reports
  // precise point geometry (the multi-display case Finder cannot describe).
  const { b } = await darwinBackend({ finderBounds: null });
  const shot = await b.screenshot({ region: [10, 10, 50, 50], path: path.join(workDir, "d-region-no-geom.png") });
  assert.deepEqual(shot.points, { x: 10, y: 10, w: 50, h: 50 });
  assert.equal(shot.note, undefined);
});

test("darwin: an unknown display origin on a full shot keeps the previous binding whole", async () => {
  const { b, exec } = await darwinBackend();
  const full = await b.screenshot({ path: path.join(workDir, "d-known.png") });
  assert.deepEqual(full.points, { x: 0, y: 0, w: 2560, h: 1440 });
  exec.impl.finderBounds = null; // non-main displays have no precise point geometry
  const shot = await b.screenshot({ path: path.join(workDir, "d-unknown.png") });
  assert.deepEqual(shot.points, full.points, "previous binding kept, not a guessed origin");
  assert.match(shot.note, /unbound/u);
  assert.equal(shot.file, path.join(workDir, "d-known.png"), "the bound file stays the previous raster");
  assert.equal(shot.path, path.join(workDir, "d-unknown.png"), "the new capture rides along unbound");
});

test("darwin: an unknown origin on the first full screenshot binds nothing and says so", async () => {
  const { b } = await darwinBackend({ finderBounds: null });
  const shot = await b.screenshot({ path: path.join(workDir, "d-first-unknown.png") });
  assert.equal(shot.points, undefined);
  assert.match(shot.note, /no raster is bound/u);
  assert.equal(shot.path, path.join(workDir, "d-first-unknown.png"));
});

test("darwin: a malformed region fails with the named error before any capture", async () => {
  const { b, exec } = await darwinBackend();
  for (const region of ["0,0,10,10", ["a", 0, 10, 10], [10, 0, 10], [0, 0, 0, 10]]) {
    await assert.rejects(() => b.screenshot({ region }), /region must be \[x, y, w, h\] in global screen points/u);
  }
  assert.equal(exec.calls.filter((c) => c[0] === "screencapture").length, 0, "no capture ran for a malformed region");
});

test("darwin: a region reaching past the only known display is clamped to it", async () => {
  const { b, exec } = await darwinBackend();
  const shot = await b.screenshot({ region: [2400, 1300, 400, 300], path: path.join(workDir, "d-clamp.png") });
  assert.deepEqual(shot.points, { x: 2160, y: 1140, w: 400, h: 300 });
  const cap = exec.calls.find((c) => c[0] === "screencapture");
  assert.equal(cap[cap.indexOf("-R") + 1], "2160,1140,400,300", "screencapture received the clamped region");
});

test("darwin: no clamping on a partial display union — it could trap another display's region", async () => {
  const { b, exec } = await darwinBackend({ extraDisplays: [{ _name: "Sidecar", spdisplays_main: "n", _spdisplays_resolution: "1366x1024" }] });
  const shot = await b.screenshot({ region: [5000, 5000, 100, 100], path: path.join(workDir, "d-no-clamp.png") });
  assert.deepEqual(shot.points, { x: 5000, y: 5000, w: 100, h: 100 }, "global origin binds verbatim");
  const cap = exec.calls.find((c) => c[0] === "screencapture");
  assert.equal(cap[cap.indexOf("-R") + 1], "5000,5000,100,100", "region passed through unclamped");
});

// ---- linux: PATH shims. linux.mjs imports run/runOk/have directly from
// exec.mjs, so an injected exec is never consulted — the shimmed PATH is the
// seam (same pattern as the ssh shims in server-protocol.test.mjs). ----

const binDir = path.join(workDir, "bin");
fs.mkdirSync(binDir, { recursive: true });

const SwayOutputs = JSON.stringify([
  { name: "DP-1", rect: { x: -1280, y: 240, width: 1280, height: 840 }, current_mode: { width: 1280, height: 840 }, scale: 1 },
  { name: "HDMI-A-1", rect: { x: 0, y: 0, width: 1920, height: 1080 }, current_mode: { width: 1920, height: 1080 }, scale: 1 },
]);
// The union is {x:-1280, y:0, w:3200, h:1080}: a layout whose smallest x is
// negative, so a full shot's raster origin is not (0,0).

for (const name of ["grim", "scrot"]) {
  fs.writeFileSync(path.join(binDir, name), `#!/bin/bash
echo "${name} $*" >> "\$CU_SHIM_LOG"
: > "\$CU_SHIM_SHOT"
exit 0
`);
}
fs.writeFileSync(path.join(binDir, "swaymsg"), `#!/bin/bash
echo '${SwayOutputs}'
exit 0
`);
for (const f of ["grim", "scrot", "swaymsg"]) fs.chmodSync(path.join(binDir, f), 0o755);

// A scaled-output fixture: sway reports layout points in rect and buffer
// pixels in current_mode; grim renders at the highest output scale.
const ScaledSwayOutputs = JSON.stringify([
  { name: "eDP-1", rect: { x: 0, y: 0, width: 1920, height: 1080 }, current_mode: { width: 3840, height: 2160 }, scale: 2 },
]);
const scaledBin = path.join(workDir, "bin-scaled");
fs.mkdirSync(scaledBin, { recursive: true });
fs.writeFileSync(path.join(scaledBin, "grim"), fs.readFileSync(path.join(binDir, "grim")));
fs.writeFileSync(path.join(scaledBin, "swaymsg"), `#!/bin/bash
echo '${ScaledSwayOutputs}'
exit 0
`);
for (const f of ["grim", "swaymsg"]) fs.chmodSync(path.join(scaledBin, f), 0o755);

const log = path.join(workDir, "shims.log");
const shotFile = path.join(workDir, "l-shot.png");

async function linuxBackend() {
  const mod = await import("../src/backends/linux.mjs");
  return mod.create({ exec: {} });
}

test("linux: a full screenshot binds the virtual screen's min corner", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${binDir}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    const shot = await b.screenshot({ path: shotFile });
    assert.ok(fs.existsSync(shot.file));
    assert.deepEqual(shot.points, { x: -1280, y: 0, w: 3200, h: 1080 });
    assert.deepEqual(shot.pixels, { w: 3200, h: 1080 });
    assert.equal(shot.scale, 1);
  });
});

test("linux: a region screenshot binds the region origin in layout coordinates", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${binDir}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    const shot = await b.screenshot({ region: [100, 50, 400, 300], path: shotFile });
    assert.deepEqual(shot.points, { x: 100, y: 50, w: 400, h: 300 });
    assert.deepEqual(shot.pixels, { w: 400, h: 300 });
    const call = fs.readFileSync(log, "utf8").trim().split("\n").pop();
    assert.match(call, /grim -g 100,50 400x300/, `grim received the region: ${call}`);
  });
});

test("linux: a region reaching past the layout is clamped and the receipt names the crop taken", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${binDir}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    // Union is 3200 wide starting at -1280: x 1800..2200 overruns the right
    // edge (1920 in layout space) and must clamp to x 1520..1920.
    const shot = await b.screenshot({ region: [1800, 50, 400, 300], path: shotFile });
    assert.deepEqual(shot.points, { x: 1520, y: 50, w: 400, h: 300 });
    const call = fs.readFileSync(log, "utf8").trim().split("\n").pop();
    assert.match(call, /grim -g 1520,50 400x300/, `grim received the clamped region: ${call}`);
  });
});

test("linux: a negative layout region is valid — layouts extend left of the primary", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${binDir}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    // DP-1's exact rect from the fixture: x -1280..0. Rejecting negative
    // layout coordinates would make this display's regions unreachable.
    const shot = await b.screenshot({ region: [-1280, 240, 1280, 840], path: shotFile });
    assert.deepEqual(shot.points, { x: -1280, y: 240, w: 1280, h: 840 });
    const call = fs.readFileSync(log, "utf8").trim().split("\n").pop();
    assert.match(call, /grim -g -1280,240 1280x840/, `grim received the negative region: ${call}`);
  });
});

test("linux: a malformed region fails with the named error", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${binDir}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    for (const region of [["a", 0, 10, 10], [10, 0, 10], [0, 0, 0, 10]]) {
      await assert.rejects(() => b.screenshot({ region }), /region must be \[x, y, w, h\] in screen points/u);
    }
  });
});

test("linux: a scaled output binds the compositor scale, not pixels = points", async () => {
  await withEnv({ WAYLAND_DISPLAY: "wayland-0", PATH: `${scaledBin}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    const full = await b.screenshot({ path: shotFile });
    // grim renders the 1920x1080 point layout at scale 2 -> 3840x2160 pixels.
    assert.deepEqual(full.points, { x: 0, y: 0, w: 1920, h: 1080 });
    assert.deepEqual(full.pixels, { w: 3840, h: 2160 });
    assert.equal(full.scale, 2);
    const region = await b.screenshot({ region: [100, 100, 200, 150], path: shotFile });
    assert.deepEqual(region.points, { x: 100, y: 100, w: 200, h: 150 });
    assert.deepEqual(region.pixels, { w: 400, h: 300 });
    assert.equal(region.scale, 2);
  });
});

test("linux: without display enumeration the origin is unknown and the receipt says so", async () => {
  const noEnumBin = path.join(workDir, "bin-no-enum");
  fs.mkdirSync(noEnumBin, { recursive: true });
  fs.writeFileSync(path.join(noEnumBin, "scrot"), fs.readFileSync(path.join(binDir, "scrot")));
  fs.chmodSync(path.join(noEnumBin, "scrot"), 0o755);
  await withEnv({ DISPLAY: ":99", PATH: `${noEnumBin}:${envSnapshot.PATH}`, CU_SHIM_LOG: log, CU_SHIM_SHOT: shotFile }, async () => {
    const b = await linuxBackend();
    const shot = await b.screenshot({ path: shotFile });
    assert.equal(shot.points, undefined);
    assert.match(shot.note, /display enumeration unavailable/);
    assert.equal(shot.path, shotFile, "the unbound capture still rides along");
    const call = fs.readFileSync(log, "utf8").trim().split("\n").pop();
    assert.match(call, /^scrot -z/, `scrot captured the full screen: ${call}`);
  });
});

// ---- win32: a powershell.exe shim that logs the decoded -EncodedCommand
// script. Real capture is a Windows-machine item; here the script
// construction, clamping math, and binding are pinned. ----

const psBin = path.join(workDir, "bin-ps");
fs.mkdirSync(psBin, { recursive: true });
const psScriptLog = path.join(workDir, "ps-script.txt");
const psShot = path.join(workDir, "w-shot.png");
fs.writeFileSync(path.join(psBin, "powershell.exe"), `#!/bin/bash
enc=""
prev=""
for a in "$@"; do
  if [ "\$prev" = "-EncodedCommand" ]; then enc="\$a"; fi
  prev="\$a"
done
if [ -n "\$enc" ]; then
  printf '%s' "\$enc" | base64 -d | iconv -f UTF-16LE -t UTF-8 > "\$CU_PS_SCRIPT_LOG" 2>/dev/null
fi
: > "\$CU_PS_SHOT"
[ -n "\$CU_PS_JSON" ] && echo "\$CU_PS_JSON"
exit 0
`);
fs.chmodSync(path.join(psBin, "powershell.exe"), 0o755);

// Multi-monitor layout whose virtual screen does not start at (0,0).
const VirtualBounds = JSON.stringify({ ok: true, x: -1920, y: 0, w: 3840, h: 1080 });

const psEnv = { PATH: `${psBin}:${envSnapshot.PATH}`, CU_PS_SCRIPT_LOG: psScriptLog, CU_PS_SHOT: psShot, CU_PS_JSON: VirtualBounds };

async function win32Backend() {
  const mod = await import("../src/backends/win32.mjs");
  return mod.create();
}

test("win32: a full screenshot echoes the virtual screen and binds its origin", async () => {
  await withEnv(psEnv, async () => {
    const b = await win32Backend();
    const shot = await b.screenshot({ path: psShot });
    assert.deepEqual(shot.points, { x: -1920, y: 0, w: 3840, h: 1080 });
    assert.deepEqual(shot.pixels, { w: 3840, h: 1080 });
    assert.equal(shot.scale, 1);
    const script = fs.readFileSync(psScriptLog, "utf8");
    assert.match(script, /CopyFromScreen\(\$bounds\.X, \$bounds\.Y, 0, 0, \$bounds\.Size\)/);
    // The script echoes the virtual-screen origin so the binding is right on
    // layouts where it is not (0,0).
    assert.match(script, /"x": ' \+ \$bounds\.X \+ ', "y": ' \+ \$bounds\.Y/);
  });
});

test("win32: a region screenshot crops at the virtual screen origin + offset", async () => {
  await withEnv(psEnv, async () => {
    const b = await win32Backend();
    const shot = await b.screenshot({ region: [100, 50, 800, 600], path: psShot });
    assert.deepEqual(shot.points, { x: -1820, y: 50, w: 800, h: 600 });
    const script = fs.readFileSync(psScriptLog, "utf8");
    assert.match(script, /CopyFromScreen\(\$bounds\.X \+ \$rx, \$bounds\.Y \+ \$ry, 0, 0, \(New-Object System\.Drawing\.Size\(\$rw, \$rh\)\)\)/);
    assert.match(script, /\[Math\]::Min\(100, \$bounds\.Width - \$rw\)/);
    assert.match(script, /\[Math\]::Min\(50, \$bounds\.Height - \$rh\)/);
  });
});

test("win32: a region reaching past the virtual screen is clamped and the receipt names the crop taken", async () => {
  await withEnv(psEnv, async () => {
    const b = await win32Backend();
    // Virtual screen is 3840x1080: x 3700..4500 clamps to 3040..3840,
    // y 900..1500 clamps to 480..1080.
    const shot = await b.screenshot({ region: [3700, 900, 800, 600], path: psShot });
    assert.deepEqual(shot.points, { x: 1120, y: 480, w: 800, h: 600 });
    const script = fs.readFileSync(psScriptLog, "utf8");
    assert.match(script, /\[Math\]::Min\(3700, \$bounds\.Width - \$rw\)/);
    assert.match(script, /\[Math\]::Min\(900, \$bounds\.Height - \$rh\)/);
  });
});

test("win32: a region larger than the virtual screen collapses to the full screen on both sides", async () => {
  await withEnv(psEnv, async () => {
    const b = await win32Backend();
    // The clip-order asymmetry (PS shrinks rw before clamping rx, node clamps
    // against the unshrunk w) must land both sides on the full virtual screen.
    const shot = await b.screenshot({ region: [5000, 5000, 8000, 6000], path: psShot });
    assert.deepEqual(shot.points, { x: -1920, y: 0, w: 3840, h: 1080 });
    const script = fs.readFileSync(psScriptLog, "utf8");
    assert.match(script, /\[Math\]::Min\(8000, \$bounds\.Width\)/);
    assert.match(script, /\[Math\]::Min\(6000, \$bounds\.Height\)/);
    assert.match(script, /\[Math\]::Min\(5000, \$bounds\.Width - \$rw\)/);
    assert.match(script, /\[Math\]::Min\(5000, \$bounds\.Height - \$rh\)/);
  });
});

test("win32: a malformed region fails with the named error before any capture", async () => {
  await withEnv(psEnv, async () => {
    const b = await win32Backend();
    await new Promise((r) => setTimeout(r, 100)); // let create()'s Add-Type bootstrap land in the log
    const before = fs.readFileSync(psScriptLog, "utf8");
    for (const region of [["a", 0, 10, 10], [10, 0, 10], [-1, 0, 10, 10], [0, 0, 0, 10]]) {
      await assert.rejects(() => b.screenshot({ region }), /region must be \[x, y, w, h\] in virtual-screen points/u);
    }
    assert.equal(fs.readFileSync(psScriptLog, "utf8"), before, "no PowerShell ran for the malformed region");
  });
});

test("win32: a capture that reports no bounds fails closed instead of binding (0,0)", async () => {
  await withEnv({ ...psEnv, CU_PS_JSON: "" }, async () => {
    const b = await win32Backend();
    await assert.rejects(() => b.screenshot({ path: psShot }), /did not report the virtual-screen bounds/);
  });
});

after(() => {
  for (const k of managed) delete process.env[k];
  for (const [k, v] of Object.entries(envSnapshot)) process.env[k] = v;
  try { fs.rmSync(workDir, { recursive: true, force: true }); } catch {}
});
