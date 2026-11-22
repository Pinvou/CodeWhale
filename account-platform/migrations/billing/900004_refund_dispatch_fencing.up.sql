-- Historical pending refunds may have reached the provider; never replay them
-- merely because their former process lost the response. No financial fact changes.
ALTER TABLE refunds
 ADD COLUMN dispatch_state TEXT NOT NULL DEFAULT 'RESULT_UNKNOWN'
   CHECK(dispatch_state IN ('NOT_STARTED','IN_PROGRESS','RESULT_UNKNOWN','COMPLETED')),
 ADD COLUMN dispatch_started_at TIMESTAMPTZ,
 ADD COLUMN dispatch_lease_until TIMESTAMPTZ;
UPDATE refunds SET dispatch_state='COMPLETED' WHERE status IN ('SUCCEEDED','FAILED');
ALTER TABLE refunds ALTER COLUMN dispatch_state SET DEFAULT 'NOT_STARTED';
CREATE INDEX refunds_dispatch_expired_idx ON refunds(dispatch_lease_until,id)
 WHERE dispatch_state='IN_PROGRESS';
