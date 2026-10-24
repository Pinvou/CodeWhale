package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Only the remote identity outcome is controlled. The durable dispatch grant,
// registration recovery, queue SQL, authorization and HTTP serializer are real.
type inspectionUnknownIdentity struct {
	IdentityProvisioner
	creates, queries, disables atomic.Int64
}

func (i *inspectionUnknownIdentity) FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error) {
	i.queries.Add(1)
	return "", false, nil
}
func (i *inspectionUnknownIdentity) CreateMobileUserFirstAttempt(context.Context, uuid.UUID, string, string, string, string) (string, error) {
	i.creates.Add(1)
	return "", errors.New("public controlled lost identity acknowledgement")
}
func (i *inspectionUnknownIdentity) DisableUserAndConfirm(context.Context, string) error {
	i.disables.Add(1)
	return nil
}

func TestRegistrationUnknownInspectionPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	t.Run("durable-unknown-visible-through-real-authorized-route-without-actions", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		r := f.repository(t)
		ctx := context.Background()
		principal, _, _ := userCodeActor(t, r, "inspection-operator")
		id := registrationDispatchReady(t, r)
		remote := &inspectionUnknownIdentity{}
		auth := newMobileAuthForTest(t, r, &smsProviderFake{}, remote)
		op, err := r.RegistrationOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = auth.resumeRegistration(ctx, op); !errors.Is(err, ErrRegistrationRecovering) {
			t.Fatal("unknown result", err)
		}
		op, err = r.RegistrationOperation(ctx, id)
		if err != nil || op.IdentityCreateAttemptedAt == nil || op.IdentityCreatedAt != nil || op.IdentitySubject != "" || op.UserID != nil || op.Status != "IDENTITY_CREATING" {
			t.Fatal("actual unknown facts", err)
		}
		now := time.Now().UTC().Add(time.Minute)
		r.now = func() time.Time { return now }
		if err = NewRegistrationRecoveryWorker(r, auth, time.Second).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if remote.creates.Load() != 1 || remote.queries.Load() < 2 {
			t.Fatal("unknown recovery redispatched", remote.creates.Load(), remote.queries.Load())
		}
		h := NewService(r, nil).WithMobileAuth(auth, "public-inspection-service-token").Handler(func(_ context.Context, token string) (identity.Principal, error) {
			if token != "operator" {
				return identity.Principal{}, errors.New("untrusted public fixture credential")
			}
			return principal, nil
		})
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		q := httptest.NewRequest(http.MethodGet, "/admin/v1/registration-operations", nil)
		q.Header.Set("Authorization", "Bearer operator")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, q)
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		var page struct {
			Items []RegistrationRepairItem `json:"items"`
		}
		if err = json.Unmarshal(out.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		var found *RegistrationRepairItem
		for index := range page.Items {
			if page.Items[index].OperationID == id {
				found = &page.Items[index]
			}
		}
		if found == nil {
			t.Fatal("durable unknown registration disappeared from authorized inspection queue")
		}
		if found.Status != "IDENTITY_CREATING" || found.Stage != "IDENTITY_CREATING" || found.Responsibility != "IDENTITY" || found.AccountCreated || found.CompensationAllowed || found.SLAExpiresAt != nil {
			t.Fatal("invented unknown lifecycle facts", found)
		}
		if historyClaimSnapshot(t, r, "registration_operations", id) != before || remote.creates.Load() != 1 {
			t.Fatal("read changed durable facts or identity dispatch")
		}
		t.Logf("PUBLIC_REGISTRATION_INSPECTION_JSON %s", out.Body.String())
		facts := inspectionRegistrationFacts(t, r, id)
		for _, action := range []string{"retry", "compensate"} {
			denied := inspectionHTTP(h, "operator", http.MethodPost, "/admin/v1/registration-operations/"+id.String()+"/actions/"+action)
			expected := http.StatusConflict
			if action == "retry" {
				expected = http.StatusInternalServerError
			} // Existing generic classification is preserved.
			if denied.Code != expected {
				t.Fatal("unexpected rejection status", action, denied.Code)
			}
			if inspectionRegistrationFacts(t, r, id) != facts || remote.creates.Load() != 1 || remote.disables.Load() != 0 {
				t.Fatal("rejected action changed lifecycle or identity")
			}
		}
		for _, secret := range []string{op.Username, op.Mobile, "identity_subject", "password", "query_token", "request_fingerprint"} {
			if strings.Contains(out.Body.String(), secret) {
				t.Fatalf("inspection disclosed private field/value %q", secret)
			}
		}
	})
	t.Run("exact-id-filters-null-sla-updated-age-and-live-authorization", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		r := f.repository(t)
		ctx := context.Background()
		principal, _, _ := userCodeActor(t, r, "inspection-filters")
		ungranted := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_BILLING_OPERATOR", "inspection-ungranted")
		now := time.Now().UTC().Truncate(time.Microsecond)
		clock := now.Add(-2 * time.Hour)
		r.now = func() time.Time { return clock }
		id := inspectionRegistrationReady(t, r, "filterunknown", "+8613800138001")
		remote := &inspectionUnknownIdentity{}
		auth := newMobileAuthForTest(t, r, &smsProviderFake{}, remote)
		op, err := r.RegistrationOperation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = auth.resumeRegistration(ctx, op); !errors.Is(err, ErrRegistrationRecovering) {
			t.Fatal(err)
		}
		clock = now
		if err = r.ScheduleRegistrationRetry(ctx, id, "IDENTITY_UNAVAILABLE"); err != nil {
			t.Fatal(err)
		}
		h := NewService(r, nil).WithMobileAuth(auth, "public-inspection-token").Handler(func(_ context.Context, token string) (identity.Principal, error) {
			switch token {
			case "operator":
				return principal, nil
			case "ungranted":
				return entitlementApprovalPrincipal(ungranted), nil
			default:
				return identity.Principal{}, errors.New("untrusted public fixture")
			}
		})
		path := "/admin/v1/registration-operations?operation_id=" + id.String()
		before := historyClaimSnapshot(t, r, "registration_operations", id)
		out := inspectionHTTP(h, "operator", http.MethodGet, path)
		rows := inspectionItems(t, out)
		if len(rows) != 1 || rows[0].OperationID != id || rows[0].IdentityCreateAttemptedAt == nil || !rows[0].CreatedAt.Before(rows[0].UpdatedAt) || rows[0].SLAExpiresAt != nil {
			t.Fatal("wrong exact projection", out.Body.String())
		}
		for _, query := range []struct {
			suffix string
			count  int
		}{
			{"&status=IDENTITY_CREATING&stage=IDENTITY_CREATING&responsibility=IDENTITY", 1},
			{"&status=REPAIR_REQUIRED", 0}, {"&responsibility=ACCOUNT", 0}, {"&sla=WITHIN", 0}, {"&sla=OVERDUE", 0},
			{"&older_than_minutes=60", 0},
		} {
			t.Run(query.suffix, func(t *testing.T) {
				got := inspectionItems(t, inspectionHTTP(h, "operator", http.MethodGet, path+query.suffix))
				if len(got) != query.count {
					t.Fatal("filter semantics", query.suffix, len(got))
				}
			})
		}
		if len(inspectionItems(t, inspectionHTTP(h, "operator", http.MethodGet, "/admin/v1/registration-operations?operation_id="+uuid.NewString()))) != 0 {
			t.Fatal("exact ID filter returned another operation")
		}
		for _, bad := range []string{"malformed", "00000000-0000-0000-0000-000000000000"} {
			if response := inspectionHTTP(h, "operator", http.MethodGet, "/admin/v1/registration-operations?operation_id="+bad); response.Code != 400 {
				t.Fatal("invalid ID was ignored", response.Code)
			}
		}
		if historyClaimSnapshot(t, r, "registration_operations", id) != before {
			t.Fatal("successful inspection reads changed DB facts")
		}
		beforeDenied := inspectionRegistrationFacts(t, r, id)
		for _, token := range []string{"ungranted", "invalid"} {
			response := inspectionHTTP(h, token, http.MethodGet, path)
			if response.Code == 200 || strings.Contains(response.Body.String(), id.String()) {
				t.Fatal("inspection leaked without permission", token, response.Code)
			}
		}
		if inspectionRegistrationFacts(t, r, id) != beforeDenied {
			t.Fatal("denied reads changed registration facts")
		}
		userCodeActor(t, r, "inspection-spare-super")
		if _, err = r.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, principal.Issuer, principal.Subject); err != nil {
			t.Fatal(err)
		}
		if response := inspectionHTTP(h, "operator", http.MethodGet, path); response.Code == 200 {
			t.Fatal("disabled operator still read the queue")
		}
	})
	t.Run("ordinary-inflight-known-terminal-and-existing-repair-compensation", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		r := f.repository(t)
		ctx := context.Background()
		principal, _, _ := userCodeActor(t, r, "inspection-existing")
		now := time.Now().UTC().Truncate(time.Microsecond)
		clock := now
		r.now = func() time.Time { return clock }
		remote := &inspectionUnknownIdentity{}
		auth := newMobileAuthForTest(t, r, &smsProviderFake{}, remote)
		h := NewService(r, nil).WithMobileAuth(auth, "public-inspection-token").Handler(func(_ context.Context, token string) (identity.Principal, error) {
			if token == "operator" {
				return principal, nil
			}
			return identity.Principal{}, errors.New("untrusted public fixture")
		})
		sms := registrationBonusTestOperation(t, ctx, r, "inspection-sms", "+8613800138002")
		preparing := inspectionRegistrationReady(t, r, "inspection-preparing", "+8613800138003")
		known := inspectionRegistrationReady(t, r, "inspection-known", "+8613800138004")
		if err := r.MarkRegistrationIdentityCreated(ctx, known, "public-known-identity"); err != nil {
			t.Fatal(err)
		}
		compensated := inspectionRegistrationReady(t, r, "inspection-compensated", "+8613800138005")
		first, version, err := r.MarkRegistrationCreateAttempted(ctx, compensated)
		if err != nil || !first {
			t.Fatal(err)
		}
		if err = r.CompensatePreIdentityRegistration(ctx, compensated, "IDENTITY_CREATE_NOT_DISPATCHED", version); err != nil {
			t.Fatal(err)
		}
		for _, id := range []uuid.UUID{sms, preparing, known, compensated} {
			if len(inspectionItems(t, inspectionHTTP(h, "operator", http.MethodGet, "/admin/v1/registration-operations?operation_id="+id.String()))) != 0 {
				t.Fatal("non-target included", id)
			}
		}
		clock = now.Add(-25 * time.Hour)
		repair := inspectionRegistrationReady(t, r, "inspection-repair", "+8613800138006")
		if err = r.MarkRegistrationIdentityCreated(ctx, repair, "public-repair-identity"); err != nil {
			t.Fatal(err)
		}
		compensate := inspectionRegistrationReady(t, r, "inspection-compensate", "+8613800138007")
		if err = r.MarkRegistrationIdentityCreated(ctx, compensate, "public-compensation-identity"); err != nil {
			t.Fatal(err)
		}
		clock = now
		for _, id := range []uuid.UUID{repair, compensate} {
			if err = r.ScheduleRegistrationRetry(ctx, id, "ACCOUNT_COMMIT_FAILED"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = r.PrepareRegistrationCompensation(ctx, principal, compensate); err != nil {
			t.Fatal(err)
		}
		response := inspectionHTTP(h, "operator", http.MethodGet, "/admin/v1/registration-operations")
		rows := inspectionItems(t, response)
		if len(rows) != 2 {
			t.Fatal("existing queue changed", response.Body.String())
		}
		for _, row := range rows {
			if row.SLAExpiresAt == nil || !row.CompensationAllowed || row.AccountCreated {
				t.Fatal("lost existing proof projection", row)
			}
		}
		t.Logf("PUBLIC_REGISTRATION_EXISTING_JSON %s", response.Body.String())
		if err = r.ConfigureRegistrationBonusFingerprint("public-inspection", bytes.Repeat([]byte{7}, 32)); err != nil {
			t.Fatal(err)
		}
		retried := inspectionHTTP(h, "operator", http.MethodPost, "/admin/v1/registration-operations/"+repair.String()+"/actions/retry")
		if retried.Code != 202 {
			t.Fatal("existing repair retry failed", retried.Code, retried.Body.String())
		}
		stored, err := r.RegistrationOperation(ctx, repair)
		if err != nil || stored.UserID == nil || stored.Status != "PROVISIONING" {
			t.Fatal("existing account commit did not advance", err)
		}
		jobs, err := r.LeaseWalletProvisioningJobs(ctx, 20, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.UserID == *stored.UserID {
				if err = r.CompleteWalletProvisioning(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(inspectionItems(t, inspectionHTTP(h, "operator", http.MethodGet, "/admin/v1/registration-operations?operation_id="+repair.String()))) != 0 {
			t.Fatal("completed registration remained in queue")
		}
		done := inspectionHTTP(h, "operator", http.MethodPost, "/admin/v1/registration-operations/"+compensate.String()+"/actions/compensate")
		if done.Code != 204 || remote.disables.Load() != 1 || remote.creates.Load() != 0 {
			t.Fatal("existing compensation contract changed", done.Code, done.Body.String())
		}
		final, err := r.RegistrationOperation(ctx, compensate)
		if err != nil || final.Status != "COMPENSATED" {
			t.Fatal("compensation did not finish", err)
		}
	})

}

func inspectionRegistrationReady(t *testing.T, r *PostgresRepository, label, mobile string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := registrationBonusTestOperation(t, ctx, r, label, mobile)
	op, err := r.RegistrationOperation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.MarkSMSChallengeSent(ctx, op.ChallengeID, "public-inspection-message", nil); err != nil {
		t.Fatal(err)
	}
	if err = r.ConsumeRegistrationSMS(ctx, id, op.ChallengeID, op.Mobile, true); err != nil {
		t.Fatal(err)
	}
	if err = r.MarkRegistrationIdentityCreating(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}
func inspectionHTTP(h http.Handler, token, method, path string) *httptest.ResponseRecorder {
	q := httptest.NewRequest(method, path, nil)
	q.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, q)
	return w
}
func inspectionItems(t *testing.T, w *httptest.ResponseRecorder) []RegistrationRepairItem {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page struct {
		Items []RegistrationRepairItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}
func inspectionRegistrationFacts(t *testing.T, r *PostgresRepository, id uuid.UUID) string {
	t.Helper()
	var value string
	if err := r.pool.QueryRow(context.Background(), `SELECT jsonb_build_object('registration',(SELECT to_jsonb(ro) FROM registration_operations ro WHERE id=$1),'users',(SELECT count(*) FROM users),'identities',(SELECT count(*) FROM identities),'wallet_jobs',(SELECT count(*) FROM wallet_provisioning_jobs),'outbox',(SELECT count(*) FROM outbox_events),'sms',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM sms_challenges s))::text`, id).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(value)
}
