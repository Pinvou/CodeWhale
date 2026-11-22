package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestApprovalRecoveryPlanClientRequiresClosedFreshNoStoreResponse(t *testing.T) {
	base := approvalRecoveryPlanView{
		SchemaVersion: 1, ObservedAt: time.Now().UTC(), Candidates: []ApprovalRecoveryCandidate{},
		Control: approvalRecoveryControlView{
			ControlID: adminApprovalControlID, SchemaVersion: 1, Mode: AdminApprovalModeLegacy,
			ApprovalWireVersion: 1, AuthorizationRelayState: AdminApprovalRelayRunning,
			ApprovalEpoch: 1, AuthorizationRelayEpoch: 1, Version: 1, UpdatedAt: time.Now().UTC(),
		},
	}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, cache string
		status            int
		accept            bool
	}{
		{"valid", string(valid), "no-store", http.StatusOK, true},
		{"cacheable", string(valid), "public", http.StatusOK, false},
		{"redirect", string(valid), "no-store", http.StatusFound, false},
		{"unknown field", string(valid[:len(valid)-1]) + `,"secret":"x"}`, "no-store", http.StatusOK, false},
		{"wrong version", string(valid[:len(valid)-1]) + `,"schema_version":2}`, "no-store", http.StatusOK, false},
		{"trailing JSON", string(valid) + `{}`, "no-store", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/approval-recovery/plan" || r.URL.Query().Get("limit") != "2" ||
					r.Header.Get("Authorization") != "Bearer dedicated" {
					t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", tc.cache)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewApprovalRecoveryPlanClient(server.URL, "dedicated")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := client.PlanProofApprovalRecovery(context.Background(), 2)
			if tc.accept && (err != nil || plan.Control.ControlID != adminApprovalControlID || len(plan.Candidates) != 0) {
				t.Fatalf("valid plan rejected: %+v %v", plan, err)
			}
			if !tc.accept && !errors.Is(err, ErrApprovalRecoveryPlanUnavailable) {
				t.Fatalf("invalid plan accepted: %+v %v", plan, err)
			}
		})
	}
}
