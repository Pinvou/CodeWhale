// Backend tests that can run on any host: harmony logic via a mocked hdc
// exec, linux fail-closed probing, and module-shape checks for win32.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import zlib from "node:zlib";
import { parseBounds, flatten } from "../src/backends/harmonyos.mjs";

function fakeJpeg(w, h) {
  // Minimal JPEG with an SOF0 marker carrying the dimensions.
  return Buffer.from([
    0xff, 0xd8, 0xff, 0xc0, 0x00, 0x0b, 0x08, (h >> 8) & 0xff, h & 0xff,
    (w >> 8) & 0xff, w & 0xff, 0x01, 0x01, 0x11, 0x00, 0xff, 0xd9,
  ]);
}

function harmonyFixtureExec(t) {
  const layout = {
    attributes: { bundleName: "com.example.app", type: "FrameNode" },
    children: [
      {
        attributes: { type: "Button", text: "OK", id: "ok_btn", bounds: "[100,200][300,260]" },
        children: [],
      },
      {
        attributes: { type: "Text", text: "Hello", bounds: "[0,0][100,50]" },
        children: [{ attributes: { type: "Text", text: "nested", bounds: "[10,10][90,40]" }, children: [] }],
      },
    ],
  };
  const calls = [];
  const exec = {
    targetArgs: [],
    run: async () => ({ code: 0, stdout: "", stderr: "" }),
    runOk: async () => ({ code: 0, stdout: "", stderr: "" }),
    shell: async (args) => { calls.push({ kind: "shell", args }); return { code: 0, stdout: "", stderr: "" }; },
    async pullFile(remote, local) {
      calls.push({ kind: "pull", remote, local });
      if (remote.includes("layout")) fs.writeFileSync(local, Buffer.from(JSON.stringify(layout)));
      else fs.writeFileSync(local, fakeJpeg(168, 120));
      return local;
    },
    async readFile(remote) {
      if (remote.includes("layout")) return Buffer.from(JSON.stringify(layout));
      return fakeJpeg(168, 120);
    },
  };
  return { exec, calls };
}

test("harmony: parseBounds handles uitest bounds strings", () => {
  assert.deepEqual(parseBounds("[100,200][300,260]"), { x: 100, y: 200, w: 200, h: 60, cx: 200, cy: 230 });
  assert.equal(parseBounds("garbage"), null);
});

test("harmony: flatten produces indexed elements with paths and geometry", () => {
  const tree = { attributes: { type: "root" }, children: [{ attributes: { type: "Button", text: "OK", bounds: "[0,0][10,10]" } }] };
  const els = flatten(tree);
  assert.equal(els[0].role, "root");
  assert.equal(els[0].path.length, 0);
  assert.equal(els[1].label, "OK");
  assert.deepEqual(els[1].path, [0]);
  assert.equal(els[1].bounds.cx, 5);
});

test("harmony: get_app_state flattens dumpLayout with indices and actions", async () => {
  const { exec } = harmonyFixtureExec();
  const mod = await import("../src/backends/harmonyos.mjs");
  const b = mod.create({ exec });
  const st = await b.get_app_state({});
  assert.equal(st.bundle_id, "com.example.app");
  assert.ok(st.elements.length >= 4);
  const ok = st.elements.find((e) => e.label === "OK");
  assert.ok(ok, "OK button flattened");
  assert.deepEqual(ok.bounds, { x: 100, y: 200, w: 200, h: 60, cx: 200, cy: 230 });
  assert.ok(ok.actions.includes("click"));
});

test("harmony: screenshot pulls the file and reports panel dimensions", async () => {
  const { exec } = harmonyFixtureExec();
  const mod = await import("../src/backends/harmonyos.mjs");
  const b = mod.create({ exec });
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-hm-test-"));
  const shot = await b.screenshot({ path: path.join(dir, "shot.jpeg") });
  assert.equal(shot.pixels.w, 168);
  assert.equal(shot.pixels.h, 120);
  assert.ok(fs.existsSync(shot.file));
  fs.rmSync(dir, { recursive: true, force: true });
});

test("harmony: click routes through uitest uiInput with validated args", async () => {
  const { exec, calls } = harmonyFixtureExec();
  const mod = await import("../src/backends/harmonyos.mjs");
  const b = mod.create({ exec });
  const r = await b.left_click({ target: { x: 123.6, y: 45.2 } });
  assert.equal(r.action_sent, true);
  const ui = calls.find((c) => c.args[0] === "uitest");
  assert.deepEqual(ui.args, ["uitest", "uiInput", "click", "124", "45"]);
});

