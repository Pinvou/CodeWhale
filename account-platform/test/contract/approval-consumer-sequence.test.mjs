import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import YAML from 'yaml';

const document = YAML.parse(await readFile(new URL('../../contracts/openapi/account.yaml', import.meta.url), 'utf8'));
const maximum = 18446744073709551615n;
for (const parent of ['ApprovalAuthorizationDeliveryObservation', 'ApprovalAuthorizationDeliveryDLQPubAck']) {
  test(`${parent} declares the exact lossless positive uint64 string domain`, () => {
    const property = document.components.schemas[parent].properties.stream_sequence;
    const validate = new Ajv2020({ strict: false }).compile({ ...property, components: document.components });
    for (const value of ['1', '9007199254740993', '9223372036854775808', maximum.toString()]) {
      assert.equal(validate(value), true, `valid unsigned sequence rejected: ${value}`);
    }
    for (const value of ['0', '01', '+1', '-1', ' 1', '1 ', '1.0', '1e1', '', (maximum + 1n).toString(), '99999999999999999999', 1, null, [], {}]) {
      assert.equal(validate(value), false, `invalid unsigned sequence admitted: ${JSON.stringify(value)}`);
    }
    // Probe every decimal boundary around the upper bound, not just one
    // overflowing example; schema patterns must not leave intermediate gaps.
    for (let exponent = 0n; exponent < 20n; exponent++) {
      const delta = 10n ** exponent;
      for (const value of [maximum - delta, maximum + delta]) {
        assert.equal(validate(value.toString()), value > 0n && value <= maximum, `incorrect boundary ${value}`);
      }
    }
  });
}
