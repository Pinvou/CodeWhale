// Desktop v1 identifiers are frozen independently of released Web v2 IDs.
// Shared enrollment and Web ownership must retain the original identity bytes.
const CONTROLLER_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/u;
const RELAY_SIGNING_KEY_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/u;
const LEGACY_WEB_ENDPOINT_ID = /^[A-Za-z0-9_-]{4,128}$/u;
const MIGRATION_REQUEST_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/u;

export function isControllerIdentifier(value) {
  return typeof value === "string" && CONTROLLER_ID.test(value);
}

// Intersection of desktop wire IDs and native pinned trust-key IDs.
// Configuration fails early without renaming persisted or wire identities.
export function isRelaySigningKeyIdentifier(value) {
  return typeof value === "string" && RELAY_SIGNING_KEY_ID.test(value);
}

export function isLegacyWebEndpointIdentifier(value) {
  return typeof value === "string" && LEGACY_WEB_ENDPOINT_ID.test(value);
}

export function isEnrollmentEndpointIdentifier(value) {
  return isControllerIdentifier(value) || isLegacyWebEndpointIdentifier(value);
}

// Administrative idempotency keys deliberately share the character set, but
// retain their own 8..128 length contract; they are not desktop wire fields.
export function isMigrationRequestIdentifier(value) {
  return typeof value === "string" && MIGRATION_REQUEST_ID.test(value);
}
