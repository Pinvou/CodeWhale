package token

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

type CredentialOIDCVerifyFunc func(context.Context, string, string) (identity.Principal, error)

func NewCredentialOIDCVerifyFunc(ctx context.Context, issuer, jwksURL, audience, environment string) (CredentialOIDCVerifyFunc, error) {
	issuer, jwksURL, audience = strings.TrimRight(strings.TrimSpace(issuer), "/"), strings.TrimSpace(jwksURL), strings.TrimSpace(audience)
	parsed, err := url.Parse(issuer)
	secure := !strings.EqualFold(environment, "development") && !strings.EqualFold(environment, "test")
	if err != nil || audience == "" || !validCredentialEndpoint(parsed, secure, true) {
		return nil, errors.New("invalid credential OIDC configuration")
	}
	if jwksURL == "" {
		jwksURL = issuer + "/protocol/openid-connect/certs"
	}
	parsedJWKS, err := url.Parse(jwksURL)
	if err != nil || !validCredentialEndpoint(parsedJWKS, secure, false) || exactURLOrigin(parsedJWKS) != exactURLOrigin(parsed) {
		return nil, errors.New("invalid credential OIDC configuration")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx = oidc.ClientContext(ctx, client)
	keys := oidc.NewRemoteKeySet(ctx, parsedJWKS.String())
	verifier := oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience})
	return func(ctx context.Context, raw, expectedNonce string) (identity.Principal, error) {
		token, err := verifier.Verify(ctx, raw)
		if err != nil {
			return identity.Principal{}, err
		}
		var claims struct {
			AuthTime      int64  `json:"auth_time"`
			Nonce         string `json:"nonce"`
			SessionID     string `json:"sid"`
			SessionState  string `json:"session_state"`
			PreferredName string `json:"preferred_username"`
		}
		if err = token.Claims(&claims); err != nil || token.Subject == "" || claims.AuthTime <= 0 || expectedNonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
			return identity.Principal{}, errors.New("credential OIDC claims are invalid")
		}
		sid := claims.SessionID
		if sid == "" {
			sid = claims.SessionState
		}
		if sid == "" {
			return identity.Principal{}, errors.New("credential OIDC session is missing")
		}
		return identity.Principal{Issuer: issuer, Subject: token.Subject, SessionID: sid, Username: claims.PreferredName, AuthenticatedAt: time.Unix(claims.AuthTime, 0).UTC()}, nil
	}, nil
}

type CredentialOriginConfig struct {
	PublicURL        *url.URL
	PortalOrigin     *url.URL
	IssuerURL        *url.URL
	AuthorizationURL *url.URL
	TokenURL         *url.URL
	JWKSURL          *url.URL
	ClientID         string
	Verify           CredentialOIDCVerifyFunc
	SecureCookies    bool
}

type CredentialOriginService struct {
	config       CredentialOriginConfig
	repository   *PostgresRepository
	validator    CredentialValidator
	providerURLs ProviderURLPolicy
	client       *http.Client
	now          func() time.Time
}

func (s *CredentialOriginService) origin() string {
	return strings.TrimRight(s.config.PublicURL.String(), "/")
}

