package identityadapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPasswordExpiryOrderCapacityAndBoundary(t *testing.T) {
	v, err := NewPasswordVaultWithCapacity([]byte(strings.Repeat("k", 32)), &directoryFake{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	now := base.Add(10 * time.Minute)
	v.now = func() time.Time { return now }
	later, err := v.Precheck("LaterPassword9")
	if err != nil {
		t.Fatal(err)
	}
	laterRecord := v.records[later.Handle]
	// A wall-clock step back makes insertion order different from expiry order.
	now = base
	earlier, err := v.Precheck("EarlierPassword9")
	if err != nil {
		t.Fatal(err)
	}
	earlierRecord := v.records[earlier.Handle]
	now = earlier.ExpiresAt
	if _, err := v.record(earlier.Handle, earlier.PolicyVersion); err != nil {
		t.Fatal("reference expired at the original inclusive validity boundary", err)
	}
	if _, err := v.Precheck("NextPassword9"); !errors.Is(err, ErrPasswordReferenceCapacity) {
		t.Fatal("capacity was released before the reference expired", err)
	}
	now = now.Add(time.Nanosecond)
	next, err := v.Precheck("NextPassword9")
	if err != nil {
		t.Fatal("earlier expired capacity was not released", err)
	}
	if earlierRecord.password != "" || earlierRecord.fingerprint != [32]byte{} {
		t.Fatal("removed reference retained plaintext or fingerprint")
	}
	if _, err := v.record(earlier.Handle, earlier.PolicyVersion); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatal("expired reference became usable", err)
	}
	if _, err := v.record(later.Handle, later.PolicyVersion); err != nil {
		t.Fatal("live reference was evicted by expiry of an earlier entry", err)
	}
	now = later.ExpiresAt.Add(time.Nanosecond)
	for range 100 {
		if _, err := v.record(next.Handle, next.PolicyVersion); err != nil {
			t.Fatal(err)
		}
	}
	if laterRecord.password != "" || laterRecord.fingerprint != [32]byte{} || len(v.records) != 1 || v.recordExpiry.Len() != 1 {
		t.Fatal("lookup retained expired state or grew the live expiry index")
	}
	now = next.ExpiresAt.Add(time.Nanosecond)
	if _, err := v.record(next.Handle, next.PolicyVersion); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatal(err)
	}
	if len(v.records) != 0 || v.recordExpiry.Len() != 0 {
		t.Fatal("expired reference state was not reclaimed")
	}
	for _, entry := range v.recordExpiry[:cap(v.recordExpiry)] {
		if entry.key != "" || !entry.at.IsZero() {
			t.Fatal("unused expiry storage retained an identifier")
		}
	}
}

func TestPasswordExpiryOperationWindowAndDispatchProof(t *testing.T) {
	v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	now := base.Add(time.Hour)
	v.now = func() time.Time { return now }
	ref, err := v.Precheck("OperationPassword9")
	if err != nil {
		t.Fatal(err)
	}
	later, earlier := uuid.New(), uuid.New()
	if err := v.ValidateReference(later, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = base
	if err := v.ValidateReference(earlier, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	if err := v.markRegistrationDispatch(later, "controlled", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if err := v.ValidateReference(later, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000"); err != nil {
			t.Fatal(err)
		}
		if err := v.markRegistrationDispatch(later, "controlled", "+8613800138000"); !errors.Is(err, ErrCreateUnconfirmed) {
			t.Fatal("dispatch proof permitted a second attempt", err)
		}
	}
	if v.proofExpiry.Len() != 2 || !v.operations[later].expiresAt.Equal(base.Add(49*time.Hour)) {
		t.Fatal("replay grew or renewed operation retention")
	}
	now = base.Add(48 * time.Hour)
	if err := v.registrationDispatchFence(later, ref.Handle, ref.PolicyVersion, "controlled", "+8613800138000", true); err != nil {
		t.Fatal(err)
	}
	if len(v.operations) != 2 {
		t.Fatal("operation evidence expired at its original validity boundary")
	}
	now = now.Add(time.Nanosecond)
	if err := v.markRegistrationDispatch(later, "controlled", "+8613800138000"); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal(err)
	}
	if _, exists := v.operations[earlier]; exists || len(v.operations) != 1 || v.proofExpiry.Len() != 1 || !v.operations[later].dispatched {
		t.Fatal("expiry order lost retained dispatch proof or kept expired PII")
	}
	now = base.Add(49*time.Hour + time.Nanosecond)
	if err := v.markRegistrationDispatch(later, "controlled", "+8613800138000"); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal(err)
	}
	if len(v.operations) != 0 || v.proofExpiry.Len() != 0 {
		t.Fatal("operation evidence survived its original retention window")
	}
}

