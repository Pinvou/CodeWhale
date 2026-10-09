package token

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWritePricingErrorPreservesBillingFailureClass(t *testing.T) {
	tests := []struct {
		name       string
		errorCode  string
		wantStatus int
		wantBody   string
	}{
		{name: "missing", errorCode: "PRICE_MISSING", wantStatus: http.StatusUnprocessableEntity, wantBody: `"code":"PRICING_MISSING"`},
		{name: "ambiguous", errorCode: "PRICE_AMBIGUOUS", wantStatus: http.StatusConflict, wantBody: `"code":"PRICING_AMBIGUOUS"`},
		{name: "blocked", errorCode: "PRICE_UNAVAILABLE", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"PRICING_UNAVAILABLE"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			response := httptest.NewRecorder()
			new(Service).writePricingError(response, request, &BillingHTTPError{Status: http.StatusUnprocessableEntity, Code: test.errorCode})
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if body := response.Body.String(); !strings.Contains(body, test.wantBody) {
				t.Fatalf("body = %s, want %s", body, test.wantBody)
			}
		})
	}
}
