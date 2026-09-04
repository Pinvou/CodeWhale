package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHTTPApprovalSnapshotSourceRequiresClosedMatchingProjection(t *testing.T) {
	modelID := uuid.New()
	fingerprint := strings.Repeat("a", 64)
	critical := map[string]any{
		"model_id": modelID, "catalog_version": 7, "current_lifecycle": "PUBLISHED", "target_lifecycle": "DEPRECATED", "lifecycle_plan_version": 0,
		"effective_at": "2026-09-04T00:00:00Z", "existing_session_cutoff_at": nil, "replacement_model_id": nil, "billing_price_version": 11,
		"route_set_fingerprint": fingerprint, "provisioning_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "policy_version": 2,
	}
	financial := map[string]any{"billing_price_revision": 11, "price_set_fingerprint": fingerprint, "models_checked": 1, "models_covered": 1, "fully_covered": true, "billing_as_of": "2026-09-04T00:00:00Z"}
	requestInput := ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), NormalizedCommand: json.RawMessage(`{"model_id":"` + modelID.String() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null}`)}
	for name, mutate := range map[string]func(map[string]any){
		"valid":        func(map[string]any) {},
		"extra":        func(value map[string]any) { value["secret"] = "must-not-pass" },
		"wrong target": func(value map[string]any) { value["target_resource_id"] = uuid.NewString() },
		"drifted revision": func(value map[string]any) {
			value["financial_snapshot"].(map[string]any)["billing_price_revision"] = 12
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{
				"command_type": requestInput.CommandType, "target_resource_type": requestInput.TargetResourceType, "target_resource_id": requestInput.TargetResourceID,
				"critical_resource_versions": cloneMap(critical), "financial_snapshot": cloneMap(financial), "precheck_fingerprint": fingerprint,
				"token_observed_at": "2026-09-04T00:00:00Z", "billing_observed_at": "2026-09-04T00:00:00Z",
			}
			mutate(body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") == "" {
					t.Fatalf("request path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, err := NewHTTPApprovalSnapshotSource(server.URL, "service-token")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			projection, err := client.PrecheckApprovalSnapshot(ctx, requestInput)
			if name == "valid" {
				if err != nil || !sameJSONObject(projection.CriticalResourceVersions, mustJSON(critical)) || !sameJSONObject(projection.FinancialSnapshot, mustJSON(financial)) {
					t.Fatalf("valid projection=%+v err=%v", projection, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe projection accepted: %+v", projection)
			}
		})
	}
}

func TestHTTPApprovalSnapshotSourceClassifiesTransientAndRejectedFailures(t *testing.T) {
	modelID := uuid.New()
	input := ApprovalSnapshotRequest{
		CommandType:        "TOKEN_MODEL_LIFECYCLE_CHANGE",
		TargetResourceType: "MODEL",
		TargetResourceID:   modelID.String(),
		NormalizedCommand:  json.RawMessage(`{"model_id":"` + modelID.String() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null}`),
	}
	for _, testCase := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "gateway timeout", status: http.StatusGatewayTimeout, want: ErrApprovalSnapshotTimeout},
		{name: "dependency unavailable", status: http.StatusServiceUnavailable, want: ErrApprovalSnapshotUnavailable},
		{name: "authoritative rejection", status: http.StatusConflict, want: ErrApprovalSnapshotRejected},
		{name: "malformed success", status: http.StatusOK, want: ErrApprovalSnapshotUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"code":"stable"}`))
			}))
			defer server.Close()
			client, err := NewHTTPApprovalSnapshotSource(server.URL, "service-token")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.PrecheckApprovalSnapshot(context.Background(), input)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("status=%d err=%v want=%v", testCase.status, err, testCase.want)
			}
		})
	}

	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-blocked }))
	defer func() {
		close(blocked)
		server.Close()
	}()
	client, err := NewHTTPApprovalSnapshotSource(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.PrecheckApprovalSnapshot(ctx, input); !errors.Is(err, ErrApprovalSnapshotTimeout) {
		t.Fatalf("deadline err=%v", err)
	}
}

func cloneMap(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
