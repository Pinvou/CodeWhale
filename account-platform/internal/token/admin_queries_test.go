package token

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

func signAdminQuerySource(request *http.Request, key []byte, source string) {
	request.Header.Set("X-Pinvou-Client-Source", source)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(request.Method + "\n" + request.URL.RequestURI() + "\n" + source))
	request.Header.Set("X-Pinvou-Client-Source-Signature", hex.EncodeToString(mac.Sum(nil)))
}

func TestAdminQueryRejectsClientSortUnknownAndDuplicateParameters(t *testing.T) {
	for _, values := range []url.Values{
		{"sort": {"name"}},
		{"order_by": {"id"}},
		{"unknown": {"value"}},
		{"q": {"one", "two"}},
	} {
		if _, err := parseAdminQuery(values, []string{"q"}); err != ErrAdminQueryInvalidFilter {
			t.Fatalf("values=%v err=%v", values, err)
		}
	}
}

func TestAdminQueryRetainsEmptyNewFilterPresenceAndRejectsMissingSnapshotMember(t *testing.T) {
	filter, err := parseAdminQuery(url.Values{"status": {""}}, []string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := filter.present["status"]; !present {
		t.Fatal("an explicitly empty new filter must not become an empty legacy query")
	}
	if _, err := orderedValues([]string{"expected"}, map[string]string{}); !errors.Is(err, ErrAdminSnapshotResourceChanged) {
		t.Fatalf("missing member returned %v", err)
	}
}

type deadlinePaginationRepository struct {
	page AdminPaginationPage
	wait bool
}

func (repository deadlinePaginationRepository) AdminPaginationPage(ctx context.Context, _ AdminPaginationRequest, _ AdminSnapshotMemberSource) (AdminPaginationPage, error) {
	if repository.wait {
		<-ctx.Done()
		return AdminPaginationPage{}, ctx.Err()
	}
	return repository.page, nil
}

func TestAdminQueryClassifiesRealSourceAndLoaderDeadlines(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		repository deadlinePaginationRepository
		loader     func(context.Context, []string) (any, error)
		status     int
		code       string
		retryAfter string
	}{
		{
			name: "first page source deadline", path: "/admin/v1/models",
			repository: deadlinePaginationRepository{wait: true},
			loader:     func(context.Context, []string) (any, error) { return nil, errors.New("loader must not run") },
			status:     http.StatusServiceUnavailable, code: "PAGINATION_SNAPSHOT_UNAVAILABLE", retryAfter: "5",
		},
		{
			name: "first page loader deadline", path: "/admin/v1/models",
			repository: deadlinePaginationRepository{page: AdminPaginationPage{ResourceIDs: []string{"model-1"}}},
			loader: func(ctx context.Context, _ []string) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			status: http.StatusServiceUnavailable, code: "PAGINATION_SNAPSHOT_UNAVAILABLE", retryAfter: "5",
		},
		{
			name: "continuation loader deadline", path: "/admin/v1/models?cursor=signed",
			repository: deadlinePaginationRepository{page: AdminPaginationPage{ResourceIDs: []string{"model-1"}}},
			loader: func(ctx context.Context, _ []string) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			status: http.StatusGatewayTimeout, code: "QUERY_TIMEOUT",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now().UTC()
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			request.Header.Set(requestdeadline.AbsoluteDeadlineHeader, now.Add(20*time.Millisecond).Format(time.RFC3339Nano))
			response := httptest.NewRecorder()
			handler := requestdeadline.TokenAdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, err := resolveAdminQueryPage(r.Context(), testCase.repository, AdminPaginationRequest{}, nil, testCase.loader)
				writeAdminQueryError(w, r, err)
			}), func() time.Time { return now })
			handler.ServeHTTP(response, request)
			if response.Code != testCase.status || !strings.Contains(response.Body.String(), testCase.code) || response.Header().Get("Retry-After") != testCase.retryAfter {
				t.Fatalf("status=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
			}
		})
	}
}

