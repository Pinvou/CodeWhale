#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";
import standaloneCode from "ajv/dist/standalone/index.js";

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const schemaPath = resolve(scriptDirectory, "../docs/schemas/remote-control-release-evidence.v1.schema.json");
const outputPath = resolve(scriptDirectory, "../docs/schemas/remote-control-release-evidence.v1.validator.mjs");
const schema = JSON.parse(await readFile(schemaPath, "utf8"));
const ajv = new Ajv2020({ allErrors: true, strict: true, code: { source: true, esm: true } });
const generated = standaloneCode(ajv, ajv.compile(schema));
const expected = `${generated}\n`;
if (process.argv.includes("--check")) {
  const existing = await readFile(outputPath, "utf8");
  if (existing !== expected) throw new Error("evidence_schema_validator_out_of_date");
} else {
  await writeFile(outputPath, expected, "utf8");
}
