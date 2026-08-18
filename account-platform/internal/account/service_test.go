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
func (*fakeRepository) ListOrganizations(context.Context, identity.Principal) ([]Organization, error) {
	panic("not used")
}
func (*fakeRepository) SetActiveContext(context.Context, identity.Principal, *uuid.UUID) (Context, error) {
	panic("not used")
}
func (*fakeRepository) CreateOrganization(context.Context, identity.Principal, string) (Organization, error) {
	panic("not used")
}
func (*fakeRepository) ListUnits(context.Context, identity.Principal, uuid.UUID) ([]Unit, error) {
	panic("not used")
}
func (*fakeRepository) ListMembers(context.Context, identity.Principal, uuid.UUID) ([]Member, error) {
	panic("not used")
}
func (*fakeRepository) ReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string) (Member, error) {
	panic("not used")
}
func (*fakeRepository) CreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string) (Unit, error) {
	panic("not used")
}
func (*fakeRepository) MoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64) (Unit, error) {
	panic("not used")
}
func (*fakeRepository) CreateInvitation(context.Context, identity.Principal, uuid.UUID, string, []string, time.Time) (Invitation, error) {
	panic("not used")
}
func (*fakeRepository) AcceptInvitation(context.Context, identity.Principal, string) (Organization, error) {
	panic("not used")
}
func (*fakeRepository) RevokeMembership(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error {
	panic("not used")
}
func (*fakeRepository) TransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error {
	panic("not used")
}
func (*fakeRepository) AdminOverview(context.Context) (AdminOverview, error) {
	return AdminOverview{}, nil
}
func (*fakeRepository) AdminUsers(context.Context, string, int) ([]AdminUser, error) { return nil, nil }
func (f *fakeRepository) AdminSetUserStatus(_ context.Context, _ identity.Principal, id uuid.UUID, status string) error {
	f.adminCalls = append(f.adminCalls, "user:"+id.String()+":"+status)
	return nil
}
func (f *fakeRepository) AdminDeleteUser(_ context.Context, _ identity.Principal, id uuid.UUID) error {
	f.adminCalls = append(f.adminCalls, "user:"+id.String()+":DELETE")
	return nil
}
func (*fakeRepository) AdminOrganizations(context.Context, string, int) ([]AdminOrganization, error) {
	return nil, nil
}
func (f *fakeRepository) AdminCreateOrganization(_ context.Context, _ identity.Principal, ownerID uuid.UUID, name string) (Organization, error) {
	f.adminCalls = append(f.adminCalls, "organization:create:"+ownerID.String()+":"+name)
	return Organization{ID: uuid.Must(uuid.NewV7()), Name: name, Status: "ACTIVE", Version: 1}, nil
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
func (f *fakeRepository) AdminUnits(_ context.Context, organizationID uuid.UUID) ([]Unit, error) {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":units")
	return []Unit{}, nil
}
func (f *fakeRepository) AdminCreateUnit(_ context.Context, _ identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name string) (Unit, error) {
	parent := "root"
	if parentID != nil {
		parent = parentID.String()
	}
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:create:"+parent+":"+name)
	return Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}, nil
}
func (f *fakeRepository) AdminRenameUnit(_ context.Context, _ identity.Principal, organizationID, unitID uuid.UUID, name string) error {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:"+unitID.String()+":rename:"+name)
	return nil
}
func (f *fakeRepository) AdminDeleteUnit(_ context.Context, _ identity.Principal, organizationID, unitID uuid.UUID) error {
	f.adminCalls = append(f.adminCalls, "organization:"+organizationID.String()+":unit:"+unitID.String()+":DELETE")
	return nil
}
func (*fakeRepository) AdminAuditLogs(context.Context, int) ([]AdminAuditLog, error) { return nil, nil }
func (*fakeRepository) Ping(context.Context) error                                   { return nil }
func (*fakeRepository) Close()                                                       {}

type fakeWalletProvisioner struct {
	err   error
	calls int
}

