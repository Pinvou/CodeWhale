import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);
const schemas = new Map();
for (const service of ['account', 'billing', 'token']) {
  const suffix = service === 'account' ? '' : `-${service}`;
  const document = JSON.parse(await readFile(new URL(`../../contracts/openapi/approval-recovery${suffix}.json`, import.meta.url), 'utf8'));
  // OpenAPI is not itself a JSON Schema. Compile its actual DTO components,
  // resolving only local component references into a closed JSON Schema root.
  const definitions = JSON.parse(JSON.stringify(document.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  const id = `urn:pinvou:test:approval-recovery:${service}`;
  ajv.addSchema({ $id: id, $defs: definitions });
  for (const name of Object.keys(definitions)) schemas.set(`${service}:${name}`, ajv.compile({ $ref: `${id}#/$defs/${name}` }));
}
export function validateApprovalRecoveryWire(service, name, value) {
  return schemas.get(`${service}:${name}`)?.(value) === true;
}
// Test-only consumer: accepts actual Go-serialized DTOs on stdin. It neither
// contacts an authority nor prints original keys, leases, payloads or errors.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    let raw = '';
    for await (const part of process.stdin) {
      raw += part;
      if (Buffer.byteLength(raw, 'utf8') > 1048576) throw new Error('limit');
    }
    if (!validateApprovalRecoveryWire(process.argv[2], process.argv[3], JSON.parse(raw))) throw new Error('invalid');
    process.stdout.write('VALID\n');
  } catch {
    process.stderr.write('INVALID_APPROVAL_RECOVERY_WIRE\n');
    process.exitCode = 1;
  }
}
