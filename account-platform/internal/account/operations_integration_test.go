package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresAdminScopedRolesPreserveOwnerAndTransactionalMembershipEvents(t *testing.T) {
	repo := accountDatabase(t)
	owner, ownerMe := accountUser(t, repo)
	_, memberMe := accountUser(t, repo)
	org := accountOrganization(t, repo, owner)
	unit := accountUnit(t, repo, owner, org.ID, nil, "scoped administration")
	otherOrg := accountOrganization(t, repo, owner)
	foreignUnit := accountUnit(t, repo, owner, otherOrg.ID, nil, "foreign scope")
	ctx := context.Background()
	admin := fixturePlatformAdmin(t, repo)
	if _, err := repo.AdminAddMember(ctx, admin, org.ID, memberMe.UserID, nil, []uuid.UUID{foreignUnit.ID}, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-organization unit role was accepted")
	}
	member, err := repo.AdminAddMember(ctx, admin, org.ID, memberMe.UserID, []string{"AUDITOR", "AUDITOR"}, []uuid.UUID{unit.ID, unit.ID}, uuid.NewString())
	if err != nil || !slices.Equal(member.Member.Roles, []string{"AUDITOR", "MEMBER", "UNIT_ADMIN"}) || !slices.Equal(member.Member.UnitAdminIDs, []uuid.UUID{unit.ID}) {
		t.Fatal("scoped roles were not normalized or persisted")
	}
	assertMembershipEventVersion(t, repo, org.ID, memberMe.UserID, "ACTIVE")
	if member.Member.Version != 1 {
		t.Fatal("new administrator membership skipped its first event version")
	}
	request := httptest.NewRequest("GET", "/v1/organizations/"+org.ID.String()+"/members", nil)
	request.Header.Set("Authorization", "Bearer fixture-owner")
	response := httptest.NewRecorder()
	NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) { return owner, nil }).ServeHTTP(response, request)
	var directory struct {
		Items []struct {
			UserID       uuid.UUID   `json:"user_id"`
			UnitAdminIDs []uuid.UUID `json:"unit_admin_unit_ids"`
		}
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &directory) != nil {
		t.Fatal("scoped membership fields did not reach the public handler")
	}
	found := false
	for _, item := range directory.Items {
		if item.UserID == memberMe.UserID {
			found = slices.Equal(item.UnitAdminIDs, []uuid.UUID{unit.ID})
		}
	}
	if !found {
		t.Fatal("public membership directory lost the new scoped role wire")
	}
	if _, err := repo.AdminReplaceMemberRoles(ctx, admin, org.ID, ownerMe.UserID, []string{"MEMBER"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AdminRemoveMember(ctx, admin, org.ID, ownerMe.UserID, uuid.NewString()); !errors.Is(err, ErrLastOwner) {
		t.Fatal("role replacement removed the protected owner")
	}
	if _, err := repo.AdminReplaceMemberRoles(ctx, admin, org.ID, memberMe.UserID, []string{"OWNER"}, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("roles endpoint granted ownership")
	}
	if _, err := repo.AdminRemoveMember(ctx, admin, org.ID, memberMe.UserID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, repo, org.ID, memberMe.UserID, "REVOKED")
	member, err = repo.AdminAddMember(ctx, admin, org.ID, memberMe.UserID, nil, nil, uuid.NewString())
	if err != nil || !slices.Equal(member.Member.Roles, []string{"MEMBER"}) || len(member.Member.UnitAdminIDs) != 0 {
		t.Fatal("re-adding a member revived revoked scoped privileges")
	}
	assertMembershipEventVersion(t, repo, org.ID, memberMe.UserID, "ACTIVE")
	rows, err := repo.pool.Query(ctx, `SELECT aggregate_version FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.membership.changed.v1' ORDER BY aggregate_version`, member.Member.MembershipID)
	if err != nil {
		t.Fatal(err)
	}
	versions := []int64{}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	rows.Close()
	if rows.Err() != nil || !slices.Equal(versions, []int64{1, 2, 3}) {
		t.Fatal("administrator member lifecycle skipped a durable event version")
	}
	if _, err := repo.pool.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT fixture_no_new_membership_event CHECK(event_type <> 'account.membership.changed.v1') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AdminReplaceMemberRoles(ctx, admin, org.ID, memberMe.UserID, []string{"ORG_ADMIN"}, nil); err == nil {
		t.Fatal("missing transactional event did not roll back the role change")
	}
	stored, err := repo.AdminMembers(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range stored {
		if item.UserID == memberMe.UserID && (item.Version != member.Member.Version || !slices.Equal(item.Roles, member.Member.Roles)) {
			t.Fatal("failed event write changed authoritative membership roles")
		}
	}
	if _, err := repo.pool.Exec(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT fixture_no_new_membership_event`); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAdministratorMembershipLifecycleEventsApplyToInboxOnce(t *testing.T) {
	ctx := context.Background()
	repo := accountDatabase(t)
	owner, _ := accountUser(t, repo)
	_, user := accountUser(t, repo)
	org := accountOrganization(t, repo, owner)
	admin := fixturePlatformAdmin(t, repo)
	member, err := repo.AdminAddMember(ctx, admin, org.ID, user.UserID, nil, nil, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AdminRemoveMember(ctx, admin, org.ID, user.UserID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AdminAddMember(ctx, admin, org.ID, user.UserID, nil, nil, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(repo.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("search_path", repo.pool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = query.Encode()
	inbox, err := events.NewPostgresStore(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	rows, err := repo.pool.Query(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE aggregate_id=$1 ORDER BY aggregate_version`, member.Member.MembershipID)
	if err != nil {
		t.Fatal(err)
	}
	var messages []events.Event
	for rows.Next() {
		var event events.Event
		if err := rows.Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, event)
	}
	rows.Close()
	if rows.Err() != nil || len(messages) != 3 {
		t.Fatal("lifecycle did not produce its three business events")
	}
	appliedCount := 0
	apply := func(context.Context, pgx.Tx, events.Event) error { appliedCount++; return nil }
	for index, event := range messages {
		if applied, err := inbox.ProcessInbox(ctx, event, apply); err != nil || !applied {
			t.Fatal("production inbox rejected an administrator lifecycle event")
		}
		if event.AggregateVersion != int64(index+1) {
			t.Fatal("administrator lifecycle skipped its event sequence")
		}
		if applied, err := inbox.ProcessInbox(ctx, event, apply); err != nil || applied {
			t.Fatal("duplicate lifecycle event escaped inbox deduplication")
		}
	}
	if appliedCount != 3 {
		t.Fatal("membership lifecycle projection did not apply once per version")
	}
}
