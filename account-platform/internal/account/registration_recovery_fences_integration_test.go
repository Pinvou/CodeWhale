package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func preparedRegistrationForTest(t *testing.T, repo *PostgresRepository, now time.Time) RegistrationOperation {
	t.Helper()
	challenge := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138000", Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Provider: SMSProviderDescriptor{ID: "http", AdapterVersion: "v1"}}
	fingerprint := registrationFingerprint("controlled.user", challenge.Mobile, challenge.ID, "v1")
	hash := registrationTokenHash("controlled-query-token")
	op := RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), Username: "controlled.user", Mobile: challenge.Mobile, ChallengeID: challenge.ID, PasswordPolicyVersion: "v1", IssuerDigest: registrationIssuerDigest("https://identity.example.test"), QueryTokenHash: hash[:], RequestFingerprint: fingerprint[:]}
	ctx := context.Background()
	if err := repo.CreateRegistrationOperation(ctx, op, challenge); err != nil {
		t.Fatal("registration preparation failed")
	}
	if err := repo.MarkSMSChallengeSent(ctx, challenge.ID, "controlled", nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.ConsumeRegistrationSMS(ctx, op.ID, challenge.ID, op.Mobile, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRegistrationIdentityCreating(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestPostgresRegistrationCancellationProofIsBoundToAttemptVersion(t *testing.T) {
	for _, winner := range []string{"later_attempt", "compensation", "legacy"} {
		t.Run(winner, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			op := preparedRegistrationForTest(t, repo, time.Now().UTC())
			first, version, err := repo.MarkRegistrationCreateAttempted(ctx, op.ID)
			if err != nil || !first || version < 1 {
				t.Fatal("initial durable create claim was not established")
			}
			switch winner {
			case "later_attempt":
				replayed, laterVersion, err := repo.MarkRegistrationCreateAttempted(ctx, op.ID)
				if err != nil || replayed || laterVersion <= version {
					t.Fatal("subsequent create did not invalidate the earlier proof generation")
				}
				if err := repo.CompensatePreIdentityRegistration(ctx, op.ID, "IDENTITY_CREATE_NOT_DISPATCHED", version); !errors.Is(err, ErrRegistrationConflict) {
					t.Fatal("old absence proof released a later attempt")
				}
			case "compensation":
				if err := repo.CompensatePreIdentityRegistration(ctx, op.ID, "IDENTITY_CREATE_NOT_DISPATCHED", version); err != nil {
					t.Fatal("authoritative current proof could not complete")
				}
				if _, _, err := repo.MarkRegistrationCreateAttempted(ctx, op.ID); !errors.Is(err, ErrRegistrationConflict) {
					t.Fatal("compensated operation could dispatch again")
				}
			case "legacy":
				accountExec(t, repo, `UPDATE registration_operations SET legacy_intent=true WHERE id=$1`, op.ID)
				if err := repo.CompensatePreIdentityRegistration(ctx, op.ID, "IDENTITY_CREATE_NOT_DISPATCHED", version); !errors.Is(err, ErrRegistrationConflict) {
					t.Fatal("legacy intent accepted a new adapter's absence proof")
				}
			}
			stored, err := repo.RegistrationOperation(ctx, op.ID)
			if err != nil || stored.IdentitySubject != "" || stored.UserID != nil {
				t.Fatal("proof handling invented identity or account facts")
			}
			if winner == "compensation" && stored.Status != "COMPENSATED" {
				t.Fatal("proof completion did not release the reservation")
			}
			if winner != "compensation" && stored.Status == "COMPENSATED" {
				t.Fatal("unproven cancellation released the reservation")
			}
		})
	}
}

func TestPostgresRegistrationUnknownIdentityEntersRepairQueue(t *testing.T) {
	repo := accountDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo.now = func() time.Time { return now }
	op := preparedRegistrationForTest(t, repo, now)
	if _, _, err := repo.MarkRegistrationCreateAttempted(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(49 * time.Hour)
	jobs, err := repo.LeaseRegistrationRecoveries(context.Background(), 20, 30*time.Second)
	if err != nil || len(jobs) != 0 {
		t.Fatal("expired unknown identity remained in automatic recovery")
	}
	queue, err := repo.RegistrationRepairQueue(context.Background(), RegistrationRepairFilter{})
	if err != nil || len(queue) != 1 || queue[0].OperationID != op.ID || queue[0].CompensationAllowed || queue[0].SLAExpiresAt == nil {
		t.Fatal("unknown identity was hidden from the repair queue or treated as proven absent")
	}
	stored, err := repo.RegistrationOperation(context.Background(), op.ID)
	if err != nil || stored.Status != "REPAIR_REQUIRED" || stored.UserID != nil {
		t.Fatal("unknown identity did not preserve its reservation and repair state")
	}
}

func TestPostgresRegistrationRecoveryRejectsStaleAndExpiredClaims(t *testing.T) {
	repo := accountDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo.now = func() time.Time { return now }
	op := preparedRegistrationForTest(t, repo, now)
	now = now.Add(6 * time.Second)
	jobs, err := repo.LeaseRegistrationRecoveries(context.Background(), 20, 30*time.Second)
	if err != nil || len(jobs) != 1 {
		t.Fatal("initial recovery lease failed")
	}
	old := jobs[0]
	now = now.Add(31 * time.Second)
	jobs, err = repo.LeaseRegistrationRecoveries(context.Background(), 20, 30*time.Second)
	if err != nil || len(jobs) != 1 || jobs[0].RecoveryGeneration <= old.RecoveryGeneration {
		t.Fatal("expired recovery lease was not reclaimed")
	}
	oldCtx, cancel := withRecoveryClaim(context.Background(), "registration", op.ID, old.RecoveryGeneration, *old.RecoveryLeaseUntil)
	defer cancel()
	if err := repo.ScheduleRegistrationRetry(oldCtx, op.ID, "STALE"); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("old recovery worker rewrote the new schedule")
	}
	if err := repo.MarkRegistrationIdentityCreated(oldCtx, op.ID, "controlled-subject", true); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("old recovery worker persisted identity facts")
	}
	current := jobs[0]
	currentCtx, currentCancel := withRecoveryClaim(context.Background(), "registration", op.ID, current.RecoveryGeneration, *current.RecoveryLeaseUntil)
	defer currentCancel()
	if err := repo.ScheduleRegistrationRetry(currentCtx, op.ID, "CURRENT"); err != nil {
		t.Fatal("current recovery claim could not update its schedule")
	}
	now = now.Add(31 * time.Second)
	if err := repo.MarkRegistrationIdentityCreated(currentCtx, op.ID, "controlled-subject", true); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("expired current worker still wrote identity facts")
	}
	stored, err := repo.RegistrationOperation(context.Background(), op.ID)
	if err != nil || stored.IdentitySubject != "" || stored.LastErrorCode != "CURRENT" {
		t.Fatal("stale recovery write altered durable facts")
	}
}

