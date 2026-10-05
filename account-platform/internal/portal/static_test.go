package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticHandlerServesBuiltPortalAtRoot(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()

	StaticHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected built portal at root, got HTTP %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `id="account-root"`) {
		t.Fatal("expected the built account portal document")
	}
}

func TestStaticHandlerServesStableAssetsWithRevalidation(t *testing.T) {
	handler := testService(t, newMemoryStore()).Handler(StaticHandler())
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("stable portal HTML did not retain revalidation")
	}
	for _, assetURL := range []string{"/assets/account.js", "/assets/account.css"} {
		if !strings.Contains(recorder.Body.String(), `"`+assetURL+`"`) {
			t.Fatal("canonical portal omitted its stable account asset")
		}
		assetRequest := httptest.NewRequest(http.MethodGet, assetURL, nil)
		assetRecorder := httptest.NewRecorder()
		handler.ServeHTTP(assetRecorder, assetRequest)
		if assetRecorder.Code != http.StatusOK || assetRecorder.Body.Len() == 0 || assetRecorder.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("stable asset was missing or cached without revalidation")
		}
	}
}
