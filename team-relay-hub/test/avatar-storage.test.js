import assert from "node:assert/strict";
import {
  access,
  chmod,
  link,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  utimes,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough, Readable } from "node:stream";
import test from "node:test";
import sharp from "sharp";
import { removeAvatarOrphans } from "../src/attachment-cleanup.js";
import {
  MAX_AVATAR_BYTES,
  MAX_GIF_FRAMES,
  MAX_GIF_PIXELS,
  processAvatarImage,
  validateAvatarMetadata,
} from "../src/avatar-image.js";
import { AvatarProcessingGate, AvatarUploadGate } from "../src/avatar-processing-gate.js";
import {
  avatarReducedMotion,
  avatarStoragePath,
  avatarVariantSize,
  commitAvatarAssets,
  detectAvatarContent,
  persistAssets,
  receiveAvatarWithGate,
  receiveAvatarMultipart,
} from "../src/avatar-service.js";

const ANIMATED_GIF = Buffer.from(
  "R0lGODlhAQABAIEAAAAAAP///wAAACH5BAEAAAEALAAAAAABAAEAAAICRAEAIfkEAQAAAQAsAAAAAAEAAQAAAgJEADs=",
  "base64",
);

async function animatedGif({ width = 2, height = 2, frames = 2, loop = 0 } = {}) {
  const pixels = Buffer.alloc(width * height * frames * 4);
  for (let frame = 0; frame < frames; frame += 1) {
    const start = frame * width * height * 4;
    for (let offset = start; offset < start + width * height * 4; offset += 4) {
      pixels[offset] = (frame * 71) % 256;
      pixels[offset + 1] = 255 - ((frame * 43) % 256);
      pixels[offset + 2] = (frame * 29) % 256;
      pixels[offset + 3] = 255;
    }
  }
  return sharp(pixels, {
    raw: { width, height: height * frames, channels: 4, pageHeight: height },
  }).gif({ delay: Array(frames).fill(80), loop }).toBuffer();
}

async function writeUpload(directory, filename, bytes) {
  const path = join(directory, "upload");
  await writeFile(path, bytes);
  return { filename, temporaryPath: path, byteSize: bytes.length };
}

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

test("avatar processing fully decodes, bounds, and sanitizes static images", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-static-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const input = await sharp({
    create: { width: 800, height: 400, channels: 4, background: "#2878d0" },
  })
    .withExif({ IFD0: { Copyright: "must-not-survive" } })
    .jpeg({ quality: 90 })
    .toBuffer();
  const processed = await processAvatarImage(
    await writeUpload(directory, "avatar.jpg", input),
    { maxAvatarBytes: MAX_AVATAR_BYTES },
  );

  assert.equal(processed.main.contentType, "image/jpeg");
  assert.equal(processed.main.width, 512);
  assert.equal(processed.main.height, 256);
  assert.ok(processed.main.byteSize <= MAX_AVATAR_BYTES);
  const metadata = await sharp(processed.main.data).metadata();
  assert.equal(metadata.exif, undefined);
  assert.equal(metadata.icc, undefined);
  assert.equal(metadata.xmp, undefined);
  assert.deepEqual([...processed.variants.keys()], [48, 96]);
  assert.equal(processed.variants.get(48).width, 48);
  assert.equal(processed.variants.get(96).width, 96);
  assert.equal(processed.reducedMotionVariants.size, 0);
});

