import assert from 'node:assert/strict';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { nativeKmsConfiguration, prepareNativeRotation, verifyNativeRotation } from './account-platform-native-kms-gate.mjs';

// Only a disposable OpenBao fixture supplies this runner's environment.
// Never prints configuration, proof, tokens, DSN or raw exceptions.
try {
  const configuration = nativeKmsConfiguration(process.env);
  const action = process.argv[2];
  assert.match(configuration.proofFile, /^\/tmp\/pinvou-openbao-fixture-[A-Za-z0-9_-]+\/gate-state\/proof$/);
  if (action === 'prepare') assert.equal((await prepareNativeRotation(configuration)).decision, 'PREPARED_NOT_GO');
  else if (action === 'verify') {
    const result = await verifyNativeRotation(configuration);
    assert.equal(result.previous_version, 1);
    assert.equal(result.active_version, 2);
    assert.equal(result.foreign_key_ciphertexts, 0);
  } else if (action === 'same-version') await assert.rejects(verifyNativeRotation(configuration), /rotation_not_observed/);
  else if (action === 'foreign') await assert.rejects(verifyNativeRotation(configuration), /database_changed_or_legacy/);
  else if (action === 'nonfresh') await assert.rejects(prepareNativeRotation(configuration), /database_not_fresh/);
  else if (action === 'rls') await assert.rejects(verifyNativeRotation(configuration), /database_query_failed/);
  else if (action === 'extra-policy') await assert.rejects(verifyNativeRotation(configuration), /identity_or_lease_invalid/);
  else if (action === 'wrong-ca') {
    configuration.caFile = path.join(path.dirname(configuration.proofFile), 'wrong-ca.crt');
    configuration.proofFile += '-rejected';
    await assert.rejects(fs.stat(configuration.proofFile), { code: 'ENOENT' });
    await assert.rejects(prepareNativeRotation(configuration), /request_failed/);
    await assert.rejects(fs.stat(configuration.proofFile), { code: 'ENOENT' });
  }
  else throw new Error('action');
  process.stdout.write(`NATIVE_FIXTURE_${action.toUpperCase()}_PASS\n`);
} catch {
  process.stderr.write('NATIVE_FIXTURE_FAILED\n');
  process.exitCode = 1;
}
