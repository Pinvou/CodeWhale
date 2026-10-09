CREATE TABLE user_active_contexts (
    user_id UUID PRIMARY KEY REFERENCES users(id),
    context_type TEXT NOT NULL CHECK (context_type IN ('PERSONAL','ORGANIZATION')),
    organization_id UUID REFERENCES organizations(id),
    context_version BIGINT NOT NULL CHECK (context_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((context_type='PERSONAL')=(organization_id IS NULL))
);

INSERT INTO user_active_contexts(user_id,context_type,organization_id,context_version,updated_at)
SELECT DISTINCT ON (account_user.id)
       account_user.id,
       CASE WHEN switched.context_type='ORGANIZATION'
                  AND organization.status='ACTIVE' AND membership.status='ACTIVE'
            THEN 'ORGANIZATION' ELSE 'PERSONAL' END,
       CASE WHEN switched.context_type='ORGANIZATION'
                  AND organization.status='ACTIVE' AND membership.status='ACTIVE'
            THEN switched.organization_id ELSE NULL END,
       GREATEST(account_user.membership_version,COALESCE(session_projection.context_version,0),COALESCE(switched.context_version,0),1),
       COALESCE(switched.occurred_at,account_user.updated_at)
FROM users account_user
LEFT JOIN LATERAL (
    SELECT change_summary->>'context_type' AS context_type,
           active_organization_id AS organization_id,
           CASE WHEN change_summary->>'context_version' ~ '^[0-9]+$'
                THEN (change_summary->>'context_version')::BIGINT END AS context_version,
           occurred_at
    FROM audit_logs
    WHERE actor_user_id=account_user.id
      AND action IN ('session.active_context.set','user.active_context.set')
      AND result='SUCCEEDED'
    ORDER BY occurred_at DESC,id DESC
    LIMIT 1
) switched ON true
LEFT JOIN LATERAL (
    SELECT MAX(membership_version) AS context_version
    FROM user_sessions
    WHERE user_id=account_user.id
) session_projection ON true
LEFT JOIN organizations organization ON organization.id=switched.organization_id
LEFT JOIN memberships membership ON membership.user_id=account_user.id AND membership.organization_id=switched.organization_id
ORDER BY account_user.id;

CREATE INDEX user_active_contexts_organization_idx
    ON user_active_contexts(organization_id,user_id)
    WHERE organization_id IS NOT NULL;

CREATE FUNCTION protect_user_active_context_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.user_id IS DISTINCT FROM NEW.user_id OR NEW.context_version<=OLD.context_version THEN
        RAISE EXCEPTION 'active context identity is immutable and version must increase';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER user_active_contexts_identity_guard
BEFORE UPDATE ON user_active_contexts
FOR EACH ROW EXECUTE FUNCTION protect_user_active_context_identity();
