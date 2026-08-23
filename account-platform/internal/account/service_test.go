package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type fakeRepository struct {
	me            Me
	activateCalls int
	adminCalls    []string
	deny          bool
	lookupResult  AdminUser
	lookupErr     error
	setExpected   int64
	setContext    Context
}

func (f *fakeRepository) Bootstrap(context.Context, identity.Principal) (Me, bool, error) {
	return f.me, true, nil
}
func (f *fakeRepository) ActivateUser(_ context.Context, userID uuid.UUID) error {
	if userID != f.me.UserID {
		return errors.New("unexpected user")
	}
	f.activateCalls++
	f.me.Status = "ACTIVE"
	return nil
}
func (f *fakeRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return f.me, nil
}
func (*fakeRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	panic("not used")
}

func (*fakeRepository) GetCommerceCustomerMapping(context.Context, identity.Principal) (CommerceCustomerMapping, error) {
	return CommerceCustomerMapping{Status: "READY"}, nil
}
func (f *fakeRepository) Authorize(_ context.Context, _ identity.Principal, permission string, organizationID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: !f.deny, Permission: permission, OrganizationID: organizationID, ContextVersion: 1, AuthorizationVersion: 1}, nil
}
func (*fakeRepository) RoleDefinitions(context.Context) ([]AdminRoleDefinition, error) {
	return []AdminRoleDefinition{{Code: "PLATFORM_SUPER_ADMIN", Scope: "PLATFORM", Permissions: []string{PermissionPlatformRolesRead}}}, nil
}
func (f *fakeRepository) LookupUserByCode(_ context.Context, _ identity.Principal, code, source, device string) (AdminUser, error) {
	f.adminCalls = append(f.adminCalls, "user-code:"+code+":"+source+":"+device)
	return f.lookupResult, f.lookupErr
}
func (*fakeRepository) ListOrganizations(context.Context, identity.Principal) ([]Organization, error) {
	panic("not used")
}
func (f *fakeRepository) SetActiveContext(_ context.Context, _ identity.Principal, organizationID *uuid.UUID, expected int64) (Context, error) {
	f.setExpected = expected
	f.setContext.OrganizationID = organizationID
	return f.setContext, nil
}
func (*fakeRepository) ListUnits(context.Context, identity.Principal, uuid.UUID) (UnitTree, error) {
	panic("not used")
}
func (*fakeRepository) ListMembers(context.Context, identity.Principal, uuid.UUID) ([]Member, error) {
	panic("not used")
}
func (*fakeRepository) ListQuotaMembers(context.Context, identity.Principal, uuid.UUID) ([]QuotaMember, error) {
	panic("not used")
}
func (*fakeRepository) ListMyRoleBindings(context.Context, identity.Principal) ([]RoleBinding, error) {
	return []RoleBinding{}, nil
}
func (*fakeRepository) ListOrganizationRoleBindings(context.Context, identity.Principal, uuid.UUID) ([]RoleBinding, error) {
	return []RoleBinding{}, nil
}
func (*fakeRepository) GrantOrganizationRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error) {
	return RoleBinding{}, nil
}
func (*fakeRepository) RevokeOrganizationRoleBinding(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (RoleBinding, error) {
	return RoleBinding{}, nil
}
func (*fakeRepository) ReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string) (Member, error) {
	panic("not used")
}
func (*fakeRepository) CreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string, string) (UnitMutationResult, error) {
	panic("not used")
}
func (*fakeRepository) MoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64, string) (UnitMutationResult, error) {
	panic("not used")
}
func (*fakeRepository) CreateInvitation(context.Context, identity.Principal, uuid.UUID, InvitationCreateRequest) (Invitation, error) {
	panic("not used")
}
func (*fakeRepository) ListMyInvitations(context.Context, identity.Principal) ([]Invitation, error) {
	panic("not used")
}
func (*fakeRepository) RevokeInvitation(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (Invitation, error) {
	panic("not used")
}
func (*fakeRepository) AcceptInvitation(context.Context, identity.Principal, string, *uuid.UUID) (MembershipMutationResult, error) {
	panic("not used")
}
func (*fakeRepository) RevokeMembership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (MembershipMutationResult, error) {
	panic("not used")
}
func (*fakeRepository) TransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (OwnershipTransferResult, error) {
	panic("not used")
}
func (*fakeRepository) AdminOverview(context.Context) (AdminOverview, error) {
	return AdminOverview{}, nil
}
func (*fakeRepository) AdminRoleBindings(context.Context, *uuid.UUID, *uuid.UUID) ([]RoleBinding, error) {
	return []RoleBinding{}, nil
}
func (*fakeRepository) AdminGrantRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error) {
	return RoleBinding{}, nil
}
func (*fakeRepository) AdminRevokeRoleBinding(context.Context, identity.Principal, uuid.UUID) (RoleBinding, error) {
	return RoleBinding{}, nil
}
func (*fakeRepository) AdminUsers(context.Context, string, int) ([]AdminUser, error) { return nil, nil }
func (*fakeRepository) AdminUserMemberships(context.Context, uuid.UUID) ([]AdminUserMembership, error) {
	return nil, nil
}
func (f *fakeRepository) AdminSetUserStatus(_ context.Context, _ identity.Principal, id uuid.UUID, status string) error {
	f.adminCalls = append(f.adminCalls, "user:"+id.String()+":"+status)
	return nil
}
func (f *fakeRepository) AdminDeleteUser(_ context.Context, _ identity.Principal, id uuid.UUID) error {
	f.adminCalls = append(f.adminCalls, "user:"+id.String()+":DELETE")
	return nil
}
func (*fakeRepository) AdminOrganizations(context.Context, AdminOrganizationFilter) (AdminOrganizationPage, error) {
	return AdminOrganizationPage{Items: []AdminOrganization{}}, nil
}
func (f *fakeRepository) AdminCreateOrganization(_ context.Context, _ identity.Principal, ownerID uuid.UUID, name, _ string) (Organization, error) {
	f.adminCalls = append(f.adminCalls, "organization:create:"+ownerID.String()+":"+name)
	return Organization{ID: uuid.Must(uuid.NewV7()), Name: name, Status: "ACTIVE", Version: 1}, nil
}
func (*fakeRepository) AdminRecordOrganizationCreationFailure(context.Context, identity.Principal, uuid.UUID, string, string) error {
	return nil
}
func (f *fakeRepository) AdminOrganizationState(_ context.Context, id uuid.UUID) (AdminOrganizationState, error) {
	return AdminOrganizationState{ID: id, Status: "DISABLED", Version: 1}, nil
}
func (*fakeRepository) AdminOrganizationDeletionBlockers(context.Context, uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	return []OrganizationLifecycleBlocker{}, nil
}
func (f *fakeRepository) AdminRenameOrganization(_ context.Context, _ identity.Principal, id uuid.UUID, name string) error {
	f.adminCalls = append(f.adminCalls, "organization:"+id.String()+":rename:"+name)
	return nil
}
func (f *fakeRepository) AdminSetOrganizationStatus(_ context.Context, _ identity.Principal, id uuid.UUID, status string) error {
	f.adminCalls = append(f.adminCalls, "organization:"+id.String()+":"+status)
	return nil
}
func (f *fakeRepository) AdminDeleteOrganization(_ context.Context, _ identity.Principal, id uuid.UUID) error {
	f.adminCalls = append(f.adminCalls, "organization:"+id.String()+":DELETE")
	return nil
}
func (f *fakeRepository) AdminUnits(_ context.Context, organizationID uuid.UUID) (UnitTree, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":units")
	return UnitTree{Items: []Unit{}, StructureVersion: 1}, nil
}
func (f *fakeRepository) AdminUnitState(_ context.Context, organizationID, unitID uuid.UUID) (UnitMutationResult, error) {
	return UnitMutationResult{Unit: Unit{ID: unitID, OrganizationID: organizationID, Status: "DELETED", Version: 2}, StructureVersion: 2}, nil
}
func (f *fakeRepository) AdminMembers(_ context.Context, organizationID uuid.UUID) ([]Member, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":members")
	return []Member{}, nil
}
func (f *fakeRepository) AdminAddMember(_ context.Context, _ identity.Principal, organizationID, userID uuid.UUID, roles []string, unitIDs []uuid.UUID, _ string) (MembershipMutationResult, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":member:add:"+userID.String())
	return MembershipMutationResult{OrganizationID: organizationID, Member: Member{UserID: userID, Roles: roles, UnitAdminIDs: unitIDs}}, nil
}
func (f *fakeRepository) AdminReplaceMemberRoles(_ context.Context, _ identity.Principal, organizationID, userID uuid.UUID, roles []string, unitIDs []uuid.UUID) (Member, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":member:roles:"+userID.String())
	return Member{UserID: userID, Roles: roles, UnitAdminIDs: unitIDs}, nil
}
func (f *fakeRepository) AdminRemoveMember(_ context.Context, _ identity.Principal, organizationID, userID uuid.UUID, _ string) (MembershipMutationResult, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":member:remove:"+userID.String())
	return MembershipMutationResult{OrganizationID: organizationID, Member: Member{UserID: userID, Status: "REVOKED"}}, nil
}
func (f *fakeRepository) AdminTransferOwnership(_ context.Context, _ identity.Principal, organizationID, ownerID uuid.UUID, _ string) (OwnershipTransferResult, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":owner:"+ownerID.String())
	return OwnershipTransferResult{OrganizationID: organizationID, NewOwnerID: ownerID}, nil
}
func (f *fakeRepository) AdminCreateUnit(_ context.Context, _ identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name, _ string) (UnitMutationResult, error) {
	parent := "root"
	if parentID != nil {
		parent = parentID.String()
	}
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:create:"+parent+":"+name)
	return UnitMutationResult{Unit: Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}, StructureVersion: 2}, nil
}
func (f *fakeRepository) AdminRenameUnit(_ context.Context, _ identity.Principal, organizationID, unitID uuid.UUID, name, _ string) (UnitMutationResult, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:"+unitID.String()+":rename:"+name)
	return UnitMutationResult{Unit: Unit{ID: unitID, OrganizationID: organizationID, Name: name, Status: "ACTIVE", Version: 2}, StructureVersion: 2}, nil
}
func (*fakeRepository) AdminUnitDeletionBlockers(context.Context, uuid.UUID, uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	return []OrganizationLifecycleBlocker{}, nil
}
func (f *fakeRepository) AdminDeleteUnit(_ context.Context, _ identity.Principal, organizationID, unitID uuid.UUID, _ string) (UnitMutationResult, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:"+unitID.String()+":DELETE")
	return UnitMutationResult{Unit: Unit{ID: unitID, OrganizationID: organizationID, Status: "DELETED", Version: 2}, StructureVersion: 2}, nil
}
func (*fakeRepository) AdminAuditLogs(context.Context, AdminAuditFilter) ([]AdminAuditLog, error) {
	return nil, nil
}
func (*fakeRepository) AdminAuditLog(context.Context, uuid.UUID) (AdminAuditLog, error) {
	return AdminAuditLog{}, nil
}
func (*fakeRepository) Ping(context.Context) error { return nil }
func (*fakeRepository) Close()                     {}

