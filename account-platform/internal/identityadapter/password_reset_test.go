package identityadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConfirmedPasswordResetResumesAfterSecretExpiry(t *testing.T) {
	directory := &directoryFake{revokeError: errors.New("controlled lost revocation acknowledgement")}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	reference, err := vault.Precheck("FixtureReset2026")
	if err != nil {
		t.Fatal(err)
	}
	operation := uuid.New()
	reset := func() error {
		return vault.ResetPassword(context.Background(), operation, reference.Handle, reference.PolicyVersion, "fixture-subject", "+8613800138000")
	}
	if err := reset(); err == nil || directory.sets != 1 || directory.revokes != 1 {
		t.Fatalf("initial uncertain revocation: err=%v sets=%d revokes=%d", err, directory.sets, directory.revokes)
	}
	now = now.Add(16 * time.Minute)
	// Force normal secret cleanup through a new precheck before replaying.
	if _, err := vault.Precheck("DifferentFixture2026"); err != nil {
		t.Fatal(err)
	}
	if vault.records[reference.Handle] != nil {
		t.Fatal("the original password record must expire independently of the receipt")
	}
	directory.revokeError = nil
	if err := reset(); err != nil || directory.sets != 1 || directory.revokes != 2 {
		t.Fatalf("revocation-only recovery: err=%v sets=%d revokes=%d", err, directory.sets, directory.revokes)
	}
	// A later login belongs to the completed state; retries must not revoke it.
	if err := reset(); err != nil || directory.sets != 1 || directory.revokes != 2 {
		t.Fatalf("acknowledged completion replay: err=%v sets=%d revokes=%d", err, directory.sets, directory.revokes)
	}
	for name, facts := range map[string][3]string{
		"subject": {reference.Handle, "different-subject", "+8613800138000"},
		"mobile":  {reference.Handle, "fixture-subject", "+8613900139000"},
		"handle":  {strings.Repeat("A", 43), "fixture-subject", "+8613800138000"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := vault.ResetPassword(context.Background(), operation, facts[0], reference.PolicyVersion, facts[1], facts[2]); !errors.Is(err, ErrReferenceConflict) {
				t.Fatalf("changed reset facts: %v", err)
			}
		})
	}
	if err := vault.ResetPassword(context.Background(), operation, reference.Handle, "different-policy", "fixture-subject", "+8613800138000"); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("changed policy: %v", err)
	}
	cold, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := cold.ResetPassword(context.Background(), operation, reference.Handle, reference.PolicyVersion, "fixture-subject", "+8613800138000"); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("missing process evidence cannot claim completion: %v", err)
	}
	now = now.Add(49 * time.Hour)
	if err := reset(); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("expired existing operation binding: %v", err)
	}
	if directory.sets != 1 || directory.revokes != 2 {
		t.Fatal("rejected facts or missing evidence performed remote work")
	}
}

func TestConcurrentPasswordResetCompletionHasOneAcknowledgedRevocation(t *testing.T) {
	directory := &directoryFake{revokeError: errors.New("controlled unknown revoke")}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := vault.Precheck("ConcurrentReset2026")
	if err != nil {
		t.Fatal(err)
	}
	operation := uuid.New()
	reset := func() error {
		return vault.ResetPassword(context.Background(), operation, reference.Handle, reference.PolicyVersion, "fixture-subject", "+8613800138000")
	}
	if err := reset(); err == nil {
		t.Fatal("unknown revocation cannot report completion")
	}
	directory.revokeError = nil
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := reset(); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if directory.sets != 1 || directory.revokes != 2 {
		t.Fatalf("sets=%d revokes=%d", directory.sets, directory.revokes)
	}
}
