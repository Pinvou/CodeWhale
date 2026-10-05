package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresHTTPCommunityMobileRegistrationWithoutCommerceActivatesRealWallet(t *testing.T) {
	ctx := context.Background()
	repository := accountDatabase(t)
	serviceToken := strings.Repeat("t", 32)
	subject := uuid.NewString()
	var remoteRegistration map[string]any
	issuer := "https://identity.example.test"
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sms/send":
			if r.Header.Get("Authorization") != "Bearer "+serviceToken {
				t.Error("SMS adapter authentication missing")
			}
			io.WriteString(w, `{"message_id":"controlled-delivery"}`)
		case "/sms/verify":
			var request struct {
				Mobile    string `json:"mobile"`
				Challenge string `json:"challenge_id"`
				Code      string `json:"code"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Challenge == "" || request.Code != "123456" {
				t.Error("SMS verification request facts missing")
			}
			io.WriteString(w, `{"verified":true}`)
		case "/token":
			io.WriteString(w, `{"access_token":"controlled-fixture-token","expires_in":3600}`)
		case "/admin/realms/pinvou/users":
			if r.Header.Get("Authorization") != "Bearer controlled-fixture-token" {
				t.Error("identity provisioning authentication missing")
			}
			if r.Method == http.MethodGet {
				if remoteRegistration == nil {
					io.WriteString(w, "[]")
				} else {
					json.NewEncoder(w).Encode([]any{remoteRegistration})
				}
			} else {
				if json.NewDecoder(r.Body).Decode(&remoteRegistration) != nil {
					t.Error("registration request invalid")
				}
				remoteRegistration["id"] = subject
				w.Header().Set("Location", "/admin/realms/pinvou/users/"+subject)
				w.WriteHeader(http.StatusCreated)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer adapter.Close()
	sms, err := NewSMSProvider(SMSProviderConfig{Provider: "http", Endpoint: adapter.URL + "/sms", ServiceToken: serviceToken, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewKeycloakProvisioner(adapter.URL, adapter.URL+"/token", "pinvou", "controlled-client", serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewMobileAuthService(repository, sms, provisioner, issuer)
	if err != nil {
		t.Fatal(err)
	}
	billingPool := testdb.Open(t, "billing")
	billingDSN, err := url.Parse(billingPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := billingDSN.Query()
	query.Set("search_path", billingPool.Config().ConnConfig.RuntimeParams["search_path"])
	billingDSN.RawQuery = query.Encode()
	billingRepository, err := billing.NewPostgresRepository(ctx, billingDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer billingRepository.Close()
	billingServer := httptest.NewServer(billing.NewService(billingRepository).Handler(serviceToken, func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{}, errors.New("controlled invalid token")
	}))
	defer billingServer.Close()
	wallets, err := NewHTTPWalletProvisioner(billingServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{Issuer: issuer, Subject: subject, SessionID: uuid.NewString()}
	service := NewService(repository, wallets).WithMobileAuth(auth, serviceToken)
	handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
	post := func(path string, body any, status int) []byte {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", path, strings.NewReader(string(encoded)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer controlled-user")
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s status=%d expected=%d", path, response.Code, status)
		}
		return response.Body.Bytes()
	}
	username := "community" + uuid.NewString()[:8]
	mobile := "+8613800138000"
	post("/v1/auth/registration/precheck", map[string]any{"username": username, "mobile": mobile, "password": "ControlledAa1"}, 200)
	var captcha SliderCaptchaResponse
	if err := json.Unmarshal(post("/v1/auth/captcha/challenges", map[string]any{"mobile": mobile, "purpose": "REGISTER"}, 201), &captcha); err != nil {
		t.Fatal(err)
	}
	// Diagnostic fixture reads only its own challenge, rather than pretending
	// this controlled HTTP test proves a person solved the visual puzzle.
	var offset int
	if err := repository.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	var challenge SMSChallenge
	if err := json.Unmarshal(post("/v1/auth/sms/challenges", map[string]any{"username": username, "mobile": mobile, "password": "ControlledAa1", "purpose": "REGISTER", "captcha_id": captcha.ID, "captcha_offset": offset}, 202), &challenge); err != nil {
		t.Fatal(err)
	}
	var registration struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := json.Unmarshal(post("/v1/auth/register", map[string]any{"username": username, "mobile": mobile, "password": "ControlledAa1", "challenge_id": challenge.ID, "code": "123456"}, 201), &registration); err != nil {
		t.Fatal(err)
	}
	var me Me
	if err := json.Unmarshal(post("/v1/account/bootstrap", map[string]any{}, 200), &me); err != nil {
		t.Fatal(err)
	}
	if me.UserID != registration.UserID || me.Status != "ACTIVE" || !me.MobileVerified || me.MallSyncStatus != "NOT_CONFIGURED" {
		t.Fatal("community registration failed to activate without commerce")
	}
	var jobs, walletCount int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM mall_user_sync_jobs WHERE user_id=$1`, me.UserID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1 AND status='ACTIVE'`, me.UserID).Scan(&walletCount); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || walletCount != 1 {
		t.Fatal("community path queued enterprise synchronization or lost wallet")
	}
	post("/v1/account/bootstrap", map[string]any{}, 200)
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1`, me.UserID).Scan(&walletCount); err != nil || walletCount != 1 {
		t.Fatal("wallet bootstrap replay was not idempotent")
	}
}

