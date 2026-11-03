package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/google/uuid"
)

// Keycloak mutation is outside this test. Both public routers, workload-authenticated
// grant claim, real Identity vault and the complete Account PostgreSQL schema are used.
type preparationUnusedDirectory struct{ identityadapter.UserDirectory }

func preparationPost(t *testing.T, endpoint string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Device-ID", "public-fixture-device")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result
}

func TestPasswordPreparationAuthorizationPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("GIN_MODE", "release")
	ctx := context.Background()
	fixture := retainedWalletFenceActors(t)
	repository := fixture.repository(t)
	signer, err := NewRegistrationTokenSigner([]byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	sms := &smsProviderFake{}
	auth := &MobileAuthService{repository: repository, sms: sms, tokens: signer, issuer: "https://public-fixture.invalid", now: func() time.Time { return time.Now().UTC() }}
	accountHandler := NewService(repository, nil).WithMobileAuth(auth, "public-fixture-workload-token").Handler(nil)
	var loseClaimACK atomic.Bool
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/auth/password-preparation-authorizations/claim" && loseClaimACK.Swap(false) {
			recorded := httptest.NewRecorder()
			accountHandler.ServeHTTP(recorded, r)
			if recorded.Code != 200 {
				t.Error("controlled claim cutpoint did not commit", recorded.Code)
			}
			connection, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			connection.Close()
			return
		}
		accountHandler.ServeHTTP(w, r)
	}))
	defer accountServer.Close()
	newIdentity := func() *httptest.Server {
		vault, e := identityadapter.NewPasswordVault([]byte(strings.Repeat("v", 32)), &preparationUnusedDirectory{})
		if e != nil {
			t.Fatal(e)
		}
		service, e := identityadapter.NewService(vault, "public-fixture-workload-token")
		if e != nil {
			t.Fatal(e)
		}
		verifier, e := identityadapter.NewHTTPPasswordPreparationVerifier(accountServer.URL, "public-fixture-workload-token", "")
		if e != nil {
			t.Fatal(e)
		}
		service, e = service.WithPasswordPreparation(verifier, "")
		if e != nil {
			t.Fatal(e)
		}
		server := httptest.NewServer(service.Handler())
		t.Cleanup(server.Close)
		return server
	}
	identityServer := newIdentity()
	provisioner, err := NewHTTPIdentityProvisioner(identityServer.URL, "public-fixture-workload-token")
	if err != nil {
		t.Fatal(err)
	}
	auth.identities = provisioner
	type attempt struct {
		body          map[string]any
		authorization passwordpreparation.Authorization
	}
	newAttempt := func(t *testing.T, purpose SMSPurpose) attempt {
		t.Helper()
		status, raw := preparationPost(t, accountServer.URL+"/v1/auth/captcha/challenges", map[string]any{"mobile": "13800138000", "purpose": purpose}, nil)
		var captcha SliderCaptchaResponse
		if status != 201 || json.Unmarshal(raw, &captcha) != nil {
			t.Fatal("real captcha producer", status, string(raw))
		}
		var target int
		if e := repository.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&target); e != nil {
			t.Fatal(e)
		}
		body := map[string]any{"mobile": "13800138000", "purpose": purpose, "captcha_id": captcha.ID, "captcha_offset": target, "operation_id": uuid.New()}
		return attempt{body: body}
	}
	issue := func(t *testing.T, a *attempt) []byte {
		t.Helper()
		status, raw := preparationPost(t, accountServer.URL+"/v1/auth/password-preparation-authorizations", a.body, nil)
		if status != 201 || json.Unmarshal(raw, &a.authorization) != nil || a.authorization.ID == uuid.Nil || a.authorization.Token == "" {
			t.Fatal("grant producer", status, string(raw))
		}
		return raw
	}
	prepare := func(t *testing.T, server *httptest.Server, a attempt, password string) (int, identityadapter.PasswordReference) {
		t.Helper()
		status, raw := preparationPost(t, server.URL+"/v1/registration/password-prechecks", map[string]any{"password": password}, map[string]string{"X-Pinvou-Password-Preparation": a.authorization.Token})
		var reference identityadapter.PasswordReference
		if status == 201 && json.Unmarshal(raw, &reference) != nil {
			t.Fatal("invalid reference")
		}
		return status, reference
	}
	t.Run("atomic-captcha-grant-replay-conflict-and-no-password-input", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		first := issue(t, &a)
		status, again := preparationPost(t, accountServer.URL+"/v1/auth/password-preparation-authorizations", a.body, nil)
		if status != 201 || !bytes.Equal(first, again) {
			t.Fatal("ACK-lost grant changed", status, string(again))
		}
		var state string
		var grants int
		if e := repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM password_preparation_authorizations WHERE id=$2) FROM slider_captcha_challenges WHERE id=$1`, a.body["captcha_id"], a.authorization.ID).Scan(&state, &grants); e != nil || state != "PASSED" || grants != 1 {
			t.Fatal("not one atomic proof", state, grants, e)
		}
		a.body["captcha_offset"] = a.body["captcha_offset"].(int) + 1
		if status, _ = preparationPost(t, accountServer.URL+"/v1/auth/password-preparation-authorizations", a.body, nil); status != 409 {
			t.Fatal("different facts replayed", status)
		}
		b := newAttempt(t, SMSPurposePasswordReset)
		b.body["password"] = "PUBLIC_SYNTHETIC_PASSWORD"
		if status, _ = preparationPost(t, accountServer.URL+"/v1/auth/password-preparation-authorizations", b.body, nil); status != 400 {
			t.Fatal("Account accepted a password", status)
		}
		if e := repository.pool.QueryRow(ctx, `SELECT status FROM slider_captcha_challenges WHERE id=$1`, b.body["captcha_id"]).Scan(&state); e != nil || state != "CREATED" {
			t.Fatal("invalid input consumed proof", state, e)
		}
	})
	t.Run("grant-write-failure-rolls-back-captcha-consumption", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		if _, e := repository.pool.Exec(ctx, `CREATE FUNCTION public.p230_inject_grant_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled grant insert failure'; END $$; CREATE TRIGGER p230_inject_grant_failure BEFORE INSERT ON password_preparation_authorizations FOR EACH ROW EXECUTE FUNCTION public.p230_inject_grant_failure()`); e != nil {
			t.Fatal(e)
		}
		status, _ := preparationPost(t, accountServer.URL+"/v1/auth/password-preparation-authorizations", a.body, nil)
		if _, e := repository.pool.Exec(ctx, `DROP TRIGGER p230_inject_grant_failure ON password_preparation_authorizations;DROP FUNCTION public.p230_inject_grant_failure()`); e != nil {
			t.Fatal(e)
		}
		var state string
		var n int
		e := repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM password_preparation_authorizations WHERE id=$2) FROM slider_captcha_challenges WHERE id=$1`, a.body["captcha_id"], a.body["operation_id"]).Scan(&state, &n)
		if status != 500 || e != nil || state != "CREATED" || n != 0 {
			t.Fatal("grant failure consumed proof", status, state, n, e)
		}
		issue(t, &a)
	})
	t.Run("real-authority-concurrent-preparation-one-incarnation-one-handle", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		issue(t, &a)
		var wg sync.WaitGroup
		references := make(chan identityadapter.PasswordReference, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, r := prepare(t, identityServer, a, "PublicSyntheticPassword9")
				if status != 201 {
					t.Error("prepare", status)
					return
				}
				references <- r
			}()
		}
		wg.Wait()
		close(references)
		var first identityadapter.PasswordReference
		for r := range references {
			if first.Handle == "" {
				first = r
			}
			if r != first {
				t.Fatal("duplicate grant allocation")
			}
		}
		if first.Handle == "" {
			t.Fatal("zero executions")
		}
		if status, _ := prepare(t, identityServer, a, "ChangedSyntheticPassword9"); status != 409 {
			t.Fatal("secret changed under same operation", status)
		}
		if status, _ := prepare(t, newIdentity(), a, "PublicSyntheticPassword9"); status != 403 {
			t.Fatal("another replica reallocated", status)
		}
		var claimed *uuid.UUID
		if e := repository.pool.QueryRow(ctx, `SELECT identity_incarnation FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&claimed); e != nil || claimed == nil {
			t.Fatal("missing durable incarnation fence", e)
		}
	})
	t.Run("claim-ACK-lost-retries-original-durable-owner-without-new-grant", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		issue(t, &a)
		loseClaimACK.Store(true)
		status, raw := preparationPost(t, identityServer.URL+"/v1/registration/password-prechecks", map[string]string{"password": "PublicSyntheticPassword9"}, map[string]string{"X-Pinvou-Password-Preparation": a.authorization.Token})
		if status != 503 || !bytes.Contains(raw, []byte("PASSWORD_PREPARATION_UNAVAILABLE")) {
			t.Fatal("unknown claim ACK retired grant", status, string(raw))
		}
		var owner uuid.UUID
		if e := repository.pool.QueryRow(ctx, `SELECT identity_incarnation FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&owner); e != nil || owner == uuid.Nil {
			t.Fatal("claim cutpoint not durable", e)
		}
		status, r := prepare(t, identityServer, a, "PublicSyntheticPassword9")
		if status != 201 || r.Handle == "" {
			t.Fatal("original owner failed to recover", status)
		}
		status, again := prepare(t, identityServer, a, "PublicSyntheticPassword9")
		if status != 201 || again != r {
			t.Fatal("recovered operation allocated twice", status)
		}
		var retained uuid.UUID
		if e := repository.pool.QueryRow(ctx, `SELECT identity_incarnation FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&retained); e != nil || retained != owner {
			t.Fatal("owner changed on recovery", e)
		}
	})
	t.Run("wrong-source-device-purpose-expiry-and-service-auth-have-no-allocation", func(t *testing.T) {
		a := newAttempt(t, SMSPurposePasswordReset)
		issue(t, &a)
		status, _ := preparationPost(t, identityServer.URL+"/v1/registration/password-prechecks", map[string]string{"password": "PublicSyntheticPassword9"}, map[string]string{"X-Pinvou-Password-Preparation": a.authorization.Token, "X-Device-ID": "different-device"})
		if status != 403 {
			t.Fatal("wrong device admitted", status)
		}
		var owner *uuid.UUID
		if e := repository.pool.QueryRow(ctx, `SELECT identity_incarnation FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&owner); e != nil || owner != nil {
			t.Fatal("failed claim wrote owner", e)
		}
		claim := passwordpreparation.Claim{Token: a.authorization.Token, Owner: uuid.New(), Source: "127.0.0.1", Device: "public-fixture-device"}
		if status, _ = preparationPost(t, accountServer.URL+"/internal/v1/auth/password-preparation-authorizations/claim", claim, nil); status != 401 {
			t.Fatal("claim workload gate", status)
		}
		wrongPurpose := map[string]any{"username": "p230.wrong-purpose", "mobile": "13800138000", "purpose": "REGISTER", "captcha_id": a.body["captcha_id"], "captcha_offset": a.body["captcha_offset"], "password_handle": "PUBLIC_SYNTHETIC_HANDLE", "password_policy_version": "user-v1", "password_preparation_token": a.authorization.Token}
		if status, _ = preparationPost(t, accountServer.URL+"/v1/auth/sms/challenges", wrongPurpose, nil); status != 403 {
			t.Fatal("wrong-purpose SMS capability", status)
		}
		var consumed bool
		if e := repository.pool.QueryRow(ctx, `SELECT sms_consumed_at IS NOT NULL FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&consumed); e != nil || consumed {
			t.Fatal("wrong-purpose wrote SMS fact", e)
		}
		claim.Source = "192.0.2.1"
		if status, _ = preparationPost(t, accountServer.URL+"/internal/v1/auth/password-preparation-authorizations/claim", claim, map[string]string{"Authorization": "Bearer public-fixture-workload-token"}); status != 403 {
			t.Fatal("wrong source admitted", status)
		}
		if _, e := repository.pool.Exec(ctx, `UPDATE password_preparation_authorizations SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, a.authorization.ID); e != nil {
			t.Fatal(e)
		}
		if status, _ = prepare(t, identityServer, a, "PublicSyntheticPassword9"); status != 403 {
			t.Fatal("expired grant allocated", status)
		}
	})
	t.Run("lock-wait-cannot-extend-unclaimed-or-SMS-deadline", func(t *testing.T) {
		for _, mode := range []string{"claim", "sms"} {
			a := newAttempt(t, SMSPurposeRegister)
			issue(t, &a)
			tx, e := repository.pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			var expires time.Time
			if e = tx.QueryRow(ctx, `UPDATE password_preparation_authorizations SET expires_at=clock_timestamp()+interval '300 milliseconds' WHERE id=$1 RETURNING expires_at`, a.authorization.ID).Scan(&expires); e != nil {
				tx.Rollback(ctx)
				t.Fatal(e)
			}
			// Commit the accelerated fixture deadline, then hold an unchanged row lock.
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			tx, e = repository.pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `SELECT 1 FROM password_preparation_authorizations WHERE id=$1 FOR UPDATE`, a.authorization.ID); e != nil {
				tx.Rollback(ctx)
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				if mode == "claim" {
					_, e := repository.ClaimPasswordPreparation(ctx, a.authorization.ID, uuid.New(), "127.0.0.1", "public-fixture-device")
					done <- e
				} else {
					done <- repository.ConsumePasswordPreparationSMS(ctx, a.authorization.ID, "+8613800138000", SMSPurposeRegister, "127.0.0.1", "public-fixture-device")
				}
			}()
			observed := false
			until := time.Now().Add(2 * time.Second)
			for time.Now().Before(until) {
				var waiting bool
				e = fixture.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event_type='Lock' AND query LIKE '%password_preparation_authorizations%')`, repository.pool.Config().ConnConfig.Database).Scan(&waiting)
				if e != nil {
					tx.Rollback(ctx)
					t.Fatal(e)
				}
				if waiting {
					observed = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !observed {
				tx.Rollback(ctx)
				t.Fatal("real row lock wait not observed")
			}
			if _, e = tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.02)`, expires); e != nil {
				tx.Rollback(ctx)
				t.Fatal(e)
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			if e = <-done; !errors.Is(e, ErrPasswordPreparation) {
				t.Fatal(mode, "lock wait extended expired authorization", e)
			}
			var wrote bool
			if e = repository.pool.QueryRow(ctx, `SELECT identity_incarnation IS NOT NULL OR sms_consumed_at IS NOT NULL FROM password_preparation_authorizations WHERE id=$1`, a.authorization.ID).Scan(&wrote); e != nil || wrote {
				t.Fatal(mode, "expired waiter wrote fact", e)
			}
		}
	})
	t.Run("actual-registration-Send-consumes-one-SMS-capability", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		issue(t, &a)
		status, r := prepare(t, identityServer, a, "PublicSyntheticPassword9")
		if status != 201 {
			t.Fatal(status)
		}
		request := map[string]any{"username": "p230.synthetic", "mobile": "13800138000", "purpose": "REGISTER", "captcha_id": a.body["captcha_id"], "captcha_offset": a.body["captcha_offset"], "password_handle": r.Handle, "password_policy_version": r.PolicyVersion, "password_preparation_token": a.authorization.Token}
		status, raw := preparationPost(t, accountServer.URL+"/v1/auth/sms/challenges", request, nil)
		if status != 202 || !bytes.Contains(raw, []byte("registration_operation_id")) {
			t.Fatal("actual Send", status, string(raw))
		}
		_, calls := sms.deliveryState()
		if calls != 1 {
			t.Fatal("SMS count", calls)
		}
		if status, _ = preparationPost(t, accountServer.URL+"/v1/auth/sms/challenges", request, nil); status != 403 {
			t.Fatal("capability resent SMS", status)
		}
		_, again := sms.deliveryState()
		if again != calls {
			t.Fatal("uncertain ACK duplicated delivery")
		}
		var n int
		if e := repository.pool.QueryRow(ctx, `SELECT count(*) FROM registration_operations WHERE mobile_e164='+8613800138000'`).Scan(&n); e != nil || n != 1 {
			t.Fatal("registration reservation duplicated", n, e)
		}
	})
	t.Run("retained-edge-canonical-source-closes-real-admission-and-preparation", func(t *testing.T) {
		// This projects only the checked nginx source-header directives into an
		// owned HTTP proxy. It does not execute or claim nginx engine acceptance.
		legacyTemplatePath := filepath.Join("..", "..", "deploy", "nginx", "account-platform.conf.template")
		rawTemplate, e := os.ReadFile(legacyTemplatePath)
		if e != nil {
			t.Fatal(e)
		}
		for _, mode := range []string{"canonical", "commented", "missing", "passthrough"} {
			t.Run(mode, func(t *testing.T) {
				projectedTemplate := string(rawTemplate)
				if mode != "canonical" {
					marker := "location = /identity/v1/registration/password-prechecks {"
					start := strings.Index(projectedTemplate, marker)
					if start < 0 {
						t.Fatal("missing mutation target")
					}
					end := strings.Index(projectedTemplate[start:], "\n    }")
					if end < 0 {
						t.Fatal("unterminated mutation target")
					}
					end += start
					directive := "proxy_set_header X-Pinvou-Client-Source $remote_addr;"
					block := projectedTemplate[start:end]
					if strings.Count(block, directive) != 1 {
						t.Fatal("ambiguous mutation target")
					}
					replacement := ""
					if mode == "commented" {
						replacement = "# " + directive
					}
					if mode == "passthrough" {
						replacement = "proxy_set_header X-Pinvou-Client-Source $http_x_pinvou_client_source;"
					}
					projectedTemplate = projectedTemplate[:start] + strings.Replace(block, directive, replacement, 1) + projectedTemplate[end:]
				}
				sourceDirective := func(location []string) string {
					values, err := preparationNginxSources(projectedTemplate, location)
					if err != nil || len(values) > 1 {
						t.Fatal("retained source directive is malformed or ambiguous", err)
					}
					// With no local source directive (or explicit $http_ passthrough),
					// nginx forwards the caller's header. This must fail real admission;
					// never turn a missing/commented directive into trusted $remote_addr.
					if len(values) == 0 {
						return "$http_x_pinvou_client_source"
					}
					value := values[0]
					if value != "$remote_addr" && value != "" && value != "$http_x_pinvou_client_source" {
						t.Fatal("test projection does not implement another source policy", value)
					}
					return value
				}
				accountSource := sourceDirective([]string{"location", "/account/"})
				identitySource := sourceDirective([]string{"location", "=", "/identity/v1/registration/password-prechecks"})
				trustedAccount, e := NewTrustedSourceHandler(NewService(repository, nil).WithMobileAuth(auth, "public-fixture-workload-token").Handler(nil), "127.0.0.1")
				if e != nil {
					t.Fatal(e)
				}
				trustedAccountServer := httptest.NewServer(trustedAccount)
				defer trustedAccountServer.Close()
				vault, e := identityadapter.NewPasswordVault([]byte(strings.Repeat("v", 32)), &preparationUnusedDirectory{})
				if e != nil {
					t.Fatal(e)
				}
				identity, e := identityadapter.NewService(vault, "public-fixture-workload-token")
				if e != nil {
					t.Fatal(e)
				}
				verifier, e := identityadapter.NewHTTPPasswordPreparationVerifier(accountServer.URL, "public-fixture-workload-token", "")
				if e != nil {
					t.Fatal(e)
				}
				identity, e = identity.WithPasswordPreparation(verifier, "127.0.0.1")
				if e != nil {
					t.Fatal(e)
				}
				trustedIdentityServer := httptest.NewServer(identity.Handler())
				defer trustedIdentityServer.Close()
				proxy := func(target, prefix, directive string) *httputil.ReverseProxy {
					endpoint, e := url.Parse(target)
					if e != nil {
						t.Fatal(e)
					}
					result := httputil.NewSingleHostReverseProxy(endpoint)
					original := result.Director
					result.Director = func(r *http.Request) {
						peer, _, e := net.SplitHostPort(r.RemoteAddr)
						if e != nil {
							t.Error(e)
						}
						r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
						original(r)
						// Set/Del replaces all client values. Never borrow XFF or an
						// incoming canonical header as the simulated connection peer.
						if directive == "$remote_addr" {
							r.Header.Set("X-Pinvou-Client-Source", peer)
						} else if directive == "" {
							r.Header.Del("X-Pinvou-Client-Source")
						} // Otherwise retain the untrusted incoming values, as nginx does.
					}
					transport := http.DefaultTransport.(*http.Transport).Clone()
					transport.Proxy = nil
					result.Transport = transport
					deferCleanup := transport.CloseIdleConnections
					t.Cleanup(deferCleanup)
					return result
				}
				accountProxy := proxy(trustedAccountServer.URL, "/account", accountSource)
				identityProxy := proxy(trustedIdentityServer.URL, "/identity", identitySource)
				edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/account/v1/auth/captcha/challenges", "/account/v1/auth/password-preparation-authorizations":
						accountProxy.ServeHTTP(w, r)
					case "/identity/v1/registration/password-prechecks":
						identityProxy.ServeHTTP(w, r)
					default:
						http.NotFound(w, r)
					}
				}))
				defer edge.Close()
				transport := &http.Transport{DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
				post := func(endpoint string, body any, token string) (int, []byte) {
					payload, e := json.Marshal(body)
					if e != nil {
						t.Fatal(e)
					}
					request, e := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
					if e != nil {
						t.Fatal(e)
					}
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("X-Device-ID", "public-fixture-device")
					request.Header["X-Pinvou-Client-Source"] = []string{"198.51.100.21", "203.0.113.22"}
					request.Header.Set("X-Forwarded-For", "203.0.113.23")
					if token != "" {
						request.Header.Set("X-Pinvou-Password-Preparation", token)
					}
					response, e := client.Do(request)
					if e != nil {
						t.Fatal(e)
					}
					defer response.Body.Close()
					result, e := io.ReadAll(io.LimitReader(response.Body, 1<<20))
					if e != nil {
						t.Fatal(e)
					}
					return response.StatusCode, result
				}
				for i, purpose := range []SMSPurpose{SMSPurposeRegister, SMSPurposePasswordReset} {
					mobile := []string{"13900139000", "13700137000"}[i]
					status, raw := post(edge.URL+"/account/v1/auth/captcha/challenges", map[string]any{"mobile": mobile, "purpose": purpose}, "")
					var captcha SliderCaptchaResponse
					if status != 201 || json.Unmarshal(raw, &captcha) != nil {
						t.Fatal("retained-edge actual captcha", status)
					}
					var target int
					if e = repository.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&target); e != nil {
						t.Fatal(e)
					}
					status, raw = post(edge.URL+"/account/v1/auth/password-preparation-authorizations", map[string]any{"mobile": mobile, "purpose": purpose, "operation_id": uuid.New(), "captcha_id": captcha.ID, "captcha_offset": target}, "")
					var authorization passwordpreparation.Authorization
					if status != 201 || json.Unmarshal(raw, &authorization) != nil {
						t.Fatal("retained-edge actual grant", status)
					}
					status, raw = post(edge.URL+"/identity/v1/registration/password-prechecks", map[string]string{"password": "PublicSyntheticPassword9"}, authorization.Token)
					var sourceMatches bool
					var owner *uuid.UUID
					if e = repository.pool.QueryRow(ctx, `SELECT source_digest=$2,identity_incarnation FROM password_preparation_authorizations WHERE id=$1`, authorization.ID, preparationDigest("127.0.0.2")).Scan(&sourceMatches, &owner); e != nil {
						t.Fatal(e)
					}
					t.Logf("purpose=%s actual client peer=127.0.0.2 edge peer=127.0.0.1 durable_source_matches=%t owner_nil=%t status=%d", purpose, sourceMatches, owner == nil, status)
					if mode != "canonical" {
						if status != http.StatusForbidden || !sourceMatches || owner != nil {
							t.Fatal("untrusted/missing edge header allocated a handle", mode, status, sourceMatches, owner != nil)
						}
						continue
					}
					var original identityadapter.PasswordReference
					if status != 201 || !sourceMatches || owner == nil || json.Unmarshal(raw, &original) != nil || original.Handle == "" {
						t.Fatal("retained edge must complete the original admitted grant", status)
					}
					status, raw = post(edge.URL+"/identity/v1/registration/password-prechecks", map[string]string{"password": "PublicSyntheticPassword9"}, authorization.Token)
					var replay identityadapter.PasswordReference
					if status != 201 || json.Unmarshal(raw, &replay) != nil || replay != original {
						t.Fatal("edge replay changed the original handle or deadline", status)
					}
				}
			})
		}
	})
	t.Run("retention-keeps-issued-grant-and-continues-later-artifact-cleanup", func(t *testing.T) {
		a := newAttempt(t, SMSPurposeRegister)
		issue(t, &a) // Real captcha/authorization producers, before advancing fixture time.
		unreferenced, young, live := uuid.New(), uuid.New(), uuid.New()
		if _, e := repository.pool.Exec(ctx, `UPDATE slider_captcha_challenges SET expires_at=now()-interval '25 hours',updated_at=now()-interval '25 hours' WHERE id=$1`, a.body["captcha_id"]); e != nil {
			t.Fatal(e)
		}
		for _, row := range []struct {
			id           uuid.UUID
			age, expires string
		}{{unreferenced, "25 hours", "-1 hour"}, {young, "12 hours", "-1 hour"}, {live, "25 hours", "1 hour"}} {
			if _, e := repository.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138001','LOGIN',20,'CREATED',now()+$2::interval,now()-$3::interval,now()-$3::interval)`, row.id, row.expires, row.age); e != nil {
				t.Fatal(e)
			}
		}
		oldSMS, uncertainSMS, liveSMS := uuid.New(), uuid.New(), uuid.New()
		for _, row := range []struct {
			id        uuid.UUID
			expires   string
			uncertain bool
		}{{oldSMS, "-100 days", false}, {uncertainSMS, "-100 days", true}, {liveSMS, "1 hour", false}} {
			if _, e := repository.pool.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version,delivery_started_at,delivery_uncertain_at) VALUES($1,'+8613800138001','LOGIN',NULL,'SENT',now()+$2::interval,now()-interval '100 days',now()-interval '100 days','fixture','v1',CASE WHEN $3 THEN now()-interval '100 days' END,CASE WHEN $3 THEN now()-interval '100 days' END)`, row.id, row.expires, row.uncertain); e != nil {
				t.Fatal(e)
			}
		}
		attempts := map[string][]uuid.UUID{}
		for _, table := range []string{"sms_send_attempts", "sms_verification_attempts"} {
			extraColumn, extraValue := "", ""
			if table == "sms_send_attempts" {
				extraColumn, extraValue = ",adapter_version", ",'v1'"
			}
			for _, age := range []string{"100 days", "89 days"} {
				id := uuid.New()
				attempts[table] = append(attempts[table], id)
				if _, e := repository.pool.Exec(ctx, `INSERT INTO `+table+`(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at`+extraColumn+`) VALUES($1,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),'LOGIN','fixture','ACCEPTED',now()-$2::interval`+extraValue+`)`, id, age); e != nil {
					t.Fatal(e)
				}
			}
		}
		var grantBefore, grantAfter []byte
		if e := repository.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM password_preparation_authorizations a WHERE id=$1`, a.authorization.ID).Scan(&grantBefore); e != nil {
			t.Fatal(e)
		}
		for pass := 0; pass < 2; pass++ {
			if e := repository.CleanupMobileRecords(ctx); e != nil {
				t.Fatalf("full cleanup must retain referenced captcha and continue 90-day cleanup: %v", e)
			}
			for _, row := range []struct {
				table string
				id    uuid.UUID
				want  bool
			}{{"slider_captcha_challenges", a.body["captcha_id"].(uuid.UUID), true}, {"slider_captcha_challenges", unreferenced, false}, {"slider_captcha_challenges", young, true}, {"slider_captcha_challenges", live, true}, {"sms_challenges", oldSMS, false}, {"sms_challenges", uncertainSMS, true}, {"sms_challenges", liveSMS, true}} {
				var exists bool
				if e := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+row.table+` WHERE id=$1)`, row.id).Scan(&exists); e != nil || exists != row.want {
					t.Fatal("retention facts", pass, row.table, row.id, exists, row.want, e)
				}
			}
			for table, ids := range attempts {
				for i, id := range ids {
					var exists bool
					if e := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&exists); e != nil || exists != (i == 1) {
						t.Fatal("later retention attempt", table, i, exists, e)
					}
				}
			}
			if e := repository.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM password_preparation_authorizations a WHERE id=$1`, a.authorization.ID).Scan(&grantAfter); e != nil || !bytes.Equal(grantBefore, grantAfter) {
				t.Fatal("cleanup changed durable authorization", e)
			}
		}
	})
	t.Run("retention-redacts-expired-mobiles-without-changing-grant-facts", func(t *testing.T) {
		for _, sample := range []struct {
			name string
			age  time.Duration
		}{{"expired", 25 * time.Hour}, {"young", 23 * time.Hour}, {"valid", 0}} {
			t.Run(sample.name, func(t *testing.T) {
				id, captcha := uuid.New(), uuid.New()
				mobile := "+8613800138003"
				expires := time.Now().UTC().Add(2 * time.Minute)
				if e := repository.CreateSliderCaptcha(ctx, SliderCaptcha{ID: captcha, Mobile: mobile, Purpose: SMSPurposeRegister, TargetX: 20, ExpiresAt: expires}); e != nil {
					t.Fatal(e)
				}
				if _, e := repository.IssuePasswordPreparation(ctx, id, captcha, mobile, SMSPurposeRegister, 20, "retention-source", "retention-device"); e != nil {
					t.Fatal(e)
				}
				if _, e := repository.ClaimPasswordPreparation(ctx, id, uuid.New(), "retention-source", "retention-device"); e != nil {
					t.Fatal(e)
				}
				if e := repository.ConsumePasswordPreparationSMS(ctx, id, mobile, SMSPurposeRegister, "retention-source", "retention-device"); e != nil {
					t.Fatal(e)
				}
				if sample.age > 0 {
					expires = time.Now().UTC().Add(-sample.age)
					if _, e := repository.pool.Exec(ctx, `UPDATE password_preparation_authorizations SET expires_at=$2 WHERE id=$1`, id, expires); e != nil {
						t.Fatal(e)
					}
					if _, e := repository.pool.Exec(ctx, `UPDATE slider_captcha_challenges SET expires_at=$2,updated_at=$2 WHERE id=$1`, captcha, expires); e != nil {
						t.Fatal(e)
					}
				}
				facts := func() string {
					t.Helper()
					var value string
					if e := repository.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(a)-'mobile_e164',to_jsonb(c)-'mobile_e164'-'updated_at')::text FROM password_preparation_authorizations a JOIN slider_captcha_challenges c ON c.id=a.captcha_id WHERE a.id=$1`, id).Scan(&value); e != nil {
						t.Fatal(e)
					}
					return value
				}
				before, previousVersions := facts(), ""
				for pass := 0; pass < 2; pass++ {
					if e := repository.CleanupMobileRecords(ctx); e != nil {
						t.Fatal(e)
					}
					var grantMobile, captchaMobile, versions string
					if e := repository.pool.QueryRow(ctx, `SELECT a.mobile_e164,c.mobile_e164,a.xmin::text||':'||c.xmin::text FROM password_preparation_authorizations a JOIN slider_captcha_challenges c ON c.id=a.captcha_id WHERE a.id=$1`, id).Scan(&grantMobile, &captchaMobile, &versions); e != nil {
						t.Fatal(e)
					}
					if sample.name == "expired" {
						if grantMobile != "expired:"+id.String() || captchaMobile != "expired:"+captcha.String() {
							t.Fatal("raw mobile retained beyond original deadline plus 24 hours")
						}
					} else if grantMobile != mobile || captchaMobile != mobile {
						t.Fatal("cleanup changed a valid or young capability")
					}
					if facts() != before || (pass == 1 && versions != previousVersions) {
						t.Fatal("cleanup changed grant facts or rewrote an already-redacted record")
					}
					previousVersions = versions
				}
			})
		}
	})
	t.Run("retention-skips-locked-expired-captcha-and-rejects-late-grant", func(t *testing.T) {
		// Independent legal target avoids consuming another test's per-mobile CAPTCHA quota.
		status, raw := preparationPost(t, accountServer.URL+"/v1/auth/captcha/challenges", map[string]any{"mobile": "13900138001", "purpose": SMSPurposeRegister}, nil)
		var captcha SliderCaptchaResponse
		if status != 201 || json.Unmarshal(raw, &captcha) != nil {
			t.Fatal("real retention-boundary captcha producer", status, string(raw))
		}
		var target int
		if err := repository.pool.QueryRow(ctx, `SELECT target_x FROM slider_captcha_challenges WHERE id=$1`, captcha.ID).Scan(&target); err != nil {
			t.Fatal(err)
		}
		a := attempt{body: map[string]any{"operation_id": uuid.New(), "captcha_id": captcha.ID, "captcha_offset": target}}

		if _, e := repository.pool.Exec(ctx, `UPDATE slider_captcha_challenges SET expires_at=now()-interval '25 hours',updated_at=now()-interval '25 hours' WHERE id=$1`, a.body["captcha_id"]); e != nil {
			t.Fatal(e)
		}
		tx, e := repository.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(ctx, `SELECT 1 FROM slider_captcha_challenges WHERE id=$1 FOR UPDATE`, a.body["captcha_id"]); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() {
			_, err := repository.IssuePasswordPreparation(ctx, a.body["operation_id"].(uuid.UUID), a.body["captcha_id"].(uuid.UUID), "+8613900138001", SMSPurposeRegister, a.body["captcha_offset"].(int), "127.0.0.1", "public-fixture-device")
			done <- err
		}()
		deadline := time.Now().Add(2 * time.Second)
		observed := false
		for time.Now().Before(deadline) {
			var waiting bool
			if e = fixture.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event_type='Lock' AND query LIKE '%slider_captcha_challenges%')`, repository.pool.Config().ConnConfig.Database).Scan(&waiting); e != nil {
				t.Fatal(e)
			}
			if waiting {
				observed = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !observed {
			t.Fatal("real grant producer did not wait for captcha row")
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if e = repository.CleanupMobileRecords(cleanupCtx); e != nil {
			t.Fatal("locked row should not stop retention", e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		if e = <-done; !errors.Is(e, ErrCaptchaInvalid) {
			t.Fatal("expired retained captcha issued a new grant", e)
		}
		var grants int
		if e = repository.pool.QueryRow(ctx, `SELECT count(*) FROM password_preparation_authorizations WHERE captcha_id=$1`, a.body["captcha_id"]).Scan(&grants); e != nil || grants != 0 {
			t.Fatal("late grant was written", grants, e)
		}
	})
	t.Run("retention-FK-protects-inflight-reference-and-subsequent-passes", func(t *testing.T) {
		// SQL-level FK concurrency control, not a claim that a 25-hour-expired
		// captcha can pass the real authorization producer's deadline check.
		id, operation := uuid.New(), uuid.New()
		if _, e := repository.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138002','REGISTER',20,'PASSED',now()-interval '25 hours',now()-interval '25 hours',now()-interval '25 hours')`, id); e != nil {
			t.Fatal(e)
		}
		tx, e := repository.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(ctx, `INSERT INTO password_preparation_authorizations(id,captcha_id,mobile_e164,purpose,source_digest,device_digest,request_digest,expires_at) VALUES($1,$2,'+8613800138002','REGISTER',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),now()-interval '25 hours')`, operation, id); e != nil {
			t.Fatal(e)
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if e = repository.CleanupMobileRecords(cleanupCtx); e != nil {
			t.Fatal("inflight FK key-share row should be skipped", e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal("retention invalidated inflight reference", e)
		}
		if e = repository.CleanupMobileRecords(ctx); e != nil {
			t.Fatal("committed reference must be retained", e)
		}
		var references int
		if e = repository.pool.QueryRow(ctx, `SELECT count(*) FROM password_preparation_authorizations a JOIN slider_captcha_challenges c ON c.id=a.captcha_id WHERE a.id=$1`, operation).Scan(&references); e != nil || references != 1 {
			t.Fatal("reference lost", references, e)
		}
		check, e := repository.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer check.Rollback(context.Background())
		_, e = check.Exec(ctx, `DELETE FROM slider_captcha_challenges WHERE id=$1`, id)
		var state interface{ SQLState() string }
		if !errors.As(e, &state) || state.SQLState() != "23503" {
			t.Fatal("FK protection was weakened", e)
		}
	})

}
