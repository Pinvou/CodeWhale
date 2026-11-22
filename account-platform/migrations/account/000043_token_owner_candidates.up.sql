WITH existing_permission AS MATERIALIZED (
    SELECT scope FROM permissions
    WHERE code='platform.tokens.owner-candidates.read'
), repaired_permission AS (
    INSERT INTO permissions(code,scope,description) VALUES
    ('platform.tokens.owner-candidates.read','PLATFORM','Read the minimal user, organization, and unit projection used to select a Token Account owner.')
    ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description
    RETURNING code
), scope_repair_roles AS MATERIALIZED (
    SELECT role_code FROM role_permissions
    WHERE permission_code='platform.tokens.owner-candidates.read'
      AND EXISTS (SELECT 1 FROM existing_permission WHERE scope<>'PLATFORM')
), revoked_roles AS (
    DELETE FROM role_permissions
    WHERE permission_code='platform.tokens.owner-candidates.read'
      AND role_code NOT IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')
    RETURNING role_code
), inserted_grants AS (
    INSERT INTO role_permissions(role_code,permission_code)
    SELECT approved_role.role_code,repaired_permission.code
    FROM (VALUES ('PLATFORM_SUPER_ADMIN'),('PLATFORM_MODEL_ADMIN')) AS approved_role(role_code)
    CROSS JOIN repaired_permission
    ON CONFLICT DO NOTHING
    RETURNING role_code
), changed_roles AS (
    SELECT role_code FROM scope_repair_roles
    UNION SELECT role_code FROM revoked_roles
    UNION SELECT role_code FROM inserted_grants
), changed_bindings AS (
    SELECT assignment.user_id,
           role_definition.scope AS role_scope,
           assignment.organization_id,
           assignment.unit_id
    FROM role_assignments assignment
    JOIN changed_roles ON changed_roles.role_code=assignment.role_code
    JOIN roles role_definition ON role_definition.code=assignment.role_code
    WHERE assignment.revoked_at IS NULL
      AND assignment.starts_at<=now()
      AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
), affected_scopes AS (
    SELECT user_id,'PERSONAL'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM changed_bindings WHERE role_scope='PERSONAL'
    UNION
    SELECT user_id,'PLATFORM',
           '00000000-0000-0000-0000-000000000000'::uuid
    FROM changed_bindings WHERE role_scope='PLATFORM'
    UNION
    SELECT user_id,'ORGANIZATION',organization_id
    FROM changed_bindings
    WHERE role_scope IN ('ORGANIZATION','UNIT') AND organization_id IS NOT NULL
    UNION
    SELECT user_id,'UNIT',unit_id
    FROM changed_bindings
    WHERE role_scope='UNIT' AND unit_id IS NOT NULL
)
SELECT increment_authorization_scope_version(user_id,scope,scope_id)
FROM affected_scopes;

-- The migration owns the permission object and exactly two grant edges. A
-- pre-existing permission is repaired in place; pre-existing grants on any
-- other role are revoked above and invalidate only their effective assignees.
-- Re-running this file therefore changes no edge and bumps no version.

CREATE TABLE IF NOT EXISTS token_owner_candidate_query_attempts (
    id UUID PRIMARY KEY,
    actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    caller_service TEXT NOT NULL CHECK (caller_service=btrim(caller_service) AND length(caller_service) BETWEEN 1 AND 128),
    caller_subject TEXT NOT NULL CHECK (caller_subject=btrim(caller_subject) AND length(caller_subject) BETWEEN 1 AND 512),
    authorization_version BIGINT NOT NULL CHECK (authorization_version>0),
    filter_fingerprint TEXT NOT NULL CHECK (length(filter_fingerprint)=64),
    high_water TIMESTAMPTZ NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('STARTED','COMPLETED','INVALID_CURSOR','RATE_LIMITED')),
    occurred_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at>occurred_at),
    completed_at TIMESTAMPTZ,
    CHECK ((result='STARTED')=(completed_at IS NULL))
);

CREATE INDEX IF NOT EXISTS token_owner_candidate_attempt_actor_window_idx
    ON token_owner_candidate_query_attempts(actor_user_id,occurred_at DESC);
CREATE INDEX IF NOT EXISTS token_owner_candidate_attempt_service_window_idx
    ON token_owner_candidate_query_attempts(caller_service,occurred_at DESC);
CREATE INDEX IF NOT EXISTS token_owner_candidate_active_scan_idx
    ON token_owner_candidate_query_attempts(actor_user_id,caller_service,expires_at)
    WHERE completed_at IS NULL;
