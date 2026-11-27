package portal

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func sharedAssetsFixture(t *testing.T) (directory string, filenames []string) {
	t.Helper()
	directory = t.TempDir()
	assets := filepath.Join(directory, "assets")
	if err := os.Mkdir(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(staticFiles, "static/assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		content, err := staticFiles.ReadFile("static/assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(assets, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
		filenames = append(filenames, name)
	}
	return directory, filenames
}

func TestPortalSharedAssetsPreserveCurrentAndRetainedPathsOverHTTP(t *testing.T) {
	directory, filenames := sharedAssetsFixture(t)
	const retained = "retained-abcdef12.js"
	if err := os.WriteFile(filepath.Join(directory, "assets", retained), []byte("previous chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", ".portal-unfinished"), []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	static, volume, err := OpenSharedStaticHandler(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	service, store := newSessionHandoffTestService(t, "")
	service.config.SecureCookies = true
	session := validSession("shared-assets-session")
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler(static))
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	do := func(method, path string, headers ...[2]string) (int, http.Header, string) {
		t.Helper()
		request, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, header := range headers {
			request.Header.Set(header[0], header[1])
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, string(body)
	}
	for _, name := range append(filenames, retained) {
		want, err := os.ReadFile(filepath.Join(directory, "assets", name))
		if err != nil {
			t.Fatal(err)
		}
		status, header, body := do(http.MethodGet, "/assets/"+name)
		if status != http.StatusOK || body != string(want) {
			t.Fatalf("asset %s returned status %d with mismatched bytes", name, status)
		}
		if header.Get("Cache-Control") != "no-cache" ||
			header.Get("Cross-Origin-Resource-Policy") != "same-origin" ||
			header.Get("Strict-Transport-Security") != "max-age=31536000; includeSubDomains" ||
			header.Get("Last-Modified") != "" ||
			header.Get("Set-Cookie") != "" {
			t.Fatalf("asset %s changed Portal response policy", name)
		}
		if status, _, body := do(http.MethodHead, "/assets/"+name); status != http.StatusOK || body != "" {
			t.Fatalf("HEAD for %s returned %d with %d bytes", name, status, len(body))
		}
	}
	if status, _, _ := do(http.MethodGet, "/assets/"+retained,
		[2]string{"If-Modified-Since", "Tue, 01 Jan 2030 00:00:00 GMT"}); status != http.StatusOK {
		t.Fatalf("date conditional shared asset returned %d, embedded assets have no date validator", status)
	}
	for _, name := range []string{"absent-abcdef12.js", ".portal-unfinished", "../admin.html", ""} {
		if status, headers, _ := do(http.MethodGet, "/assets/"+name); status != http.StatusNotFound || headers.Get("Cache-Control") != "no-cache" {
			t.Fatalf("unexpected asset response for %q: %d %v", name, status, headers)
		}
	}
	if status, headers, _ := do(http.MethodPost, "/assets/"+retained); status != http.StatusMethodNotAllowed || headers.Get("Allow") != "GET, HEAD" {
		t.Fatalf("shared asset mutation response: %d %v", status, headers)
	}
	sessionHeader := [2]string{"Cookie", service.sessionCookieName() + "=" + session.ID}
	assetReferences := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`)
	for _, path := range []string{"/", "/admin.html", "/portal-session-handoff"} {
		status, _, body := do(http.MethodGet, path, sessionHeader)
		references := assetReferences.FindAllStringSubmatch(body, -1)
		if status != http.StatusOK || len(references) == 0 {
			t.Fatalf("embedded entry %q returned %d", path, status)
		}
		for _, reference := range references {
			if status, _, _ := do(http.MethodGet, reference[1], sessionHeader); status != http.StatusOK {
				t.Fatalf("asset %q from %s returned %d", reference[1], path, status)
			}
		}
	}
	if status, headers, _ := do(http.MethodGet, "/auth/login"); status != http.StatusFound ||
		headers.Get("Location") == "" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected login contract: %d", status)
	}
	if status, headers, body := do(http.MethodGet, "/api/session", sessionHeader); status != http.StatusOK ||
		headers.Get("Cache-Control") != "no-store" || !strings.Contains(body, `"authenticated":true`) {
		t.Fatalf("unexpected session API contract: %d", status)
	}
}

func TestPortalSharedAssetsRejectIncompleteOrMismatchedRelease(t *testing.T) {
	directory, filenames := sharedAssetsFixture(t)
	name := filepath.Join(directory, "assets", filenames[0])
	stat, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, make([]byte, stat.Size()), 0o644); err != nil {
		t.Fatal(err)
	}
	if handler, volume, err := OpenSharedStaticHandler(directory); err == nil || handler != nil || volume != nil {
		t.Fatal("same-size wrong asset did not block Portal startup")
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if handler, volume, err := OpenSharedStaticHandler(directory); err == nil || handler != nil || volume != nil {
		t.Fatal("missing asset did not block Portal startup")
	}
}

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
