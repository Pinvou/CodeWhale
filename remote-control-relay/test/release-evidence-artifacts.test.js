import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { test } from "node:test";
import { artifactIndex, readEvidenceArtifact } from "../scripts/release-evidence-artifacts.mjs";

const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
async function fixture(t, bytes = Buffer.alloc(160_000, 97)) {
  const root = await fs.mkdtemp(join(tmpdir(), "pinvou-artifact-read-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const path = join(root, "artifact.bin");
  await fs.writeFile(path, bytes);
  return { root, path, item: { path: "artifact.bin", kind: "check", sha256: digest(bytes) }, bytes };
}

test("digest lookup rejects either ordering of distinct signed identities", () => {
  const left = { path: "a.json", sha256: digest("same"), kind: "check", machine_role: "A" };
  for (const right of [{ ...left, path: "b.json" }, { ...left, path: "b.json", machine_role: "B" },
    { ...left, path: "b.json", kind: "log" }]) {
    for (const entries of [[left, right], [right, left]]) {
      assert.throws(() => artifactIndex(entries).get(left.sha256), /evidence_artifact_digest_ambiguous/u);
    }
  }
  assert.equal(artifactIndex([left]).get(left.sha256), left);
  assert.equal(artifactIndex([left]).get(digest("missing")), undefined);
});

test("oversize and empty files are rejected before open or content reads", async (t) => {
  const { root, path, item } = await fixture(t);
  let opens = 0;
  const open = fs.open;
  t.mock.method(fs, "open", (...args) => { opens++; return open(...args); });
  await fs.truncate(path, 32 * 1024 * 1024 + 1);
  await assert.rejects(readEvidenceArtifact(root, item), /evidence_artifact_size_invalid/u);
  await fs.truncate(path, 0);
  await assert.rejects(readEvidenceArtifact(root, item), /evidence_artifact_size_invalid/u);
  await fs.truncate(path, 1024 * 1024 * 1024 + 1);
  await assert.rejects(readEvidenceArtifact(root, { ...item, kind: "installer" }), /evidence_artifact_size_invalid/u);
  assert.equal(opens, 0);
});

test("text at its exact limit and binary data use bounded actual reads", async (t) => {
  const bytes = Buffer.alloc(32 * 1024 * 1024, 97);
  const { root, item } = await fixture(t, bytes);
  const open = fs.open;
  let reads = 0; let total = 0; let closes = 0;
  t.mock.method(fs, "open", async (...args) => {
    const handle = await open(...args);
    const read = handle.read.bind(handle); const close = handle.close.bind(handle);
    handle.read = async (...readArgs) => {
      assert.ok(readArgs[2] <= 64 * 1024);
      reads++;
      const result = await read(...readArgs); total += result.bytesRead; return result;
    };
    handle.close = async () => { closes++; await close(); };
    return handle;
  });
  assert.deepEqual(await readEvidenceArtifact(root, item), bytes);
  assert.equal(total, bytes.length);
  assert.equal(await readEvidenceArtifact(root, { ...item, kind: "installer" }), null);
  assert.equal(total, bytes.length * 2);
  assert.ok(reads > 1000); assert.equal(closes, 2);
});

test("actual growth and truncation during reading fail and close the handle", async (t) => {
  for (const action of ["grow", "shrink"]) {
    await t.test(action, async (t) => {
      const { root, path, item } = await fixture(t);
      const open = fs.open; let closes = 0; let reads = 0;
      t.mock.method(fs, "open", async (...args) => {
        const handle = await open(...args);
        const read = handle.read.bind(handle); const close = handle.close.bind(handle);
        handle.read = async (...readArgs) => {
          const result = await read(...readArgs);
          if (++reads === 1) {
            if (action === "grow") await fs.appendFile(path, "changed");
            else await fs.truncate(path, 1);
          }
          return result;
        };
        handle.close = async () => { closes++; await close(); };
        return handle;
      });
      await assert.rejects(readEvidenceArtifact(root, item), /evidence_artifact_changed/u);
      assert.equal(closes, 1);
    });
  }
});

test("later reads reject replacement bytes instead of trusting the earlier hash", async (t) => {
  const { root, path, item, bytes } = await fixture(t);
  assert.deepEqual(await readEvidenceArtifact(root, item), bytes);
  await fs.writeFile(path, Buffer.alloc(bytes.length, 98));
  await assert.rejects(readEvidenceArtifact(root, item), /evidence_artifact_digest_mismatch/u);
});

test("replacement between path check and open is rejected before reading", async (t) => {
  const { root, path, item } = await fixture(t);
  const open = fs.open; let reads = 0; let closes = 0;
  t.mock.method(fs, "open", async (...args) => {
    await fs.rename(path, join(root, "original.bin"));
    await fs.writeFile(path, "different file");
    const handle = await open(...args);
    const read = handle.read.bind(handle); const close = handle.close.bind(handle);
    handle.read = (...args) => { reads++; return read(...args); };
    handle.close = async () => { closes++; await close(); };
    return handle;
  });
  await assert.rejects(readEvidenceArtifact(root, item), /evidence_artifact_changed/u);
  assert.equal(reads, 0); assert.equal(closes, 1);
});
