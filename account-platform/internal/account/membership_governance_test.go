package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestInvitationTargetsNormalizeWithoutLookup(t *testing.T) {
	tests := []struct {
		targetType string
		value      string
		wantType   string
	}{
		{"phone", "13800138000", "PHONE"},
		{"username", " Example_User ", "USERNAME"},
		{"link", "", "LINK"},
	}
	for _, test := range tests {
		targetType, hint, digest, err := normalizeInvitationTarget(test.targetType, test.value)
		if err != nil || targetType != test.wantType {
			t.Fatalf("normalize %s: type=%s err=%v", test.targetType, targetType, err)
		}
		if targetType == "LINK" && (hint != "" || digest != nil) {
			t.Fatal("link invitations must not persist a target")
		}
		if targetType != "LINK" && (hint == "" || len(digest) != 32) {
			t.Fatalf("target %s must persist only a hint and digest", targetType)
		}
	}
}

func TestMembershipCommandHashIsCanonicalAndTargetBound(t *testing.T) {
	target := uuid.Must(uuid.NewV7())
	unitA, unitB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	left := membershipRequestHash("ADD", target, []string{"MEMBER", "AUDITOR"}, []uuid.UUID{unitA, unitB})
	right := membershipRequestHash("ADD", target, []string{"AUDITOR", "MEMBER"}, []uuid.UUID{unitB, unitA})
	if left != right {
		t.Fatal("equivalent membership commands must have one canonical fingerprint")
	}
	if left == membershipRequestHash("ADD", uuid.Must(uuid.NewV7()), []string{"AUDITOR", "MEMBER"}, []uuid.UUID{unitB, unitA}) {
		t.Fatal("membership command fingerprints must bind the target user")
	}
}

func TestOrganizationMemberLifecycleClientValidatesReturnedTarget(t *testing.T) {
	organizationID, userID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/organizations/"+organizationID.String()+"/members/"+userID.String()+"/lifecycle" || r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatalf("unexpected lifecycle request %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organization_id":"` + organizationID.String() + `","user_id":"` + userID.String() + `","status":"REVOKED","membership_version":3}`))
	}))
	defer server.Close()
	client, err := NewHTTPOrganizationLifecycleClient(server.URL, "service-token", "BILLING")
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.ApplyMemberLifecycle(context.Background(), organizationID, userID, "REVOKED", 3)
	if err != nil || state.Status != "REVOKED" {
		t.Fatalf("apply member lifecycle: %#v %v", state, err)
	}
	if strings.TrimSpace(state.Status) == "" {
		t.Fatal("lifecycle status is required")
	}
}
