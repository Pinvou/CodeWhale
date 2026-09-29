import test from "node:test";
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { inspectPeInstaller } from "../src/pe-inspector.js";
import { peFixture } from "./pe-fixture.js";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const realInstaller = resolve(
  repositoryRoot,
  "pinvou3-app/src-tauri/target/release/bundle/nsis/pinvou3_0.10.4_x64-setup.exe",
);

test("接受 I386/PE32 NSIS stub 和 AMD64/PE32+ stub", async () => {
  const directory = await mkdtemp(join(tmpdir(), "pinvou-pe-stub-"));
  try {
    const i386 = join(directory, "i386.exe");
    const amd64 = join(directory, "amd64.exe");
    await writeFile(i386, peFixture());
    await writeFile(amd64, peFixture({ machine: 0x8664, imageFormat: "pe32+" }));
    assert.deepEqual(
      { ...(await inspectPeInstaller(i386, "0.10.4")), versionSource: undefined },
      { machine: "I386", imageFormat: "PE32", version: "0.10.4.0", versionSource: undefined },
    );
    assert.equal((await inspectPeInstaller(amd64, "0.10.4")).machine, "AMD64");
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test(
  "本地真实 Tauri NSIS 产物通过外壳和版本检查",
  { skip: !existsSync(realInstaller) },
  async () => {
    const result = await inspectPeInstaller(realInstaller, "0.10.4");
    assert.equal(result.machine, "I386");
    assert.equal(result.imageFormat, "PE32");
    assert.equal(result.version, "0.10.4");
  },
);
