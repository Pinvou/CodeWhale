package token

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRoutingNeverFallsBackAcrossContexts(t *testing.T) {
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	userID, organizationID, walletID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	personal := Candidate{TokenAccountID: uuid.Must(uuid.NewV7()), OwnerType: "USER", OwnerID: userID, WalletID: walletID, Status: "ACTIVE", AllowedModels: []string{"gpt-test"}, HealthStatus: "HEALTHY", HealthExpiresAt: &expiresAt, DatabaseNow: now, CredentialID: uuid.Must(uuid.NewV7()), Priority: 100}
	context := AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID}
	if _, err := SelectRoute(context, RouteRequest{Model: "gpt-test"}, []Candidate{personal}); !errors.Is(err, ErrNoEligibleRoute) {
		t.Fatalf("organization context fell back to personal token: %v", err)
	}
}

func TestRoutingScopesAndAmbiguity(t *testing.T) {
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	userID, organizationID, unitID, walletID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	context := AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID, UnitIDs: []uuid.UUID{unitID}}
	candidate := func(priority int) Candidate {
		return Candidate{TokenAccountID: uuid.Must(uuid.NewV7()), OwnerType: "ORG_UNIT", OwnerID: unitID, OwnerOrganization: &organizationID, OwnerUnit: &unitID, WalletID: walletID, Status: "ACTIVE", ShareScope: "OWNER_UNIT_ONLY", AllowedModels: []string{"gpt-test"}, DefaultScope: "UNIT", Priority: priority, HealthStatus: "HEALTHY", HealthExpiresAt: &expiresAt, DatabaseNow: now, CredentialID: uuid.Must(uuid.NewV7())}
	}
	high, low := candidate(200), candidate(100)
	selected, err := SelectRoute(context, RouteRequest{Model: "gpt-test"}, []Candidate{low, high})
	if err != nil || selected.TokenAccountID != high.TokenAccountID {
		t.Fatalf("unexpected route: %#v, %v", selected, err)
	}
	tie := candidate(200)
	if _, err := SelectRoute(context, RouteRequest{Model: "gpt-test"}, []Candidate{high, tie}); !errors.Is(err, ErrAmbiguousRoute) {
		t.Fatalf("equal priority route was not rejected: %v", err)
	}
}

func TestExplicitMemberScope(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Minute)
	userID, otherID, organizationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	candidate := Candidate{OwnerType: "ORG", OwnerOrganization: &organizationID, WalletID: uuid.Must(uuid.NewV7()), Status: "ACTIVE", ShareScope: "EXPLICIT_MEMBERS", ExplicitMembers: []uuid.UUID{userID}, AllowedModels: []string{"gpt-test"}, HealthStatus: "HEALTHY", HealthExpiresAt: &expiresAt, DatabaseNow: time.Now().UTC(), CredentialID: uuid.Must(uuid.NewV7())}
	if !Eligible(AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID}, candidate, "gpt-test") {
		t.Fatal("explicit member was rejected")
	}
	if Eligible(AuthorizationContext{UserID: otherID, ContextType: "ORGANIZATION", OrganizationID: &organizationID}, candidate, "gpt-test") {
		t.Fatal("non-member was authorized")
	}
}

func TestSelectRouteRequiresDeclaredProtocol(t *testing.T) {
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	userID := uuid.Must(uuid.NewV7())
	candidate := Candidate{TokenAccountID: uuid.Must(uuid.NewV7()), OwnerType: "USER", OwnerID: userID, WalletID: uuid.Must(uuid.NewV7()), CredentialID: uuid.Must(uuid.NewV7()), Status: "ACTIVE", HealthStatus: "HEALTHY", HealthExpiresAt: &expiresAt, DatabaseNow: now, AllowedModels: []string{"gpt-test"}, ProtocolCode: "CHAT"}
	context := AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}
	if _, err := SelectRoute(context, RouteRequest{Model: "gpt-test", ProtocolCode: "EMBEDDING"}, []Candidate{candidate}); !errors.Is(err, ErrNoEligibleRoute) {
		t.Fatalf("undeclared protocol should be unavailable: %v", err)
	}
}

func TestSelectRouteRejectsExpiredOrMissingHealthEvidence(t *testing.T) {
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	userID := uuid.Must(uuid.NewV7())
	base := Candidate{TokenAccountID: uuid.Must(uuid.NewV7()), OwnerType: "USER", OwnerID: userID, WalletID: uuid.Must(uuid.NewV7()), CredentialID: uuid.Must(uuid.NewV7()), Status: "ACTIVE", HealthStatus: "HEALTHY", DatabaseNow: now, AllowedModels: []string{"gpt-test"}, ProtocolCode: "CHAT"}
	context := AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}
	for _, expiresAt := range []*time.Time{nil, func() *time.Time { value := now; return &value }()} {
		candidate := base
		candidate.HealthExpiresAt = expiresAt
		if _, err := SelectRoute(context, RouteRequest{Model: "gpt-test", ProtocolCode: "CHAT"}, []Candidate{candidate}); !errors.Is(err, ErrNoEligibleRoute) {
			t.Fatalf("expired or missing evidence should be unavailable: %v", err)
		}
	}
}
