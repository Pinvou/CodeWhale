package securityadmin

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type retainedActivationTrace struct {
	start    func(context.Context, *pgx.Conn, string)
	rollback *atomic.Int32
}

func (tr retainedActivationTrace) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if tr.start != nil {
		tr.start(ctx, c, d.SQL)
	}
	return ctx
}
func (tr retainedActivationTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var e *pgconn.PgError
	if tr.rollback != nil && errors.As(d.Err, &e) && e.Code == "P0001" && strings.Contains(e.Message, "controlled activation commit rollback") {
		tr.rollback.Add(1)
	}
}

// The server raises at deferred-constraint COMMIT, not in a mocked Go Commit.
// Only the T1 manager pool opts in through a custom test GUC.
func retainedActivationRollback(t *testing.T, f *retainedSecurityFixture, trace retainedActivationTrace) *Manager {
	t.Helper()
	if _, e := f.connection.Exec(f.ctx, `CREATE FUNCTION retained_activation_rollback() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF current_setting('pinvou.test_activation_rollback',true)='on' AND NEW.action IN ('security.super-admin.bootstrap.activated','security.break-glass.activated') THEN RAISE EXCEPTION 'controlled activation commit rollback'; END IF;RETURN NEW;END $$;
 CREATE CONSTRAINT TRIGGER retained_activation_rollback AFTER INSERT ON audit_logs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION retained_activation_rollback()`); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(f.accountURL)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["pinvou.test_activation_rollback"] = "on"
	cfg.ConnConfig.Tracer = trace
	pool, e := pgxpool.NewWithConfig(f.ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	return &Manager{pool: pool, wallets: f.manager.wallets, directory: f.manager.directory, issuer: f.manager.issuer, now: f.manager.now, breakGlassVerifier: f.manager.breakGlassVerifier}
}
func retainedActivationPending(t *testing.T, f *retainedSecurityFixture) BootstrapIssue {
	t.Helper()
	issue, e := f.manager.IssueBootstrap(f.ctx, "history-owned-deployment", "ONE_TIME_BOOTSTRAP", 15*time.Minute)
	if e != nil || issue.Activated {
		t.Fatal("pending issue", e)
	}
	if e := f.manager.MarkDelivered(f.ctx, issue.CredentialID); e != nil {
		t.Fatal(e)
	}
	return issue
}
func retainedActivationWait(t *testing.T, ctx context.Context, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(label, ctx.Err())
	}
}

