package token

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationLifecycleConflictHTTPError(t *testing.T) {
	for _, failure := range []error{ErrOrganizationLifecycleConflict, fmt.Errorf("private implementation detail: %w", ErrOrganizationLifecycleConflict)} {
		response := httptest.NewRecorder()
		(&Service{}).writeError(response, httptest.NewRequest(http.MethodPost, "/internal/organizations/controlled/lifecycle", nil), failure)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ORGANIZATION_LIFECYCLE_CONFLICT"`) || strings.Contains(response.Body.String(), "private implementation") {
			t.Fatal("domain conflict lost its safe HTTP identity", response.Code, response.Body.String())
		}
	}
}
