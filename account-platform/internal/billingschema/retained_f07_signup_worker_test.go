package billingschema

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// Private DRAFT assembly only. External SMS/Identity acknowledgements use the
// actual repository transitions but controlled authority, not a real provider.
func TestF07SignupWorkerDurableGift(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("dedicated owned Account/Billing PostgreSQL fixture is not configured")
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, databaseURL)
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	check := func(t *testing.T, step string, err error) {
		t.Helper()
		if err != nil {
			state := "non-SQL error"
			if v, ok := err.(interface{ SQLState() string }); ok {
				state = v.SQLState()
			}
			t.Fatalf("%s failed: %s", step, state)
		}
	}
	am, err := migrations.Load("../../migrations/account")
	check(t, "load Account", err)
	check(t, "public Account Up", accountschema.Up(ctx, ac, am))
	_, err = migrations.VerifyApplied(ctx, ac, am)
	check(t, "Account Verify", err)
	bm, err := migrations.Load("../../migrations/billing")
	check(t, "load Billing", err)
	check(t, "public Billing Up", Up(ctx, bc, bm))
	_, err = migrations.VerifyApplied(ctx, bc, bm)
	check(t, "Billing Verify", err)
	ar, err := account.NewPostgresRepository(ctx, f.serviceURL(t, ac, "account_service"))
	check(t, "Account repository", err)
	defer ar.Close()
	br, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, bc, "billing_service"))
	check(t, "Billing repository", err)
	defer br.Close()
	check(t, "configure synthetic fingerprint", ar.ConfigureRegistrationBonusFingerprint("f07-synthetic-key", bytes.Repeat([]byte{7}, 32)))
	ready, err := ar.PrepareRegistrationBonusEligibility(ctx)
	check(t, "prepare real eligibility", err)
	if ready.Status != "READY" || ready.CompletedAt == nil || ready.RecoverableRegistrationCount != 0 || ready.UnrecoverableRegistrationCount != 0 {
		t.Fatal("fresh eligibility not real READY")
	}
	const token = "f07-controlled-workload"
	as := httptest.NewServer(account.NewService(ar, nil).WithServiceToken(token).Handler(nil))
	defer as.Close()
	verifier, err := billing.NewHTTPRegistrationBonusEligibilityVerifier(as.URL, token)
	check(t, "real HTTP readiness constructor", err)
	check(t, "real Account HTTP READY", verifier.RequireReady(ctx))
	actual := billing.NewService(br).WithRegistrationBonusEligibilityVerifier(verifier).Handler(token)
	var mode atomic.Int32
	var controlledFailures atomic.Int32
	bs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		actual.ServeHTTP(rec, r)
		if r.URL.Path == "/internal/v1/registration-bonus-grants" && rec.Code >= 200 && rec.Code < 300 && mode.CompareAndSwap(1, 0) {
			controlledFailures.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		if r.URL.Path == "/internal/v1/wallets/ensure" && rec.Code == http.StatusOK && mode.CompareAndSwap(2, 0) {
			controlledFailures.Add(1)
			var dto map[string]any
			dec := json.NewDecoder(rec.Body)
			dec.UseNumber()
			if err := dec.Decode(&dto); err != nil {
				w.WriteHeader(500)
				return
			}
			dto["owner_id"] = uuid.NewString()
			w.WriteHeader(rec.Code)
			_ = json.NewEncoder(w).Encode(dto)
			return
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer bs.Close()
	client, err := account.NewHTTPWalletProvisioner(bs.URL, token)
	check(t, "real typed wallet client", err)
	worker := account.NewWalletProvisioningWorker(ar, client, time.Second)
	sequence := 0
	register := func(t *testing.T) account.RegistrationOperation {
		t.Helper()
		sequence++
		now := time.Now().UTC()
		op := account.RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), QueryTokenHash: bytes.Repeat([]byte{1}, 32), RequestFingerprint: bytes.Repeat([]byte{2}, 32), Username: fmt.Sprintf("f07user%d", sequence), Mobile: fmt.Sprintf("+861390000%04d", sequence), ChallengeID: uuid.New(), PasswordPolicyVersion: "user-v1", CreatedAt: now, UpdatedAt: now}
		challenge := account.SMSChallenge{ID: op.ChallengeID, Mobile: op.Mobile, Purpose: account.SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Source: uuid.NewString(), Device: uuid.NewString(), Provider: account.SMSProviderDescriptor{ID: "generated", AdapterVersion: "v1"}}
		check(t, "create actual registration", ar.CreateRegistrationOperation(ctx, op, challenge))
		check(t, "controlled SMS send acknowledged", ar.MarkSMSChallengeSent(ctx, op.ChallengeID, "controlled-message", bytes.Repeat([]byte{3}, 32)))
		check(t, "consume actual SMS transition", ar.ConsumeRegistrationSMS(ctx, op.ID, op.ChallengeID, op.Mobile, true))
		check(t, "begin actual identity transition", ar.MarkRegistrationIdentityCreating(ctx, op.ID))
		check(t, "controlled identity acknowledged", ar.MarkRegistrationIdentityCreated(ctx, op.ID, "f07-subject-"+op.ID.String()))
		committed, err := ar.CommitRegistration(ctx, op.ID, "https://identity.registration-bonus.test")
		check(t, "actual CommitRegistration", err)
		if committed.Status != "PROVISIONING" || committed.UserID == nil || *committed.UserID != op.CandidateUserID {
			t.Fatal("commit bypassed provisioning")
		}
		var state, bonus, key string
		var fingerprint []byte
		var operation uuid.UUID
		check(t, "frozen job tuple", ac.QueryRow(ctx, `SELECT status,registration_bonus_mode,registration_bonus_fingerprint_key_id,registration_bonus_fingerprint,registration_operation_id FROM wallet_provisioning_jobs WHERE user_id=$1`, op.CandidateUserID).Scan(&state, &bonus, &key, &fingerprint, &operation))
		if state != "PENDING" || bonus != "EVALUATE" || key != "f07-synthetic-key" || len(fingerprint) != 32 || operation != op.ID {
			t.Fatal("signup lost frozen eligible tuple")
		}
		return op
	}
	verifyCompleted := func(t *testing.T, op account.RegistrationOperation, outcome string, amount int64) {
		t.Helper()
		var userStatus, operationStatus, jobStatus, bonus string
		var completed *time.Time
		var key *string
		var fp []byte
		check(t, "actual Account completion facts", ac.QueryRow(ctx, `SELECT u.status,o.status,j.status,j.completed_at,j.registration_bonus_mode,j.registration_bonus_fingerprint_key_id,j.registration_bonus_fingerprint FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id JOIN registration_operations o ON o.id=j.registration_operation_id WHERE u.id=$1`, op.CandidateUserID).Scan(&userStatus, &operationStatus, &jobStatus, &completed, &bonus, &key, &fp))
		if userStatus != "ACTIVE" || operationStatus != "ACTIVE" || jobStatus != "COMPLETED" || completed == nil || bonus != "NOT_APPLICABLE" || key != nil || fp != nil {
			t.Fatalf("completion states user=%s operation=%s job=%s completed=%t bonus=%s keyAbsent=%t fingerprintAbsent=%t", userStatus, operationStatus, jobStatus, completed != nil, bonus, key == nil, fp == nil)
		}
		var wallet, claim uuid.UUID
		var revision, granted, posted int64
		var got, owner, status string
		check(t, "durable Billing receipt/wallet", bc.QueryRow(ctx, `SELECT r.wallet_id,r.claim_id,r.policy_revision,r.outcome,r.granted_microcredits,w.posted_microcredits,w.owner_type,w.status FROM registration_bonus_receipts r JOIN wallets w ON w.id=r.wallet_id AND w.owner_id=r.user_id WHERE r.registration_operation_id=$1 AND r.user_id=$2`, op.ID, op.CandidateUserID).Scan(&wallet, &claim, &revision, &got, &granted, &posted, &owner, &status))
		if wallet == uuid.Nil || claim == uuid.Nil || revision < 1 || got != outcome || granted != amount || posted != amount || owner != "PERSONAL" || status != "ACTIVE" {
			t.Fatal("actual gift/owner/balance facts differ")
		}
		var requested, done, personal int
		check(t, "predecessor events and default role", ac.QueryRow(ctx, `SELECT (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.wallet.provisioning.requested.v1' AND aggregate_version=1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.wallet.provisioning.completed.v1' AND aggregate_version=2),(SELECT count(*) FROM role_assignments WHERE user_id=$1 AND role_code='PERSONAL_USER')`, op.CandidateUserID).Scan(&requested, &done, &personal))
		if requested != 1 || done != 1 || personal != 1 {
			t.Fatal("F01 job events or personal role not preserved")
		}
		check(t, "duplicate worker cycle", worker.RunOnce(ctx))
		var count int
		check(t, "receipt once", bc.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE registration_operation_id=$1`, op.ID).Scan(&count))
		if count != 1 {
			t.Fatal("duplicate receipt")
		}
	}
	t.Run("default-disabled-signup-worker-completes-real-zero-wallet", func(t *testing.T) {
		op := register(t)
		check(t, "worker", worker.RunOnce(ctx))
		verifyCompleted(t, op, "NOT_GRANTED_DISABLED", 0)
	})
	actor := uuid.New()
	policy, _, err := br.UpdateRegistrationBonusPolicy(ctx, actor, true, 0, 30, 1, "controlled valid reason", "f07-worker-zero-key")
	check(t, "enable zero policy", err)
	t.Run("enabled-zero-signup-worker-completes", func(t *testing.T) {
		op := register(t)
		check(t, "worker", worker.RunOnce(ctx))
		verifyCompleted(t, op, "NOT_GRANTED_ZERO", 0)
	})
	const amount = int64(9007199254750000)
	policy, _, err = br.UpdateRegistrationBonusPolicy(ctx, actor, true, amount, 30, policy.Revision, "controlled valid reason", "f07-worker-grant-key")
	check(t, "enable grant policy", err)
	t.Run("granted-signup-worker-exact-package-ledger", func(t *testing.T) {
		op := register(t)
		check(t, "worker", worker.RunOnce(ctx))
		verifyCompleted(t, op, "GRANTED", amount)
		var packages, postings int
		var sum int64
		var refundable bool
		check(t, "actual nonrefundable package and balanced journal", bc.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_packages WHERE wallet_id=r.wallet_id),(SELECT count(*) FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference='registration-bonus:'||r.claim_id::text),(SELECT COALESCE(sum(p.delta_microcredits),0)::bigint FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference='registration-bonus:'||r.claim_id::text),(SELECT refundable FROM credit_lots WHERE wallet_id=r.wallet_id) FROM registration_bonus_receipts r WHERE r.registration_operation_id=$1`, op.ID).Scan(&packages, &postings, &sum, &refundable))
		if packages != 1 || postings != 2 || sum != 0 || refundable {
			t.Fatal("actual granted package/journal not exact once")
		}
	})
	for _, tc := range []struct {
		name string
		mode int32
	}{{"committed-gift-response-lost-keeps-job-and-same-operation", 1}, {"wrong-wallet-owner-ack-cannot-complete", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			op := register(t)
			mode.Store(tc.mode)
			before := controlledFailures.Load()
			check(t, "controlled failing worker cycle", worker.RunOnce(ctx))
			if controlledFailures.Load() != before+1 {
				t.Fatal("controlled real response boundary not reached")
			}
			var state, bonus, key string
			var fp []byte
			check(t, "failure preserves actual job tuple", ac.QueryRow(ctx, `SELECT status,registration_bonus_mode,registration_bonus_fingerprint_key_id,registration_bonus_fingerprint FROM wallet_provisioning_jobs WHERE user_id=$1`, op.CandidateUserID).Scan(&state, &bonus, &key, &fp))
			if state != "RETRY" || bonus != "EVALUATE" || key != "f07-synthetic-key" || len(fp) != 32 {
				t.Fatal("unknown ACK completed or lost retry facts")
			}
			var userStatus string
			check(t, "not yet ACTIVE", ac.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, op.CandidateUserID).Scan(&userStatus))
			if userStatus != "PROVISIONING" {
				t.Fatal("unknown ACK activated user")
			}
			var receipts int
			check(t, "first HTTP attempt really committed gift", bc.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE registration_operation_id=$1 AND outcome='GRANTED'`, op.ID).Scan(&receipts))
			if receipts != 1 {
				t.Fatal("failure fixture did not commit real grant")
			}
			_, err = ac.Exec(ctx, `UPDATE wallet_provisioning_jobs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND status='RETRY'`, op.CandidateUserID)
			check(t, "controlled retry scheduling only", err)
			check(t, "same operation retry worker", worker.RunOnce(ctx))
			verifyCompleted(t, op, "GRANTED", amount)
		})
	}
	t.Run("ordinary-bootstrap-nonbonus-job-still-completes", func(t *testing.T) {
		principal := identity.Principal{Issuer: "https://identity.registration-bonus.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "f07ordinary", AuthenticatedAt: time.Now().UTC()}
		me, created, err := ar.Bootstrap(ctx, principal)
		check(t, "actual ordinary Bootstrap", err)
		if !created || me.Status != "PROVISIONING" {
			t.Fatal("ordinary Bootstrap fixture not a real pending job")
		}
		var status, bonus string
		var key *string
		var fp []byte
		var op *uuid.UUID
		check(t, "ordinary nullable job facts", ac.QueryRow(ctx, `SELECT status,registration_bonus_mode,registration_bonus_fingerprint_key_id,registration_bonus_fingerprint,registration_operation_id FROM wallet_provisioning_jobs WHERE user_id=$1`, me.UserID).Scan(&status, &bonus, &key, &fp, &op))
		if status != "PENDING" || bonus != "NOT_APPLICABLE" || key != nil || fp != nil || op != nil {
			t.Fatal("ordinary job should carry no bonus tuple")
		}
		check(t, "ordinary job real worker", worker.RunOnce(ctx))
		check(t, "ordinary job durable complete", ac.QueryRow(ctx, `SELECT u.status,j.status FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1`, me.UserID).Scan(&status, &bonus))
		if status != "ACTIVE" || bonus != "COMPLETED" {
			t.Fatal("ordinary pending wallet job did not activate")
		}
		var receipts int
		check(t, "ordinary job no bonus receipt", bc.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE user_id=$1`, me.UserID).Scan(&receipts))
		if receipts != 0 {
			t.Fatal("ordinary job received unrequested gift")
		}
	})
}
