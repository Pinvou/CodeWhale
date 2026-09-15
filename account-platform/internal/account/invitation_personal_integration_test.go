package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPersonalInvitationsWithoutEmailIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "invitation-owner")
	target := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PERSONAL_USER", "invitation-target")
	other := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PERSONAL_USER", "invitation-other")
	principal := func(actor integrationActor) identity.Principal {
		return identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject}
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE users SET username='invited_user',mobile_e164='+8613800138000',email=NULL WHERE id=$1`, target.ID); err != nil {
		t.Fatal(err)
	}
	repository := &PostgresRepository{pool: db.runtimePool, now: func() time.Time { return time.Now().UTC() }}
	organization, err := repository.AdminCreateOrganization(ctx, principal(owner), owner.ID, "Invitation test organization", "invitation-org-create")
	if err != nil {
		t.Fatal(err)
	}
	create := func(kind, value string) Invitation {
		t.Helper()
		item, err := repository.CreateInvitation(ctx, principal(owner), organization.ID, InvitationCreateRequest{
			TargetType: kind, Target: value, Roles: []string{"MEMBER"}, ExpiresInHours: 72, IdempotencyKey: uuid.NewString(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	usernameInvite := create("USERNAME", "Invited_User")
	items, err := repository.ListMyInvitations(ctx, principal(target))
	if err != nil || len(items) != 1 || items[0].ID != usernameInvite.ID || items[0].OrganizationName != organization.Name || items[0].Token != "" {
		t.Fatalf("targeted inbox with organization name and no token: count=%d err=%v", len(items), err)
	}
	if items, err := repository.ListMyInvitations(ctx, principal(other)); err != nil || len(items) != 0 {
		t.Fatalf("unrelated identity received invitations: count=%d err=%v", len(items), err)
	}
	if _, err := repository.AcceptInvitation(ctx, principal(other), "", &usernameInvite.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong target must receive non-enumerating not-found: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := repository.AcceptInvitation(ctx, principal(target), "", &usernameInvite.ID)
		if err != nil || result.OrganizationID != organization.ID || result.Member.UserID != target.ID || result.Replayed != (attempt == 1) {
			t.Fatalf("accept/recover attempt %d: result=%+v err=%v", attempt, result, err)
		}
	}
	phoneInvite := create("PHONE", "13800138000")
	if items, err := repository.ListMyInvitations(ctx, principal(target)); err != nil || len(items) != 0 {
		t.Fatalf("unverified phone must not reveal invitation: count=%d err=%v", len(items), err)
	}
	if _, err := repository.AcceptInvitation(ctx, principal(target), "", &phoneInvite.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified phone must not accept: %v", err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE users SET mobile_verified_at=now() WHERE id=$1`, target.ID); err != nil {
		t.Fatal(err)
	}
	if items, err := repository.ListMyInvitations(ctx, principal(target)); err != nil || len(items) != 1 || items[0].ID != phoneInvite.ID {
		t.Fatalf("verified phone inbox: count=%d err=%v", len(items), err)
	}
	if result, err := repository.AcceptInvitation(ctx, principal(target), "", &phoneInvite.ID); err != nil || !result.Replayed {
		t.Fatalf("existing member acceptance must not duplicate membership: %+v %v", result, err)
	}
	var memberships int
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE organization_id=$1 AND user_id=$2`, organization.ID, target.ID).Scan(&memberships); err != nil || memberships != 1 {
		t.Fatalf("membership count=%d err=%v", memberships, err)
	}
	revoked := create("USERNAME", "invited_user")
	if _, err := repository.RevokeInvitation(ctx, principal(owner), organization.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AcceptInvitation(ctx, principal(target), "", &revoked.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked invitation accepted: %v", err)
	}
	expired := create("USERNAME", "invited_user")
	repository.now = func() time.Time { return time.Now().UTC().Add(96 * time.Hour) }
	if _, err := repository.AcceptInvitation(ctx, principal(target), "", &expired.ID); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("expired invitation accepted: %v", err)
	}
}
