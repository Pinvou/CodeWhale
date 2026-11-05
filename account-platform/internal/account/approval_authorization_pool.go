package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrApprovalWriterUnavailable   = errors.New("approval authorization writer is unavailable")
	ErrApprovalBackfillUnavailable = errors.New("approval authorization backfill is unavailable")
)

const approvalWriterPrepareFunction = "account_approval_authorization.prepare_authorization_event(uuid,uuid,bigint,jsonb,text,bigint,text)"
const approvalWriterFinalizeFunction = "account_approval_authorization.finalize_authorization_event(uuid,text,bytea)"
const approvalWriterOutcomeFunction = "account_approval_authorization.authorization_commit_outcome(uuid,uuid,bigint,text,text,bigint,uuid,uuid)"
const approvalWriterTerminalOutcomeFunction = "account_approval_authorization.approval_terminal_commit_outcome(uuid,bigint,text,uuid,text)"
const approvalBackfillCandidatesFunction = "account_approval_authorization.authorization_backfill_candidates(timestamp with time zone,uuid,timestamp with time zone,uuid,integer)"
const approvalBackfillReadinessFunction = "account_approval_authorization.authorization_backfill_readiness()"
const approvalBackfillOutcomeFunction = "account_approval_authorization.authorization_backfill_outcome(uuid,text,text)"

// WithApprovalWriterReadiness exposes a separate release-gate probe. Ordinary
// liveness/readiness must not evict a service that can still reject or withdraw.
func (r *PostgresRepository) WithApprovalWriterReadiness(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health/ready/approval-writer" {
			next.ServeHTTP(w, request)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.ApprovalWriterReady(request.Context()) != nil {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
}

// ConfigureApprovalWriter is startup-only. The ordinary repository remains usable
// if this independent credential is missing or invalid. No database error or DSN
// escapes this boundary: authentication errors may contain credential material.
func (r *PostgresRepository) ConfigureApprovalWriter(ctx context.Context, dsn string) error {
	if r.approvalWriterPool != nil {
		return ErrApprovalWriterUnavailable
	}
	pool, err := newApprovalAuthorizationPool(ctx, dsn, "account_approval_writer")
	if err != nil {
		return ErrApprovalWriterUnavailable
	}
	r.approvalWriterPool = pool
	if err := r.ApprovalWriterReady(ctx); err != nil {
		// Keep a valid pool so a transient outage or corrected role privileges can
		// recover on the next probe. A malformed configuration never gets a pool.
		return err
	}
	return nil
}

// NewApprovalBackfillPool is for the separate, bounded maintenance process only.
// The long-running Account service never accepts a backfill credential.
func NewApprovalBackfillPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := newApprovalAuthorizationPool(ctx, dsn, "account_approval_backfill")
	if err != nil {
		return nil, ErrApprovalBackfillUnavailable
	}
	if err := ApprovalBackfillReady(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// ApprovalBackfillReady checks the independent maintenance identity without
// taking an approval lock or disclosing database diagnostics.
func ApprovalBackfillReady(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return ErrApprovalBackfillUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var valid bool
	err := pool.QueryRow(ctx, `SELECT session_user='account_approval_backfill' AND current_user=session_user
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control') AND c.relkind IN ('r','p','v','m','f')
              AND pg_catalog.has_any_column_privilege(current_user,c.oid,'SELECT'))
        AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
        AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid)
        AND NOT pg_catalog.has_database_privilege(current_user,current_database(),'CREATE')
        AND NOT EXISTS (
            SELECT 1 FROM unnest(ARRAY['public','account_approval_control','account_approval_authorization']) AS s(name)
            WHERE pg_catalog.has_schema_privilege(current_user,s.name,'CREATE'))
        AND pg_catalog.has_function_privilege(current_user,'account_approval_authorization.backfill_authorization_sidecar(uuid,bytea,text)','EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$3::text::regprocedure,'EXECUTE')
        AND NOT pg_catalog.has_function_privilege(current_user,'public.lock_approval_writer_authorization(uuid,text)','EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$1::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$2::text,'EXECUTE')
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
            WHERE n.nspname='account_approval_authorization'
              AND p.oid NOT IN ('account_approval_authorization.backfill_authorization_sidecar(uuid,bytea,text)'::regprocedure, $1::text::regprocedure, $2::text::regprocedure, $3::text::regprocedure)
              AND pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE'))
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace,
                unnest(ARRAY['INSERT','UPDATE','DELETE','TRUNCATE','TRIGGER','REFERENCES']) AS p(name)
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control')
              AND c.relkind IN ('r','p','v','m','f') AND pg_catalog.has_table_privilege(current_user,c.oid,p.name))
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace,
                unnest(ARRAY['INSERT','UPDATE','REFERENCES']) AS p(name)
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control')
              AND c.relkind IN ('r','p','v','m','f') AND pg_catalog.has_any_column_privilege(current_user,c.oid,p.name))
        FROM pg_catalog.pg_roles r WHERE rolname=session_user`, approvalBackfillCandidatesFunction, approvalBackfillReadinessFunction, approvalBackfillOutcomeFunction).Scan(&valid)
	if err != nil || !valid {
		return ErrApprovalBackfillUnavailable
	}
	return nil
}

func newApprovalAuthorizationPool(ctx context.Context, dsn, role string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, ErrApprovalWriterUnavailable
	}
	configuration, err := pgxpool.ParseConfig(dsn)
	if err != nil || configuration.ConnConfig.User != role {
		return nil, ErrApprovalWriterUnavailable
	}
	configuration.MaxConns = 4
	configuration.MinConns = 0
	configuration.MaxConnLifetime = 30 * time.Minute
	configuration.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, ErrApprovalWriterUnavailable
	}
	return pool, nil
}

