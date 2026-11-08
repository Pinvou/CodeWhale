-- Completed scheduled observations remain immutable; changed internal facts create a new run.
DROP INDEX reconciliation_runs_input_once_idx;
CREATE UNIQUE INDEX reconciliation_runs_input_once_idx ON reconciliation_runs(provider_id,provider_version,channel,business_date,source_mode,input_digest) WHERE source_mode='FILE_IMPORT' AND input_digest IS NOT NULL;
CREATE INDEX reconciliation_runs_scheduled_digest_idx ON reconciliation_runs(provider_id,provider_version,channel,business_date,input_digest,started_at DESC,id DESC) WHERE source_mode='SCHEDULED_API';

-- Store the same selected binding evidence used by the immutable observation digest.
ALTER TABLE reconciliation_observations ADD COLUMN source_bindings JSONB;
