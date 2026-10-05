package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresPasswordResetResumesSameIdentityAfterUnknownResults(t *testing.T) {
	for _, fault := range []string{"revoke_lost_ack", "local_completion_write", "prepare_insert"} {
		t.Run(fault, func(t *testing.T) {
			repo := accountDatabase(t)
			principal, me := accountUser(t, repo)
			mobile := "+8613800138000"
			accountExec(t, repo, `UPDATE users SET mobile_e164=$2,mobile_verified_at=statement_timestamp() WHERE id=$1`, me.UserID, mobile)
			var mu sync.Mutex
			setCalls, revokeCalls := 0, 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.URL.Path == "/token":
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "controlled", "expires_in": 3600})
				case strings.HasSuffix(r.URL.Path, "/reset-password"):
					setCalls++
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/logout"):
					revokeCalls++
					if fault == "revoke_lost_ack" && revokeCalls == 1 {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("controlled logout acknowledgement fault failed")
							return
						}
						_ = connection.Close()
						return
					}
					w.WriteHeader(204)
				default:
					w.WriteHeader(404)
				}
			}))
			defer remote.Close()
			identityClient, vault := newIdentityBoundaryForTest(t, remote.URL, strings.Repeat("s", 32))
			reference, err := vault.Precheck("ControlledPassword1")
			if err != nil {
				t.Fatal("password reference fixture failed")
			}
			sms := &smsProviderFake{verified: true}
			auth, err := NewMobileAuthService(repo, sms, identityClient, principal.Issuer, registrationTokensForTest(t))
			if err != nil {
				t.Fatal(err)
			}
			profiles, err := NewProfileChangeService(repo, auth, identityClient)
			if err != nil {
				t.Fatal(err)
			}
			challenge := SMSChallenge{ID: uuid.New(), Mobile: mobile, Purpose: SMSPurposePasswordReset, ExpiresAt: time.Now().Add(time.Minute), Provider: sms.Descriptor()}
			if err := repo.CreateSMSChallenge(context.Background(), challenge); err != nil {
				t.Fatal(err)
			}
			if err := repo.MarkSMSChallengeSent(context.Background(), challenge.ID, "controlled", nil); err != nil {
				t.Fatal(err)
			}
			if fault == "prepare_insert" {
				accountExec(t, repo, `CREATE FUNCTION reject_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled reset persistence'; END $$; CREATE TRIGGER reject_reset BEFORE INSERT ON password_reset_operations FOR EACH ROW EXECUTE FUNCTION reject_reset()`)
			} else if fault == "local_completion_write" {
				accountExec(t, repo, `CREATE FUNCTION reject_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='COMPLETED' THEN RAISE EXCEPTION 'controlled reset completion'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_reset BEFORE UPDATE ON password_reset_operations FOR EACH ROW EXECUTE FUNCTION reject_reset()`)
			}
			server := httptest.NewServer(NewService(repo, nil).WithMobileAuth(auth, strings.Repeat("s", 32)).WithProfileChanges(profiles).Handler(nil))
			defer server.Close()
			post := func(handle string) int {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"mobile": mobile, "challenge_id": challenge.ID, "code": "123456", "password_handle": handle, "password_policy_version": reference.PolicyVersion})
				response, err := server.Client().Post(server.URL+"/v1/auth/password-resets", "application/json", strings.NewReader(string(body)))
				if err != nil {
					t.Fatal("controlled Account password reset request failed")
				}
				defer response.Body.Close()
				return response.StatusCode
			}
			if status := post(reference.Handle); status < 500 {
				t.Fatal("unknown reset result was reported as completed")
			}
			var challengeStatus string
			var count int
			if err := repo.pool.QueryRow(context.Background(), `SELECT status FROM sms_challenges WHERE id=$1`, challenge.ID).Scan(&challengeStatus); err != nil {
				t.Fatal(err)
			}
			if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM password_reset_operations`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if fault == "prepare_insert" {
				if challengeStatus != "SENT" || count != 0 {
					t.Fatal("failed reset preparation consumed its SMS proof")
				}
			} else {
				if challengeStatus != "CONSUMED" || count != 1 {
					t.Fatal("unknown reset result lost its durable operation")
				}
				if status := post("different-controlled-handle"); status != 409 {
					t.Fatal("reset replay changed its immutable password-reference facts")
				}
			}
			if fault != "revoke_lost_ack" {
				accountExec(t, repo, `DROP TRIGGER reject_reset ON password_reset_operations`)
			}
			if post(reference.Handle) != 204 || post(reference.Handle) != 204 {
				t.Fatal("same reset operation did not recover idempotently")
			}
			var completed, auditCount int
			if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM password_reset_operations WHERE status='COMPLETED'`).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action='account.password_reset.completed'`).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if setCalls != 1 || revokeCalls < 1 || completed != 1 || auditCount != 1 {
				t.Fatal("reset recovery changed the password twice or duplicated completion facts")
			}
		})
	}
}
