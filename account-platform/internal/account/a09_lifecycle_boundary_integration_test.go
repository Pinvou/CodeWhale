package account

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresA09InvitationExpiryIsCurrentAfterOrganizationLock(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	invitation, err := repository.CreateInvitation(context.Background(), owner, organization.ID, InvitationCreateRequest{TargetType: "EMAIL", Target: member.Email, Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var boundary time.Time
	if err := repository.pool.QueryRow(ctx, `UPDATE invitations SET expires_at=statement_timestamp()+interval '1 second' WHERE id=$1 RETURNING expires_at`, invitation.ID).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	gate, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var blocker int
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, organization.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := repository.AcceptInvitation(ctx, member, invitation.Token, nil); done <- err }()
	for {
		var waiting bool
		if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("acceptance did not wait for the organization lock: %v", err)
		default:
		}
		if time.Now().After(boundary) {
			t.Fatal("invitation expired before the controlled lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(boundary) + 20*time.Millisecond)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvitationExpired) {
		t.Fatal("acceptance used an expiry check from before its lock wait")
	}
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE organization_id=$1 AND user_id=$2`, organization.ID, me.UserID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired acceptance created a membership")
	}
}

func TestPostgresA09OwnershipTransferRejectsExpiredOwnerAfterLockWait(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var boundary time.Time
	if err := repository.pool.QueryRow(ctx, `UPDATE role_bindings SET expires_at=statement_timestamp()+interval '1 second' WHERE role='OWNER' AND revoked_at IS NULL AND membership_id IN(SELECT id FROM memberships WHERE organization_id=$1) RETURNING expires_at`, organization.ID).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	gate, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var blocker int
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, organization.ID); err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repository.TransferOwnership(ctx, owner, organization.ID, me.UserID, uuid.NewString())
		done <- err
	}()
	for {
		var waiting bool
		if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("transfer did not wait for the organization lock: %v", err)
		default:
		}
		if time.Now().After(boundary) {
			t.Fatal("owner expired before the controlled wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(boundary) + 20*time.Millisecond)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("expired owner transferred ownership using stale transaction time")
	}
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings WHERE role='OWNER' AND membership_id IN(SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2)`, organization.ID, me.UserID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired authority changed the target's ownership")
	}
	var eventsAfter, changedMembers, commands, revokedOwners int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM outbox_events),(SELECT count(*) FROM memberships WHERE organization_id=$1 AND version<>1),(SELECT count(*) FROM membership_commands WHERE organization_id=$1),(SELECT count(*) FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND rb.role='OWNER' AND rb.revoked_at IS NOT NULL)`, organization.ID).Scan(&eventsAfter, &changedMembers, &commands, &revokedOwners); err != nil || eventsAfter != eventsBefore || changedMembers != 0 || commands != 0 || revokedOwners != 0 {
		t.Fatal("rejected transfer changed role history, membership versions, command receipts or outbox events")
	}
}

func TestPostgresA09MigrationsPreserveA08IdentityMembershipAndGlobalContext(t *testing.T) {
	repository := accountDatabase(t)
	owner, ownerMe := accountUser(t, repository)
	member, memberMe := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	if _, err := repository.SetActiveContext(context.Background(), member, &organization.ID, accountContextVersion(t, repository, member)); err != nil {
		t.Fatal(err)
	}
	items, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	const a08Version, a09Version = int64(18), int64(22)
	var a09Migrations []migrations.Migration
	for _, item := range items {
		if item.Version <= a09Version {
			a09Migrations = append(a09Migrations, item)
		}
	}
	if len(a09Migrations) == 0 || a09Migrations[len(a09Migrations)-1].Version != a09Version || len(items) <= len(a09Migrations) {
		t.Fatal("fixture requires the A09 boundary and subsequent account migrations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := repository.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for round := 0; round < 2; round++ {
		for i := len(items) - 1; i >= 0 && items[i].Version > a08Version; i-- {
			if err := applyMigrationSet(ctx, conn.Conn(), items, true); err != nil {
				t.Fatal(err)
			}
		}
		var version int64
		if err := conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != a08Version {
			t.Fatal("fixture did not reach the preserved A08 schema")
		}
		var users, members, owners int
		if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE id=ANY($1)),(SELECT count(*) FROM memberships WHERE organization_id=$2 AND status='ACTIVE'),(SELECT count(*) FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$2 AND rb.role='OWNER' AND rb.revoked_at IS NULL)`, []uuid.UUID{ownerMe.UserID, memberMe.UserID}, organization.ID).Scan(&users, &members, &owners); err != nil || users != 2 || members != 2 || owners != 1 {
			t.Fatal("rollback damaged inherited account or role facts")
		}
		// Verify A09 independently, then preserve the same facts through A10.
		for _, target := range [][]migrations.Migration{a09Migrations, items} {
			if err := applyMigrationSet(ctx, conn.Conn(), target, false); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != target[len(target)-1].Version {
				t.Fatal("upgrade did not reach the requested account schema boundary")
			}
			current, err := repository.GetMe(ctx, member)
			if err != nil || current.UserID != memberMe.UserID || current.Status != "ACTIVE" || current.ActiveContext.OrganizationID == nil || *current.ActiveContext.OrganizationID != organization.ID {
				t.Fatal("upgrade lost existing identity or the audited global context")
			}
			resolved, err := repository.ResolveAuthorizationContext(ctx, member)
			if err != nil || resolved.MemberVersion != 1 || !slices.Equal(resolved.Roles, []string{"MEMBER"}) {
				t.Fatal("upgrade changed inherited membership authority")
			}
		}
	}
}