test("GIF processing keeps avatars continuously animated while bounding main and display variants", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-gif-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const input = await animatedGif({ width: 400, height: 100, frames: 3, loop: 2 });
  assert.equal((await sharp(input, { animated: true }).metadata()).loop, 2);
  const processed = await processAvatarImage(
    await writeUpload(directory, "avatar.gif", input),
    { maxAvatarBytes: MAX_AVATAR_BYTES },
  );

  assert.equal(processed.main.contentType, "image/gif");
  assert.equal(processed.main.width, 384);
  assert.equal(processed.main.frames, 3);
  assert.ok(processed.main.byteSize <= MAX_AVATAR_BYTES);
  const mainMetadata = await sharp(processed.main.data, { animated: true }).metadata();
  assert.equal(mainMetadata.loop, 0);
  assert.deepEqual(mainMetadata.delay, [80, 80, 80]);
  for (const [size, variant] of processed.variants) {
    assert.equal(variant.contentType, "image/gif");
    assert.equal(variant.width, size);
    assert.equal(variant.frames, 3);
    assert.ok(variant.byteSize <= MAX_AVATAR_BYTES);
    const metadata = await sharp(variant.data, { animated: true }).metadata();
    assert.equal(metadata.loop, 0);
    assert.deepEqual(metadata.delay, [80, 80, 80]);
  }
  assert.deepEqual([...processed.reducedMotionVariants.keys()], [48, 96]);
  for (const [size, variant] of processed.reducedMotionVariants) {
    assert.equal(variant.contentType, "image/png");
    assert.equal(variant.width, size);
    assert.equal(variant.frames, 1);
    assert.ok(variant.byteSize <= MAX_AVATAR_BYTES);
    const metadata = await sharp(variant.data).metadata();
    assert.equal(metadata.format, "png");
    assert.equal(metadata.pages, undefined);
  }
});

test("GIF re-encoding removes comment extensions from every animated output", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-gif-comment-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const original = await animatedGif({ width: 4, height: 4, frames: 3 });
  assert.equal(original.at(-1), 0x3b, "test fixture must end with a GIF trailer");
  const comment = Buffer.from("private-gif-comment", "ascii");
  const extension = Buffer.concat([
    Buffer.from([0x21, 0xfe, comment.length]),
    comment,
    Buffer.from([0x00]),
  ]);
  const input = Buffer.concat([original.subarray(0, -1), extension, original.subarray(-1)]);
  assert.ok(input.indexOf(comment) >= 0);
  const processed = await processAvatarImage(
    await writeUpload(directory, "commented.gif", input),
    { maxAvatarBytes: MAX_AVATAR_BYTES },
  );

  for (const asset of [
    processed.main,
    ...processed.variants.values(),
    ...processed.reducedMotionVariants.values(),
  ]) {
    assert.equal(asset.data.indexOf(comment), -1, "GIF comment payload must be stripped");
    const metadata = await sharp(asset.data, { animated: true }).metadata();
    assert.equal(metadata.pages ?? 1, asset.contentType === "image/gif" ? 3 : 1);
  }
});

test("metadata policy rejects excessive frames and decoded pixels before full decode", () => {
  assert.throws(
    () => validateAvatarMetadata({
      format: "png",
      width: 3_000,
      height: 3_000,
      pages: 1,
    }, "image/png"),
    error => error?.code === "avatar_image_limits_exceeded",
  );
  assert.throws(
    () => validateAvatarMetadata({
      format: "gif",
      width: 64,
      height: 64 * (MAX_GIF_FRAMES + 1),
      pageHeight: 64,
      pages: MAX_GIF_FRAMES + 1,
    }, "image/gif"),
    error => error?.code === "avatar_image_limits_exceeded",
  );
  assert.throws(
    () => validateAvatarMetadata({
      format: "gif",
      width: 384,
      height: 384 * 28,
      pageHeight: 384,
      pages: 28,
    }, "image/gif"),
    error => error?.code === "avatar_image_limits_exceeded"
      && 384 * 384 * 28 > MAX_GIF_PIXELS,
  );
});

test("oversized static sources are rejected before full bitmap decode", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-static-limits-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const input = await sharp({
    create: { width: 3_000, height: 3_000, channels: 3, background: "#2878d0" },
  }).png({ compressionLevel: 9 }).toBuffer();
  assert.ok(input.length < MAX_AVATAR_BYTES, "fixture must reach the decoded-pixel guard");
  await assert.rejects(
    processAvatarImage(
      await writeUpload(directory, "too-many-pixels.png", input),
      { maxAvatarBytes: MAX_AVATAR_BYTES },
    ),
    error => error?.status === 422 && error?.code === "avatar_image_limits_exceeded",
  );
});

