CREATE INDEX audit_logs_occurred_idx
    ON audit_logs (occurred_at DESC, id DESC);

CREATE INDEX audit_logs_actor_occurred_idx
    ON audit_logs (actor_user_id, occurred_at DESC)
    WHERE actor_user_id IS NOT NULL;

CREATE INDEX audit_logs_result_occurred_idx
    ON audit_logs (result, occurred_at DESC);
