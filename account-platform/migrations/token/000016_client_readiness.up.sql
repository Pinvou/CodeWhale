CREATE TABLE client_readiness_attestations (
    id UUID PRIMARY KEY,
    environment TEXT NOT NULL CHECK (char_length(environment) BETWEEN 1 AND 64),
    artifact_sha256 TEXT NOT NULL CHECK (artifact_sha256 ~ '^[0-9a-f]{64}$'),
    client_version TEXT NOT NULL CHECK (char_length(client_version) BETWEEN 1 AND 64),
    service_version TEXT NOT NULL,
    contract_version TEXT NOT NULL,
    account_contract_version TEXT NOT NULL,
    catalog_version BIGINT NOT NULL CHECK (catalog_version>0),
    protocol_code TEXT NOT NULL,
    check_snapshot JSONB NOT NULL,
    claims_sha256 TEXT NOT NULL UNIQUE CHECK (claims_sha256 ~ '^[0-9a-f]{64}$'),
    key_id TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at>issued_at AND expires_at<=issued_at+interval '15 minutes')
);

CREATE INDEX client_readiness_attestations_artifact_idx
    ON client_readiness_attestations(environment,artifact_sha256,client_version,issued_at DESC);

CREATE FUNCTION protect_client_readiness_attestations() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'client readiness attestations are immutable';
END $$;
CREATE TRIGGER client_readiness_attestations_immutable
BEFORE UPDATE OR DELETE ON client_readiness_attestations
FOR EACH ROW EXECUTE FUNCTION protect_client_readiness_attestations();
