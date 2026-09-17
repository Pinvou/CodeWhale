/**
 * Raster geometry shared by the MCP server: coordinate targets are raster
 * pixels, and the server maps them to screen points through the bound raster
 * ({scale, origin}). Zoom crops a region out of the previous raster 1:1 on
 * every backend (sips / GDI / ffmpeg), so the child raster's pixels are the
 * parent's pixels offset by the region origin at the same scale.
 */

/**
 * Binding geometry of the raster a zoom returns, given the geometry of the
 * raster the region was taken from. The returned shape is what bindRaster
 * stores (`origin` in screen points — screenshots carry `points`, a zoom
 * child carries `origin`). Returns null when either side is unusable — the
 * caller then keeps the previous binding instead of rebinding.
 */
export function zoomChildRaster(prev, region) {
  if (!prev) return null;
  if (!Array.isArray(region) || region.length < 2) return null;
  if (!Number.isFinite(region[0]) || !Number.isFinite(region[1])) return null;
  const scale = prev.scale && prev.scale > 0 ? prev.scale : 1;
  return {
    scale,
    origin: {
      x: (prev.origin?.x ?? 0) + region[0] / scale,
      y: (prev.origin?.y ?? 0) + region[1] / scale,
    },
  };
}

/**
 * The region a zoom actually crops, clipped against the source raster's pixel
 * size. Every backend crops and returns this exact rect: the croppers adjust
 * an out-of-bounds request on their own (ffmpeg clamps the origin silently,
 * sips/GDI are unspecified), so a backend that cropped the raw region and
 * echoed it would hand the server a binding for a crop that was never taken.
 * Returns null for a degenerate region (w or h below 1 pixel) — callers
 * reject that with the named region error.
 */
export function clampRegion(region, srcW, srcH) {
  if (!Array.isArray(region) || region.length < 4 || !region.slice(0, 4).every((n) => Number.isFinite(n))) return null;
  if (!Number.isInteger(srcW) || !Number.isInteger(srcH) || srcW < 1 || srcH < 1) return null;
  const [x, y, w, h] = region.map(Math.round);
  if (w < 1 || h < 1) return null;
  return [
    Math.max(0, Math.min(x, srcW - w)),
    Math.max(0, Math.min(y, srcH - h)),
    Math.min(w, srcW),
    Math.min(h, srcH),
  ];
}