func NewCredentialOriginService(config CredentialOriginConfig, repository *PostgresRepository, validator CredentialValidator, providerURLs ProviderURLPolicy) (*CredentialOriginService, error) {
	if config.PublicURL == nil || config.PortalOrigin == nil || config.IssuerURL == nil || config.AuthorizationURL == nil || config.TokenURL == nil || config.JWKSURL == nil || config.Verify == nil || strings.TrimSpace(config.ClientID) == "" || repository == nil || validator == nil {
		return nil, errors.New("invalid credential origin configuration")
	}
	if config.PublicURL.User != nil || config.PublicURL.RawQuery != "" || config.PublicURL.Fragment != "" || config.PublicURL.Path != "" && config.PublicURL.Path != "/" {
		return nil, errors.New("credential public URL must be an origin")
	}
	host := strings.ToLower(config.PublicURL.Hostname())
	if config.SecureCookies {
		if config.PublicURL.Scheme != "https" || config.PublicURL.Port() != "" || host != "credentials.pinvou.com" && host != "credentials-uat.pinvou.com" {
			return nil, errors.New("credential origin requires the fixed production or UAT HTTPS origin")
		}
	} else if config.PublicURL.Scheme != "http" || host != "127.0.0.1" && host != "localhost" {
		return nil, errors.New("development credential origin must use loopback HTTP")
	}
	if err := validateCredentialPortalOrigin(config); err != nil {
		return nil, err
	}
	if err := validateCredentialOIDCEndpoints(config); err != nil {
		return nil, err
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &CredentialOriginService{config: config, repository: repository, validator: validator, providerURLs: providerURLs, client: client, now: func() time.Time { return time.Now().UTC() }}, nil
}

func exactURLOrigin(value *url.URL) string {
	if value == nil {
		return ""
	}
	return strings.ToLower(value.Scheme + "://" + value.Host)
}

func credentialOriginHeaderMatches(expected *url.URL, raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && exactURLOrigin(parsed) == exactURLOrigin(expected)
}

func validCredentialEndpoint(value *url.URL, secure bool, allowRoot bool) bool {
	if value == nil || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return false
	}
	host := strings.ToLower(value.Hostname())
	if secure {
		if value.Scheme != "https" || value.Port() != "" {
			return false
		}
	} else if value.Scheme != "http" || host != "127.0.0.1" && host != "localhost" {
		return false
	}
	path := strings.ToLower(strings.TrimSpace(value.EscapedPath()))
	if strings.Contains(path, "userinfo") || !allowRoot && (path == "" || path == "/") {
		return false
	}
	return true
}

func validateCredentialPortalOrigin(config CredentialOriginConfig) error {
	if !validCredentialEndpoint(config.PortalOrigin, config.SecureCookies, true) || config.PortalOrigin.Path != "" && config.PortalOrigin.Path != "/" {
		return errors.New("credential launch requires one trusted Portal origin")
	}
	if config.SecureCookies {
		expectedHost := "account.pinvou.com"
		if strings.EqualFold(config.PublicURL.Hostname(), "credentials-uat.pinvou.com") {
			expectedHost = "account-uat.pinvou.com"
		}
		if !strings.EqualFold(config.PortalOrigin.Hostname(), expectedHost) {
			return errors.New("credential launch Portal origin does not match the deployment allowlist")
		}
	}
	return nil
}

func validateCredentialOIDCEndpoints(config CredentialOriginConfig) error {
	values := []*url.URL{config.IssuerURL, config.AuthorizationURL, config.TokenURL, config.JWKSURL}
	trustedOrigin := ""
	for index, value := range values {
		if !validCredentialEndpoint(value, config.SecureCookies, index == 0) {
			return errors.New("credential OIDC endpoints must use one trusted origin")
		}
		if index == 0 {
			trustedOrigin = exactURLOrigin(value)
		} else if exactURLOrigin(value) != trustedOrigin {
			return errors.New("credential OIDC endpoints must use one trusted origin")
		}
	}
	return nil
}

const credentialSessionCookie = "__Host-pinvou-credential-session"
const credentialBindingCookie = "__Host-pinvou-credential-binding"

func (s *CredentialOriginService) credentialSessionCookieName() string {
	if s.config.SecureCookies {
		return credentialSessionCookie
	}
	return "pinvou-dev-credential-session"
}

func (s *CredentialOriginService) credentialBindingCookieName() string {
	if s.config.SecureCookies {
		return credentialBindingCookie
	}
	return "pinvou-dev-credential-binding"
}

func (s *CredentialOriginService) Handler() http.Handler {
	router := httpserver.NewRouter()
	router.GET("/assets/credential.css", httpserver.AdaptFunc(s.stylesheet))
	router.GET("/assets/credential-auth.js", httpserver.AdaptFunc(s.authenticationScript))
	router.GET("/assets/credential-submit.js", httpserver.AdaptFunc(s.credentialSubmitScript))
	router.POST("/launch", httpserver.AdaptFunc(s.launch))
	router.GET("/session", httpserver.AdaptFunc(s.session))
	router.GET("/oidc/callback", httpserver.AdaptFunc(s.callback))
	router.POST("/credentials", httpserver.AdaptFunc(s.submitCredential))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		oidcOrigin := s.config.AuthorizationURL.Scheme + "://" + s.config.AuthorizationURL.Host
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' "+oidcOrigin+"; form-action 'self' "+oidcOrigin+"; frame-ancestors 'none'; base-uri 'none'")
		router.ServeHTTP(w, r)
	})
}

