import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');

test('password reset and profile changes are valid SMS send-attempt purposes', async () => {
  const add = await readFile(
    path.join(root, 'migrations/account/000078_expand_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const validate = await readFile(
    path.join(root, 'migrations/account/000079_validate_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const activate = await readFile(
    path.join(root, 'migrations/account/000080_activate_sms_send_attempt_purposes.up.sql'),
    'utf8',
  );
  const down = await readFile(
    path.join(root, 'migrations/account/000080_activate_sms_send_attempt_purposes.down.sql'),
    'utf8',
  );

  assert.match(add, /ADD CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(add, /SET LOCAL lock_timeout = '2s'/);
  assert.match(add, /NOT VALID/);
  assert.match(
    add,
    /CHECK \(purpose IN \('REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET'\)\)/,
  );
  assert.match(validate, /VALIDATE CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(validate, /SET LOCAL lock_timeout = '2s'/);
  assert.match(activate, /AND convalidated/);
  assert.match(activate, /SET LOCAL lock_timeout = '2s'/);
  assert.match(activate, /DROP CONSTRAINT sms_send_attempts_purpose_check/);
  assert.match(activate, /RENAME CONSTRAINT sms_send_attempts_purpose_v2_check/);
  assert.match(down, /ACCOUNT_SMS_SEND_ATTEMPT_PURPOSES_FORWARD_ONLY/);
  assert.match(down, /ERRCODE = '55000'/);
});
