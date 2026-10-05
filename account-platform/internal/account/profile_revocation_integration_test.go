package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestPostgresProfileRevocationPreservesLaterRemoteLogin(t *testing.T) {
	for _, fault := range []string{"stale_completed", "local_projection", "unknown_remote_ack", "adapter_restart"} {
		t.Run(fault, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			principal, _ := accountUser(t, repo)
			stale, err := repo.CreateProfileChange(ctx, principal, "USERNAME", "profile_revoke_owned")
			if err != nil {
				t.Fatal(err)
			}
			var revocations atomic.Int32
			var active atomic.Bool
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/token":
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "controlled", "expires_in": 3600})
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{"id": principal.Subject, "username": "profile_old_owned", "enabled": true, "attributes": map[string][]string{}})
				case strings.HasSuffix(r.URL.Path, "/logout"):
					revocations.Add(1)
					active.Store(false)
					if fault == "unknown_remote_ack" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("controlled revocation acknowledgement fault failed")
							return
						}
						_ = connection.Close()
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPut:
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer remote.Close()
			client, _ := newIdentityBoundaryForTest(t, remote.URL, strings.Repeat("s", 32))
			service := &ProfileChangeService{store: repo, identities: client, now: time.Now}
			accountExec(t, repo, `INSERT INTO user_sessions(issuer,session_id,user_id,active_context_type,membership_version,last_seen_at,created_at) VALUES($1,'profile-original-owned',$2,'PERSONAL',1,statement_timestamp(),statement_timestamp())`, principal.Issuer, stale.UserID)
			if fault == "local_projection" || fault == "adapter_restart" {
				accountExec(t, repo, `CREATE FUNCTION reject_profile_revocation_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.sessions_revoked_at IS NOT NULL THEN RAISE EXCEPTION 'controlled profile revocation projection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_profile_revocation_projection BEFORE UPDATE ON profile_change_requests FOR EACH ROW EXECUTE FUNCTION reject_profile_revocation_projection()`)
			}
			completed, firstErr := service.resume(ctx, stale)
			if fault == "stale_completed" {
				if firstErr != nil || completed.Status != "COMPLETED" {
					t.Fatal("first profile change did not complete")
				}
			} else if firstErr == nil {
				t.Fatal("unknown or unpersisted revocation was reported completed")
			}
			if revocations.Load() != 1 {
				t.Fatal("first operation did not reach exactly one remote revocation")
			}
			active.Store(true)
			accountExec(t, repo, `INSERT INTO user_sessions(issuer,session_id,user_id,active_context_type,membership_version,last_seen_at,created_at) VALUES($1,'profile-later-owned',$2,'PERSONAL',1,statement_timestamp(),statement_timestamp())`, principal.Issuer, stale.UserID)
			if fault == "local_projection" || fault == "adapter_restart" {
				accountExec(t, repo, `DROP TRIGGER reject_profile_revocation_projection ON profile_change_requests`)
			}
			if fault == "adapter_restart" {
				// A fresh real Identity service has no in-memory receipt. Account's
				// durable first flag remains false and must not authorize a new call.
				service.identities, _ = newIdentityBoundaryForTest(t, remote.URL, strings.Repeat("s", 32))
			}
			_, retryErr := service.resume(ctx, stale)
			if fault == "adapter_restart" || fault == "unknown_remote_ack" {
				if retryErr == nil {
					t.Fatal("missing or unknown receipt was reported confirmed")
				}
				current, err := repo.ProfileChange(ctx, stale.ID, nil)
				if err != nil || current.Status != "REPAIR_REQUIRED" || current.LastErrorCode != "SESSION_REVOCATION_UNCONFIRMED" || current.SessionsRevokeAttemptedAt == nil {
					t.Fatal("unconfirmed revocation did not retain its durable repair facts")
				}
			} else if retryErr != nil {
				t.Fatal("confirmed profile receipt failed to converge")
			}
			if revocations.Load() != 1 || !active.Load() {
				t.Fatal("profile replay revoked a later remote login")
			}
			var sessions int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE session_id='profile-later-owned'`).Scan(&sessions); err != nil || sessions != 1 {
				t.Fatal("profile replay or delayed projection removed a later Account session")
			}
			if fault == "stale_completed" || fault == "local_projection" {
				if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE session_id='profile-original-owned'`).Scan(&sessions); err != nil || sessions != 0 {
					t.Fatal("confirmed projection did not remove its frozen original Account session")
				}
			}
		})
	}
}

func TestPostgresProfileRevocationFirstDispatchFenceHasOneWinner(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	principal, _ := accountUser(t, repo)
	change, err := repo.CreateProfileChange(ctx, principal, "USERNAME", "profile_fence_owned")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitProfileChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			first, err := repo.MarkProfileSessionsRevocationAttempt(ctx, change.ID)
			if err != nil {
				t.Error("concurrent first-dispatch fencing failed")
			} else if first {
				winners.Add(1)
			}
		}()
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatal("profile revocation granted more than one durable first dispatch")
	}
	var revocations atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logout") {
			revocations.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer remote.Close()
	client, _ := newIdentityBoundaryForTest(t, remote.URL, strings.Repeat("s", 32))
	service := &ProfileChangeService{store: repo, identities: client, now: time.Now}
	// Simulate losing the process after committing first=true but before HTTP.
	// A missing receipt is unknown, not permission to establish another dispatch.
	if _, err := service.resume(ctx, change); err == nil || revocations.Load() != 0 {
		t.Fatal("missing receipt after durable dispatch fencing guessed a new remote call")
	}
	current, err := repo.ProfileChange(ctx, change.ID, nil)
	if err != nil || current.Status != "REPAIR_REQUIRED" || current.LastErrorCode != "SESSION_REVOCATION_UNCONFIRMED" {
		t.Fatal("interrupted first dispatch did not remain visible for repair")
	}
}

func TestPostgresProfileRecoveryResponseRemainsOwnerBound(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	owner, _ := accountUser(t, repo)
	foreign, _ := accountUser(t, repo)
	change, err := repo.CreateProfileChange(ctx, owner, "USERNAME", "profile_private_owned")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ScheduleProfileChangeRetry(ctx, change.ID, "CONTROLLED_RETRY"); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"foreign", "stale_owner"} {
		t.Run(scenario, func(t *testing.T) {
			principal, expected := foreign, http.StatusNotFound
			principal.AuthenticatedAt = time.Now()
			if scenario == "stale_owner" {
				principal, expected = owner, http.StatusUnauthorized
				principal.AuthenticatedAt = time.Now().Add(-time.Hour)
			}
			service := NewService(repo, nil).WithProfileChanges(&ProfileChangeService{store: repo, now: time.Now})
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			request := httptest.NewRequest(http.MethodPost, "/v1/me/profile-change-requests/"+change.ID.String()+"/actions/complete", strings.NewReader(`{"code":""}`))
			request.Header.Set("Authorization", "Bearer controlled")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != expected || strings.Contains(response.Body.String(), "profile_private_owned") {
				t.Fatal("profile recovery response bypassed identity or recent-authentication checks")
			}
		})
	}
}
