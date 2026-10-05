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

func TestStaticHandlerServesRegistrationRepairConsole(t *testing.T) {
	handler := testService(t, newMemoryStore()).Handler(StaticHandler())
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin.html", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="admin-root"`) {
		t.Fatal("expected the modern admin application root")
	}
	for _, asset := range []string{"/assets/admin.js", "/assets/admin.css"} {
		if !strings.Contains(page.Body.String(), `"`+asset+`"`) {
			t.Fatal("admin HTML omitted its canonical asset")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 || response.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("expected revalidated embedded admin asset")
		}
		if strings.HasSuffix(asset, ".js") && !strings.Contains(response.Body.String(), "registration-operations") {
			t.Fatal("built admin entry omitted the registration repair API")
		}
	}
	for _, retired := range []string{"/assets/registration-repair.js", "/assets/registration-repair-i18n.js", "/assets/registration-repair.css"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, retired, nil))
		if response.Code != http.StatusNotFound {
			t.Fatal("retired parallel repair implementation was revived")
		}
	}
}
