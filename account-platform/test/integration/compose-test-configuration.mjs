import { createPrivateKey, createPublicKey, randomBytes } from 'node:crypto';

const keyIDPattern = /^[a-z][a-z0-9._:-]{1,127}$/;
function required(environment, name) {
  const value = environment[name];
  if (typeof value !== 'string' || !value.trim() || value.length > 65536 || /[\r\n\0]/.test(value)) {
    throw new Error(`COMPOSE_TEST_CONFIGURATION_REQUIRED:${name}`);
  }
  return value.trim();
}
function rawURLKey(value, sizes) {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) throw new Error('COMPOSE_TEST_DELEGATION_INVALID');
  const bytes = Buffer.from(value, 'base64url');
  if (!sizes.includes(bytes.length) || bytes.toString('base64url') !== value) throw new Error('COMPOSE_TEST_DELEGATION_INVALID');
  return bytes;
}

// Existing services remain the authority. These checks only reject incomplete
// test configuration before Docker; they do not mint workload/user credentials.
export function composeTestConfiguration(environment = process.env) {
  const keyID = required(environment, 'HUMAN_DELEGATION_SIGNING_KEY_ID');
  const signing = required(environment, 'HUMAN_DELEGATION_SIGNING_KEY_BASE64URL');
  const verification = required(environment, 'HUMAN_DELEGATION_VERIFICATION_KEYS_JSON');
  if (!keyIDPattern.test(keyID)) throw new Error('COMPOSE_TEST_DELEGATION_INVALID');
  const signingBytes = rawURLKey(signing, [32, 64]);
  let keys;
  try { keys = JSON.parse(verification); } catch { throw new Error('COMPOSE_TEST_DELEGATION_INVALID'); }
  if (!keys || Array.isArray(keys) || typeof keys !== 'object' || !Object.keys(keys).length) throw new Error('COMPOSE_TEST_DELEGATION_INVALID');
  for (const [id, value] of Object.entries(keys)) {
    if (!keyIDPattern.test(id) || typeof value !== 'string') throw new Error('COMPOSE_TEST_DELEGATION_INVALID');
    rawURLKey(value, [32]);
  }
  const privateKey = createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), signingBytes.subarray(0, 32)]), format: 'der', type: 'pkcs8' });
  const publicBytes = createPublicKey(privateKey).export({ format: 'der', type: 'spki' }).subarray(-32);
  if (keys[keyID] !== publicBytes.toString('base64url') || (signingBytes.length === 64 && !signingBytes.subarray(32).equals(publicBytes))) throw new Error('COMPOSE_TEST_DELEGATION_INVALID');

  const providerURL = required(environment, 'TOKEN_DEFAULT_PROVIDER_BASE_URL');
  const providerKey = required(environment, 'TOKEN_DEFAULT_PROVIDER_API_KEY');
  const adminToken = required(environment, 'TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN');
  const adminUser = required(environment, 'TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID');
  let url;
  try { url = new URL(providerURL); } catch { throw new Error('COMPOSE_TEST_PROVIDER_CONFIGURATION_INVALID'); }
  // mock-newapi has no administration API. Do not label its random mock key
  // as an administrator token. The existing real-newapi profile and operator
  // bootstrapped provider credentials are prerequisites, not tested deployment.
  if (url.protocol !== 'http:' || url.hostname !== 'newapi' || url.port !== '3000' || url.username || url.password || url.search || url.hash || !['', '/'].includes(url.pathname)) throw new Error('COMPOSE_TEST_REAL_NEWAPI_PROFILE_REQUIRED');
  if (!/^[1-9][0-9]*$/.test(adminUser) || BigInt(adminUser) > 9223372036854775807n) throw new Error('COMPOSE_TEST_PROVIDER_CONFIGURATION_INVALID');
  return {
    OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64: randomBytes(32).toString('base64'),
    // Compose consumes the same variable for Token verification and Portal signing.
    TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64: randomBytes(32).toString('base64'),
    HUMAN_DELEGATION_SIGNING_KEY_ID: keyID,
    HUMAN_DELEGATION_SIGNING_KEY_BASE64URL: signing,
    HUMAN_DELEGATION_VERIFICATION_KEYS_JSON: verification,
    TOKEN_DEFAULT_PROVIDER_BASE_URL: providerURL,
    TOKEN_DEFAULT_PROVIDER_API_KEY: providerKey,
    TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN: adminToken,
    TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID: adminUser,
  };
}
