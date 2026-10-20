package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditExportMalformedJSONIsDefiniteClientError(t *testing.T) {
	service := &Service{}
	for _, body := range []string{`{"filter":`, `{"unknown":1}`, `{} {}`} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/audit-exports", strings.NewReader(body))
		service.createAuditExport(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
			t.Fatalf("invalid request was not rejected: status=%d", response.Code)
		}
	}
}