test("harmony: clipboard and select_text fail closed with named reasons", async () => {
  const { exec } = harmonyFixtureExec();
  const mod = await import("../src/backends/harmonyos.mjs");
  const b = mod.create({ exec });
  await assert.rejects(() => b.read_clipboard(), /not exposed by hdc/);
  await assert.rejects(() => b.select_text({}), /not exposed by uitest/);
  assert.throws(() => b.key({ text: "cmd+c" }), /unsupported key/);
});

test("linux: probe reports the session and names missing tools (fail-closed)", async () => {
  const mod = await import("../src/backends/linux.mjs");
  const b = mod.create({ exec: (await import("../src/remote-runtime.mjs")).exec });
  const p = await b.probe();
  assert.equal(p.platform, "linux");
  assert.ok(["x11", "wayland", null].includes(p.session));
  assert.equal(typeof p.missing.length, "number");
  // On a host with no display session, screenshot must fail closed.
  if (p.session === null) {
    await assert.rejects(() => b.screenshot({}), /X11|Wayland/);
  }
});

test("win32: module loads with the full backend surface", async () => {
  const mod = await import("../src/backends/win32.mjs");
  assert.equal(typeof mod.create, "function");
  if (process.platform !== "win32") {
    const b = mod.create();
    for (const m of ["probe", "screenshot", "left_click", "type", "key", "recordingStart", "get_app_state", "set_value"]) {
      assert.equal(typeof b[m], "function", `win32 backend missing ${m}`);
    }
  }
});

test("remote agent refuses tools outside the allow-list", async () => {
  const { run } = await import("../src/exec.mjs");
  const sentinel = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "cu-agent-deny-")), "x");
  const payload = Buffer.from(JSON.stringify({ tool: "write_file", args: { path: sentinel } })).toString("base64");
  const r = await run("node", [new URL("../agent.mjs", import.meta.url).pathname, payload]);
  const reply = JSON.parse(r.stdout.trim());
  assert.equal(reply.ok, false);
  assert.equal(reply.error.code, "tool_not_allowed");
  assert.ok(!fs.existsSync(sentinel));
});

test("remote agent answers the platform probe", async () => {
  const { run } = await import("../src/exec.mjs");
  const payload = Buffer.from(JSON.stringify({ tool: "platform" })).toString("base64");
  const r = await run("node", [new URL("../agent.mjs", import.meta.url).pathname, payload]);
  const reply = JSON.parse(r.stdout.trim());
  assert.equal(reply.ok, true);
  assert.equal(reply.platform, process.platform);
});

// ---- zoom crop semantics ----
// Minimal PNG codec (8-bit RGB/RGBA, non-interlaced) so the crop tests can
// verify actual pixel content instead of trusting command-line argument
// order: a swapped cropOffset/crop argument fails the pixel assertions.
const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type, data) {
  const head = Buffer.alloc(4);
  head.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([head, body, crc]);
}

function encodePNG(width, height, px) {
  const raw = Buffer.alloc(height * (1 + width * 3));
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const [r, g, b] = px(x, y);
      const o = y * (1 + width * 3) + 1 + x * 3;
      raw[o] = r; raw[o + 1] = g; raw[o + 2] = b;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // color type RGB
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", ihdr),
    pngChunk("IDAT", zlib.deflateSync(raw)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

function decodePNG(buf) {
  let off = 8;
  let width, height, bitDepth, colorType, interlace;
  const idat = [];
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString("ascii", off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (type === "IHDR") {
      width = data.readUInt32BE(0); height = data.readUInt32BE(4);
      bitDepth = data[8]; colorType = data[9]; interlace = data[12];
    } else if (type === "IDAT") idat.push(data);
    off += 12 + len;
  }
  if (bitDepth !== 8 || interlace !== 0 || ![2, 6].includes(colorType)) {
    throw new Error(`unsupported PNG: depth=${bitDepth} color=${colorType} interlace=${interlace}`);
  }
  const ch = colorType === 6 ? 4 : 3;
  const raw = zlib.inflateSync(Buffer.concat(idat));
  const stride = width * ch;
  const out = Buffer.alloc(height * stride);
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)];
    const row = out.subarray(y * stride, (y + 1) * stride);
    const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null;
    for (let i = 0; i < stride; i++) {
      const a = i >= ch ? row[i - ch] : 0;
      const b = prev ? prev[i] : 0;
      const c = i >= ch && prev ? prev[i - ch] : 0;
      let v = raw[y * (stride + 1) + 1 + i];
      if (filter === 1) v += a;
      else if (filter === 2) v += b;
      else if (filter === 3) v += (a + b) >> 1;
      else if (filter === 4) {
        const p = a + b - c;
        const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
        v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      }
      row[i] = v & 0xff;
    }
  }
  return {
    width, height,
    px(x, y) { const o = y * stride + x * ch; return [out[o], out[o + 1], out[o + 2]]; },
  };
}

