#!/usr/bin/env node
import crypto from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function exactObject(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).sort().join("\0") !== [...keys].sort().join("\0")) fail(code);
  return value;
}

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function canonicalJson(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`).join(",")}}`;
  return JSON.stringify(value);
}

function argument(name) {
  const index = process.argv.indexOf(name);
  return index < 0 ? undefined : process.argv[index + 1];
}

function assertPublicGateway(value) {
  let url;
  try { url = new URL(value); } catch { fail("legacy_gateway_url_invalid"); }
  if (url.protocol !== "wss:" || url.username || url.password || url.search || url.hash
    || /^(?:localhost|127\.0\.0\.1|\[?::1\]?)$/iu.test(url.hostname)) {
    fail("legacy_gateway_url_invalid");
  }
  return url.href;
}

function strictRoute(value, code) {
  exactObject(value, ["route_id", "gateway_url_sha256", "target", "enabled", "no_bypass"], code);
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$/u.test(value.route_id)
    || !/^[a-f0-9]{64}$/u.test(value.gateway_url_sha256)
    || value.target !== "legacy_relay" || typeof value.enabled !== "boolean" || value.no_bypass !== true) fail(code);
  return value;
}

export async function prepareLegacyGatewayRoute({ inventoryPath, configPath, gatewayUrl, runNonce, outputPath }) {
  if (!/^[a-f0-9]{64}$/u.test(runNonce || "")) fail("legacy_gateway_run_nonce_invalid");
  const normalizedUrl = assertPublicGateway(gatewayUrl);
  const [inventoryBytes, configBytes] = await Promise.all([readFile(resolve(inventoryPath)), readFile(resolve(configPath))]);
  let inventory; let config;
  try { inventory = JSON.parse(inventoryBytes); config = JSON.parse(configBytes); } catch { fail("legacy_gateway_inputs_invalid"); }
  exactObject(inventory, ["schema", "version", "public_gateways"], "legacy_gateway_inventory_schema_invalid");
  exactObject(config, ["schema", "version", "no_bypass", "routes"], "legacy_gateway_config_schema_invalid");
  if (inventory.schema !== "pinvou-remote-endpoint-inventory" || inventory.version !== 1 || !Array.isArray(inventory.public_gateways)
    || config.schema !== "pinvou-remote-gateway-config" || config.version !== 1 || config.no_bypass !== true || !Array.isArray(config.routes)) {
    fail("legacy_gateway_inputs_invalid");
  }
  const inventoryRoutes = inventory.public_gateways.map((item) => strictRoute(item, "legacy_gateway_inventory_schema_invalid"));
  const configRoutes = config.routes.map((item) => strictRoute(item, "legacy_gateway_config_schema_invalid"));
  const enabledInventory = inventoryRoutes.filter((item) => item.enabled);
  const enabledConfig = configRoutes.filter((item) => item.enabled);
  const gatewayUrlSha256 = sha256(Buffer.from(normalizedUrl, "utf8"));
  if (enabledInventory.length !== 1 || enabledConfig.length !== 1) fail("legacy_gateway_enabled_route_count_invalid");
  const left = enabledInventory[0]; const right = enabledConfig[0];
  if (left.route_id !== right.route_id || left.target !== right.target || left.no_bypass !== right.no_bypass
    || left.gateway_url_sha256 !== gatewayUrlSha256 || right.gateway_url_sha256 !== gatewayUrlSha256) {
    fail("legacy_gateway_route_mismatch");
  }
  const canonicalInventorySha256 = sha256(Buffer.from(canonicalJson(inventory), "utf8"));
  const canonicalGatewayConfigSha256 = sha256(Buffer.from(canonicalJson(config), "utf8"));
  const result = {
    schema: "pinvou-legacy-gateway-route-spec", version: 1, run_nonce: runNonce,
    route_id: left.route_id, gateway_url_sha256: gatewayUrlSha256, target: left.target, no_bypass: true,
    endpoint_inventory_sha256: sha256(inventoryBytes), gateway_config_sha256: sha256(configBytes),
    canonical_inventory_sha256: canonicalInventorySha256, canonical_gateway_config_sha256: canonicalGatewayConfigSha256,
    canonical_input_state_digest: sha256(Buffer.from(canonicalJson({ inventory_sha256: canonicalInventorySha256, gateway_config_sha256: canonicalGatewayConfigSha256 }), "utf8")),
  };
  const serialized = `${JSON.stringify(result)}\n`;
  if (/(?:password|secret|token|cookie|credential|private_key|authorization)/iu.test(serialized)) fail("legacy_gateway_route_spec_sensitive");
  await writeFile(resolve(outputPath), serialized, { encoding: "utf8", flag: "wx" });
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  prepareLegacyGatewayRoute({
    inventoryPath: argument("--inventory"), configPath: argument("--config"), gatewayUrl: argument("--gateway-url"),
    runNonce: argument("--run-nonce"), outputPath: argument("--output"),
  }).then((value) => process.stdout.write(`${JSON.stringify(value)}\n`)).catch((error) => {
    process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "legacy_gateway_route_prepare_failed" })}\n`);
    process.exitCode = 1;
  });
}
