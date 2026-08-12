import assert from "node:assert/strict";
import { posix, win32 } from "node:path";
import test from "node:test";
import { expireClosed } from "../src/attachment-cleanup.js";
import { isPathInside } from "../src/attachment-service.js";

test("attachment paths stay inside the storage root on POSIX and Windows", () => {
  assert.equal(isPathInside("/srv/attachments", "/srv/attachments/ab/file", posix), true);
  assert.equal(isPathInside("/srv/attachments", "/srv/attachments-escape/file", posix), false);
  assert.equal(isPathInside("C:\\attachments", "C:\\attachments\\ab\\file", win32), true);
  assert.equal(isPathInside("C:\\attachments", "C:\\attachments-escape\\file", win32), false);
  assert.equal(isPathInside("C:\\attachments", "D:\\attachments\\ab\\file", win32), false);
});

test("closed attachment cleanup remains retryable after file removal fails", async () => {
  let deletedAt = null;
  const queries = [];
  const pool = {
    async query(sql) {
      queries.push(sql);
      if (sql.includes("SELECT storage_key")) {
        return deletedAt
          ? { rowCount: 0, rows: [] }
          : { rowCount: 1, rows: [{ storage_key: "ab/00000000-0000-4000-8000-000000000000" }] };
      }
      if (sql.includes("UPDATE attachments SET deleted_at")) {
        deletedAt = new Date();
        return { rowCount: 1, rows: [{ id: "attachment-id" }] };
      }
      throw new Error(`unexpected query: ${sql}`);
    },
  };
  const config = { attachmentDir: "/tmp/team-relay-attachment-test" };
  const candidate = { id: "attachment-id" };

  await assert.rejects(
    expireClosed(pool, config, candidate, async () => {
      const error = new Error("simulated I/O failure");
      error.code = "EACCES";
      throw error;
    }),
    /simulated I\/O failure/,
  );
  assert.equal(deletedAt, null, "failed file removal must not mark metadata as deleted");
  assert.equal(queries.filter(sql => sql.includes("UPDATE attachments SET deleted_at")).length, 0);

  let removedPath;
  assert.equal(await expireClosed(pool, config, candidate, async (path) => {
    removedPath = path;
  }), true);
  assert.equal(deletedAt instanceof Date, true);
  assert.match(removedPath, /ab[\\/]00000000-0000-4000-8000-000000000000$/);
});
