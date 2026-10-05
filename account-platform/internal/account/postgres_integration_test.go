package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func accountDatabase(t *testing.T) *PostgresRepository {
	t.Helper()
	return &PostgresRepository{pool: testdb.Open(t, "account"), now: func() time.Time { return time.Now().UTC() }, walletPolicyVersion: walletProvisioningPolicyVersion, walletProvisioningSLA: walletProvisioningSLA}
}

func accountUser(t *testing.T, repository *PostgresRepository) (identity.Principal, Me) {
	t.Helper()
	identifier := uuid.NewString()
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: identifier, SessionID: uuid.NewString(), Email: identifier + "@example.test"}
	me, _, err := repository.Bootstrap(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ActivateUser(context.Background(), me.UserID); err != nil {
		t.Fatal(err)
	}
	me, err = repository.GetMe(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	return principal, me
}

func accountOrganization(t *testing.T, repository *PostgresRepository, principal identity.Principal) Organization {
	t.Helper()
	organization, err := repository.CreateOrganization(context.Background(), principal, "test organization")
	if err != nil {
		t.Fatal(err)
	}
	return organization
}

func accountInvitation(t *testing.T, repository *PostgresRepository, owner, member identity.Principal, organizationID uuid.UUID, roles []string) Invitation {
	t.Helper()
	invitation, err := repository.CreateInvitation(context.Background(), owner, organizationID, member.Email, roles, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AcceptInvitation(context.Background(), member, invitation.Token); err != nil {
		t.Fatal(err)
	}
	return invitation
}

func accountUnit(t *testing.T, repository *PostgresRepository, owner identity.Principal, organizationID uuid.UUID, parent *uuid.UUID, name string) Unit {
	t.Helper()
	unit, err := repository.CreateUnit(context.Background(), owner, organizationID, parent, name)
	if err != nil {
		t.Fatal(err)
	}
	return unit
}

func accountExec(t *testing.T, repository *PostgresRepository, sql string, arguments ...any) {
	t.Helper()
	if _, err := repository.pool.Exec(context.Background(), sql, arguments...); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBootstrapPreservesSessionContextAndRejectsSessionReuse(t *testing.T) {
	repository := accountDatabase(t)
	principal, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, principal)
	selected, err := repository.SetActiveContext(context.Background(), principal, &organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	repeated, created, err := repository.Bootstrap(context.Background(), principal)
	if err != nil || created || repeated.ActiveContext.Type != "ORGANIZATION" || repeated.ActiveContext.OrganizationID == nil || *repeated.ActiveContext.OrganizationID != organization.ID || repeated.ActiveContext.MembershipVersion != selected.MembershipVersion {
		t.Fatalf("repeated bootstrap=%+v created=%v error=%v", repeated, created, err)
	}
	otherSession := principal
	otherSession.SessionID = uuid.NewString()
	other, created, err := repository.Bootstrap(context.Background(), otherSession)
	if err != nil || created || other.UserID != me.UserID || other.ActiveContext.Type != "PERSONAL" || other.ActiveContext.OrganizationID != nil {
		t.Fatalf("new session=%+v created=%v error=%v", other, created, err)
	}
	intruder := principal
	intruder.Subject = uuid.NewString()
	if _, _, err := repository.Bootstrap(context.Background(), intruder); !errors.Is(err, ErrForbidden) {
		t.Fatalf("session reuse error=%v", err)
	}
	var identities int
	if err := repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM identities WHERE issuer=$1 AND subject=$2`, intruder.Issuer, intruder.Subject).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("rejected session creation did not roll back: count=%d error=%v", identities, err)
	}
}

func TestPostgresConcurrentBootstrapHasOneIdentity(t *testing.T) {
	repository := accountDatabase(t)
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	start := make(chan struct{})
	results := make(chan error, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _, err := repository.Bootstrap(context.Background(), principal)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent bootstrap failed: %v", err)
		}
	}
	var users, sessions int
	if err := repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if users != 1 || sessions != 1 {
		t.Fatalf("users=%d sessions=%d", users, sessions)
	}
}

func TestPostgresMoveUnitChecksDestinationScopeAndActiveState(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	admin, adminMe := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, admin, organization.ID, []string{"MEMBER"})
	root := accountUnit(t, repository, owner, organization.ID, nil, "authorized root")
	sibling := accountUnit(t, repository, owner, organization.ID, nil, "other root")
	child := accountUnit(t, repository, owner, organization.ID, &root.ID, "child")
	destination := accountUnit(t, repository, owner, organization.ID, &root.ID, "destination")
	grandchild := accountUnit(t, repository, owner, organization.ID, &child.ID, "grandchild")
	accountExec(t, repository, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at) SELECT $1,id,'UNIT_ADMIN',$2,$3,now() FROM memberships WHERE organization_id=$4 AND user_id=$3`, uuid.New(), root.ID, adminMe.UserID, organization.ID)
	for _, target := range []*uuid.UUID{&sibling.ID, nil} {
		if _, err := repository.MoveUnit(context.Background(), admin, organization.ID, child.ID, target, 1); !errors.Is(err, ErrForbidden) {
			t.Fatalf("out-of-scope move target=%v error=%v", target, err)
		}
	}
	accountExec(t, repository, `UPDATE organization_units SET status='DISABLED' WHERE id=$1`, destination.ID)
	if _, err := repository.MoveUnit(context.Background(), admin, organization.ID, child.ID, &destination.ID, 1); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("disabled destination error=%v", err)
	}
	accountExec(t, repository, `UPDATE organization_units SET status='ACTIVE' WHERE id=$1`, destination.ID)
	moved, err := repository.MoveUnit(context.Background(), admin, organization.ID, child.ID, &destination.ID, 1)
	if err != nil || moved.Version != 2 || moved.ParentID == nil || *moved.ParentID != destination.ID {
		t.Fatalf("authorized move=%+v error=%v", moved, err)
	}
	var depth int
	if err := repository.pool.QueryRow(context.Background(), `SELECT depth FROM organization_unit_closure WHERE organization_id=$1 AND ancestor_id=$2 AND descendant_id=$3`, organization.ID, root.ID, grandchild.ID).Scan(&depth); err != nil || depth != 3 {
		t.Fatalf("subtree closure depth=%d error=%v", depth, err)
	}
	if _, err := repository.MoveUnit(context.Background(), owner, organization.ID, destination.ID, &grandchild.ID, 1); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("cycle error=%v", err)
	}
	accountExec(t, repository, `UPDATE organization_units SET status='DISABLED' WHERE id=$1`, child.ID)
	if _, err := repository.MoveUnit(context.Background(), owner, organization.ID, child.ID, nil, 2); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("disabled source error=%v", err)
	}
	missing := uuid.New()
	if _, err := repository.MoveUnit(context.Background(), owner, organization.ID, destination.ID, &missing, 1); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("missing destination error=%v", err)
	}
	foreign := accountOrganization(t, repository, owner)
	foreignUnit := accountUnit(t, repository, owner, foreign.ID, nil, "foreign")
	if _, err := repository.MoveUnit(context.Background(), owner, organization.ID, destination.ID, &foreignUnit.ID, 1); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("foreign organization destination error=%v", err)
	}
}

