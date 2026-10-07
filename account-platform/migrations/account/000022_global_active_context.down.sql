UPDATE user_sessions session
SET active_context_type=context.context_type,
    active_organization_id=context.organization_id,
    membership_version=GREATEST(session.membership_version,context.context_version),
    last_seen_at=GREATEST(session.last_seen_at,context.updated_at)
FROM user_active_contexts context
WHERE context.user_id=session.user_id;

DROP TRIGGER user_active_contexts_identity_guard ON user_active_contexts;
DROP FUNCTION protect_user_active_context_identity();
DROP TABLE user_active_contexts;
