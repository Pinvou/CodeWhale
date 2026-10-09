ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('BOOTSTRAP_PENDING', 'PROVISIONING', 'ACTIVE', 'REPAIR_REQUIRED', 'DISABLED', 'DELETED'));

CREATE TABLE super_admin_bootstrap_credentials (
    id UUID PRIMARY KEY,
    deployment_identity TEXT NOT NULL CHECK (char_length(deployment_identity) BETWEEN 3 AND 200),
    deployment_generation BIGINT NOT NULL CHECK (deployment_generation > 0),
    user_id UUID NOT NULL REFERENCES users(id),
    identity_subject TEXT NOT NULL CHECK (char_length(identity_subject) BETWEEN 1 AND 200),
    purpose TEXT NOT NULL DEFAULT 'SUPER_ADMIN_ACTIVATION' CHECK (purpose = 'SUPER_ADMIN_ACTIVATION'),
    mode TEXT NOT NULL CHECK (mode IN ('INTERACTIVE_SET_PASSWORD', 'ONE_TIME_BOOTSTRAP')),
    verifier BYTEA CHECK (verifier IS NULL OR octet_length(verifier) = 32),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'REDEEMING', 'CONSUMED', 'EXPIRED', 'LOCKED', 'SUPERSEDED')),
    failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts BETWEEN 0 AND 5),
    expires_at TIMESTAMPTZ NOT NULL,
    delivery_consumed_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (deployment_identity, deployment_generation),
    CHECK (
        (mode = 'ONE_TIME_BOOTSTRAP' AND (
            (status IN ('PENDING', 'REDEEMING') AND verifier IS NOT NULL)
            OR (status IN ('CONSUMED', 'EXPIRED', 'LOCKED', 'SUPERSEDED') AND verifier IS NULL)
        ))
        OR (mode = 'INTERACTIVE_SET_PASSWORD' AND verifier IS NULL)
    ),
    CHECK ((status = 'CONSUMED') = (consumed_at IS NOT NULL))
);

CREATE UNIQUE INDEX super_admin_bootstrap_pending_idx
    ON super_admin_bootstrap_credentials (deployment_identity)
    WHERE status IN ('PENDING', 'REDEEMING');

CREATE TABLE break_glass_operations (
    id UUID PRIMARY KEY,
    workload_identity TEXT NOT NULL CHECK (char_length(workload_identity) BETWEEN 3 AND 200),
    target_user_id UUID NOT NULL REFERENCES users(id),
    role_assignment_id UUID NOT NULL REFERENCES role_assignments(id),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 20 AND 2000),
    unreachable_evidence_sha256 BYTEA NOT NULL CHECK (octet_length(unreachable_evidence_sha256) = 32),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE security_alerts (
    id UUID PRIMARY KEY,
    severity TEXT NOT NULL CHECK (severity IN ('HIGH', 'CRITICAL')),
    event_type TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    details JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION protect_security_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'security recovery evidence is immutable' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER break_glass_operations_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON break_glass_operations
FOR EACH STATEMENT EXECUTE FUNCTION protect_security_evidence();

CREATE TRIGGER security_alerts_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON security_alerts
FOR EACH STATEMENT EXECUTE FUNCTION protect_security_evidence();

CREATE FUNCTION protect_security_audit_log() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' OR OLD.action LIKE 'security.%' THEN
        RAISE EXCEPTION 'security audit evidence is immutable' USING ERRCODE = '55000';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER audit_logs_security_immutable
BEFORE UPDATE OR DELETE ON audit_logs
FOR EACH ROW EXECUTE FUNCTION protect_security_audit_log();

CREATE TRIGGER audit_logs_security_no_truncate
BEFORE TRUNCATE ON audit_logs
FOR EACH STATEMENT EXECUTE FUNCTION protect_security_audit_log();

CREATE FUNCTION assert_not_last_active_super_admin(target_user UUID) RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM users u
        JOIN role_assignments ra ON ra.user_id=u.id
        WHERE u.id=target_user AND u.status='ACTIVE'
          AND ra.role_code='PLATFORM_SUPER_ADMIN' AND ra.organization_id IS NULL
          AND ra.revoked_at IS NULL AND ra.starts_at<=now()
          AND (ra.expires_at IS NULL OR ra.expires_at>now())
    ) AND NOT EXISTS (
        SELECT 1 FROM users u
        JOIN role_assignments ra ON ra.user_id=u.id
        WHERE u.id<>target_user AND u.status='ACTIVE'
          AND ra.role_code='PLATFORM_SUPER_ADMIN' AND ra.organization_id IS NULL
          AND ra.revoked_at IS NULL AND ra.starts_at<=now()
          AND (ra.expires_at IS NULL OR ra.expires_at>now())
    ) THEN
        RAISE EXCEPTION 'LAST_ACTIVE_SUPER_ADMIN' USING ERRCODE = 'P0001';
    END IF;
END;
$$;

CREATE FUNCTION protect_last_active_super_admin_assignment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.role_code='PLATFORM_SUPER_ADMIN'
       AND OLD.revoked_at IS NULL AND OLD.starts_at<=now()
       AND (OLD.expires_at IS NULL OR OLD.expires_at>now())
       AND (TG_OP='DELETE' OR NEW.revoked_at IS NOT NULL OR NEW.role_code<>'PLATFORM_SUPER_ADMIN' OR NEW.user_id<>OLD.user_id) THEN
        PERFORM assert_not_last_active_super_admin(OLD.user_id);
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER role_assignments_last_super_admin
BEFORE UPDATE OR DELETE ON role_assignments
FOR EACH ROW EXECUTE FUNCTION protect_last_active_super_admin_assignment();

CREATE FUNCTION protect_last_active_super_admin_user() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status='ACTIVE' AND NEW.status<>'ACTIVE' THEN
        PERFORM assert_not_last_active_super_admin(OLD.id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_last_super_admin
BEFORE UPDATE OF status ON users
FOR EACH ROW EXECUTE FUNCTION protect_last_active_super_admin_user();
