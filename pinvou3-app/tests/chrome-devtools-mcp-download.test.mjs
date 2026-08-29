import assert from "node:assert/strict";
// A fake ClientRequest needs the EventEmitter-compatible `once`/`emit` API used by node:https.
// eslint-disable-next-line unicorn/prefer-event-target
import { EventEmitter } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import test from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const {
  downloadHttpsOnce,
  downloadWithRetries,
} = require("../scripts/tauri/chrome-devtools-mcp.js");

function fakeHttpsGet(routes, requests) {
  return (url, options, onResponse) => {
    const request = new EventEmitter();
    request.setTimeout = () => request;
    request.destroy = (error) => request.emit("error", error);
    queueMicrotask(() => {
      const key = url.toString();
      requests.push({ url: key, options });
      const route = routes.get(key);
      if (!route) {
        request.emit("error", new Error(`unexpected request: ${key}`));
        return;
      }
      const response = Readable.from(route.body ? [route.body] : []);
      response.statusCode = route.statusCode;
      response.headers = route.headers || {};
      onResponse(response);
    });
    return request;
  };
}

test("vendor download uses Node HTTPS and follows HTTPS redirects", async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-cdmcp-download-"));
  const destination = path.join(root, "package.tgz");
  const requests = [];
  const get = fakeHttpsGet(
    new Map([
      [
        "https://registry.example.test/package.tgz",
        { statusCode: 302, headers: { location: "/objects/package.tgz" } },
      ],
      [
        "https://registry.example.test/objects/package.tgz",
        { statusCode: 200, body: Buffer.from("pinned tarball") },
      ],
    ]),
    requests,
  );
  try {
    await downloadHttpsOnce("https://registry.example.test/package.tgz", destination, { get });
    assert.equal(fs.readFileSync(destination, "utf8"), "pinned tarball");
    assert.deepEqual(
      requests.map(({ url }) => url),
      [
        "https://registry.example.test/package.tgz",
        "https://registry.example.test/objects/package.tgz",
      ],
    );
    assert.equal(requests[0].options.headers.Accept, "application/octet-stream");
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("vendor download rejects redirects that leave HTTPS", async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-cdmcp-download-"));
  const destination = path.join(root, "package.tgz");
  const get = fakeHttpsGet(
    new Map([
      [
        "https://registry.example.test/package.tgz",
        {
          statusCode: 302,
          // eslint-disable-next-line unicorn/prefer-https -- this fixture verifies downgrade rejection
          headers: { location: "http://mirror.example.test/package.tgz" },
        },
      ],
    ]),
    [],
  );
  try {
    await assert.rejects(
      downloadHttpsOnce("https://registry.example.test/package.tgz", destination, { get }),
      /redirect to http:/,
    );
    assert.equal(fs.existsSync(destination), false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("vendor download retries without retaining a partial tarball", async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-cdmcp-download-"));
  const destination = path.join(root, "package.tgz");
  const sleeps = [];
  let attempts = 0;
  try {
    await downloadWithRetries("https://registry.example.test/package.tgz", destination, {
      attempts: 3,
      retryDelayMs: 7,
      sleep: async (milliseconds) => {
        sleeps.push(milliseconds);
      },
      download: async (_url, file) => {
        attempts += 1;
        if (attempts === 1) {
          fs.writeFileSync(file, "partial");
          throw new Error("transient network failure");
        }
        assert.equal(fs.existsSync(file), false, "a retry must not reuse partial bytes");
        fs.writeFileSync(file, "complete");
      },
    });
    assert.equal(attempts, 2);
    assert.deepEqual(sleeps, [7]);
    assert.equal(fs.readFileSync(destination, "utf8"), "complete");
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("vendor script has no external curl dependency", () => {
  const source = fs.readFileSync(
    path.join(import.meta.dirname, "..", "scripts", "tauri", "chrome-devtools-mcp.js"),
    "utf8",
  );
  assert.match(source, /require\("node:https"\)/);
  assert.doesNotMatch(source, /run\(\s*["']curl["']/);
});
