CREATE TABLE simulator_audit_logs (
    id UUID PRIMARY KEY,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    scenario_id TEXT,
    request_id TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX simulator_audit_logs_occurred_idx
    ON simulator_audit_logs (occurred_at DESC);