test("real 61-frame GIF is rejected end to end", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-gif-frames-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const input = await animatedGif({ width: 4, height: 4, frames: 61 });
  const metadata = await sharp(input, { animated: true }).metadata();
  assert.equal(metadata.pages, 61, "fixture must exercise the decoder frame count");
  await assert.rejects(
    processAvatarImage(
      await writeUpload(directory, "too-many-frames.gif", input),
      { maxAvatarBytes: MAX_AVATAR_BYTES },
    ),
    error => error?.status === 422 && error?.code === "avatar_image_limits_exceeded",
  );
});

test("magic bytes without a decodable image are rejected", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-invalid-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const truncatedPng = Buffer.concat([
    Buffer.from("89504e470d0a1a0a0000000d49484452ffffffffffffffff0806000000", "hex"),
    Buffer.alloc(4),
  ]);
  assert.equal(truncatedPng.length, 33);
  await assert.rejects(
    processAvatarImage(
      await writeUpload(directory, "avatar.png", truncatedPng),
      { maxAvatarBytes: MAX_AVATAR_BYTES },
    ),
    error => error?.status === 422 && error?.code === "avatar_image_invalid",
  );
});

test("avatar variant size accepts only the published display variants", () => {
  assert.equal(avatarVariantSize(null), null);
  assert.equal(avatarVariantSize("48"), 48);
  assert.equal(avatarVariantSize(96), 96);
  assert.throws(() => avatarVariantSize("64"), error => error?.status === 400);
  assert.throws(() => avatarVariantSize("48px"), error => error?.status === 400);
  assert.equal(avatarReducedMotion(null), false);
  assert.equal(avatarReducedMotion("false"), false);
  assert.equal(avatarReducedMotion("true"), true);
  assert.throws(() => avatarReducedMotion("1"), error => error?.status === 400);
});

test("upload lifecycle gate rejects duplicate principals without consuming Sharp slots", () => {
  const uploadGate = new AvatarUploadGate();
  const releaseA = uploadGate.enter("principal-a");
  assert.throws(
    () => uploadGate.enter("principal-a"),
    error => error?.status === 409 && error?.code === "avatar_upload_in_progress",
  );
  const releaseB = uploadGate.enter("principal-b");
  const releaseC = uploadGate.enter("principal-c");
  releaseA();
  releaseB();
  releaseC();
});

test("processing gate caps only active image processing", () => {
  const gate = new AvatarProcessingGate({ maximumConcurrent: 2 });
  const releaseA = gate.enter();
  const releaseB = gate.enter();
  assert.throws(
    () => gate.enter(),
    error => error?.status === 429 && error?.code === "avatar_processing_busy",
  );
  releaseA();
  const releaseC = gate.enter();
  releaseA();
  releaseB();
  releaseC();
  assert.doesNotThrow(() => gate.enter()());
});

test("slow request bodies do not reserve global image-processing slots", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-slow-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const uploadGate = new AvatarUploadGate();
  const boundary = "pinvou-avatar-slow-boundary";
  const partialBody = `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="avatar.gif"\r\nContent-Type: image/gif\r\n\r\n`;
  const slowRequests = ["principal-a", "principal-b"].map(principalId => {
    const request = new PassThrough();
    request.headers = { "content-type": `multipart/form-data; boundary=${boundary}` };
    const receiving = receiveAvatarWithGate(
      request,
      { attachmentDir: directory, maxAvatarBytes: MAX_AVATAR_BYTES },
      uploadGate,
      principalId,
    );
    request.write(partialBody);
    request.write(ANIMATED_GIF.subarray(0, 8));
    return { request, receiving };
  });

  const complete = Readable.from([
    Buffer.from(partialBody),
    ANIMATED_GIF,
    Buffer.from(`\r\n--${boundary}--\r\n`),
  ]);
  complete.headers = { "content-type": `multipart/form-data; boundary=${boundary}` };
  const received = await receiveAvatarWithGate(
    complete,
    { attachmentDir: directory, maxAvatarBytes: MAX_AVATAR_BYTES },
    uploadGate,
    "principal-c",
  );
  received.release();
  await rm(received.upload.temporaryPath, { force: true });

  for (const { request } of slowRequests) request.emit("aborted");
  const interrupted = await Promise.allSettled(slowRequests.map(entry => entry.receiving));
  assert.ok(interrupted.every(result => result.status === "rejected"));
  assert.equal(uploadGate.activePrincipals.size, 0);
  assert.deepEqual(await readdir(join(directory, "avatars", ".tmp")), []);
});

