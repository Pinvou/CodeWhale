import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { test } from 'node:test';
import YAML from 'yaml';

import { localizedProductDisplayName } from '../../../pinvou3-app/src/features/account-platform/localized-product-display-name.js';

const root = path.resolve(import.meta.dirname, '../..');
const read = (name) => readFile(path.join(root, name), 'utf8');

test('registration bonus consumes mobile eligibility once even while disabled or zero', async () => {
  const [accountMigration, billingMigration, implementation, worker] = await Promise.all([
    read('migrations/account/000087_registration_bonus_fingerprint.up.sql'),
    read('migrations/billing/000079_registration_bonus.up.sql'),
    read('internal/billing/registration_bonus.go'),
    read('internal/account/wallet_provisioning.go'),
  ]);
  assert.match(accountMigration, /registration_bonus_mode IN \('NOT_APPLICABLE','EVALUATE'\)/);
  assert.match(accountMigration, /octet_length\(registration_bonus_fingerprint\) = 32/);
  assert.match(accountMigration, /registration_bonus_eligibility_state/);
  assert.match(accountMigration, /status IN \('PENDING','READY','BLOCKED'\)/);
  assert.match(accountMigration, /fingerprint_key_verifier BYTEA/);
  assert.match(accountMigration, /registration_bonus_mobile_eligibility_immutable/);
  assert.match(billingMigration, /mobile_fingerprint BYTEA NOT NULL UNIQUE/);
  assert.match(billingMigration, /entitlement_package_id UUID REFERENCES entitlement_packages\(id\) DEFERRABLE INITIALLY DEFERRED/);
  assert.match(billingMigration, /NOT_GRANTED_DISABLED/);
  assert.match(billingMigration, /NOT_GRANTED_ZERO/);
  assert.match(billingMigration, /registration_bonus_policy_commands_immutable/);
  assert.match(billingMigration, /registration_bonus_policy_audit_immutable/);
  assert.ok(implementation.indexOf('INSERT INTO registration_bonus_claims') < implementation.indexOf('writeRegistrationBonusGrant'));
  assert.match(implementation, /ON CONFLICT\(mobile_fingerprint\) DO NOTHING/);
  assert.match(implementation, /registration-bonus-policy:"\+actor\.String\(\)\+":"\+idempotencyKey/);
  assert.equal((implementation.match(/IsoLevel: pgx\.ReadCommitted/g) || []).length, 2);
  assert.match(worker, /RegistrationBonusMode == "EVALUATE"/);
  assert.doesNotMatch(worker, /mobile_e164|raw_mobile/i);
});

test('registration bonus admin and internal contracts expose the exact guarded paths', async () => {
  const [adminSource, billingSource, accountSource, service, accountService, authorization, policies, readinessClient] = await Promise.all([
    read('contracts/openapi/admin.yaml'),
    read('contracts/openapi/billing.yaml'),
    read('contracts/openapi/account.yaml'),
    read('internal/billing/service.go'),
    read('internal/account/service.go'),
    read('internal/account/authorization.go'),
    read('internal/platform/approvalpolicy/registry.go'),
    read('internal/billing/registration_bonus_readiness.go'),
  ]);
  const admin = YAML.parse(adminSource);
  const billing = YAML.parse(billingSource);
  const account = YAML.parse(accountSource);
  assert.ok(admin.paths['/billing/admin/v1/registration-bonus-policy']?.get);
  assert.ok(admin.paths['/billing/admin/v1/registration-bonus-policy']?.put);
  assert.ok(billing.paths['/internal/v1/registration-bonus-grants']?.post);
  assert.ok(account.paths['/internal/v1/registration-bonus-eligibility/readiness']?.get);
  assert.match(service, /platform\.registration-bonus\.read/);
  assert.match(service, /platform\.registration-bonus\.manage/);
  assert.match(service, /internal\.billing\.registration-bonus\.provision/);
  assert.match(accountService, /internal\.registration-bonus-eligibility\.read/);
  assert.match(readinessClient, /internal\/v1\/registration-bonus-eligibility\/readiness/);
  assert.match(service, /BILLING_REGISTRATION_BONUS_POLICY_UPDATE.*approvalpolicy\.AuditOnly/);
  assert.match(authorization, /PermissionPlatformRegistrationBonusRead/);
  assert.match(authorization, /PermissionPlatformRegistrationBonusManage/);
  assert.match(policies, /"BILLING_REGISTRATION_BONUS_POLICY_UPDATE":\s+"platform\.registration-bonus\.manage"/);
});

test('registration bonus fingerprint secret is deployment-managed and rotation is fail-closed', async () => {
  const [example, deployment, accountMain] = await Promise.all([
    read('deploy/compose/.env.example'),
    read('docs/deployment.md'),
    read('cmd/account-service/main.go'),
  ]);
  assert.match(example, /REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64=/);
  assert.match(deployment, /当前版本不支持直接轮换/);
  assert.match(accountMain, /must be standard Base64 encoding exactly 32 random bytes/);
});

test('registration bonus entitlement uses one stable identifier and localized current names', async () => {
  const [packages, implementation, rechargeCatalog] = await Promise.all([
    read('internal/billing/packages.go'),
    read('internal/billing/registration_bonus.go'),
    read('internal/billing/recharge_product_catalog.go'),
  ]);
  assert.match(packages, /registrationBonusProductID\s*= "REGISTRATION_BONUS"/);
  assert.match(packages, /registrationBonusDisplayNameZhCN\s*= "新注册赠送积分"/);
  assert.match(packages, /registrationBonusDisplayNameEN\s*= "New-registration bonus points"/);
  assert.match(packages, /registrationBonusDisplayNameJA\s*= "新規登録特典ポイント"/);
  assert.match(packages, /COALESCE\(system_product\.display_name_zh_cn,catalog\.display_name_zh_cn,''\)/);
  assert.match(implementation, /\$9,\$9.*registrationBonusProductID/s);
  assert.match(rechargeCatalog, /strings\.EqualFold\(input\.ProductID, registrationBonusProductID\)/);

  const item = {
    plan_id: 'REGISTRATION_BONUS',
    plan_name: 'REGISTRATION_BONUS',
    current_plan_name_zh_cn: '新注册赠送积分',
    current_plan_name_en: 'New-registration bonus points',
    current_plan_name_ja: '新規登録特典ポイント',
  };
  assert.equal(localizedProductDisplayName(item, 'zh-CN', 'plan_id'), '新注册赠送积分');
  assert.equal(localizedProductDisplayName(item, 'en-US', 'plan_id'), 'New-registration bonus points');
  assert.equal(localizedProductDisplayName(item, 'ja-JP', 'plan_id'), '新規登録特典ポイント');
});