func (s *CredentialOriginService) stylesheet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = io.WriteString(w, credentialStylesheet)
}

func (s *CredentialOriginService) authenticationScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = io.WriteString(w, "const form=document.getElementById('credential-auth');if(form){form.requestSubmit()}else{const next=document.querySelector('meta[name=pinvou-credential-next]')?.content;if(next==='/session'){location.replace(next)}}")
}

func (s *CredentialOriginService) credentialSubmitScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = io.WriteString(w, `(()=>{'use strict';const form=document.getElementById('credential-form');const status=document.getElementById('credential-status');let active=false;form?.addEventListener('submit',async event=>{event.preventDefault();if(active)return;active=true;const button=form.querySelector('button[type=submit]');if(button)button.disabled=true;let payload=new URLSearchParams(new FormData(form));try{const response=await fetch('/credentials',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:payload,credentials:'same-origin',redirect:'follow',referrerPolicy:'no-referrer'});payload=null;const secret=form.querySelector('input[name=secret]');if(secret)secret.value='';if(!response.ok||new URL(response.url).origin!==location.origin||new URL(response.url).pathname!=='/session')throw new Error('CREDENTIAL_SUBMISSION_FAILED');location.replace('/session')}catch{payload=null;const secret=form.querySelector('input[name=secret]');if(secret)secret.value='';if(status)status.hidden=false;if(button)button.disabled=false;active=false}})})()`)
}

func (s *CredentialOriginService) setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	// Lax is required for the top-level OIDC authorization-code callback. The
	// session remains protected by the exact origin, state, nonce, PKCE, actor
	// and browser-binding checks; Strict would drop it on the IdP redirect.
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: s.config.SecureCookies, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}

func (s *CredentialOriginService) cookies(r *http.Request) (uuid.UUID, string, error) {
	sessionCookie, err := r.Cookie(s.credentialSessionCookieName())
	if err != nil {
		return uuid.Nil, "", ErrNotFound
	}
	bindingCookie, err := r.Cookie(s.credentialBindingCookieName())
	if err != nil {
		return uuid.Nil, "", ErrNotFound
	}
	sessionID, err := uuid.Parse(sessionCookie.Value)
	if err != nil || len(bindingCookie.Value) < 32 {
		return uuid.Nil, "", ErrNotFound
	}
	return sessionID, bindingCookie.Value, nil
}

func exactCredentialForm(r *http.Request, allowed ...string) error {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/x-www-form-urlencoded" {
		return ErrTokenCommandInvalid
	}
	if err := r.ParseForm(); err != nil {
		return err
	}
	allow := map[string]bool{}
	for _, key := range allowed {
		allow[key] = true
	}
	for key, values := range r.PostForm {
		if !allow[key] || len(values) != 1 {
			return ErrTokenCommandInvalid
		}
	}
	return nil
}

func (s *CredentialOriginService) launch(w http.ResponseWriter, r *http.Request) {
	if !credentialOriginHeaderMatches(s.config.PortalOrigin, r.Header.Get("Origin")) {
		api.WriteError(w, http.StatusForbidden, "CREDENTIAL_LAUNCH_ORIGIN_INVALID", "credential launch origin is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err := exactCredentialForm(r, "launch_id"); err != nil || len(r.PostForm) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_LAUNCH", "credential launch is invalid", httpserver.RequestID(r.Context()))
		return
	}
	launchID, err := uuid.Parse(r.PostForm.Get("launch_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_LAUNCH", "credential launch is invalid", httpserver.RequestID(r.Context()))
		return
	}
	binding, bindingErr := randomCredentialToken(32)
	state, stateErr := randomCredentialToken(32)
	nonce, nonceErr := randomCredentialToken(32)
	verifier, verifierErr := randomCredentialToken(48)
	csrf := credentialDigest(verifier, "csrf")
	if errors.Join(bindingErr, stateErr, nonceErr, verifierErr) != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "CREDENTIAL_SESSION_UNAVAILABLE", "credential session could not be created", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.repository.ConsumeCredentialLaunch(r.Context(), launchID, s.origin(), binding, state, nonce, verifier, csrf)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "CREDENTIAL_LAUNCH_INVALID", "credential launch is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	s.setCookie(w, s.credentialSessionCookieName(), session.ID.String(), session.ExpiresAt)
	s.setCookie(w, s.credentialBindingCookieName(), binding, session.ExpiresAt)
	http.Redirect(w, r, "/session", http.StatusSeeOther)
}