func TestAdminQueryNormalizesAndBindsOnlyResourceFilters(t *testing.T) {
	filter, err := parseAdminQuery(url.Values{"q": {" model "}, "status": {"ACTIVE"}, "cursor": {"signed"}, "limit": {"50"}}, []string{"q", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if filter.cursor() != "signed" || filter.paginationValues()["q"] != "model" || filter.paginationValues()["status"] != "ACTIVE" {
		t.Fatalf("unexpected normalized filter: %#v", filter)
	}
	if _, exists := filter.paginationValues()["cursor"]; exists {
		t.Fatal("cursor must not be folded into the filter digest")
	}
}

func TestEquivalentAdminFiltersShareOneCanonicalCursorDigest(t *testing.T) {
	modelA, _ := parseAdminQuery(url.Values{"q": {"  ＭＯＤＥＬ  "}, "lifecycle_status": {"published"}, "source_missing": {"TRUE"}}, []string{"q", "lifecycle_status", "source_missing"})
	modelB, _ := parseAdminQuery(url.Values{"q": {"model"}, "lifecycle_status": {"PUBLISHED"}, "source_missing": {"true"}}, []string{"q", "lifecycle_status", "source_missing"})
	if _, err := modelSnapshotSource(modelA); err != nil {
		t.Fatal(err)
	}
	if _, err := modelSnapshotSource(modelB); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(modelA.paginationValues(), modelB.paginationValues()) {
		t.Fatalf("equivalent model filters diverged: %#v %#v", modelA.values, modelB.values)
	}

	id := uuid.New()
	formulaA, _ := parseAdminQuery(url.Values{"protocol_definition_id": {strings.ToUpper(id.String())}, "published": {"TRUE"}, "created_from": {"2026-09-03T08:00:00+08:00"}}, []string{"protocol_definition_id", "published", "created_from"})
	formulaB, _ := parseAdminQuery(url.Values{"protocol_definition_id": {id.String()}, "published": {"true"}, "created_from": {"2026-09-03T00:00:00Z"}}, []string{"protocol_definition_id", "published", "created_from"})
	if _, err := formulaSnapshotSource(formulaA); err != nil {
		t.Fatal(err)
	}
	if _, err := formulaSnapshotSource(formulaB); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(formulaA.paginationValues(), formulaB.paginationValues()) {
		t.Fatalf("equivalent formula filters diverged: %#v %#v", formulaA.values, formulaB.values)
	}
}

func TestOwnerCandidateNormalizationUsesNFKCAndUnicodeCaseFold(t *testing.T) {
	if got, want := normalizeOwnerCandidateLabel("  ＰｉｎＶＯＵ  "), "pinvou"; got != want {
		t.Fatalf("normalized label=%q want=%q", got, want)
	}
}

func TestOwnerCandidateAuthorityReceivesOrganizationFilterAndServiceAuth(t *testing.T) {
	organizationID := uuid.New()
	candidateID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Fatal("missing service authorization")
		}
		if r.URL.Query().Get("actor_issuer") != "https://issuer.example" || r.URL.Query().Get("actor_subject") != "admin" || r.URL.Query().Get("owner_type") != "UNIT" || r.URL.Query().Get("organization_id") != organizationID.String() {
			t.Fatalf("unexpected authority query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": candidateID, "type": "UNIT", "display_label": "研发", "status": "ACTIVE"}}, "next_cursor": nil, "has_more": false, "authorization_version": 7, "owner_versions": map[string]int64{candidateID.String(): 3}})
	}))
	defer server.Close()
	directory, err := NewHTTPOwnerCandidateDirectory(server.URL, "legacy-service-token")
	if err != nil {
		t.Fatal(err)
	}
	items := []AdminOwnerCandidate{}
	err = directory.Scan(context.Background(), identity.Principal{Issuer: "https://issuer.example", Subject: "admin"}, "UNIT", &organizationID, func(page []AdminOwnerCandidate) error {
		items = append(items, page...)
		return nil
	})
	if err != nil || len(items) != 1 || items[0].Type != "UNIT" {
		t.Fatalf("items=%v err=%v", items, err)
	}
	encoded, err := json.Marshal(items[0])
	if err != nil || bytes.Contains(encoded, []byte("authorization_version")) {
		t.Fatalf("owner candidate leaked internal authorization version: %s err=%v", encoded, err)
	}
}

func TestOwnerCandidateAuthorityFailsClosedForNonSelectableLifecycles(t *testing.T) {
	for _, status := range []string{"REPAIR_REQUIRED", "CLOSING", "ANONYMIZED", "DELETION_PENDING", "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": uuid.New(), "type": "USER", "display_label": "excluded", "status": status}}, "has_more": false, "authorization_version": 7})
			}))
			defer server.Close()
			directory, err := NewHTTPOwnerCandidateDirectory(server.URL, "service-token")
			if err != nil {
				t.Fatal(err)
			}
			err = directory.Scan(context.Background(), identity.Principal{Issuer: "https://issuer.example", Subject: "admin"}, "", nil, func([]AdminOwnerCandidate) error {
				t.Fatal("non-selectable owner candidate reached the consumer")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "non-selectable lifecycle") {
				t.Fatalf("status=%s err=%v", status, err)
			}
		})
	}
}

func TestOwnerCandidateAuthorityRejectsAuthorizationVersionDriftAcrossPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := int64(7)
		candidateID := uuid.New()
		response := map[string]any{"items": []map[string]any{{"id": candidateID, "type": "USER", "display_label": "candidate", "status": "ACTIVE"}}, "owner_versions": map[string]int64{candidateID.String(): 2}, "has_more": true, "next_cursor": "page-2", "authorization_version": version}
		if r.URL.Query().Get("cursor") == "page-2" {
			response["has_more"] = false
			response["next_cursor"] = nil
			response["authorization_version"] = int64(8)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	directory, err := NewHTTPOwnerCandidateDirectory(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	err = directory.Scan(context.Background(), identity.Principal{Issuer: "https://issuer.example", Subject: "admin"}, "USER", nil, func([]AdminOwnerCandidate) error { return nil })
	if !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("authorization version drift err=%v", err)
	}
}

func TestTrustedAdminQuerySourceRequiresPrivatePeerAndValidPortalSignature(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	service := &Service{adminProxyHMACKey: key}
	request := httptest.NewRequest(http.MethodGet, "http://token.internal/admin/v1/models?limit=20", nil)
	request.RemoteAddr = "10.0.0.8:4321"
	request.Header.Set("X-Pinvou-Client-Source", "203.0.113.19")
	request.Header.Set("X-Pinvou-Client-Source-Signature", "forged")
	if got := service.trustedAdminQuerySource(request); got != "10.0.0.8" {
		t.Fatalf("forged source was trusted: %q", got)
	}
	signAdminQuerySource(request, key, "203.0.113.19")
	if got := service.trustedAdminQuerySource(request); got != "203.0.113.19" {
		t.Fatalf("valid Portal source was not trusted: %q", got)
	}
	request.RemoteAddr = "198.51.100.5:4321"
	if got := service.trustedAdminQuerySource(request); got != "198.51.100.5" {
		t.Fatalf("public peer unexpectedly injected a source: %q", got)
	}
}