func TestPasswordExpiryPublicReplayAndPurposeBoundaries(t *testing.T) {
	for _, purpose := range []string{"REGISTER", "PASSWORD_RESET"} {
		t.Run(purpose, func(t *testing.T) {
			s, verifier, _ := preparationFixture(t)
			base := time.Now().UTC()
			now := base.Add(10 * time.Minute)
			s.vault.now = func() time.Time { return now }
			verifier.facts.Purpose = purpose
			verifier.facts.ExpiresAt = now.Add(time.Minute)
			laterID := verifier.facts.ID
			laterToken := laterID.String() + ".PUBLIC_SYNTHETIC_MAC"
			first := preparationHTTP(s, laterToken, "LaterPassword9")
			var later PasswordReference
			if first.Code != http.StatusCreated || json.Unmarshal(first.Body.Bytes(), &later) != nil {
				t.Fatal("first admitted reference failed", first.Code)
			}
			laterRecord := s.vault.records[later.Handle]
			now = base
			verifier.facts.ID = uuid.New()
			verifier.facts.ExpiresAt = now.Add(time.Minute)
			earlierToken := verifier.facts.ID.String() + ".PUBLIC_SYNTHETIC_MAC"
			second := preparationHTTP(s, earlierToken, "EarlierPassword9")
			var earlier PasswordReference
			if second.Code != http.StatusCreated || json.Unmarshal(second.Body.Bytes(), &earlier) != nil {
				t.Fatal("second admitted reference failed", second.Code)
			}
			earlierRecord := s.vault.records[earlier.Handle]
			now = base.Add(2 * time.Minute)
			for range 100 {
				response := preparationHTTP(s, earlierToken, "EarlierPassword9")
				var replay PasswordReference
				if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &replay) != nil || replay != earlier {
					t.Fatal("accepted reference replay changed", response.Code)
				}
			}
			if verifier.calls.Load() != 2 || len(s.preparations.records) != 2 || s.preparations.expiry.Len() != 2 || s.vault.recordExpiry.Len() != 2 {
				t.Fatal("replay reclaimed authority or grew expiry metadata")
			}
			if earlierRecord.preparation.Purpose != purpose || laterRecord.preparation.Purpose != purpose {
				t.Fatal("expiry indexing changed purpose provenance")
			}
			now = earlier.ExpiresAt
			if response := preparationHTTP(s, earlierToken, "EarlierPassword9"); response.Code != http.StatusForbidden {
				t.Fatal("preparation metadata outlived original exact deadline", response.Code)
			}
			if s.preparations.expiry.Len() != 1 || len(s.preparations.records) != 1 {
				t.Fatal("earlier preparation was not reclaimed in expiry order")
			}
			if _, err := s.vault.record(earlier.Handle, earlier.PolicyVersion); err != nil {
				t.Fatal("vault strict deadline was changed", err)
			}
			response := preparationHTTP(s, laterToken, "LaterPassword9")
			var replay PasswordReference
			if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &replay) != nil || replay != later {
				t.Fatal("live grant receipt was evicted", response.Code)
			}
			now = now.Add(time.Nanosecond)
			if response := preparationHTTP(s, laterToken, "LaterPassword9"); response.Code != http.StatusCreated {
				t.Fatal("next replay failed", response.Code)
			}
			if earlierRecord.password != "" || earlierRecord.fingerprint != [32]byte{} {
				t.Fatal("expired plaintext was not cleared")
			}
			verifier.facts.ID = laterID
			verifier.facts.ExpiresAt = base.Add(11 * time.Minute)
			now = later.ExpiresAt
			if response := preparationHTTP(s, laterToken, "LaterPassword9"); response.Code != http.StatusForbidden {
				t.Fatal("expired preparation resurrected", response.Code)
			}
			if len(s.preparations.records) != 0 || s.preparations.expiry.Len() != 0 || len(s.preparations.locks) != 0 {
				t.Fatal("expired preparation metadata or grant ownership leaked")
			}
			now = now.Add(time.Nanosecond)
			if _, err := s.vault.record(later.Handle, later.PolicyVersion); !errors.Is(err, ErrReferenceInvalid) {
				t.Fatal(err)
			}
			if laterRecord.password != "" || laterRecord.fingerprint != [32]byte{} {
				t.Fatal("last plaintext was not cleared")
			}
		})
	}
}
