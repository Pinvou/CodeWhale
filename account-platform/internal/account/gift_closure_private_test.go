package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real public migrations and runtime repositories, controlled SMS/Identity and
// loopback transport only. No fabricated ACTIVE pending-bonus state.
func TestGiftClosureProducerBoundaries(t *testing.T) {
	if os.Getenv("F08_ACCOUNT_URL") == "" || os.Getenv("F08_BILLING_URL") == "" || os.Getenv("F08_TOKEN_URL") == "" {
		t.Skip("explicit public closure Account/Billing/Token runtime fixture is required")
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, e := NewPostgresRepository(ctx, os.Getenv("F08_ACCOUNT_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	bp, e := pgxpool.New(ctx, os.Getenv("F08_BILLING_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer bp.Close()
	br := billing.NewPostgresRepositoryWithPool(bp)
	secret, e := token.NewLocalAESProvider("closure-fixture", bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	tr, e := token.NewPostgresRepository(ctx, os.Getenv("F08_TOKEN_URL"), secret)
	if e != nil {
		t.Fatal(e)
	}
	defer tr.Close()
	check := func(step string, e error) {
		t.Helper()
		if e != nil {
			t.Fatalf("%s: %v", step, e)
		}
	}
	check("configure real fingerprint", r.ConfigureRegistrationBonusFingerprint("f07-synthetic-key", bytes.Repeat([]byte{7}, 32)))
	readiness, e := r.PrepareRegistrationBonusEligibility(ctx)
	check("actual eligibility backfill", e)
	if readiness.Status != "READY" {
		t.Fatal("eligibility not READY")
	}
	fixtureToken := strings.Repeat("controlled-closure-authority-", 2)
	auth := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			if q.Header.Get("Authorization") != "Bearer "+fixtureToken {
				w.WriteHeader(401)
				return
			}
			h.ServeHTTP(w, q)
		})
	}
	as := httptest.NewServer(NewService(r, nil).WithServiceToken(fixtureToken).Handler(nil))
	defer as.Close()
	verifier, e := billing.NewHTTPRegistrationBonusEligibilityVerifier(as.URL, fixtureToken)
	check("readiness authority", e)
	actualBilling := billing.NewService(br).WithRegistrationBonusEligibilityVerifier(verifier).Handler(fixtureToken)
	var lastGiftStatus atomic.Int64
	var lost atomic.Bool
	lost.Store(true)
	bs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		out := httptest.NewRecorder()
		actualBilling.ServeHTTP(out, q)
		if q.URL.Path == "/internal/v1/registration-bonus-grants" {
			lastGiftStatus.Store(int64(out.Code))
		}
		if q.URL.Path == "/internal/v1/registration-bonus-grants" && out.Code >= 200 && out.Code < 300 && lost.Swap(false) {
			w.WriteHeader(503)
			return
		}
		for k, v := range out.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(out.Code)
		_, _ = w.Write(out.Body.Bytes())
	}))
	defer bs.Close()
	ts := httptest.NewServer(token.NewService(nil, nil, nil).WithManagement(tr, nil, token.ProviderURLPolicy{}, fixtureToken, nil).Handler(nil))
	defer ts.Close()
	ids := httptest.NewServer(auth(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodDelete {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(204)
	})))
	defer ids.Close()
	wallets, e := NewHTTPWalletProvisioner(bs.URL, fixtureToken)
	check("typed wallet client", e)
	bw := NewWalletProvisioningWorker(r, wallets, time.Second)
	ic, e := NewHTTPIdentityProvisioner(ids.URL, fixtureToken)
	check("identity client", e)
	bc, e := NewHTTPAccountClosureClient(bs.URL, fixtureToken, "BILLING")
	check("Billing closure client", e)
	tc, e := NewHTTPAccountClosureClient(ts.URL, fixtureToken, "TOKEN")
	check("Token closure client", e)
	cw := NewAccountClosureWorker(r, time.Second, bc, tc).WithCleanup(ic, bc, tc)
	seq := 0
	mobile := "+8613900088888"
	issuer := "https://identity.registration-bonus.test"
	signup := func() (RegistrationOperation, identity.Principal) {
		seq++
		now := time.Now().UTC()
		op := RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), QueryTokenHash: bytes.Repeat([]byte{1}, 32), RequestFingerprint: bytes.Repeat([]byte{2}, 32), Username: fmt.Sprintf("closuregift%d", seq), Mobile: mobile, ChallengeID: uuid.New(), PasswordPolicyVersion: "user-v1", CreatedAt: now, UpdatedAt: now}
		challenge := SMSChallenge{ID: op.ChallengeID, Mobile: op.Mobile, Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Source: uuid.NewString(), Device: uuid.NewString(), Provider: SMSProviderDescriptor{ID: "generated", AdapterVersion: "v1"}}
		check("real registration", r.CreateRegistrationOperation(ctx, op, challenge))
		check("controlled SMS acknowledged", r.MarkSMSChallengeSent(ctx, op.ChallengeID, "controlled-message", bytes.Repeat([]byte{3}, 32)))
		check("real SMS consume", r.ConsumeRegistrationSMS(ctx, op.ID, op.ChallengeID, op.Mobile, true))
		check("real identity creating", r.MarkRegistrationIdentityCreating(ctx, op.ID))
		subject := "closuregift-" + op.ID.String()
		check("controlled identity acknowledged", r.MarkRegistrationIdentityCreated(ctx, op.ID, subject))
		committed, e := r.CommitRegistration(ctx, op.ID, issuer)
		check("real registration commit", e)
		if committed.Status != "PROVISIONING" {
			t.Fatal("registration bypassed job")
		}
		return op, identity.Principal{Issuer: issuer, Subject: subject, SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
	}
	op, p := signup()
	var permanentAccount, permanentBilling string
	t.Run("lost-gift-ACK-keeps-PROVISIONING-and-denies-closure", func(t *testing.T) {
		check("real lost ACK cycle", bw.RunOnce(ctx))
		var status, mode string
		var fp []byte
		check("real pending tuple", r.pool.QueryRow(ctx, `SELECT u.status,j.registration_bonus_mode,j.registration_bonus_fingerprint FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1`, op.CandidateUserID).Scan(&status, &mode, &fp))
		if status != "PROVISIONING" || mode != "EVALUATE" || len(fp) != 32 {
			t.Fatal("lost ACK cleared job or activated")
		}
		var n int
		check("actual committed disabled receipt", bp.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE registration_operation_id=$1 AND outcome='NOT_GRANTED_DISABLED' AND granted_microcredits=0`, op.ID).Scan(&n))
		if n != 1 {
			var diagnostic string
			check("safe provisioning diagnostic", r.pool.QueryRow(ctx, `SELECT COALESCE(last_error_code,'NONE') FROM wallet_provisioning_jobs WHERE user_id=$1`, op.CandidateUserID).Scan(&diagnostic))
			t.Fatalf("lost ACK did not commit actual zero receipt; gift_http_status=%d safe_job_code=%v", lastGiftStatus.Load(), diagnostic)
		}
		_, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), accountClosureConfirmation)
		if !errors.Is(e, ErrConflict) {
			t.Fatalf("pending signup closure accepted: %v", e)
		}
		check("no closure row", r.pool.QueryRow(ctx, `SELECT count(*) FROM account_closure_requests WHERE user_id=$1`, op.CandidateUserID).Scan(&n))
		if n != 0 {
			t.Fatal("pending closure mutated")
		}
	})
	var completedJob WalletProvisioningJob
	t.Run("same-gift-replay-completes-then-clears-only-temporary-facts", func(t *testing.T) {
		_, e := r.pool.Exec(ctx, `UPDATE wallet_provisioning_jobs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND status='RETRY'`, op.CandidateUserID)
		check("retry schedule only", e)
		jobs, e := r.LeaseWalletProvisioningJobs(ctx, 20, 30*time.Second)
		check("actual retry lease", e)
		if len(jobs) != 1 || jobs[0].UserID != op.CandidateUserID {
			t.Fatal("unexpected actual lease")
		}
		completedJob = jobs[0]
		check("actual frozen replay", wallets.ProvisionRegistrationWallet(ctx, RegistrationWalletProvisionRequest{UserID: op.CandidateUserID, RegistrationOperationID: op.ID, FingerprintKeyID: completedJob.FingerprintKeyID, MobileFingerprint: fmt.Sprintf("%x", completedJob.MobileFingerprint)}))
		if _, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), accountClosureConfirmation); !errors.Is(e, ErrConflict) {
			t.Fatalf("before COMPLETE closure accepted: %v", e)
		}
		check("fresh lease complete", r.CompleteWalletProvisioning(ctx, completedJob))
		var u, j, m string
		var key *string
		var fp []byte
		check("actual activated completion", r.pool.QueryRow(ctx, `SELECT u.status,j.status,j.registration_bonus_mode,j.registration_bonus_fingerprint_key_id,j.registration_bonus_fingerprint FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1`, op.CandidateUserID).Scan(&u, &j, &m, &key, &fp))
		if u != "ACTIVE" || j != "COMPLETED" || m != "NOT_APPLICABLE" || key != nil || fp != nil {
			t.Fatal("completion did not atomically scrub temporary tuple")
		}
		check("permanent eligibility snapshot", r.pool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM registration_bonus_mobile_eligibility e WHERE first_user_id=$1`, op.CandidateUserID).Scan(&permanentAccount))
		check("permanent claim/receipt snapshot", bp.QueryRow(ctx, `SELECT jsonb_build_object('claim',to_jsonb(c),'receipt',to_jsonb(r))::text FROM registration_bonus_claims c JOIN registration_bonus_receipts r ON r.claim_id=c.id WHERE r.user_id=$1`, op.CandidateUserID).Scan(&permanentBilling))
	})
	t.Run("actual-closure-anonymizes-while-permanent-eligibility-and-claim-survive", func(t *testing.T) {
		item, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), accountClosureConfirmation)
		check("actual ACTIVE closure", e)
		cw.runOnce(ctx)
		var stage, status string
		check("actual anonymous stage", r.pool.QueryRow(ctx, `SELECT q.stage,u.status FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, item.ID).Scan(&stage, &status))
		if stage != "ANONYMIZED" || status != "DELETED" {
			t.Fatalf("closure stage=%s status=%s", stage, status)
		}
		var a, b string
		check("permanent eligibility retained", r.pool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM registration_bonus_mobile_eligibility e WHERE first_user_id=$1`, op.CandidateUserID).Scan(&a))
		check("permanent claim retained", bp.QueryRow(ctx, `SELECT jsonb_build_object('claim',to_jsonb(c),'receipt',to_jsonb(r))::text FROM registration_bonus_claims c JOIN registration_bonus_receipts r ON r.claim_id=c.id WHERE r.user_id=$1`, op.CandidateUserID).Scan(&b))
		if a != permanentAccount || b != permanentBilling {
			t.Fatal("irreversible first-registration facts changed")
		}
		var clean bool
		check("temporary PII gone", r.pool.QueryRow(ctx, `SELECT u.mobile_e164 LIKE 'closed:%' AND o.mobile_e164 LIKE 'closed:%' AND j.registration_bonus_fingerprint_key_id IS NULL AND j.registration_bonus_fingerprint IS NULL FROM users u JOIN registration_operations o ON o.committed_user_id=u.id JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1`, op.CandidateUserID).Scan(&clean))
		if !clean {
			t.Fatal("temporary source PII remains")
		}
	})
	t.Run("late-old-job-cannot-activate-or-grant-after-closure", func(t *testing.T) {
		if e := r.CompleteWalletProvisioning(ctx, completedJob); !errors.Is(e, ErrConflict) {
			t.Fatalf("old completed lease accepted: %v", e)
		}
		check("worker after closure", bw.RunOnce(ctx))
		var status string
		check("closed status", r.pool.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, op.CandidateUserID).Scan(&status))
		if status != "DELETED" {
			t.Fatal("late worker revived")
		}
		var b string
		check("permanent receipt unchanged", bp.QueryRow(ctx, `SELECT jsonb_build_object('claim',to_jsonb(c),'receipt',to_jsonb(r))::text FROM registration_bonus_claims c JOIN registration_bonus_receipts r ON r.claim_id=c.id WHERE r.user_id=$1`, op.CandidateUserID).Scan(&b))
		if b != permanentBilling {
			t.Fatal("late worker changed claim")
		}
	})
	t.Run("same-mobile-new-registration-cannot-consume-gift-again", func(t *testing.T) {
		next, _ := signup()
		var mode string
		check("permanent first-registration mode", r.pool.QueryRow(ctx, `SELECT registration_bonus_mode FROM wallet_provisioning_jobs WHERE user_id=$1`, next.CandidateUserID).Scan(&mode))
		if mode != "NOT_APPLICABLE" {
			t.Fatal("closed mobile regained first-registration gift")
		}
		check("second ordinary wallet worker", bw.RunOnce(ctx))
		var n int
		check("no second gift receipt", bp.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE user_id=$1`, next.CandidateUserID).Scan(&n))
		if n != 0 {
			t.Fatal("second gift consumed")
		}
	})
}
