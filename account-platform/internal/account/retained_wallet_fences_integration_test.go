package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The seven original A08 wallet-fence scenarios below are retained. Only the
// fixture is projected to current public F01 migrations. Administrative setup
// creates exclusively absent actors; business operations use account_service.
type retainedWalletFenceFixture struct {
	ctx    context.Context
	admin  *pgx.Conn
	config *pgx.ConnConfig
}

func retainedWalletFenceActors(t *testing.T) *retainedWalletFenceFixture {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL required for wallet lease fences")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("open fixture setup actor")
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse fixture setup configuration")
	}
	roles := []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery"}
	var existing int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("fixed actor conflict: no setup writes")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal("begin fixture actor transaction")
	}
	defer tx.Rollback(context.Background())
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("concurrent actor conflict")
	}
	for _, role := range roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" {
			inherit = "INHERIT"
		}
		if _, err = tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("owned actor setup failed")
		}
	}
	if _, err = tx.Exec(ctx, "GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE"); err != nil {
		t.Fatal("retained actor membership setup failed")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("commit owned actors")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		var outside bool
		err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend d WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members a WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, roles).Scan(&outside)
		if err != nil || outside {
			t.Error("external dependencies: owned roles retained")
			return
		}
		for i := len(roles) - 1; i >= 0; i-- {
			if _, err = admin.Exec(clean, "DROP ROLE "+pgx.Identifier{roles[i]}.Sanitize()); err != nil {
				t.Error("owned role cleanup failed")
			}
		}
		if err = admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
			t.Error("owned actors remain")
		}
	})
	return &retainedWalletFenceFixture{ctx, admin, config}
}

func (f *retainedWalletFenceFixture) repository(t *testing.T) *PostgresRepository {
	return f.repositoryWithInitialStage(t, nil)
}

