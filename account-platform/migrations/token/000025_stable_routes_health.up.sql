ALTER TABLE routing_policies
    ADD COLUMN model_id UUID REFERENCES model_identities(id),
    ADD COLUMN protocol_version_id UUID REFERENCES protocol_definitions(id),
    ADD COLUMN reference_status TEXT NOT NULL DEFAULT 'LEGACY_UNRESOLVED'
        CHECK (reference_status IN ('RESOLVED','LEGACY_UNRESOLVED','DELETED')),
    ADD COLUMN route_version BIGINT NOT NULL DEFAULT 1 CHECK (route_version > 0),
    ADD COLUMN health_version BIGINT NOT NULL DEFAULT 1 CHECK (health_version > 0),
    ADD COLUMN health_observed_at TIMESTAMPTZ,
    ADD COLUMN health_expires_at TIMESTAMPTZ,
    ADD COLUMN health_diagnostic_code TEXT,
    ADD COLUMN health_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (health_failure_count >= 0),
    ADD COLUMN next_probe_at TIMESTAMPTZ,
    ADD COLUMN probe_lease_owner TEXT,
    ADD COLUMN probe_lease_until TIMESTAMPTZ;

-- The original text reference cannot represent two published protocol versions
-- for the same model and also prevents a deleted stable route being recreated.
ALTER TABLE routing_policies
    DROP CONSTRAINT routing_policies_token_account_id_model_key;

ALTER TABLE token_accounts
    ADD COLUMN management_version BIGINT NOT NULL DEFAULT 1 CHECK (management_version > 0);

ALTER TABLE route_provisionings
    ADD COLUMN protocol_version_id UUID REFERENCES protocol_definitions(id);

ALTER TABLE routing_policies DROP CONSTRAINT routing_policies_health_status_check;
ALTER TABLE routing_policies ADD CONSTRAINT routing_policies_health_status_check
    CHECK (health_status IN ('UNKNOWN','PROBING','HEALTHY','DEGRADED','UNHEALTHY','STALE'));

WITH current_catalog AS MATERIALIZED (
    SELECT max(version) version FROM model_catalog_versions
), candidates AS MATERIALIZED (
    SELECT route.id route_id, identity.id model_id, protocol.id protocol_version_id,
           identity.upstream_model_ref, protocol.protocol_code,
           identity.provider_id, identity.adapter_version
      FROM routing_policies route
      JOIN token_accounts account ON account.id=route.token_account_id
      JOIN provider_credentials credential ON credential.token_account_id=account.id
      JOIN model_identities identity
        ON identity.provider_id=credential.provider
       AND identity.upstream_model_ref=route.model
      JOIN current_catalog current ON true
      JOIN model_catalog_entries entry
        ON entry.catalog_version=current.version
       AND entry.model_id=identity.id
       AND entry.lifecycle_status='PUBLISHED'
       AND entry.source_available=true
      JOIN protocol_definitions protocol
        ON protocol.id=entry.protocol_definition_id
       AND protocol.protocol_code=route.protocol_code
       AND protocol.status='PUBLISHED'
      JOIN model_capability_versions capability
        ON capability.id=entry.model_capability_version_id
       AND capability.model_id=identity.id
       AND capability.protocol_definition_id=protocol.id
     WHERE credential.status IN ('ACTIVE','RETIRED')
       AND route.model=ANY(credential.allowed_models)
), resolved AS MATERIALIZED (
    SELECT route_id,
           (array_agg(model_id ORDER BY model_id))[1] model_id,
           (array_agg(protocol_version_id ORDER BY protocol_version_id))[1] protocol_version_id,
           min(upstream_model_ref) upstream_model_ref,
           min(protocol_code) protocol_code
      FROM candidates
     GROUP BY route_id
    HAVING count(DISTINCT (model_id,protocol_version_id,provider_id,adapter_version))=1
)
UPDATE routing_policies route
   SET model_id=resolved.model_id,
       protocol_version_id=resolved.protocol_version_id,
       model=resolved.upstream_model_ref,
       protocol_code=resolved.protocol_code,
       reference_status='RESOLVED',
       health_status='STALE',
       health_diagnostic_code='LEGACY_EVIDENCE_EXPIRED',
       health_observed_at=transaction_timestamp(),
       health_expires_at=NULL,
       next_probe_at=transaction_timestamp()
  FROM resolved
 WHERE route.id=resolved.route_id;

UPDATE routing_policies
   SET health_status='STALE',
       health_diagnostic_code='LEGACY_REFERENCE_UNRESOLVED',
       health_observed_at=transaction_timestamp(),
       health_expires_at=NULL,
       next_probe_at=NULL
 WHERE reference_status='LEGACY_UNRESOLVED';

