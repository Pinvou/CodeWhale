package billingschema

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Original deployment actors, SQL seeds and actual organization creation are
// used here. No permission or role-assignment rows are injected by this test.
func TestOrganizationMemberUnitProducerPublicPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive PostgreSQL actor fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	m, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, c, selectVersions(m, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, stageAccountSeed); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-subject", SessionID: "owned-session"}
	owner := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	org, err := r.AdminCreateOrganization(ctx, principal, owner, "owned authority organization", "f06-unit-producer-organization")
	if err != nil {
		t.Fatal(err)
	}

	root, err := r.AdminCreateUnit(ctx, principal, org.ID, nil, "unit root", "unit-producer-root-key")
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.AdminCreateUnit(ctx, principal, org.ID, &root.Unit.ID, "unit child", "unit-producer-child-key")
	if err != nil {
		t.Fatal(err)
	}
	outside, err := r.AdminCreateUnit(ctx, principal, org.ID, nil, "outside unit", "unit-producer-outside-key")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := func(label string) (identity.Principal, uuid.UUID) {
		t.Helper()
		p := identity.Principal{Issuer: "https://owned.invalid", Subject: "unit-" + label, SessionID: "session-" + label, Username: "unit_" + label}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := r.LeaseWalletProvisioningJobs(ctx, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, job := range jobs {
			if job.UserID == me.UserID {
				if err = r.CompleteWalletProvisioning(ctx, job); err != nil {
					t.Fatal(err)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("legitimate wallet job absent")
		}
		return p, me.UserID
	}
	unitActor, actorID := bootstrap("actor")
	if _, err = r.AdminAddMember(ctx, principal, org.ID, actorID, []string{"MEMBER"}, []uuid.UUID{root.Unit.ID}, "unit-producer-actor-add"); err != nil {
		t.Fatal(err)
	}
	_, targetID := bootstrap("target")
	added, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, targetID, []string{"MEMBER"}, nil, []uuid.UUID{child.Unit.ID}, "unit-producer-target-add")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("explicit-initialization-and-replay", func(t *testing.T) {
		if !slices.Equal(added.Member.UnitIDs, []uuid.UUID{child.Unit.ID}) || len(added.Member.UnitAdminIDs) != 0 {
			t.Fatal("ordinary attachment confused with authority")
		}
		replay, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, targetID, []string{"MEMBER"}, nil, []uuid.UUID{child.Unit.ID}, "unit-producer-target-add")
		if err != nil || !replay.Replayed || replay.Member.MembershipID != added.Member.MembershipID {
			t.Fatalf("initialization receipt: %+v %v", replay, err)
		}
		if _, err = r.AdminAddMemberWithUnits(ctx, principal, org.ID, targetID, []string{"MEMBER"}, nil, []uuid.UUID{outside.Unit.ID}, "unit-producer-target-add"); !errors.Is(err, account.ErrConflict) {
			t.Fatalf("changed attachment reused receipt: %v", err)
		}
		actorMember, err := r.ListMembers(ctx, principal, org.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range actorMember {
			if item.UserID == actorID && (len(item.UnitIDs) != 0 || len(item.UnitAdminIDs) != 1) {
				t.Fatal("role scope silently created ordinary attachment")
			}
		}
	})
	t.Run("unit-admin-all-scope-replace-replay-and-deny", func(t *testing.T) {
		changed, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, targetID, added.Member.Version, []uuid.UUID{root.Unit.ID, child.Unit.ID}, "unit-producer-replace", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(changed.Member.UnitIDs) != 2 || changed.Member.Version != added.Member.Version+1 {
			t.Fatal("local member version did not advance")
		}
		replay, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, targetID, added.Member.Version, []uuid.UUID{child.Unit.ID, root.Unit.ID}, "unit-producer-replace", false)
		if err != nil || !replay.Replayed || replay.Member.Version != changed.Member.Version {
			t.Fatalf("canonical retry: %+v %v", replay, err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, unitActor, org.ID, targetID, added.Member.Version, []uuid.UUID{outside.Unit.ID}, "unit-producer-replace", false); !errors.Is(err, account.ErrConflict) {
			t.Fatal("different scope reused receipt", err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, unitActor, org.ID, targetID, changed.Member.Version, []uuid.UUID{outside.Unit.ID}, "unit-producer-forbidden", false); !errors.Is(err, account.ErrForbidden) {
			t.Fatal("escaped administrator subtree", err)
		}
		items, err := r.ListMembers(ctx, unitActor, org.ID, &root.Unit.ID)
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, item := range items {
			seen = seen || item.UserID == targetID
		}
		if !seen {
			t.Fatal("legally attached target invisible in subtree")
		}
	})

	t.Run("unscoped-target-not-adopted-by-unit-admin", func(t *testing.T) {
		_, id := bootstrap("unscoped")
		base, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, id, []string{"MEMBER"}, nil, []uuid.UUID{}, "unit-producer-unscoped-add")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, unitActor, org.ID, id, base.Member.Version, []uuid.UUID{child.Unit.ID}, "unit-producer-adopt-unscoped", false); !errors.Is(err, account.ErrForbidden) {
			t.Fatal("unit administrator acquired an unscoped target", err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, principal, org.ID, id, base.Member.Version, []uuid.UUID{child.Unit.ID}, "unit-producer-orgwide-scope", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cross-org-unit-and-platform-flag-do-not-authorize", func(t *testing.T) {
		_, id := bootstrap("foreign_scope")
		base, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, id, []string{"MEMBER"}, nil, []uuid.UUID{child.Unit.ID}, "unit-producer-foreign-add")
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := r.AdminCreateOrganization(ctx, principal, owner, "foreign organization", "unit-producer-foreign-org")
		if err != nil {
			t.Fatal(err)
		}
		otherUnit, err := r.AdminCreateUnit(ctx, principal, foreign.ID, nil, "foreign root", "unit-producer-foreign-unit")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, principal, org.ID, id, base.Member.Version, []uuid.UUID{otherUnit.Unit.ID}, "unit-producer-cross-org-unit", true); !errors.Is(err, account.ErrNotFound) {
			t.Fatal("cross-org unit assigned", err)
		}
		if _, err = r.ReplaceMemberUnits(ctx, unitActor, org.ID, id, base.Member.Version, []uuid.UUID{outside.Unit.ID}, "unit-producer-platform-flag", true); !errors.Is(err, account.ErrForbidden) {
			t.Fatal("caller flag conferred platform authority", err)
		}
	})
	t.Run("remove-clears-projection-and-restores-own-receipt", func(t *testing.T) {
		first, err := r.RevokeMembership(ctx, unitActor, org.ID, targetID, "unit-producer-remove")
		if err != nil {
			t.Fatal(err)
		}
		if first.Member.Status != "REVOKED" || len(first.Member.UnitIDs) != 0 {
			t.Fatal("live attachment survived removal")
		}
		replay, err := r.RevokeMembership(ctx, unitActor, org.ID, targetID, "unit-producer-remove")
		if err != nil || !replay.Replayed || replay.Member.Status != "REVOKED" {
			t.Fatalf("unit remove lost-ACK recovery: %+v %v", replay, err)
		}
		var n int
		if err = c.QueryRow(ctx, `SELECT count(*) FROM membership_commands WHERE command_type='REMOVE' AND target_user_id=$1`, targetID).Scan(&n); err != nil || n != 1 {
			t.Fatal("remove receipt duplicated", n, err)
		}
	})
	t.Run("unit-receipt-current-projection-after-real-remove", func(t *testing.T) {
		var before, after int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1`, added.Member.MembershipID.String()).Scan(&before); err != nil {
			t.Fatal(err)
		}
		replay, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, targetID, added.Member.Version, []uuid.UUID{root.Unit.ID, child.Unit.ID}, "unit-producer-replace", false)
		if err != nil || !replay.Replayed || replay.Member.Status != "REVOKED" || replay.Member.Version <= added.Member.Version+1 || len(replay.Member.UnitIDs) != 0 {
			t.Fatalf("real removed current replay: %+v %v", replay, err)
		}
		if err := c.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1`, added.Member.MembershipID.String()).Scan(&after); err != nil || before != after {
			t.Fatal("replay added audit", before, after, err)
		}
	})
	t.Run("concurrent-cas-one-mutation", func(t *testing.T) {
		_, id := bootstrap("concurrent")
		base, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, id, []string{"MEMBER"}, nil, []uuid.UUID{child.Unit.ID}, "unit-producer-concurrent-add")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, key := range []string{"unit-producer-cas-a", "unit-producer-cas-b"} {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				_, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, id, base.Member.Version, []uuid.UUID{root.Unit.ID}, key, false)
				results <- err
			}(key)
		}
		wg.Wait()
		close(results)
		passed, conflicted := 0, 0
		for err := range results {
			if err == nil {
				passed++
			} else if errors.Is(err, account.ErrConflict) {
				conflicted++
			} else {
				t.Fatal(err)
			}
		}
		if passed != 1 || conflicted != 1 {
			t.Fatalf("CAS outcomes %d/%d", passed, conflicted)
		}
	})
	t.Run("invitation-frozen-scope-real-acceptance", func(t *testing.T) {
		acceptor, id := bootstrap("invited")
		request := account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 24, UnitIDs: []uuid.UUID{child.Unit.ID}, IdempotencyKey: "unit-producer-invitation"}
		invitation, err := r.CreateInvitation(ctx, unitActor, org.ID, request)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(invitation.UnitIDs, request.UnitIDs) {
			t.Fatal("invitation did not freeze attachment")
		}
		if _, err = r.CreateInvitation(ctx, unitActor, org.ID, request); err != nil {
			t.Fatal(err)
		}
		request.UnitIDs = []uuid.UUID{outside.Unit.ID}
		if _, err = r.CreateInvitation(ctx, unitActor, org.ID, request); err == nil {
			t.Fatal("foreign scope accepted by unit actor")
		}
		accepted, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accepted.Member.UserID != id || !slices.Equal(accepted.Member.UnitIDs, []uuid.UUID{child.Unit.ID}) {
			t.Fatal("invitation scope did not reach ordinary membership")
		}
		replay, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
		if err != nil || !replay.Replayed || replay.Member.MembershipID != accepted.Member.MembershipID {
			t.Fatalf("invitation retry: %+v %v", replay, err)
		}
	})

	t.Run("explicit-empty-clears-but-receipt-retains-original-authority", func(t *testing.T) {
		_, id := bootstrap("clear_scope")
		base, err := r.AdminAddMemberWithUnits(ctx, principal, org.ID, id, []string{"MEMBER"}, nil, []uuid.UUID{child.Unit.ID}, "unit-producer-clear-add")
		if err != nil {
			t.Fatal(err)
		}
		first, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, id, base.Member.Version, []uuid.UUID{}, "unit-producer-clear-scope", false)
		if err != nil || len(first.Member.UnitIDs) != 0 {
			t.Fatalf("explicit [] did not clear: %+v %v", first, err)
		}
		replay, err := r.ReplaceMemberUnits(ctx, unitActor, org.ID, id, base.Member.Version, []uuid.UUID{}, "unit-producer-clear-scope", false)
		if err != nil || !replay.Replayed || len(replay.Member.UnitIDs) != 0 {
			t.Fatalf("cleared scope lost own receipt: %+v %v", replay, err)
		}
		var n int
		if err = c.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='membership.units.replace' AND resource_id=$1`, base.Member.MembershipID.String()).Scan(&n); err != nil || n != 1 {
			t.Fatal("retry duplicated mutation audit", n, err)
		}
	})
	t.Run("public-down-retains-scope-history", func(t *testing.T) {
		ordered, err := accountschema.Ordered(m)
		if err != nil {
			t.Fatal(err)
		}
		var before, after int64
		if err = c.QueryRow(ctx, `SELECT count(*) FROM membership_units`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		err = migrations.DownOne(ctx, c, ordered)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "55000" {
			t.Fatal("public downgrade did not fail at forward-only evidence guard", err)
		}
		if err = c.QueryRow(ctx, `SELECT count(*) FROM membership_units`).Scan(&after); err != nil || before != after {
			t.Fatal("failed public rollback changed member attachments", err)
		}
	})
	for _, state := range []string{"current", "revoked", "expired"} {
		for _, explicit := range []bool{false, true} {
			name := "role-authority-" + state + "-legacy"
			var units []uuid.UUID
			if explicit {
				name = "role-authority-" + state + "-explicit"
				units = []uuid.UUID{child.Unit.ID}
			}
			t.Run(name, func(t *testing.T) {
				inviter, inviterID := bootstrap(name + "_inviter")
				created, err := r.AdminAddMember(ctx, principal, org.ID, inviterID, []string{"ORG_ADMIN"}, []uuid.UUID{root.Unit.ID}, name+"-inviter-add")
				if err != nil {
					t.Fatal(err)
				}
				acceptor, acceptorID := bootstrap(name + "_acceptor")
				invitation, err := r.CreateInvitation(ctx, inviter, org.ID, account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"BILLING_ADMIN"}, ExpiresInHours: 24, UnitIDs: units, IdempotencyKey: name + "-create-invitation"})
				if err != nil {
					t.Fatal(err)
				}
				if state == "revoked" {
					if _, err = r.AdminReplaceMemberRoles(ctx, principal, org.ID, inviterID, []string{"MEMBER"}, []uuid.UUID{root.Unit.ID}); err != nil {
						t.Fatal(err)
					}
				}
				if state == "expired" {
					// The normal role-binding expiry column and authority trigger are
					// used; no permission seed or trigger bypass grants authority.
					if _, err = c.Exec(ctx, `UPDATE role_bindings SET expires_at=clock_timestamp()+interval '100 milliseconds' WHERE membership_id=$1 AND role='ORG_ADMIN' AND revoked_at IS NULL`, created.Member.MembershipID); err != nil {
						t.Fatal(err)
					}
					for {
						var expired bool
						if err = c.QueryRow(ctx, `SELECT bool_and(expires_at<=clock_timestamp()) FROM role_bindings WHERE membership_id=$1 AND role='ORG_ADMIN' AND revoked_at IS NULL`, created.Member.MembershipID).Scan(&expired); err != nil {
							t.Fatal(err)
						}
						if expired {
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(10 * time.Millisecond):
						}
					}
				}
				permission, err := r.Authorize(ctx, inviter, account.PermissionOrganizationRolesAssign, &org.ID, nil)
				if err != nil || permission.Allowed != (state == "current") {
					t.Fatal("fixture current role authority mismatch", permission.Allowed, err)
				}
				if explicit && state != "current" {
					members, err := r.Authorize(ctx, inviter, account.PermissionOrganizationMembersManage, &org.ID, &child.Unit.ID)
					if err != nil || !members.Allowed {
						t.Fatal("fixture lost independent scoped member management", err)
					}
				}
				accepted, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
				if state == "current" {
					if err != nil || !slices.Contains(accepted.Member.Roles, "BILLING_ADMIN") || !slices.Equal(accepted.Member.UnitIDs, units) {
						t.Fatal("currently authorized frozen role was not accepted", err)
					}
				} else {
					if !errors.Is(err, account.ErrForbidden) {
						t.Fatalf("revoked/expired role authority accepted elevated invitation: %v", err)
					}
					var memberships, roles int
					if err = c.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE organization_id=$1 AND user_id=$2`, org.ID, acceptorID).Scan(&memberships); err != nil {
						t.Fatal(err)
					}
					if err = c.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE organization_id=$1 AND user_id=$2 AND revoked_at IS NULL`, org.ID, acceptorID).Scan(&roles); err != nil {
						t.Fatal(err)
					}
					if memberships != 0 || roles != 0 {
						t.Fatal("denied acceptance committed membership or role effects", memberships, roles)
					}
				}
			})
		}
	}
	t.Run("accepted-role-replay-does-not-regrant-after-inviter-revoked", func(t *testing.T) {
		inviter, id := bootstrap("accepted_role_inviter")
		if _, err = r.AdminAddMember(ctx, principal, org.ID, id, []string{"ORG_ADMIN"}, nil, "accepted-role-inviter-add"); err != nil {
			t.Fatal(err)
		}
		acceptor, _ := bootstrap("accepted_role_target")
		invitation, err := r.CreateInvitation(ctx, inviter, org.ID, account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"BILLING_ADMIN"}, ExpiresInHours: 24, IdempotencyKey: "accepted-role-invitation"})
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.AdminRemoveMember(ctx, principal, org.ID, id, "accepted-role-inviter-remove"); err != nil {
			t.Fatal(err)
		}
		replay, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
		if err != nil || !replay.Replayed || replay.Member.MembershipID != accepted.Member.MembershipID || replay.Member.Version != accepted.Member.Version {
			t.Fatal("already accepted receipt incorrectly required new grant authority", err)
		}
		var n int
		if err = c.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='invitation.accept' AND resource_id=$1`, invitation.ID.String()).Scan(&n); err != nil || n != 1 {
			t.Fatal("accepted replay created another grant audit", n, err)
		}
	})
	t.Run("legacy-member-only-invitation-preserves-frozen-intent", func(t *testing.T) {
		inviter, id := bootstrap("ordinary_legacy_inviter")
		if _, err = r.AdminAddMember(ctx, principal, org.ID, id, []string{"ORG_ADMIN"}, nil, "ordinary-legacy-inviter-add"); err != nil {
			t.Fatal(err)
		}
		acceptor, _ := bootstrap("ordinary_legacy_target")
		invitation, err := r.CreateInvitation(ctx, inviter, org.ID, account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 24, IdempotencyKey: "ordinary-legacy-invitation"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.AdminRemoveMember(ctx, principal, org.ID, id, "ordinary-legacy-inviter-remove"); err != nil {
			t.Fatal(err)
		}
		accepted, err := r.AcceptInvitation(ctx, acceptor, invitation.Token, nil)
		if err != nil || !slices.Equal(accepted.Member.Roles, []string{"MEMBER"}) || len(accepted.Member.UnitIDs) != 0 {
			t.Fatal("ordinary legacy invitation changed semantics", err)
		}
	})
	t.Run("revoked-inviter-denies-new-acceptance", func(t *testing.T) {
		acceptor, _ := bootstrap("revoked_invite")
		invitation, err := r.CreateInvitation(ctx, unitActor, org.ID, account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 24, UnitIDs: []uuid.UUID{child.Unit.ID}, IdempotencyKey: "unit-producer-revoked-invitation"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.AdminRemoveMember(ctx, principal, org.ID, actorID, "unit-producer-remove-actor"); err != nil {
			t.Fatal(err)
		}
		if _, err = r.AcceptInvitation(ctx, acceptor, invitation.Token, nil); !errors.Is(err, account.ErrForbidden) {
			t.Fatal("revoked inviter scope retained authority", err)
		}
		if _, err = r.RevokeMembership(ctx, unitActor, org.ID, targetID, "unit-producer-remove"); !errors.Is(err, account.ErrForbidden) {
			t.Fatal("revoked actor replay retained old authorization", err)
		}
	})
}