test("gate rejection drains the body without creating avatar storage", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-gate-reject-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  let resumed = false;
  let entered = false;
  const request = {
    resume() { resumed = true; },
  };
  const gate = {
    enter() {
      entered = true;
      throw Object.assign(new Error("busy"), { status: 429 });
    },
  };
  await assert.rejects(
    receiveAvatarWithGate(
      request,
      { attachmentDir: directory, maxAvatarBytes: MAX_AVATAR_BYTES },
      gate,
      "principal-a",
    ),
    error => error?.status === 429,
  );
  assert.equal(entered, true);
  assert.equal(resumed, true);
  await assert.rejects(
    access(join(directory, "avatars", ".tmp")),
    error => error?.code === "ENOENT",
  );
});

test("upload lifecycle gate rate-limits each principal with bounded expiring state", () => {
  let now = 10_000;
  const gate = new AvatarUploadGate({
    maximumAttempts: 2,
    windowMs: 1_000,
    maximumEntries: 2,
    now: () => now,
  });
  gate.enter("principal-a")();
  gate.enter("principal-a")();
  assert.throws(
    () => gate.enter("principal-a"),
    error => error?.status === 429
      && error?.code === "avatar_upload_rate_limited"
      && error?.details?.retry_after_seconds === 1,
  );
  gate.enter("principal-b")();
  gate.enter("principal-c")();
  assert.equal(gate.attempts.size, 2);
  now += 1_001;
  assert.doesNotThrow(() => gate.enter("principal-a")());
  assert.equal(gate.attempts.size, 1);
});

test("avatar gates validate every resource-bound option", () => {
  for (const option of [
    "maximumAttempts",
    "windowMs",
    "maximumEntries",
  ]) {
    assert.throws(
      () => new AvatarUploadGate({ [option]: 0 }),
      error => error instanceof TypeError && error.message.includes(option),
    );
    assert.throws(
      () => new AvatarUploadGate({ [option]: Number.MAX_SAFE_INTEGER + 1 }),
      error => error instanceof TypeError && error.message.includes(option),
    );
  }
  for (const value of [0, Number.MAX_SAFE_INTEGER + 1]) {
    assert.throws(
      () => new AvatarProcessingGate({ maximumConcurrent: value }),
      error => error instanceof TypeError && error.message.includes("maximumConcurrent"),
    );
  }
});

test("transaction failure removes every newly persisted avatar asset", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-rollback-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const entries = [null, 48, 96].map((size, index) => {
    const id = `${index + 1}0000000-0000-4000-8000-00000000000${index + 1}`;
    const storageKey = `${id.slice(0, 2)}/${id}`;
    return {
      size,
      storageKey,
      finalPath: avatarStoragePath({ attachmentDir: directory }, storageKey),
      asset: {
        contentType: "image/png",
        byteSize: 4,
        sha256: "a".repeat(64),
      },
    };
  });
  for (const entry of entries) {
    await mkdir(join(entry.finalPath, ".."), { recursive: true });
    await writeFile(entry.finalPath, "test");
  }
  const databaseFailure = new Error("database unavailable");
  await assert.rejects(
    commitAvatarAssets(
      {},
      "00000000-0000-4000-8000-000000000000",
      entries,
      { attachmentDir: directory },
      async () => { throw databaseFailure; },
    ),
    databaseFailure,
  );
  for (const entry of entries) {
    await assert.rejects(access(entry.finalPath), error => error?.code === "ENOENT");
  }
});

