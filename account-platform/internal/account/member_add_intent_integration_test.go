package account

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Entire installed account migration chain, genuine account_service actor.
func TestF06ActiveMemberAddPreservesIntentAndReceipt(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	user := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "m" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	actor, owner := user()
	_, target := user()
	org, err := r.AdminCreateOrganization(ctx, actor, owner, "Controlled organization", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unitA, err := r.AdminCreateUnit(ctx, actor, org.ID, nil, "Unit A", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unitB, err := r.AdminCreateUnit(ctx, actor, org.ID, nil, "Unit B", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	// Grant an installed platform role through ordinary fixture setup, not a
	// synthetic permission catalog or a bypass of production authorization.
	if _, err = r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) SELECT $1,$2,role.code,$2,clock_timestamp(),clock_timestamp() FROM roles role WHERE role.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions p WHERE p.role_code=role.code AND p.permission_code=$3) ORDER BY role.code LIMIT 1`, uuid.New(), owner, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	first, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, []string{"AUDITOR"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{unitA.Unit.ID}, key)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m),'grants',(SELECT jsonb_agg(to_jsonb(b) ORDER BY id) FROM role_bindings b),'commands',(SELECT count(*) FROM membership_commands),'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events),'user',(SELECT to_jsonb(u) FROM users u WHERE id=$1))::text`, target).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, scenario := range []struct {
		name          string
		roles         []string
		admins, units []uuid.UUID
	}{
		{"role change", []string{"ORG_ADMIN"}, []uuid.UUID{unitA.Unit.ID}, nil},
		{"unit admin change", []string{"AUDITOR"}, []uuid.UUID{unitB.Unit.ID}, nil},
		{"explicit ordinary units change", []string{"AUDITOR"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := snapshot()
			_, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, scenario.roles, scenario.admins, scenario.units, uuid.NewString())
			if !errors.Is(err, ErrConflict) || snapshot() != before {
				t.Fatalf("changed ADD was accepted or mutated facts: %v", err)
			}
		})
	}
	t.Run("same facts new key is a no-op", func(t *testing.T) {
		result, err := r.AdminAddMember(ctx, actor, org.ID, target, []string{"AUDITOR", "MEMBER", "AUDITOR"}, []uuid.UUID{unitA.Unit.ID}, uuid.NewString())
		if err != nil || !result.Replayed || result.Member.Version != first.Member.Version {
			t.Fatal(result, err)
		}
	})
	t.Run("old receipt returns current roles without regrant", func(t *testing.T) {
		current, err := r.AdminReplaceMemberRoles(ctx, actor, org.ID, target, []string{"ORG_ADMIN"}, []uuid.UUID{unitB.Unit.ID})
		if err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		result, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, []string{"AUDITOR"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{unitA.Unit.ID}, key)
		if err != nil || !result.Replayed || result.Member.Version != current.Version || !slices.Equal(result.Member.Roles, current.Roles) || snapshot() != before {
			t.Fatal("receipt regranted historical roles", result, err)
		}
		if _, err = r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, []string{"ORG_ADMIN"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{unitA.Unit.ID}, key); !errors.Is(err, ErrConflict) {
			t.Fatal("same key changed facts", err)
		}
	})
	t.Run("owner implicit member remains owner without mutation", func(t *testing.T) {
		result, err := r.AdminAddMember(ctx, actor, org.ID, owner, nil, nil, uuid.NewString())
		if err != nil || !result.Replayed || !slices.Equal(result.Member.Roles, []string{"OWNER"}) {
			t.Fatal(result, err)
		}
		before := snapshot()
		if _, err = r.AdminAddMember(ctx, actor, org.ID, owner, []string{"AUDITOR"}, nil, uuid.NewString()); !errors.Is(err, ErrConflict) || snapshot() != before {
			t.Fatal("owner ADD replaced grants", err)
		}
	})
	t.Run("revoked receipt stays revoked and a fresh add reactivates", func(t *testing.T) {
		removed, err := r.AdminRemoveMember(ctx, actor, org.ID, target, uuid.NewString())
		if err != nil || removed.Member.Status != "REVOKED" {
			t.Fatal(removed, err)
		}
		replay, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, []string{"AUDITOR"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{unitA.Unit.ID}, key)
		if err != nil || replay.Member.Status != "REVOKED" || !replay.Replayed {
			t.Fatal("historical add reactivated membership", replay, err)
		}
		added, err := r.AdminAddMember(ctx, actor, org.ID, target, []string{"AUDITOR"}, []uuid.UUID{unitB.Unit.ID}, uuid.NewString())
		if err != nil || added.Member.Status != "ACTIVE" || added.Replayed || !slices.Contains(added.Member.Roles, "AUDITOR") || added.Member.Version <= removed.Member.Version {
			t.Fatal("fresh activation lost its grants", added, err)
		}
	})
	t.Run("expired and future grants are not current permissions", func(t *testing.T) {
		for _, timing := range []string{"expired", "future"} {
			_, userID := user()
			added, err := r.AdminAddMember(ctx, actor, org.ID, userID, []string{"AUDITOR"}, nil, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			statement := `UPDATE role_bindings SET starts_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE membership_id=$1 AND role='AUDITOR'`
			if timing == "future" {
				statement = `UPDATE role_bindings SET starts_at=clock_timestamp()+interval '1 hour' WHERE membership_id=$1 AND role='AUDITOR'`
			}
			if _, err = r.pool.Exec(ctx, statement, added.Member.MembershipID); err != nil {
				t.Fatal(err)
			}
			if _, err = r.AdminAddMember(ctx, actor, org.ID, userID, []string{"AUDITOR"}, nil, uuid.NewString()); !errors.Is(err, ErrConflict) {
				t.Fatal("inactive grant accepted as current", timing, err)
			}
			if result, err := r.AdminAddMember(ctx, actor, org.ID, userID, nil, nil, uuid.NewString()); err != nil || !result.Replayed || !slices.Equal(result.Member.Roles, []string{"MEMBER"}) {
				t.Fatal("effective baseline no-op rejected", timing, result, err)
			}
		}
	})
}
