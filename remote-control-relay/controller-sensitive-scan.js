"use strict";

export const CONTROLLER_SCAN_SURFACES = Object.freeze([
  "logs",
  "persistence",
  "packet_capture",
  "crash_diagnostics",
]);

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function surfaceBytes(value) {
  if (Buffer.isBuffer(value)) return value;
  if (value instanceof Uint8Array) return Buffer.from(value);
  if (typeof value === "string") return Buffer.from(value, "utf8");
  if (!value || typeof value !== "object") fail("controller_scan_surface_invalid");
  try {
    const serialized = JSON.stringify(value);
    if (typeof serialized !== "string") fail("controller_scan_surface_invalid");
    return Buffer.from(serialized, "utf8");
  } catch {
    // A cycle, BigInt or custom serializer must not leak its exception/payload.
    fail("controller_scan_surface_invalid");
  }
}

function sentinelNeedles(value) {
  // Pairing codes are seven characters, so the scanner must accept the real
  // shortest secret rather than replacing it with a longer synthetic value.
  if (typeof value !== "string" || value.length < 7 || Buffer.byteLength(value) > 4096) {
    throw new Error("controller_scan_sentinel_invalid");
  }
  const raw = Buffer.from(value, "utf8");
  let uri;
  try { uri = encodeURIComponent(value); } catch { fail("controller_scan_sentinel_invalid"); }
  const base64 = raw.toString("base64");
  const base64url = raw.toString("base64url");
  const hex = raw.toString("hex");
  const unicode = value.split("").map((unit) => `\\u${unit.charCodeAt(0).toString(16).padStart(4, "0")}`).join("");
  const variants = [value, base64, base64.replace(/=+$/u, ""), base64url,
    base64url.padEnd(base64.length, "="), hex, hex.toUpperCase(), JSON.stringify(value).slice(1, -1),
    unicode, unicode.replace(/[a-f]/gu, (unit) => unit.toUpperCase()), uri,
    hex.replace(/../gu, (byte) => `%${byte}`), hex.replace(/../gu, (byte) => `%${byte.toUpperCase()}`)];
  return [...new Set(variants)].map((variant) => Buffer.from(variant, "utf8"));
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
      fail("controller_scan_surfaces_invalid");
    }
    for (const name of Reflect.ownKeys(surfaces)) {
      if (!CONTROLLER_SCAN_SURFACES.includes(name)) {
        fail("controller_scan_surface_unknown");
      }
    }
    if (CONTROLLER_SCAN_SURFACES.some((name) => !Object.hasOwn(surfaces, name))) {
      fail("controller_scan_surfaces_incomplete");
    }
    const matches = [];
    for (const name of CONTROLLER_SCAN_SURFACES) {
      const property = Object.getOwnPropertyDescriptor(surfaces, name);
      if (!property || !Object.hasOwn(property, "value")) fail("controller_scan_surface_invalid");
      const haystack = surfaceBytes(property.value);
      if (this.needles.some((variants) => variants.some((needle) => haystack.indexOf(needle) !== -1))) {
        matches.push(name);
      }
    }
    return Object.freeze({
      clean: matches.length === 0,
      scanned_surfaces: CONTROLLER_SCAN_SURFACES,
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
