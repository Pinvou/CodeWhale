package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"testing"
	"time"
)

func TestHoldRecoveryEntryFailsClosedBeforeAnyStoreAction(t *testing.T) {
	for _, args := range [][]string{nil, {"--confirm=false"}, {"--confirm", "--limit=0"}, {"--confirm", "--limit=1001"}, {"--confirm", "--limit=oops"}, {"--confirm", "unexpected"}} {
		t.Run("arguments", func(t *testing.T) {
			var out bytes.Buffer
			called := false
			code := run(context.Background(), args, func(string) string { return "" }, &out, func(context.Context, string, billing.ExpiredHoldProofClient, int) error { called = true; return nil })
			if code != 2 || called {
				t.Fatalf("code=%d action=%v", code, called)
			}
		})
	}
	base := map[string]string{"DATABASE_URL": "fixture-store", "TOKEN_HOLD_EXPIRY_URL": "https://token.example.invalid", "INTERNAL_SERVICE_TOKEN": "fixture-short-lived-workload"}
	for _, tc := range []struct{ name, key, value string }{{"missing-proof-endpoint", "TOKEN_HOLD_EXPIRY_URL", ""}, {"http-proof-denied", "TOKEN_HOLD_EXPIRY_URL", "http://token.example.invalid"}, {"missing-credential", "INTERNAL_SERVICE_TOKEN", ""}, {"missing-store", "DATABASE_URL", ""}, {"invalid-CA", "WORKLOAD_IDENTITY_CA_FILE", "relative-ca.pem"}} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env[tc.key] = tc.value
			called := false
			var out bytes.Buffer
			if code := run(context.Background(), []string{"--confirm"}, func(k string) string { return env[k] }, &out, func(context.Context, string, billing.ExpiredHoldProofClient, int) error { called = true; return nil }); code != 1 || called {
				t.Fatalf("code=%d action=%v", code, called)
			}
		})
	}
}
func TestHoldRecoveryEntryUsesBoundedTrustedWorkerCarrier(t *testing.T) {
	env := map[string]string{"DATABASE_URL": "fixture-store", "TOKEN_HOLD_EXPIRY_URL": "https://token.example.invalid", "INTERNAL_SERVICE_TOKEN": "fixture-short-lived-workload"}
	for _, wantErr := range []bool{false, true} {
		t.Run("carrier", func(t *testing.T) {
			var out bytes.Buffer
			called := false
			code := run(context.Background(), []string{"--confirm", "--limit=7"}, func(k string) string { return env[k] }, &out, func(ctx context.Context, db string, p billing.ExpiredHoldProofClient, n int) error {
				called = true
				if db != "fixture-store" || p == nil || n != 7 {
					t.Fatal("worker inputs changed")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 45*time.Second {
					t.Fatal("missing bounded deadline")
				}
				identity, ok := serviceauth.FromContext(ctx)
				if !ok || identity.Service != "hold-recovery" || identity.JobType != "EXPIRED_HOLD_RECOVERY" {
					t.Fatal("missing exact service identity")
				}
				if wantErr {
					return errors.New("unproven")
				}
				return nil
			})
			want := 0
			if wantErr {
				want = 1
			}
			if code != want || !called {
				t.Fatalf("code=%d action=%v", code, called)
			}
		})
	}
}
