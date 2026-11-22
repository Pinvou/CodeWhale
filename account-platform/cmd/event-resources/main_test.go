package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func TestResourceOperatorHasFiniteArgumentsAndExplicitIdentity(t *testing.T) {
	for _, create := range []bool{false, true} {
		args := []string{}
		if create {
			args = []string{"-create-only"}
		}
		called := false
		result, err := run(args, func(key string) string { return key }, func(ctx context.Context, url string, cfg events.NATSConfig, actual bool) error {
			called = true
			if url != "NATS_URL" || cfg.ResourceMode != "provisioned" || !cfg.RequireExplicitAuthentication || cfg.SeedFile != "NATS_NKEY_SEED_FILE" || actual != create {
				t.Fatal("operator identity or creation boundary changed")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("operator must have bounded lifetime")
			}
			return nil
		})
		if !called || err != nil || result != "NATS_FIXED_STREAM_VERIFIED" {
			t.Fatal("operator result is incorrect")
		}
	}
	for _, args := range [][]string{{"-render-authorization"}, {"-consumer-manifest", "anything"}, {"positional"}, {"-create-only", "unexpected"}} {
		if _, err := run(args, func(string) string { t.Fatal("invalid flags read runtime environment"); return "" }, nil); err == nil {
			t.Fatal("future or extra arguments must be rejected")
		}
	}
}

func TestResourceOperatorFailureNeverReturnsRawDiagnostics(t *testing.T) {
	result, err := run(nil, func(string) string { return "" }, func(context.Context, string, events.NATSConfig, bool) error {
		return errors.New("controlled-sensitive-diagnostic")
	})
	if result != "" || err == nil || err.Error() != "NATS_RESOURCE_CHECK_OR_CREATION_FAILED" {
		t.Fatal("raw dependency diagnostic reached output")
	}
}
