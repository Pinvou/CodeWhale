package account

import (
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type auditExportBoundaryWallet struct{}

func (auditExportBoundaryWallet) EnsurePersonalWallet(context.Context, uuid.UUID) error { return nil }
func auditExportBoundaryErrorClass(err error) string {
	if err == nil {
		return "nil"
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		return "PG_" + p.Code
	}
	if errors.Is(err, ErrConflict) {
		return "CONFLICT"
	}
	if errors.Is(err, ErrNotFound) {
		return "NOT_FOUND"
	}
	return fmt.Sprintf("%T", err)
}

// This test uses an exclusively owned database and deployment actors. Wallet ACK is controlled; Billing ledger and admin permission middleware are outside this boundary test.
func TestAuditExportPersistedBoundaries(t *testing.T) {
	if os.Getenv("F17_TEST_DATABASE_URL") == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	f := newAuditExportBoundaryFixture(t, ctx, os.Getenv("F17_TEST_DATABASE_URL"))
	c := f.database(t, ctx, "account", "account_service")
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("SETUP_LOAD")
	}
	if err = accountschema.Up(ctx, c, loaded); err != nil {
		t.Fatalf("SETUP_UP_%s", auditExportBoundaryErrorClass(err))
	}
	if _, verifyErr := migrations.VerifyApplied(ctx, c, loaded); verifyErr != nil {
		t.Fatal("SETUP_VERIFY")
	}
	migrations.Unlock(ctx, c)
	repo, err := NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("SETUP_REPO")
	}
	t.Cleanup(repo.Close)
	principal := identity.Principal{Issuer: "https://test.invalid/realm", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
	me, _, err := repo.Bootstrap(ctx, principal)
	if err != nil {
		t.Fatalf("SETUP_BOOTSTRAP_%s", auditExportBoundaryErrorClass(err))
	}
	_ = me
	if err = NewWalletProvisioningWorker(repo, auditExportBoundaryWallet{}, time.Second).RunOnce(ctx); err != nil {
		t.Fatalf("SETUP_WALLET_%s", auditExportBoundaryErrorClass(err))
	}
	exports, err := NewAuditExportService(repo, make([]byte, 32), "test")
	if err != nil {
		t.Fatal("SETUP_EXPORT")
	}
	create := func(reason string) AuditExportJob {
		t.Helper()
		now := time.Now().UTC()
		j, e := exports.Create(ctx, principal, AuditExportRequest{Reason: reason, IdempotencyKey: uuid.NewString(), Filter: AuditExportFilter{From: now.Add(-time.Minute), To: now}})
		if e != nil {
			t.Fatalf("SETUP_CREATE_%s", auditExportBoundaryErrorClass(e))
		}
		return j
	}
	ready := func() AuditExportJob {
		t.Helper()
		j := create("public test reason")
		claim, ok, e := exports.claim(ctx)
		if e != nil || !ok || claim.ID != j.ID {
			t.Fatal("SETUP_CLAIM")
		}
		if e = exports.generate(ctx, claim); e != nil {
			t.Fatalf("SETUP_GENERATE_%s", auditExportBoundaryErrorClass(e))
		}
		return j
	}
	t.Run("pending_ticket_nullable", func(t *testing.T) {
		j := create("public test reason")
		_, e := exports.IssueDownloadTicket(ctx, principal, j.ID)
		if !errors.Is(e, ErrConflict) {
			t.Errorf("EXPECTED_CONFLICT_ACTUAL_%s", auditExportBoundaryErrorClass(e))
		}
		_, e = repo.pool.Exec(ctx, `UPDATE audit_export_jobs SET status='FAILED' WHERE id=$1`, j.ID)
		if e != nil {
			t.Fatal("FIXTURE_RETIRE")
		}
	})
	t.Run("ready_without_ticket_nullable", func(t *testing.T) {
		j := ready()
		_, e := exports.Download(ctx, principal, j.ID, strings.Repeat("x", 43))
		if !errors.Is(e, ErrNotFound) {
			t.Errorf("EXPECTED_NOT_FOUND_ACTUAL_%s", auditExportBoundaryErrorClass(e))
		}
	})
	t.Run("reason_characters", func(t *testing.T) {
		now := time.Now().UTC()
		_, e := exports.Create(ctx, principal, AuditExportRequest{Reason: "中文五字啊", IdempotencyKey: uuid.NewString(), Filter: AuditExportFilter{From: now.Add(-time.Minute), To: now}})
		if !errors.Is(e, ErrInvalidAuditExportRequest) {
			t.Errorf("EXPECTED_VALIDATION_ACTUAL_%s", auditExportBoundaryErrorClass(e))
		}
	})
	t.Run("ticket_lock_crosses_expiry", func(t *testing.T) {
		j := ready()
		lock, e := repo.pool.Begin(ctx)
		if e != nil {
			t.Fatal("FIXTURE_LOCK")
		}
		defer lock.Rollback(ctx)
		var expiry time.Time
		if e = lock.QueryRow(ctx, `UPDATE audit_export_jobs SET file_expires_at=clock_timestamp()+interval '500 milliseconds' WHERE id=$1 RETURNING file_expires_at`, j.ID).Scan(&expiry); e != nil {
			t.Fatal("FIXTURE_EXPIRY")
		}
		type response struct {
			ticket AuditExportDownloadTicket
			err    error
		}
		ch := make(chan response, 1)
		go func() { v, e := exports.IssueDownloadTicket(ctx, principal, j.ID); ch <- response{v, e} }()
		deadline := time.Now().Add(3 * time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			var n int
			if e = f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE '%file_expires_at%'`, c.Config().Database).Scan(&n); e != nil {
				t.Fatal("FIXTURE_WAIT")
			}
			if n > 0 {
				blocked = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("FIXTURE_NO_REAL_LOCKWAIT")
		}
		time.Sleep(time.Until(expiry) + 50*time.Millisecond)
		if e = lock.Commit(ctx); e != nil {
			t.Fatal("FIXTURE_UNLOCK")
		}
		v := <-ch
		if !errors.Is(v.err, ErrConflict) {
			t.Errorf("EXPECTED_EXPIRED_CONFLICT_ACTUAL_%s", auditExportBoundaryErrorClass(v.err))
		}
	})
	t.Run("http_head_preserves_single_use", func(t *testing.T) {
		j := ready()
		ticket, e := exports.IssueDownloadTicket(ctx, principal, j.ID)
		if e != nil {
			t.Fatal("FIXTURE_TICKET")
		}
		svc := NewService(repo, nil).WithAuditExports(exports)
		router := httpserver.NewRouter()
		router.Use(identity.GinMiddleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }))
		router.GET("/admin/v1/audit-exports/:export_id/download", httpserver.AdaptFunc(svc.downloadAuditExport))
		server := httptest.NewServer(router)
		defer server.Close()
		call := func(method string) int {
			r, _ := http.NewRequest(method, server.URL+"/admin/v1/audit-exports/"+j.ID.String()+"/download", nil)
			r.Header.Set("Authorization", "Bearer public-synthetic-test")
			r.Header.Set("X-Pinvou-Download-Token", ticket.Token)
			v, e := server.Client().Do(r)
			if e != nil {
				t.Fatal("FIXTURE_HTTP")
			}
			defer v.Body.Close()
			return v.StatusCode
		}
		head := call(http.MethodHead)
		get := call(http.MethodGet)
		if head != http.StatusMethodNotAllowed || get != http.StatusOK {
			t.Errorf("HEAD=%d_GET=%d_EXPECTED_405_200", head, get)
		}
	})
	t.Run("expired_worker_reclaim_fenced", func(t *testing.T) {
		j := create("public test reason")
		first, ok, e := exports.claim(ctx)
		if e != nil || !ok || first.ID != j.ID {
			t.Fatal("FIXTURE_CLAIM")
		}
		if _, e = repo.pool.Exec(ctx, `UPDATE audit_export_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); e != nil {
			t.Fatal("FIXTURE_LEASE")
		}
		second, ok, e := exports.claim(ctx)
		if e != nil || !ok || second.ID != j.ID || second.Attempt != first.Attempt+1 {
			t.Fatal("FIXTURE_RECLAIM")
		}
		if e = exports.generate(ctx, first); !errors.Is(e, ErrConflict) {
			t.Errorf("OLD_GENERATE_%s", auditExportBoundaryErrorClass(e))
		}
		if e = exports.fail(ctx, first, "GENERATION_FAILED"); !errors.Is(e, ErrConflict) {
			t.Errorf("OLD_FAIL_%s", auditExportBoundaryErrorClass(e))
		}
		if e = exports.generate(ctx, second); e != nil {
			t.Fatalf("LIVE_GENERATE_%s", auditExportBoundaryErrorClass(e))
		}
		if e = exports.fail(ctx, first, "GENERATION_FAILED"); !errors.Is(e, ErrConflict) {
			t.Errorf("READY_OVERWRITE_%s", auditExportBoundaryErrorClass(e))
		}
		got, e := exports.Get(ctx, principal, j.ID)
		if e != nil || got.Status != "READY" {
			t.Fatal("READY_NOT_PRESERVED")
		}
	})
	t.Run("expired_same_attempt_fenced", func(t *testing.T) {
		j := create("public test reason")
		claim, ok, e := exports.claim(ctx)
		if e != nil || !ok || claim.ID != j.ID {
			t.Fatal("FIXTURE_CLAIM")
		}
		if _, e = repo.pool.Exec(ctx, `UPDATE audit_export_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); e != nil {
			t.Fatal("FIXTURE_LEASE")
		}
		if e = exports.generate(ctx, claim); !errors.Is(e, ErrConflict) {
			t.Errorf("EXPIRED_GENERATE_%s", auditExportBoundaryErrorClass(e))
		}
		if e = exports.fail(ctx, claim, "GENERATION_FAILED"); !errors.Is(e, ErrConflict) {
			t.Errorf("EXPIRED_FAIL_%s", auditExportBoundaryErrorClass(e))
		}
		// Retire this deliberately expired fixture before the next independent job.
		if _, e = repo.pool.Exec(ctx, `UPDATE audit_export_jobs SET status='FAILED' WHERE id=$1`, j.ID); e != nil {
			t.Fatal("FIXTURE_RETIRE_EXPIRED")
		}
	})
	t.Run("preview_export_same_actor_subject", func(t *testing.T) {
		now := time.Now().UTC()
		actor := "public-test-actor-subject"
		from := now.Add(-time.Minute)
		to := now.Add(time.Second)
		_, e := repo.pool.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,'public.actor.filter','test','public-test','SUCCEEDED','public-request','public-request',jsonb_build_object('actor_subject',$3::text),$4)`, uuid.New(), me.UserID, actor, now)
		if e != nil {
			t.Fatal("FIXTURE_AUDIT")
		}
		rows, e := repo.AdminAuditLogs(ctx, AdminAuditFilter{Actor: actor, From: &from, To: &to, Limit: 100})
		if e != nil || len(rows) != 1 {
			t.Fatal("PREVIEW_NOT_ONE")
		}
		job, e := exports.Create(ctx, principal, AuditExportRequest{Reason: "public filter reason", IdempotencyKey: uuid.NewString(), Filter: AuditExportFilter{Actor: actor, From: from, To: to}})
		if e != nil {
			t.Fatal("CREATE_FILTER")
		}
		claim, ok, e := exports.claim(ctx)
		if e != nil || !ok || claim.ID != job.ID {
			t.Fatal("CLAIM_FILTER")
		}
		if e = exports.generate(ctx, claim); e != nil {
			t.Fatalf("GENERATE_%s", auditExportBoundaryErrorClass(e))
		}
		got, e := exports.Get(ctx, principal, job.ID)
		if e != nil || got.RowCount == nil {
			t.Fatal("GET_FILTER")
		}
		if *got.RowCount != len(rows) {
			t.Errorf("PREVIEW=%d_EXPORT=%d", len(rows), *got.RowCount)
		}
	})

}

