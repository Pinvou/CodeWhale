package account

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Real public Account migrations, non-owner runtime and original finite owned
// fixture. Only Root executes PG. No remote projection/deployment is claimed.
func TestF06OrganizationDeletionVersionAndTerminalScope(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	user := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "d" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
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
	foreignActor, foreignOwner := user()
	_, memberID := user()
	org, err := r.AdminCreateOrganization(ctx, actor, owner, "Deletion target", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := r.AdminCreateOrganization(ctx, foreignActor, foreignOwner, "Foreign preserved organization", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unit, err := r.AdminCreateUnit(ctx, actor, org.ID, nil, "Attached unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	foreignUnit, err := r.AdminCreateUnit(ctx, foreignActor, foreign.ID, nil, "Foreign unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) SELECT $1,$2,role.code,$2,clock_timestamp(),clock_timestamp() FROM roles role WHERE role.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions p WHERE p.role_code=role.code AND p.permission_code=$3) ORDER BY role.code LIMIT 1`, uuid.New(), owner, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AdminAddMemberWithUnits(ctx, actor, org.ID, memberID, []string{"AUDITOR"}, []uuid.UUID{unit.Unit.ID}, []uuid.UUID{unit.Unit.ID}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO membership_units(membership_id,unit_id,created_at) SELECT id,$2,clock_timestamp() FROM memberships WHERE organization_id=$1 AND user_id=$3 ON CONFLICT DO NOTHING`, foreign.ID, foreignUnit.Unit.ID, foreignOwner); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('organization',(SELECT to_jsonb(o) FROM organizations o WHERE id=$1),'memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m WHERE organization_id=$1),'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.id) FROM role_bindings b JOIN memberships m ON m.id=b.membership_id WHERE m.organization_id=$1),'units',(SELECT jsonb_agg(to_jsonb(u) ORDER BY membership_id,unit_id) FROM membership_units u JOIN memberships m ON m.id=u.membership_id WHERE m.organization_id=$1),'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events))::text`, org.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	foreignSnapshot := func() string {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('organization',(SELECT to_jsonb(o) FROM organizations o WHERE id=$1),'memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m WHERE organization_id=$1),'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.id) FROM role_bindings b JOIN memberships m ON m.id=b.membership_id WHERE m.organization_id=$1),'units',(SELECT jsonb_agg(to_jsonb(u) ORDER BY membership_id,unit_id) FROM membership_units u JOIN memberships m ON m.id=u.membership_id WHERE m.organization_id=$1))::text`, foreign.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	state, err := r.AdminOrganizationState(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("active-not-deletable", func(t *testing.T) {
		before := snapshot()
		err := r.AdminDeleteOrganization(ctx, actor, org.ID, state.Version, "controlled deletion")
		if !errors.Is(err, ErrConflict) || snapshot() != before {
			t.Fatal("active organization deleted or changed", err)
		}
	})
	if err = r.AdminSetOrganizationStatus(ctx, actor, org.ID, "DISABLED"); err != nil {
		t.Fatal(err)
	}
	state, err = r.AdminOrganizationState(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("stale-observed-version-no-effects", func(t *testing.T) {
		before := snapshot()
		err := r.AdminDeleteOrganization(ctx, actor, org.ID, state.Version-1, "controlled deletion")
		if !errors.Is(err, ErrConflict) || snapshot() != before {
			t.Fatal("stale remote tuple deleted current local state", err)
		}
	})
	t.Run("contending-version-change-fails-closed", func(t *testing.T) {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		lock, err := r.pool.Begin(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var version int64
		if err = lock.QueryRow(ctx, `SELECT version FROM organizations WHERE id=$1 FOR UPDATE`, org.ID).Scan(&version); err != nil {
			_ = lock.Rollback(ctx)
			cancel()
			t.Fatal(err)
		}
		var worker sync.WaitGroup
		worker.Add(1)
		result := make(chan error, 1)
		defer func() {
			cancel()
			if err := lock.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("owned lock cleanup failed", err)
			}
			worker.Wait()
		}()
		go func() {
			defer worker.Done()
			result <- r.AdminDeleteOrganization(callCtx, actor, org.ID, version, "controlled deletion")
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND wait_event_type='Lock' AND query LIKE '%SELECT status,version FROM organizations%'`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("actual deletion did not reach its existing row lock")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err = lock.Exec(ctx, `UPDATE organizations SET version=version+1 WHERE id=$1`, org.ID); err != nil {
			t.Fatal(err)
		}
		if err = lock.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-result:
		case <-callCtx.Done():
			t.Fatal("contending operation did not finish")
		}
		var serialization *pgconn.PgError
		if !errors.Is(err, ErrConflict) && !(errors.As(err, &serialization) && serialization.Code == "40001") {
			t.Fatal("stale snapshot was not rejected", err)
		}
		current, err := r.AdminOrganizationState(ctx, org.ID)
		if err != nil || current.Status != "DISABLED" || current.Version != version+1 {
			t.Fatal("contender deleted newer local lifecycle", current, err)
		}
		state = current
	})
	foreignBefore := foreignSnapshot()
	var bindingsBefore int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings binding JOIN memberships member ON member.id=binding.membership_id WHERE member.organization_id=$1`, org.ID).Scan(&bindingsBefore); err != nil || bindingsBefore < 3 {
		t.Fatal("original OWNER/member/unit grants not seeded", bindingsBefore, err)
	}
	t.Run("terminal-only-cleanup-keeps-history", func(t *testing.T) {
		if err := r.AdminDeleteOrganization(ctx, actor, org.ID, state.Version, "controlled deletion"); err != nil {
			t.Fatal(err)
		}
		var total, live, attached, active, audits, events int
		err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM role_bindings b JOIN memberships m ON m.id=b.membership_id WHERE m.organization_id=$1),(SELECT count(*) FROM role_bindings b JOIN memberships m ON m.id=b.membership_id WHERE m.organization_id=$1 AND b.revoked_at IS NULL),(SELECT count(*) FROM membership_units u JOIN memberships m ON m.id=u.membership_id WHERE m.organization_id=$1),(SELECT count(*) FROM memberships WHERE organization_id=$1 AND status<>'REVOKED'),(SELECT count(*) FROM audit_logs WHERE resource_id=$1::text AND action='admin.organization.delete'),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.organization.deleted.v1')`, org.ID).Scan(&total, &live, &attached, &active, &audits, &events)
		current, stateErr := r.AdminOrganizationState(ctx, org.ID)
		if err != nil || stateErr != nil || total != bindingsBefore || live != 0 || attached != 0 || active != 0 || audits != 1 || events != 1 || current.Status != "DELETED" || current.Version != state.Version+1 || foreignSnapshot() != foreignBefore {
			t.Fatal("terminal scope/history or version violated", total, live, attached, active, audits, events, current, err, stateErr)
		}
	})
	t.Run("exact-terminal-retry-no-effects", func(t *testing.T) {
		before := snapshot()
		err := r.AdminDeleteOrganization(ctx, actor, org.ID, state.Version, "controlled deletion")
		if err != nil || snapshot() != before {
			t.Fatal("same confirmed tuple was not idempotent", err)
		}
	})
	t.Run("different-terminal-tuple-rejected", func(t *testing.T) {
		before := snapshot()
		err := r.AdminDeleteOrganization(ctx, actor, org.ID, state.Version+1, "controlled deletion")
		if !errors.Is(err, ErrConflict) || snapshot() != before {
			t.Fatal("unconfirmed terminal tuple accepted", err)
		}
	})
}
