import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { avatarStoragePath, detectAvatarContent } from "../src/avatar-service.js";

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
  const animatedGif = Buffer.from(
    "R0lGODlhAQABAIEAAAAAAP///wAAACH5BAEAAAEALAAAAAABAAEAAAICRAEAIfkEAQAAAQAsAAAAAAEAAQAAAgJEADs=",
    "base64",
  );
  await writeFile(path, animatedGif);
  assert.equal(await detectAvatarContent({ filename: "avatar.gif", temporaryPath: path }), "image/gif");
  await assert.rejects(
    detectAvatarContent({ filename: "avatar.png", temporaryPath: path }),
    error => error?.status === 415 && error?.code === "avatar_type_not_allowed",
  );
});
