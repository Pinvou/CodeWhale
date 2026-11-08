package portal

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestPortalBuiltEntriesAndAssetsAreServed(t *testing.T) {
	handler := StaticHandler()
	for _, path := range []string{"/", "/admin.html", "/portal-session-handoff.html"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("entry returned status %d, type %q", response.Code, response.Header().Get("Content-Type"))
			}
			assets := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(response.Body.String(), -1)
			if len(assets) == 0 {
				t.Fatal("built entry has no asset references")
			}
			for _, asset := range assets {
				result := httptest.NewRecorder()
				handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, asset[1], nil))
				if result.Code != http.StatusOK || result.Body.Len() == 0 {
					t.Fatalf("built asset %q returned %d", asset[1], result.Code)
				}
			}
			head := httptest.NewRecorder()
			handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
			if head.Code != http.StatusOK || head.Body.Len() != 0 {
				t.Fatalf("HEAD returned %d with %d bytes", head.Code, head.Body.Len())
			}
		})
	}
	// A Portal release must not embed future feature artifacts or source trees.
	entries, err := fs.ReadDir(staticFiles, "static")
	if err != nil || len(entries) != 4 {
		t.Fatalf("unexpected embedded root: %v, %v", entries, err)
	}
}

func TestPortalStaticRejectsNonAssetPathsAndMethods(t *testing.T) {
	handler := StaticHandler()
	for _, path := range []string{"/account.html", "/assets/", "/assets/../account.html", "/source.go", "/future-feature.html"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%q returned %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("static POST accepted")
	}
}
