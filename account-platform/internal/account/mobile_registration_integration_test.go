package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresRegistrationRecoversOnlyItsOwnIntentWithoutResettingPassword(t *testing.T) {
	for _, fault := range []string{"lost_create_ack", "local_identity_write"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			repo := accountDatabase(t)
			issuer := "https://identity.example.test"
			username, mobile := "recover"+uuid.NewString()[:8], "+8613800138000"
			now := time.Now().UTC()
			repo.now = func() time.Time { return now }
			var mu sync.Mutex
			var remote map[string]any
			var firstPassword string
			createCalls := 0
			failLookup := false
			subject := uuid.NewString()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/sms/send":
					json.NewEncoder(w).Encode(map[string]any{"message_id": "controlled"})
				case "/sms/verify":
					json.NewEncoder(w).Encode(map[string]any{"verified": true})
				case "/token":
					json.NewEncoder(w).Encode(map[string]any{"access_token": "controlled", "expires_in": 3600})
				case "/admin/realms/pinvou/users":
					if r.Method == http.MethodGet {
						if failLookup {
							w.WriteHeader(503)
							return
						}
						if remote == nil {
							json.NewEncoder(w).Encode([]any{})
							return
						}
						json.NewEncoder(w).Encode([]any{remote})
						return
					}
					createCalls++
					if r.Method != http.MethodPost || remote != nil {
						w.WriteHeader(409)
						return
					}
					var request struct {
						Username    string              `json:"username"`
						Enabled     bool                `json:"enabled"`
						Attributes  map[string][]string `json:"attributes"`
						Credentials []struct {
							Value string `json:"value"`
						} `json:"credentials"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Credentials) != 1 {
						w.WriteHeader(400)
						return
					}
					firstPassword = request.Credentials[0].Value
					remote = map[string]any{"id": subject, "username": request.Username, "enabled": request.Enabled, "attributes": request.Attributes}
					if fault == "lost_create_ack" {
						failLookup = true
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("controlled connection fault failed")
							return
						}
						conn.Close()
						return
					}
					w.Header().Set("Location", "/admin/realms/pinvou/users/"+subject)
					w.WriteHeader(201)
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			provisioner, err := NewKeycloakProvisioner(server.URL, server.URL+"/token", "pinvou", "controlled", strings.Repeat("s", 32))
			if err != nil {
				t.Fatal(err)
			}
			sms, err := NewSMSProvider(SMSProviderConfig{Provider: "http", Endpoint: server.URL + "/sms", ServiceToken: strings.Repeat("s", 32), AllowInsecure: true})
			if err != nil {
				t.Fatal(err)
			}
			auth, err := NewMobileAuthService(repo, sms, provisioner, issuer)
			if err != nil {
				t.Fatal(err)
			}
			auth.now = func() time.Time { return now }
			accountHTTP := httptest.NewServer(NewService(repo, nil).WithMobileAuth(auth, strings.Repeat("s", 32)).Handler(nil))
			defer accountHTTP.Close()
			post := func(path string, body any, expected int) []byte {
				t.Helper()
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				response, err := accountHTTP.Client().Post(accountHTTP.URL+path, "application/json", strings.NewReader(string(encoded)))
				if err != nil {
					t.Fatal("controlled Account HTTP unavailable")
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != expected {
					t.Fatalf("Account route %s status=%d expected=%d", path, response.StatusCode, expected)
				}
				return data
			}
			challenge := func(password string) uuid.UUID {
				t.Helper()
				post("/v1/auth/registration/precheck", map[string]any{"username": username, "mobile": mobile, "password": password}, 200)
				var captcha SliderCaptchaResponse
				if err := json.Unmarshal(post("/v1/auth/captcha/challenges", map[string]any{"mobile": mobile, "purpose": "REGISTER"}, 201), &captcha); err != nil {
					t.Fatal(err)
				}
				// This diagnostic reads its own random challenge; it does not
				// claim a human solved the visual puzzle or a real SMS was sent.
				var offset int
				if err := repo.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&offset); err != nil {
					t.Fatal(err)
				}
				var smsChallenge SMSChallenge
				if err := json.Unmarshal(post("/v1/auth/sms/challenges", map[string]any{"username": username, "mobile": mobile, "password": password, "purpose": "REGISTER", "captcha_id": captcha.ID, "captcha_offset": offset}, 202), &smsChallenge); err != nil {
					t.Fatal(err)
				}
				return smsChallenge.ID
			}
			if fault == "local_identity_write" {
				if _, err := repo.pool.Exec(ctx, `CREATE FUNCTION fail_registration_identity() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'controlled identity fault'; END$$; CREATE TRIGGER fail_registration_identity BEFORE INSERT ON identities FOR EACH ROW EXECUTE FUNCTION fail_registration_identity()`); err != nil {
					t.Fatal(err)
				}
			}
			post("/v1/auth/register", map[string]any{"username": username, "mobile": mobile, "challenge_id": challenge("FirstPassword1"), "code": "123456", "password": "FirstPassword1"}, 500)
			pending, err := repo.PendingMobileRegistration(ctx, issuer, username, mobile)
			if err != nil || pending.UserID == uuid.Nil {
				t.Fatal("durable registration intent missing")
			}
			if _, err := repo.MobileIdentitySubject(ctx, issuer, mobile); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("uncompleted registration exposed an identity")
			}
			if _, err := auth.PrecheckRegistration(ctx, username, mobile, "DifferentPassword2"); err != nil {
				t.Fatal("owned pending registration cannot recover")
			}
			if fault == "local_identity_write" {
				if _, err := repo.pool.Exec(ctx, `DROP TRIGGER fail_registration_identity ON identities`); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			failLookup = false
			mu.Unlock()
			now = now.Add(61 * time.Second)
			var result MobileRegistrationResult
			err = json.Unmarshal(post("/v1/auth/register", map[string]any{"username": username, "mobile": mobile, "challenge_id": challenge("DifferentPassword2"), "code": "123456", "password": "DifferentPassword2"}, 201), &result)
			if err != nil || result.UserID != pending.UserID || !result.PasswordPreserved {
				t.Fatal("registration did not recover its exact identity")
			}
			mu.Lock()
			calls, password := createCalls, firstPassword
			mu.Unlock()
			if calls != 1 || password != "FirstPassword1" {
				t.Fatal("recovery recreated identity or reset password")
			}
			if bound, err := repo.MobileIdentitySubject(ctx, issuer, mobile); err != nil || bound != subject {
				t.Fatal("real remote subject was not bound")
			}
			var users, identities, complete int
			if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM identities),(SELECT count(*) FROM audit_logs WHERE action='account.mobile.registration.complete')`).Scan(&users, &identities, &complete); err != nil || users != 1 || identities != 1 || complete != 1 {
				t.Fatal("registration completion was not atomic and unique")
			}
			var sensitive bool
			if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE change_summary::text LIKE '%'||$1||'%' OR change_summary::text LIKE '%'||$2||'%')`, username, mobile).Scan(&sensitive); err != nil || sensitive {
				t.Fatal("registration audit disclosed typed identity facts")
			}
			if err := repo.CompleteMobileRegistration(ctx, issuer, pending, subject, false); err != nil {
				t.Fatal("exact completion replay failed")
			}
		})
	}
}

func TestPostgresRegistrationCaptchaOnlyAllowsItsUnfinishedIssuerIntent(t *testing.T) {
	for _, scenario := range []string{"pending", "active", "disabled", "deleted", "foreign_issuer", "unmarked", "identity_bound", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repo := accountDatabase(t)
			issuer := "https://identity.example.test"
			mobile := "+8613800138000"
			registration, err := repo.PrepareMobileRegistration(ctx, issuer, "captcha"+uuid.NewString()[:8], mobile)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "active", "disabled", "deleted":
				_, err = repo.pool.Exec(ctx, `UPDATE users SET status=$2 WHERE id=$1`, registration.UserID, strings.ToUpper(scenario))
			case "foreign_issuer":
				issuer = "https://other.example.test"
			case "unmarked":
				_, err = repo.pool.Exec(ctx, `DELETE FROM audit_logs WHERE action='account.mobile.registration.prepare'`)
			case "identity_bound":
				_, err = repo.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,now(),now())`, issuer, uuid.NewString(), registration.UserID)
			case "complete":
				err = repo.CompleteMobileRegistration(ctx, issuer, registration, uuid.NewString(), false)
				if err == nil {
					// Remove only this fixture's binding to isolate the completed
					// intent guard from the independent no-identity guard.
					_, err = repo.pool.Exec(ctx, `DELETE FROM identities WHERE user_id=$1`, registration.UserID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			auth, err := NewMobileAuthService(repo, &smsProviderFake{}, &identityProvisionerFake{}, issuer)
			if err != nil {
				t.Fatal(err)
			}
			handler := NewService(repo, nil).WithMobileAuth(auth, strings.Repeat("s", 32)).Handler(nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/auth/captcha/challenges", strings.NewReader(`{"mobile":"`+mobile+`","purpose":"REGISTER"}`)))
			expected := http.StatusConflict
			if scenario == "pending" || scenario == "deleted" {
				// A deleted phone may be reused under a new username. It is
				// not a recovered intent and cannot pass Send with the old name.
				expected = http.StatusCreated
			}
			if response.Code != expected {
				t.Fatalf("captcha eligibility status=%d expected=%d", response.Code, expected)
			}
			if scenario != "pending" {
				if recoverable, err := repo.MobileRegistrationRecoverable(ctx, issuer, mobile); err != nil || recoverable {
					t.Fatal("nonrecoverable registration passed the typed issuer gate")
				}
			}
		})
	}
}