func TestRetainedSecurityRollbackSuccessorAuthority(t *testing.T) {
	for _, mode := range []string{"same_bootstrap", "same_breakglass", "bootstrap_to_breakglass", "breakglass_to_active_pending_bootstrap", "unchanged_bootstrap", "unchanged_breakglass", "version_drift", "mapping_drift"} {
		t.Run(mode, func(t *testing.T) {
			f := retainedSecuritySetup(t)
			state := retainedRealIdentity(t, f)
			issue := retainedActivationPending(t, f)
			firstBG := strings.HasPrefix(mode, "same_breakglass") || strings.HasPrefix(mode, "breakglass_") || mode == "unchanged_breakglass"
			if firstBG {
				if e := f.manager.BreakGlass(f.ctx, retainedSignedRecovery(t, f)); e != nil {
					t.Fatal(e)
				}
			}
			var firstRequest BreakGlassRequest
			if firstBG {
				firstRequest = retainedSignedRecovery(t, f)
			}
			reached, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			defer release()
			var rollbacks atomic.Int32
			old := retainedActivationRollback(t, f, retainedActivationTrace{rollback: &rollbacks, start: func(c context.Context, _ *pgx.Conn, sql string) {
				if strings.HasPrefix(sql, "begin") && strings.Contains(sql, "read committed") {
					close(reached)
					select {
					case <-resume:
					case <-c.Done():
					}
				}
			}})
			state.mu.Lock()
			before := state.disables
			state.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				if firstBG {
					done <- old.BreakGlass(f.ctx, firstRequest)
				} else {
					done <- old.Redeem(f.ctx, "history-owned-deployment", issue.Secret, "S3cure-Quartz-71")
				}
			}()
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			defer release()
			retainedActivationWait(t, ctx, reached, "rollback outcome not reached")
			if rollbacks.Load() != 1 {
				t.Fatal("actual deferred COMMIT rollback not observed", rollbacks.Load())
			}
			unchanged := strings.HasPrefix(mode, "unchanged_")
			var t2err error
			switch mode {
			case "same_bootstrap", "breakglass_to_active_pending_bootstrap":
				var beforeVersion, afterVersion int64
				if e := f.connection.QueryRow(f.ctx, `SELECT version FROM users WHERE username='admin'`).Scan(&beforeVersion); e != nil {
					t.Fatal(e)
				}
				t2err = f.manager.Redeem(f.ctx, "history-owned-deployment", issue.Secret, "An0ther-Quartz-72")
				if e := f.connection.QueryRow(f.ctx, `SELECT version FROM users WHERE username='admin'`).Scan(&afterVersion); e != nil {
					t.Fatal(e)
				}
				if mode == "breakglass_to_active_pending_bootstrap" && beforeVersion != afterVersion {
					t.Fatal("ACTIVE bootstrap unexpectedly changed version", beforeVersion, afterVersion)
				}
			case "same_breakglass", "bootstrap_to_breakglass":
				t2err = f.manager.BreakGlass(f.ctx, retainedSignedRecovery(t, f))
			case "version_drift":
				_, t2err = f.connection.Exec(f.ctx, `UPDATE users SET version=version+1 WHERE username='admin'`)
			case "mapping_drift":
				_, t2err = f.connection.Exec(f.ctx, `UPDATE identities SET subject=$1 WHERE subject=$2`, uuid.NewString(), f.directory.subject)
			}
			if t2err != nil {
				t.Fatal("successor did not succeed", t2err)
			}
			release()
			var result error
			select {
			case result = <-done:
			case <-ctx.Done():
				t.Fatal("old call did not finish")
			}
			if result == nil {
				t.Fatal("rolled back old call claimed success")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if unchanged {
				if state.enabled || state.disables-before != 1 {
					t.Fatal("unchanged authority not compensated exactly once", state.enabled, state.disables-before)
				}
			} else {
				if !strings.Contains(result.Error(), "SECURITY_ACTIVATION_COMMIT_UNCERTAIN") {
					t.Fatal("old attempt lacks uncertainty", result)
				}
				if !state.enabled || state.disables != before {
					t.Fatal("old compensation disabled successor/changed authority", state.enabled, state.disables-before)
				}
			}
		})
	}
}

