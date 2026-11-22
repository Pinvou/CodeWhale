package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Original public migration/role fixture; no database fallback or synthetic
// permission catalogue. Root executes this opt-in PG formal under its token.
func TestF06MemberUnitsAuthorityBeforeTargetFacts(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	newUser := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	owner, ownerID := newUser()
	outsider, _ := newUser()
	scoped, scopedID := newUser()
	_, target := newUser()
	org, err := r.AdminCreateOrganization(ctx, owner, ownerID, "Controlled member authority", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unitA, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "A", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unitB, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "B", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) SELECT $1,$2,role.code,$2,clock_timestamp(),clock_timestamp() FROM roles role WHERE role.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions p WHERE p.role_code=role.code AND p.permission_code=$3) ORDER BY role.code LIMIT 1`, uuid.New(), ownerID, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)); err != nil {
		t.Fatal(err)
	}
	member, err := r.AdminAddMemberWithUnits(ctx, owner, org.ID, target, []string{"MEMBER"}, nil, []uuid.UUID{unitA.Unit.ID}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AdminAddMemberWithUnits(ctx, owner, org.ID, scopedID, []string{"MEMBER"}, []uuid.UUID{unitA.Unit.ID}, []uuid.UUID{unitA.Unit.ID}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m),'units',(SELECT jsonb_agg(to_jsonb(u) ORDER BY membership_id,unit_id) FROM membership_units u),'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY id) FROM role_bindings b),'commands',(SELECT jsonb_agg(to_jsonb(c) ORDER BY idempotency_key) FROM membership_commands c),'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events),'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u))::text`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, tc := range []struct {
		name        string
		actor       identity.Principal
		org, target uuid.UUID
		version     int64
		units       []uuid.UUID
		want        error
	}{
		{"unauthorized-existing", outsider, org.ID, target, member.Member.Version, []uuid.UUID{}, ErrForbidden},
		{"unauthorized-missing-member", outsider, org.ID, uuid.New(), member.Member.Version, []uuid.UUID{}, ErrForbidden},
		{"unauthorized-stale-version", outsider, org.ID, target, member.Member.Version + 1, []uuid.UUID{}, ErrForbidden},
		{"unauthorized-missing-org", outsider, uuid.New(), target, member.Member.Version, []uuid.UUID{}, ErrForbidden},
		{"owner-missing-member", owner, org.ID, uuid.New(), member.Member.Version, []uuid.UUID{}, ErrNotFound},
		{"owner-stale-version", owner, org.ID, target, member.Member.Version + 1, []uuid.UUID{}, ErrConflict},
		{"scoped-cannot-move-outside-authority", scoped, org.ID, target, member.Member.Version, []uuid.UUID{unitB.Unit.ID}, ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot()
			_, err := r.ReplaceMemberUnits(ctx, tc.actor, tc.org, tc.target, tc.version, tc.units, uuid.NewString(), false)
			if !errors.Is(err, tc.want) || snapshot() != before {
				t.Fatalf("authority/unchanged facts: %v want %v", err, tc.want)
			}
		})
	}
	scopedKey, scopedVersion := uuid.NewString(), member.Member.Version
	t.Run("scoped-current-target-allowed", func(t *testing.T) {
		changed, err := r.ReplaceMemberUnits(ctx, scoped, org.ID, target, scopedVersion, []uuid.UUID{unitA.Unit.ID}, scopedKey, false)
		if err != nil || changed.Member.Version != member.Member.Version+1 {
			t.Fatal("current scoped manager denied", err)
		}
		member = changed
	})
	replay := func(t *testing.T) {
		t.Helper()
		before := snapshot()
		result, err := r.ReplaceMemberUnits(ctx, scoped, org.ID, target, scopedVersion, []uuid.UUID{unitA.Unit.ID}, scopedKey, false)
		if err != nil || !result.Replayed || result.Member.Version != member.Member.Version || snapshot() != before {
			t.Fatal("own receipt lost immutable scope or mutated current facts", result, err)
		}
	}
	t.Run("owner-current-target-allowed", func(t *testing.T) {
		changed, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, member.Member.Version, []uuid.UUID{unitB.Unit.ID}, uuid.NewString(), false)
		if err != nil || changed.Member.Version != member.Member.Version+1 {
			t.Fatal("organization-wide manager denied", err)
		}
		member = changed
	})
	t.Run("scoped-original-receipt-after-target-move", replay)
	t.Run("scoped-new-target-scope-denied", func(t *testing.T) {
		before := snapshot()
		_, err := r.ReplaceMemberUnits(ctx, scoped, org.ID, target, member.Member.Version+1, []uuid.UUID{unitA.Unit.ID}, uuid.NewString(), false)
		if !errors.Is(err, ErrForbidden) || snapshot() != before {
			t.Fatal("target version revealed outside current scope", err)
		}
	})
	t.Run("inactive-target-remains-rejected", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		_, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, member.Member.Version, []uuid.UUID{}, uuid.NewString(), false)
		if !errors.Is(err, ErrNotFound) || snapshot() != before {
			t.Fatal("inactive target admission changed", err)
		}
	})
	t.Run("scoped-original-receipt-after-target-disable", replay)
}
