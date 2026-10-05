package account

import (
	"context"
	"errors"
	"testing"
)

func TestPostgresProfileCannotAdoptUsernameRetainedByDeletedIdentity(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	_, deleted := accountUser(t, repo)
	principal, _ := accountUser(t, repo)
	accountExec(t, repo, `UPDATE users SET username='deleted_identity_name' WHERE id=$1`, deleted.UserID)
	if err := repo.AdminDeleteUser(ctx, fixturePlatformAdmin(t, repo), deleted.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateProfileChange(ctx, principal, "USERNAME", "deleted_identity_name"); !errors.Is(err, ErrUsernameAlreadyRegistered) {
		t.Fatal("profile rename treated a retained Keycloak username as reusable")
	}
}

func TestPostgresCompletedProfileReplayPreservesNewSessionAndVersion(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	principal, _ := accountUser(t, repo)
	change, err := repo.CreateProfileChange(ctx, principal, "USERNAME", "profile_completed_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitProfileChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkProfileSessionsRevocationAttempt(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkProfileSessionsRevoked(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteProfileChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	accountExec(t, repo, `INSERT INTO user_sessions(issuer,session_id,user_id,active_context_type,membership_version,last_seen_at,created_at) VALUES($1,'new-login-after-change',$2,'PERSONAL',1,statement_timestamp(),statement_timestamp())`, principal.Issuer, change.UserID)
	var before int64
	if err := repo.pool.QueryRow(ctx, `SELECT version FROM profile_change_requests WHERE id=$1`, change.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitProfileChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkProfileSessionsRevoked(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteProfileChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	var sessions int
	var after int64
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, change.UserID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal("completed profile replay revoked a subsequent login")
	}
	if err := repo.pool.QueryRow(ctx, `SELECT version FROM profile_change_requests WHERE id=$1`, change.ID).Scan(&after); err != nil || after != before {
		t.Fatal("completed profile replay changed its final revision")
	}
}

func TestPostgresCancelledProfileCannotResumeOrComplete(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	principal, _ := accountUser(t, repo)
	change, err := repo.CreateProfileChange(ctx, principal, "USERNAME", "profile_cancelled_fixture")
	if err != nil {
		t.Fatal(err)
	}
	accountExec(t, repo, `UPDATE profile_change_requests SET status='CANCELLED_ACCOUNT_CLOSURE' WHERE id=$1`, change.ID)
	for name, call := range map[string]func() error{
		"identity": func() error { return repo.MarkProfileIdentityUpdated(ctx, change.ID) },
		"commit":   func() error { return repo.CommitProfileChange(ctx, change.ID) },
		"revoke":   func() error { return repo.MarkProfileSessionsRevoked(ctx, change.ID) },
		"complete": func() error { return repo.CompleteProfileChange(ctx, change.ID) },
		"retry":    func() error { return repo.ScheduleProfileChangeRetry(ctx, change.ID, "OLD_FAILURE") },
	} {
		t.Run(name, func(t *testing.T) {
			if call() == nil {
				t.Fatal("cancelled profile accepted a late forward write")
			}
		})
	}
	stored, err := repo.ProfileChange(ctx, change.ID, nil)
	if err != nil || stored.Status != "CANCELLED_ACCOUNT_CLOSURE" || stored.IdentityUpdatedAt != nil || stored.AccountCommittedAt != nil || stored.SessionsRevokedAt != nil {
		t.Fatal("cancelled profile lost its terminal facts")
	}
}
