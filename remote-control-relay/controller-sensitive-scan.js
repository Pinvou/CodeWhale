"use strict";

export const CONTROLLER_SCAN_SURFACES = Object.freeze([
  "logs",
  "persistence",
  "packet_capture",
  "crash_diagnostics",
]);

function surfaceBytes(value) {
  if (Buffer.isBuffer(value)) return value;
  if (value instanceof Uint8Array) return Buffer.from(value);
  if (typeof value === "string") return Buffer.from(value, "utf8");
  return Buffer.from(JSON.stringify(value), "utf8");
}

function sentinelNeedles(value) {
  // Pairing codes are seven characters, so the scanner must accept the real
  // shortest secret rather than replacing it with a longer synthetic value.
  if (typeof value !== "string" || value.length < 7 || Buffer.byteLength(value) > 4096) {
    throw new Error("controller_scan_sentinel_invalid");
  }
  const raw = Buffer.from(value, "utf8");
  return [raw, Buffer.from(raw.toString("base64"), "ascii"), Buffer.from(raw.toString("hex"), "ascii")];
}

/**
 * Scans test/deployment evidence without returning the sensitive canary value.
 * Callers provide synthetic canaries that were injected into credentials,
 * prompts, paths and content before collecting each fixed surface.
 */
export class ControllerSensitiveScanner {
  constructor(sentinels) {
    if (!Array.isArray(sentinels) || sentinels.length === 0 || sentinels.length > 32) {
      throw new Error("controller_scan_sentinels_invalid");
    }
    this.needles = sentinels.map(sentinelNeedles);
  }

  scan(surfaces) {
    if (!surfaces || typeof surfaces !== "object" || Array.isArray(surfaces)) {
      throw new Error("controller_scan_surfaces_invalid");
    }
    for (const name of Object.keys(surfaces)) {
      if (!CONTROLLER_SCAN_SURFACES.includes(name)) {
        throw new Error("controller_scan_surface_unknown");
      }
    }
    const matches = [];
    for (const name of CONTROLLER_SCAN_SURFACES) {
      if (!Object.hasOwn(surfaces, name)) continue;
      const haystack = surfaceBytes(surfaces[name]);
      if (this.needles.some((variants) => variants.some((needle) => haystack.indexOf(needle) !== -1))) {
        matches.push(name);
      }
    }
    return Object.freeze({
      clean: matches.length === 0,
      scanned_surfaces: Object.freeze(CONTROLLER_SCAN_SURFACES.filter((name) => Object.hasOwn(surfaces, name))),
      matched_surfaces: Object.freeze(matches),
    });
  }

  assertClean(surfaces) {
    const result = this.scan(surfaces);
    if (!result.clean) {
      const error = new Error(`controller_sensitive_sentinel_detected:${result.matched_surfaces.join(",")}`);
      error.code = "controller_sensitive_sentinel_detected";
      throw error;
    }
    return result;
  }
}
