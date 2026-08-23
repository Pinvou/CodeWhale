package identityadapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type directoryFake struct {
	password    string
	creates     int
	operationID uuid.UUID
}

func (f *directoryFake) CreateMobileUser(_ context.Context, operationID uuid.UUID, _, _, password string) (string, error) {
	f.password, f.creates, f.operationID = password, f.creates+1, operationID
	return "identity-subject", nil
}
func (f *directoryFake) FindMobileUserByRegistrationOperation(_ context.Context, operationID uuid.UUID) (string, bool, error) {
	return "identity-subject", f.operationID == operationID, nil
}
func (*directoryFake) DeleteUser(context.Context, string) error            { return nil }
func (*directoryFake) DisableUserAndConfirm(context.Context, string) error { return nil }

func TestPasswordPolicyPreservesExactPrintableASCII(t *testing.T) {
	for _, password := range []string{"pinvou2026", "Pinvou 2026", "Pinvou2026@*+-&"} {
		if err := ValidatePassword(password); err != nil {
			t.Fatalf("password %q: %v", password, err)
		}
	}
	for _, password := range []string{"password", "12345678", "a1", "Pinvou\t2026", "Pinvou\n2026", "密碼Pinvou2026"} {
		if !errors.Is(ValidatePassword(password), ErrPasswordInvalid) {
			t.Fatalf("password %q must be invalid", password)
		}
	}
	for _, password := range []string{" Pinvou2026", "Pinvou2026 "} {
		if !errors.Is(ValidatePassword(password), ErrPasswordEdgeSpace) {
			t.Fatalf("password %q must report edge space", password)
		}
	}
}

func TestPasswordReferenceBindsRegistrationAndCreatesExactlyOnce(t *testing.T) {
	directory := &directoryFake{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := vault.Precheck("Pinvou middle 2026")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.ValidateReference(uuid.Nil, reference.Handle, reference.PolicyVersion, "pinvou", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	if err := vault.ValidateReference(uuid.Nil, reference.Handle, reference.PolicyVersion, "other", "+8613800138000"); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("binding conflict=%v", err)
	}
	operationID := uuid.New()
	if err := vault.ValidateReference(operationID, reference.Handle, reference.PolicyVersion, "pinvou", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		subject, createErr := vault.CreateMobileUser(context.Background(), operationID, "", reference.PolicyVersion, "pinvou", "+8613800138000")
		if createErr != nil || subject != "identity-subject" {
			t.Fatalf("subject=%q err=%v", subject, createErr)
		}
	}
	if directory.creates != 1 || directory.password != "Pinvou middle 2026" {
		t.Fatalf("creates=%d password=%q", directory.creates, directory.password)
	}
	if err := vault.ValidateReference(operationID, reference.Handle, reference.PolicyVersion, "pinvou", "+8613800138000"); err != nil {
		t.Fatalf("completed operation must retain its keyed fingerprint until expiry: %v", err)
	}
}

func TestExpiredPasswordReferenceCannotCreateIdentity(t *testing.T) {
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	vault.now = func() time.Time { return now }
	reference, err := vault.Precheck("Pinvou2026")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if _, err := vault.CreateMobileUser(context.Background(), uuid.New(), reference.Handle, reference.PolicyVersion, "pinvou", "+8613800138000"); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("expired reference=%v", err)
	}
}

func TestPasswordReferenceBindsOperationToPasswordFingerprint(t *testing.T) {
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := vault.Precheck("Password123")
	if err != nil {
		t.Fatal(err)
	}
	second, err := vault.Precheck("Changed456")
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.New()
	if err := vault.ValidateReference(operationID, first.Handle, first.PolicyVersion, "pinvou", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	if err := vault.ValidateReference(operationID, second.Handle, second.PolicyVersion, "pinvou", "+8613800138000"); !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("expected operation password conflict, got %v", err)
	}
}