func TestMobileHTTPMalformedBodiesAreRejectedBeforeSideEffects(t *testing.T) {
	service := NewService(nil, nil).WithMobileAuth(&MobileAuthService{}, strings.Repeat("t", 32))
	handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{}, errors.New("controlled invalid token")
	})
	for _, path := range []string{"/v1/auth/registration/precheck", "/v1/auth/captcha/challenges", "/v1/auth/sms/challenges", "/v1/auth/register", "/internal/v1/auth/sms/verify"} {
		for _, body := range []string{"{", `{"unknown":true}`, `{} {}`, `{"challenge_id":"not-a-uuid"}`, strings.Repeat(" ", 1<<20) + "{}"} {
			t.Run(fmt.Sprintf("%s/%d", path, len(body)), func(t *testing.T) {
				response := httptest.NewRecorder()
				request := httptest.NewRequest("POST", path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
				handler.ServeHTTP(response, request)
				var errorBody struct {
					Code string `json:"code"`
				}
				json.Unmarshal(response.Body.Bytes(), &errorBody)
				if response.Code != 400 || errorBody.Code != "INVALID_REQUEST" {
					t.Fatalf("bad input status=%d code=%s", response.Code, errorBody.Code)
				}
			})
		}
	}
}

func TestSMSAndKeycloakResponsesRequireCompleteBoundedJSONAndPositiveTTL(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing_ttl", `{"access_token":"controlled"}`},
		{"zero_ttl", `{"access_token":"controlled","expires_in":0}`},
		{"negative_ttl", `{"access_token":"controlled","expires_in":-1}`},
		{"overflow_ttl", `{"access_token":"controlled","expires_in":9223372036854775807}`},
		{"tail", `{"access_token":"controlled","expires_in":3600}{}`},
		{"oversize", `{"access_token":"controlled","expires_in":3600}` + strings.Repeat(" ", 256<<10) + "{}"},
		{"valid", `{"access_token":"controlled","expires_in":3600}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, test.body) }))
			defer server.Close()
			provisioner, err := NewKeycloakProvisioner(server.URL, server.URL, "pinvou", "controlled", "controlled-secret")
			if err != nil {
				t.Fatal(err)
			}
			_, err = provisioner.accessToken(context.Background())
			if (err == nil) != (test.name == "valid") {
				t.Fatal("provider TTL/body acceptance mismatch")
			}
		})
	}
	for _, provider := range []string{"http", "aliyun"} {
		for _, mode := range []string{"tail", "oversize", "redirect", "valid"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "redirect" {
						w.Header().Set("Location", "/unexpected")
						w.WriteHeader(302)
						return
					}
					body := `{"message_id":"controlled","verified":true}`
					if provider == "aliyun" {
						body = `{"Code":"OK","Success":true,"Model":{"BizId":"controlled","VerifyResult":"PASS"}}`
					}
					if mode == "tail" {
						body += "{}"
					}
					if mode == "oversize" {
						body += strings.Repeat(" ", 256<<10) + "{}"
					}
					io.WriteString(w, body)
				}))
				defer server.Close()
				var sms SMSProvider
				if provider == "http" {
					p, err := NewSMSProvider(SMSProviderConfig{Provider: "http", Endpoint: server.URL, ServiceToken: strings.Repeat("t", 32)})
					if err != nil {
						t.Fatal(err)
					}
					p.(*httpSMSProvider).client.Transport = server.Client().Transport
					sms = p
				} else {
					p, err := NewAliyunSMSProvider(AliyunSMSConfig{Endpoint: server.URL, AccessKeyID: "controlled", AccessKeySecret: strings.Repeat("t", 32), SignName: "controlled", RegisterTemplate: "controlled", LoginTemplate: "controlled"})
					if err != nil {
						t.Fatal(err)
					}
					p.client.Transport = server.Client().Transport
					sms = p
				}
				_, sendErr := sms.SendCode(context.Background(), "+8613800138000", SMSPurposeRegister, uuid.New())
				_, verifyErr := sms.VerifyCode(context.Background(), "+8613800138000", uuid.New(), "123456")
				if mode == "valid" {
					if sendErr != nil || verifyErr != nil {
						t.Fatal("valid provider response rejected")
					}
				} else if sendErr == nil || verifyErr == nil {
					t.Fatal("invalid provider body/redirect accepted")
				}
			})
		}
	}
}

func TestPostgresSMSAttemptWriteFailureCannotMasqueradeAsConsumedLimit(t *testing.T) {
	repository := accountDatabase(t)
	ctx := context.Background()
	challenge := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138000", Purpose: SMSPurposeLogin, ExpiresAt: time.Now().Add(time.Hour)}
	if err := repository.CreateSMSChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkSMSChallengeSent(ctx, challenge.ID, "controlled"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `CREATE FUNCTION reject_sms_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled fixture'; END $$; CREATE TRIGGER reject_sms_attempt BEFORE UPDATE ON sms_challenges FOR EACH ROW EXECUTE FUNCTION reject_sms_attempt()`); err != nil {
		t.Fatal(err)
	}
	if err := repository.ConsumeSMSChallenge(ctx, challenge.ID, challenge.Mobile, challenge.Purpose, false); err == nil || errors.Is(err, ErrSMSInvalid) {
		t.Fatal("SQL failure was reported as a successful invalid-code attempt write")
	}
	if _, err := repository.pool.Exec(ctx, `DROP TRIGGER reject_sms_attempt ON sms_challenges`); err != nil {
		t.Fatal(err)
	}
	if err := repository.ConsumeSMSChallenge(ctx, challenge.ID, challenge.Mobile, challenge.Purpose, false); !errors.Is(err, ErrSMSInvalid) {
		t.Fatal("invalid code should persist one rejected attempt")
	}
	var attempts int
	if err := repository.pool.QueryRow(ctx, `SELECT attempts FROM sms_challenges WHERE id=$1`, challenge.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("failed write changed attempt count")
	}
}
