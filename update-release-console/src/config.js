import { isAbsolute, relative, resolve, sep } from "node:path";
import { isIP } from "node:net";

const DEFAULT_MAX_UPLOAD_BYTES = 4 * 1024 * 1024 * 1024;
const DEFAULT_UPLOAD_IDLE_TIMEOUT_MS = 2 * 60 * 1000;

function required(environment, name) {
  const value = environment[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function parseInteger(value, name, { min, max }) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < min || parsed > max) {
    throw new Error(`${name} must be an integer between ${min} and ${max}`);
  }
  return parsed;
}

function normalizedBasePath(value) {
  const trimmed = (value || "/").trim();
  if (!trimmed.startsWith("/") || trimmed.includes("?") || trimmed.includes("#")) {
    throw new Error("PINVOU_UPDATE_CONSOLE_BASE_PATH must be an absolute URL path");
  }
  if (trimmed === "/") return "";
  return trimmed.replace(/\/+$/u, "");
}

function isInside(parent, candidate) {
  const path = relative(parent, candidate);
  return path === "" || (!path.startsWith(`..${sep}`) && path !== ".." && !isAbsolute(path));
}

function ipv4Number(address) {
  return address.split(".").reduce((value, octet) => ((value << 8) | Number(octet)) >>> 0, 0);
}

export function parseTrustedProxies(value = "") {
  if (!value.trim()) return [];
  return value.split(",").map((raw) => {
    const rule = raw.trim();
    const [address, prefixText] = rule.split("/");
    const family = isIP(address);
    if (!family || rule.split("/").length > 2) {
      throw new Error(`PINVOU_UPDATE_TRUSTED_PROXIES contains invalid address: ${rule}`);
    }
    const prefix = prefixText === undefined ? (family === 4 ? 32 : 128) : Number(prefixText);
    if (!Number.isInteger(prefix) || prefix < 0 || prefix > (family === 4 ? 32 : 128)) {
      throw new Error(`PINVOU_UPDATE_TRUSTED_PROXIES contains invalid CIDR: ${rule}`);
    }
    if (family === 6 && prefix !== 128) {
      throw new Error("PINVOU_UPDATE_TRUSTED_PROXIES currently supports IPv6 hosts only (/128)");
    }
    return { address, family, prefix, network: family === 4 ? ipv4Number(address) : address.toLowerCase() };
  });
}

export function isTrustedProxy(remoteAddress, rules) {
  const address = remoteAddress.startsWith("::ffff:") ? remoteAddress.slice(7) : remoteAddress;
  const family = isIP(address);
  return rules.some((rule) => {
    if (rule.family !== family) return false;
    if (family === 6) return address.toLowerCase() === rule.network;
    const mask = rule.prefix === 0 ? 0 : (0xffffffff << (32 - rule.prefix)) >>> 0;
    return (ipv4Number(address) & mask) === (rule.network & mask);
  });
}

export function loadConfig(environment = process.env) {
  const publicRoot = resolve(required(environment, "PINVOU_UPDATE_PUBLIC_ROOT"));
  const stateRoot = resolve(required(environment, "PINVOU_UPDATE_STATE_ROOT"));
  if (isInside(publicRoot, stateRoot)) {
    throw new Error("PINVOU_UPDATE_STATE_ROOT must not be inside PINVOU_UPDATE_PUBLIC_ROOT");
  }

  const publicBaseUrl = new URL(required(environment, "PINVOU_UPDATE_PUBLIC_BASE_URL"));
  if (
    publicBaseUrl.protocol !== "https:"
    || publicBaseUrl.username
    || publicBaseUrl.password
    || publicBaseUrl.search
    || publicBaseUrl.hash
  ) {
    throw new Error("PINVOU_UPDATE_PUBLIC_BASE_URL must be a clean HTTPS URL");
  }

  const adminToken = required(environment, "PINVOU_UPDATE_ADMIN_TOKEN");
  if (adminToken.length < 32 || adminToken.length > 512) {
    throw new Error("PINVOU_UPDATE_ADMIN_TOKEN must contain 32 to 512 characters");
  }

  return {
    host: environment.PINVOU_UPDATE_HOST?.trim() || "127.0.0.1",
    port: parseInteger(environment.PINVOU_UPDATE_PORT || "8792", "PINVOU_UPDATE_PORT", {
      min: 1,
      max: 65535,
    }),
    basePath: normalizedBasePath(environment.PINVOU_UPDATE_CONSOLE_BASE_PATH),
    publicRoot,
    stateRoot,
    publicBaseUrl: publicBaseUrl.href.replace(/\/+$/u, ""),
    adminToken,
    maxUploadBytes: parseInteger(
      environment.PINVOU_UPDATE_MAX_UPLOAD_BYTES || String(DEFAULT_MAX_UPLOAD_BYTES),
      "PINVOU_UPDATE_MAX_UPLOAD_BYTES",
      { min: 1, max: Number.MAX_SAFE_INTEGER },
    ),
    uploadIdleTimeoutMs: parseInteger(
      environment.PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS || String(DEFAULT_UPLOAD_IDLE_TIMEOUT_MS),
      "PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS",
      // The bundled Nginx configuration uses a 150-second client body idle
      // timeout. Keep the application watchdog at least 30 seconds lower so
      // it can return a structured error and clean staging first.
      { min: 1000, max: DEFAULT_UPLOAD_IDLE_TIMEOUT_MS },
    ),
    onlineVerifyTimeoutMs: parseInteger(
      environment.PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS || String(15 * 60 * 1000),
      "PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS",
      { min: 1000, max: 60 * 60 * 1000 },
    ),
    trustedProxies: parseTrustedProxies(environment.PINVOU_UPDATE_TRUSTED_PROXIES),
    authFailureLimit: parseInteger(
      environment.PINVOU_UPDATE_AUTH_FAILURE_LIMIT || "5",
      "PINVOU_UPDATE_AUTH_FAILURE_LIMIT",
      { min: 2, max: 100 },
    ),
    authFailureWindowMs: parseInteger(
      environment.PINVOU_UPDATE_AUTH_FAILURE_WINDOW_MS || String(5 * 60 * 1000),
      "PINVOU_UPDATE_AUTH_FAILURE_WINDOW_MS",
      { min: 1000, max: 24 * 60 * 60 * 1000 },
    ),
    authBlockMs: parseInteger(
      environment.PINVOU_UPDATE_AUTH_BLOCK_MS || String(15 * 60 * 1000),
      "PINVOU_UPDATE_AUTH_BLOCK_MS",
      { min: 1000, max: 24 * 60 * 60 * 1000 },
    ),
    authLimiterTtlMs: parseInteger(
      environment.PINVOU_UPDATE_AUTH_LIMITER_TTL_MS || String(60 * 60 * 1000),
      "PINVOU_UPDATE_AUTH_LIMITER_TTL_MS",
      { min: 1000, max: 7 * 24 * 60 * 60 * 1000 },
    ),
    authLimiterMaxEntries: parseInteger(
      environment.PINVOU_UPDATE_AUTH_LIMITER_MAX_ENTRIES || "10000",
      "PINVOU_UPDATE_AUTH_LIMITER_MAX_ENTRIES",
      { min: 100, max: 1_000_000 },
    ),
  };
}