func (f *retainedWalletFenceFixture) repositoryWithInitialStage(t *testing.T, initial func(context.Context, *pgx.Conn, []migrations.Migration) error) *PostgresRepository {
	t.Helper()
	ctx := f.ctx
	name := "f01_wallet_fence_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := f.admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER account_service"); err != nil {
		t.Fatal("owned database setup failed")
	}
	t.Logf("owned wallet fixture database=%s owner=account_service", name)
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		var owner string
		var active int
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := f.admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active)
			if err != nil || owner != "account_service" {
				t.Error("own database identity guard failed")
				return
			}
			if active == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Error("own database has live sessions; retained")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := f.admin.Exec(clean, "DROP DATABASE "+quoted); err != nil {
			t.Error("nonforce owned database cleanup failed")
			return
		}
		var absent bool
		if err := f.admin.QueryRow(clean, `SELECT NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&absent); err != nil || !absent {
			t.Error("owned database remains")
		}
	})
	if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+quoted+" TO account_migrator"); err != nil {
		t.Fatal("original migration CREATE grant failed")
	}
	migrationConfig := f.config.Copy()
	migrationConfig.Database = name
	migrationConfig.User = "account_migrator"
	migrationConfig.Password = ""
	connection, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal("open genuine migrator")
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, "SET ROLE account_service"); err != nil {
		t.Fatal("select original schema owner")
	}
	var actor, session string
	var super bool
	if err = connection.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "account_service" || session != "account_migrator" || super {
		t.Fatal("migration actor isolation not proven")
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if initial != nil {
		if err = initial(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
	}
	if err = accountschema.Up(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(f.config.ConnString())
	if err != nil {
		t.Fatal("parse genuine runtime configuration")
	}
	configuration.MaxConns = 4
	configuration.ConnConfig.Database = name
	configuration.ConnConfig.User = "account_service"
	configuration.ConnConfig.Password = ""
	configuration.ConnConfig.RuntimeParams["application_name"] = "walletfence_" + name
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal("open genuine runtime")
	}
	t.Cleanup(pool.Close)
	if err = pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &super); err != nil || actor != "account_service" || super {
		t.Fatal("runtime actor isolation not proven")
	}
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, walletPolicyVersion: walletProvisioningPolicyVersion, walletProvisioningSLA: walletProvisioningSLA}
}

func retainedWalletFenceSnapshot(t *testing.T, repo *PostgresRepository, id uuid.UUID) string {
	t.Helper()
	var facts string
	err := repo.pool.QueryRow(context.Background(), `SELECT jsonb_build_object('user',to_jsonb(u),'job',(SELECT to_jsonb(j) FROM wallet_provisioning_jobs j WHERE j.user_id=u.id),'operations',(SELECT COALESCE(jsonb_agg(o ORDER BY o.id),'[]'::jsonb) FROM registration_operations o WHERE o.committed_user_id=u.id),'events',(SELECT COALESCE(jsonb_agg(e ORDER BY e.event_id),'[]'::jsonb) FROM outbox_events e WHERE e.aggregate_id=u.id),'audits',(SELECT COALESCE(jsonb_agg(a ORDER BY a.id),'[]'::jsonb) FROM audit_logs a WHERE a.resource_id=u.id::text))::text FROM users u WHERE u.id=$1::uuid`, id).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}
func retainedWalletProvisioningUser(t *testing.T, repo *PostgresRepository) uuid.UUID {
	t.Helper()
	me, _, err := repo.Bootstrap(context.Background(), identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "pending" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	return me.UserID
}

func TestPostgresWalletProvisioningRejectsExpiredAndSupersededClaims(t *testing.T) {
	fixture := retainedWalletFenceActors(t)
	for _, scenario := range []string{"old_complete", "old_retry", "expired_complete", "expired_retry", "disabled", "current_complete", "current_retry"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repo := fixture.repository(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			repo.now = func() time.Time { return now }
			userID := retainedWalletProvisioningUser(t, repo)
			jobs, err := repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
			if err != nil || len(jobs) != 1 {
				t.Fatal("wallet job was not claimed")
			}
			old := jobs[0]
			if old.Attempts != 1 || old.LeaseUntil.IsZero() {
				t.Fatal("claim must expose its identity")
			}
			if scenario == "old_complete" || scenario == "old_retry" || scenario == "expired_complete" || scenario == "expired_retry" {
				now = now.Add(31 * time.Second)
			}
			var successor WalletProvisioningJob
			if scenario == "old_complete" || scenario == "old_retry" {
				jobs, err = repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
				if err != nil || len(jobs) != 1 {
					t.Fatal("expired claim was not reclaimed")
				}
				successor = jobs[0]
			}
			if scenario == "disabled" {
				if _, err := repo.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, userID); err != nil {
					t.Fatal(err)
				}
			}
			beforeFacts := retainedWalletFenceSnapshot(t, repo, userID)
			if scenario == "old_retry" || scenario == "expired_retry" || scenario == "current_retry" {
				err = repo.FailWalletProvisioning(ctx, old, "BILLING_UNAVAILABLE")
			} else {
				err = repo.CompleteWalletProvisioning(ctx, old)
			}
			valid := scenario == "current_complete" || scenario == "current_retry"
			if valid && err != nil || !valid && !errors.Is(err, ErrConflict) {
				t.Fatal("claim result did not respect the current lease")
			}
			if !valid && retainedWalletFenceSnapshot(t, repo, userID) != beforeFacts {
				t.Fatal("rejected wallet claim changed durable facts")
			}
			var status string
			var attempts int
			var until *time.Time
			if err := repo.pool.QueryRow(ctx, `SELECT status,attempts,lease_until FROM wallet_provisioning_jobs WHERE user_id=$1`, userID).Scan(&status, &attempts, &until); err != nil {
				t.Fatal(err)
			}
			if !valid && (status != "LEASED" || until == nil) {
				t.Fatal("stale worker changed the current claim")
			}
			if successor.UserID != uuid.Nil && (attempts != successor.Attempts || !until.Equal(successor.LeaseUntil)) {
				t.Fatal("stale worker erased the successor lease")
			}
			if scenario == "current_complete" {
				if status != "COMPLETED" || until != nil {
					t.Fatal("current completion did not finish")
				}
				if !errors.Is(repo.CompleteWalletProvisioning(ctx, old), ErrConflict) {
					t.Fatal("completed claim was replayed")
				}
				completedFacts := retainedWalletFenceSnapshot(t, repo, userID)
				if !errors.Is(repo.FailWalletProvisioning(ctx, old, "BILLING_UNAVAILABLE"), ErrConflict) {
					t.Fatal("completed claim was moved back to retry")
				}
				if retainedWalletFenceSnapshot(t, repo, userID) != completedFacts {
					t.Fatal("late failure changed completed durable facts")
				}
				var events int
				if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='account.wallet.provisioning.completed.v1' AND aggregate_id=$1`, userID).Scan(&events); err != nil || events != 1 {
					t.Fatal("completion event was not unique")
				}
			}
		})
	}
}

