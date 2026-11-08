package account

import (
	"bytes"
	"context"
	"errors"
	"net/http"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

type registrationUnknownCreateDirectory struct {
	identityadapter.UserDirectory
	id      uuid.UUID
	creates atomic.Int64
	visible atomic.Bool
}

func (d *registrationUnknownCreateDirectory) CreateMobileUser(_ context.Context, id uuid.UUID, _, _, _ string) (string, error) {
	if id != d.id {
		return "", errors.New("unexpected operation")
	}
	d.creates.Add(1)
	return "", errors.New("controlled unknown remote creation acknowledgement")
}
func (d *registrationUnknownCreateDirectory) FindMobileUserByRegistrationOperation(_ context.Context, id uuid.UUID) (string, bool, error) {
	if id != d.id {
		return "", false, errors.New("unexpected operation")
	}
	return "controlled-subject-" + id.String(), d.visible.Load(), nil
}

func registrationDispatchReady(t *testing.T, r *PostgresRepository) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := registrationBonusTestOperation(t, ctx, r, "dispatch"+strings.ReplaceAll(uuid.NewString(), "-", "")[:12], "+8613800138000")
	operation, err := r.RegistrationOperation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.MarkSMSChallengeSent(ctx, operation.ChallengeID, "controlled", nil); err != nil {
		t.Fatal(err)
	}
	if err = r.ConsumeRegistrationSMS(ctx, id, operation.ChallengeID, operation.Mobile, true); err != nil {
		t.Fatal(err)
	}
	if err = r.MarkRegistrationIdentityCreating(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRegistrationCreateDispatchRuntimePostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	t.Run("only-explicit-proof-at-exact-first-version-compensates", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		id := registrationDispatchReady(t, r)
		first, version, err := r.MarkRegistrationCreateAttempted(ctx, id)
		if err != nil || !first || version < 1 {
			t.Fatal(first, version, err)
		}
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		if err = r.CompensatePreIdentityRegistration(ctx, id, "PASSWORD_REFERENCE_EXPIRED", version); !errors.Is(err, ErrRegistrationConflict) {
			t.Fatal("generic expiry accepted", err)
		}
		if historyClaimSnapshot(t, r, "registration_operations", id) != before {
			t.Fatal("rejected proof wrote facts")
		}
		if err = r.CompensatePreIdentityRegistration(ctx, id, "IDENTITY_CREATE_NOT_DISPATCHED", version); err != nil {
			t.Fatal(err)
		}
		var status string
		var proof bool
		var storedVersion int64
		if err = r.pool.QueryRow(ctx, `SELECT status,compensation_evidence->>'authority'='IDENTITY_ADAPTER' AND (compensation_evidence->>'creation_never_dispatched')::boolean,(compensation_evidence->>'attempt_version')::bigint FROM registration_operations WHERE id=$1`, id).Scan(&status, &proof, &storedVersion); err != nil || status != "COMPENSATED" || !proof || storedVersion != version {
			t.Fatal("compensation evidence", status, proof, storedVersion, err)
		}
	})
	t.Run("concurrent-grants-one-first-and-later-version-defeats-compensation", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		id := registrationDispatchReady(t, r)
		type grant struct {
			first   bool
			version int64
			err     error
		}
		results := make(chan grant, 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				first, v, e := r.MarkRegistrationCreateAttempted(ctx, id)
				results <- grant{first, v, e}
			}()
		}
		wg.Wait()
		close(results)
		firsts := 0
		var firstVersion int64
		for g := range results {
			if g.err != nil {
				t.Fatal(g.err)
			}
			if g.first {
				firsts++
				firstVersion = g.version
			}
		}
		if firsts != 1 {
			t.Fatal("first grant count", firsts)
		}
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		if err := r.CompensatePreIdentityRegistration(ctx, id, "IDENTITY_CREATE_NOT_DISPATCHED", firstVersion); !errors.Is(err, ErrRegistrationConflict) {
			t.Fatal("later attempt hidden by stale first proof", err)
		}
		if before != historyClaimSnapshot(t, r, "registration_operations", id) {
			t.Fatal("stale proof changed facts")
		}
	})
	t.Run("detached-old-recovery-cannot-grant-or-compensate", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		id := registrationDispatchReady(t, r)
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		now = now.Add(6 * time.Second)
		old, err := r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second)
		if err != nil || len(old) != 1 {
			t.Fatal(err, len(old))
		}
		claim, cancel, err := withIdentityRecoveryClaim(ctx, "registration", id, old[0].RecoveryGeneration, old[0].RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		claim = context.WithoutCancel(claim)
		first, v, err := r.MarkRegistrationCreateAttempted(claim, id)
		if err != nil || !first {
			t.Fatal(first, err)
		}
		now = now.Add(31 * time.Second)
		if _, err = r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		if _, _, err = r.MarkRegistrationCreateAttempted(claim, id); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("old grant", err)
		}
		if err = r.CompensatePreIdentityRegistration(claim, id, "IDENTITY_CREATE_NOT_DISPATCHED", v); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("old compensate", err)
		}
		if before != historyClaimSnapshot(t, r, "registration_operations", id) {
			t.Fatal("old worker changed facts")
		}
	})
	t.Run("public-upgrade-keeps-old-facts-and-classifies-old-creating-unknown", func(t *testing.T) {
		ctx := context.Background()
		var id uuid.UUID
		var beforeRow, beforeLedger string
		r := f.repositoryWithInitialStage(t, func(ctx context.Context, c *pgx.Conn, installed []migrations.Migration) error {
			var old []migrations.Migration
			for _, m := range installed {
				if m.Version != 900008 {
					old = append(old, m)
				}
			}
			if err := accountschema.Up(ctx, c, old); err != nil {
				return err
			}
			cfg, err := pgxpool.ParseConfig("")
			if err != nil {
				return err
			}
			cfg.ConnConfig = c.Config().Copy()
			cfg.ConnConfig.User = "account_service"
			cfg.ConnConfig.Password = ""
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			legacy := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
			// These pre-create methods use exactly the published pre900008
			// columns; no impossible new-schema operation is forged in SQL.
			id = registrationBonusTestOperation(t, ctx, legacy, "oldcreating", "+8613800138000")
			var challenge uuid.UUID
			if err = pool.QueryRow(ctx, `SELECT sms_challenge_id FROM registration_operations WHERE id=$1`, id).Scan(&challenge); err != nil {
				return err
			}
			if err = legacy.MarkSMSChallengeSent(ctx, challenge, "controlled", nil); err != nil {
				return err
			}
			if err = legacy.ConsumeRegistrationSMS(ctx, id, challenge, "+8613800138000", true); err != nil {
				return err
			}
			if err = legacy.MarkRegistrationIdentityCreating(ctx, id); err != nil {
				return err
			}
			if err = pool.QueryRow(ctx, `SELECT to_jsonb(ro)::text FROM registration_operations ro WHERE id=$1`, id).Scan(&beforeRow); err != nil {
				return err
			}
			return pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&beforeLedger)
		})
		var afterRow, afterLedger string
		if err := r.pool.QueryRow(ctx, `SELECT (to_jsonb(ro)-'identity_create_attempted_at')::text FROM registration_operations ro WHERE id=$1`, id).Scan(&afterRow); err != nil {
			t.Fatal(err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900008`).Scan(&afterLedger); err != nil {
			t.Fatal(err)
		}
		if afterRow != beforeRow || afterLedger != beforeLedger {
			t.Fatal("upgrade rewrote published operation or ledger facts")
		}
		operation, err := r.RegistrationOperation(ctx, id)
		if err != nil || operation.IdentityCreateAttemptedAt == nil {
			t.Fatal("old creating granted fresh first", err)
		}
		if first, _, err := r.MarkRegistrationCreateAttempted(ctx, id); err != nil || first {
			t.Fatal("old ambiguous state became first", first, err)
		}
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		assertHistoricalForwardOnlyMigration(t, r, installed, 900008,
			"registration dispatch evidence is forward-only")
		var present bool
		if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='registration_operations' AND column_name='identity_create_attempted_at') AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=900008)`).Scan(&present); err != nil || !present {
			t.Fatal("failed down discarded evidence", present, err)
		}
	})
	t.Run("actual-register-http-adapter-and-worker-recover-late-identity-once", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		for _, key := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE"} {
			t.Setenv(key, "")
		}
		r := f.repository(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		if err := r.ConfigureRegistrationBonusFingerprint("controlled-dispatch", bytes.Repeat([]byte{7}, 32)); err != nil {
			t.Fatal(err)
		}
		id, challengeID := uuid.New(), uuid.New()
		directory := &registrationUnknownCreateDirectory{id: id}
		vault, err := identityadapter.NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := vault.Precheck("Controlled2026")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := identityadapter.NewService(vault, "controlled-dispatch-token")
		if err != nil {
			t.Fatal(err)
		}
		var createRequests atomic.Int64
		handler := adapter.Handler()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			if q.Method == http.MethodPost && q.URL.Path == "/internal/v1/users/mobile" {
				createRequests.Add(1)
			}
			handler.ServeHTTP(w, q)
		}))
		defer server.Close()
		client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-dispatch-token")
		if err != nil {
			t.Fatal(err)
		}
		signer, err := NewRegistrationTokenSigner([]byte(strings.Repeat("q", 32)))
		if err != nil {
			t.Fatal(err)
		}
		queryToken, _, err := signer.Issue(id)
		if err != nil {
			t.Fatal(err)
		}
		queryHash := registrationTokenHash(queryToken)
		fingerprint := registrationFingerprint("controlled", "+8613800138000", challengeID, ref.PolicyVersion)
		op := RegistrationOperation{ID: id, CandidateUserID: uuid.New(), QueryTokenHash: queryHash[:], RequestFingerprint: fingerprint[:], Username: "controlled", Mobile: "+8613800138000", ChallengeID: challengeID, PasswordPolicyVersion: ref.PolicyVersion, CreatedAt: now, UpdatedAt: now}
		challenge := SMSChallenge{ID: challengeID, Mobile: op.Mobile, Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Source: "controlled-dispatch", Device: uuid.NewString(), Provider: SMSProviderDescriptor{ID: "fake", AdapterVersion: "v1"}}
		if err = r.CreateRegistrationOperation(ctx, op, challenge); err != nil {
			t.Fatal(err)
		}
		if err = r.MarkSMSChallengeSent(ctx, challengeID, "controlled-message", []byte("verification")); err != nil {
			t.Fatal(err)
		}
		auth, err := NewMobileAuthService(r, &smsProviderFake{verified: true}, client, "https://controlled.invalid/realms/pinvou", signer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = auth.Register(ctx, id, queryToken, "", "", uuid.Nil, "123456", ref.Handle, ref.PolicyVersion); !errors.Is(err, ErrRegistrationRecovering) {
			t.Fatal("unknown first request", err)
		}
		worker := NewRegistrationRecoveryWorker(r, auth, 5*time.Second)
		now = now.Add(6 * time.Second)
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		unknown, err := r.RegistrationOperation(ctx, id)
		if err != nil || unknown.Status != "IDENTITY_CREATING" || unknown.UserID != nil || directory.creates.Load() != 1 || createRequests.Load() != 1 {
			t.Fatal("unknown retry compensated or dispatched again", unknown.Status, directory.creates.Load(), createRequests.Load(), err)
		}
		directory.visible.Store(true)
		now = now.Add(31 * time.Second)
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		ready, err := r.RegistrationOperation(ctx, id)
		var identities int
		if err == nil {
			err = r.pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE subject=$1`, "controlled-subject-"+id.String()).Scan(&identities)
		}
		if err != nil || ready.Status != "PROVISIONING" || ready.UserID == nil || identities != 1 || directory.creates.Load() != 1 || createRequests.Load() != 1 {
			t.Fatal("late identity did not finish exactly once", ready.Status, identities, directory.creates.Load(), createRequests.Load(), err)
		}
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if directory.creates.Load() != 1 {
			t.Fatal("terminal tick redispatched")
		}
	})
	for _, mode := range []string{"explicit-never-dispatched", "generic-reference-expired", "later-version-before-proof", "wrong-status-proof", "wrong-conflict-status-proof"} {
		t.Run("actual-service-proof-"+mode, func(t *testing.T) {
			// This controls the adapter outcome at HTTP. Its proof issuance and
			// replay rules use the real adapter tests; the SQL CAS here is real.
			t.Setenv("APP_ENV", "test")
			r := f.repository(t)
			ctx := context.Background()
			id := registrationDispatchReady(t, r)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if q.Method == "GET" {
					w.WriteHeader(404)
					w.Write([]byte(`{"code":"IDENTITY_REGISTRATION_NOT_FOUND"}`))
					return
				}
				if mode == "later-version-before-proof" {
					if _, _, err := r.MarkRegistrationCreateAttempted(ctx, id); err != nil {
						t.Error(err)
					}
				}
				if mode == "wrong-status-proof" {
					w.WriteHeader(503)
					w.Write([]byte(`{"code":"IDENTITY_CREATE_NOT_DISPATCHED"}`))
				} else if mode == "wrong-conflict-status-proof" {
					w.WriteHeader(409)
					w.Write([]byte(`{"code":"IDENTITY_CREATE_NOT_DISPATCHED"}`))
				} else if mode == "generic-reference-expired" {
					w.WriteHeader(410)
					w.Write([]byte(`{"code":"PASSWORD_REFERENCE_INVALID"}`))
				} else {
					w.WriteHeader(410)
					w.Write([]byte(`{"code":"IDENTITY_CREATE_NOT_DISPATCHED"}`))
				}
			}))
			defer server.Close()
			client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-proof-token")
			if err != nil {
				t.Fatal(err)
			}
			auth := newMobileAuthForTest(t, r, &smsProviderFake{}, client)
			op, err := r.RegistrationOperation(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, resultErr := auth.resumeRegistration(ctx, op)
			stored, err := r.RegistrationOperation(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "explicit-never-dispatched" {
				if !errors.Is(resultErr, ErrRegistrationRestart) || stored.Status != "COMPENSATED" {
					t.Fatal("explicit proof did not restart safely", stored.Status, resultErr)
				}
			} else if !errors.Is(resultErr, ErrRegistrationRecovering) || stored.Status != "IDENTITY_CREATING" {
				t.Fatal("unproved or stale proof terminated intent", stored.Status, resultErr)
			}
		})
	}
}
