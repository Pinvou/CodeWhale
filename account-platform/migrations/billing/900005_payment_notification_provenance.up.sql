-- Legacy callback facts lack trustworthy verifier provenance. Preserve them
-- without inventing a provider; only newly authenticated ingest binds identity.
ALTER TABLE payment_notifications
 ADD COLUMN provider_id TEXT,
 ADD COLUMN provider_version TEXT;