// These helpers read current authority versions for inherited domain tests.
// They do not relax the new production compare-and-swap contracts.
func accountContextVersion(t *testing.T, repository *PostgresRepository, principal identity.Principal) int64 {
	t.Helper()
	var version int64
	if err := repository.pool.QueryRow(context.Background(), `SELECT active.context_version FROM identities i JOIN user_active_contexts active ON active.user_id=i.user_id WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestPostgresA09OwnerConstraintUsesCurrentCommitClock(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	tx, err := repository.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT pg_sleep(0.05)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE role_bindings SET starts_at=clock_timestamp() WHERE role='OWNER' AND revoked_at IS NULL AND membership_id IN (SELECT id FROM memberships WHERE organization_id=$1)`, organization.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal("a currently effective owner was evaluated using stale transaction time")
	}
	for _, state := range []string{"future", "expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			tx, err := repository.pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			statement := map[string]string{
				"future":  "starts_at=statement_timestamp()+interval '1 hour'",
				"expired": "starts_at=statement_timestamp()-interval '2 hours',expires_at=statement_timestamp()-interval '1 hour'",
				"revoked": "revoked_at=statement_timestamp()",
			}[state]
			if _, err := tx.Exec(context.Background(), `UPDATE role_bindings SET `+statement+` WHERE role='OWNER' AND revoked_at IS NULL AND membership_id IN (SELECT id FROM memberships WHERE organization_id=$1)`, organization.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(context.Background()); err == nil || !strings.Contains(err.Error(), "effective organization must have exactly one owner") {
				t.Fatal("an organization lost its only currently effective owner")
			}
		})
	}
	_, other := accountUser(t, repository)
	admin := fixturePlatformAdmin(t, repository)
	if _, err := repository.AdminAddMember(context.Background(), admin, organization.ID, other.UserID, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	_, err = repository.pool.Exec(context.Background(), `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) SELECT $1,id,'OWNER',$2,statement_timestamp(),statement_timestamp() FROM memberships WHERE organization_id=$3 AND user_id=$2`, uuid.New(), other.UserID, organization.ID)
	if err == nil || !strings.Contains(err.Error(), "effective organization must have exactly one owner") {
		t.Fatal("multiple effective owners were accepted")
	}
}