func TestPostgresTransferOwnershipRetainsMembershipAndVersions(t *testing.T) {
	repository := accountDatabase(t)
	owner, ownerMe := accountUser(t, repository)
	target, targetMe := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	accountInvitation(t, repository, owner, target, organization.ID, []string{"MEMBER"})
	if _, err := repository.SetActiveContext(context.Background(), owner, &organization.ID); err != nil {
		t.Fatal(err)
	}
	before, err := repository.ResolveAuthorizationContext(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.TransferOwnership(context.Background(), owner, organization.ID, targetMe.UserID); err != nil {
		t.Fatal(err)
	}
	organizations, err := repository.ListOrganizations(context.Background(), owner)
	if err != nil || len(organizations) != 1 || !slices.Equal(organizations[0].Roles, []string{"MEMBER"}) {
		t.Fatalf("previous owner membership=%+v error=%v", organizations, err)
	}
	resolved, err := repository.ResolveAuthorizationContext(context.Background(), owner)
	if err != nil || !slices.Equal(resolved.Roles, []string{"MEMBER"}) || resolved.MembershipVersion <= before.MembershipVersion {
		t.Fatalf("previous owner context=%+v error=%v", resolved, err)
	}
	if err := repository.RevokeMembership(context.Background(), target, organization.ID, targetMe.UserID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("last owner error=%v", err)
	}
	members, err := repository.ListMembers(context.Background(), target, organization.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members=%+v error=%v", members, err)
	}
	for _, member := range members {
		if member.Version != 2 || (member.UserID == ownerMe.UserID && slices.Contains(member.Roles, "OWNER")) || (member.UserID == targetMe.UserID && !slices.Contains(member.Roles, "OWNER")) {
			t.Fatalf("transfer member=%+v", member)
		}
	}
	assertMembershipEvents(t, repository)
}

func TestPostgresReinvitationCannotRestoreRevokedGrants(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	invitation := accountInvitation(t, repository, owner, member, organization.ID, []string{"BILLING_ADMIN"})
	unit := accountUnit(t, repository, owner, organization.ID, nil, "old scope")
	// The original schema permits multiple owners. Revoking one and inviting
	// them back as a member must not silently restore ownership either.
	accountExec(t, repository, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at) SELECT $1,id,'OWNER',$2,now() FROM memberships WHERE organization_id=$3 AND user_id=$2`, uuid.New(), me.UserID, organization.ID)
	accountExec(t, repository, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at) SELECT $1,id,'UNIT_ADMIN',$2,$3,now() FROM memberships WHERE organization_id=$4 AND user_id=$3`, uuid.New(), unit.ID, me.UserID, organization.ID)
	accountExec(t, repository, `INSERT INTO membership_units(membership_id,unit_id,created_at) SELECT id,$1,now() FROM memberships WHERE organization_id=$2 AND user_id=$3`, unit.ID, organization.ID, me.UserID)
	if _, err := repository.SetActiveContext(context.Background(), member, &organization.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReplaceMemberRoles(context.Background(), owner, organization.ID, me.UserID, []string{"BILLING_ADMIN"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeMembership(context.Background(), owner, organization.ID, me.UserID); err != nil {
		t.Fatal(err)
	}
	// Replaying an old accepted token must not change the revoked membership.
	if _, err := repository.AcceptInvitation(context.Background(), member, invitation.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetActiveContext(context.Background(), member, &organization.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked membership reactivated by old token: %v", err)
	}
	accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	if _, err := repository.SetActiveContext(context.Background(), member, &organization.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := repository.ResolveAuthorizationContext(context.Background(), member)
	if err != nil || !slices.Equal(resolved.Roles, []string{"MEMBER"}) || len(resolved.UnitIDs) != 0 || len(resolved.AncestorUnitIDs) != 0 {
		t.Fatalf("new invitation restored old grants: context=%+v error=%v", resolved, err)
	}
	members, err := repository.ListMembers(context.Background(), owner, organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.UserID == me.UserID && (member.Version != 4 || len(member.UnitIDs) != 0 || !slices.Equal(member.Roles, []string{"MEMBER"})) {
			t.Fatalf("rejoined member=%+v", member)
		}
	}
	assertMembershipEvents(t, repository)
}

func TestPostgresDisabledOrganizationFailsClosed(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	member, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	invitation := accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"})
	pending, err := repository.CreateInvitation(context.Background(), owner, organization.ID, member.Email, []string{"BILLING_ADMIN"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	unit := accountUnit(t, repository, owner, organization.ID, nil, "unit")
	if _, err := repository.SetActiveContext(context.Background(), owner, &organization.ID); err != nil {
		t.Fatal(err)
	}
	accountExec(t, repository, `UPDATE organizations SET status='DISABLED' WHERE id=$1`, organization.ID)
	checks := map[string]func() error{
		"authorization": func() error {
			_, err := repository.ResolveAuthorizationContext(context.Background(), owner)
			return err
		},
		"directory units": func() error { _, err := repository.ListUnits(context.Background(), owner, organization.ID); return err },
		"directory members": func() error {
			_, err := repository.ListMembers(context.Background(), owner, organization.ID)
			return err
		},
		"create unit": func() error {
			_, err := repository.CreateUnit(context.Background(), owner, organization.ID, nil, "new")
			return err
		},
		"move unit": func() error {
			_, err := repository.MoveUnit(context.Background(), owner, organization.ID, unit.ID, nil, 1)
			return err
		},
		"accept invitation": func() error {
			_, err := repository.AcceptInvitation(context.Background(), member, invitation.Token)
			return err
		},
		"accept pending invitation": func() error {
			_, err := repository.AcceptInvitation(context.Background(), member, pending.Token)
			return err
		},
		"replace roles": func() error {
			_, err := repository.ReplaceMemberRoles(context.Background(), owner, organization.ID, me.UserID, nil)
			return err
		},
		"revoke membership": func() error {
			return repository.RevokeMembership(context.Background(), owner, organization.ID, me.UserID)
		},
		"transfer ownership": func() error {
			return repository.TransferOwnership(context.Background(), owner, organization.ID, me.UserID)
		},
		"set context": func() error {
			_, err := repository.SetActiveContext(context.Background(), owner, &organization.ID)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
				t.Fatalf("disabled organization action error=%v", err)
			}
		})
	}
	organizations, err := repository.ListOrganizations(context.Background(), owner)
	if err != nil || len(organizations) != 0 {
		t.Fatalf("disabled organization listed: %+v error=%v", organizations, err)
	}
	if _, err := repository.SetActiveContext(context.Background(), owner, nil); err != nil {
		t.Fatal(err)
	}
	if resolved, err := repository.ResolveAuthorizationContext(context.Background(), owner); err != nil || resolved.ContextType != "PERSONAL" {
		t.Fatalf("personal recovery failed: %+v error=%v", resolved, err)
	}
}

func TestPostgresDirectoryRolesAreUnique(t *testing.T) {
	repository := accountDatabase(t)
	owner, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	for _, name := range []string{"one", "two"} {
		unit := accountUnit(t, repository, owner, organization.ID, nil, name)
		accountExec(t, repository, `INSERT INTO role_bindings(id,membership_id,role,unit_id,created_by,created_at) SELECT $1,id,'UNIT_ADMIN',$2,$3,now() FROM memberships WHERE organization_id=$4 AND user_id=$3`, uuid.New(), unit.ID, me.UserID, organization.ID)
	}
	organizations, err := repository.ListOrganizations(context.Background(), owner)
	if err != nil || len(organizations) != 1 || !slices.Equal(organizations[0].Roles, []string{"OWNER", "UNIT_ADMIN"}) {
		t.Fatalf("organization roles=%+v error=%v", organizations, err)
	}
	members, err := repository.ListMembers(context.Background(), owner, organization.ID)
	if err != nil || len(members) != 1 || !slices.Equal(members[0].Roles, []string{"OWNER", "UNIT_ADMIN"}) {
		t.Fatalf("member roles=%+v error=%v", members, err)
	}
	updated, err := repository.ReplaceMemberRoles(context.Background(), owner, organization.ID, me.UserID, []string{"AUDITOR"})
	if err != nil || !slices.Equal(updated.Roles, []string{"AUDITOR", "MEMBER", "OWNER", "UNIT_ADMIN"}) {
		t.Fatalf("updated roles=%+v error=%v", updated, err)
	}
	handler := NewService(repository, &fakeWalletProvisioner{}).Handler(func(context.Context, string) (identity.Principal, error) { return owner, nil })
	request := httptest.NewRequest(http.MethodGet, "/v1/me/organizations", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var wire struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || len(wire.Items) != 1 || len(wire.Items[0]) != 5 || wire.Items[0]["status"] != "ACTIVE" || wire.Items[0]["version"] != float64(1) {
		t.Fatalf("organization summary wire status=%d body=%s error=%v", response.Code, response.Body.String(), err)
	}
	assertMembershipEvents(t, repository)
}

func TestPostgresMembershipEventCorrelationFitsExistingSchema(t *testing.T) {
	repository := accountDatabase(t)
	owner, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	application := NewService(repository, &fakeWalletProvisioner{}).Handler(func(context.Context, string) (identity.Principal, error) { return owner, nil })
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "account-service"}, application, nil)
	for _, supplied := range []string{strings.Repeat("x", 65), "caller-correlation", "", strings.Repeat("界", 64), strings.Repeat("界", 65), "\xff"} {
		request := httptest.NewRequest(http.MethodPut, "/v1/organizations/"+organization.ID.String()+"/members/"+me.UserID.String()+"/roles", strings.NewReader(`{"roles":["MEMBER"]}`))
		request.Header.Set("Authorization", "Bearer test-token")
		request.Header.Set("X-Request-ID", supplied)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("role replace status=%d body=%s", response.Code, response.Body.String())
		}
		var traceID string
		if err := repository.pool.QueryRow(context.Background(), `SELECT trace_id FROM outbox_events WHERE event_type='account.membership.changed.v1' ORDER BY occurred_at DESC,event_id DESC LIMIT 1`).Scan(&traceID); err != nil {
			t.Fatal(err)
		}
		if traceID == "" || !utf8.ValidString(traceID) || utf8.RuneCountInString(traceID) > 64 || traceID != response.Header().Get("X-Trace-ID") {
			t.Fatalf("event correlation violates schema: trace length=%d response trace equal=%v", utf8.RuneCountInString(traceID), traceID == response.Header().Get("X-Trace-ID"))
		}
		if supplied != "" && utf8.ValidString(supplied) && utf8.RuneCountInString(supplied) <= 64 && response.Header().Get("X-Request-ID") != supplied {
			t.Fatal("valid caller request correlation was not preserved")
		}
	}
	assertMembershipEvents(t, repository)
}

func TestPostgresAccountHTTPMalformedRolesRequestDoesNotMutate(t *testing.T) {
	repository := accountDatabase(t)
	owner, _ := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	handler := NewService(repository, &fakeWalletProvisioner{}).Handler(func(context.Context, string) (identity.Principal, error) { return owner, nil })
	me, err := repository.GetMe(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{`, `null`, `{}`, `{"roles":null}`, `{"roles":["MEMBER"],"unknown":true}`, `{"roles":[]} {}`, strings.Repeat(" ", (1<<20)+1)} {
		request := httptest.NewRequest(http.MethodPut, "/v1/organizations/"+organization.ID.String()+"/members/"+me.UserID.String()+"/roles", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_REQUEST") {
			t.Fatalf("malformed request status=%d body=%s", response.Code, response.Body.String())
		}
	}
	members, err := repository.ListMembers(context.Background(), owner, organization.ID)
	if err != nil || len(members) != 1 || members[0].Version != 1 || !slices.Equal(members[0].Roles, []string{"OWNER"}) {
		t.Fatalf("malformed request mutated roles: %+v error=%v", members, err)
	}
}

func assertMembershipEvents(t *testing.T, repository *PostgresRepository) {
	t.Helper()
	rows, err := repository.pool.Query(context.Background(), `SELECT aggregate_id,aggregate_version,payload FROM outbox_events WHERE event_type='account.membership.changed.v1' ORDER BY occurred_at,event_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	latest := map[uuid.UUID]int64{}
	for rows.Next() {
		count++
		var membershipID uuid.UUID
		var version int64
		var payload []byte
		if err := rows.Scan(&membershipID, &version, &payload); err != nil {
			t.Fatal(err)
		}
		var event struct {
			OrganizationID uuid.UUID `json:"organization_id"`
			UserID         uuid.UUID `json:"user_id"`
			Status         string    `json:"membership_status"`
			Version        int64     `json:"membership_version"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil || event.OrganizationID == uuid.Nil || event.UserID == uuid.Nil || event.Version != version || version < 1 || !slices.Contains([]string{"ACTIVE", "SUSPENDED", "REVOKED"}, event.Status) {
			t.Fatalf("membership event version=%d payload=%s error=%v", version, payload, err)
		}
		if previous := latest[membershipID]; previous != 0 && version != previous+1 {
			t.Fatalf("membership event version did not advance: previous=%d current=%d", previous, version)
		}
		latest[membershipID] = version
	}
	if rows.Err() != nil || count == 0 {
		t.Fatalf("membership event count=%d error=%v", count, rows.Err())
	}
	rows.Close()
	for membershipID, version := range latest {
		var stored int64
		if err := repository.pool.QueryRow(context.Background(), `SELECT version FROM memberships WHERE id=$1`, membershipID).Scan(&stored); err != nil || stored != version {
			t.Fatalf("latest event version=%d persisted=%d error=%v", version, stored, err)
		}
	}
}

func TestPostgresOrganizationOwnerMembershipEventsStartAtOne(t *testing.T) {
	repository := accountDatabase(t)
	owner, me := accountUser(t, repository)
	organization := accountOrganization(t, repository, owner)
	if _, err := repository.ReplaceMemberRoles(context.Background(), owner, organization.ID, me.UserID, []string{"MEMBER"}); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.pool.Query(context.Background(), `SELECT e.aggregate_version FROM outbox_events e JOIN memberships m ON m.id=e.aggregate_id WHERE e.event_type='account.membership.changed.v1' AND m.organization_id=$1 AND m.user_id=$2 ORDER BY e.aggregate_version`, organization.ID, me.UserID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := []int64{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if rows.Err() != nil || !slices.Equal(versions, []int64{1, 2}) {
		t.Fatalf("owner membership versions=%v error=%v", versions, rows.Err())
	}
}