func TestPostgresMobileRegistrationIntentCannotAdoptOtherProvisioningUsers(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	issuer := "https://identity.example.test"
	username := "pending" + uuid.NewString()[:8]
	mobile := "+8613800138000"
	var wg sync.WaitGroup
	results := make(chan MobileRegistration, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registration, err := repo.PrepareMobileRegistration(ctx, issuer, username, mobile)
			results <- registration
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent preparation failed")
		}
	}
	var id uuid.UUID
	for registration := range results {
		if id == uuid.Nil {
			id = registration.UserID
		}
		if id != registration.UserID {
			t.Fatal("concurrent preparation split registration identity")
		}
	}
	if _, err := repo.PrepareMobileRegistration(ctx, issuer, username, "+8613900138000"); !errors.Is(err, ErrUsernameAlreadyRegistered) {
		t.Fatal("changed registration facts adopted existing intent")
	}
	if _, err := repo.PendingMobileRegistration(ctx, "https://other.example.test", username, mobile); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("foreign issuer adopted registration intent")
	}
	if _, err := repo.pool.Exec(ctx, `DELETE FROM audit_logs WHERE action='account.mobile.registration.prepare'`); err != nil {
		t.Fatal(err)
	} // owned fault injection only
	if _, err := repo.PendingMobileRegistration(ctx, issuer, username, mobile); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("unmarked PROVISIONING user was adopted")
	}
	if err := repo.CompleteMobileRegistration(ctx, issuer, MobileRegistration{UserID: id, Username: username, Mobile: mobile}, uuid.NewString(), false); !errors.Is(err, ErrConflict) {
		t.Fatal("unmarked user completed registration")
	}
}

