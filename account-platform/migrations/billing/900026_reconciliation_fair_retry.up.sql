-- Scheduling metadata only: preserve immutable input and every financial fact.
ALTER TABLE reconciliation_row_progress ADD COLUMN last_attempted_at TIMESTAMPTZ;
CREATE INDEX reconciliation_row_progress_fair_retry_idx
 ON reconciliation_row_progress(run_id,last_attempted_at NULLS FIRST,row_number)
 WHERE processed_at IS NULL;