type fakeWalletProvisioner struct {
	err   error
	calls int
}

func (f *fakeWalletProvisioner) EnsurePersonalWallet(context.Context, uuid.UUID) error {
	f.calls++
	return f.err
}

func TestBootstrapReturnsProvisioningWithoutCallingBilling(t *testing.T) {
	userID := uuid.Must(uuid.NewV7())
	repository := &fakeRepository{me: Me{UserID: userID, Status: "PROVISIONING", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if me.Status != "PROVISIONING" || wallets.calls != 0 || repository.activateCalls != 0 {
		t.Fatalf("me=%+v wallet calls=%d activate calls=%d", me, wallets.calls, repository.activateCalls)
	}
}

func TestBootstrapDoesNotDependOnBillingAvailability(t *testing.T) {
	repository := &fakeRepository{me: Me{UserID: uuid.Must(uuid.NewV7()), Status: "PROVISIONING", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{err: errors.New("unavailable")}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if me.Status != "PROVISIONING" || wallets.calls != 0 || repository.activateCalls != 0 {
		t.Fatalf("me=%+v wallet calls=%d activate calls=%d", me, wallets.calls, repository.activateCalls)
	}
}

func TestBootstrapDoesNotReactivateDisabledUser(t *testing.T) {
	repository := &fakeRepository{me: Me{UserID: uuid.Must(uuid.NewV7()), Status: "DISABLED", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if me.Status != "DISABLED" || wallets.calls != 0 || repository.activateCalls != 0 {
		t.Fatalf("me=%+v wallet calls=%d activate calls=%d", me, wallets.calls, repository.activateCalls)
	}
}

func TestActiveContextSwitchRequiresAndReturnsGlobalVersion(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	repository := &fakeRepository{
		me:         Me{UserID: uuid.Must(uuid.NewV7()), Status: "ACTIVE", ActiveContext: Context{Type: "PERSONAL", ContextVersion: 7, AuthorizationVersion: 2}},
		setContext: Context{Type: "ORGANIZATION", MembershipVersion: 3, ContextVersion: 8, AuthorizationVersion: 4},
	}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "user", SessionID: "session"}, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/current/active-context", strings.NewReader(`{"context_type":"ORGANIZATION","organization_id":"`+organizationID.String()+`","expected_context_version":7}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusOK || repository.setExpected != 7 || response.Header().Get("X-Context-Version") != "8" || !strings.Contains(response.Body.String(), `"context_version":8`) {
		t.Fatalf("status=%d expected=%d headers=%v body=%s", response.Code, repository.setExpected, response.Header(), response.Body.String())
	}
}

func TestPersonalUsersCannotCreateOrganizations(t *testing.T) {
	repository := &fakeRepository{}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "user", SessionID: "session"}, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/organizations", strings.NewReader(`{"name":"Example"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLegacyRealmRoleCannotBypassAccountPermissionDenial(t *testing.T) {
	repository := &fakeRepository{deny: true}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "legacy-admin", SessionID: "session", RealmRoles: []string{"platform-admin"}}, nil
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/users", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "PERMISSION_DENIED") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminUserCodeLookupUsesDedicatedExactRouteAndNoLeakage(t *testing.T) {
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "operator", SessionID: "session"}, nil
	}
	code := "USR-0123-4567-89AB-CDEF-MV"
	repository := &fakeRepository{lookupResult: AdminUser{ID: uuid.Must(uuid.NewV7()), UserCode: code, Status: "ACTIVE"}}
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/user-code-lookups/"+code, nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("X-Device-ID", "device-1")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), code) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := repository.adminCalls[len(repository.adminCalls)-1]; got != "user-code:"+code+":192.0.2.10:device-1" {
		t.Fatalf("lookup=%q", got)
	}

	repository.lookupErr = ErrNotFound
	response = httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).Handler(verify).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), code) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminLifecycleRoutesDispatchProtectedMutations(t *testing.T) {
	repository := &fakeRepository{}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "admin", SessionID: "session"}, nil
	}
	handler := NewService(repository, &fakeWalletProvisioner{}).Handler(verify)
	userID := uuid.Must(uuid.NewV7())
	organizationID := uuid.Must(uuid.NewV7())
	tests := []struct {
		method string
		path   string
		body   string
		want   string
	}{
		{http.MethodPatch, "/admin/v1/users/" + userID.String() + "/status", `{"status":"DISABLED"}`, "user:" + userID.String() + ":DISABLED"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String(), `{"name":"Renamed"}`, "organization:" + organizationID.String() + ":rename:Renamed"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String() + "/status", `{"status":"ACTIVE"}`, "organization:" + organizationID.String() + ":ACTIVE"},
		{http.MethodDelete, "/admin/v1/organizations/" + organizationID.String(), "", "organization:" + organizationID.String() + ":DELETE"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer test-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "organization-test-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
		if got := repository.adminCalls[len(repository.adminCalls)-1]; got != test.want {
			t.Fatalf("admin call=%q want=%q", got, test.want)
		}
	}
	request := httptest.NewRequest(http.MethodDelete, "/admin/v1/users/"+userID.String(), nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("direct administrator deletion must not bypass closure saga: status=%d", response.Code)
	}
}