func TestKeycloakRegistrationRejectsForeignMarkerFactsAndAmbiguousSubjects(t *testing.T) {
	registration := MobileRegistration{UserID: uuid.New(), Username: "controlleduser", Mobile: "+8613800138000"}
	for _, scenario := range []string{"wrong_mobile", "wrong_username", "wrong_marker", "multiple_subjects", "disabled", "trailing_json", "null_result"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					json.NewEncoder(w).Encode(map[string]any{"access_token": "controlled", "expires_in": 3600})
					return
				}
				if r.Method != http.MethodGet {
					t.Error("foreign response triggered remote mutation")
					w.WriteHeader(400)
					return
				}
				if scenario == "null_result" {
					w.Write([]byte("null"))
					return
				}
				username, mobile, marker, enabled := registration.Username, registration.Mobile, registration.UserID.String(), true
				switch scenario {
				case "wrong_mobile":
					mobile = "+8613900138000"
				case "wrong_username":
					username = "foreignuser"
				case "wrong_marker":
					marker = uuid.NewString()
				case "disabled":
					enabled = false
				}
				user := map[string]any{"id": uuid.NewString(), "username": username, "enabled": enabled, "attributes": map[string][]string{"phone_number": {mobile}, "phone_number_verified": {"true"}, "pinvou_registration_id": {marker}}}
				users := []any{user}
				if scenario == "multiple_subjects" {
					users = append(users, user)
				}
				json.NewEncoder(w).Encode(users)
				if scenario == "trailing_json" {
					w.Write([]byte("{}"))
				}
			}))
			defer server.Close()
			p, err := NewKeycloakProvisioner(server.URL, server.URL+"/token", "pinvou", "controlled", strings.Repeat("s", 32))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.CreateMobileUser(context.Background(), registration, "ControlledPassword1"); err == nil {
				t.Fatal("foreign or ambiguous identity was accepted")
			}
		})
	}
}

