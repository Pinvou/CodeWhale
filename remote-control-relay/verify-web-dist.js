import { readFileSync, realpathSync, statSync } from "node:fs";
import { isAbsolute, relative, resolve, sep } from "node:path";

// Tokenize the generated entry without interpreting markup inside attributes,
// comments or raw/inert elements. No browser execution or runtime dependency.
function entryTags(html) {
  const tags = [];
  const titles = [];
  const lower = html.toLowerCase();
  const rawElements = new Set(["script", "style", "textarea", "title", "template",
    "xmp", "iframe", "noembed", "noframes", "noscript", "plaintext"]);
  let offset = 0;
  while (offset < html.length) {
    const start = html.indexOf("<", offset);
    if (start < 0) break;
    if (html.startsWith("<!--", start)) {
      const end = html.indexOf("-->", start + 4);
      if (end < 0) break;
      offset = end + 3;
      continue;
    }
    let quote = null;
    let end = start + 1;
    for (; end < html.length; end++) {
      const character = html[end];
      if (quote) { if (character === quote) quote = null; }
      else if (character === '"' || character === "'") quote = character;
      else if (character === ">") break;
    }
    if (end === html.length) throw new Error("unterminated Web entry tag");
    const tag = html.slice(start, end + 1);
    const name = /^<([a-z][a-z0-9:-]*)\b/i.exec(tag)?.[1]?.toLowerCase();
    offset = end + 1;
    if (name) tags.push({ name, source: tag });
    if (rawElements.has(name)) {
      // Generated entries have no nested inert templates. Reject these rather
      // than inventing HTML template/parser recovery semantics.
      if (name === "template" || name === "plaintext") throw new Error("unsupported inert Web entry");
      const closing = new RegExp(`</${name}(?=[\\s/>])`).exec(lower.slice(offset));
      if (!closing) throw new Error("unterminated raw Web entry element");
      if (name === "title") titles.push(html.slice(offset, offset + closing.index));
      offset += closing.index;
    }
    if (name === "svg" || name === "math") throw new Error("unsupported foreign Web entry");
  }
  return { tags, titles };
}

function entryAttributes(tag) {
  const start = /^<[a-z][a-z0-9:-]*/i.exec(tag);
  if (!start) throw new Error("invalid Web entry tag");
  const attributes = new Map();
  const body = tag.slice(start[0].length, -1);
  const pattern = /\s+([^\s"'<>\/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>]+)))?/y;
  let offset = 0;
  while (offset < body.length) {
    if (!body.slice(offset).trim()) break;
    if (body.slice(offset).trim() === "/") break;
    pattern.lastIndex = offset;
    const match = pattern.exec(body);
    if (!match) throw new Error("invalid Web entry attributes");
    const name = match[1].toLowerCase();
    if (attributes.has(name)) throw new Error("duplicate Web entry attribute");
    if (["src", "href", "type"].includes(name) && match[2] === undefined && match[3] === undefined) {
      throw new Error("Web resource attributes must be quoted");
    }
    attributes.set(name, match[2] ?? match[3] ?? match[4] ?? "");
    offset = pattern.lastIndex;
  }
  return attributes;
}

// Validate the entry's real local resources, not a stripped Tauri bridge.
// The deployment CLI imports this pure checker before replacement.
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
  // This verifier accepts the generated Vite entry, not arbitrary HTML.
  // Fail closed on ambiguous bases (including duplicate/comment/raw-text
  // lookalikes), rather than letting a string match hide the browser's first base.
  const { tags, titles } = entryTags(html);
  const baseTags = tags.filter((tag) => tag.name === "base");
  if (titles.length !== 1 || !titles[0].startsWith("PINVOU 智能助手")
    || (html.match(/<base\b/gi) ?? []).length !== 1 || baseTags.length !== 1
    || baseTags[0].source !== `<base href="${base}">`) {
    throw new Error("Web entry title/base mismatch");
  }
  let moduleEntry = false;
  let startup = false;
  for (const tag of tags.filter((tag) => tag.name === "script" || tag.name === "link")) {
    const attributes = entryAttributes(tag.source);
    const reference = attributes.get(tag.name === "script" ? "src" : "href");
    if (!reference || /^(?:https?:|data:|#|\/\/)/i.test(reference)) continue;
    const pathname = decodeURIComponent(reference.split(/[?#]/, 1)[0]);
    if (pathname.endsWith("/")) throw new Error("Web asset URL cannot end with a slash");
    const local = pathname.startsWith(base) ? pathname.slice(base.length) : pathname;
    containedFile(local);
    if (tag.name === "script" && attributes.get("type")?.toLowerCase() === "module" && local.startsWith("assets/")) {
      moduleEntry = true;
    }
    if (tag.name === "script" && local.startsWith("startup/")) startup = true;
  }
  if (!moduleEntry || !startup) throw new Error("Web module/startup entry missing");
}