func TestAdminOrganizationManagementRoutesCreateOrganizationsAndUnits(t *testing.T) {
	repository := &fakeRepository{}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "admin", SessionID: "session"}, nil
	}
	handler := NewService(repository, &fakeWalletProvisioner{}).Handler(verify)
	ownerID := uuid.Must(uuid.NewV7())
	organizationID := uuid.Must(uuid.NewV7())
	parentID := uuid.Must(uuid.NewV7())
	unitID := uuid.Must(uuid.NewV7())
	tests := []struct {
		method string
		path   string
		body   string
		status int
		want   string
	}{
		{http.MethodPost, "/admin/v1/organizations", `{"name":"Example","owner_user_id":"` + ownerID.String() + `"}`, http.StatusCreated, "organization:create:" + ownerID.String() + ":Example"},
		{http.MethodGet, "/admin/v1/organizations/" + organizationID.String() + "/units", "", http.StatusOK, "organization:" + organizationID.String() + ":units"},
		{http.MethodGet, "/admin/v1/organizations/" + organizationID.String() + "/members", "", http.StatusOK, "organization:" + organizationID.String() + ":members"},
		{http.MethodPost, "/admin/v1/organizations/" + organizationID.String() + "/ownership", `{"new_owner_user_id":"` + ownerID.String() + `"}`, http.StatusOK, "organization:" + organizationID.String() + ":owner:" + ownerID.String()},
		{http.MethodPost, "/admin/v1/organizations/" + organizationID.String() + "/units", `{"name":"Engineering","parent_id":"` + parentID.String() + `"}`, http.StatusCreated, "organization:" + organizationID.String() + ":unit:create:" + parentID.String() + ":Engineering"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String() + "/units/" + unitID.String(), `{"name":"Platform"}`, http.StatusOK, "organization:" + organizationID.String() + ":unit:" + unitID.String() + ":rename:Platform"},
		{http.MethodDelete, "/admin/v1/organizations/" + organizationID.String() + "/units/" + unitID.String(), "", http.StatusOK, "organization:" + organizationID.String() + ":unit:" + unitID.String() + ":DELETE"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer test-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "organization-create-test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
		if got := repository.adminCalls[len(repository.adminCalls)-1]; got != test.want {
			t.Fatalf("admin call=%q want=%q", got, test.want)
		}
	}
}

