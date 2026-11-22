-- Append durable worker ownership without changing any published identity.
-- Stop old Account/profile workers before applying this migration: old binaries
-- do not carry these fences and must not run concurrently with the new writer.
ALTER TABLE registration_operations
 ADD COLUMN recovery_generation bigint NOT NULL DEFAULT 0 CHECK (recovery_generation >= 0),
 ADD COLUMN recovery_lease_until timestamptz;
ALTER TABLE profile_change_requests
 ADD COLUMN recovery_generation bigint NOT NULL DEFAULT 0 CHECK (recovery_generation >= 0),
 ADD COLUMN recovery_lease_until timestamptz,
 ADD COLUMN sessions_revocation_started_at timestamptz;

-- A pre-upgrade committed profile may already have dispatched logout even when
-- Account lacks its ACK. Never manufacture a fresh first attempt from that gap.
-- Rows not committed locally cannot yet have reached the logout stage.
UPDATE profile_change_requests
 SET sessions_revocation_started_at=COALESCE(sessions_revoked_at,account_committed_at)
 WHERE account_committed_at IS NOT NULL;
