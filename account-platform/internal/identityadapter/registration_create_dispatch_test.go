package identityadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type unknownRegistrationDirectory struct {
	directoryFake
	unknownCreates int
}

func (d *unknownRegistrationDirectory) CreateMobileUser(context.Context, uuid.UUID, string, string, string) (string, error) {
	d.unknownCreates++
	return "", context.DeadlineExceeded
}

func registrationDispatchVault(t *testing.T, directory UserDirectory) (*PasswordVault, *time.Time, PasswordReference, uuid.UUID) {
	t.Helper()
	v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	v.now = func() time.Time { return now }
	ref, err := v.Precheck("Controlled2026")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err = v.ValidateReference(id, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	return v, &now, ref, id
}

func TestRegistrationCreateDispatchProof(t *testing.T) {
	t.Run("retained-first-expired-reference-fences-before-dispatch", func(t *testing.T) {
		d := &directoryFake{}
		v, now, ref, id := registrationDispatchVault(t, d)
		*now = ref.ExpiresAt.Add(time.Second)
		for range 2 {
			_, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true)
			if !errors.Is(err, ErrCreateNotDispatched) || d.creates != 0 {
				t.Fatal("never-dispatched proof", err, d.creates)
			}
		}
		if _, err := v.CreateMobileUser(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000"); errors.Is(err, ErrCreateNotDispatched) {
			t.Fatal("legacy call borrowed first proof")
		}
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "changed-handle", ref.PolicyVersion, "controlled", "+8613800138000", true); !errors.Is(err, ErrReferenceConflict) {
			t.Fatal("changed handle borrowed cancelled first proof", err)
		}
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", "unknown-policy", "controlled", "+8613800138000", true); errors.Is(err, ErrCreateNotDispatched) {
			t.Fatal("changed policy borrowed cancelled first proof", err)
		}
	})
	t.Run("unknown-dispatched-timeout-never-recreates-or-proves-absence", func(t *testing.T) {
		d := &unknownRegistrationDirectory{}
		v, now, ref, id := registrationDispatchVault(t, d)
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		for _, first := range []bool{true, false} {
			if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", first); err == nil || errors.Is(err, ErrCreateNotDispatched) {
				t.Fatal("unknown became no-dispatch proof", err)
			}
		}
		*now = ref.ExpiresAt.Add(time.Second)
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true); err == nil || errors.Is(err, ErrCreateNotDispatched) {
			t.Fatal("expiry hid dispatched fact", err)
		}
		if d.unknownCreates != 1 {
			t.Fatal("unknown create redispatched", d.unknownCreates)
		}
	})
	t.Run("restart-and-nonfirst-have-no-dispatch-authority", func(t *testing.T) {
		d := &directoryFake{}
		v, _, ref, id := registrationDispatchVault(t, d)
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000", false); !errors.Is(err, ErrCreateUnconfirmed) {
			t.Fatal(err)
		}
		restarted, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true); err == nil || errors.Is(err, ErrCreateNotDispatched) {
			t.Fatal("restart invented first proof", err)
		}
		if d.creates != 0 {
			t.Fatal("unattested create", d.creates)
		}
		unbound, err := restarted.Precheck("Newbound2026")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.CreateMobileUserFirstAttempt(context.Background(), id, unbound.Handle, unbound.PolicyVersion, "controlled", "+8613800138000", true); !errors.Is(err, ErrCreateUnconfirmed) || d.creates != 0 {
			t.Fatal("missing retained operation silently rebound and dispatched", err, d.creates)
		}
	})
	t.Run("operation-binding-and-confirmed-correlation", func(t *testing.T) {
		d := &directoryFake{}
		v, _, ref, id := registrationDispatchVault(t, d)
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, ref.Handle, ref.PolicyVersion, "different", "+8613800138000", true); !errors.Is(err, ErrReferenceConflict) {
			t.Fatal(err)
		}
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000", true); err != nil {
			t.Fatal(err)
		}
		if subject, err := v.CreateMobileUser(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000"); err != nil || subject != "identity-subject" || d.creates != 1 {
			t.Fatal("confirmed correlation replay", subject, err, d.creates)
		}
	})
	t.Run("proof-cleanup-keeps-original-window-and-does-not-reinvent-first", func(t *testing.T) {
		d := &unknownRegistrationDirectory{}
		v, now, ref, id := registrationDispatchVault(t, d)
		originalExpiry := v.operations[id].expiresAt
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		*now = ref.ExpiresAt.Add(time.Second)
		_, _ = v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true)
		if len(v.records) != 0 || len(v.operations) != 1 || !v.operations[id].dispatched || !v.operations[id].expiresAt.Equal(originalExpiry) {
			t.Fatal("plaintext cleanup erased or extended retained dispatch proof")
		}
		*now = originalExpiry.Add(time.Second)
		if _, err := v.Precheck("Different2026"); err != nil {
			t.Fatal(err)
		}
		if len(v.operations) != 0 {
			t.Fatal("existing public precheck cleanup trigger retained expired operation PII")
		}
		if _, err := v.CreateMobileUserFirstAttempt(context.Background(), id, "", ref.PolicyVersion, "controlled", "+8613800138000", true); err == nil || errors.Is(err, ErrCreateNotDispatched) {
			t.Fatal("expired binding invented proof", err)
		}
		if len(v.operations) != 0 || d.unknownCreates != 1 {
			t.Fatal("proof cleanup grew state or redispatched", len(v.operations), d.unknownCreates)
		}
	})
	t.Run("actual-http-requires-explicit-first-and-replayed-first-does-not-dispatch", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		for _, k := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE"} {
			t.Setenv(k, "")
		}
		d := &unknownRegistrationDirectory{}
		v, _, ref, id := registrationDispatchVault(t, d)
		s, err := NewService(v, "controlled-token")
		if err != nil {
			t.Fatal(err)
		}
		handler := s.Handler()
		request := func(first *bool) int {
			body := map[string]any{"registration_operation_id": id, "password_handle": ref.Handle, "password_policy_version": ref.PolicyVersion, "username": "controlled", "mobile": "+8613800138000"}
			if first != nil {
				body["first_attempt"] = *first
			}
			raw, _ := json.Marshal(body)
			q := httptest.NewRequest(http.MethodPost, "/internal/v1/users/mobile", bytes.NewReader(raw))
			q.Header.Set("Authorization", "Bearer controlled-token")
			q.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, q)
			return res.Code
		}
		if got := request(nil); got != 400 || d.unknownCreates != 0 {
			t.Fatal("missing attestation dispatched", got, d.unknownCreates)
		}
		no, yes := false, true
		if got := request(&no); got != 503 || d.unknownCreates != 0 {
			t.Fatal("nonfirst dispatched", got, d.unknownCreates)
		}
		if got := request(&yes); got != 500 || d.unknownCreates != 1 {
			t.Fatal("first unknown dispatch", got, d.unknownCreates)
		}
		if got := request(&yes); got != 503 || d.unknownCreates != 1 {
			t.Fatal("replayed first dispatched again", got, d.unknownCreates)
		}
	})
	t.Run("actual-http-never-dispatched-proof-preserves-original410", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		for _, k := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE"} {
			t.Setenv(k, "")
		}
		d := &directoryFake{}
		v, now, ref, id := registrationDispatchVault(t, d)
		*now = ref.ExpiresAt.Add(time.Second)
		s, err := NewService(v, "controlled-token")
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"registration_operation_id": id, "password_handle": "", "password_policy_version": ref.PolicyVersion, "username": "controlled", "mobile": "+8613800138000", "first_attempt": true}
		raw, _ := json.Marshal(body)
		q := httptest.NewRequest(http.MethodPost, "/internal/v1/users/mobile", bytes.NewReader(raw))
		q.Header.Set("Authorization", "Bearer controlled-token")
		q.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, q)
		if res.Code != 410 || !strings.Contains(res.Body.String(), "IDENTITY_CREATE_NOT_DISPATCHED") || d.creates != 0 {
			t.Fatal("canonical never-dispatched proof", res.Code, res.Body.String(), d.creates)
		}
	})
}
