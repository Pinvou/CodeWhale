package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A failed observation must never become an authoritative empty provider catalog.
// These use the production HTTP adapter and synchronizer; persistence is observed
// through the same repository boundary as the catalog synchronization tests.
func TestRetainedDiscoveryFactsPreserveLastCatalog(t *testing.T) {
	tests := []struct {
		name, body string
		truncated  bool
	}{
		{"missing", `{}`, false},
		{"null", `{"data":null}`, false},
		{"object", `{"data":{}}`, false},
		{"missing-id", `{"data":[{}]}`, false},
		{"blank-id", `{"data":[{"id":"  "}]}`, false},
		{"null-entry", `{"data":[null]}`, false},
		{"null-id", `{"data":[{"id":null}]}`, false},
		{"numeric-id", `{"data":[{"id":12}]}`, false},
		{"extra-object", `{"data":[]} {}`, false},
		{"trailing-garbage", `{"data":[]} x`, false},
		{"overflow", `{"data":[]}` + strings.Repeat(" ", 4<<20), false},
		{"incomplete-body", `{"data":[]}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("unexpected discovery transport")
				}
				if test.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)+1))
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			repository := &catalogPreviewRepository{version: 7, models: []CatalogModel{{ProviderID: "PROVIDER", UpstreamModelRef: "existing", SourceAvailable: true, LifecycleStatus: "PUBLISHED"}}}
			before := append([]CatalogModel(nil), repository.models...)
			synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider", server.URL, []byte("test-only"), 128000, 4096, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := synchronizer.Sync(context.Background()); err == nil {
				t.Error("invalid or incomplete provider facts were accepted")
			}
			if repository.syncCalls != 0 || !reflect.DeepEqual(repository.models, before) {
				t.Error("failed discovery reached catalog persistence")
			}
		})
	}
}

func TestRetainedDiscoveryValidEmptyAndDeduplicatedCatalog(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       []DiscoveredModel
	}{
		{"empty", `{"data":[]}`, nil},
		{"extensions-and-deduplication", `{"extension":true,"data":[{"id":" b ","extra":1},{"id":"a"},{"id":"b"}]}`, []DiscoveredModel{{UpstreamModelRef: "a", DisplayName: "a"}, {UpstreamModelRef: "b", DisplayName: "b"}}},
		{"exact-bound", `{"data":[]}` + strings.Repeat(" ", (4<<20)-len(`{"data":[]}`)), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			repository := &catalogPreviewRepository{version: 7}
			synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider", server.URL, []byte("test-only"), 128000, 4096, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			version, changed, err := synchronizer.Sync(context.Background())
			if err != nil || !changed || version != 8 || repository.syncCalls != 1 || !reflect.DeepEqual(repository.synchronized, test.want) {
				t.Fatalf("valid discovery did not retain canonical semantics: version=%d changed=%v writes=%d models=%v err=%v", version, changed, repository.syncCalls, repository.synchronized, err)
			}
		})
	}
}

func TestRetainedCredentialValidationCompleteFacts(t *testing.T) {
	for _, test := range []struct {
		name, body       string
		truncated, valid bool
	}{
		{"extra-object", `{"data":[{"id":"model"}]} {}`, false, false},
		{"trailing-garbage", `{"data":[{"id":"model"}]} x`, false, false},
		{"overflow", `{"data":[{"id":"model"}]}` + strings.Repeat(" ", 2<<20), false, false},
		{"incomplete-body", `{"data":[{"id":"model"}]}`, true, false},
		{"extensions", `{"extra":true,"data":[{"id":"model","extra":1}]}`, false, true},
		{"exact-bound", `{"data":[{"id":"model"}]}` + strings.Repeat(" ", (2<<20)-len(`{"data":[{"id":"model"}]}`)), false, true},
		{"requested-model-absent", `{"data":[]}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("unexpected validator request")
				}
				if test.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)+1))
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			policy, err := NewProviderURLPolicy("development", []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			validator := NewHTTPModelValidator(policy)
			err = validator.Validate(context.Background(), server.URL, []byte("test-only"), []string{"model"})
			if (err == nil) != test.valid {
				t.Fatalf("complete credential fact contract: valid=%v err=%v", test.valid, err)
			}
		})
	}
}
