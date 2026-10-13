import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { RelayStateStore } from "../relay-state-store.js";

test("dedicated directory rejects foreign file, directory and real link without changing evidence", async (t) => {
  for (const kind of ["file", "directory", "link"]) {
    await t.test(kind, async (t) => {
      const temporaryRoot = await fs.realpath(os.tmpdir());
      const fixtureRoot = await fs.realpath(await fs.mkdtemp(path.join(temporaryRoot, "pinvou-dir-boundary-")));
      const statePath = path.join(fixtureRoot, "controller", "state.json");
      const options = {
        statePath,
        legacyStatePath: path.join(fixtureRoot, "legacy.json"),
        processIdentity: async (pid) => `directory-fixture:${pid}`,
        ...(process.platform === "win32" ? {
          protectPath: async () => {}, verifyPath: async () => {}, verifyAncestors: async () => {},
        } : {}),
      };
      let store;
      t.after(async () => {
        await store?.close();
        // Delete only this canonical test-owned directory under the captured temp root.
        const cleanupRoot = await fs.realpath(fixtureRoot);
        const relative = path.relative(temporaryRoot, cleanupRoot);
        assert.equal(cleanupRoot, fixtureRoot);
        assert.ok(relative && !relative.startsWith("..") && !path.isAbsolute(relative));
        assert.ok(path.basename(cleanupRoot).startsWith("pinvou-dir-boundary-"));
        await fs.rm(cleanupRoot, { recursive: true, force: true });
      });
      store = await RelayStateStore.open(options);
      await store.compareAndSwap("directory_evidence", "0", { retained: "fixture-only evidence" });
      await store.close();
      const directory = path.dirname(statePath);
      const alien = path.join(directory, "foreign-entry");
      const target = path.join(fixtureRoot, "link-target");
      if (kind === "file") await fs.writeFile(alien, "foreign fixture evidence");
      if (kind === "directory") {
        await fs.mkdir(alien);
        await fs.writeFile(path.join(alien, "retained.txt"), "foreign fixture evidence");
      }
      if (kind === "link") {
        await fs.mkdir(target);
        await fs.writeFile(path.join(target, "retained.txt"), "foreign fixture evidence");
        await fs.symlink(target, alien, process.platform === "win32" ? "junction" : "dir");
        assert.ok((await fs.lstat(alien)).isSymbolicLink());
        assert.equal(await fs.realpath(alien), await fs.realpath(target));
      }
      const identity = await fs.lstat(alien);
      const entries = (await fs.readdir(directory)).sort();
      const protectedPaths = [statePath, `${statePath}.bak`, path.join(directory, ".pinvou-controller-state-directory")];
      const bytes = await Promise.all(protectedPaths.map((name) => fs.readFile(name)));
      for (let attempt = 0; attempt < 2; attempt += 1) {
        await assert.rejects(async () => {
          const unexpected = await RelayStateStore.open(options);
          await unexpected.close();
        }, { code: "relay_state_directory_not_dedicated" });
        assert.deepEqual((await fs.readdir(directory)).sort(), entries);
        for (let index = 0; index < protectedPaths.length; index += 1) {
          assert.deepEqual(await fs.readFile(protectedPaths[index]), bytes[index]);
        }
        const after = await fs.lstat(alien);
        assert.equal(after.ino, identity.ino);
        assert.equal(after.dev, identity.dev);
        assert.equal(after.isSymbolicLink(), identity.isSymbolicLink());
        assert.equal(await fs.readFile(kind === "file" ? alien : path.join(alien, "retained.txt"), "utf8"), "foreign fixture evidence");
      }
      if (kind === "link") assert.equal(await fs.readFile(path.join(target, "retained.txt"), "utf8"), "foreign fixture evidence");
    });
  }
});