func TestAdminOrganizationFilterCursorRoundTrip(t *testing.T) {
	item := AdminOrganization{ID: uuid.Must(uuid.NewV7()), UpdatedAt: time.Date(2026, time.August, 23, 12, 34, 56, 0, time.UTC)}
	cursor, err := encodeAdminOrganizationCursor(item)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/organizations?status=disabled&limit=25&cursor="+cursor, nil)
	filter, err := parseAdminOrganizationFilter(request)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Status != "DISABLED" || filter.Limit != 25 || filter.CursorID == nil || *filter.CursorID != item.ID || filter.CursorUpdatedAt == nil || !filter.CursorUpdatedAt.Equal(item.UpdatedAt) {
		t.Fatalf("unexpected filter: %#v", filter)
	}
}

func TestAdminOrganizationFilterRejectsUnknownStatusAndMalformedCursor(t *testing.T) {
	for _, target := range []string{"/admin/v1/organizations?status=PAUSED", "/admin/v1/organizations?cursor=not-base64"} {
		if _, err := parseAdminOrganizationFilter(httptest.NewRequest(http.MethodGet, target, nil)); err == nil {
			t.Fatalf("expected invalid filter for %s", target)
		}
	}
}

func TestInvitationRolesAreRestrictedAndMemberIsImplicit(t *testing.T) {
	roles, err := validateInvitationRoles([]string{"AUDITOR", "AUDITOR"})
	if err != nil || len(roles) != 2 || roles[0] != "AUDITOR" || roles[1] != "MEMBER" {
		t.Fatalf("roles=%v error=%v", roles, err)
	}
	if _, err := validateInvitationRoles([]string{"OWNER"}); err == nil {
		t.Fatal("owner must be transferred, not invited")
	}
}

