package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Only the identity permission decision is controlled. Reads, rename, status
// completion and their side effects use the real restricted Account repository.
type f06StatusCASHTTPRepository struct {
	*PostgresRepository
	actor uuid.UUID
}

func (r *f06StatusCASHTTPRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: permission == PermissionPlatformOrganizationsStatusManage, UserID: r.actor, UserStatus: "ACTIVE", Permission: permission, AuthorizationVersion: 1}, nil
}

// The controlled remote ACK is a deterministic barrier between the handler's
// real read and real status transaction. It does not claim cross-domain atomicity
// or compensate a remote fence after a legitimate concurrent local rename.
func TestF06OrganizationStatusProjectionVersionPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "s" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16], AuthenticatedAt: time.Now()}
	me, _, err := r.Bootstrap(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
		t.Fatal(err)
	}
	snapshot := func(org uuid.UUID) (string, error) {
		var value string
		err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('organization',(SELECT to_jsonb(o) FROM organizations o WHERE id=$1),'memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m WHERE organization_id=$1),'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u),'contexts',(SELECT jsonb_agg(to_jsonb(c) ORDER BY user_id) FROM user_active_contexts c),'sessions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY issuer,session_id) FROM user_sessions s),'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events))::text`, org).Scan(&value)
		return value, err
	}
	for _, mode := range []string{"fresh-disable", "rename-before-completion"} {
		t.Run(mode, func(t *testing.T) {
			org, err := r.AdminCreateOrganization(ctx, principal, me.UserID, "Status target "+mode, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			observed, err := r.AdminOrganizationState(ctx, org.ID)
			if err != nil {
				t.Fatal(err)
			}
			ackCount := 0
			var acknowledgedVersion int64
			var afterRename string
			var remoteErr error
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var in struct {
					Status    string `json:"status"`
					Version   int64  `json:"account_version"`
					Preflight bool   `json:"preflight_only"`
				}
				if request.Method != "PUT" || request.URL.Path != "/internal/v1/organizations/"+org.ID.String()+"/lifecycle" || request.Header.Get("Authorization") != "Bearer controlled-service" || json.NewDecoder(request.Body).Decode(&in) != nil || in.Preflight || in.Status != "DISABLED" {
					http.Error(w, "unexpected controlled lifecycle request", 400)
					return
				}
				ackCount++
				acknowledgedVersion = in.Version
				if mode == "rename-before-completion" {
					remoteErr = r.AdminRenameOrganization(ctx, principal, org.ID, "Legitimate concurrent rename")
					if remoteErr == nil {
						afterRename, remoteErr = snapshot(org.ID)
					}
					if remoteErr != nil {
						http.Error(w, "controlled rename failed", 500)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(OrganizationLifecycleAssessment{OrganizationID: org.ID, Status: in.Status, AccountVersion: in.Version, Ready: true, Blockers: []OrganizationLifecycleBlocker{}})
			}))
			defer server.Close()
			client, err := NewHTTPOrganizationLifecycleClient(server.URL, "controlled-service", "TOKEN")
			if err != nil {
				t.Fatal(err)
			}
			handler := NewService(&f06StatusCASHTTPRepository{PostgresRepository: r, actor: me.UserID}, nil).WithOrganizationLifecycle(client).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			call := func() *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPatch, "/admin/v1/organizations/"+org.ID.String()+"/status", strings.NewReader(`{"status":"DISABLED"}`))
				request.Header.Set("Authorization", "Bearer controlled")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			response := call()
			if remoteErr != nil || ackCount != 1 || acknowledgedVersion != observed.Version+1 {
				t.Fatalf("remote ACK barrier failed: calls=%d version=%d error=%v", ackCount, acknowledgedVersion, remoteErr)
			}
			if mode == "rename-before-completion" {
				after, err := snapshot(org.ID)
				if err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusConflict || after != afterRename {
					t.Fatalf("stale projection must reject with zero local effects: HTTP=%d body=%s snapshotChanged=%t", response.Code, response.Body, after != afterRename)
				}
				return
			}
			state, err := r.AdminOrganizationState(ctx, org.ID)
			if err != nil || response.Code != http.StatusNoContent || state.Status != "DISABLED" || state.Version != observed.Version+1 {
				t.Fatalf("fresh confirmed completion: HTTP=%d state=%+v err=%v body=%s", response.Code, state, err, response.Body)
			}
			beforeRetry, err := snapshot(org.ID)
			if err != nil {
				t.Fatal(err)
			}
			response = call()
			afterRetry, err := snapshot(org.ID)
			if err != nil || response.Code != http.StatusNoContent || acknowledgedVersion != state.Version || beforeRetry != afterRetry {
				t.Fatalf("same-state handler retry changed original tuple/effects: HTTP=%d err=%v", response.Code, err)
			}
		})
	}
	t.Run("internal-confirmed-tuple-and-newer-revision", func(t *testing.T) {
		org, err := r.AdminCreateOrganization(ctx, principal, me.UserID, "Exact status tuple", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		state, err := r.AdminOrganizationState(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AdminSetOrganizationStatus(ctx, principal, org.ID, state.Version, "DISABLED"); err != nil {
			t.Fatal(err)
		}
		before, err := snapshot(org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AdminSetOrganizationStatus(ctx, principal, org.ID, state.Version, "DISABLED"); err != nil {
			t.Fatal("exact acknowledged target tuple could not replay", err)
		}
		for _, version := range []int64{0, state.Version - 1, state.Version + 1} {
			if err = r.AdminSetOrganizationStatus(ctx, principal, org.ID, version, "DISABLED"); err != ErrConflict {
				t.Fatalf("unconfirmed same-status tuple accepted: version=%d err=%v", version, err)
			}
		}
		after, err := snapshot(org.ID)
		if err != nil || before != after {
			t.Fatal("replay or rejected tuple changed local facts", err)
		}
		if err = r.AdminRenameOrganization(ctx, principal, org.ID, "Later revision"); err != nil {
			t.Fatal(err)
		}
		before, err = snapshot(org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AdminSetOrganizationStatus(ctx, principal, org.ID, state.Version, "DISABLED"); err != ErrConflict {
			t.Fatal("higher version borrowed an old projection receipt", err)
		}
		after, err = snapshot(org.ID)
		if err != nil || before != after {
			t.Fatal("stale replay changed newer organization", err)
		}
		current, err := r.AdminOrganizationState(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AdminSetOrganizationStatus(ctx, principal, org.ID, current.Version, "ACTIVE"); err != nil {
			t.Fatal("fresh enable failed", err)
		}
		currentAfter, err := r.AdminOrganizationState(ctx, org.ID)
		if err != nil || currentAfter.Status != "ACTIVE" || currentAfter.Version != current.Version+1 {
			t.Fatal("fresh enable did not preserve normal state transition", err)
		}
	})
}
