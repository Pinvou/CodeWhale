package billing

import (
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectPaymentRepairRequiresCommandIdentity(t *testing.T) {
	s := NewService(&PostgresRepository{})
	for _, f := range []struct{ name, body, key, code string }{
		{"legacy", `{"reason":"controlled repair reason"}`, "controlled-stable-repair-key", "COMMAND_PROTOCOL_REQUIRED"},
		{"unsupported", `{"reason":"controlled repair reason","command_protocol":2}`, "controlled-stable-repair-key", "COMMAND_PROTOCOL_REQUIRED"},
		{"missing-key", `{"reason":"controlled repair reason","command_protocol":1}`, "", "IDEMPOTENCY_KEY_REQUIRED"},
		{"short-key", `{"reason":"controlled repair reason","command_protocol":1}`, "short", "IDEMPOTENCY_KEY_REQUIRED"},
	} {
		t.Run(f.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/controlled", strings.NewReader(f.body))
			request.SetPathValue("order_id", uuid.NewString())
			request.Header.Set("Idempotency-Key", f.key)
			response := httptest.NewRecorder()
			s.adminRepairPaymentOrder(response, request)
			if response.Code != 400 || !strings.Contains(response.Body.String(), f.code) {
				t.Fatal("unversioned repair reached coordinator", response.Code, response.Body.String())
			}
		})
	}
}
