import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('password change and recovery enforce personal security permission and lifecycle', async () => {
  const [service, authorization, mobileAuth, profiles, migration, contract] = await Promise.all([
    read('internal/account/service.go'),
    read('internal/account/authorization.go'),
    read('internal/account/mobile_auth.go'),
    read('internal/account/profile_changes.go'),
    read('migrations/account/000036_password_security_authorization.up.sql'),
    read('contracts/openapi/account.yaml'),
  ]);

  assert.match(service, /\/v1\/me\/password-changes[^\n]+PermissionPersonalSecurityManage/);
  assert.match(authorization, /case "PROVISIONING", "REPAIR_REQUIRED"[\s\S]+PermissionPersonalSecurityManage/);
  assert.match(mobileAuth, /SMSPurposePasswordReset && exists[\s\S]+AuthorizeMobilePersonalSecurity/);
  assert.match(mobileAuth, /purpose == SMSPurposeLogin \|\| passwordResetAllowed/);
  assert.match(profiles, /u\.status IN \('ACTIVE','PROVISIONING','REPAIR_REQUIRED'\)/);
  assert.match(profiles, /rp\.permission_code=\$3/);
  assert.match(profiles, /PermissionPersonalSecurityManage/);
  assert.match(profiles, /requireRecent\(principal\)/);
  assert.match(profiles, /DELETE FROM user_sessions/);
  assert.match(migration, /method IN \('SMS','AUTHENTICATED'\)/);
  assert.match(contract, /\/me\/password-changes:/);
});
