package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type f06DeleteHTTPRepository struct {
	personalBoundaryRepository
	organizationAdminRepository
	mu       sync.Mutex
	state    AdminOrganizationState
	deletes  int
	blockers []OrganizationLifecycleBlocker
}

func (r *f06DeleteHTTPRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: !r.deny && permission == PermissionPlatformOrganizationsDelete, UserID: r.userID, UserStatus: "ACTIVE", Permission: permission, AuthorizationVersion: 1}, nil
}
func (r *f06DeleteHTTPRepository) AdminOrganizationState(context.Context, uuid.UUID) (AdminOrganizationState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, nil
}
func (r *f06DeleteHTTPRepository) AdminOrganizationDeletionBlockers(context.Context, uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	return r.blockers, nil
}
func (r *f06DeleteHTTPRepository) AdminDeleteOrganization(_ context.Context, p identity.Principal, org uuid.UUID, version int64, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if org != r.state.ID || p.Subject != "controlled-delete" || reason != "controlled deletion" {
		panic("original delete intent changed")
	}
	if r.state.Status != "DISABLED" || r.state.Version != version {
		return ErrConflict
	}
	r.deletes++
	r.state.Status = "DELETED"
	r.state.Version++
	return nil
}

// Actual installed identity/permission/router and actual HTTP lifecycle clients.
// Authorities below model only the existing exact status/version receipt rules,
// never an actual deployed Billing wallet, Token worker or financial side effect.
func TestF06OrganizationDeleteResumesOnlyConfirmedTuple(t *testing.T) {
	for _, tc := range []struct {
		name, billing, token, fault string
		want                        int
		deny                        bool
	}{
		{"both-applied", "DELETED", "DELETED", "", 204, false},
		{"one-applied-other-disabled", "DELETED", "DISABLED", "", 204, false},
		{"one-applied-other-active", "DELETED", "ACTIVE", "", 204, false},
		{"original-disabled", "DISABLED", "DISABLED", "", 204, false},
		{"remote-unknown", "DELETED", "DISABLED", "unknown", 503, false},
		{"remote-conflicting-tuple", "DELETED", "DISABLED", "version", 503, false},
		{"remote-blocker", "DELETED", "DISABLED", "blocker", 409, false},
		{"remote-inconsistent-ready", "DELETED", "DISABLED", "ready", 503, false},
		{"remote-apply-lost-ack", "DELETED", "DISABLED", "apply", 503, false},
		{"concurrent-local-version", "DELETED", "DISABLED", "local-version", 409, false},
		{"permission-denied", "DELETED", "DISABLED", "", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := uuid.New()
			repo := &f06DeleteHTTPRepository{personalBoundaryRepository: personalBoundaryRepository{userID: uuid.New(), deny: tc.deny}, state: AdminOrganizationState{ID: org, Status: "DISABLED", Version: 7}}
			trace := []string{}
			states := map[string]string{"BILLING": tc.billing, "TOKEN": tc.token}
			versions := map[string]int64{"BILLING": 7, "TOKEN": 7}
			for domain, state := range states {
				if state == "DELETED" {
					versions[domain] = 8
				}
			}
			clients := []OrganizationLifecycleClient{}
			for _, domain := range []string{"BILLING", "TOKEN"} {
				domain := domain
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					repo.mu.Lock()
					defer repo.mu.Unlock()
					var in struct {
						Status    string `json:"status"`
						Version   int64  `json:"account_version"`
						Preflight bool   `json:"preflight_only"`
					}
					if r.Method != "PUT" || r.URL.Path != "/internal/v1/organizations/"+org.String()+"/lifecycle" || r.Header.Get("Authorization") != "Bearer controlled-service" || json.NewDecoder(r.Body).Decode(&in) != nil {
						t.Error("wrong original lifecycle request")
						http.Error(w, "bad", 400)
						return
					}
					kind := "apply"
					if in.Preflight {
						kind = "preflight"
					}
					trace = append(trace, domain+":"+kind+":"+in.Status)
					if domain == "TOKEN" && tc.fault == "unknown" {
						http.Error(w, "temporary", 503)
						return
					}
					if states[domain] == "DELETED" && in.Status != "DELETED" || in.Version < versions[domain] {
						w.WriteHeader(409)
						_, _ = fmt.Fprint(w, `{"code":"LIFECYCLE_CONFLICT"}`)
						return
					}
					value := OrganizationLifecycleAssessment{OrganizationID: org, Status: in.Status, AccountVersion: in.Version, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
					if !(in.Version == versions[domain] && in.Status == states[domain]) && in.Status == "DELETED" && states[domain] != "DISABLED" {
						value.Ready = false
						value.Blockers = []OrganizationLifecycleBlocker{{Code: "ORGANIZATION_NOT_DISABLED"}}
					}
					if domain == "TOKEN" && tc.fault == "version" {
						value.AccountVersion++
					}
					if domain == "TOKEN" && tc.fault == "blocker" {
						value.Ready = false
						value.Blockers = []OrganizationLifecycleBlocker{{Code: "UNSETTLED_USAGE_REQUEST"}}
					}
					if domain == "TOKEN" && tc.fault == "ready" {
						value.Blockers = []OrganizationLifecycleBlocker{{Code: "UNSETTLED_USAGE_REQUEST"}}
					}
					if !in.Preflight && value.Ready {
						states[domain], versions[domain] = in.Status, in.Version
					}
					if domain == "TOKEN" && !in.Preflight && in.Status == "DELETED" {
						if tc.fault == "apply" {
							http.Error(w, "lost acknowledgement", 503)
							return
						}
						if tc.fault == "local-version" {
							repo.state.Version++
						}
					}
					if !value.Ready {
						w.WriteHeader(409)
					}
					_ = json.NewEncoder(w).Encode(value)
				}))
				defer server.Close()
				client, err := NewHTTPOrganizationLifecycleClient(server.URL, "controlled-service", domain)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
			}
			h := NewService(repo, nil).WithOrganizationLifecycle(clients...).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://owned.invalid", Subject: "controlled-delete", SessionID: "controlled-session", AuthenticatedAt: time.Now()}, nil
			})
			req := httptest.NewRequest("DELETE", "/admin/v1/organizations/"+org.String(), strings.NewReader(`{"reason":"controlled deletion"}`))
			req.Header.Set("Authorization", "Bearer controlled")
			req.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if out.Code != tc.want {
				t.Fatalf("HTTP=%d body=%s trace=%v", out.Code, out.Body, trace)
			}
			if tc.want == 204 {
				if repo.deletes != 1 || repo.state.Status != "DELETED" || repo.state.Version != 8 || states["BILLING"] != "DELETED" || states["TOKEN"] != "DELETED" || versions["BILLING"] != 8 || versions["TOKEN"] != 8 {
					t.Fatal("local completion lacked both exact receipts")
				}
				if len(trace) < 4 || trace[len(trace)-2] != "BILLING:apply:DELETED" || trace[len(trace)-1] != "TOKEN:apply:DELETED" {
					t.Fatal("preflight substituted for a write acknowledgement", trace)
				}
			} else if repo.deletes != 0 {
				t.Fatal("unconfirmed/denied operation deleted local state")
			}
			if tc.billing == "DELETED" {
				for _, call := range trace {
					if call == "BILLING:apply:DISABLED" {
						t.Fatal("terminal authority was regressed", trace)
					}
				}
			}
			if tc.deny && len(trace) != 0 {
				t.Fatal("denied caller reached lifecycle authorities")
			}
		})
	}
}
