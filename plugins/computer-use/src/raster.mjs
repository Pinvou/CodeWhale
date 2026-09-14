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
    scale: prev.scale,
    origin: {
      x: (prev.origin?.x ?? 0) + region[0] / scale,
      y: (prev.origin?.y ?? 0) + region[1] / scale,
    },
  };
}
