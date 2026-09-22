import { isAbsolute, relative, resolve, sep } from "node:path";

const DEFAULT_MAX_UPLOAD_BYTES = 4 * 1024 * 1024 * 1024;

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
    onlineVerifyTimeoutMs: parseInteger(
      environment.PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS || String(15 * 60 * 1000),
      "PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS",
      { min: 1000, max: 60 * 60 * 1000 },
    ),
  };
}