func TestManagedRolesCannotGrantOwnership(t *testing.T) {
	roles, err := validateManagedRoles([]string{"AUDITOR", "AUDITOR"})
	if err != nil || len(roles) != 2 || roles[0] != "AUDITOR" || roles[1] != "MEMBER" {
		t.Fatalf("roles=%v error=%v", roles, err)
	}
	if _, err := validateManagedRoles([]string{"OWNER"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner grant must use ownership transfer: %v", err)
	}
	if _, err := validateManagedRoles([]string{"UNIT_ADMIN"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unit admin requires a scoped assignment workflow: %v", err)
	}
}

func TestRoleBindingGrantNormalizesValidityAndIdempotency(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 0, 0, 0, time.UTC)
	organizationID := uuid.Must(uuid.NewV7())
	expiresAt := now.Add(time.Hour)
	request, err := normalizeRoleBindingGrant(RoleBindingGrant{UserID: uuid.Must(uuid.NewV7()), OrganizationID: &organizationID, RoleCode: " auditor ", ExpiresAt: &expiresAt, IdempotencyKey: " grant-123 "}, now)
	if err != nil {
		t.Fatal(err)
	}
	if request.RoleCode != "AUDITOR" || request.IdempotencyKey != "grant-123" || request.StartsAt == nil || !request.StartsAt.Equal(now) {
		t.Fatalf("unexpected normalized grant: %#v", request)
	}
	invalidExpiry := now.Add(-time.Second)
	if _, err := normalizeRoleBindingGrant(RoleBindingGrant{UserID: request.UserID, OrganizationID: &organizationID, RoleCode: "AUDITOR", ExpiresAt: &invalidExpiry, IdempotencyKey: "grant-456"}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected invalid validity window conflict, got %v", err)
	}
}

func TestNormalizeAdminMemberRolesSeparatesUnitScope(t *testing.T) {
	unitID := uuid.Must(uuid.NewV7())
	roles, units, err := normalizeAdminMemberRoles([]string{"auditor", "ORG_ADMIN", "auditor"}, []uuid.UUID{unitID, unitID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(roles, ",") != "AUDITOR,MEMBER,ORG_ADMIN" || len(units) != 1 || units[0] != unitID {
		t.Fatalf("roles=%v units=%v", roles, units)
	}
	if _, _, err := normalizeAdminMemberRoles([]string{"OWNER"}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("OWNER must be changed through ownership transfer, got %v", err)
	}
	if _, _, err := normalizeAdminMemberRoles(nil, []uuid.UUID{uuid.Nil}); !errors.Is(err, ErrConflict) {
		t.Fatalf("nil unit must be rejected, got %v", err)
	}
}

func TestParseAdminAuditFilter(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/audit-logs?actor=admin%40example.com&action=organization&resource=unit&result=SUCCEEDED&from=2026-08-01T00:00:00Z&to=2026-08-19T23:59:59Z&limit=80", nil)
	filter, err := parseAdminAuditFilter(request)
	if err != nil || filter.Actor != "admin@example.com" || filter.Action != "organization" || filter.Resource != "unit" || filter.Result != "SUCCEEDED" || filter.Limit != 80 || filter.From == nil || filter.To == nil {
		t.Fatalf("unexpected audit filter: %#v, %v", filter, err)
	}
	invalid := httptest.NewRequest(http.MethodGet, "/admin/v1/audit-logs?result=UNKNOWN", nil)
	if _, err := parseAdminAuditFilter(invalid); err == nil {
		t.Fatal("invalid audit result must be rejected")
	}
}
