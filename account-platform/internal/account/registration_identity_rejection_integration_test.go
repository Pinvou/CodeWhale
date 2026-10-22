package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type rejectedRegistrationDirectory struct {
	identityadapter.UserDirectory
	createCalls  atomic.Int64
	disableCalls atomic.Int64
	onCreate     func()
}

func (d *rejectedRegistrationDirectory) CreateMobileUser(context.Context, uuid.UUID, string, string, string) (string, error) {
	d.createCalls.Add(1)
	if d.onCreate != nil {
		d.onCreate()
	}
	return "", identityadapter.ErrUsernameAlreadyRegistered
}
func (*rejectedRegistrationDirectory) FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error) {
	return "", false, nil
}
func (d *rejectedRegistrationDirectory) DisableUserAndConfirm(context.Context, string) error {
	d.disableCalls.Add(1)
	return errors.New("unexpected disable without owned identity")
}

func TestRegistrationIdentityRejectionRuntimePostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	for _, mode := range []string{"original", "lookup-backoff", "late-find"} {
		name := "typed-conflict-restart-original-key-and-authorized-repair"
		if mode == "lookup-backoff" {
			name = "typed-conflict-after-concurrent-lookup-backoff"
		}
		if mode == "late-find" {
			name = "typed-conflict-before-inflight-ordinary-find-returns"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("APP_ENV", "test")
			for _, key := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE"} {
				t.Setenv(key, "")
			}
			r := f.repository(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			r.now = func() time.Time { return now }
			directory := &rejectedRegistrationDirectory{}
			vault, err := identityadapter.NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := vault.Precheck("Controlled2026")
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := identityadapter.NewService(vault, "controlled-rejection-token")
			if err != nil {
				t.Fatal(err)
			}
			var httpCreates, httpFinds atomic.Int64
			findBlocked, releaseFind := make(chan struct{}), make(chan struct{})
			lateFinished := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Method == http.MethodGet && mode == "late-find" && httpFinds.Add(1) == 2 {
					close(findBlocked)
					select {
					case <-releaseFind:
					case <-q.Context().Done():
						return
					}
				}
				if q.Method == http.MethodPost && q.URL.Path == "/internal/v1/users/mobile" {
					httpCreates.Add(1)
				}
				adapter.Handler().ServeHTTP(w, q)
			}))
			defer server.Close()
			client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-rejection-token")
			if err != nil {
				t.Fatal(err)
			}
			signer, err := NewRegistrationTokenSigner([]byte(strings.Repeat("q", 32)))
			if err != nil {
				t.Fatal(err)
			}
			id, challengeID := uuid.New(), uuid.New()
			queryToken, _, err := signer.Issue(id)
			if err != nil {
				t.Fatal(err)
			}
			hash := registrationTokenHash(queryToken)
			fingerprint := registrationFingerprint("rejected", "+8613800138000", challengeID, ref.PolicyVersion)
			op := RegistrationOperation{ID: id, CandidateUserID: uuid.New(), QueryTokenHash: hash[:], RequestFingerprint: fingerprint[:], Username: "rejected", Mobile: "+8613800138000", ChallengeID: challengeID, PasswordPolicyVersion: ref.PolicyVersion, CreatedAt: now, UpdatedAt: now}
			challenge := SMSChallenge{ID: challengeID, Mobile: op.Mobile, Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Source: "controlled-rejection", Device: uuid.NewString(), Provider: SMSProviderDescriptor{ID: "fake", AdapterVersion: "v1"}}
			if err = r.CreateRegistrationOperation(ctx, op, challenge); err != nil {
				t.Fatal(err)
			}
			if err = r.MarkSMSChallengeSent(ctx, challengeID, "controlled", []byte("verification")); err != nil {
				t.Fatal(err)
			}
			auth, err := NewMobileAuthService(r, &smsProviderFake{verified: true}, client, "https://identity.example.invalid/realms/pinvou", signer)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "lookup-backoff" {
				directory.onCreate = func() {
					now = now.Add(6 * time.Second)
					if err := NewRegistrationRecoveryWorker(r, auth, time.Second).RunOnce(ctx); err != nil {
						t.Error(err)
					}
				}
			}
			if mode == "late-find" {
				directory.onCreate = func() {
					inflight, readErr := r.RegistrationOperation(ctx, id)
					if readErr != nil {
						t.Error(readErr)
						return
					}
					go func() { _, lateErr := auth.resumeRegistration(ctx, inflight); lateFinished <- lateErr }()
					select {
					case <-findBlocked:
					case <-time.After(5 * time.Second):
						t.Error("ordinary second request never reached real HTTP lookup barrier")
					}
				}
			}
			result, err := auth.Register(ctx, id, queryToken, "", "", uuid.Nil, "123456", ref.Handle, ref.PolicyVersion)
			if mode == "late-find" {
				// First Create's canonical rejection is committed before the older request's
				// lookup is allowed to return 404 and execute detached ScheduleRetry.
				close(releaseFind)
				select {
				case lateErr := <-lateFinished:
					if !errors.Is(lateErr, ErrRegistrationRecovering) {
						t.Fatal(lateErr)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("late lookup did not complete")
				}
				var statusErr error
				result, statusErr = auth.RegistrationStatus(ctx, id, queryToken)
				if statusErr != nil {
					t.Fatal(statusErr)
				}
			}
			if !errors.Is(err, ErrRegistrationRecovering) || result.Status != "REPAIR_REQUIRED" || !result.ManualReviewRequired || result.Reason != "USERNAME_ALREADY_REGISTERED" {
				t.Fatal("typed rejection not visible", result, err)
			}
			stored, err := r.RegistrationOperation(ctx, id)
			if err != nil || stored.IdentityCreateAttemptedAt == nil || stored.IdentityCreatedAt != nil || stored.IdentitySubject != "" || stored.UserID != nil {
				t.Fatal("rejection invented identity/account facts", stored, err)
			}
			firstAt := *stored.IdentityCreateAttemptedAt
			var audits int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='registration.identity.create.rejected' AND result='FAILED' AND change_summary->>'manual_review_required'='true'`, id.String()).Scan(&audits); err != nil || audits != 1 {
				t.Fatal("missing durable rejection audit", audits, err)
			}
			// A restarted service/worker never obtains another create grant.
			restarted, err := NewMobileAuthService(r, &smsProviderFake{verified: true}, client, auth.issuer, signer)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if err = NewRegistrationRecoveryWorker(r, restarted, time.Second).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			result, err = restarted.Register(ctx, id, queryToken, "", "", uuid.Nil, "123456", "expired-or-lost-reference", ref.PolicyVersion)
			if !errors.Is(err, ErrRegistrationRecovering) || result.Status != "REPAIR_REQUIRED" || !result.ManualReviewRequired || result.OperationID != id {
				t.Fatal("original operation did not recover its repair receipt", result, err)
			}
			principal, _, _ := userCodeActor(t, r, "registration-repair")
			h := NewService(r, nil).WithMobileAuth(restarted, "controlled-account-token").Handler(func(_ context.Context, token string) (identity.Principal, error) {
				if token != "operator" {
					return identity.Principal{}, errors.New("untrusted fixture credential")
				}
				return principal, nil
			})
			request := func(method, path, token string) *httptest.ResponseRecorder {
				q := httptest.NewRequest(method, path, nil)
				q.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, q)
				return w
			}
			list := request(http.MethodGet, "/admin/v1/registration-operations", "operator")
			if list.Code != 200 || !strings.Contains(list.Body.String(), id.String()) || !strings.Contains(list.Body.String(), `"reason":"USERNAME_ALREADY_REGISTERED"`) || !strings.Contains(list.Body.String(), `"compensation_allowed":false`) {
				t.Fatal("authorized queue hides rejection or allows compensation", list.Code, list.Body.String())
			}
			if bad := request(http.MethodGet, "/admin/v1/registration-operations", "untrusted"); bad.Code == 200 {
				t.Fatal("repair queue is public")
			}
			retry := request(http.MethodPost, "/admin/v1/registration-operations/"+id.String()+"/actions/retry", "operator")
			if retry.Code != 202 || !strings.Contains(retry.Body.String(), `"manual_review_required":true`) {
				t.Fatal("manual retry falsely completed or hid repair", retry.Code, retry.Body.String())
			}
			compensate := request(http.MethodPost, "/admin/v1/registration-operations/"+id.String()+"/actions/compensate", "operator")
			if compensate.Code != 409 || !strings.Contains(compensate.Body.String(), "REGISTRATION_COMPENSATION_FORBIDDEN") {
				t.Fatal("rejection bypassed compensation proof", compensate.Code, compensate.Body.String())
			}
			stored, err = r.RegistrationOperation(ctx, id)
			if err != nil || stored.Status != "REPAIR_REQUIRED" || stored.IdentityCreateAttemptedAt == nil || !stored.IdentityCreateAttemptedAt.Equal(firstAt) || directory.createCalls.Load() != 1 || httpCreates.Load() != 1 || directory.disableCalls.Load() != 0 {
				t.Fatal("restart/admin action redispatched or changed grant", stored.Status, directory.createCalls.Load(), httpCreates.Load(), directory.disableCalls.Load(), err)
			}
			// State remains a reservation until independent controlled review; no new cancel policy.
			copy := op
			copy.ID, copy.CandidateUserID, copy.ChallengeID = uuid.New(), uuid.New(), uuid.New()
			challenge.ID = copy.ChallengeID
			if err := r.CreateRegistrationOperation(ctx, copy, challenge); !errors.Is(err, ErrUsernameAlreadyRegistered) && !errors.Is(err, ErrMobileAlreadyRegistered) {
				t.Fatal("repair silently released reservation", err)
			}
		})
	}
	for _, outcome := range []struct {
		name   string
		status int
		code   string
	}{
		{"generic-conflict", 409, "CONFLICT"}, {"typed-code-wrong-status", 503, "USERNAME_ALREADY_REGISTERED"}, {"unknown-ack", 503, "UNAVAILABLE"},
	} {
		t.Run(outcome.name+"-remains-unknown-no-compensation", func(t *testing.T) {
			t.Setenv("APP_ENV", "test")
			r := f.repository(t)
			ctx := context.Background()
			id := registrationDispatchReady(t, r)
			var creates atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Method == http.MethodGet {
					w.WriteHeader(404)
					w.Write([]byte(`{"code":"IDENTITY_REGISTRATION_NOT_FOUND"}`))
					return
				}
				creates.Add(1)
				w.WriteHeader(outcome.status)
				json.NewEncoder(w).Encode(map[string]string{"code": outcome.code})
			}))
			defer server.Close()
			client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-unknown-token")
			if err != nil {
				t.Fatal(err)
			}
			auth := newMobileAuthForTest(t, r, &smsProviderFake{}, client)
			op, err := r.RegistrationOperation(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, err = auth.resumeRegistration(ctx, op)
			if !errors.Is(err, ErrRegistrationRecovering) {
				t.Fatal(err)
			}
			op, err = r.RegistrationOperation(ctx, id)
			if err != nil || op.Status != "IDENTITY_CREATING" || op.IdentityCreateAttemptedAt == nil {
				t.Fatal("unknown terminated or became repair proof", op.Status, err)
			}
			if _, err = auth.resumeRegistration(ctx, op); !errors.Is(err, ErrRegistrationRecovering) {
				t.Fatal(err)
			}
			if creates.Load() != 1 {
				t.Fatal("unknown repeated create", creates.Load())
			}
		})
	}
	t.Run("non-rejection-repair-retains-existing-retry-behavior", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		id := registrationDispatchReady(t, r)
		if err := r.MarkRegistrationIdentityCreated(ctx, id, "controlled-known-subject"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(31 * time.Minute)
		if err := r.ScheduleRegistrationRetry(ctx, id, "ACCOUNT_COMMIT_FAILED"); err != nil {
			t.Fatal(err)
		}
		before, err := r.RegistrationOperation(ctx, id)
		if err != nil || before.Status != "REPAIR_REQUIRED" {
			t.Fatal(before.Status, err)
		}
		if err = r.ScheduleRegistrationRetry(ctx, id, "CONTROLLED_EXISTING_REPAIR_RETRY"); err != nil {
			t.Fatal(err)
		}
		after, err := r.RegistrationOperation(ctx, id)
		if err != nil || after.LastErrorCode != "CONTROLLED_EXISTING_REPAIR_RETRY" || after.Version <= before.Version {
			t.Fatal("existing non-rejection repair was frozen", after.LastErrorCode, err)
		}
	})
	t.Run("rejection-checkpoint-retains-claim-and-newer-fact-fences", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		id := registrationDispatchReady(t, r)
		first, version, err := r.MarkRegistrationCreateAttempted(ctx, id)
		if err != nil || !first {
			t.Fatal(first, err)
		}
		if err = r.MarkRegistrationIdentityRejected(ctx, id, "IDENTITY_UNAVAILABLE", version); !errors.Is(err, ErrRegistrationConflict) {
			t.Fatal("unknown code was accepted as rejection", err)
		}
		if err = r.MarkRegistrationIdentityRejected(ctx, id, "USERNAME_ALREADY_REGISTERED", version+999); !errors.Is(err, ErrRegistrationConflict) {
			t.Fatal("unissued future version accepted", err)
		}
		now = now.Add(6 * time.Second) // the public writer schedules the first recovery five seconds later
		leased, err := r.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second)
		if err != nil || len(leased) != 1 {
			t.Fatal("lease", len(leased), err)
		}
		claim, cancel, err := withIdentityRecoveryClaim(ctx, "registration", id, leased[0].RecoveryGeneration, leased[0].RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		now = now.Add(31 * time.Second)
		if err = r.MarkRegistrationIdentityRejected(claim, id, "USERNAME_ALREADY_REGISTERED", version); !errors.Is(err, ErrIdentityRecoveryClaimLost) {
			t.Fatal("expired claim wrote rejection", err)
		}
		if err = r.MarkRegistrationIdentityCreated(ctx, id, "controlled-correlated-subject"); err != nil {
			t.Fatal(err)
		}
		if err = r.MarkRegistrationIdentityRejected(ctx, id, "USERNAME_ALREADY_REGISTERED", version); !errors.Is(err, ErrRegistrationConflict) {
			t.Fatal("late rejection overwrote newer identity", err)
		}
		stored, err := r.RegistrationOperation(ctx, id)
		if err != nil || stored.Status != "IDENTITY_CREATED" || stored.IdentitySubject != "controlled-correlated-subject" {
			t.Fatal(stored.Status, err)
		}
	})
}
