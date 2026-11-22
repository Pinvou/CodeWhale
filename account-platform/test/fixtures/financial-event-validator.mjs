import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

export const financialEventTypes = Object.freeze([
  'account.wallet.provisioning.completed.v1',
  'account.wallet.provisioning.requested.v1',
  'billing.late-payment.review-required.v1',
  'billing.manual-review.sla-escalated.v1',
  'billing.payment-anomaly.review-required.v1',
  'billing.provider-blind.detected.v1',
  'billing.recharge.succeeded.v1',
]);
const ajv = new Ajv2020({ allErrors: true, strict: true, allowUnionTypes: true });
addFormats(ajv);
const validators = new Map();
for (const name of financialEventTypes) {
  const schema = JSON.parse(await readFile(new URL(`../../contracts/events/${name}.schema.json`, import.meta.url), 'utf8'));
  validators.set(name, ajv.compile(schema));
}

// This is a test-only consumer. It neither connects to a broker nor publishes,
// invokes a domain writer, or returns input data in diagnostics.
export function validateFinancialEvent(value) {
  return validators.get(value?.event_type)?.(value) === true;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    let body = '';
    for await (const part of process.stdin) {
      body += part;
      if (Buffer.byteLength(body, 'utf8') > 65536) throw new Error('limit');
    }
    if (!validateFinancialEvent(JSON.parse(body))) throw new Error('invalid');
    process.stdout.write('VALID\n');
  } catch {
    process.stderr.write('INVALID_FINANCIAL_EVENT\n');
    process.exitCode = 1;
  }
}