func TestPostgresProfileRecoveryRejectsStaleAndExpiredClaims(t *testing.T) {
	repo := accountDatabase(t)
	principal, _ := accountUser(t, repo)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo.now = func() time.Time { return now }
	change, err := repo.CreateProfileChange(context.Background(), principal, "USERNAME", "profile"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	if err != nil {
		t.Fatal("profile change preparation failed")
	}
	accountExec(t, repo, `UPDATE profile_change_requests SET status='VERIFIED' WHERE id=$1`, change.ID)
	old, err := repo.ClaimProfileChange(context.Background())
	if err != nil || old == nil {
		t.Fatal("profile recovery lease failed")
	}
	now = now.Add(31 * time.Second)
	current, err := repo.ClaimProfileChange(context.Background())
	if err != nil || current == nil || current.Attempts <= old.Attempts {
		t.Fatal("profile claim was not reclaimed")
	}
	oldCtx, cancel := withRecoveryClaim(context.Background(), "profile", change.ID, int64(old.Attempts), *old.LeaseUntil)
	defer cancel()
	if err := repo.ScheduleProfileChangeRetry(oldCtx, change.ID, "STALE"); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("old profile worker rewrote current recovery")
	}
	if err := repo.MarkProfileIdentityUpdated(oldCtx, change.ID); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("old profile worker persisted an update")
	}
	currentCtx, currentCancel := withRecoveryClaim(context.Background(), "profile", change.ID, int64(current.Attempts), *current.LeaseUntil)
	defer currentCancel()
	if err := repo.ScheduleProfileChangeRetry(currentCtx, change.ID, "CURRENT"); err != nil {
		t.Fatal("current profile claim could not schedule recovery")
	}
	now = now.Add(31 * time.Second)
	if err := repo.MarkProfileSessionsRevoked(currentCtx, change.ID); !errors.Is(err, errRecoveryClaimLost) {
		t.Fatal("expired profile worker revoked sessions")
	}
	stored, err := repo.ProfileChange(context.Background(), change.ID, nil)
	if err != nil || stored.IdentityUpdatedAt != nil || stored.SessionsRevokedAt != nil || stored.LastErrorCode != "CURRENT" {
		t.Fatal("stale profile recovery altered durable facts")
	}
}