type auditExportBoundaryFixture struct {
	admin       *pgx.Conn
	config      *pgx.ConnConfig
	roles       []string
	dbs         map[string]string
	connections []*pgx.Conn
}

func newAuditExportBoundaryFixture(t *testing.T, ctx context.Context, raw string) *auditExportBoundaryFixture {
	t.Helper()
	expectedPort, expectedUser, expectedDirectory := uint16(55504), "pinvou_wave_test", "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data"
	switch os.Getenv("F17_TEST_DATABASE_PROFILE") {
	case "":
	case "github-actions-service":
		if os.Getenv("GITHUB_ACTIONS") != "true" {
			t.Fatal("CI fixture profile requires GitHub Actions")
		}
		expectedPort, expectedUser, expectedDirectory = 5432, "postgres", "/var/lib/postgresql/data"
	default:
		t.Fatal("unknown isolated PostgreSQL profile")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Host != "127.0.0.1" || config.Port != expectedPort || config.User != expectedUser || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 {
		t.Fatal("only the explicitly owned local PostgreSQL fixture is allowed")
	}
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect schema setup identity")
	}
	// Only this branch's fixtures share the deployment-actor set on their isolated instance.
	// The session lock serializes those fixtures across Go packages; it grants no authority.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou.f17.audit-export.boundary.fixture'))`); err != nil {
		t.Fatal("exclusive fixture lock failed")
	}
	var session, current, directory string
	var port, otherSessions, otherDBs int
	if err := admin.QueryRow(ctx, `SELECT session_user,current_user,inet_server_port(),current_setting('data_directory'),(SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND backend_type='client backend'),(SELECT count(*) FROM pg_database WHERE NOT datistemplate AND datname<>'postgres')`).Scan(&session, &current, &port, &directory, &otherSessions, &otherDBs); err != nil || session != expectedUser || current != expectedUser || port != int(expectedPort) || strings.ReplaceAll(directory, "\\", "/") != expectedDirectory || otherSessions != 0 || otherDBs != 0 {
		t.Fatal("owned server/identity/exclusive session/database guard failed")
	}
	f := &auditExportBoundaryFixture{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}}
	var count int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
		admin.Close(context.Background())
		t.Fatal("existing deployment actors: no fixture writes permitted")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, role := range f.roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" || role == "billing_service" {
			inherit = "INHERIT"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("exclusive actor creation rejected")
		}
	}
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		defer admin.Close(clean)
		for _, c := range f.connections {
			if err := c.Close(clean); err != nil {
				t.Error("owned migration connection close failed")
			}
		}
		allGone := true
		for name, owner := range f.dbs {
			var got string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			var queryErr error
			for {
				queryErr = admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&got, &active)
				if queryErr != nil || got != owner || active == 0 || !time.Now().Before(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if queryErr != nil || got != owner || active != 0 {
				t.Error("owned DB cleanup identity/connection guard failed")
				allGone = false
				continue
			}
			if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error("nonforce owned DB cleanup failed")
				allGone = false
			}
		}
		if !allGone {
			t.Error("owned actors retained after incomplete DB cleanup")
			return
		}
		var outside bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, f.roles).Scan(&outside); err != nil || outside {
			t.Error("external dependencies forbid actor cleanup")
			return
		}
		for i := len(f.roles) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{f.roles[i]}.Sanitize()); err != nil {
				t.Error("nonforce owned actor cleanup failed")
			}
		}
		if err := admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
			t.Error("owned actors remain after cleanup")
		} else {
			t.Log("OWNED_DATABASES_AND_ACTORS_CLEANED")
		}
	})
	return f
}