func (f *fakeWalletProvisioner) EnsurePersonalWallet(context.Context, uuid.UUID) error {
	f.calls++
	return f.err
}

func TestBootstrapActivatesOnlyAfterWalletProvisioning(t *testing.T) {
	userID := uuid.Must(uuid.NewV7())
	repository := &fakeRepository{me: Me{UserID: userID, Status: "PROVISIONING", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if me.Status != "ACTIVE" || wallets.calls != 1 || repository.activateCalls != 1 {
		t.Fatalf("me=%+v wallet calls=%d activate calls=%d", me, wallets.calls, repository.activateCalls)
	}
}

func TestBootstrapRemainsProvisioningWhenBillingIsUnavailable(t *testing.T) {
	repository := &fakeRepository{me: Me{UserID: uuid.Must(uuid.NewV7()), Status: "PROVISIONING", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{err: errors.New("unavailable")}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{})
	if !errors.Is(err, ErrWalletProvisioning) {
		t.Fatalf("error = %v, want ErrWalletProvisioning", err)
	}
	if me.Status != "PROVISIONING" || repository.activateCalls != 0 {
		t.Fatalf("me=%+v activate calls=%d", me, repository.activateCalls)
	}
}

func TestBootstrapDoesNotReactivateDisabledUser(t *testing.T) {
	repository := &fakeRepository{me: Me{UserID: uuid.Must(uuid.NewV7()), Status: "DISABLED", ActiveContext: Context{Type: "PERSONAL", MembershipVersion: 1}}}
	wallets := &fakeWalletProvisioner{}
	me, err := NewService(repository, wallets).Bootstrap(context.Background(), identity.Principal{})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	if me.Status != "DISABLED" || wallets.calls != 0 || repository.activateCalls != 0 {
		t.Fatalf("me=%+v wallet calls=%d activate calls=%d", me, wallets.calls, repository.activateCalls)
	}
}

func TestAdminLifecycleRoutesDispatchProtectedMutations(t *testing.T) {
	repository := &fakeRepository{}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "admin", SessionID: "session", RealmRoles: []string{"platform-admin"}}, nil
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
		{http.MethodDelete, "/admin/v1/users/" + userID.String(), "", "user:" + userID.String() + ":DELETE"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String(), `{"name":"Renamed"}`, "organization:" + organizationID.String() + ":rename:Renamed"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String() + "/status", `{"status":"ACTIVE"}`, "organization:" + organizationID.String() + ":ACTIVE"},
		{http.MethodDelete, "/admin/v1/organizations/" + organizationID.String(), "", "organization:" + organizationID.String() + ":DELETE"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer test-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
		if got := repository.adminCalls[len(repository.adminCalls)-1]; got != test.want {
			t.Fatalf("admin call=%q want=%q", got, test.want)
		}
	}
}

func TestAdminOrganizationManagementRoutesCreateOrganizationsAndUnits(t *testing.T) {
	repository := &fakeRepository{}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "admin", SessionID: "session", RealmRoles: []string{"platform-admin"}}, nil
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
		{http.MethodPost, "/admin/v1/organizations/" + organizationID.String() + "/units", `{"name":"Engineering","parent_id":"` + parentID.String() + `"}`, http.StatusCreated, "organization:" + organizationID.String() + ":unit:create:" + parentID.String() + ":Engineering"},
		{http.MethodPatch, "/admin/v1/organizations/" + organizationID.String() + "/units/" + unitID.String(), `{"name":"Platform"}`, http.StatusNoContent, "organization:" + organizationID.String() + ":unit:" + unitID.String() + ":rename:Platform"},
		{http.MethodDelete, "/admin/v1/organizations/" + organizationID.String() + "/units/" + unitID.String(), "", http.StatusNoContent, "organization:" + organizationID.String() + ":unit:" + unitID.String() + ":DELETE"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer test-token")
		request.Header.Set("Content-Type", "application/json")
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