// ApprovalWriterReady is independent from ordinary service liveness. It performs
// no writes or row locks and rechecks the actual session identity and privileges.
func (r *PostgresRepository) ApprovalWriterReady(ctx context.Context) error {
	if err := r.approvalWriterReadiness(ctx); err != nil {
		return ErrApprovalWriterUnavailable
	}
	return nil
}

// Keep low-level diagnostics private; public readiness never exposes them.
func (r *PostgresRepository) approvalWriterReadiness(ctx context.Context) error {
	if r.approvalWriterPool == nil || r.approvalWriterPool == r.pool {
		return ErrApprovalWriterUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var valid bool
	err := r.approvalWriterPool.QueryRow(ctx, `SELECT
        session_user='account_approval_writer' AND current_user=session_user
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control') AND c.relkind IN ('r','p','v','m','f')
              AND NOT (n.nspname='public' AND c.relname IN ('identities','users','role_assignments','role_permissions',
                'authorization_versions','approval_command_policies','approval_requests','approval_request_snapshots'))
              AND pg_catalog.has_any_column_privilege(current_user,c.oid,'SELECT'))
        AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
        AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid)
        AND pg_catalog.has_schema_privilege(current_user,'account_approval_authorization','USAGE')
        AND pg_catalog.has_function_privilege(current_user,$1::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$2::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$3::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$4::text,'EXECUTE')
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
            WHERE n.nspname='account_approval_authorization' AND p.oid NOT IN ($1::text::regprocedure,$2::text::regprocedure,$3::text::regprocedure,$4::text::regprocedure)
              AND pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE'))
        AND pg_catalog.has_function_privilege(current_user,'account_approval_control.lock_admin_approval_control_shared()','EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,'public.lock_approval_writer_authorization(uuid,text)','EXECUTE')
        AND NOT pg_catalog.has_function_privilege(current_user,'account_approval_authorization.backfill_authorization_sidecar(uuid,bytea,text)','EXECUTE')
        AND NOT pg_catalog.has_database_privilege(current_user,current_database(),'CREATE')
        AND NOT EXISTS (
            SELECT 1 FROM unnest(ARRAY['public','account_approval_control','account_approval_authorization']) AS s(name)
            WHERE pg_catalog.has_schema_privilege(current_user,s.name,'CREATE'))
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace,
                unnest(ARRAY['INSERT','UPDATE','DELETE','TRUNCATE','TRIGGER','REFERENCES']) AS p(name)
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control')
              AND c.relkind IN ('r','p','v','m','f')
              AND NOT (n.nspname='public' AND c.relname IN ('approval_decisions','approval_state_events') AND p.name='INSERT')
              AND pg_catalog.has_table_privilege(current_user,c.oid,p.name))
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace,
                unnest(ARRAY['INSERT','UPDATE','REFERENCES']) AS p(name)
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control')
              AND c.relkind IN ('r','p','v','m','f')
              AND NOT (n.nspname='public' AND c.relname IN ('approval_decisions','approval_state_events') AND p.name='INSERT')
              AND NOT (n.nspname='public' AND c.relname='approval_requests' AND p.name='UPDATE')
              AND pg_catalog.has_any_column_privilege(current_user,c.oid,p.name))
        AND NOT EXISTS (
            SELECT 1 FROM unnest(ARRAY['identities','users','role_assignments','role_permissions',
                'authorization_versions','approval_command_policies','approval_requests','approval_request_snapshots']) AS t(name)
            WHERE NOT pg_catalog.has_table_privilege(current_user,'public.'||t.name,'SELECT'))
        AND pg_catalog.has_table_privilege(current_user,'public.approval_decisions','INSERT')
        AND pg_catalog.has_table_privilege(current_user,'public.approval_state_events','INSERT')
        AND NOT EXISTS (
            SELECT 1 FROM unnest(ARRAY['approval_requests','approval_decisions','approval_state_events']) AS t(name),
                unnest(ARRAY['DELETE','TRUNCATE','TRIGGER','REFERENCES']) AS p(name)
            WHERE pg_catalog.has_table_privilege(current_user,'public.'||t.name,p.name))
        AND NOT pg_catalog.has_table_privilege(current_user,'public.approval_requests','INSERT')
        AND NOT pg_catalog.has_any_column_privilege(current_user,'public.approval_decisions','UPDATE')
        AND NOT pg_catalog.has_any_column_privilege(current_user,'public.approval_state_events','UPDATE')
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_attribute a
            WHERE a.attrelid='public.approval_requests'::regclass AND a.attnum>0 AND NOT a.attisdropped
                AND a.attname NOT IN ('status','execution_deadline','decided_at','version','updated_at','invalidation_code','invalidation_details')
                AND pg_catalog.has_column_privilege(current_user,a.attrelid,a.attnum,'UPDATE'))
        AND NOT EXISTS (
            SELECT 1 FROM unnest(ARRAY['status','execution_deadline','decided_at','version','updated_at','invalidation_code','invalidation_details']) AS c(name)
            WHERE NOT pg_catalog.has_column_privilege(current_user,'public.approval_requests',c.name,'UPDATE'))
        FROM pg_catalog.pg_roles r WHERE rolname=session_user`, approvalWriterPrepareFunction, approvalWriterFinalizeFunction, approvalWriterOutcomeFunction, approvalWriterTerminalOutcomeFunction).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrApprovalWriterUnavailable
	}
	return nil
}
