import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const document = JSON.parse(await readFile(new URL('../../contracts/openapi/chargeback-principal.json', import.meta.url), 'utf8'));
const definitions = JSON.parse(JSON.stringify(document.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
const ajv = new Ajv2020({ strict: true, allErrors: true });
addFormats(ajv);
const id = 'urn:pinvou:test:chargeback-principal';
ajv.addSchema({ $id: id, $defs: definitions });
const validators = new Map(Object.keys(definitions).map(name => [name, ajv.compile({ $ref: `${id}#/$defs/${name}` })]));
export const validateChargebackPrincipalWire = (name, value) => validators.get(name)?.(value) === true;

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    let raw = '';
    for await (const part of process.stdin) {
      raw += part;
      if (Buffer.byteLength(raw, 'utf8') > 1048576) throw new Error('limit');
    }
    if (!validateChargebackPrincipalWire(process.argv[2], JSON.parse(raw))) throw new Error('invalid');
    process.stdout.write('VALID\n');
  } catch {
    process.stderr.write('INVALID_CHARGEBACK_PRINCIPAL_WIRE\n');
    process.exitCode = 1;
  }
}
