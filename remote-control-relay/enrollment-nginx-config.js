// Offline rollback deployment contract. Never reads credentials or manages nginx.
function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function port(value, code) {
  if (!/^\d+$/u.test(String(value))) fail(code);
  const number = Number(value);
  if (!Number.isSafeInteger(number) || number < 1024 || number > 65535) fail(code);
  return number;
}

export function loadEnrollmentNginxConfig(env) {
  const basePath = env.PINVOU_REMOTE_PUBLIC_BASE_PATH || "/pinvou3/remote";
  // nginx normally merges adjacent slashes before selecting an exact location.
  // Reject that ambiguous input instead of generating an unreachable location.
  if (!/^\/[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*$/u.test(basePath)) fail("gateway_base_path_invalid");
  const host = env.PINVOU_REMOTE_GATEWAY_HOST;
  if (host !== "127.0.0.1" && host !== "::1") fail("gateway_host_not_loopback");
  const gatewayPort = port(env.PINVOU_REMOTE_GATEWAY_PORT, "gateway_port_invalid");
  let upstream;
  try { upstream = new URL(env.PINVOU_REMOTE_LEGACY_RELAY_URL); } catch { fail("gateway_upstream_invalid"); }
  if (!["ws:", "wss:"].includes(upstream.protocol) || upstream.username || upstream.password
    || upstream.search || upstream.hash) fail("gateway_upstream_invalid");
  const legacyHost = upstream.hostname === "[::1]" ? "::1" : upstream.hostname;
  if (legacyHost !== "127.0.0.1" && legacyHost !== "::1") fail("gateway_upstream_not_loopback");
  if (env.PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST !== legacyHost
    || env.PINVOU_REMOTE_HOST !== legacyHost) fail("gateway_legacy_bind_mismatch");
  const legacyPort = port(env.PORT || "8787", "gateway_legacy_port_invalid");
  const upstreamPort = Number(upstream.port || (upstream.protocol === "wss:" ? 443 : 80));
  if (upstreamPort !== legacyPort || upstream.pathname !== `${basePath}/ws`) {
    fail("gateway_legacy_route_mismatch");
  }
  if (host === legacyHost && gatewayPort === legacyPort) fail("gateway_listener_collision");
  const trusted = String(env.PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS || "")
    .split(",").map((value) => value.trim()).filter(Boolean);
  if (!trusted.includes(host)) fail("gateway_proxy_not_trusted");
  let entrypoints;
  try { entrypoints = JSON.parse(env.PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON || "[]"); } catch {
    fail("gateway_public_entrypoints_invalid");
  }
  if (!Array.isArray(entrypoints) || entrypoints.length === 0) fail("gateway_public_entrypoints_invalid");
  for (const value of entrypoints) {
    let url;
    try { url = new URL(value); } catch { fail("gateway_public_entrypoints_invalid"); }
    if (typeof value !== "string" || url.protocol !== "https:" || url.username || url.password
      || url.search || url.hash || url.pathname !== basePath) fail("gateway_public_route_mismatch");
  }
  return Object.freeze({ basePath, host, gatewayPort });
}

export function renderEnrollmentNginxConfig(env) {
  const { basePath, host, gatewayPort } = loadEnrollmentNginxConfig(env);
  const authority = `${host === "::1" ? "[::1]" : host}:${gatewayPort}`;
  return `# Generated rollback perimeter; never proxy the legacy Relay directly.
# Regenerate/check with scripts/enrollment-nginx-config.js after env overrides.
location = ${basePath}/ws {
    proxy_pass http://${authority};
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    # Overwrite rather than append the immediate proxy source.
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
}

location = ${basePath}/healthz {
    proxy_pass http://${authority};
    proxy_set_header Host $host;
}
`;
}
