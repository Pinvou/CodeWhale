package account

// A unit is usable only while its authoritative parent organization is active.
// The raw unit tombstone takes precedence; its database version remains the
// authority version. Parent transitions must never fabricate a higher version.
const tokenOwnerUnitStatusSQL = "CASE WHEN unit.status='DELETED' THEN 'DELETED' WHEN organization.status='ACTIVE' THEN unit.status ELSE 'DISABLED' END"
