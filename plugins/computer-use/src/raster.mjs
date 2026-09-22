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

/**
 * Union bounding box (screen points) of enumerated displays — the surface a
 * full-virtual-screen capture actually photographs (linux grim/scrot/import
 * crop the compositor/root layout; the win32 VirtualScreen starts at its min
 * corner, which is not 0,0 on multi-monitor layouts). `scale` is the highest
 * contributing output's scale — grim renders at the highest of all output
 * scales, so the captured PNG carries that many pixels per point. Returns
 * null when no display carries usable point geometry — the caller then cannot
 * know where its capture sits and must not guess an origin.
 */
export function virtualScreen(displays) {
  if (!Array.isArray(displays)) return null;
  let x0 = null, y0 = null, x1 = null, y1 = null, scale = null;
  for (const d of displays) {
    const p = d?.points;
    if (!p || !Number.isFinite(p.x) || !Number.isFinite(p.y) || !Number.isFinite(p.w) || !Number.isFinite(p.h)) continue;
    x0 = x0 === null ? p.x : Math.min(x0, p.x);
    y0 = y0 === null ? p.y : Math.min(y0, p.y);
    x1 = x1 === null ? p.x + p.w : Math.max(x1, p.x + p.w);
    y1 = y1 === null ? p.y + p.h : Math.max(y1, p.y + p.h);
    if (Number.isFinite(d.scale) && d.scale > 0) scale = scale === null ? d.scale : Math.max(scale, d.scale);
  }
  if (x0 === null || y0 === null || x1 <= x0 || y1 <= y0) return null;
  return { x: x0, y: y0, w: x1 - x0, h: y1 - y0, scale: scale ?? 1 };
}

/**
 * Binding geometry of a screenshot raster taken off `surface` — the captured
 * surface's full bounds in screen points (a display's `points`, or the virtual
 * screen), with `region` = [x, y, w, h] relative to the surface origin, or
 * null/undefined for a full-surface shot. Returns what the server's
 * bindRaster needs ({points, pixels, scale}), or null when the surface origin
 * is unknown or the region is unusable — the caller then keeps the previous
 * binding and says so instead of binding a guessed origin, the same rule the
 * zoom path follows (Pinvou/pinvou-agent#554).
 */
export function screenshotRaster(surface, region) {
  if (!surface || !Number.isFinite(surface.x) || !Number.isFinite(surface.y)) return null;
  const scale = surface.scale && surface.scale > 0 ? surface.scale : 1;
  if (region == null) {
    const w = Number.isFinite(surface.w) ? surface.w : null;
    const h = Number.isFinite(surface.h) ? surface.h : null;
    return {
      points: { x: surface.x, y: surface.y, w, h },
      pixels: surface.pixels ?? { w: w != null ? Math.round(w * scale) : null, h: h != null ? Math.round(h * scale) : null },
      scale,
    };
  }
  if (!Array.isArray(region) || region.length !== 4 || !region.every((n) => Number.isFinite(n)) || region[2] < 1 || region[3] < 1) return null;
  return {
    points: { x: surface.x + region[0], y: surface.y + region[1], w: region[2], h: region[3] },
    pixels: { w: Math.round(region[2] * scale), h: Math.round(region[3] * scale) },
    scale,
  };
}
