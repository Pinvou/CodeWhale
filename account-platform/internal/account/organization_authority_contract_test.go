package account

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationAuthorityContract(t *testing.T) {
	org, unit := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name     string
		unit     bool
		status   int
		ready    bool
		blocked  bool
		overflow bool
		reject   bool
	}{
		{"organization_complete_body", false, 200, true, false, true, true},
		{"unit_complete_body", true, 200, true, false, true, true},
		{"unit_explicit_not_ready_without_blocker", true, 200, false, false, false, true},
		{"organization_conflict_is_not_activation_receipt", false, 409, true, false, false, true},
		{"unit_real_blocker", true, 200, false, true, false, false},
		{"organization_real_conflict", false, 409, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := map[string]any{"organization_id": org, "status": "DELETED", "account_version": 3, "unit_id": unit, "structure_version": 4, "ready": tc.ready, "blockers": []map[string]string{}}
			if tc.blocked {
				facts["blockers"] = []map[string]string{{"code": "RESOURCE_ACTIVE", "resource_type": "fixture", "resource_id": org.String()}}
			}
			body, _ := json.Marshal(facts)
			if tc.overflow {
				body = append(body, []byte(strings.Repeat(" ", (1<<20)-len(body)))...)
				body = append(body, 'X')
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); _, _ = w.Write(body) }))
			defer server.Close()
			c, err := NewHTTPOrganizationLifecycleClient(server.URL, "PUBLIC_CONTROLLED_TOKEN", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			if tc.unit {
				got, e := c.ApplyUnitLifecycle(context.Background(), org, unit, "DELETED", 4)
				err = e
				if err == nil && !tc.reject && got.Ready != tc.ready {
					t.Fatal("explicit authority readiness changed")
				}
			} else {
				got, e := c.Apply(context.Background(), org, "DELETED", 3)
				err = e
				if err == nil && !tc.reject && got.Ready != tc.ready {
					t.Fatal("explicit authority readiness changed")
				}
			}
			if tc.reject && err == nil {
				t.Fatal("invalid lifecycle authority accepted as completion")
			}
			if !tc.reject && err != nil {
				t.Fatal("valid current authority rejected")
			}
		})
	}
}
