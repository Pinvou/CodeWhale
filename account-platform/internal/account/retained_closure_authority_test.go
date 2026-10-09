package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type retainedClosureStore struct {
	Repository // Unused methods fail if the leaf handler unexpectedly calls them.
	writes     int
}

func (s *retainedClosureStore) CreateAccountClosure(context.Context, identity.Principal, string, string) (AccountClosureRequest, error) {
	s.writes++
	return AccountClosureRequest{}, nil
}
func (*retainedClosureStore) AccountClosureForPrincipal(context.Context, identity.Principal) (AccountClosureRequest, error) {
	return AccountClosureRequest{}, nil
}
func (*retainedClosureStore) ListAccountClosureRepairs(context.Context, AccountClosureRepairFilter) ([]AccountClosureRequest, error) {
	return nil, nil
}
func (s *retainedClosureStore) RetryAccountClosure(context.Context, identity.Principal, uuid.UUID, string, string) (AccountClosureRequest, error) {
	s.writes++
	return AccountClosureRequest{}, nil
}

// Exercises the real leaf handlers and shared DecodeJSON contract. Authentication
// and repository effects belong to the separately assembled production host.
func TestRetainedClosureInvalidJSONIsExplicit(t *testing.T) {
	for _, operation := range []string{"create", "retry"} {
		for name, body := range map[string]string{"malformed": "{", "unknown": `{"unknown":true}`, "trailing": `{} {}`, "oversize": `{"reason":"` + strings.Repeat("x", 1<<20) + `"}`} {
			t.Run(operation+"/"+name, func(t *testing.T) {
				store := &retainedClosureStore{}
				service := &Service{repository: store}
				r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				r.SetPathValue("closure_request_id", uuid.NewString())
				w := httptest.NewRecorder()
				if operation == "create" {
					service.createAccountClosure(w, r)
				} else {
					service.adminRetryAccountClosure(w, r)
				}
				var wire struct {
					Code string `json:"code"`
				}
				if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &wire) != nil || wire.Code != "INVALID_REQUEST" || store.writes != 0 {
					t.Fatalf("expected explicit rejection and no repository mutation; status=%d writes=%d", w.Code, store.writes)
				}
			})
		}
	}
}

func TestRetainedClosureAssessmentAuthority(t *testing.T) {
	user := uuid.New()
	valid := `{"user_id":"` + user.String() + `","ready":true,"blockers":[]}`
	blocked := `{"user_id":"` + user.String() + `","ready":false,"blockers":[{"code":"OPEN","domain":"BILLING","resource_type":"WALLET"}]}`
	for _, c := range []struct {
		name, body string
		accept     bool
	}{
		{"ready", valid, true}, {"blocked", blocked, true},
		{"missing-ready", `{"user_id":"` + user.String() + `","blockers":[]}`, false},
		{"null-ready", `{"user_id":"` + user.String() + `","ready":null,"blockers":[]}`, false},
		{"missing-blockers", `{"user_id":"` + user.String() + `","ready":true}`, false},
		{"null-blockers", `{"user_id":"` + user.String() + `","ready":true,"blockers":null}`, false},
		{"false-empty", strings.Replace(valid, `"ready":true`, `"ready":false`, 1), false},
		{"true-nonempty", strings.Replace(blocked, `"ready":false`, `"ready":true`, 1), false},
		{"foreign-user", strings.Replace(valid, user.String(), uuid.NewString(), 1), false},
		{"foreign-domain", strings.Replace(blocked, "BILLING", "TOKEN", 1), false},
		{"blank-code", strings.Replace(blocked, "OPEN", " ", 1), false},
		{"blank-resource", strings.Replace(blocked, "WALLET", " ", 1), false},
		{"trailing", valid + ` {}`, false}, {"oversize", valid + strings.Repeat(" ", 1<<20), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/users/"+user.String()+"/account-closure-assessment" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("wrong authority request")
				}
				_, _ = w.Write([]byte(c.body))
			}))
			defer server.Close()
			client, err := NewHTTPAccountClosureClient(server.URL, "fixture-token", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Assess(context.Background(), user)
			if (err == nil) != c.accept {
				t.Fatalf("accept=%v error=%v", c.accept, err)
			}
			if c.accept && (got.UserID != user || got.Ready != (len(got.Blockers) == 0)) {
				t.Fatal("authority facts changed")
			}
		})
	}
}

func TestRetainedClosureIsolationReceipt(t *testing.T) {
	user, closure := uuid.New(), uuid.New()
	valid := `{"closure_request_id":"` + closure.String() + `","member_user_id":"` + user.String() + `","subject_lifecycle_version":2,"isolated_grant_count":0,"replayed":false}`
	for _, c := range []struct {
		name, body string
		status     int
		accept     bool
	}{
		{"fresh", valid, 200, true}, {"retry", strings.Replace(valid, `"replayed":false`, `"replayed":true`, 1), 200, true},
		{"empty", "", 200, false}, {"foreign-closure", strings.Replace(valid, closure.String(), uuid.NewString(), 1), 200, false},
		{"foreign-user", strings.Replace(valid, user.String(), uuid.NewString(), 1), 200, false},
		{"foreign-lifecycle", strings.Replace(valid, `"subject_lifecycle_version":2`, `"subject_lifecycle_version":3`, 1), 200, false},
		{"null-count", strings.Replace(valid, `"isolated_grant_count":0`, `"isolated_grant_count":null`, 1), 200, false},
		{"negative-count", strings.Replace(valid, `"isolated_grant_count":0`, `"isolated_grant_count":-1`, 1), 200, false},
		{"null-replayed", strings.Replace(valid, `"replayed":false`, `"replayed":null`, 1), 200, false},
		{"trailing", valid + ` {}`, 200, false}, {"oversize", valid + strings.Repeat(" ", 1<<20), 200, false}, {"not-200", valid, 201, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var facts struct {
					ClosureID uuid.UUID `json:"closure_request_id"`
					Lifecycle int64     `json:"subject_lifecycle_version"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/users/"+user.String()+"/account-closure-grants/isolate" || r.Header.Get("Authorization") != "Bearer fixture-token" || json.NewDecoder(r.Body).Decode(&facts) != nil || facts.ClosureID != closure || facts.Lifecycle != 2 {
					t.Error("wrong isolation request facts")
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer server.Close()
			client, err := NewHTTPAccountClosureClient(server.URL, "fixture-token", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			err = client.IsolateGrants(context.Background(), user, closure, 2)
			if (err == nil) != c.accept {
				t.Fatalf("accept=%v error=%v", c.accept, err)
			}
		})
	}
}

func TestRetainedClosureCredentialFileIsReadPerRequest(t *testing.T) {
	user := uuid.New()
	path := filepath.Join(t.TempDir(), "workload-token")
	expected := "fixture-first"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("stale workload credential")
		}
		_ = json.NewEncoder(w).Encode(AccountClosureAssessment{UserID: user, Ready: true, Blockers: []AccountClosureBlocker{}})
	}))
	defer server.Close()
	client, err := NewHTTPAccountClosureClient(server.URL, "file:"+path, "BILLING")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"fixture-first", "fixture-second"} {
		expected = token
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Assess(context.Background(), user); err != nil {
			t.Fatal(err)
		}
	}
}
