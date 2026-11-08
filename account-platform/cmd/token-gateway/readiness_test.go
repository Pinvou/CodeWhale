package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func TestTokenReadinessUsesCanonicalProductionClassification(t *testing.T) {
	for _, environment := range []string{"production", "production-new", "production-eu", " Production-East "} {
		t.Run(environment, func(t *testing.T) {
			ctx := context.Background()
			var count int64 = 1
			var pingErr, rotationErr error
			reads := 0
			ready := tokenReadiness(environment, func(context.Context) error { return pingErr }, func(context.Context) (int64, error) { reads++; return count, rotationErr })
			if !errors.Is(ready(ctx), token.ErrSecretRotationPending) || reads != 1 {
				t.Fatal("remaining ciphertext bypassed strict readiness")
			}
			count = 0
			if err := ready(ctx); err != nil {
				t.Fatal("fully rotated healthy instance was rejected", err)
			}
			rotationErr = errors.New("controlled rotation failure")
			if !errors.Is(ready(ctx), rotationErr) {
				t.Fatal("rotation read failure was hidden")
			}
			pingErr = errors.New("controlled database failure")
			before := reads
			if !errors.Is(ready(ctx), pingErr) || reads != before {
				t.Fatal("failed database ping did not short circuit")
			}
		})
	}
	t.Run("development keeps database-only readiness", func(t *testing.T) {
		calls := 0
		ready := tokenReadiness("development", func(context.Context) error { calls++; return nil }, func(context.Context) (int64, error) {
			t.Fatal("development unexpectedly requires rotation")
			return 1, nil
		})
		if err := ready(context.Background()); err != nil || calls != 1 {
			t.Fatal("database-only readiness changed", err, calls)
		}
	})
}
