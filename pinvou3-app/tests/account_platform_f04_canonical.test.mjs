import test from 'node:test';
import assert from 'node:assert/strict';
import { highRiskPayloadKey, governedTokenEnvelope } from '../src/features/account-platform/admin-high-risk-action-state.js';

test('governed payload keeps Go JSON HTML and separator escaping at every nesting depth', () => {
  assert.equal(highRiskPayloadKey({ z: ['<>&', '\u2028\u2029'], a: { '<': '&' } }),
    '{"a":{"\\u003c":"\\u0026"},"z":["\\u003c\\u003e\\u0026","\\u2028\\u2029"]}');
});
test('integer-like and non-BMP keys use Go UTF8 lexical order without object enumeration', () => {
  assert.equal(highRiskPayloadKey({ '\u{10000}': 4, '\ue000': 3, 2: 2, 10: 1 }),
    '{"10":1,"2":2,"\ue000":3,"\u{10000}":4}');
});
test('wire replacement, null, arrays and omitted values preserve JSON transport semantics', () => {
  const array = [undefined, undefined, null, true, false];
  delete array[0];
  assert.equal(0 in array, false);
  assert.equal(highRiskPayloadKey({ '\ud800': 'first', '\ud801': '\udc00', omit: undefined, array }),
    '{"array":[null,null,null,true,false],"\ufffd":"\ufffd"}');
});
test('existing ASCII publication fingerprint remains byte-for-byte unchanged', async () => {
  const envelope = await governedTokenEnvelope({ commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL',
    targetResourceID: '018f4f72-9b3a-7abc-8def-1234567890ab', reason: 'publish reviewed immediate draft',
    impact: { precheck_fingerprint: '5'.repeat(64), expected_versions: {
      model_id: '018f4f72-9b3a-7abc-8def-1234567890ab', expected_catalog_version: 4, expected_draft_version: 2,
      draft_fingerprint: '1'.repeat(64), protocol_definition_id: '018f4f72-9b3a-7abc-8def-1234567890ac', protocol_version: 1,
      stable_route_count: 1, stable_route_set_fingerprint: '2'.repeat(64), provisioning_count: 1, ready_route_count: 1,
      provisioning_set_fingerprint: '3'.repeat(64), financial_fact_revision: 7, price_set_fingerprint: '4'.repeat(64)
    } }, command: { model_id: '018f4f72-9b3a-7abc-8def-1234567890ab' } });
  assert.equal(envelope.expected_versions.request_fingerprint, '2f4f93843cf8f35a065298fb6239977ccc2b8e9dacaac88c3a434ac0225f551b');
});
