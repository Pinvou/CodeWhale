import { readFile } from 'node:fs/promises';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const document = JSON.parse(await readFile(new URL('../../contracts/openapi/commerce-manual-completion.json', import.meta.url), 'utf8'));
const definitions = JSON.parse(JSON.stringify(document.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
const id = 'urn:pinvou:test:commerce-manual-completion';
const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);
ajv.addSchema({ $id: id, $defs: definitions });
const schemas = new Map(Object.keys(definitions).map(name => [name, ajv.compile({ $ref: `${id}#/$defs/${name}` })]));

// Test-only structural consumer. Provider evidence and human authorization are
// verified by the real domain tests, not by JSON Schema or this process.
try {
  let raw = '';
  for await (const part of process.stdin) {
    raw += part;
    if (Buffer.byteLength(raw, 'utf8') > 1048576) throw new Error('limit');
  }
  if (schemas.get(process.argv[2])?.(JSON.parse(raw)) !== true) throw new Error('invalid');
  process.stdout.write('VALID\n');
} catch {
  process.stderr.write('INVALID_COMMERCE_REPAIR_WIRE\n');
  process.exitCode = 1;
}
