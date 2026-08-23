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

func TestStaticHandlerServesRegistrationRepairConsole(t *testing.T) {
	page := httptest.NewRecorder()
	StaticHandler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin.html", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="registration-repair-root"`) {
		t.Fatal("expected registration repair root in the admin portal")
	}
	for _, asset := range []string{"/assets/registration-repair.js", "/assets/registration-repair-i18n.js", "/assets/registration-repair.css"} {
		response := httptest.NewRecorder()
		StaticHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("expected embedded repair asset %q", asset)
		}
	}
}
