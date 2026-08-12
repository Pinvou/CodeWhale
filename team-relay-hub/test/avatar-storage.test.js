import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import test from "node:test";
import {
  avatarStoragePath,
  detectAvatarContent,
  receiveAvatarMultipart,
} from "../src/avatar-service.js";

const ANIMATED_GIF = Buffer.from(
  "R0lGODlhAQABAIEAAAAAAP///wAAACH5BAEAAAEALAAAAAABAAEAAAICRAEAIfkEAQAAAQAsAAAAAAEAAQAAAgJEADs=",
  "base64",
);

test("avatar storage keys cannot escape the private avatar root", () => {
  const config = { attachmentDir: join(tmpdir(), "team-relay-avatar-root") };
  const path = avatarStoragePath(config, "ab/00000000-0000-4000-8000-000000000000");
  assert.match(path, /avatars[\\/]ab[\\/]00000000-0000-4000-8000-000000000000$/);
  assert.throws(() => avatarStoragePath(config, "../outside"), /invalid avatar storage key/);
});

test("avatar content detection preserves GIF and rejects misleading extensions", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-test-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const path = join(directory, "upload");
  await writeFile(path, ANIMATED_GIF);
  assert.equal(await detectAvatarContent({ filename: "avatar.gif", temporaryPath: path }), "image/gif");
  await assert.rejects(
    detectAvatarContent({ filename: "avatar.png", temporaryPath: path }),
    error => error?.status === 415 && error?.code === "avatar_type_not_allowed",
  );
});

test("one avatar multipart file stays below Busboy's sentinel parts limit", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-multipart-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const boundary = "pinvou-avatar-test-boundary";
  const request = Readable.from([
    Buffer.from(
      `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="avatar.gif"\r\nContent-Type: image/gif\r\n\r\n`,
    ),
    ANIMATED_GIF,
    Buffer.from(`\r\n--${boundary}--\r\n`),
  ]);
  request.headers = { "content-type": `multipart/form-data; boundary=${boundary}` };

  const upload = await receiveAvatarMultipart(request, {
    attachmentDir: directory,
    maxAvatarBytes: 2048,
  });

  assert.equal(upload.filename, "avatar.gif");
  assert.equal(upload.byteSize, ANIMATED_GIF.length);
  assert.deepEqual(await readFile(upload.temporaryPath), ANIMATED_GIF);
});
