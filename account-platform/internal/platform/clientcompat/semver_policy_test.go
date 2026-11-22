package clientcompat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func semverPolicyReleases(versions [3]string) []Release {
	return []Release{
		{Version: versions[0], ReleasedAt: "2026-08-01T00:00:00Z"},
		{Version: versions[1], ReleasedAt: "2026-07-01T00:00:00Z", DeprecationNoticeAt: "2026-08-31T00:00:00Z", SunsetAt: "2026-11-30T00:00:00Z"},
		{Version: versions[2], ReleasedAt: "2026-06-01T00:00:00Z", DeprecationNoticeAt: "2026-08-31T00:00:00Z", SunsetAt: "2026-11-30T00:00:00Z"},
	}
}

func TestSemVerPolicyAcceptsFullPrecedence(t *testing.T) {
	for _, versions := range [][3]string{
		{"1.2.3+stable.001", "1.2.3-rc.1+ci.009", "1.2.3-beta.11"},
		{"1.2.3-beta.11", "1.2.3-beta.2", "1.2.3-alpha"},
		{"999999999999999999999999999999999994.2.1", "999999999999999999999999999999999993.2.1", "999999999999999999999999999999999992.2.1"},
	} {
		t.Run(versions[0], func(t *testing.T) {
			policy, err := NewPolicy("v1", "https://updates.example.invalid/latest", semverPolicyReleases(versions))
			if err != nil {
				t.Fatal("valid descending SemVer releases rejected", err)
			}
			if policy.Releases[0].Version != versions[0] || policy.Releases[1].Version != versions[1] {
				t.Fatal("release identity was normalized instead of retaining original strings")
			}
		})
	}
}

func TestSemVerPolicyRejectsInvalidVersionsAndEqualBuildPrecedence(t *testing.T) {
	for _, invalid := range []string{"1.2.3-01", "1.2.3-alpha..1", "1.2.3+build..1", "1.2.3-", "01.2.3", "1.2.3 ", "1.2.3-α"} {
		t.Run(invalid, func(t *testing.T) {
			if _, err := NewPolicy("v1", "https://updates.example.invalid/latest", semverPolicyReleases([3]string{invalid, "1.2.2", "1.2.1"})); err == nil {
				t.Fatal("invalid release version accepted")
			}
		})
	}
	if _, err := NewPolicy("v1", "https://updates.example.invalid/latest", semverPolicyReleases([3]string{"1.2.3+one", "1.2.3+two", "1.2.2"})); err == nil {
		t.Fatal("build metadata created an artificial precedence between releases")
	}
}

func TestSemVerClientWriteFloorAndExistingExceptions(t *testing.T) {
	policy := testPolicy(t)
	for _, sample := range []struct {
		name, method, path, version, auth string
		want                              int
	}{
		{"RC-below-stable-floor", "POST", "/v1/chat/completions", "3.0.0-rc.1", "", 426},
		{"stable-build-equal-floor", "POST", "/v1/chat/completions", "3.0.0+build.001", "", 204},
		{"higher-prerelease-allowed", "POST", "/v1/chat/completions", "3.1.0-alpha.1+ci.7", "", 204},
		{"numeric-leading-zero-invalid", "POST", "/v1/chat/completions", "999.1.1-01", "", 426},
		{"empty-identifier-invalid", "POST", "/v1/chat/completions", "999.1.1+build..1", "", 426},
		{"nonASCII-invalid", "POST", "/v1/chat/completions", "999.1.1-α", "", 426},
		{"whitespace-invalid", "POST", "/v1/chat/completions", " 3.2.0", "", 426},
		{"unversioned-authenticated-write", "POST", "/v1/chat/completions", "", "Bearer controlled", 426},
		{"unversioned-unauthed-existing-behavior", "POST", "/v1/chat/completions", "", "", 204},
		{"GET-still-readable", "GET", "/v1/models", "999.1.1-01", "", 204},
		{"HEAD-still-readable", "HEAD", "/v1/models", "invalid", "", 204},
		{"OPTIONS-still-readable", "OPTIONS", "/v1/models", "invalid", "", 204},
		{"internal-write-exempt", "POST", "/internal/v1/proof", "invalid", "Bearer controlled", 204},
		{"health-exempt", "POST", "/health/ready", "invalid", "Bearer controlled", 204},
		{"metrics-exempt", "POST", "/metrics", "invalid", "Bearer controlled", 204},
	} {
		t.Run(sample.name, func(t *testing.T) {
			called := false
			handler := policy.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) }))
			request := httptest.NewRequest(sample.method, sample.path, nil)
			request.Header.Set(ClientVersionHeader, sample.version)
			request.Header.Set("Authorization", sample.auth)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			if out.Code != sample.want || called != (sample.want == 204) {
				t.Fatalf("version policy status=%d called=%t want=%d", out.Code, called, sample.want)
			}
			if sample.want == 426 {
				var receipt apiError
				if json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt.Code != "CLIENT_UPGRADE_REQUIRED" || out.Header().Get("Link") == "" {
					t.Fatal("existing upgrade receipt/code/link missing")
				}
			}
		})
	}
}

func TestSemVerReleaseLookupKeepsExactIdentity(t *testing.T) {
	p := testPolicy(t).WithNow(func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) })
	if _, ok := p.release("3.0.0"); !ok {
		t.Fatal("existing exact release lookup lost")
	}
	for _, different := range []string{"3.0.0+build", "3.0.0-rc.1"} {
		if _, ok := p.release(different); ok {
			t.Fatal("precedence was substituted for exact release identity")
		}
	}
}
