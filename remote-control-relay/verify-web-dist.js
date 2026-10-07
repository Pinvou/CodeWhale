import { readFileSync, realpathSync, statSync } from "node:fs";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

// Validate the entry's real local resources, not a stripped Tauri bridge.
// This same file is shipped with the Relay modules and runs before replacement.
export function verifyWebDist(directory, basePath) {
  const root = realpathSync(directory);
  const base = `${basePath.replace(/\/+$/, "")}/`;
  if (!base.startsWith("/") || base.split("/").some((part) => part === "." || part === "..") || base.includes("\\")) {
    throw new Error("invalid Web base path");
  }
  const containedFile = (local) => {
    if (isAbsolute(local) || local.includes("\\")) throw new Error("Web asset outside base");
    const target = realpathSync(resolve(root, local));
    const within = relative(root, target);
    if (within === ".." || within.startsWith(`..${sep}`) || isAbsolute(within) || !statSync(target).isFile()) {
      throw new Error("Web asset outside dist or not a file");
    }
    return target;
  };
  const html = readFileSync(containedFile("index.html"), "utf8");
  if (!html.includes('<title>PINVOU 智能助手') || !html.includes(`<base href="${base}">`)) {
    throw new Error("Web entry title/base mismatch");
  }
  let moduleEntry = false;
  let startup = false;
  for (const tag of html.matchAll(/<(?:script|link)\b[^>]*>/gi)) {
    const reference = tag[0].match(/\b(?:src|href)\s*=\s*["']([^"']+)["']/i)?.[1];
    if (!reference || /^(?:https?:|data:|#|\/\/)/i.test(reference)) continue;
    const pathname = decodeURIComponent(reference.split(/[?#]/, 1)[0]);
    const local = pathname.startsWith(base) ? pathname.slice(base.length) : pathname;
    containedFile(local);
    if (/^<script\b/i.test(tag[0]) && /\btype\s*=\s*["']module["']/i.test(tag[0]) && local.startsWith("assets/")) {
      moduleEntry = true;
    }
    if (/^<script\b/i.test(tag[0]) && local.startsWith("startup/")) startup = true;
  }
  if (!moduleEntry || !startup) throw new Error("Web module/startup entry missing");
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    verifyWebDist(process.argv[2], process.argv[3]);
  } catch {
    // Keep host paths and infrastructure details out of validation output.
    console.error("WebUI 产物校验失败：入口、base 或引用资源不完整");
    process.exitCode = 1;
  }
}
