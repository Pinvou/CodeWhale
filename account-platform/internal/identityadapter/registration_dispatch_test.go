package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type uncertainRegistrationDirectory struct {
	directoryFake
}

func (d *uncertainRegistrationDirectory) CreateMobileUser(context.Context, uuid.UUID, string, string, string, bool) (RegistrationIdentity, error) {
	d.creates++
	return RegistrationIdentity{}, errors.New("synthetic lost create response")
}

func registrationCreateCode(t *testing.T, vault *PasswordVault, operationID uuid.UUID, reference PasswordReference, username string, firstCreateAttempt bool) string {
	t.Helper()
	service, err := NewService(vault, "synthetic-internal-token")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(registrationReferenceRequest{RegistrationOperationID: operationID, PasswordHandle: reference.Handle, PasswordPolicyVersion: reference.PolicyVersion, Username: username, Mobile: "+8613800138000", FirstCreateAttempt: firstCreateAttempt})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/users/mobile", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer synthetic-internal-token")
	res := httptest.NewRecorder()
	service.Handler().ServeHTTP(res, req)
	var result struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
		t.Fatalf("status=%d invalid response: %v", res.Code, err)
	}
	return result.Code
}

func TestExpiredRegistrationReferenceFencesOnlyKnownNeverDispatchedOperation(t *testing.T) {
	directory := &directoryFake{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if code := registrationCreateCode(t, vault, op, ref, "first", true); code != "IDENTITY_CREATE_NOT_DISPATCHED" {
		t.Fatalf("known never dispatched expiry code=%s", code)
	}
	newRef, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	if code := registrationCreateCode(t, vault, op, newRef, "first", true); code != "IDENTITY_CREATE_NOT_DISPATCHED" {
		t.Fatalf("cancelled operation was resurrected: code=%s creates=%d", code, directory.creates)
	}
	if directory.creates != 0 {
		t.Fatalf("cancelled registration dispatched %d creates", directory.creates)
	}
	// Restarting uses a new server-owned operation; a cancelled intent cannot be
	// rebound while a legitimate fresh registration remains usable.
	if _, err := vault.CreateMobileUser(context.Background(), uuid.New(), newRef.Handle, newRef.PolicyVersion, "first", "+8613800138000", false, false); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredRegistrationReferenceNeverProvesUnknownCreateDidNotDispatch(t *testing.T) {
	directory := &uncertainRegistrationDirectory{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if _, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", false, false); err == nil {
		t.Fatal("unknown create result was accepted")
	}
	now = now.Add(16 * time.Minute)
	if code := registrationCreateCode(t, vault, op, ref, "first", false); code != "PASSWORD_REFERENCE_INVALID" {
		t.Fatalf("unknown remote create incorrectly proved absent: %s", code)
	}
	if directory.creates != 1 {
		t.Fatalf("expired reference dispatched another create: %d", directory.creates)
	}
}

func TestExpiredRegistrationReferenceWithoutProcessBindingHasNoAbsenceProof(t *testing.T) {
	for _, lifetime := range []time.Duration{16 * time.Minute, 49 * time.Hour} {
		t.Run(lifetime.String(), func(t *testing.T) {
			directory := &directoryFake{}
			vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
			vault.now = func() time.Time { return now }
			ref, err := vault.Precheck("FirstPassword123")
			if err != nil {
				t.Fatal(err)
			}
			op := uuid.New()
			if lifetime > 48*time.Hour {
				if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(lifetime)
			if code := registrationCreateCode(t, vault, op, ref, "first", true); code != "PASSWORD_REFERENCE_INVALID" {
				t.Fatalf("missing or expired operation binding proved absence: %s", code)
			}
			if directory.creates != 0 {
				t.Fatalf("expired create dispatched %d calls", directory.creates)
			}
		})
	}
}

func TestRegistrationWrongFactsCannotCancelValidIntent(t *testing.T) {
	directory := &directoryFake{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	if code := registrationCreateCode(t, vault, op, ref, "other", true); code != "PASSWORD_REFERENCE_CONFLICT" {
		t.Fatalf("changed facts code=%s", code)
	}
	if _, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", false, false); err != nil {
		t.Fatal(err)
	}
	if directory.creates != 1 {
		t.Fatalf("valid intent creates=%d", directory.creates)
	}
}

func TestLegacyRegistrationExpiryCannotProveOldProvisionerNeverDispatched(t *testing.T) {
	directory := &directoryFake{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if _, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", true, false); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("legacy unknown create obtained absence proof: %v", err)
	}
	if directory.creates != 0 {
		t.Fatalf("expired legacy create dispatched %d calls", directory.creates)
	}
}

type blockedRegistrationDirectory struct {
	directoryFake
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (d *blockedRegistrationDirectory) CreateMobileUser(context.Context, uuid.UUID, string, string, string, bool) (RegistrationIdentity, error) {
	d.calls.Add(1)
	close(d.entered)
	<-d.release
	return RegistrationIdentity{}, errors.New("synthetic remote outcome unknown")
}

func TestRegistrationExpiryCannotFenceAnInFlightCreate(t *testing.T) {
	directory := &blockedRegistrationDirectory{entered: make(chan struct{}), release: make(chan struct{})}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	var seconds atomic.Int64
	seconds.Store(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC).Unix())
	vault.now = func() time.Time { return time.Unix(seconds.Load(), 0).UTC() }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	first := make(chan error, 1)
	go func() {
		_, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", false, false)
		first <- err
	}()
	select {
	case <-directory.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not reach remote adapter")
	}
	seconds.Add(int64(16 * time.Minute / time.Second))
	second := make(chan error, 1)
	go func() {
		_, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", false, false)
		second <- err
	}()
	close(directory.release)
	if err := <-first; err == nil {
		t.Fatal("unknown remote result was accepted")
	}
	select {
	case err := <-second:
		if !errors.Is(err, ErrReferenceInvalid) || errors.Is(err, ErrCreateNotDispatched) {
			t.Fatalf("in-flight create fenced as absent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expiry and create deadlocked")
	}
	if directory.calls.Load() != 1 {
		t.Fatalf("remote creates=%d", directory.calls.Load())
	}
}

func TestRegistrationCancelledIntentRejectsConcurrentFreshReferenceReplays(t *testing.T) {
	directory := &directoryFake{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if _, err := vault.CreateMobileUser(context.Background(), op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000", false, true); !errors.Is(err, ErrCreateNotDispatched) {
		t.Fatal(err)
	}
	fresh, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, err := vault.CreateMobileUser(context.Background(), op, fresh.Handle, fresh.PolicyVersion, "first", "+8613800138000", false, false)
			results <- err
		}()
	}
	callers.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, ErrReferenceInvalid) || errors.Is(err, ErrCreateNotDispatched) {
			t.Fatalf("cancelled replay=%v", err)
		}
	}
	if directory.creates != 0 {
		t.Fatalf("cancelled concurrent creates=%d", directory.creates)
	}
}

func TestRegistrationRetryCannotAssertAbsenceFromAnotherIdentityProcess(t *testing.T) {
	directory := &uncertainRegistrationDirectory{}
	firstProcess, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	secondProcess, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	firstProcess.now = func() time.Time { return now }
	secondProcess.now = func() time.Time { return now }
	op := uuid.New()
	refs := make([]PasswordReference, 2)
	for index, vault := range []*PasswordVault{firstProcess, secondProcess} {
		refs[index], err = vault.Precheck("FirstPassword123")
		if err != nil {
			t.Fatal(err)
		}
		if err := vault.ValidateReference(op, refs[index].Handle, refs[index].PolicyVersion, "first", "+8613800138000"); err != nil {
			t.Fatal(err)
		}
	}
	// Account attests first=true only for the persistent NULL->attempt winner.
	if _, err := firstProcess.CreateMobileUser(context.Background(), op, refs[0].Handle, refs[0].PolicyVersion, "first", "+8613800138000", false, true); err == nil {
		t.Fatal("unknown remote create accepted")
	}
	now = now.Add(16 * time.Minute)
	if code := registrationCreateCode(t, secondProcess, op, refs[1], "first", false); code != "PASSWORD_REFERENCE_INVALID" {
		t.Fatalf("another process's unknown create was declared absent: %s", code)
	}
	if directory.creates != 1 {
		t.Fatalf("remote calls=%d", directory.creates)
	}
}

func TestRegistrationCancelledProofCannotBeReusedByRetryHTTP(t *testing.T) {
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	ref, err := vault.Precheck("FirstPassword123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err := vault.ValidateReference(op, ref.Handle, ref.PolicyVersion, "first", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if code := registrationCreateCode(t, vault, op, ref, "first", true); code != "IDENTITY_CREATE_NOT_DISPATCHED" {
		t.Fatalf("first=%s", code)
	}
	if code := registrationCreateCode(t, vault, op, ref, "first", false); code != "PASSWORD_REFERENCE_INVALID" {
		t.Fatalf("retry reused first proof: %s", code)
	}
}