test("post-link persistence errors immediately remove the observable final file", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-link-rollback-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const asset = {
    data: Buffer.from("image"),
    contentType: "image/png",
    byteSize: 5,
    sha256: "a".repeat(64),
  };
  const linkedPaths = [];
  const fileOperations = {
    writeFile,
    mkdir,
    rm,
    async link(source, destination) {
      await link(source, destination);
      linkedPaths.push(destination);
    },
    async chmod(path, mode) {
      if (linkedPaths.includes(path)) throw new Error("simulated final chmod failure");
      await chmod(path, mode);
    },
  };
  await assert.rejects(
    persistAssets(
      { attachmentDir: directory },
      {
        main: asset,
        variants: new Map([[48, asset], [96, asset]]),
        reducedMotionVariants: new Map(),
      },
      fileOperations,
    ),
    /simulated final chmod failure/,
  );
  assert.equal(linkedPaths.length, 1);
  await assert.rejects(access(linkedPaths[0]), error => error?.code === "ENOENT");
  assert.deepEqual(await readdir(join(directory, "avatars", ".tmp")), []);
});

test("interrupted multipart uploads remove their temporary file", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-abort-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const boundary = "pinvou-avatar-abort-boundary";
  const request = new PassThrough();
  request.headers = { "content-type": `multipart/form-data; boundary=${boundary}` };
  const receiving = receiveAvatarMultipart(request, {
    attachmentDir: directory,
    maxAvatarBytes: MAX_AVATAR_BYTES,
  });
  request.write(
    `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="avatar.gif"\r\nContent-Type: image/gif\r\n\r\n`,
  );
  request.write(Buffer.alloc(1024));
  request.emit("aborted");
  request.destroy();
  await assert.rejects(
    receiving,
    error => error?.status === 400 && /interrupted/i.test(error.message),
  );
  assert.deepEqual(await readdir(join(directory, "avatars", ".tmp")), []);
});

test("truncated multipart uploads are client errors and remove temporary files", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-truncated-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const boundary = "pinvou-avatar-truncated-boundary";
  const request = Readable.from([
    Buffer.from(
      `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="avatar.gif"\r\nContent-Type: image/gif\r\n\r\n`,
    ),
    ANIMATED_GIF,
  ]);
  request.headers = { "content-type": `multipart/form-data; boundary=${boundary}` };
  await assert.rejects(
    receiveAvatarMultipart(request, {
      attachmentDir: directory,
      maxAvatarBytes: MAX_AVATAR_BYTES,
    }),
    error => error?.status === 400 && error?.code === "invalid_request",
  );
  assert.deepEqual(await readdir(join(directory, "avatars", ".tmp")), []);
});

test("avatar cleanup removes stale temporary and unreferenced final files", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-cleanup-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const temporaryDir = join(directory, "avatars", ".tmp");
  const shardDir = join(directory, "avatars", "ab");
  await mkdir(temporaryDir, { recursive: true });
  await mkdir(shardDir, { recursive: true });
  const temporaryName = "10000000-0000-4000-8000-000000000001";
  const orphanName = "20000000-0000-4000-8000-000000000002";
  const referencedName = "30000000-0000-4000-8000-000000000003";
  const temporaryPath = join(temporaryDir, temporaryName);
  const orphanPath = join(shardDir, orphanName);
  const referencedPath = join(shardDir, referencedName);
  await Promise.all([
    writeFile(temporaryPath, "temporary"),
    writeFile(orphanPath, "orphan"),
    writeFile(referencedPath, "referenced"),
  ]);
  const stale = new Date(Date.now() - 2 * 60 * 60 * 1000);
  await Promise.all([
    utimes(temporaryPath, stale, stale),
    utimes(orphanPath, stale, stale),
    utimes(referencedPath, stale, stale),
  ]);
  const pool = {
    async query() {
      return { rows: [{ storage_key: `ab/${referencedName}` }] };
    },
  };
  const result = await removeAvatarOrphans(pool, {
    attachmentDir: directory,
    orphanAttachmentHours: 1,
  });
  assert.deepEqual(result, { temporary: 1, orphaned: 1 });
  await assert.rejects(access(temporaryPath), error => error?.code === "ENOENT");
  await assert.rejects(access(orphanPath), error => error?.code === "ENOENT");
  await access(referencedPath);
});

