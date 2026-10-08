DELETE FROM role_permissions
WHERE role_code='PLATFORM_SUPER_ADMIN' AND permission_code='platform.audit.pii.export';

INSERT INTO roles(code,scope,assignable,description) VALUES
('PLATFORM_AUDIT_PII_EXPORT','PLATFORM',TRUE,'Temporary, expiring authorization for unredacted audit PII export.');
INSERT INTO role_permissions(role_code,permission_code) VALUES
('PLATFORM_AUDIT_PII_EXPORT','platform.audit.pii.export');

INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at) VALUES
('ACCOUNT_AUDIT_EXPORT_CREATE',1,'AUDIT_ONLY','platform.audit.export','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET',1,'AUDIT_ONLY','platform.audit.export','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now());

CREATE TABLE audit_export_jobs (
    id UUID PRIMARY KEY,
    actor_user_id UUID NOT NULL REFERENCES users(id),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    filter_snapshot JSONB NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 10 AND 500),
    include_pii BOOLEAN NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING','GENERATING','READY','FAILED','EXPIRED','DOWNLOADED')),
    wrapped_file_key_nonce BYTEA,
    wrapped_file_key BYTEA,
    file_payload BYTEA,
    file_signature TEXT CHECK (file_signature IS NULL OR file_signature ~ '^[0-9a-f]{64}$'),
    filename TEXT,
    row_count INTEGER CHECK (row_count IS NULL OR row_count BETWEEN 0 AND 100000),
    size_bytes BIGINT CHECK (size_bytes IS NULL OR size_bytes BETWEEN 0 AND 524288000),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts>=0),
    lease_until TIMESTAMPTZ,
    download_token_digest TEXT CHECK (download_token_digest IS NULL OR download_token_digest ~ '^[0-9a-f]{64}$'),
    download_ticket_issued_at TIMESTAMPTZ,
    download_expires_at TIMESTAMPTZ,
    downloaded_at TIMESTAMPTZ,
    file_expires_at TIMESTAMPTZ,
    error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE(actor_user_id,idempotency_key_digest)
);
CREATE INDEX audit_export_jobs_work_idx ON audit_export_jobs(status,lease_until,created_at,id)
WHERE status IN ('PENDING','GENERATING','READY');
CREATE INDEX audit_export_jobs_actor_quota_idx ON audit_export_jobs(actor_user_id,created_at,status);

CREATE TABLE audit_export_events (
    id UUID PRIMARY KEY,
    export_id UUID NOT NULL REFERENCES audit_export_jobs(id),
    event_type TEXT NOT NULL CHECK (event_type IN ('CREATED','READY','FAILED','DOWNLOAD_TICKET_ISSUED','DOWNLOAD_FAILED','DOWNLOADED','EXPIRED')),
    actor_user_id UUID,
    snapshot JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX audit_export_events_timeline_idx ON audit_export_events(export_id,occurred_at,id);

CREATE FUNCTION reject_audit_export_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit export evidence is immutable';
END $$;
CREATE TRIGGER audit_export_events_immutable BEFORE UPDATE OR DELETE ON audit_export_events
FOR EACH ROW EXECUTE FUNCTION reject_audit_export_event_mutation();
