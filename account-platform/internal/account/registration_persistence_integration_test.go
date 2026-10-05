package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
	"time"
)

func TestPostgresRegistrationOperationPersistsBeforeDelivery(t *testing.T) {
	repo := accountDatabase(t)
	now := time.Now().UTC()
	challenge := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138000", Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Provider: SMSProviderDescriptor{ID: "http", AdapterVersion: "v1"}}
	fingerprint := registrationFingerprint("controlled.user", challenge.Mobile, challenge.ID, "v1")
	hash := registrationTokenHash("public-controlled-query-token")
	operation := RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), QueryTokenHash: hash[:], RequestFingerprint: fingerprint[:], Username: "controlled.user", Mobile: challenge.Mobile, ChallengeID: challenge.ID, PasswordPolicyVersion: "v1", IssuerDigest: registrationIssuerDigest("https://identity.example.test")}
	if err := repo.CreateRegistrationOperation(context.Background(), operation, challenge); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) {
			if strings.HasPrefix(pgerr.Message, "operator does not exist:") {
				t.Log(pgerr.Message)
			}
			t.Fatalf("registration persistence SQLSTATE=%s routine=%s", pgerr.Code, pgerr.Routine)
		}
		t.Fatal("registration persistence failed before dispatch")
	}
	stored, err := repo.RegistrationOperation(context.Background(), operation.ID)
	if err != nil || stored.ChallengeID != challenge.ID || stored.Status != "SMS_PENDING" {
		t.Fatal("registration facts were not stored atomically")
	}
}
