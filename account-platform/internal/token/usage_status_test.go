package token

import (
	"testing"

	"github.com/google/uuid"
)

func TestUsageDisplayStatusKeepsSettlementPendingDistinct(t *testing.T) {
	cases := map[string]string{
		"CREATED": "IN_PROGRESS", "RESERVED": "IN_PROGRESS", "UPSTREAM_ACTIVE": "IN_PROGRESS",
		"COMMITTED": "SETTLED", "SETTLEMENT_PENDING": "SETTLEMENT_PENDING", "RELEASED": "RELEASED", "REVIEW": "MANUAL_REVIEW",
	}
	for status, expected := range cases {
		if actual := usageDisplayStatus(UsageRequest{Status: status}); actual != expected {
			t.Fatalf("usageDisplayStatus(%q) = %q, want %q", status, actual, expected)
		}
	}
}

func TestUsageStatusRequiresSameSessionAndActiveContext(t *testing.T) {
	userID := uuid.New()
	organizationID := uuid.New()
	usage := UsageRequest{UserID: userID, SessionID: "session", OrganizationID: &organizationID}
	context := AuthorizationContext{UserID: userID, SessionID: "session", ContextType: "ORGANIZATION", OrganizationID: &organizationID}
	if !usageContextMatches(context, usage) {
		t.Fatal("same session and organization should be authorized")
	}
	personal := context
	personal.ContextType, personal.OrganizationID = "PERSONAL", nil
	if usageContextMatches(personal, usage) {
		t.Fatal("organization usage leaked into personal context")
	}
	otherSession := context
	otherSession.SessionID = "other"
	if usageContextMatches(otherSession, usage) {
		t.Fatal("usage leaked across sessions")
	}
}
