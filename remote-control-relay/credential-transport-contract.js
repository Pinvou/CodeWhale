// Legacy Web credential transport counts JavaScript UTF-16 code units.
// New Enrollment writes intersect this limit with their existing byte and
// well-formed-text checks; historical Enrollment authorization stays byte-based.
export const CREDENTIAL_TRANSPORT_MAX_UTF16_UNITS = 1024;
