package account

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegistrationQueryTokenIsOperationBoundAndExpires(t *testing.T) {
	signer, err := NewRegistrationTokenSigner([]byte(strings.Repeat("r", 32)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	operationID := uuid.New()
	token, _, err := signer.Issue(operationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Validate(operationID, token); err != nil {
		t.Fatal(err)
	}
	if err := signer.Validate(uuid.New(), token); !errors.Is(err, ErrRegistrationQueryUnauthorized) {
		t.Fatalf("wrong operation validation=%v", err)
	}
	now = now.Add(49 * time.Hour)
	if err := signer.Validate(operationID, token); !errors.Is(err, ErrRegistrationQueryUnauthorized) {
		t.Fatalf("expired validation=%v", err)
	}
}
