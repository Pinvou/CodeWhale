package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestPasswordResetRecoveryPostgresIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("the password-reset recovery gate requires isolated PostgreSQL")
	}
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, migrationConnection, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 84)
	now := time.Now().UTC().Add(-time.Hour)

	legacyVerifiedUser := uuid.New()
	legacyFailedUser := uuid.New()
	for _, userID := range []uuid.UUID{legacyVerifiedUser, legacyFailedUser} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE','legacy',generate_unique_user_code(),$2,$2)`, userID, now); err != nil {
			t.Fatal(err)
		}
	}
	verifiedOlder, verifiedNewest, failedNewest := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,last_error_code,created_at,completed_at) VALUES
($1,$2,'issuer','verified-subject','AUTHENTICATED','FAILED','IDENTITY_RESET_FAILED',$6,$6),
($3,$2,'issuer','verified-subject','AUTHENTICATED','VERIFIED',NULL,$7,NULL),
($4,$5,'issuer','failed-subject','AUTHENTICATED','FAILED','IDENTITY_RESET_FAILED',$7,$7)`, verifiedOlder, legacyVerifiedUser, verifiedNewest, failedNewest, legacyFailedUser, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAccountIntegrationMigrations(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, migrationConnection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, migrationConnection)

	for _, operationID := range []uuid.UUID{verifiedNewest, failedNewest} {
		var status, code string
		var started bool
		if err := repository.pool.QueryRow(ctx, `SELECT status,last_error_code,password_write_started_at IS NOT NULL FROM password_reset_operations WHERE id=$1`, operationID).Scan(&status, &code, &started); err != nil {
			t.Fatal(err)
		}
		if status != "PASSWORD_STARTED" || code != "LEGACY_SESSION_REVOCATION_UNCERTAIN" || !started {
			t.Fatalf("legacy operation %s migrated to status=%s code=%s started=%v", operationID, status, code, started)
		}
	}
	var oldStatus, oldCode string
	if err := repository.pool.QueryRow(ctx, `SELECT status,last_error_code FROM password_reset_operations WHERE id=$1`, verifiedOlder).Scan(&oldStatus, &oldCode); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "FAILED" || oldCode != "MIGRATION_SUPERSEDED" {
		t.Fatalf("older ambiguous operation status=%s code=%s", oldStatus, oldCode)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE password_reset_operations SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id IN ($1,$2)`, verifiedNewest, failedNewest); err != nil {
		t.Fatal(err)
	}

	actorID := uuid.New()
	actor := identity.Principal{Issuer: "https://password-reset.integration", Subject: "concurrent-user", SessionID: "session", AuthenticatedAt: time.Now().UTC()}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,mobile_e164,created_at,updated_at) VALUES($1,'ACTIVE','concurrent',generate_unique_user_code(),'+8613800138000',$2,$2)`, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, actor.Issuer, actor.Subject, actorID, now); err != nil {
		t.Fatal(err)
	}
	if err = ensureDefaultPersonalRole(ctx, tx, actorID, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, createErr := repository.CreateAuthenticatedPasswordReset(ctx, actor, "opaque-password-reference-000000000000000000000", "user-v1")
			results <- createErr
		}()
	}
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for createErr := range results {
		switch {
		case createErr == nil:
			successes++
		case errors.Is(createErr, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent create returned %v", createErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent create successes=%d conflicts=%d", successes, conflicts)
	}

	crossID := uuid.New()
	cross := identity.Principal{Issuer: actor.Issuer, Subject: "cross-kind-user", SessionID: "cross-session", AuthenticatedAt: time.Now().UTC()}
	tx, err = repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,mobile_e164,created_at,updated_at) VALUES($1,'ACTIVE','cross',generate_unique_user_code(),'+8613800138001',$2,$2)`, crossID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, cross.Issuer, cross.Subject, crossID, now); err != nil {
		t.Fatal(err)
	}
	if err = ensureDefaultPersonalRole(ctx, tx, crossID, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	crossResults := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, createErr := repository.CreateAuthenticatedPasswordReset(ctx, cross, "opaque-password-reference-111111111111111111111", "user-v1")
		crossResults <- createErr
	}()
	go func() {
		defer wait.Done()
		_, createErr := repository.CreateProfileChange(ctx, cross, "USERNAME", "cross_kind_user")
		crossResults <- createErr
	}()
	wait.Wait()
	close(crossResults)
	successes, conflicts = 0, 0
	for createErr := range crossResults {
		switch {
		case createErr == nil:
			successes++
		case errors.Is(createErr, ErrConflict), errors.Is(createErr, ErrProfileChangeConflict):
			conflicts++
		default:
			t.Fatalf("cross-kind create returned %v", createErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("cross-kind create successes=%d conflicts=%d", successes, conflicts)
	}

	var operationID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `UPDATE password_reset_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 RETURNING id`, actorID).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	claims := make(chan *PasswordResetOperation, 2)
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			claimed, claimErr := repository.ClaimPasswordReset(ctx)
			if claimErr != nil {
				t.Errorf("claim: %v", claimErr)
			}
			claims <- claimed
		}()
	}
	wait.Wait()
	close(claims)
	claimedCount := 0
	for claimed := range claims {
		if claimed != nil && claimed.ID == operationID {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("concurrent claim count=%d", claimedCount)
	}

	startResults := make(chan bool, 2)
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			acquired, startErr := repository.MarkPasswordWriteStarted(ctx, operationID, uuid.New())
			if startErr != nil {
				t.Errorf("mark password write started: %v", startErr)
			}
			startResults <- acquired
		}()
	}
	wait.Wait()
	close(startResults)
	acquiredCount := 0
	for acquired := range startResults {
		if acquired {
			acquiredCount++
		}
	}
	if acquiredCount != 1 {
		t.Fatalf("concurrent password-write CAS acquired=%d, want 1", acquiredCount)
	}
	if err := repository.MarkPasswordUpdated(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkPasswordSessionsRevoked(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompletePasswordReset(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	// Lost responses can replay every checkpoint out of order after completion.
	for _, checkpoint := range []func(context.Context, uuid.UUID) error{
		repository.MarkPasswordSessionsRevoked,
		repository.CompletePasswordReset,
		repository.MarkPasswordUpdated,
	} {
		if err := checkpoint(ctx, operationID); err != nil {
			t.Fatalf("idempotent checkpoint: %v", err)
		}
	}
	if acquired, err := repository.MarkPasswordWriteStarted(ctx, operationID, uuid.New()); err != nil || acquired {
		t.Fatalf("completed operation password-write CAS acquired=%v err=%v", acquired, err)
	}

	activeID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,mobile_e164,password_handle,password_policy_version,fence_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'issuer','active','AUTHENTICATED','VERIFIED','','','',2,clock_timestamp(),clock_timestamp()+interval '30 minutes',clock_timestamp(),clock_timestamp())`, activeID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,mobile_e164,password_handle,password_policy_version,fence_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'issuer','duplicate','AUTHENTICATED','VERIFIED','','','',3,clock_timestamp(),clock_timestamp()+interval '30 minutes',clock_timestamp(),clock_timestamp())`, uuid.New(), actorID); err == nil {
		t.Fatal("active-user unique fence accepted a second operation")
	}
	if err := migrations.DownOne(ctx, migrationConnection, loaded); err == nil {
		t.Fatal("down migration accepted a nonterminal password reset")
	}
}