WITH candidates AS MATERIALIZED (
    SELECT provision.id provisioning_id,route.protocol_version_id
      FROM route_provisionings provision
      JOIN routing_policies route
        ON route.token_account_id=provision.active_token_account_id
       AND route.model_id=provision.model_id
       AND route.protocol_code=provision.protocol_code
       AND route.route_generation=provision.actual_generation
       AND route.reference_status='RESOLVED'
), resolved AS MATERIALIZED (
    SELECT provisioning_id,(array_agg(protocol_version_id ORDER BY protocol_version_id))[1] protocol_version_id
      FROM candidates
     GROUP BY provisioning_id
    HAVING count(DISTINCT protocol_version_id)=1
)
UPDATE route_provisionings provision
   SET protocol_version_id=resolved.protocol_version_id
  FROM resolved
 WHERE provision.id=resolved.provisioning_id;

ALTER TABLE routing_policies ADD CONSTRAINT routing_policy_stable_reference_complete CHECK (
    (reference_status='RESOLVED' AND model_id IS NOT NULL AND protocol_version_id IS NOT NULL)
    OR
    (reference_status='LEGACY_UNRESOLVED' AND model_id IS NULL AND protocol_version_id IS NULL)
    OR
    (reference_status='DELETED' AND model_id IS NOT NULL AND protocol_version_id IS NOT NULL)
);
ALTER TABLE routing_policies ADD CONSTRAINT routing_policy_health_evidence_consistent CHECK (
    reference_status='LEGACY_UNRESOLVED'
    OR
    (health_status='HEALTHY'
        AND health_observed_at IS NOT NULL
        AND health_expires_at IS NOT NULL
        AND health_expires_at > health_observed_at
        AND health_expires_at <= health_observed_at + interval '5 minutes')
    OR
    (health_status<>'HEALTHY' AND health_expires_at IS NULL)
);
ALTER TABLE routing_policies ADD CONSTRAINT routing_policy_probe_lease_consistent CHECK (
    (probe_lease_owner IS NULL) = (probe_lease_until IS NULL)
    AND (health_status<>'PROBING' OR probe_lease_owner IS NOT NULL)
);
ALTER TABLE routing_policies ADD CONSTRAINT routing_policy_health_diagnostic_stable CHECK (
    health_diagnostic_code IS NULL OR health_diagnostic_code ~ '^[A-Z][A-Z0-9_]{0,63}$'
);
CREATE UNIQUE INDEX routing_policy_stable_reference_uniq
    ON routing_policies(token_account_id,model_id,protocol_version_id)
    WHERE reference_status='RESOLVED';
CREATE INDEX routing_policy_health_due_idx
    ON routing_policies(next_probe_at,probe_lease_until,id)
    WHERE reference_status='RESOLVED';

CREATE TABLE route_health_observations (
    id UUID PRIMARY KEY,
    route_id UUID NOT NULL REFERENCES routing_policies(id) ON DELETE CASCADE,
    route_version BIGINT NOT NULL CHECK (route_version > 0),
    health_version BIGINT NOT NULL CHECK (health_version > 0),
    status TEXT NOT NULL CHECK (status IN ('PROBING','HEALTHY','DEGRADED','UNHEALTHY','STALE')),
    diagnostic_code TEXT NOT NULL CHECK (diagnostic_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    worker_id TEXT NOT NULL,
    UNIQUE(route_id,health_version)
);
CREATE INDEX route_health_observation_route_idx
    ON route_health_observations(route_id,observed_at DESC,id DESC);

CREATE TABLE routing_policy_mutations (
    idempotency_key TEXT PRIMARY KEY,
    actor_id UUID NOT NULL,
    command_type TEXT NOT NULL CHECK (command_type IN ('TOKEN_ROUTE_UPSERT','TOKEN_ROUTE_DELETE')),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    route_id UUID NOT NULL,
    result_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at >= created_at + interval '24 hours')
);

CREATE TABLE routing_policy_tombstones (
    route_id UUID PRIMARY KEY,
    token_account_id UUID NOT NULL,
    model_id UUID NOT NULL,
    protocol_version_id UUID NOT NULL,
    route_version BIGINT NOT NULL CHECK (route_version > 0),
    route_generation BIGINT NOT NULL CHECK (route_generation > 0),
    deleted_by UUID NOT NULL,
    reason TEXT NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL
);

CREATE OR REPLACE FUNCTION reject_route_health_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'route health history is immutable';
END $$;
CREATE TRIGGER route_health_observation_immutable
    BEFORE UPDATE OR DELETE ON route_health_observations
    FOR EACH ROW EXECUTE FUNCTION reject_route_health_history_mutation();
CREATE TRIGGER routing_policy_tombstone_immutable
    BEFORE UPDATE OR DELETE ON routing_policy_tombstones
    FOR EACH ROW EXECUTE FUNCTION reject_route_health_history_mutation();
