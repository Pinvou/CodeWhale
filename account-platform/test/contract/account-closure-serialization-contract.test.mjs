import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { parse } from 'yaml';

const contract = parse(readFileSync(new URL('../../contracts/openapi/account-closure.yaml', import.meta.url), 'utf8'));

test('closure create documents both 409 codes and the original-key recovery boundary', () => {
  assert.equal(contract.openapi, '3.1.0');
  const response = contract.paths['/v1/me/account-closure-requests'].post.responses['409'];
  assert.equal(response.$ref, '#/components/responses/Error');
  assert.equal(typeof response.description, 'string');
  const serialization = response.description.split('ACCOUNT_CLOSURE_SERIALIZATION_CONFLICT:')[1]?.split('ACCOUNT_CONFLICT:')[0];
  assert.ok(serialization, 'the serialization code must have its own recovery description');
  for (const phrase of ['same principal', 'same idempotency key', 'unchanged confirmation']) {
    assert.ok(serialization.includes(phrase), `serialization recovery must retain ${phrase}`);
  }
  assert.match(serialization, /not an instruction to change the key\s+or to automatically retry an unknown result/i,
    'serialization guidance must exclude changing the key or automatically retrying an unknown result');
  const domain = response.description.split('ACCOUNT_CONFLICT:')[1];
  assert.ok(domain?.includes('existing account state or key/facts binding'));
  assert.ok(domain.includes('read the existing closure'));
  assert.ok(domain.includes('Changing the key does not establish that an earlier request had no effect'));
  assert.deepEqual(contract.components.responses.Error.content['application/json'].schema.required, ['code', 'message']);
});