func TestPostgresA09MemberVersionIsLocalAndQuotaDirectoryIsAuthorized(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	if _, err := repository.SetActiveContext(context.Background(), member, &organization.ID, accountContextVersion(t, repository, member)); err != nil {
		t.Fatal(err)
	}
	before, err := repository.ResolveAuthorizationContext(context.Background(), member)
	if err != nil || before.MemberVersion != 1 {
		t.Fatal("current organization member-local version was not resolved")
	}
	second := member
	second.SessionID = uuid.NewString()
	if _, _, err := repository.Bootstrap(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	otherOrg := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, member, otherOrg.ID, []string{"MEMBER"})
	for _, session := range []identity.Principal{member, second} {
		current, err := repository.ResolveAuthorizationContext(context.Background(), session)
		if err != nil || current.MemberVersion != before.MemberVersion || current.MembershipVersion <= before.MembershipVersion || current.OrganizationID == nil || *current.OrganizationID != organization.ID {
			t.Fatal("global membership changes were confused with the current local member version")
		}
	}
	directory, err := repository.ListQuotaMembers(context.Background(), owner, organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range directory {
		if item.UserID == me.UserID {
			found = item.Version == before.MemberVersion
		}
	}
	if !found {
		t.Fatal("quota directory did not return the same local member version")
	}
	if _, err := repository.ListQuotaMembers(context.Background(), member, organization.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary members read the allocation directory without its permission")
	}
	outsider, _ := accountUser(t, repository)
	if _, err := repository.ListQuotaMembers(context.Background(), outsider, organization.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal("an outsider read the allocation directory")
	}
	if _, err := repository.SetActiveContext(context.Background(), member, nil, accountContextVersion(t, repository, member)); err != nil {
		t.Fatal(err)
	}
	personal, err := repository.ResolveAuthorizationContext(context.Background(), member)
	raw, marshalErr := json.Marshal(personal)
	if err != nil || marshalErr != nil || personal.MemberVersion != 0 || strings.Contains(string(raw), "member_version") {
		t.Fatal("personal context claimed an organization member version")
	}
}

func TestPostgresA09RoleChangesPublishEveryMembershipVersionAtomically(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	ctx := context.Background()
	grant := RoleBindingGrant{UserID: me.UserID, OrganizationID: &organization.ID, RoleCode: "BILLING_ADMIN", IdempotencyKey: uuid.NewString()}
	first, err := repository.GrantOrganizationRoleBinding(ctx, owner, grant)
	if err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, repository, organization.ID, me.UserID, "ACTIVE")
	if _, err := repository.RevokeOrganizationRoleBinding(ctx, owner, organization.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, repository, organization.ID, me.UserID, "ACTIVE")
	expires := time.Now().UTC().Add(time.Minute)
	grant.RoleCode, grant.IdempotencyKey, grant.ExpiresAt = "AUDITOR", uuid.NewString(), &expires
	if _, err := repository.GrantOrganizationRoleBinding(ctx, owner, grant); err != nil {
		t.Fatal(err)
	}
	repository.now = func() time.Time { return expires.Add(time.Second) }
	if count, err := repository.ExpireRoleBindings(ctx, 10); err != nil || count != 1 {
		t.Fatal("expiry did not publish its membership transition")
	}
	assertMembershipEventVersion(t, repository, organization.ID, me.UserID, "ACTIVE")
	var versions []int64
	if err := repository.pool.QueryRow(ctx, `SELECT array_agg(aggregate_version ORDER BY aggregate_version) FROM outbox_events WHERE aggregate_id=(SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2) AND event_type='account.membership.changed.v1'`, organization.ID, me.UserID).Scan(&versions); err != nil || !slices.Equal(versions, []int64{1, 2, 3, 4, 5}) {
		t.Fatal("role changes left a gap in the membership event sequence")
	}
	repository.now = time.Now
	accountExec(t, repository, `ALTER TABLE outbox_events ADD CONSTRAINT fixture_membership_event_rejected CHECK(event_type<>'account.membership.changed.v1') NOT VALID`)
	grant.RoleCode, grant.IdempotencyKey, grant.ExpiresAt = "BILLING_ADMIN", uuid.NewString(), nil
	if _, err := repository.GrantOrganizationRoleBinding(ctx, owner, grant); err == nil {
		t.Fatal("grant succeeded without its membership event")
	}
	var current, bindings int64
	if err := repository.pool.QueryRow(ctx, `SELECT version,(SELECT count(*) FROM role_bindings WHERE membership_id=m.id AND idempotency_key=$3) FROM memberships m WHERE organization_id=$1 AND user_id=$2`, organization.ID, me.UserID, grant.IdempotencyKey).Scan(&current, &bindings); err != nil || current != 5 || bindings != 0 {
		t.Fatal("failed membership publication did not roll back the grant and version")
	}
}