func TestRetainedSecurityCompensationSerializesCrossMode(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	issue := retainedActivationPending(t, f)
	var rollback atomic.Int32
	old := retainedActivationRollback(t, f, retainedActivationTrace{rollback: &rollback})
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	state.mu.Lock()
	state.disableEntered = entered
	state.disableContinue = resume
	before := state.disables
	state.mu.Unlock()
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	defer release()
	oldDone := make(chan error, 1)
	go func() { oldDone <- old.Redeem(f.ctx, "history-owned-deployment", issue.Secret, "S3cure-Quartz-71") }()
	retainedActivationWait(t, ctx, entered, "compensation HTTP not reached")
	request := retainedSignedRecovery(t, f)
	pids := make(chan uint32, 1)
	var seen sync.Once
	cfg, e := pgxpool.ParseConfig(f.accountURL)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.Tracer = retainedActivationTrace{start: func(_ context.Context, c *pgx.Conn, sql string) {
		if strings.Contains(sql, "FOR UPDATE OF u") {
			seen.Do(func() { pids <- c.PgConn().PID() })
		}
	}}
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	defer cancel()
	defer release()
	next := &Manager{pool: pool, wallets: f.manager.wallets, directory: f.manager.directory, issuer: f.manager.issuer, now: f.manager.now, breakGlassVerifier: f.manager.breakGlassVerifier}
	nextDone := make(chan error, 1)
	go func() { nextDone <- next.BreakGlass(ctx, request) }()
	var pid uint32
	select {
	case pid = <-pids:
	case <-ctx.Done():
		t.Fatal("successor lock query not reached")
	}
	for {
		var blocked bool
		if e := f.connection.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("cross-mode successor was not database-blocked")
		}
	}
	t.Logf("cross-mode successor backend %d waits while compensation owns user", pid)
	release()
	select {
	case e := <-oldDone:
		if e == nil {
			t.Fatal("old rollback succeeded")
		}
	case <-ctx.Done():
		t.Fatal("old call stuck")
	}
	select {
	case e := <-nextDone:
		if e != nil {
			t.Fatal("successor activation failed", e)
		}
	case <-ctx.Done():
		t.Fatal("successor stuck")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if rollback.Load() != 1 || !state.enabled || state.disables-before != 1 {
		t.Fatal("compensation did not precede successor", rollback.Load(), state.enabled, state.disables-before)
	}
}

func TestRetainedSecurityDurableReceiptWithoutCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"bootstrap", "breakglass"} {
		t.Run(mode, func(t *testing.T) {
			f := retainedSecuritySetup(t)
			state := retainedRealIdentity(t, f)
			if mode == "breakglass" {
				f.activate(t)
			}
			var hookErr error
			proxy, armed, drops := retainedCommitACKProxyHook(t, f.accountURL, false, func() {
				// Keep a second valid administrator: the real last-admin trigger must
				// stay enabled. This is a legal retained-state SQL fixture, not
				// a claim that admin assignment approvals were exercised.
				var code string
				code, hookErr = account.GenerateUserCode()
				if hookErr != nil {
					return
				}
				other := uuid.New()
				_, hookErr = f.connection.Exec(f.ctx, `INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,$2,'ACTIVE',now(),now())`, other, code)
				if hookErr != nil {
					return
				}
				_, hookErr = f.connection.Exec(f.ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($1,$2,'PLATFORM_SUPER_ADMIN','ACCOUNT','ACCOUNT',now(),now())`, uuid.New(), other)
				if hookErr != nil {
					return
				}
				_, hookErr = f.connection.Exec(f.ctx, `UPDATE role_assignments SET revoked_at=now() WHERE role_code='PLATFORM_SUPER_ADMIN' AND revoked_at IS NULL AND user_id=(SELECT id FROM users WHERE username='admin')`)

			})
			m, e := NewManager(f.ctx, proxy, f.manager.issuer, f.manager.directory)
			if e != nil {
				t.Fatal(e)
			}
			defer m.Close()
			m.WithWalletProvisioner(f.manager.wallets)
			var request BreakGlassRequest
			var issue BootstrapIssue
			if mode == "bootstrap" {
				issue = retainedActivationPending(t, f)
			} else {
				request = retainedSignedRecovery(t, f)
				m.WithBreakGlassVerifier(f.manager.breakGlassVerifier)
			}
			state.mu.Lock()
			before := state.disables
			state.mu.Unlock()
			armed.Store(true)
			var result error
			if mode == "bootstrap" {
				result = m.Redeem(f.ctx, "history-owned-deployment", issue.Secret, "S3cure-Quartz-71")
			} else {
				result = m.BreakGlass(f.ctx, request)
			}
			if hookErr != nil || drops.Load() != 1 || result == nil || !strings.Contains(result.Error(), "SECURITY_ACTIVATION_COMMIT_UNCERTAIN") {
				t.Fatal("durable incomplete receipt not uncertain", hookErr, drops.Load(), result)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if !state.enabled || state.disables != before {
				t.Fatal("durable incomplete receipt compensated", state.enabled, state.disables-before)
			}
		})
	}
}