// Deterministic gradient so every sampled pixel identifies its source
// coordinate: r=x*4, g=y*5, b=x+y (mod 256).
const grad = (x, y) => [(x * 4) % 256, (y * 5) % 256, (x + y) % 256];

test("darwin: zoom crops 1:1 in raster pixels and advances the last raster so chained zooms crop from the child", { skip: process.platform !== "darwin" && "pixel check runs real sips (macOS only)" }, async (t) => {
  const { create } = await import("../src/backends/darwin.mjs");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-zoom-darwin-"));
  t.after(() => { try { fs.rmSync(dir, { recursive: true, force: true }); } catch {} });
  process.env.CODEWHALE_CU_RECORDINGS_DIR = path.join(dir, "rec");
  const backend = create({ exec: { run: async () => ({ code: 0, stdout: "", stderr: "" }) } });
  const src = path.join(dir, "src.png");
  fs.writeFileSync(src, encodePNG(64, 48, grad));

  const one = await backend.zoom({ source: src, region: [10, 6, 40, 30], path: path.join(dir, "one.png") });
  assert.equal(one.source, src);
  const img1 = decodePNG(fs.readFileSync(one.file));
  assert.equal(img1.width, 40);
  assert.equal(img1.height, 30);
  // child (0,0) is src (10,6); child (39,29) is src (49,35) — a swapped
  // cropOffset or -c h/w would land on different gradient values.
  assert.deepEqual(img1.px(0, 0), grad(10, 6));
  assert.deepEqual(img1.px(39, 29), grad(49, 35));

  // The zoom child became the last raster, so a source-less follow-up zoom
  // crops from the child: child2 (0,0) = child1 (20,10) = src (30,16).
  const two = await backend.zoom({ region: [20, 10, 10, 10], path: path.join(dir, "two.png") });
  assert.equal(two.source, one.file, "chained zoom must default to the advanced child raster");
  const img2 = decodePNG(fs.readFileSync(two.file));
  assert.equal(img2.width, 10);
  assert.equal(img2.height, 10);
  assert.deepEqual(img2.px(0, 0), grad(30, 16));
  assert.deepEqual(img2.px(9, 9), grad(39, 25));
});

test("linux: zoom crops 1:1 in raster pixels and advances the last raster so chained zooms crop from the child", { skip: process.platform !== "linux" && "pixel check runs real ffmpeg (linux CI)" }, async (t) => {
  const { create } = await import("../src/backends/linux.mjs");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-zoom-linux-"));
  t.after(() => { try { fs.rmSync(dir, { recursive: true, force: true }); } catch {} });
  process.env.CODEWHALE_CU_RECORDINGS_DIR = path.join(dir, "rec");
  const backend = create({ exec: {} });
  await backend.list_displays().catch(() => {}); // fills the tool probe cache; headless hosts may fail harmlessly
  const src = path.join(dir, "src.png");
  fs.writeFileSync(src, encodePNG(64, 48, grad));

  const one = await backend.zoom({ source: src, region: [10, 6, 40, 30], path: path.join(dir, "one.png") });
  assert.equal(one.source, src);
  const img1 = decodePNG(fs.readFileSync(one.file));
  assert.equal(img1.width, 40);
  assert.equal(img1.height, 30);
  assert.deepEqual(img1.px(0, 0), grad(10, 6));
  assert.deepEqual(img1.px(39, 29), grad(49, 35));

  const two = await backend.zoom({ region: [20, 10, 10, 10], path: path.join(dir, "two.png") });
  assert.equal(two.source, one.file, "chained zoom must default to the advanced child raster");
  const img2 = decodePNG(fs.readFileSync(two.file));
  assert.deepEqual(img2.px(0, 0), grad(30, 16));
});
