package billing

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostgresPricedHoldReplayBindsImmutableFacts(t *testing.T) {
	for _, member := range []bool{false, true} {
		name := "ordinary"
		if member {
			name = "member"
		}
		t.Run(name, func(t *testing.T) {
			r, w, _ := forcedRefundPaidSource(t)
			ctx := context.Background()
			user := uuid.New()
			if member {
				if _, e := r.ApplyOrganizationMemberLifecycle(ctx, w.OwnerID, user, "ACTIVE", 1); e != nil {
					t.Fatal("controlled member authority setup failed")
				}
			}
			secret := strings.Repeat("x", 32)
			handler := NewService(r).Handler(secret)
			usage := uuid.New()
			formula := uuid.New()
			base := map[string]any{"usage_request_id": usage, "wallet_id": w.ID, "maximum_microcredits": 0, "lease_seconds": 900, "model_id": "controlled-model", "protocol_code": "CHAT", "formula_version_id": formula, "pricing_snapshot": map[string]any{"controlled_rate": 0}, "request_snapshot": map[string]any{"controlled_input": 1}}
			if member {
				base["organization_id"] = w.OwnerID
				base["member_user_id"] = user
				base["membership_version"] = 1
			}
			invoke := func(b map[string]any) int {
				encoded, e := json.Marshal(b)
				if e != nil {
					t.Fatal(e)
				}
				req := httptest.NewRequest(http.MethodPost, "/internal/v1/holds/reserve", strings.NewReader(string(encoded)))
				req.Header.Set("Authorization", "Bearer "+secret)
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				return res.Code
			}
			if invoke(base) != 200 || invoke(base) != 200 {
				t.Fatal("legitimate initial or immutable replay rejected")
			}
			equivalent := map[string]any{}
			for k, v := range base {
				equivalent[k] = v
			}
			equivalent["request_snapshot"] = json.RawMessage(`{"controlled_input":1e0}`)
			if invoke(equivalent) != 200 {
				t.Fatal("JSON-equivalent numeric snapshot replay rejected")
			}
			for _, kind := range []string{"model", "protocol", "formula", "pricing", "request", "unpriced"} {
				t.Run(kind, func(t *testing.T) {
					b := map[string]any{}
					for k, v := range base {
						b[k] = v
					}
					switch kind {
					case "model":
						b["model_id"] = "controlled-other-model"
					case "protocol":
						b["protocol_code"] = "CONTROLLED_OTHER"
					case "formula":
						b["formula_version_id"] = uuid.New()
					case "pricing":
						b["pricing_snapshot"] = map[string]any{"controlled_rate": 1}
					case "request":
						b["request_snapshot"] = map[string]any{"controlled_input": 2}
					case "unpriced":
						b["maximum_microcredits"] = 0
						for _, k := range []string{"model_id", "protocol_code", "formula_version_id", "pricing_snapshot", "request_snapshot"} {
							delete(b, k)
						}
					}
					status := invoke(b)
					if status != 409 {
						t.Errorf("different newly persisted pricing facts accepted same usage receipt: kind=%s status=%d", kind, status)
					}
				})
			}
			var rows int
			if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_holds WHERE usage_request_id=$1`, usage).Scan(&rows); e != nil || rows != 1 {
				t.Fatal("hold duplication or query failure")
			}
		})
	}
}
