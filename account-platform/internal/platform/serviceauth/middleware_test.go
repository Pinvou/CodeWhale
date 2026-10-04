package serviceauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareRequiresExactToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, test := range []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer correct-token", http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", test.header)
		response := httptest.NewRecorder()
		Middleware("correct-token", next).ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("header %q: status = %d, want %d", test.header, response.Code, test.want)
		}
	}
}
