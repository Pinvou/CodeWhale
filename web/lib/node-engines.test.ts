import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("declared Node engines", () => {
  it("tracks the range the installed vitest toolchain requires", () => {
    // The engines field is the pre-install visibility the README's local
    // minimum documents (review 5947077435): without it, an incompatible
    // local Node surfaces as an opaque vitest failure instead of an
    // install-time warning. The declared range must keep tracking the
    // vitest version the lockfile installs, so the expected value is read
    // from the installed package rather than restated as a constant — a
    // vitest bump that changes its supported range fails here until the
    // declared field moves with it.
    const pkg = JSON.parse(
      readFileSync(new URL("../package.json", import.meta.url), "utf8"),
    );
    const vitest = JSON.parse(
      readFileSync(
        new URL("../node_modules/vitest/package.json", import.meta.url),
        "utf8",
      ),
    );
    expect(pkg.engines?.node).toBeTypeOf("string");
    expect(pkg.engines.node).toBe(vitest.engines.node);
  });
});