func TestRetainedWalletCompletionFencesAfterLockWait(t *testing.T) {
	fixture := retainedWalletFenceActors(t)
	for _, scenario := range []string{"expired-complete-job-lock", "expired-fail-job-lock", "expired-complete-user-lock", "disabled-during-user-lock"} {
		t.Run(scenario, func(t *testing.T) {
			repo := fixture.repository(t)
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			var clock atomic.Int64
			base := time.Now().UTC().Truncate(time.Microsecond)
			clock.Store(base.UnixNano())
			repo.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			id := retainedWalletProvisioningUser(t, repo)
			jobs, err := repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
			if err != nil || len(jobs) != 1 {
				t.Fatal("claim missing")
			}
			job := jobs[0]
			blocker, err := repo.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			table := "wallet_provisioning_jobs"
			column := "user_id"
			if strings.Contains(scenario, "user-lock") {
				table = "users"
				column = "id"
			}
			if _, err = blocker.Exec(ctx, "SELECT 1 FROM "+table+" WHERE "+column+"=$1 FOR UPDATE", id); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				if strings.Contains(scenario, "fail-") {
					finished <- repo.FailWalletProvisioning(ctx, job, "BILLING_UNAVAILABLE")
				} else {
					finished <- repo.CompleteWalletProvisioning(ctx, job)
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			waited := false
			for time.Now().Before(deadline) {
				if err = fixture.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND application_name=$2 AND wait_event_type='Lock' AND query LIKE $3)`, repo.pool.Config().ConnConfig.Database, repo.pool.Config().ConnConfig.RuntimeParams["application_name"], "%"+table+"%FOR UPDATE%").Scan(&waited); err != nil {
					t.Fatal(err)
				}
				if waited {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !waited {
				t.Fatal("production statement never waited on the owned blocker")
			}
			if scenario == "disabled-during-user-lock" {
				if _, err = blocker.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			} else {
				clock.Store(base.Add(31 * time.Second).UnixNano())
			}
			// Read the precommit target after the controlled state transition, while the
			// production transaction is still blocked. Only the administrative disable
			// itself is allowed to mutate the snapshot.
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-finished:
			case <-ctx.Done():
				t.Fatal("bounded wallet mutation did not finish")
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("post-lock authority must reject: %T", err)
			}
			var status, jobStatus string
			var completed *time.Time
			var attempt, events, audits int
			if err = repo.pool.QueryRow(ctx, `SELECT u.status,j.status,j.completed_at,j.attempts,(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.wallet.provisioning.completed.v1'),(SELECT count(*) FROM audit_logs WHERE resource_id=($1::uuid)::text AND action='account.wallet.provision') FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1::uuid::uuid`, id).Scan(&status, &jobStatus, &completed, &attempt, &events, &audits); err != nil {
				t.Fatal(err)
			}
			expected := "PROVISIONING"
			if scenario == "disabled-during-user-lock" {
				expected = "DISABLED"
			}
			if status != expected || jobStatus != "LEASED" || completed != nil || attempt != job.Attempts || events != 0 || audits != 0 {
				t.Fatal("rejected completion changed job/user/event/audit facts")
			}
		})
	}
}

func TestRetainedWalletExpiredFailureUserLock(t *testing.T) {
	fixture := retainedWalletFenceActors(t)
	repo := fixture.repository(t)
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	var clock atomic.Int64
	base := time.Now().UTC().Truncate(time.Microsecond)
	clock.Store(base.UnixNano())
	repo.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	id := retainedWalletProvisioningUser(t, repo)
	if _, err := repo.pool.Exec(ctx, `UPDATE wallet_provisioning_jobs SET sla_expires_at=$2 WHERE user_id=$1`, id, base.Add(-time.Second)); err != nil {
		t.Fatal("setup already elapsed SLA")
	}
	jobs, err := repo.LeaseWalletProvisioningJobs(ctx, 1, 30*time.Second)
	if err != nil || len(jobs) != 1 {
		t.Fatal("real live claim missing")
	}
	job := jobs[0]
	before := retainedWalletFenceSnapshot(t, repo, id)
	blocker, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin blocker")
	}
	defer blocker.Rollback(context.Background())
	var blockerPID int
	if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal("blocker pid")
	}
	if _, err = blocker.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal("lock user")
	}
	finished := make(chan error, 1)
	go func() { finished <- repo.FailWalletProvisioning(ctx, job, "BILLING_UNAVAILABLE") }()
	waited := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err = fixture.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND application_name=$2 AND $3=ANY(pg_blocking_pids(pid)) AND query LIKE '%UPDATE users%')`, repo.pool.Config().ConnConfig.Database, repo.pool.Config().ConnConfig.RuntimeParams["application_name"], blockerPID).Scan(&waited); err != nil {
			t.Fatal("observe exact production user wait")
		}
		if waited {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waited {
		t.Fatal("Fail never reached actual SLA user-row wait")
	}
	clock.Store(base.Add(31 * time.Second).UnixNano())
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal("release pure row lock")
	}
	select {
	case err = <-finished:
	case <-ctx.Done():
		t.Fatal("bounded failure stalled")
	}
	unchanged := retainedWalletFenceSnapshot(t, repo, id) == before
	if !errors.Is(err, ErrConflict) || !unchanged {
		t.Fatalf("expired failure after actual user wait: error=%T unchanged=%t", err, unchanged)
	}
}
