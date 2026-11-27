import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

type Clause = { min: [number, number, number]; maxMajor: number | null };

function parseClause(text: string, source: string): Clause {
  const caret = text.match(/^\^(\d+)\.(\d+)\.(\d+)$/);
  if (caret) {
    const major = Number(caret[1]);
    return {
      min: [major, Number(caret[2]), Number(caret[3])],
      maxMajor: major + 1,
    };
  }
  const gte = text.match(/^>=(\d+)(?:\.(\d+))?(?:\.(\d+))?$/);
  if (gte) {
    return {
      min: [Number(gte[1]), Number(gte[2] ?? 0), Number(gte[3] ?? 0)],
      maxMajor: null,
    };
  }
  throw new Error(
    `unsupported engines clause "${text}" in "${source}" — teach the parser before changing the range`,
  );
}

function parseRange(range: string, source: string): Clause[] {
  return range
    .split("||")
    .map((part) => parseClause(part.trim(), source));
}

function satisfies(version: [number, number, number], clauses: Clause[]): boolean {
  return clauses.some(({ min, maxMajor }) => {
    const [vMajor, vMinor, vPatch] = version;
    if (maxMajor !== null && vMajor >= maxMajor) return false;
    if (vMajor !== min[0]) return vMajor > min[0];
    if (vMinor !== min[1]) return vMinor > min[1];
    return vPatch >= min[2];
  });
}

function accepts(range: string, version: [number, number, number], source: string): boolean {
  return satisfies(version, parseRange(range, source));
}

describe("declared Node engines", () => {
  it("stays inside the installed lint and test toolchains' ranges", () => {
    // The engines field is the pre-install visibility the README's local
    // minimum documents (review 5947077435): without it, an incompatible
    // local Node surfaces as an opaque toolchain failure instead of an
    // install-time warning. The declared range must never advertise a
    // version either installed toolchain rejects — eslint 10 dropped
    // Node 22.12 while vitest still accepts it (review 6027839559), so
    // exact equality with one toolchain cannot express the contract.
    // Instead, every floor the declared range advertises must be accepted
    // by both installed toolchains, and known-rejected floors must stay
    // out. An unrecognised range shape fails here loudly instead of
    // silently passing.
    const read = (url: string): { engines?: { node?: string } } =>
      JSON.parse(readFileSync(new URL(url, import.meta.url), "utf8")) as {
        engines?: { node?: string };
      };
    const pkg = read("../package.json");
    const vitest = read("../node_modules/vitest/package.json");
    const eslint = read("../node_modules/eslint/package.json");
    const declared = pkg.engines?.node;
    const vitestRange = vitest.engines?.node;
    const eslintRange = eslint.engines?.node;
    expect(declared).toBeTypeOf("string");
    expect(vitestRange).toBeTypeOf("string");
    expect(eslintRange).toBeTypeOf("string");

    for (const clause of parseRange(declared as string, "package.json")) {
      expect(
        accepts(vitestRange as string, clause.min, `vitest ${vitestRange}`),
        `declared floor ${clause.min.join(".")} must be inside vitest's ${vitestRange}`,
      ).toBe(true);
      expect(
        accepts(eslintRange as string, clause.min, `eslint ${eslintRange}`),
        `declared floor ${clause.min.join(".")} must be inside eslint's ${eslintRange}`,
      ).toBe(true);
    }

    // Regression pins for the drift this guards (review 6027839559):
    // eslint 10's floor on the 22.x line is 22.13, not vitest's 22.12.
    expect(accepts(declared as string, [22, 12, 9], "declared")).toBe(false);
    expect(accepts(declared as string, [22, 13, 0], "declared")).toBe(true);
    expect(accepts(declared as string, [24, 1, 0], "declared")).toBe(true);
    expect(accepts(declared as string, [26, 0, 0], "declared")).toBe(true);
  });
});
