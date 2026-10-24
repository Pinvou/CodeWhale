package account

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func historyClaimSnapshot(t *testing.T, r *PostgresRepository, table string, id uuid.UUID) string {
	t.Helper()
	var result string
	query := `SELECT jsonb_build_object('operation',(SELECT to_jsonb(x) FROM ` + table + ` x WHERE id=$1),'audits',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events),'sessions',(SELECT count(*) FROM user_sessions),'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u))::text`
	if err := r.pool.QueryRow(context.Background(), query, id).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
func historyProfile(t *testing.T, r *PostgresRepository) ProfileChange {
	t.Helper()
	p, _ := f01ActiveFixtureUser(t, r)
	c, err := r.CreateProfileChange(context.Background(), p, "USERNAME", "history"+strings.ReplaceAll(uuid.NewString(), "-", "")[:16])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(context.Background(), `UPDATE profile_change_requests SET status='VERIFIED',next_attempt_at=$2 WHERE id=$1`, c.ID, r.now()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIdentityRecoveryClaimsRuntimePostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	t.Run("registration-stale-expired-and-detached-cleanup", func(t *testing.T) {
		r := f.repository(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		ctx := context.Background()
		id := registrationBonusTestOperation(t, ctx, r, "claim_history", "+8613800138000")
		var challenge uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT sms_challenge_id FROM registration_operations WHERE id=$1`, id).Scan(&challenge); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkSMSChallengeSent(ctx, challenge, "controlled", nil); err != nil {
			t.Fatal(err)
		}
		if err := r.ConsumeRegistrationSMS(ctx, id, challenge, "+8613800138000", true); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRegistrationIdentityCreating(ctx, id); err != nil {
			t.Fatal(err)
		}
		now = now.Add(6 * time.Second)
		old, err := r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second)
		if err != nil || len(old) != 1 {
			t.Fatal("first claim", err, len(old))
		}
		oldCtx, cancel, err := withIdentityRecoveryClaim(ctx, "registration", id, old[0].RecoveryGeneration, old[0].RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		oldCtx = context.WithoutCancel(oldCtx)
		// Moving retry earlier cannot let another worker steal an unexpired lease.
		if _, err = r.pool.Exec(ctx, `UPDATE registration_operations SET next_retry_at=$2 WHERE id=$1`, id, now); err != nil {
			t.Fatal(err)
		}
		if got, err := r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second); err != nil || len(got) != 0 {
			t.Fatal("schedule bypassed lease", err)
		}
		now = now.Add(31 * time.Second)
		current, err := r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second)
		if err != nil || len(current) != 1 || current[0].RecoveryGeneration <= old[0].RecoveryGeneration {
			t.Fatal("reclaim", err)
		}
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		writes := []func() error{
			func() error { return r.MarkRegistrationIdentityCreating(oldCtx, id) },
			func() error { return r.MarkRegistrationIdentityCreated(oldCtx, id, "controlled-subject") },
			func() error { _, err := r.CommitRegistration(oldCtx, id, "https://identity.example.test"); return err },
			func() error { return r.ScheduleRegistrationRetry(oldCtx, id, "STALE") },
			func() error { return r.CompensatePreIdentityRegistration(oldCtx, id, "STALE", 1) },
		}
		for i, write := range writes {
			if err := write(); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
				t.Fatalf("old writer %d: %v", i, err)
			}
		}
		if after := historyClaimSnapshot(t, r, "registration_operations", id); after != before {
			t.Fatal("old claim changed operation/user/session/audit/outbox facts")
		}
		currentCtx, currentCancel, err := withIdentityRecoveryClaim(ctx, "registration", id, current[0].RecoveryGeneration, current[0].RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		defer currentCancel()
		if err := r.ScheduleRegistrationRetry(currentCtx, id, "CURRENT"); err != nil {
			t.Fatal("current owner", err)
		}
		now = now.Add(31 * time.Second)
		if err := r.MarkRegistrationIdentityCreated(context.WithoutCancel(currentCtx), id, "controlled-subject"); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("expired owner wrote", err)
		}
		stored, err := r.RegistrationOperation(ctx, id)
		if err != nil || stored.IdentitySubject != "" || stored.LastErrorCode != "CURRENT" {
			t.Fatal("claim facts changed", err)
		}
	})
	t.Run("profile-stale-expired-lock-order-and-first-dispatch", func(t *testing.T) {
		r := f.repository(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		ctx := context.Background()
		c := historyProfile(t, r)
		old, err := r.ClaimProfileChange(ctx)
		if err != nil || old == nil {
			t.Fatal("first profile claim", err)
		}
		oldCtx, cancel, err := withIdentityRecoveryClaim(ctx, "profile", c.ID, old.RecoveryGeneration, old.RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		oldCtx = context.WithoutCancel(oldCtx)
		now = now.Add(31 * time.Second)
		current, err := r.ClaimProfileChange(ctx)
		if err != nil || current == nil || current.RecoveryGeneration <= old.RecoveryGeneration {
			t.Fatal("profile reclaim", err)
		}
		before := historyClaimSnapshot(t, r, "profile_change_requests", c.ID)
		for _, bad := range []identityRecoveryClaim{
			{kind: "registration", id: c.ID, generation: current.RecoveryGeneration, until: *current.RecoveryLeaseUntil},
			{kind: "profile", id: uuid.New(), generation: current.RecoveryGeneration, until: *current.RecoveryLeaseUntil},
			{kind: "profile", id: c.ID, generation: current.RecoveryGeneration - 1, until: *current.RecoveryLeaseUntil},
		} {
			badCtx := context.WithValue(ctx, identityRecoveryClaimKey{}, bad)
			if err := r.ScheduleProfileChangeRetry(badCtx, c.ID, "WRONG_OWNER"); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
				t.Fatal("claim kind/id/generation mismatch accepted", err)
			}
		}

		writes := []func() error{
			func() error { return r.MarkProfileIdentityUpdated(oldCtx, c.ID) },
			func() error { return r.CommitProfileChange(oldCtx, c.ID) },
			func() error { _, err := r.StartProfileSessionRevocation(oldCtx, c.ID); return err },
			func() error { return r.MarkProfileSessionsRevoked(oldCtx, c.ID) },
			func() error { return r.CompleteProfileChange(oldCtx, c.ID) },
			func() error { return r.ScheduleProfileChangeRetry(oldCtx, c.ID, "STALE") },
		}
		for i, write := range writes {
			if err := write(); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
				t.Fatalf("old profile writer %d: %v", i, err)
			}
		}
		if after := historyClaimSnapshot(t, r, "profile_change_requests", c.ID); after != before {
			t.Fatal("stale profile changed protected facts")
		}
		currentCtx, currentCancel, err := withIdentityRecoveryClaim(ctx, "profile", c.ID, current.RecoveryGeneration, current.RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		defer currentCancel()
		if err := r.MarkProfileIdentityUpdated(currentCtx, c.ID); err != nil {
			t.Fatal("current mark", err)
		}
		if err := r.CommitProfileChange(currentCtx, c.ID); err != nil {
			t.Fatal("current commit", err)
		}
		if first, err := r.StartProfileSessionRevocation(currentCtx, c.ID); err != nil || !first {
			t.Fatal("current first dispatch", err)
		}
		if first, err := r.StartProfileSessionRevocation(currentCtx, c.ID); err != nil || first {
			t.Fatal("duplicate first dispatch", err)
		}
		now = now.Add(31 * time.Second)
		if err := r.MarkProfileSessionsRevoked(context.WithoutCancel(currentCtx), c.ID); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("expired sessions write", err)
		}
	})
	t.Run("expiry-after-row-lock-wait", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		id := registrationBonusTestOperation(t, ctx, r, "lock_wait_history", "+8613800138000")
		if _, err := r.pool.Exec(ctx, `UPDATE registration_operations SET status='IDENTITY_CREATING',next_retry_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		jobs, err := r.LeaseRegistrationRecoveries(ctx, 1, time.Second)
		if err != nil || len(jobs) != 1 {
			t.Fatal("lock fixture claim", err)
		}
		owned, cancel, err := withIdentityRecoveryClaim(ctx, "registration", id, jobs[0].RecoveryGeneration, jobs[0].RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		owned = context.WithoutCancel(owned)
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		var locked uuid.UUID
		if err = blocker.QueryRow(ctx, `SELECT id FROM registration_operations WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		done := make(chan error, 1)
		go func() { done <- r.ScheduleRegistrationRetry(owned, id, "LOCK_WAIT_STALE") }()
		deadline := time.Now().Add(3 * time.Second)
		var waiting bool
		for time.Now().Before(deadline) {
			if err = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE 'SELECT recovery_generation,%')`, r.pool.Config().ConnConfig.Database).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("writer never reached actual PostgreSQL row-lock barrier")
		}
		if delay := time.Until(*jobs[0].RecoveryLeaseUntil) + 20*time.Millisecond; delay > 0 {
			time.Sleep(delay)
		}
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-done; !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("claim expiry during lock wait was not checked", err)
		}
		if after := historyClaimSnapshot(t, r, "registration_operations", id); after != before {
			t.Fatal("lock-wait expiry changed protected facts")
		}
	})
	t.Run("upgrade-preserves-identities-and-unconfirmed-dispatch", func(t *testing.T) {
		var id uuid.UUID
		var previousLedger, previousOperation string
		r := f.repositoryWithInitialStage(t, func(ctx context.Context, c *pgx.Conn, all []migrations.Migration) error {
			var old []migrations.Migration
			for _, m := range all {
				if m.Version != 900007 {
					old = append(old, m)
				}
			}
			if err := accountschema.Up(ctx, c, old); err != nil {
				return err
			}
			var has bool
			if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='profile_change_requests' AND column_name='sessions_revocation_started_at')`).Scan(&has); err != nil || has {
				return fmt.Errorf("new field existed before upgrade: %v", err)
			}
			if err := c.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&previousLedger); err != nil {
				return err
			}
			user := uuid.New()
			id = uuid.New()
			if _, err := c.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,created_at,updated_at) VALUES($1,generate_unique_user_code(),'ACTIVE','history_upgrade','history',now(),now())`, user); err != nil {
				return err
			}
			_, err := c.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,fence_version,status,account_committed_at,identity_updated_at,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'https://identity.example.test','subject','USERNAME','old','new',1,1,'ACCOUNT_COMMITTED',now(),now(),now(),now()+interval '1 hour',now(),now())`, id, user)
			if err != nil {
				return err
			}
			if err = c.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM profile_change_requests p WHERE id=$1`, id).Scan(&previousOperation); err != nil {
				return err
			}
			return nil
		})
		var fields int
		if err := r.pool.QueryRow(f.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name IN ('registration_operations','profile_change_requests') AND column_name IN ('recovery_generation','recovery_lease_until','sessions_revocation_started_at')`).Scan(&fields); err != nil || fields != 5 {
			t.Fatal("actual claim schema incomplete", err, fields)
		}
		t.Logf("actual new identity recovery fields=%d", fields)
		var after string
		if err := r.pool.QueryRow(f.ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900007`).Scan(&after); err != nil || after != previousLedger {
			t.Fatal("published migration identity changed", err)
		}
		var upgradedOperation string
		if err := r.pool.QueryRow(f.ctx, `SELECT (to_jsonb(p)-'recovery_generation'-'recovery_lease_until'-'sessions_revocation_started_at')::text FROM profile_change_requests p WHERE id=$1`, id).Scan(&upgradedOperation); err != nil || upgradedOperation != previousOperation {
			t.Fatal("upgrade rewrote pre-existing operation facts", err)
		}
		var started, committed time.Time
		if err := r.pool.QueryRow(f.ctx, `SELECT sessions_revocation_started_at,account_committed_at FROM profile_change_requests WHERE id=$1`, id).Scan(&started, &committed); err != nil || !started.Equal(committed) {
			t.Fatal("uncertain history invented first dispatch", err)
		}
		if first, err := r.StartProfileSessionRevocation(f.ctx, id); err != nil || first {
			t.Fatal("old committed gap granted fresh logout", err)
		}
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		assertHistoricalForwardOnlyMigration(t, r, installed, 900007,
			"identity recovery generations and profile dispatch facts are forward-only")
		var afterDown string
		if err = r.pool.QueryRow(f.ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900007`).Scan(&afterDown); err != nil || afterDown != previousLedger {
			t.Fatal("failed down changed history", err)
		}
		t.Setenv("APP_ENV", "test")
		old, err := r.ProfileChange(f.ctx, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		directory := &historyLogoutDirectory{}
		profileService := &ProfileChangeService{store: r, identities: historyAdapter(t, directory), now: time.Now}
		if _, err = profileService.resume(f.ctx, old); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) {
			t.Fatal("uncertain old profile guessed a new logout", err)
		}
		repaired, err := r.ProfileChange(f.ctx, id, nil)
		if err != nil || repaired.Status != "REPAIR_REQUIRED" || directory.calls.Load() != 0 {
			t.Fatal("old dispatch gap not reserved for manual review", err)
		}
		var actor string
		var super bool
		if err := r.pool.QueryRow(f.ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &super); err != nil || actor != "account_service" || super {
			t.Fatal("runtime actor", err)
		}
		t.Logf("runtime actor=%s superuser=%t; previous ledger unchanged and DownOne rejected", actor, super)
	})
}

type historyLogoutDirectory struct {
	identityadapter.UserDirectory
	calls   atomic.Int64
	unknown bool
}

func (d *historyLogoutDirectory) RevokeSessions(context.Context, string) error {
	d.calls.Add(1)
	if d.unknown {
		return errors.New("controlled lost logout ACK")
	}
	return nil
}

type historyCheckpointStore struct {
	ProfileChangeStore
	fail bool
}

func (s *historyCheckpointStore) MarkProfileSessionsRevoked(ctx context.Context, id uuid.UUID) error {
	if s.fail {
		return errors.New("controlled Account checkpoint unavailable")
	}
	return s.ProfileChangeStore.MarkProfileSessionsRevoked(ctx, id)
}
func historyAdapter(t *testing.T, d *historyLogoutDirectory) *HTTPIdentityProvisioner {
	t.Helper()
	vault, err := identityadapter.NewPasswordVault([]byte(strings.Repeat("k", 32)), d)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identityadapter.NewService(vault, "controlled-profile-service-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-profile-service-token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func TestProfileSessionRevocationRecoveryRuntimePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	f := retainedWalletFenceActors(t)
	for _, mode := range []string{"confirmed-checkpoint-lost", "unknown-ack", "adapter-restart"} {
		t.Run(mode, func(t *testing.T) {
			r := f.repository(t)
			ctx := context.Background()
			c := historyProfile(t, r)
			if err := r.MarkProfileIdentityUpdated(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			if err := r.CommitProfileChange(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			c, err := r.ProfileChange(ctx, c.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			d := &historyLogoutDirectory{unknown: mode == "unknown-ack"}
			client := historyAdapter(t, d)
			store := &historyCheckpointStore{ProfileChangeStore: r, fail: mode != "unknown-ack"}
			service := &ProfileChangeService{store: store, identities: client, now: time.Now}
			if _, err := service.resume(ctx, c); err == nil {
				t.Fatal("first incomplete outcome hidden")
			}
			if d.calls.Load() != 1 {
				t.Fatal("first dispatch count", d.calls.Load())
			}
			c, err = r.ProfileChange(ctx, c.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.SessionsRevocationStartedAt == nil || c.SessionsRevokedAt != nil {
				t.Fatal("dispatch checkpoint is not durable")
			}
			store.fail = false
			if mode == "adapter-restart" {
				service.identities = historyAdapter(t, d)
			}
			_, resumeErr := service.resume(ctx, c)
			final, err := r.ProfileChange(ctx, c.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "confirmed-checkpoint-lost" {
				if resumeErr != nil || final.Status != "COMPLETED" || final.SessionsRevokedAt == nil {
					t.Fatal("confirmed receipt failed to converge", resumeErr)
				}
			} else {
				if !errors.Is(resumeErr, ErrProfileSessionRevocationUnconfirmed) || final.Status != "REPAIR_REQUIRED" || final.SessionsRevokedAt != nil {
					t.Fatal("uncertain logout was guessed successful", resumeErr, final.Status)
				}
				if claimed, err := r.ClaimProfileChange(ctx); err != nil || claimed != nil {
					t.Fatal("manual review automatically claimed", err)
				}
				var audits int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='account.profile_change.repair_required' AND resource_id=$1`, c.ID.String()).Scan(&audits); err != nil || audits != 1 {
					t.Fatal("missing/duplicate repair audit", err, audits)
				}
			}
			if d.calls.Load() != 1 {
				t.Fatal("retry repeated logout", d.calls.Load())
			}
			var starts int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='account.profile_change.revocation_started' AND resource_id=$1`, c.ID.String()).Scan(&starts); err != nil || starts != 1 {
				t.Fatal("dispatch grant audit", err, starts)
			}
		})
	}
	t.Run("production-dto-operation-and-subject-binding", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		c := historyProfile(t, r)
		if err := r.MarkProfileIdentityUpdated(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.CommitProfileChange(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		first, err := r.StartProfileSessionRevocation(ctx, c.ID)
		if err != nil || !first {
			t.Fatal("durable grant", err)
		}
		d := &historyLogoutDirectory{}
		client := historyAdapter(t, d)
		if err = client.RevokeProfileSessions(ctx, c.IdentitySubject, c.ID, first); err != nil {
			t.Fatal(err)
		}
		if err = client.RevokeProfileSessions(ctx, "other-subject", c.ID, false); !errors.Is(err, ErrProfileChangeConflict) {
			t.Fatal("subject mismatch borrowed command", err)
		}
		if err = client.RevokeProfileSessions(ctx, c.IdentitySubject, uuid.New(), false); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) {
			t.Fatal("new operation borrowed receipt", err)
		}
		if d.calls.Load() != 1 {
			t.Fatal("scope mismatch repeated remote effect", d.calls.Load())
		}
	})
	t.Run("first-grant-crash-before-http-fails-closed", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		c := historyProfile(t, r)
		if err := r.MarkProfileIdentityUpdated(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.CommitProfileChange(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		first, err := r.StartProfileSessionRevocation(ctx, c.ID)
		if err != nil || !first {
			t.Fatal("durable first grant", err)
		}
		// Crash after the durable grant but before HTTP leaves no adapter receipt.
		// The restarted sender cannot reconstruct whether dispatch happened.
		c, err = r.ProfileChange(ctx, c.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := &historyLogoutDirectory{}
		service := &ProfileChangeService{store: r, identities: historyAdapter(t, d), now: time.Now}
		if _, err = service.resume(ctx, c); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) {
			t.Fatal("restart guessed a fresh first attempt", err)
		}
		final, err := r.ProfileChange(ctx, c.ID, nil)
		if err != nil || final.Status != "REPAIR_REQUIRED" || d.calls.Load() != 0 {
			t.Fatal("first grant gap did not fail closed", err)
		}
	})
	t.Run("concurrent-durable-first-grant", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		c := historyProfile(t, r)
		if err := r.MarkProfileIdentityUpdated(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.CommitProfileChange(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		var firsts atomic.Int64
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				first, err := r.StartProfileSessionRevocation(ctx, c.ID)
				if err != nil {
					t.Error(err)
				}
				if first {
					firsts.Add(1)
				}
			}()
		}
		wg.Wait()
		if firsts.Load() != 1 {
			t.Fatal("durable grant repeated", firsts.Load())
		}
	})

	historyProfileAuditLockOrder(t, f)
}

// A newer forward-only migration must not hide the historical guard under test.
// Invoke the exact published DownSQL in the same owner identity and transaction
// used by the migrator; this is not a claim that DownOne can target old versions.
func assertHistoricalForwardOnlyMigration(t *testing.T, r *PostgresRepository, installed []migrations.Migration, version int64, message string) {
	t.Helper()
	var downSQL string
	for _, migration := range installed {
		if migration.Version == version {
			downSQL = migration.DownSQL
		}
	}
	if strings.TrimSpace(downSQL) == "" {
		t.Fatal("historical rollback SQL missing", version)
	}
	ctx := context.Background()
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User = "account_migrator"
	cfg.Password = ""
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("open historical rollback migrator", err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, "SET ROLE account_service"); err != nil {
		t.Fatal(err)
	}
	var actor, session string
	if err = conn.QueryRow(ctx, "SELECT current_user,session_user").Scan(&actor, &session); err != nil || actor != "account_service" || session != "account_migrator" {
		t.Fatal("historical rollback identity mismatch", err)
	}
	var before, after string
	ledgerSQL := "SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m"
	if err = conn.QueryRow(ctx, ledgerSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, downErr := tx.Exec(ctx, downSQL)
	rollbackErr := tx.Rollback(ctx)
	var refusal *pgconn.PgError
	if !errors.As(downErr, &refusal) || refusal.Code != "55000" || refusal.Message != message {
		t.Fatal("exact historical rollback guard missing", version, downErr)
	}
	if rollbackErr != nil {
		t.Fatal("historical rollback transaction cleanup", rollbackErr)
	}
	if err = conn.QueryRow(ctx, ledgerSQL).Scan(&after); err != nil || before != after {
		t.Fatal("historical rollback changed migration ledger", err)
	}
	if _, err = migrations.VerifyApplied(ctx, conn, installed); err != nil {
		t.Fatal("historical rollback changed migration identity", err)
	}
}
