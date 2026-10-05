package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalGateRequiresExactAccountSuccessStatus(t *testing.T) {
	for _, operation := range []string{"begin", "complete"} {
		for _, status := range []int{200, 201, 202, 204} {
			t.Run(operation+"/"+strconv.Itoa(status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					if status != http.StatusNoContent {
						_, _ = w.Write([]byte(`{}`))
					}
				}))
				defer server.Close()
				gate, err := NewHTTPApprovalExecutionGate(server.URL, "synthetic-service-token")
				if err != nil {
					t.Fatal(err)
				}
				var valid bool
				if operation == "begin" {
					_, err = gate.Begin(context.Background(), uuid.New(), uuid.NewString(), json.RawMessage(`{"adjustment_version":1}`))
					valid = status == http.StatusOK
				} else {
					err = gate.Complete(context.Background(), uuid.New(), uuid.NewString(), "EXECUTED", json.RawMessage(`{}`), json.RawMessage(`[]`), "")
					valid = status == http.StatusNoContent
				}
				if (err == nil) != valid {
					t.Fatalf("Account success protocol violated: operation=%s status=%d accepted=%t", operation, status, err == nil)
				}
			})
		}
	}
}
