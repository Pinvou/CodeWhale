import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { AppError, badRequest } from "./errors.js";
import { loadConfig } from "./config.js";
import { ReleaseService } from "./release-service.js";

const sourceDir = dirname(fileURLToPath(import.meta.url));
const indexPath = resolve(sourceDir, "../public/index.html");

function sendJson(response, status, value) {
  const body = `${JSON.stringify(value)}\n`;
  response.writeHead(status, {
    "Cache-Control": "no-store",
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
    "X-Content-Type-Options": "nosniff",
  });
  response.end(body);
}

function sendEmpty(response, status) {
  response.writeHead(status, {
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  response.end();
}

function tokenDigest(value) {
  return createHash("sha256").update(value).digest();
}

function authorized(request, expectedToken) {
  const match = /^Bearer ([^\s]{32,512})$/u.exec(request.headers.authorization || "");
  if (!match) return false;
  return timingSafeEqual(tokenDigest(match[1]), tokenDigest(expectedToken));
}

async function readJsonBody(request, limit = 16 * 1024) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > limit) throw new AppError(413, "body_too_large", "请求体过大");
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw badRequest("invalid_json", "请求体必须是有效 JSON");
  }
}

function routePath(pathname, basePath) {
  if (!basePath) return pathname;
  if (pathname === basePath) return "redirect";
  if (!pathname.startsWith(`${basePath}/`)) return null;
  return pathname.slice(basePath.length) || "/";
}

function requestAddress(request) {
  const direct = request.socket.remoteAddress || "unknown";
  const isLoopback = direct === "127.0.0.1" || direct === "::1" || direct === "::ffff:127.0.0.1";
  const forwarded = request.headers["x-real-ip"];
  if (isLoopback && typeof forwarded === "string" && /^[0-9a-f:.]{2,64}$/iu.test(forwarded)) {
    return forwarded;
  }
  return direct;
}

function errorResponse(response, error) {
  if (error instanceof AppError) {
    sendJson(response, error.status, { error: error.code, message: error.message });
    return;
  }
  console.error(error);
  sendJson(response, 500, { error: "internal_error", message: "服务端内部错误" });
}

export async function createAppServer(config, dependencies = {}) {
  const service = dependencies.service || new ReleaseService(config);
  await service.initialize();
  const indexTemplate = await readFile(indexPath, "utf8");

  const server = createServer(async (request, response) => {
    try {
      const url = new URL(request.url, "http://localhost");
      const path = routePath(url.pathname, config.basePath);
      if (path === null) {
        sendJson(response, 404, { error: "not_found", message: "资源不存在" });
        return;
      }
      if (path === "redirect") {
        response.writeHead(308, { Location: `${config.basePath}/` });
        response.end();
        return;
      }
      if (request.method === "GET" && path === "/healthz") {
        sendJson(response, 200, { status: "ok" });
        return;
      }
      if (request.method === "GET" && path === "/") {
        const nonce = randomBytes(18).toString("base64");
        const body = indexTemplate.replaceAll("__CSP_NONCE__", nonce);
        response.writeHead(200, {
          "Cache-Control": "no-store",
          "Content-Security-Policy": `default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`,
          "Content-Type": "text/html; charset=utf-8",
          "Content-Length": Buffer.byteLength(body),
          "Referrer-Policy": "no-referrer",
          "X-Content-Type-Options": "nosniff",
          "X-Frame-Options": "DENY",
        });
        response.end(body);
        return;
      }
      if (!path.startsWith("/api/")) {
        sendJson(response, 404, { error: "not_found", message: "资源不存在" });
        return;
      }
      if (!authorized(request, config.adminToken)) {
        response.setHeader("WWW-Authenticate", "Bearer");
        sendJson(response, 401, { error: "unauthorized", message: "管理令牌无效" });
        return;
      }

      const remoteAddress = requestAddress(request);
      if (request.method === "GET" && path === "/api/status") {
        sendJson(response, 200, await service.getStatus());
        return;
      }
      if (request.method === "POST" && path === "/api/releases/windows/upload") {
        const contentLength = Number(request.headers["content-length"] || 0);
        if (contentLength > config.maxUploadBytes + 64 * 1024) {
          throw new AppError(413, "upload_too_large", "安装器超过服务端允许的大小");
        }
        sendJson(response, 201, await service.createDraft(request, remoteAddress));
        return;
      }
      if (request.method === "DELETE" && path === "/api/releases/windows/draft") {
        const body = await readJsonBody(request);
        sendJson(response, 200, await service.cancelDraft(body.draftId, body.operator, remoteAddress));
        return;
      }
      if (request.method === "POST" && path === "/api/releases/windows/publish") {
        const body = await readJsonBody(request);
        sendJson(response, 200, await service.publish(body.draftId, body.operator, remoteAddress));
        return;
      }
      if (request.method === "POST" && path === "/api/releases/windows/rollback") {
        const body = await readJsonBody(request);
        sendJson(
          response,
          200,
          await service.rollback(body.releaseId, body.operator, body.reason, remoteAddress),
        );
        return;
      }
      if (request.method === "OPTIONS") {
        sendEmpty(response, 204);
        return;
      }
      sendJson(response, 404, { error: "not_found", message: "资源不存在" });
    } catch (error) {
      errorResponse(response, error);
    }
  });
  server.requestTimeout = 60 * 60 * 1000;
  server.headersTimeout = 65 * 1000;
  server.keepAliveTimeout = 5 * 1000;
  return { server, service };
}

async function main() {
  const config = loadConfig();
  const { server } = await createAppServer(config);
  server.listen(config.port, config.host, () => {
    console.log(`Pinvou update release console listening on http://${config.host}:${config.port}${config.basePath || "/"}`);
  });
}

const entryPoint = process.argv[1] ? pathToFileURL(resolve(process.argv[1])).href : "";
if (import.meta.url === entryPoint) {
  main().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
}
