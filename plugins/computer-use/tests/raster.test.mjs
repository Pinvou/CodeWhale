// Pure geometry for coordinate targets: a zoom crops the previous raster 1:1,
// so child pixels must resolve against the region offset at the parent scale;
// a screenshot must bind the geometry of the crop actually taken, not the
// display's full bounds.
import { test } from "node:test";
import assert from "node:assert/strict";
import { zoomChildRaster, clampRegion, screenshotRaster, virtualScreen } from "../src/raster.mjs";

test("zoom child raster offsets the parent origin by the region at the parent scale", () => {
  const child = zoomChildRaster({ scale: 2, origin: { x: 10, y: 20 } }, [100, 50, 300, 200]);
  assert.deepEqual(child, { scale: 2, origin: { x: 60, y: 45 } });
});

test("zoom child raster at scale 1 keeps the parent scale and shifts by raw region", () => {
  const child = zoomChildRaster({ scale: 1, origin: { x: 0, y: 0 } }, [640, 480, 200, 100]);
  assert.deepEqual(child, { scale: 1, origin: { x: 640, y: 480 } });
});

test("chained zooms accumulate region offsets at the same scale", () => {
  // The second zoom's region is in child-raster pixels, so its offset adds on
  // top of the first zoom's screen point.
  const parent = { scale: 2, origin: { x: 10, y: 20 } };
  const child = zoomChildRaster(parent, [100, 50, 300, 200]);
  assert.deepEqual(zoomChildRaster(child, [40, 30, 100, 100]), { scale: 2, origin: { x: 80, y: 60 } });
});

test("zoom child raster falls back to scale 1 for an unnormalized parent", () => {
  const child = zoomChildRaster({ origin: { x: 5, y: 5 } }, [10, 10, 50, 50]);
  assert.deepEqual(child, { scale: 1, origin: { x: 15, y: 15 } });
});

test("zoom child raster refuses unusable inputs so the caller keeps the previous binding", () => {
  assert.equal(zoomChildRaster(null, [0, 0, 1, 1]), null);
  assert.equal(zoomChildRaster({ scale: 1, origin: { x: 0, y: 0 } }, null), null);
  assert.equal(zoomChildRaster({ scale: 1, origin: { x: 0, y: 0 } }, [0]), null);
  assert.equal(zoomChildRaster({ scale: 1, origin: { x: 0, y: 0 } }, ["a", 0, 1, 1]), null);
});

test("clampRegion clips an out-of-bounds region to the source raster", () => {
  // The croppers adjust an out-of-bounds request on their own — ffmpeg
  // clamps the origin silently (x 50 -> 32 on a 64-wide raster) — so the
  // effective region must be the crop actually taken, not the raw request.
  assert.deepEqual(clampRegion([50, 30, 32, 32], 64, 48), [32, 16, 32, 32]);
  assert.deepEqual(clampRegion([0, 0, 200, 200], 64, 48), [0, 0, 64, 48]);
  assert.deepEqual(clampRegion([10, 6, 40, 30], 64, 48), [10, 6, 40, 30]);
});

test("clampRegion rounds and refuses degenerate or unusable input", () => {
  assert.deepEqual(clampRegion([10.4, 6.6, 40, 30], 64, 48), [10, 7, 40, 30]);
  assert.equal(clampRegion([0, 0, 0, 10], 64, 48), null);
  assert.equal(clampRegion([0, 0, 10, 0.2], 64, 48), null);
  assert.equal(clampRegion(["a", 0, 10, 10], 64, 48), null);
  assert.equal(clampRegion([0, 0], 64, 48), null);
  assert.equal(clampRegion([0, 0, 10, 10], 0, 48), null);
  assert.equal(clampRegion([0, 0, 10, 10], 64, undefined), null);
});

test("screenshotRaster binds a full-surface shot at the surface origin", () => {
  const geom = screenshotRaster({ x: -1920, y: 0, w: 3840, h: 1080, scale: 1, pixels: { w: 3840, h: 1080 } }, null);
  assert.deepEqual(geom.points, { x: -1920, y: 0, w: 3840, h: 1080 });
  assert.deepEqual(geom.pixels, { w: 3840, h: 1080 });
  assert.equal(geom.scale, 1);
});

test("screenshotRaster offsets a region shot by the surface origin, not the display bounds", () => {
  // A region crop's raster origin is display origin + region offset; binding
  // the display bounds here is exactly the #554 bug.
  const geom = screenshotRaster({ x: 100, y: 200, w: 2560, h: 1440, scale: 1, pixels: { w: 2560, h: 1440 } }, [40, 60, 640, 480]);
  assert.deepEqual(geom.points, { x: 140, y: 260, w: 640, h: 480 });
  assert.deepEqual(geom.pixels, { w: 640, h: 480 });
});

test("screenshotRaster scales region pixels by the display scale", () => {
  // Retina: a 640x480 point region is 1280x960 pixels; the origin stays in
  // points because the server divides pixel targets by the bound scale.
  const geom = screenshotRaster({ x: 0, y: 0, w: 1920, h: 1080, scale: 2, pixels: { w: 3840, h: 2160 } }, [100, 200, 640, 480]);
  assert.deepEqual(geom.points, { x: 100, y: 200, w: 640, h: 480 });
  assert.deepEqual(geom.pixels, { w: 1280, h: 960 });
  assert.equal(geom.scale, 2);
});

test("screenshotRaster refuses an unknown surface origin so the caller keeps the previous binding", () => {
  assert.equal(screenshotRaster(null, [0, 0, 10, 10]), null);
  assert.equal(screenshotRaster(undefined, null), null);
  assert.equal(screenshotRaster({ x: null, y: null, w: null, h: null }, null), null);
  assert.equal(screenshotRaster({ x: 0, y: 0, w: 100, h: 100 }, [0, 0, 0, 10]), null);
  assert.equal(screenshotRaster({ x: 0, y: 0, w: 100, h: 100 }, [0, 0]), null);
});

test("virtualScreen unions display point geometry into one surface", () => {
  // A layout whose smallest x is negative: the full-shot surface starts at
  // x -1280, not 0,0.
  const displays = [
    { points: { x: 0, y: 0, w: 1920, h: 1080 } },
    { points: { x: -1280, y: 240, w: 1280, h: 840 } },
  ];
  assert.deepEqual(virtualScreen(displays), { x: -1280, y: 0, w: 3200, h: 1080 });
});

test("virtualScreen refuses unusable geometry so the caller does not guess", () => {
  assert.equal(virtualScreen([]), null);
  assert.equal(virtualScreen(undefined), null);
  assert.equal(virtualScreen([{ points: { x: null, y: null, w: null, h: null } }]), null);
  assert.equal(virtualScreen([{ points: { x: 0, y: 0, w: 0, h: 100 } }]), null);
  assert.equal(virtualScreen([{}]), null);
});
