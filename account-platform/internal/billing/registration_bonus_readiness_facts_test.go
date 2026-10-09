package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistrationBonusReadinessRequiresCompleteFacts(t *testing.T) {
	cases := []struct {
		name, body       string
		available, ready bool
	}{
		{"missing counts", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"null recoverable", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":null,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"null unrecoverable", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0,"unrecoverable_registration_count":null,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"missing key", `{"status":"READY","recoverable_registration_count":0,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"missing completion", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0,"unrecoverable_registration_count":0}`, false, false},
		{"ready contradiction", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0,"unrecoverable_registration_count":1,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"blocked contradiction", `{"status":"BLOCKED","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"pending", `{"status":"PENDING","recoverable_registration_count":0,"unrecoverable_registration_count":0}`, true, false},
		{"ready zero", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, true, true},
		{"ready exact int64", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":9007199254740993,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, true, true},
		{"blocked", `{"status":"BLOCKED","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":12,"unrecoverable_registration_count":2,"completed_at":"2026-09-29T18:31:58Z"}`, true, false},
		{"negative", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":-1,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
		{"fraction", `{"status":"READY","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":0.5,"unrecoverable_registration_count":0,"completed_at":"2026-09-29T18:31:58Z"}`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(c.body)) }))
			defer server.Close()
			verifier, err := NewHTTPRegistrationBonusEligibilityVerifier(server.URL, "synthetic-readiness-token")
			if err != nil {
				t.Fatal(err)
			}
			facts, err := verifier.Readiness(context.Background())
			if !c.available {
				if !errors.Is(err, ErrRegistrationBonusEligibilityUnavailable) {
					t.Fatalf("incomplete facts accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "ready exact int64" && facts.RecoverableRegistrationCount != 9007199254740993 {
				t.Fatal("count precision lost")
			}
			err = verifier.RequireReady(context.Background())
			if c.ready && err != nil {
				t.Fatal(err)
			}
			if !c.ready && !errors.Is(err, ErrRegistrationBonusEligibilityNotReady) {
				t.Fatalf("expected not ready: %v", err)
			}
		})
	}
}