test("referenced avatar batches cannot starve later orphan cleanup", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "team-relay-avatar-cleanup-fair-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const referencedDir = join(directory, "avatars", "00");
  const orphanDir = join(directory, "avatars", "01");
  await mkdir(referencedDir, { recursive: true });
  await mkdir(orphanDir, { recursive: true });
  const stale = new Date(Date.now() - 2 * 60 * 60 * 1000);
  const referencedKeys = [];
  for (let offset = 0; offset < 500; offset += 100) {
    await Promise.all(Array.from({ length: 100 }, async (_unused, index) => {
      const suffix = String(offset + index + 1).padStart(12, "0");
      const name = `00000000-0000-4000-8000-${suffix}`;
      const path = join(referencedDir, name);
      referencedKeys.push(`00/${name}`);
      await writeFile(path, "referenced");
      await utimes(path, stale, stale);
    }));
  }
  const orphanName = "ffffffff-ffff-4fff-8fff-ffffffffffff";
  const orphanKey = `01/${orphanName}`;
  const orphanPath = join(orphanDir, orphanName);
  await writeFile(orphanPath, "orphan");
  await utimes(orphanPath, stale, stale);

  let queries = 0;
  const pool = {
    async query(_sql, [candidateKeys]) {
      queries += 1;
      return {
        rows: candidateKeys
          .filter(storageKey => storageKey !== orphanKey)
          .map(storageKey => ({ storage_key: storageKey })),
      };
    },
  };
  const result = await removeAvatarOrphans(pool, {
    attachmentDir: directory,
    orphanAttachmentHours: 1,
  });
  assert.deepEqual(result, { temporary: 0, orphaned: 1 });
  assert.ok(queries >= 2, "the scanner must continue past a fully referenced batch");
  await assert.rejects(access(orphanPath), error => error?.code === "ENOENT");
  await access(avatarStoragePath({ attachmentDir: directory }, referencedKeys[0]));
});

test("hardening migration invalidates every unsanitized legacy avatar before tightening limits", async () => {
  const migration = await readFile(
    new URL("../../docs/team-relay/migrations/0011_avatar_hardening.sql", import.meta.url),
    "utf8",
  );
  const deleteLegacy = migration.indexOf("DELETE FROM principal_avatars;");
  const tightenLimit = migration.indexOf("CHECK (byte_size > 0 AND byte_size <= 1572864)");
  const createVariants = migration.indexOf("CREATE TABLE principal_avatar_variants");
  assert.ok(deleteLegacy >= 0, "legacy raw avatar rows must be invalidated");
  assert.ok(deleteLegacy < tightenLimit, "old 1.5–5 MiB rows must not make the new constraint fail");
  assert.ok(deleteLegacy < createVariants, "legacy rows cannot be treated as safely processed variants");
});

test("reduced-motion migration stores static and animated variants independently", async () => {
  const migration = await readFile(
    new URL("../../docs/team-relay/migrations/0012_avatar_reduced_motion.sql", import.meta.url),
    "utf8",
  );
  assert.match(migration, /animation_mode varchar\(16\) NOT NULL DEFAULT 'animated'/);
  assert.match(migration, /PRIMARY KEY \(principal_id, size, animation_mode\)/);
  assert.match(migration, /0012_avatar_reduced_motion/);
});