func accountStructureVersion(t *testing.T, repository *PostgresRepository, organizationID uuid.UUID) int64 {
	t.Helper()
	var version int64
	if err := repository.pool.QueryRow(context.Background(), `SELECT structure_version FROM organizations WHERE id=$1`, organizationID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestPostgresA09GlobalContextPreservesAllSessionsAndRejectsStaleCAS(t *testing.T) {
	repository := accountDatabase(t)
	principal, user := accountUser(t, repository)
	organization := accountOrganization(t, repository, principal)
	other := principal
	other.SessionID = uuid.NewString()
	if _, _, err := repository.Bootstrap(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	initial := accountContextVersion(t, repository, principal)
	selected, err := repository.SetActiveContext(context.Background(), principal, &organization.ID, initial)
	if err != nil || selected.ContextVersion != initial+1 {
		t.Fatalf("switch version=%d error=%v", selected.ContextVersion, err)
	}
	for _, session := range []identity.Principal{principal, other} {
		me, err := repository.GetMe(context.Background(), session)
		if err != nil || me.UserID != user.UserID || me.ActiveContext.OrganizationID == nil || *me.ActiveContext.OrganizationID != organization.ID || me.ActiveContext.ContextVersion != selected.ContextVersion {
			t.Fatal("global context was not shared by the user's existing sessions")
		}
	}
	if _, err := repository.SetActiveContext(context.Background(), other, nil, initial); !errors.Is(err, ErrConflict) {
		t.Fatal("a stale session overwrote the global context")
	}
	other.SessionID = uuid.NewString()
	newSession, _, err := repository.Bootstrap(context.Background(), other)
	if err != nil || newSession.ActiveContext.OrganizationID == nil || *newSession.ActiveContext.OrganizationID != organization.ID || newSession.ActiveContext.ContextVersion != selected.ContextVersion {
		t.Fatal("a new login reset the user's global context")
	}
	unchanged, err := repository.SetActiveContext(context.Background(), other, &organization.ID, selected.ContextVersion)
	if err != nil || unchanged.ContextVersion != selected.ContextVersion {
		t.Fatal("an unchanged context advanced its revision")
	}
}

func TestPostgresA09ProvisioningAndCurrentContextSerializePermissionArrays(t *testing.T) {
	repository := accountDatabase(t)
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	service := NewService(repository, &fakeWalletProvisioner{})
	pending, err := service.Bootstrap(context.Background(), principal)
	if err != nil || pending.Status != "PROVISIONING" || pending.ActiveContext.Permissions == nil || len(pending.ActiveContext.Permissions) != 0 {
		t.Fatal("pending bootstrap fabricated permissions or serialized null")
	}
	assertArray := func(me Me) {
		t.Helper()
		raw, err := json.Marshal(me)
		if err != nil {
			t.Fatal(err)
		}
		var dto struct {
			Context struct {
				Permissions json.RawMessage `json:"permissions"`
			} `json:"active_context"`
		}
		if json.Unmarshal(raw, &dto) != nil || len(dto.Context.Permissions) == 0 || dto.Context.Permissions[0] != '[' {
			t.Fatal("production Me serializer did not emit a permission array")
		}
	}
	assertArray(pending)
	me, err := repository.GetMe(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	assertArray(me)
	if err := repository.ActivateUser(context.Background(), pending.UserID); err != nil {
		t.Fatal(err)
	}
	organization := accountOrganization(t, repository, principal)
	if _, err := repository.SetActiveContext(context.Background(), principal, &organization.ID, accountContextVersion(t, repository, principal)); err != nil {
		t.Fatal(err)
	}
	me, err = repository.GetMe(context.Background(), principal)
	if err != nil || !slices.Contains(me.ActiveContext.Permissions, PermissionOrganizationMembersRead) {
		t.Fatal("Me omitted the current owner's actual permission")
	}
	assertArray(me)
}

func TestPostgresA09MutationAuthorityRemainsCurrentAfterLockWait(t *testing.T) {
	for _, kind := range []string{"create-unit", "move-unit", "remove-member", "create-invitation", "revoke-invitation", "replace-roles"} {
		t.Run(kind, func(t *testing.T) {
			repository := accountDatabase(t)
			owner, _ := accountUser(t, repository)
			actor, actorMe := accountUser(t, repository)
			target, targetMe := accountUser(t, repository)
			organization := accountOrganization(t, repository, owner)
			accountInvitation(t, repository, owner, actor, organization.ID, []string{"ORG_ADMIN"})
			accountInvitation(t, repository, owner, target, organization.ID, []string{"MEMBER"})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var source, destination UnitMutationResult
			var invitation Invitation
			var err error
			if kind == "move-unit" {
				source, err = repository.CreateUnit(ctx, owner, organization.ID, nil, "source", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				destination, err = repository.CreateUnit(ctx, owner, organization.ID, nil, "destination", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				// The destination remains authorized after the organization-wide
				// source grant expires. Both original scopes must be checked.
				_, err = repository.GrantOrganizationRoleBinding(ctx, owner, RoleBindingGrant{UserID: actorMe.UserID, OrganizationID: &organization.ID, UnitID: &destination.Unit.ID, RoleCode: "UNIT_ADMIN", IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "revoke-invitation" {
				invitation, err = repository.CreateInvitation(ctx, owner, organization.ID, InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
			}
			var boundary time.Time
			if err := repository.pool.QueryRow(ctx, `UPDATE role_bindings rb SET expires_at=statement_timestamp()+interval '1 second' FROM memberships m WHERE rb.membership_id=m.id AND m.organization_id=$1 AND m.user_id=$2 AND rb.role='ORG_ADMIN' AND rb.revoked_at IS NULL RETURNING rb.expires_at`, organization.ID, actorMe.UserID).Scan(&boundary); err != nil {
				t.Fatal(err)
			}
			before := accountMutationSnapshot(t, repository, organization.ID)
			gate, err := repository.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			var blocker int
			if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "remove-member", "replace-roles":
				_, err = gate.Exec(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organization.ID, targetMe.UserID)
			case "revoke-invitation":
				_, err = gate.Exec(ctx, `SELECT id FROM invitations WHERE id=$1 FOR UPDATE`, invitation.ID)
			default:
				_, err = gate.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, organization.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "create-unit":
					_, err = repository.CreateUnit(ctx, actor, organization.ID, nil, "expired-authority", uuid.NewString())
				case "move-unit":
					_, err = repository.MoveUnit(ctx, actor, organization.ID, source.Unit.ID, &destination.Unit.ID, accountStructureVersion(t, repository, organization.ID), uuid.NewString())
				case "remove-member":
					_, err = repository.RevokeMembership(ctx, actor, organization.ID, targetMe.UserID, uuid.NewString())
				case "replace-roles":
					_, err = repository.ReplaceMemberRoles(ctx, actor, organization.ID, targetMe.UserID, []string{"AUDITOR"})
				case "create-invitation":
					_, err = repository.CreateInvitation(ctx, actor, organization.ID, InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
				case "revoke-invitation":
					_, err = repository.RevokeInvitation(ctx, actor, organization.ID, invitation.ID)
				}
				done <- err
			}()
			for {
				var waiting bool
				if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("mutation did not wait for the controlled lock: %v", err)
				default:
				}
				if time.Now().After(boundary) {
					t.Fatal("authority expired before the controlled wait")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(time.Until(boundary) + 20*time.Millisecond)
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrForbidden) {
				t.Fatal("expired authority was accepted after the controlled wait", err)
			}
			if after := accountMutationSnapshot(t, repository, organization.ID); after != before {
				t.Fatal("rejected mutation changed units, members, roles, invitations, receipts, audit or outbox")
			}
		})
	}
}

func accountMutationSnapshot(t *testing.T, repository *PostgresRepository, organizationID uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := repository.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'organization',(SELECT to_jsonb(o) FROM organizations o WHERE id=$1),
 'units',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM organization_units u WHERE organization_id=$1),
 'closure',(SELECT jsonb_agg(to_jsonb(c) ORDER BY ancestor_id,descendant_id) FROM organization_unit_closure c WHERE organization_id=$1),
 'members',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m WHERE organization_id=$1),
 'roles',(SELECT jsonb_agg(to_jsonb(rb) ORDER BY rb.id) FROM role_bindings rb JOIN memberships m ON rb.membership_id=m.id WHERE m.organization_id=$1),
 'invitations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM invitations i WHERE organization_id=$1),
 'unit_receipts',(SELECT count(*) FROM organization_unit_commands WHERE organization_id=$1),
 'member_receipts',(SELECT count(*) FROM membership_commands WHERE organization_id=$1),
 'audit_count',(SELECT count(*) FROM audit_logs),
 'outbox_count',(SELECT count(*) FROM outbox_events))::text`, organizationID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPostgresA09MutationRejectsRoleRevokedDuringResourceLockWait(t *testing.T) {
	for _, kind := range []string{"create-unit", "move-unit", "remove-member", "create-invitation", "revoke-invitation", "replace-roles"} {
		t.Run(kind, func(t *testing.T) {
			repository := accountDatabase(t)
			owner, _ := accountUser(t, repository)
			actor, actorMe := accountUser(t, repository)
			target, targetMe := accountUser(t, repository)
			organization := accountOrganization(t, repository, owner)
			accountInvitation(t, repository, owner, actor, organization.ID, []string{"ORG_ADMIN"})
			accountInvitation(t, repository, owner, target, organization.ID, []string{"MEMBER"})
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			var source, destination UnitMutationResult
			var invitation Invitation
			var err error
			if kind == "move-unit" {
				source, err = repository.CreateUnit(ctx, owner, organization.ID, nil, "source", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				destination, err = repository.CreateUnit(ctx, owner, organization.ID, nil, "destination", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "revoke-invitation" {
				invitation, err = repository.CreateInvitation(ctx, owner, organization.ID, InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
			}
			structureVersion := accountStructureVersion(t, repository, organization.ID)
			var bindingID uuid.UUID
			if err := repository.pool.QueryRow(ctx, `SELECT rb.id FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND m.user_id=$2 AND rb.role='ORG_ADMIN' AND rb.revoked_at IS NULL`, organization.ID, actorMe.UserID).Scan(&bindingID); err != nil {
				t.Fatal(err)
			}
			gate, err := repository.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			var blocker int
			if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "remove-member", "replace-roles":
				_, err = gate.Exec(ctx, `SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organization.ID, targetMe.UserID)
			case "revoke-invitation":
				_, err = gate.Exec(ctx, `SELECT id FROM invitations WHERE id=$1 FOR UPDATE`, invitation.ID)
			default:
				_, err = gate.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, organization.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var callErr error
				switch kind {
				case "create-unit":
					_, callErr = repository.CreateUnit(ctx, actor, organization.ID, nil, "revoked authority", uuid.NewString())
				case "move-unit":
					_, callErr = repository.MoveUnit(ctx, actor, organization.ID, source.Unit.ID, &destination.Unit.ID, structureVersion, uuid.NewString())
				case "remove-member":
					_, callErr = repository.RevokeMembership(ctx, actor, organization.ID, targetMe.UserID, uuid.NewString())
				case "create-invitation":
					_, callErr = repository.CreateInvitation(ctx, actor, organization.ID, InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
				case "revoke-invitation":
					_, callErr = repository.RevokeInvitation(ctx, actor, organization.ID, invitation.ID)
				case "replace-roles":
					_, callErr = repository.ReplaceMemberRoles(ctx, actor, organization.ID, targetMe.UserID, []string{"AUDITOR"})
				}
				done <- callErr
			}()
			for {
				var waiting bool
				if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("mutation did not wait: %v", err)
				default:
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				time.Sleep(5 * time.Millisecond)
			}
			revoked, err := repository.RevokeOrganizationRoleBinding(ctx, owner, organization.ID, bindingID)
			if err != nil || revoked.Status != "REVOKED" {
				t.Fatal("production revocation did not commit", err)
			}
			before := accountMutationSnapshot(t, repository, organization.ID)
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			callErr := <-done
			var pgErr *pgconn.PgError
			if !errors.Is(callErr, ErrForbidden) && !(errors.As(callErr, &pgErr) && pgErr.Code == "40001") {
				t.Fatal("revoked authority was accepted from an old snapshot", callErr)
			}
			if accountMutationSnapshot(t, repository, organization.ID) != before {
				t.Fatal("unauthorized mutation changed business data, receipt, audit or outbox")
			}
		})
	}
}