func (f *auditExportBoundaryFixture) database(t *testing.T, ctx context.Context, domain, owner string) *pgx.Conn {
	t.Helper()
	name := "f17_boundary_" + domain + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := f.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatal("create exclusively owned DB")
	}
	f.dbs[name] = owner
	t.Logf("owned fixture database=%s owner=%s", name, owner)
	config := f.config.Copy()
	config.Database = name
	if domain == "account" {
		config.User = "account_migrator"
		config.Password = ""
		if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO account_migrator"); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "billing" {
		config.User = "billing_migrator"
		config.Password = ""
	}
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect isolated migration actor")
	}
	f.connections = append(f.connections, c)
	if domain == "billing" {
		if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service`); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "account" || domain == "billing" {
		if _, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{domain + "_service"}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		var session, current string
		var super bool
		if err := c.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); err != nil || session != domain+"_migrator" || current != domain+"_service" || super {
			t.Fatal("non-super migration actor contract failed")
		}
	}
	return c
}

func (f *auditExportBoundaryFixture) serviceURL(t *testing.T, c *pgx.Conn, user string) string {
	t.Helper()
	parsed, err := url.Parse(f.config.ConnString())
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("schema fixture requires a PostgreSQL URL")
	}
	parsed.User = url.User(user)
	parsed.Path = "/" + c.Config().Database
	return parsed.String()
}
