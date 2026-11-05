package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObservedHTTPClientDoesNotForwardSecretsAcrossRedirect(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer redirect.Close()
	req, _ := http.NewRequest(http.MethodPost, redirect.URL, strings.NewReader("private body"))
	req.Header.Set("Authorization", "Bearer fixture-only")
	response, err := NewHTTPClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 307 || forwarded {
		t.Fatalf("status=%d forwarded=%v", response.StatusCode, forwarded)
	}
}
