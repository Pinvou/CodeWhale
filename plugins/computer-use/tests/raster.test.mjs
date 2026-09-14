// Pure geometry for coordinate targets: a zoom crops the previous raster 1:1,
// so child pixels must resolve against the region offset at the parent scale.
import { test } from "node:test";
import assert from "node:assert/strict";
import { zoomChildRaster } from "../src/raster.mjs";

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
