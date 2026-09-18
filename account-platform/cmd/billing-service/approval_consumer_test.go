package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A Billing process must never derive a Q+1 resource expectation from a partial
// or unverified deployment. Every case below returns an error instead of a
// binding, including "all names present but the artifacts are not the trusted
// content", which only the package reader can accept.
func TestBillingApprovalConsumerResourcesFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	for _, name := range billingApprovalConsumerArtifactEnvironment {
		t.Setenv(name, "")
	}
	t.Setenv("NATS_NKEY_SEED_FILE", "")
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	if binding, err := billingApprovalConsumerResources(now); err == nil || binding != nil {
		t.Fatalf("unconfigured consumer returned a binding: %v", binding)
	}

	complete := func() {
		for _, name := range billingApprovalConsumerArtifactEnvironment {
			t.Setenv(name, filepath.Join(directory, name+".json"))
		}
	}
	for _, missing := range billingApprovalConsumerArtifactEnvironment {
		complete()
		t.Setenv(missing, "")
		if binding, err := billingApprovalConsumerResources(now); err == nil || binding != nil {
			t.Fatalf("incomplete consumer configuration (%s) returned a binding: %v", missing, binding)
		}
	}

	t.Setenv("NATS_NKEY_SEED_FILE", filepath.Join(directory, "nkey.seed"))
	if err := os.WriteFile(filepath.Join(directory, "nkey.seed"), []byte("SUABEXAMPLE"), 0o600); err != nil {
		t.Fatal(err)
	}
	complete()
	for _, name := range billingApprovalConsumerArtifactEnvironment {
		if err := os.WriteFile(filepath.Join(directory, name+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if binding, err := billingApprovalConsumerResources(now); err == nil || binding != nil {
		t.Fatalf("untrusted artifacts returned a binding: %v", binding)
	}
}
