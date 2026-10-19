package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Delegates to a real transaction while counting the round trips at the
// changed helper boundary. No SQL result or permission fact is simulated.
type f06UnitRoundTripTx struct {
	pgx.Tx
	queries, executions int
}

func (tx *f06UnitRoundTripTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.queries++
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *f06UnitRoundTripTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.executions++
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestF06Review5566377593PublicPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "r" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	owner, ownerID := user()
	outsider, _ := user()
	_, target := user()
	org, err := r.AdminCreateOrganization(ctx, owner, ownerID, "Review role and unit boundaries", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	// Explicit AdminAdd member attachments use the existing platform fence;
	// derive the fixture actor's grant from the installed permission catalogue.
	grant, err := r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at)
SELECT $1,$2,role.code,$2,clock_timestamp(),clock_timestamp() FROM roles role
WHERE role.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions p WHERE p.role_code=role.code AND p.permission_code=$3)
ORDER BY role.code LIMIT 1`, uuid.New(), ownerID, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage))
	if err != nil || grant.RowsAffected() != 1 {
		t.Fatal("installed platform membership permission was not provisioned for fixture actor", err)
	}
	member, err := r.AdminAddMember(ctx, owner, org.ID, target, []string{"MEMBER"}, nil, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unit, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "Initial unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),
'members',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),
'units',(SELECT jsonb_agg(to_jsonb(x) ORDER BY membership_id,unit_id) FROM membership_units x),
'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),
'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),
'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY actor_user_id,organization_id,idempotency_key) FROM membership_commands x),
'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("non-active-targets-reject-both-role-entrypoints-without-effects", func(t *testing.T) {
		for _, status := range []string{"DISABLED", "PROVISIONING", "REPAIR_REQUIRED", "BOOTSTRAP_PENDING", "DELETED"} {
			t.Run(status, func(t *testing.T) {
				if _, err := r.pool.Exec(ctx, `UPDATE users SET status=$2 WHERE id=$1`, target, status); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, target); err != nil {
						t.Error(err)
					}
				})
				before := snapshot()
				_, grantErr := r.GrantOrganizationRoleBinding(ctx, owner, RoleBindingGrant{UserID: target, RoleCode: "AUDITOR", OrganizationID: &org.ID, IdempotencyKey: uuid.NewString()})
				_, replaceErr := r.AdminReplaceMemberRoles(ctx, owner, org.ID, target, []string{"AUDITOR"}, nil)
				if !errors.Is(grantErr, ErrNotFound) || !errors.Is(replaceErr, ErrNotFound) || snapshot() != before {
					t.Fatal("inactive target received grants or changed durable facts", grantErr, replaceErr)
				}
			})
		}
	})
	t.Run("missing-admin-actor-never-becomes-target-created-by", func(t *testing.T) {
		before := snapshot()
		_, err := r.AdminReplaceMemberRoles(ctx, identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString()}, org.ID, target, []string{"AUDITOR"}, nil)
		if !errors.Is(err, ErrForbidden) || snapshot() != before {
			t.Fatal("unknown actor mutated roles or audit attribution", err)
		}
	})
	t.Run("unit-existence-is-hidden-before-authority", func(t *testing.T) {
		for _, id := range []uuid.UUID{unit.Unit.ID, uuid.New()} {
			before := snapshot()
			_, err := r.GrantOrganizationRoleBinding(ctx, outsider, RoleBindingGrant{UserID: target, RoleCode: "UNIT_ADMIN", OrganizationID: &org.ID, UnitID: &id, IdempotencyKey: uuid.NewString()})
			if !errors.Is(err, ErrForbidden) || snapshot() != before {
				t.Fatal("unauthorized unit existence escaped or denial mutated facts", err)
			}
		}
		missing := uuid.New()
		before := snapshot()
		_, err := r.GrantOrganizationRoleBinding(ctx, owner, RoleBindingGrant{UserID: target, RoleCode: "UNIT_ADMIN", OrganizationID: &org.ID, UnitID: &missing, IdempotencyKey: uuid.NewString()})
		if !errors.Is(err, ErrNotFound) || snapshot() != before {
			t.Fatal("authorized missing-unit response changed", err)
		}
	})
	t.Run("active-role-grant-and-original-default-start-replay", func(t *testing.T) {
		request := RoleBindingGrant{UserID: target, RoleCode: "UNIT_ADMIN", OrganizationID: &org.ID, UnitID: &unit.Unit.ID, IdempotencyKey: uuid.NewString()}
		binding, err := r.GrantOrganizationRoleBinding(ctx, owner, request)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		replay, err := r.GrantOrganizationRoleBinding(ctx, owner, request)
		if err != nil || replay.ID != binding.ID || !replay.StartsAt.Equal(binding.StartsAt) || snapshot() != before {
			t.Fatal("active grant replay changed its frozen identity or effects", err)
		}
		if _, err := r.AdminReplaceMemberRoles(ctx, owner, org.ID, target, []string{"AUDITOR"}, nil); err != nil {
			t.Fatal("active admin role replacement stopped working", err)
		}
	})
	for _, entrypoint := range []string{"grant", "admin-replace"} {
		t.Run("concurrent-disable-fences-"+entrypoint, func(t *testing.T) {
			before := snapshot()
			blocker, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(ctx)
			if _, err := blocker.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, target); err != nil {
				t.Fatal(err)
			}
			workerCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			done := make(chan error, 1)
			go func() {
				if entrypoint == "grant" {
					_, err := r.GrantOrganizationRoleBinding(workerCtx, owner, RoleBindingGrant{UserID: target, RoleCode: "ORG_ADMIN", OrganizationID: &org.ID, IdempotencyKey: uuid.NewString()})
					done <- err
				} else {
					_, err := r.AdminReplaceMemberRoles(workerCtx, owner, org.ID, target, []string{"ORG_ADMIN"}, nil)
					done <- err
				}
			}()
			// Observe the actual target-admission query waiting on the user row,
			// rather than inferring a fence from a sleep or a later cache write.
			locked := false
			waitUntil := time.Now().Add(3 * time.Second)
			for !locked && time.Now().Before(waitUntil) {
				if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%target.status=''ACTIVE''%' AND query LIKE '%FOR UPDATE OF member,target%')`, r.pool.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&locked); err != nil {
					blocker.Rollback(ctx)
					stop()
					t.Fatal(err)
				}
				if !locked {
					time.Sleep(5 * time.Millisecond)
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-workerCtx.Done():
				t.Fatal("role mutation failed to finish after owned blocker committed")
			}
			if _, resetErr := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, target); resetErr != nil {
				t.Fatal(resetErr)
			}
			if !locked || err == nil || snapshot() != before {
				t.Fatal("target admission did not serialize with disable or mutated grants", locked, err)
			}
		})
	}
	t.Run("ordinary-and-admin-scope-may-overlap", func(t *testing.T) {
		_, overlap := user()
		added, err := r.AdminAddMemberWithUnits(ctx, owner, org.ID, overlap, []string{"MEMBER"}, []uuid.UUID{unit.Unit.ID}, []uuid.UUID{unit.Unit.ID}, uuid.NewString())
		if err != nil || !slices.Equal(added.Member.UnitIDs, []uuid.UUID{unit.Unit.ID}) || !slices.Equal(added.Member.UnitAdminIDs, []uuid.UUID{unit.Unit.ID}) {
			t.Fatal("valid overlap was mistaken for missing units", err)
		}
	})
	ids := []uuid.UUID{unit.Unit.ID}
	for i := 1; i < 205; i++ {
		created, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, fmt.Sprintf("Large set %03d", i), uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.Unit.ID)
	}
	canonical, err := normalizedMemberUnits(ids)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("large-and-duplicate-sets-use-fixed-sql-round-trips", func(t *testing.T) {
		before := snapshot()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		tracked := &f06UnitRoundTripTx{Tx: tx}
		if err := validateAdminUnits(ctx, tracked, org.ID, append(slices.Clone(canonical), canonical[0])); err != nil || tracked.queries != 1 {
			t.Fatal("large/duplicate validation changed or still fans out", tracked.queries, err)
		}
		if err := writeMemberUnits(ctx, tracked, member.Member.MembershipID, canonical, time.Now().UTC()); err != nil || tracked.executions != 2 {
			t.Fatal("large attachment write changed or still fans out", tracked.executions, err)
		}
		stored, err := readMemberUnits(ctx, tx, member.Member.MembershipID)
		if err != nil || !slices.Equal(stored, canonical) {
			t.Fatal("bulk insert did not retain every distinct unit", err)
		}
		if err := tx.Rollback(ctx); err != nil || snapshot() != before {
			t.Fatal("helper rollback changed durable facts", err)
		}
	})
	t.Run("large-replacement-and-normalized-replay-remain-complete", func(t *testing.T) {
		var version int64
		if err := r.pool.QueryRow(ctx, `SELECT version FROM memberships WHERE id=$1`, member.Member.MembershipID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		key := uuid.NewString()
		input := append(slices.Clone(ids), ids[0])
		changed, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, version, input, key, false)
		if err != nil || changed.Replayed || changed.Member.Version != version+1 || !slices.Equal(changed.Member.UnitIDs, canonical) {
			t.Fatal("existing greater-than-200 acceptance or canonical output changed", err)
		}
		member = changed
		before := snapshot()
		replay, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, version, canonical, key, false)
		if err != nil || !replay.Replayed || snapshot() != before {
			t.Fatal("same normalized intent acquired another effect", err)
		}
	})
	t.Run("large-unit-admin-batch-preserves-author-time-order-and-effects", func(t *testing.T) {
		var auditBefore, outboxBefore int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_logs),(SELECT count(*) FROM outbox_events)`).Scan(&auditBefore, &outboxBefore); err != nil {
			t.Fatal(err)
		}
		updated, err := r.AdminReplaceMemberRoles(ctx, owner, org.ID, target, []string{"AUDITOR"}, append(slices.Clone(ids), ids[0]))
		if err != nil || updated.Version != member.Member.Version+1 || !slices.Equal(updated.UnitAdminIDs, canonical) || !slices.Equal(updated.UnitIDs, canonical) {
			t.Fatal("large admin scopes lost units, version or independent ordinary scope", err)
		}
		member.Member = updated
		rows, err := r.pool.Query(ctx, `SELECT unit_id,id,created_by,created_at,starts_at,grant_source FROM role_bindings WHERE membership_id=$1 AND role='UNIT_ADMIN' AND revoked_at IS NULL ORDER BY unit_id`, member.Member.MembershipID)
		if err != nil {
			t.Fatal(err)
		}
		var stored []uuid.UUID
		var prior uuid.UUID
		var common time.Time
		for rows.Next() {
			var unitID, bindingID, actorID uuid.UUID
			var createdAt, startsAt time.Time
			var source string
			if err := rows.Scan(&unitID, &bindingID, &actorID, &createdAt, &startsAt, &source); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if len(stored) == 0 {
				common = createdAt
			}
			if bindingID.Version() != 7 || actorID != ownerID || source != "ADMIN" || !createdAt.Equal(common) || !startsAt.Equal(common) || prior != uuid.Nil && bindingID.String() <= prior.String() {
				rows.Close()
				t.Fatal("batch changed UUIDv7 generation order, actor or shared time")
			}
			stored = append(stored, unitID)
			prior = bindingID
		}
		rows.Close()
		if err := rows.Err(); err != nil || !slices.Equal(stored, canonical) {
			t.Fatal("not every canonical admin unit received one binding", err)
		}
		var auditAfter, outboxAfter int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_logs),(SELECT count(*) FROM outbox_events)`).Scan(&auditAfter, &outboxAfter); err != nil || auditAfter != auditBefore+1 || outboxAfter != outboxBefore+1 {
			t.Fatal("admin batch changed audit or lifecycle publication count", err)
		}
	})
	t.Run("unit-admin-helper-round-trips-are-independent-of-unit-count", func(t *testing.T) {
		before := snapshot()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		tracked := &f06UnitRoundTripTx{Tx: tx}
		if _, err := replaceAdminMemberRoles(ctx, tracked, member.Member.MembershipID, target, ownerID, []string{"MEMBER"}, canonical, time.Now().UTC()); err != nil || tracked.executions != 5 || tracked.queries != 1 {
			t.Fatal("large admin binding insertion still fans out or changed effects", tracked.executions, tracked.queries, err)
		}
		if err := tx.Rollback(ctx); err != nil || snapshot() != before {
			t.Fatal("admin helper rollback changed durable roles", err)
		}
	})
	t.Run("large-admin-scope-rejection-is-atomic", func(t *testing.T) {
		for _, id := range []uuid.UUID{uuid.Nil, uuid.New()} {
			before := snapshot()
			_, err := r.AdminReplaceMemberRoles(ctx, owner, org.ID, target, []string{"ORG_ADMIN"}, append(slices.Clone(canonical), id))
			want := ErrNotFound
			if id == uuid.Nil {
				want = ErrConflict
			}
			if !errors.Is(err, want) || snapshot() != before {
				t.Fatal("invalid admin scope changed grants or audit facts", err)
			}
		}
	})
	t.Run("failed-admin-batch-rolls-back-revocation-and-inserts", func(t *testing.T) {
		for _, bad := range [][]uuid.UUID{{ids[0], uuid.New()}, {ids[0], ids[0]}} {
			before := snapshot()
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = replaceAdminMemberRoles(ctx, tx, member.Member.MembershipID, target, ownerID, []string{"MEMBER"}, bad, time.Now().UTC())
			if err == nil {
				tx.Rollback(ctx)
				t.Fatal("invalid admin helper input unexpectedly succeeded")
			}
			if err := tx.Rollback(ctx); err != nil || snapshot() != before {
				t.Fatal("failed admin insert lost original grants or audit facts", err)
			}
		}
	})
	t.Run("invalid-and-missing-units-atomically-reject", func(t *testing.T) {
		for _, id := range []uuid.UUID{uuid.Nil, uuid.New()} {
			before := snapshot()
			_, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, member.Member.Version, append(slices.Clone(ids), id), uuid.NewString(), false)
			want := ErrNotFound
			if id == uuid.Nil {
				want = ErrConflict
			}
			if !errors.Is(err, want) || snapshot() != before {
				t.Fatal("invalid large request changed durable scope or command facts", err)
			}
		}
	})
	t.Run("failed-bulk-insert-rolls-back-original-attachments", func(t *testing.T) {
		for _, bad := range [][]uuid.UUID{{ids[0], uuid.New()}, {ids[0], ids[0]}} {
			before := snapshot()
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = writeMemberUnits(ctx, tx, member.Member.MembershipID, bad, time.Now().UTC())
			if err == nil {
				tx.Rollback(ctx)
				t.Fatal("invalid helper input unexpectedly committed")
			}
			if err := tx.Rollback(ctx); err != nil || snapshot() != before {
				t.Fatal("failed helper insert lost original attachments", err)
			}
		}
	})
}
