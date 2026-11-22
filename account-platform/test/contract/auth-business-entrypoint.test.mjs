import assert from 'node:assert/strict';
import test from 'node:test';
import { acceptanceConfiguration } from '../integration/auth-business-configuration.mjs';

const environment = {
  PINVOU_TEST_ACCOUNT_ORIGIN:'http://127.0.0.1:18101', PINVOU_TEST_TOKEN_ORIGIN:'http://127.0.0.1:18103', PINVOU_TEST_PAYMENT_REQUEST_ORIGIN:'http://127.0.0.1:18104',
  PINVOU_TEST_OIDC_TOKEN_URL:'http://127.0.0.1:18080/realms/fixture/protocol/openid-connect/token', PINVOU_TEST_OIDC_CLIENT_ID:'fixture-desktop', PINVOU_TEST_OIDC_AUDIENCE:'fixture-account', PINVOU_TEST_OIDC_USERNAME:'fixture-user', PINVOU_TEST_OIDC_PASSWORD:'fixture-only-not-a-credential', PINVOU_TEST_PRODUCT_ID:'fixture-product', PINVOU_TEST_MODEL_ID:'fixture-model', PINVOU_TEST_RESPONSE_MODEL:'fixture-upstream',
};
test('business acceptance is explicit, complete and restricted to disposable loopback configuration before any network access', () => {
  assert.throws(()=>acceptanceConfiguration(environment,[]), /explicit disposable/);
  assert.throws(()=>acceptanceConfiguration(environment,['--execute-disposable-fixture','--extra']), /explicit disposable/);
  assert.equal(acceptanceConfiguration(environment,['--execute-disposable-fixture']).accountOrigin,'http://127.0.0.1:18101');
  for(const name of Object.keys(environment)){
    const missing={...environment};delete missing[name];
    assert.throws(()=>acceptanceConfiguration(missing,['--execute-disposable-fixture']), /must be supplied/);
  }
  for(const name of ['PINVOU_TEST_ACCOUNT_ORIGIN','PINVOU_TEST_TOKEN_ORIGIN','PINVOU_TEST_PAYMENT_REQUEST_ORIGIN','PINVOU_TEST_OIDC_TOKEN_URL']){
    for(const value of ['https://fixture.example.invalid','http://user:private@127.0.0.1:18101','http://127.0.0.1:18101?secret=x','http://127.0.0.1:18101#x']){
      assert.throws(()=>acceptanceConfiguration({...environment,[name]:value},['--execute-disposable-fixture']), /disposable loopback URL/);
    }
  }
});
