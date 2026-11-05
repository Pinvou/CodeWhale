WITH revoked_grants AS (
    DELETE FROM role_permissions
    WHERE permission_code='platform.tokens.owner-candidates.read'
      AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')
    RETURNING role_code
), changed_bindings AS (
    SELECT assignment.user_id,
           role_definition.scope AS role_scope,
           assignment.organization_id,
           assignment.unit_id
    FROM role_assignments assignment
    JOIN revoked_grants ON revoked_grants.role_code=assignment.role_code
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

DELETE FROM permissions WHERE code='platform.tokens.owner-candidates.read';

DROP TABLE IF EXISTS token_owner_candidate_query_attempts;
