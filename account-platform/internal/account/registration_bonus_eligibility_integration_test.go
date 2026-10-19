package account

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository := retainedWalletFenceActors(t).repository(t)
	key := bytes.Repeat([]byte{0x31}, 32)
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", key); err != nil {
		t.Fatal(err)
	}
	historicalUser := uuid.New()
	if _, err := repository.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) SELECT $1,generate_unique_user_code(),'ACTIVE',$2,$2,$3,now(),now(),now() FROM capability`, historicalUser, "historical_bonus_user", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	readiness, err := repository.PrepareRegistrationBonusEligibility(ctx)
	if err != nil || readiness.Status != "READY" || readiness.RecoverableRegistrationCount != 1 || readiness.UnrecoverableRegistrationCount != 0 {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,generate_unique_user_code(),'PROVISIONING',$2,$2,$3,now(),now(),now())`, uuid.New(), "legacy_writer_after_ready", "+8613500135000"); err == nil || !strings.Contains(err.Error(), "registration-bonus-capable") {
		t.Fatalf("legacy writer was not fenced after READY: %v", err)
	}
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", bytes.Repeat([]byte{0x32}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PrepareRegistrationBonusEligibility(ctx); err == nil {
		t.Fatal("same key id with different key bytes must fail closed")
	}
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", key); err != nil {
		t.Fatal(err)
	}

	first := commitRegistrationBonusTestOperation(t, ctx, repository, "bonus_first", "+8613900139000")
	assertRegistrationBonusJobMode(t, ctx, repository, first, "EVALUATE")
	if _, err := repository.pool.Exec(ctx, `UPDATE users SET status='DELETED',username='closed_'||replace(gen_random_uuid()::text,'-',''),mobile_e164='closed:'||gen_random_uuid()::text,mobile_verified_at=NULL,updated_at=now() WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	// A real new registration still obeys SMS cooldown. Advance the repository's
	// injected business clock, without deleting or rewriting admission evidence.
	nextRegistration := repository.now().Add(2 * time.Minute)
	repository.now = func() time.Time { return nextRegistration }
	second := commitRegistrationBonusTestOperation(t, ctx, repository, "bonus_second", "+8613900139000")
	assertRegistrationBonusJobMode(t, ctx, repository, second, "NOT_APPLICABLE")
}

func TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository := retainedWalletFenceActors(t).repository(t)
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", bytes.Repeat([]byte{0x41}, 32)); err != nil {
		t.Fatal(err)
	}
	legacyUser := uuid.New()
	legacyOperation := registrationBonusTestOperation(t, ctx, repository, "legacy_anonymized", "+8613700137000")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,created_at,updated_at) VALUES($1,generate_unique_user_code(),'DELETED',$2,$2,'closed:'||gen_random_uuid()::text,now(),now())`, legacyUser, "legacy_deleted_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE registration_operations SET status='ACTIVE',identity_subject='legacy-subject',committed_user_id=$2,mobile_e164='closed:'||gen_random_uuid()::text,account_committed_at=now(),updated_at=now() WHERE id=$1`, legacyOperation, legacyUser); err != nil {
		t.Fatal(err)
	}
	readiness, err := repository.PrepareRegistrationBonusEligibility(ctx)
	if err != nil || readiness.Status != "BLOCKED" || readiness.UnrecoverableRegistrationCount != 1 {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	newUser := commitRegistrationBonusTestOperation(t, ctx, repository, "blocked_new", "+8613600136000")
	assertRegistrationBonusJobMode(t, ctx, repository, newUser, "NOT_APPLICABLE")
	var eligibilityCount int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_mobile_eligibility WHERE first_user_id=$1 AND source='REGISTRATION'`, newUser).Scan(&eligibilityCount); err != nil || eligibilityCount != 1 {
		t.Fatalf("blocked registration eligibility count=%d err=%v", eligibilityCount, err)
	}
}

func registrationBonusTestOperation(t *testing.T, ctx context.Context, repository *PostgresRepository, username, mobile string) uuid.UUID {
	t.Helper()
	now := repository.now()
	operationID, candidateUserID, challengeID := uuid.New(), uuid.New(), uuid.New()
	operation := RegistrationOperation{ID: operationID, CandidateUserID: candidateUserID, QueryTokenHash: bytes.Repeat([]byte{1}, 32), RequestFingerprint: bytes.Repeat([]byte{2}, 32), Username: username, Mobile: mobile, ChallengeID: challengeID, PasswordPolicyVersion: "user-v1", CreatedAt: now, UpdatedAt: now}
	challenge := SMSChallenge{ID: challengeID, Mobile: mobile, Purpose: SMSPurposeRegister, ExpiresAt: now.Add(5 * time.Minute), Source: "registration-bonus-test", Device: uuid.NewString(), Provider: SMSProviderDescriptor{ID: "generated", AdapterVersion: "v1"}}
	if err := repository.CreateRegistrationOperation(ctx, operation, challenge); err != nil {
		t.Fatal(err)
	}
	return operationID
}

func commitRegistrationBonusTestOperation(t *testing.T, ctx context.Context, repository *PostgresRepository, username, mobile string) uuid.UUID {
	t.Helper()
	operationID := registrationBonusTestOperation(t, ctx, repository, username, mobile)
	if _, err := repository.pool.Exec(ctx, `UPDATE registration_operations SET status='IDENTITY_CREATED',identity_subject=$2,identity_created_at=now(),updated_at=now() WHERE id=$1`, operationID, "subject-"+operationID.String()); err != nil {
		t.Fatal(err)
	}
	operation, err := repository.CommitRegistration(ctx, operationID, "https://identity.registration-bonus.test")
	if err != nil || operation.UserID == nil {
		t.Fatalf("commit operation=%+v err=%v", operation, err)
	}
	return *operation.UserID
}

func assertRegistrationBonusJobMode(t *testing.T, ctx context.Context, repository *PostgresRepository, userID uuid.UUID, expected string) {
	t.Helper()
	var mode string
	var keyID *string
	var fingerprint []byte
	if err := repository.pool.QueryRow(ctx, `SELECT registration_bonus_mode,registration_bonus_fingerprint_key_id,registration_bonus_fingerprint FROM wallet_provisioning_jobs WHERE user_id=$1`, userID).Scan(&mode, &keyID, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if mode != expected || expected == "EVALUATE" && (keyID == nil || len(fingerprint) != 32) || expected == "NOT_APPLICABLE" && (keyID != nil || fingerprint != nil) {
		t.Fatalf("mode=%s key_id=%v fingerprint_length=%d, want %s", mode, keyID, len(fingerprint), expected)
	}
}
