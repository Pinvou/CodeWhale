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

func TestStaticHandlerServesHashedAssets(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	StaticHandler().ServeHTTP(recorder, request)
	assetStart := strings.Index(recorder.Body.String(), "/assets/account-")
	if assetStart < 0 {
		t.Fatal("expected a hashed account asset reference")
	}
	assetEnd := strings.Index(recorder.Body.String()[assetStart:], `"`)
	if assetEnd < 0 {
		t.Fatal("expected the asset URL to be quoted")
	}
	assetURL := recorder.Body.String()[assetStart : assetStart+assetEnd]

	assetRequest := httptest.NewRequest(http.MethodGet, assetURL, nil)
	assetRecorder := httptest.NewRecorder()
	StaticHandler().ServeHTTP(assetRecorder, assetRequest)
	if assetRecorder.Code != http.StatusOK || assetRecorder.Body.Len() == 0 {
		t.Fatalf("expected embedded asset %q, got HTTP %d", assetURL, assetRecorder.Code)
	}
}