func (s *CredentialOriginService) session(w http.ResponseWriter, r *http.Request) {
	sessionID, binding, err := s.cookies(r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "CREDENTIAL_SESSION_REQUIRED", "credential session is required", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.repository.ConfirmCredentialSession(r.Context(), sessionID, binding)
	if err != nil || session.CredentialOrigin != s.origin() || !session.ExpiresAt.After(s.now()) || !secureCredentialValue(session.BrowserDigest, binding) {
		api.WriteError(w, http.StatusUnauthorized, "CREDENTIAL_SESSION_INVALID", "credential session is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if session.Status == "AUTHENTICATING" {
		challenge := sha256.Sum256([]byte(session.Verifier))
		query := s.config.AuthorizationURL.Query()
		query.Set("client_id", s.config.ClientID)
		query.Set("response_type", "code")
		query.Set("redirect_uri", s.config.PublicURL.ResolveReference(&url.URL{Path: "/oidc/callback"}).String())
		query.Set("scope", "openid")
		query.Set("state", session.StateDigest)
		query.Set("nonce", session.NonceDigest)
		query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		query.Set("code_challenge_method", "S256")
		query.Set("prompt", "login")
		query.Set("max_age", "900")
		fields := make([]credentialAuthorizationField, 0, len(query))
		for _, name := range []string{"client_id", "response_type", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "prompt", "max_age"} {
			fields = append(fields, credentialAuthorizationField{Name: name, Value: query.Get(name)})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		copy := credentialPageCopyFor(r.Header.Get("Accept-Language"))
		_ = credentialAuthorizationPage.Execute(w, struct {
			Action, Language, Title, Continue string
			Fields                            []credentialAuthorizationField
		}{Action: s.config.AuthorizationURL.String(), Language: copy.Language, Title: copy.Title, Continue: copy.Continue, Fields: fields})
		return
	}
	if session.Status == "COMPLETED" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = credentialSuccessPage.Execute(w, credentialPageCopyFor(r.Header.Get("Accept-Language")))
		return
	}
	if session.Status != "READY" {
		api.WriteError(w, http.StatusConflict, "CREDENTIAL_SESSION_TERMINAL", "credential session is no longer writable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	copy := credentialPageCopyFor(r.Header.Get("Accept-Language"))
	_ = credentialPage.Execute(w, struct {
		credentialPageCopy
		CSRF   string
		Action string
	}{copy, credentialDigest(session.Verifier, "csrf"), session.Action})
}

type credentialPageCopy struct {
	Language, Title, Lead, ActionLabel, Provider, BaseURL, AllowedModels, Secret, Submit, Success, Close, Failure, Continue string
}

func credentialPageCopyFor(acceptLanguage string) credentialPageCopy {
	language := strings.ToLower(strings.TrimSpace(strings.Split(acceptLanguage, ",")[0]))
	if strings.HasPrefix(language, "zh") {
		return credentialPageCopy{"zh-CN", "凭据安全接入", "Secret 只会提交到此隔离安全域，不会返回管理后台。", "操作", "凭据类型", "Provider 基础地址", "允许模型（逗号分隔）", "Secret", "安全提交", "凭据已安全更新", "可以关闭此窗口。", "凭据提交失败，请安全重试。", "继续认证"}
	}
	if strings.HasPrefix(language, "ja") {
		return credentialPageCopy{"ja", "認証情報の安全な登録", "Secret はこの隔離された安全ドメインにのみ送信され、管理ポータルには返りません。", "操作", "認証情報種別", "Provider ベース URL", "許可モデル（カンマ区切り）", "Secret", "安全に送信", "認証情報を安全に更新しました", "このウィンドウを閉じることができます。", "認証情報を送信できませんでした。安全に再試行してください。", "認証を続行"}
	}
	return credentialPageCopy{"en", "Secure credential enrollment", "The secret is submitted only to this isolated security origin and is never returned to the admin portal.", "Action", "Credential type", "Provider base URL", "Allowed models (comma-separated)", "Secret", "Submit securely", "Credential updated securely", "You may close this window.", "Credential submission failed. Retry securely.", "Continue authentication"}
}

var credentialPage = template.Must(template.New("credential").Parse(`<!doctype html><html lang="{{.Language}}"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><link rel="stylesheet" href="/assets/credential.css"><script defer src="/assets/credential-submit.js"></script></head><body><main><h1>{{.Title}}</h1><p>{{.Lead}}</p><p><strong>{{.ActionLabel}}:</strong> {{.Action}}</p><form id="credential-form" method="post" action="/credentials" autocomplete="off"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>{{.Provider}}<input name="provider" required maxlength="64"></label><label>{{.BaseURL}}<input name="base_url" type="url" required></label><label>{{.AllowedModels}}<input name="allowed_models" required></label><label>{{.Secret}}<input name="secret" type="password" required minlength="8" autocomplete="new-password"></label><button type="submit">{{.Submit}}</button><p id="credential-status" role="alert" hidden>{{.Failure}}</p></form></main></body></html>`))

type credentialAuthorizationField struct{ Name, Value string }

var credentialAuthorizationPage = template.Must(template.New("credential-auth").Parse(`<!doctype html><html lang="{{.Language}}"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><script defer src="/assets/credential-auth.js"></script></head><body><form id="credential-auth" method="get" action="{{.Action}}">{{range .Fields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}<noscript><button type="submit">{{.Continue}}</button></noscript></form></body></html>`))

var credentialAuthenticationCompletePage = template.Must(template.New("credential-auth-complete").Parse(`<!doctype html><html lang="{{.Language}}"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta name="pinvou-credential-next" content="/session"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><script defer src="/assets/credential-auth.js"></script></head><body><noscript><a href="/session">{{.Continue}}</a></noscript></body></html>`))

var credentialSuccessPage = template.Must(template.New("credential-success").Parse(`<!doctype html><html lang="{{.Language}}"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Success}}</title><link rel="stylesheet" href="/assets/credential.css"></head><body><main><h1>{{.Success}}</h1><p>{{.Close}}</p></main></body></html>`))

const credentialStylesheet = `:root{font-family:Inter,system-ui,sans-serif;color:#171b25;background:#f4f6fa}*{box-sizing:border-box}body{margin:0;padding:clamp(1rem,5vw,3rem)}main{width:min(34rem,100%);margin:0 auto;padding:clamp(1rem,4vw,2rem);background:#fff;border:1px solid #dce2ed;border-radius:1rem;box-shadow:0 .5rem 1.5rem rgba(24,35,55,.08)}h1{font-size:clamp(1.4rem,6vw,2rem);line-height:1.2;margin:0 0 1rem}p{line-height:1.6;color:#526079}form{display:grid;gap:1rem;margin-top:1.5rem}label{display:grid;gap:.4rem;font-weight:600}input,button{min-height:2.75rem;border-radius:.55rem;font:inherit}input{width:100%;padding:.65rem .75rem;border:1px solid #aeb8ca}input:focus-visible,button:focus-visible{outline:3px solid #8fa8ff;outline-offset:2px}button{border:0;padding:.7rem 1rem;color:#fff;background:#315bea;font-weight:700;cursor:pointer}@media(max-width:360px){body{padding:.5rem}main{padding:1rem;border-radius:.7rem}}`

type credentialTokenResponse struct {
	IDToken   string `json:"id_token"`
	TokenType string `json:"token_type"`
}

func (s *CredentialOriginService) callback(w http.ResponseWriter, r *http.Request) {
	sessionID, binding, err := s.cookies(r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "CREDENTIAL_SESSION_REQUIRED", "credential session is required", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.repository.CredentialIsolationSession(r.Context(), sessionID)
	if err != nil || session.CredentialOrigin != s.origin() || session.Status != "AUTHENTICATING" || !secureCredentialValue(session.BrowserDigest, binding) {
		api.WriteError(w, http.StatusUnauthorized, "CREDENTIAL_SESSION_INVALID", "credential session is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err = s.repository.CheckCredentialIsolationSessionAuthority(r.Context(), sessionID); err != nil {
		api.WriteError(w, http.StatusForbidden, "CREDENTIAL_AUTHORIZATION_CHANGED", "credential authorization changed", httpserver.RequestID(r.Context()))
		return
	}
	state, code := strings.TrimSpace(r.URL.Query().Get("state")), strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" || subtle.ConstantTimeCompare([]byte(state), []byte(session.StateDigest)) != 1 {
		api.WriteError(w, http.StatusBadRequest, "CREDENTIAL_OIDC_STATE_INVALID", "credential authentication state is invalid", httpserver.RequestID(r.Context()))
		return
	}
	values := url.Values{"grant_type": {"authorization_code"}, "client_id": {s.config.ClientID}, "code": {code}, "redirect_uri": {s.config.PublicURL.ResolveReference(&url.URL{Path: "/oidc/callback"}).String()}, "code_verifier": {session.Verifier}}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.config.TokenURL.String(), strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "CREDENTIAL_OIDC_EXCHANGE_FAILED", "credential authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	if response.Request == nil || exactURLOrigin(response.Request.URL) != exactURLOrigin(s.config.TokenURL) || response.StatusCode == http.StatusTemporaryRedirect || response.StatusCode == http.StatusPermanentRedirect {
		api.WriteError(w, http.StatusBadGateway, "CREDENTIAL_OIDC_EXCHANGE_FAILED", "credential authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode/100 != 2 {
		api.WriteError(w, http.StatusBadGateway, "CREDENTIAL_OIDC_EXCHANGE_FAILED", "credential authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	var tokens credentialTokenResponse
	if json.Unmarshal(body, &tokens) != nil || tokens.IDToken == "" || !strings.EqualFold(tokens.TokenType, "Bearer") {
		api.WriteError(w, http.StatusBadGateway, "CREDENTIAL_OIDC_EXCHANGE_FAILED", "credential authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	principal, err := s.config.Verify(r.Context(), tokens.IDToken, session.NonceDigest)
	if err != nil || principal.Issuer != session.ActorIssuer || principal.Subject != session.ActorSubject {
		api.WriteError(w, http.StatusForbidden, "CREDENTIAL_SUBJECT_MISMATCH", "credential authentication subject does not match", httpserver.RequestID(r.Context()))
		return
	}
	if err = s.repository.AuthenticateCredentialSession(r.Context(), sessionID, session.ActorID, principal.AuthenticatedAt); err != nil {
		api.WriteError(w, http.StatusForbidden, "RECENT_AUTH_REQUIRED", "recent authentication is required", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = credentialAuthenticationCompletePage.Execute(w, credentialPageCopyFor(r.Header.Get("Accept-Language")))
}

func (s *CredentialOriginService) submitCredential(w http.ResponseWriter, r *http.Request) {
	if err := exactCredentialForm(r, "csrf", "provider", "base_url", "allowed_models", "secret"); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT", "credential input is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/"); origin != strings.TrimRight(s.config.PublicURL.String(), "/") {
		api.WriteError(w, http.StatusForbidden, "CREDENTIAL_ORIGIN_INVALID", "credential origin is invalid", httpserver.RequestID(r.Context()))
		return
	}
	sessionID, binding, err := s.cookies(r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "CREDENTIAL_SESSION_REQUIRED", "credential session is required", httpserver.RequestID(r.Context()))
		return
	}
	secret := []byte(r.PostForm.Get("secret"))
	r.PostForm.Set("secret", "")
	r.Form.Set("secret", "")
	_, err = s.repository.CompleteCredentialSession(r.Context(), sessionID, s.origin(), binding, r.PostForm.Get("csrf"), r.PostForm.Get("provider"), r.PostForm.Get("base_url"), strings.Split(r.PostForm.Get("allowed_models"), ","), secret, s.validator, s.providerURLs)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "CREDENTIAL_SUBMISSION_REJECTED", "credential submission was rejected", httpserver.RequestID(r.Context()))
		return
	}
	http.Redirect(w, r, "/session", http.StatusSeeOther)
}
