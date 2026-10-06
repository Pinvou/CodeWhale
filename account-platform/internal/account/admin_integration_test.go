package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func fixturePlatformAdmin(t *testing.T, repo *PostgresRepository) identity.Principal {
	t.Helper()
	principal, me := accountUser(t, repo)
	principal.RealmRoles = []string{"platform-admin"}
	_, err := repo.pool.Exec(context.Background(), `INSERT INTO role_assignments(id,user_id,role_code,source,created_at) VALUES($1,$2,'PLATFORM_SUPER_ADMIN','TEST',statement_timestamp())`, uuid.New(), me.UserID)
	if err != nil {
		t.Fatal("platform authority fixture could not be created")
	}
	return principal
}

func TestPostgresHTTPOnlyRealmAdministratorCreatesOrganizationWithInitialOwner(t *testing.T) {
	repo := accountDatabase(t)
	owner, me := accountUser(t, repo)
	admin := fixturePlatformAdmin(t, repo)
	server := httptest.NewServer(NewService(repo, nil).Handler(func(_ context.Context, bearer string) (identity.Principal, error) {
		if bearer == "fixture-admin" {
			return admin, nil
		}
		return owner, nil
	}))
	defer server.Close()
	body := `{"name":"fixture administered organization","owner_user_id":"` + me.UserID.String() + `"}`
	for _, scenario := range []struct {
		name, path, bearer string
		status             int
	}{{"ordinary-self-create-absent", "/v1/organizations", "fixture-member", 404}, {"ordinary-admin-create-denied", "/admin/v1/organizations", "fixture-member", 403}, {"administrator-create", "/admin/v1/organizations", "fixture-admin", 201}} {
		t.Run(scenario.name, func(t *testing.T) {
			request, _ := http.NewRequest("POST", server.URL+scenario.path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+scenario.bearer)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("organization creation HTTP request failed")
			}
			defer response.Body.Close()
			if response.StatusCode != scenario.status {
				t.Fatal("organization creation did not preserve the selected administrator-only contract")
			}
			if scenario.status == 201 {
				var org Organization
				if json.NewDecoder(response.Body).Decode(&org) != nil || org.ID == uuid.Nil {
					t.Fatal("admin create did not return the actual organization")
				}
				assertMembershipEventVersion(t, repo, org.ID, me.UserID, "ACTIVE")
				var roles int
				if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM role_bindings r JOIN memberships m ON m.id=r.membership_id WHERE m.organization_id=$1 AND m.user_id=$2 AND r.role='OWNER'`, org.ID, me.UserID).Scan(&roles); err != nil || roles != 1 {
					t.Fatal("initial owner did not receive exactly one authoritative owner grant")
				}
			}
		})
	}
	var organizations int
	if err := repo.pool.QueryRow(context.Background(), "SELECT count(*) FROM organizations").Scan(&organizations); err != nil || organizations != 1 {
		t.Fatal("denied ordinary creation mutated organizations")
	}
}

func assertMembershipEventVersion(t *testing.T, repository *PostgresRepository, organizationID, userID uuid.UUID, wantStatus string) {
	t.Helper()
	var version, eventVersion int64
	var status, eventStatus string
	err := repository.pool.QueryRow(context.Background(), `SELECT m.version,m.status,COALESCE(e.aggregate_version,0),COALESCE(e.payload->>'membership_status','') FROM memberships m LEFT JOIN LATERAL (SELECT aggregate_version,payload FROM outbox_events WHERE aggregate_id=m.id AND event_type='account.membership.changed.v1' ORDER BY aggregate_version DESC LIMIT 1) e ON true WHERE m.organization_id=$1 AND m.user_id=$2`, organizationID, userID).Scan(&version, &status, &eventVersion, &eventStatus)
	if err != nil || status != wantStatus || eventStatus != status || eventVersion != version {
		t.Fatal("membership change did not retain its actual version and status in the transactional event")
	}
}

func TestPostgresAdminCreationPublishesInitialOwnerMembership(t *testing.T) {
	r := accountDatabase(t)
	_, owner := accountUser(t, r)
	organization, err := r.AdminCreateOrganization(context.Background(), fixturePlatformAdmin(t, r), owner.UserID, "admin-created organization", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, r, organization.ID, owner.UserID, "ACTIVE")
}

func TestPostgresAdminOwnershipTransferPreservesPreviousMemberAndEvents(t *testing.T) {
	r := accountDatabase(t)
	owner, ownerMe := accountUser(t, r)
	target, targetMe := accountUser(t, r)
	organization := accountOrganization(t, r, owner)
	accountInvitation(t, r, owner, target, organization.ID, []string{"MEMBER"})
	if _, err := r.AdminTransferOwnership(context.Background(), fixturePlatformAdmin(t, r), organization.ID, targetMe.UserID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetActiveContext(context.Background(), owner, &organization.ID, accountContextVersion(t, r, owner)); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.ResolveAuthorizationContext(context.Background(), owner)
	if err != nil || len(resolved.Roles) != 1 || resolved.Roles[0] != "MEMBER" {
		t.Fatal("former owner lost ordinary membership or retained owner authority")
	}
	assertMembershipEventVersion(t, r, organization.ID, ownerMe.UserID, "ACTIVE")
	assertMembershipEventVersion(t, r, organization.ID, targetMe.UserID, "ACTIVE")
}

func TestPostgresAdminOrganizationStatusAndDeletionPublishEachMembershipVersion(t *testing.T) {
	r := accountDatabase(t)
	owner, ownerMe := accountUser(t, r)
	organization := accountOrganization(t, r, owner)
	admin := fixturePlatformAdmin(t, r)
	for _, state := range []struct{ organization, membership string }{{"DISABLED", "SUSPENDED"}, {"ACTIVE", "ACTIVE"}} {
		if err := r.AdminSetOrganizationStatus(context.Background(), admin, organization.ID, state.organization); err != nil {
			t.Fatal(err)
		}
		assertMembershipEventVersion(t, r, organization.ID, ownerMe.UserID, state.membership)
	}
	if err := r.AdminSetOrganizationStatus(context.Background(), admin, organization.ID, "DISABLED"); err != nil {
		t.Fatal(err)
	}
	if err := r.AdminDeleteOrganization(context.Background(), admin, organization.ID); err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, r, organization.ID, ownerMe.UserID, "REVOKED")
}

func TestPostgresAdminLeafDeletionBlocksLiveGrantsAndRetainsHistory(t *testing.T) {
	r := accountDatabase(t)
	owner, _ := accountUser(t, r)
	member, memberMe := accountUser(t, r)
	organization := accountOrganization(t, r, owner)
	accountInvitation(t, r, owner, member, organization.ID, []string{"MEMBER"})
	unit := accountUnit(t, r, owner, organization.ID, nil, "leaf")
	accountExec(t, r, `INSERT INTO membership_units(membership_id,unit_id,created_at) SELECT id,$1,now() FROM memberships WHERE organization_id=$2 AND user_id=$3`, unit.ID, organization.ID, memberMe.UserID)
	admin := fixturePlatformAdmin(t, r)
	key := uuid.NewString()
	if _, err := r.AdminDeleteUnit(context.Background(), admin, organization.ID, unit.ID, key); err == nil {
		t.Fatal("a unit with active member access was deleted")
	}
	assertMembershipEventVersion(t, r, organization.ID, memberMe.UserID, "ACTIVE")
	var version int64
	if err := r.pool.QueryRow(context.Background(), `SELECT version FROM memberships WHERE organization_id=$1 AND user_id=$2`, organization.ID, memberMe.UserID).Scan(&version); err != nil || version != 1 {
		t.Fatal("blocked deletion changed the membership")
	}
	if _, err := r.AdminRemoveMember(context.Background(), admin, organization.ID, memberMe.UserID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, r, organization.ID, memberMe.UserID, "REVOKED")
	deleted, err := r.AdminDeleteUnit(context.Background(), admin, organization.ID, unit.ID, key)
	if err != nil || deleted.Unit.Status != "DELETED" {
		t.Fatal("a unit could not be logically deleted after its access was explicitly revoked")
	}
	if replay, err := r.AdminDeleteUnit(context.Background(), admin, organization.ID, unit.ID, key); err != nil || !replay.Replayed || replay.Unit.Version != deleted.Unit.Version {
		t.Fatal("unit deletion replay changed its history")
	}
	if err := r.pool.QueryRow(context.Background(), `SELECT version FROM memberships WHERE organization_id=$1 AND user_id=$2`, organization.ID, memberMe.UserID).Scan(&version); err != nil || version != 2 {
		t.Fatal("explicit membership revocation did not retain the advanced version")
	}
}

func TestPostgresAdminUserDeletionPublishesRevocation(t *testing.T) {
	r := accountDatabase(t)
	owner, _ := accountUser(t, r)
	member, memberMe := accountUser(t, r)
	organization := accountOrganization(t, r, owner)
	accountInvitation(t, r, owner, member, organization.ID, []string{"MEMBER"})
	if err := r.AdminDeleteUser(context.Background(), fixturePlatformAdmin(t, r), memberMe.UserID); err != nil {
		t.Fatal(err)
	}
	assertMembershipEventVersion(t, r, organization.ID, memberMe.UserID, "REVOKED")
}

func TestPostgresAdminMembershipMutationRollsBackWhenEventCannotPersist(t *testing.T) {
	r := accountDatabase(t)
	owner, ownerMe := accountUser(t, r)
	organization := accountOrganization(t, r, owner)
	accountExec(t, r, `ALTER TABLE outbox_events ADD CONSTRAINT fixture_admin_outbox_fail CHECK(event_type<>'account.membership.changed.v1') NOT VALID`)
	if err := r.AdminSetOrganizationStatus(context.Background(), fixturePlatformAdmin(t, r), organization.ID, "DISABLED"); err == nil {
		t.Fatal("admin mutation committed without its membership event")
	}
	var organizationStatus, membershipStatus string
	var membershipVersion int64
	if err := r.pool.QueryRow(context.Background(), `SELECT o.status,m.status,m.version FROM organizations o JOIN memberships m ON m.organization_id=o.id WHERE o.id=$1 AND m.user_id=$2`, organization.ID, ownerMe.UserID).Scan(&organizationStatus, &membershipStatus, &membershipVersion); err != nil || organizationStatus != "ACTIVE" || membershipStatus != "ACTIVE" || membershipVersion != 1 {
		t.Fatal("failed event persistence did not roll back the admin mutation")
	}
	assertMembershipEventVersion(t, r, organization.ID, ownerMe.UserID, "ACTIVE")
}

func TestPostgresHTTPAdminRejectsInvalidJSONWithoutMutation(t *testing.T) {
	repo := accountDatabase(t)
	owner, me := accountUser(t, repo)
	org := accountOrganization(t, repo, owner)
	unit := accountUnit(t, repo, owner, org.ID, nil, "fixture leaf")
	admin := fixturePlatformAdmin(t, repo)
	server := httptest.NewServer(NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) { return admin, nil }))
	defer server.Close()
	routes := []struct{ name, method, path, valid, invalidType string }{
		{"add-member", "POST", "/admin/v1/organizations/" + org.ID.String() + "/members", `{}`, `{"user_id":"invalid-uuid"}`},
		{"replace-roles", "PUT", "/admin/v1/organizations/" + org.ID.String() + "/members/" + me.UserID.String() + "/roles", `{}`, `{"unit_admin_unit_ids":["invalid-uuid"]}`},
		{"create-organization", "POST", "/admin/v1/organizations", "{\"name\":\"fixture name\",\"owner_user_id\":\"" + me.UserID.String() + "\"}", "{\"owner_user_id\":\"invalid-uuid\"}"},
		{"rename-organization", "PATCH", "/admin/v1/organizations/" + org.ID.String(), "{\"name\":\"fixture name\"}", "{\"name\":[]}"},
		{"transfer-owner", "POST", "/admin/v1/organizations/" + org.ID.String() + "/ownership", "{\"new_owner_user_id\":\"" + me.UserID.String() + "\"}", "{\"new_owner_user_id\":\"invalid-uuid\"}"},
		{"create-unit", "POST", "/admin/v1/organizations/" + org.ID.String() + "/units", "{\"name\":\"fixture name\"}", "{\"parent_id\":\"invalid-uuid\"}"},
		{"rename-unit", "PATCH", "/admin/v1/organizations/" + org.ID.String() + "/units/" + unit.ID.String(), "{\"name\":\"fixture name\"}", "{\"name\":[]}"},
		{"user-status", "PATCH", "/admin/v1/users/" + me.UserID.String() + "/status", "{\"status\":\"DISABLED\"}", "{\"status\":[]}"},
		{"organization-status", "PATCH", "/admin/v1/organizations/" + org.ID.String() + "/status", "{\"status\":\"DISABLED\"}", "{\"status\":[]}"},
	}
	var auditBefore int
	if err := repo.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_logs").Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			for _, body := range []struct{ name, value string }{
				{"malformed", "{"}, {"unknown-field", "{\"unexpected_field\":true}"}, {"invalid-type-or-uuid", route.invalidType}, {"multiple-values", route.valid + " {}"}, {"over-limit", "{\"name\":\"" + strings.Repeat("x", 1<<20) + "\"}"},
			} {
				t.Run(body.name, func(t *testing.T) {
					request, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(body.value))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer fixture-admin-token")
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					raw, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					var payload struct{ Code, Message string }
					if json.Unmarshal(raw, &payload) != nil || response.StatusCode != http.StatusBadRequest || payload.Code != "INVALID_REQUEST" || payload.Message != "invalid administration request" {
						t.Fatal("invalid admin JSON did not return the static 400 contract")
					}
				})
			}
		})
	}
	var auditAfter int
	if err := repo.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_logs").Scan(&auditAfter); err != nil || auditAfter != auditBefore+5 {
		t.Fatal("invalid admin JSON changed history outside the five creation failure audits")
	}
	var creationFailures int
	if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action='admin.organization.create' AND result='FAILED' AND resource_id='pending'`).Scan(&creationFailures); err != nil || creationFailures != 5 {
		t.Fatal("invalid organization creation requests did not record only failed attempts")
	}
	var userStatus, orgStatus, unitStatus string
	if err := repo.pool.QueryRow(context.Background(), "SELECT u.status,o.status,n.status FROM users u,organizations o,organization_units n WHERE u.id=$1 AND o.id=$2 AND n.id=$3", me.UserID, org.ID, unit.ID).Scan(&userStatus, &orgStatus, &unitStatus); err != nil || userStatus != "ACTIVE" || orgStatus != "ACTIVE" || unitStatus != "ACTIVE" {
		t.Fatal("invalid admin JSON mutated existing business state")
	}
}