func TestPostgresMobileRegistrationFaultsRollbackTypedFactsAndCompletion(t *testing.T) {
	for _, stage := range []string{"prepare_audit", "complete_audit", "commerce_job"} {
		t.Run(stage, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			issuer := "https://identity.example.test"
			username := "atomic" + uuid.NewString()[:8]
			mobile := "+8613800138000"
			var registration MobileRegistration
			if stage != "prepare_audit" {
				var err error
				registration, err = repo.PrepareMobileRegistration(ctx, issuer, username, mobile)
				if err != nil {
					t.Fatal(err)
				}
			}
			table, condition := "audit_logs", "NEW.action='account.mobile.registration.prepare'"
			if stage == "complete_audit" {
				condition = "NEW.action='account.mobile.registration.complete'"
			}
			if stage == "commerce_job" {
				table = "mall_user_sync_jobs"
				condition = "TRUE"
			}
			if _, err := repo.pool.Exec(ctx, `CREATE FUNCTION fail_registration_stage() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF `+condition+` THEN RAISE EXCEPTION 'controlled registration stage fault'; END IF; RETURN NEW; END$$; CREATE TRIGGER fail_registration_stage BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION fail_registration_stage()`); err != nil {
				t.Fatal(err)
			}
			subject := uuid.NewString()
			if stage == "prepare_audit" {
				if _, err := repo.PrepareMobileRegistration(ctx, issuer, username, mobile); err == nil {
					t.Fatal("prepare audit failure was hidden")
				}
			} else if err := repo.CompleteMobileRegistration(ctx, issuer, registration, subject, true); err == nil {
				t.Fatal("complete failure was hidden")
			}
			var users, ids, jobs int
			if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM identities),(SELECT count(*) FROM mall_user_sync_jobs)`).Scan(&users, &ids, &jobs); err != nil {
				t.Fatal(err)
			}
			expectedUsers := 1
			if stage == "prepare_audit" {
				expectedUsers = 0
			}
			if users != expectedUsers || ids != 0 || jobs != 0 {
				t.Fatal("registration transaction left partial identity facts")
			}
			if _, err := repo.pool.Exec(ctx, `DROP TRIGGER fail_registration_stage ON `+table); err != nil {
				t.Fatal(err)
			}
			if stage == "prepare_audit" {
				var err error
				registration, err = repo.PrepareMobileRegistration(ctx, issuer, username, mobile)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.CompleteMobileRegistration(ctx, issuer, registration, subject, true); err != nil {
				t.Fatal(err)
			}
			if err := repo.CompleteMobileRegistration(ctx, issuer, registration, subject, true); err != nil {
				t.Fatal(err)
			}
			if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities),(SELECT count(*) FROM mall_user_sync_jobs)`).Scan(&ids, &jobs); err != nil || ids != 1 || jobs != 1 {
				t.Fatal("completion retry duplicated identity or commerce job")
			}
		})
	}
}

func TestPostgresMobileIdentityResolutionUsesCurrentAccountAndReservesDeletedUsername(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	issuer := "https://identity.example.test"
	mobile := "+8613800138000"
	old, err := repo.PrepareMobileRegistration(ctx, issuer, "old"+uuid.NewString()[:8], mobile)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteMobileRegistration(ctx, issuer, old, uuid.NewString(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE users SET status='DELETED' WHERE id=$1`, old.UserID); err != nil {
		t.Fatal(err)
	}
	if exists, err := repo.UsernameExists(ctx, old.Username); err != nil || !exists {
		t.Fatal("deleted realm identity username was advertised as reusable")
	}
	current, err := repo.PrepareMobileRegistration(ctx, issuer, "current"+uuid.NewString()[:8], mobile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MobileIdentitySubject(ctx, issuer, mobile); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("unfinished replacement returned the deleted subject")
	}
	subject := uuid.NewString()
	if err := repo.CompleteMobileRegistration(ctx, issuer, current, subject, false); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("s", 32)
	service := NewService(repo, nil).WithMobileAuth(&MobileAuthService{repository: repo, issuer: issuer}, token)
	for _, authorized := range []bool{false, true} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/internal/v1/auth/mobile/identity", strings.NewReader(`{"mobile":"`+mobile+`"}`))
		if authorized {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		service.Handler(nil).ServeHTTP(response, request)
		if authorized {
			if response.Code != 204 || response.Header().Get("X-Pinvou-Identity-Subject") != subject {
				t.Fatal("identity resolver did not bind current Account subject")
			}
		} else if response.Code != 401 || response.Header().Get("X-Pinvou-Identity-Subject") != "" {
			t.Fatal("identity resolver lacked service authentication")
		}
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, current.UserID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteMobileRegistration(ctx, issuer, current, subject, false); !errors.Is(err, ErrConflict) {
		t.Fatal("late completion bypassed business disable")
	}
}

func TestPostgresHTTPBootstrapWalletFailureRetainsCompleteMobileDefaults(t *testing.T) {
	repo := accountDatabase(t)
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer billing.Close()
	wallets, err := NewHTTPWalletProvisioner(billing.URL, strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), Username: "regular" + uuid.NewString()[:8], SessionID: uuid.NewString()}
	handler := NewService(repo, wallets).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/account/bootstrap", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer controlled")
	handler.ServeHTTP(response, request)
	var body struct {
		Account Me `json:"account"`
	}
	if response.Code != 503 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Account.Status != "PROVISIONING" || body.Account.MobileVerified || body.Account.Mobile != nil || body.Account.MallSyncStatus != "NOT_CONFIGURED" {
		t.Fatal("wallet failure emitted incomplete mobile account defaults")
	}
	var status string
	if err := repo.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id=$1`, body.Account.UserID).Scan(&status); err != nil || status != "PROVISIONING" {
		t.Fatal("wallet failure activated account")
	}
}
