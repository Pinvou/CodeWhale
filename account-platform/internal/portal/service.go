package portal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type Config struct {
	PublicURL         *url.URL
	AuthorizationURL  *url.URL
	TokenURL          *url.URL
	ClientID          string
	AccountURL        *url.URL
	BillingURL        *url.URL
	TokenGatewayURL   *url.URL
	KeycloakStatusURL *url.URL
	NewAPIStatusURL   *url.URL
	NATSStatusURL     *url.URL
	RelayURLs         map[string]*url.URL
	SecureCookies     bool
	SessionMaxAge     time.Duration
}

const defaultSessionMaxAge = 30 * 24 * time.Hour

type Service struct {
	config Config
	store  Store
	client *http.Client
	now    func() time.Time
}

func NewService(config Config, store Store) (*Service, error) {
	if config.PublicURL == nil || config.AuthorizationURL == nil || config.TokenURL == nil || config.AccountURL == nil || config.BillingURL == nil || config.TokenGatewayURL == nil || strings.TrimSpace(config.ClientID) == "" || store == nil {
		return nil, errors.New("invalid portal configuration")
	}
	if config.PublicURL.Scheme != "https" && config.SecureCookies {
		return nil, errors.New("secure portal cookies require an HTTPS public URL")
	}
	if config.SessionMaxAge <= 0 {
		config.SessionMaxAge = defaultSessionMaxAge
	}
	return &Service{config: config, store: store, client: observability.NewHTTPClient(15 * time.Second), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Handler(static http.Handler) http.Handler {
	router := httpserver.NewRouter()
	router.GET("/auth/login", httpserver.AdaptFunc(s.login))
	router.GET("/auth/callback", httpserver.AdaptFunc(s.callback))
	router.GET("/api/session", httpserver.AdaptFunc(s.sessionStatus))
	router.POST("/api/logout", httpserver.AdaptFunc(s.logout))
	router.Any("/api/account/*resource", httpserver.AdaptFunc(s.proxyAccount))
	router.Any("/api/token/*resource", httpserver.AdaptFunc(s.proxyToken))
	router.Any("/api/admin/account/*resource", httpserver.AdaptFunc(s.proxyAdminAccount))
	router.Any("/api/admin/billing/*resource", httpserver.AdaptFunc(s.proxyAdminBilling))
	router.Any("/api/admin/token/*resource", httpserver.AdaptFunc(s.proxyAdminToken))
	router.GET("/api/admin/system/status", httpserver.AdaptFunc(s.adminSystemStatus))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.config.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || static == nil {
			router.ServeHTTP(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken(32)
	verifier, verifierErr := randomToken(48)
	nonce, nonceErr := randomToken(32)
	err = errors.Join(err, verifierErr, nonceErr)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, "LOGIN_UNAVAILABLE", "login could not be started", httpserver.RequestID(r.Context()))
		return
	}
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	flow := LoginFlow{Verifier: verifier, Nonce: nonce, ReturnTo: returnTo, ExpiresAt: s.now().Add(10 * time.Minute)}
	if err := s.store.SaveFlow(r.Context(), state, flow); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_UNAVAILABLE", "login could not be started", httpserver.RequestID(r.Context()))
		return
	}
	s.setCookie(w, s.flowCookieName(), state, flow.ExpiresAt, true)
	challenge := sha256.Sum256([]byte(verifier))
	query := s.config.AuthorizationURL.Query()
	query.Set("client_id", s.config.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", s.callbackURL())
	query.Set("scope", "openid profile email offline_access")
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	redirect := *s.config.AuthorizationURL
	redirect.RawQuery = query.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	cookie, err := r.Cookie(s.flowCookieName())
	if err != nil || state == "" || code == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		api.WriteError(w, http.StatusBadRequest, "LOGIN_STATE_INVALID", "login state is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	flow, err := s.store.TakeFlow(r.Context(), state)
	if err != nil || !flow.ExpiresAt.After(s.now()) {
		api.WriteError(w, http.StatusBadRequest, "LOGIN_STATE_INVALID", "login state is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	tokens, err := s.exchange(r.Context(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.config.ClientID},
		"code":          {code},
		"redirect_uri":  {s.callbackURL()},
		"code_verifier": {flow.Verifier},
	})
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_EXCHANGE_FAILED", "login could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	now := s.now()
	sessionID, sessionIDErr := randomToken(32)
	csrfToken, csrfErr := randomToken(32)
	session := Session{ID: sessionID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, AccessExpiresAt: now.Add(time.Duration(tokens.ExpiresIn) * time.Second), RefreshExpiresAt: s.refreshExpiry(now, tokens.RefreshExpiresIn, now.Add(s.config.SessionMaxAge)), CSRFToken: csrfToken, CreatedAt: now}
	if sessionIDErr != nil || csrfErr != nil || session.AccessToken == "" || session.RefreshToken == "" || tokens.ExpiresIn <= 0 || !strings.EqualFold(tokens.TokenType, "Bearer") || s.store.SaveSession(r.Context(), session) != nil {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_SESSION_FAILED", "login session could not be created", httpserver.RequestID(r.Context()))
		return
	}
	s.expireCookie(w, s.flowCookieName(), true)
	s.setCookie(w, s.sessionCookieName(), session.ID, session.RefreshExpiresAt, true)
	s.setCookie(w, s.csrfCookieName(), session.CSRFToken, session.RefreshExpiresAt, false)
	http.Redirect(w, r, flow.ReturnTo, http.StatusFound)
}

func (s *Service) sessionStatus(w http.ResponseWriter, r *http.Request) {
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": session.CSRFToken})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil || !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	_ = s.store.DeleteSession(r.Context(), session.ID)
	s.expireCookie(w, s.sessionCookieName(), true)
	s.expireCookie(w, s.csrfCookieName(), false)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) proxyAccount(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.AccountURL, "/"+r.PathValue("resource"), accountPathAllowed)
}

func (s *Service) proxyToken(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenPathAllowed)
}

func (s *Service) proxyAdminAccount(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.AccountURL, "/"+r.PathValue("resource"), accountAdminPathAllowed)
}

func (s *Service) proxyAdminBilling(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.BillingURL, "/"+r.PathValue("resource"), billingAdminPathAllowed)
}

func (s *Service) proxyAdminToken(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenAdminPathAllowed)
}

func (s *Service) proxy(w http.ResponseWriter, r *http.Request, upstream *url.URL, resource string, allowed func(string, string) bool) {
	resource = path.Clean(resource)
	if !allowed(r.Method, resource) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	target := *upstream
	target.Path = strings.TrimRight(target.Path, "/") + resource
	target.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), io.LimitReader(r.Body, 4<<20))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Pinvou-Client-Source", portalClientSource(r))
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := s.client.Do(request)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	if contextVersion := response.Header.Get("X-Context-Version"); contextVersion != "" {
		w.Header().Set("X-Context-Version", contextVersion)
	}
	if authorizationVersion := response.Header.Get("X-Authorization-Version"); authorizationVersion != "" {
		w.Header().Set("X-Authorization-Version", authorizationVersion)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 32<<20))
}

func (s *Service) authenticatedSession(ctx context.Context, r *http.Request) (Session, error) {
	cookie, err := r.Cookie(s.sessionCookieName())
	if err != nil || cookie.Value == "" {
		return Session{}, ErrSessionNotFound
	}
	session, err := s.store.GetSession(ctx, cookie.Value)
	if err != nil || !session.RefreshExpiresAt.After(s.now()) {
		return Session{}, ErrSessionNotFound
	}
	if session.AccessExpiresAt.After(s.now().Add(30 * time.Second)) {
		return session, nil
	}
	return s.refreshSession(ctx, session)
}

func (s *Service) refreshSession(ctx context.Context, session Session) (Session, error) {
	deadline := s.now().Add(5 * time.Second)
	for {
		locked, err := s.store.TryRefreshLock(ctx, session.ID)
		if err != nil {
			return Session{}, err
		}
		if locked {
			defer s.store.ReleaseRefreshLock(context.WithoutCancel(ctx), session.ID)
			latest, err := s.store.GetSession(ctx, session.ID)
			if err != nil || latest.AccessExpiresAt.After(s.now().Add(30*time.Second)) {
				return latest, err
			}
			tokens, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {s.config.ClientID}, "refresh_token": {latest.RefreshToken}})
			if err != nil {
				_ = s.store.DeleteSession(ctx, session.ID)
				return Session{}, err
			}
			now := s.now()
			latest.AccessToken = tokens.AccessToken
			latest.AccessExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
			if tokens.RefreshToken != "" {
				latest.RefreshToken = tokens.RefreshToken
			}
			if tokens.RefreshExpiresIn > 0 {
				latest.RefreshExpiresAt = s.refreshExpiry(now, tokens.RefreshExpiresIn, latest.CreatedAt.Add(s.config.SessionMaxAge))
			}
			return latest, s.store.SaveSession(ctx, latest)
		}
		if !deadline.After(s.now()) {
			return Session{}, errors.New("session refresh timed out")
		}
		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			session, err = s.store.GetSession(ctx, session.ID)
			if err == nil && session.AccessExpiresAt.After(s.now().Add(30*time.Second)) {
				return session, nil
			}
		}
	}
}

func (s *Service) refreshExpiry(now time.Time, seconds int64, limit time.Time) time.Time {
	if seconds <= 0 || seconds >= int64(s.config.SessionMaxAge/time.Second) {
		return limit
	}
	candidate := now.Add(time.Duration(seconds) * time.Second)
	if candidate.After(limit) {
		return limit
	}
	return candidate
}

func (s *Service) exchange(ctx context.Context, form url.Values) (TokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.TokenURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		return TokenResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return TokenResponse{}, fmt.Errorf("OIDC token endpoint returned HTTP %d", response.StatusCode)
	}
	var tokens TokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tokens); err != nil {
		return TokenResponse{}, err
	}
	return tokens, nil
}

func (s *Service) validMutation(r *http.Request, session Session) bool {
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	expected := strings.TrimRight(s.config.PublicURL.Scheme+"://"+s.config.PublicURL.Host, "/")
	csrfCookie, err := r.Cookie(s.csrfCookieName())
	header := r.Header.Get("X-CSRF-Token")
	return err == nil && origin == expected && len(header) >= 32 && subtle.ConstantTimeCompare([]byte(header), []byte(csrfCookie.Value)) == 1 && subtle.ConstantTimeCompare([]byte(header), []byte(session.CSRFToken)) == 1
}

func (s *Service) callbackURL() string {
	result := *s.config.PublicURL
	result.Path = strings.TrimRight(result.Path, "/") + "/auth/callback"
	result.RawQuery, result.Fragment = "", ""
	return result.String()
}

func (s *Service) sessionCookieName() string {
	return cookieName(s.config.SecureCookies, "portal_session")
}
func (s *Service) flowCookieName() string { return cookieName(s.config.SecureCookies, "portal_flow") }
func (s *Service) csrfCookieName() string { return cookieName(s.config.SecureCookies, "portal_csrf") }

func cookieName(secure bool, name string) string {
	if secure {
		return "__Host-" + name
	}
	return name
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, expires time.Time, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Expires: expires, MaxAge: max(1, int(time.Until(expires).Seconds())), Secure: s.config.SecureCookies, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode})
}

func (s *Service) expireCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", Expires: time.Unix(1, 0), MaxAge: -1, Secure: s.config.SecureCookies, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode})
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func safeReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\r\n") {
		return "/"
	}
	return value
}

func accountPathAllowed(method, resource string) bool {
	if !strings.HasPrefix(resource, "/v1/") || strings.Contains(resource, "..") {
		return false
	}
	return method == http.MethodGet || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func tokenPathAllowed(method, resource string) bool {
	if method == http.MethodGet {
		switch resource {
		case "/v1/recharge/products", "/v1/recharge/orders", "/v1/recharge/orders/recent", "/v1/me/balance", "/v1/me/entitlement-packages", "/v1/me/usage-requests":
			return true
		}
		if orderID, ok := strings.CutPrefix(resource, "/v1/recharge/orders/"); ok {
			return safeProxyPathID(orderID)
		}
	}
	if method == http.MethodPost && resource == "/v1/recharge/orders" {
		return true
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "organizations" || !safeProxyPathID(parts[2]) {
		return false
	}
	if len(parts) == 4 && parts[3] == "quota-grants" {
		return method == http.MethodGet
	}
	if len(parts) == 4 && parts[3] == "quota-grants:batch" {
		return method == http.MethodPost
	}
	if len(parts) == 5 && parts[3] == "recharge" && parts[4] == "orders" {
		return method == http.MethodPost
	}
	return len(parts) == 6 && parts[3] == "quota-grants" && safeProxyPathID(parts[4]) && parts[5] == "revoke" && method == http.MethodPost
}

func safeProxyPathID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.Contains(value, "/") && strings.IndexFunc(value, func(char rune) bool {
		return !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_')
	}) == -1
}

func portalClientSource(r *http.Request) string {
	if source := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(source) != nil {
		return source
	}
	if forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ","); len(forwarded) > 0 {
		if source := strings.TrimSpace(forwarded[0]); net.ParseIP(source) != nil {
			return source
		}
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func accountAdminPathAllowed(method, resource string) bool {
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/roles", "/admin/v1/role-bindings", "/admin/v1/users", "/admin/v1/organizations", "/admin/v1/audit-logs", "/admin/v1/registration-operations", "/admin/v1/approval-policies", "/admin/v1/approval-requests") {
		return true
	}
	if method == http.MethodPost && resource == "/admin/v1/organizations" {
		return true
	}
	if method == http.MethodPost && resource == "/admin/v1/role-bindings" {
		return true
	}
	if resource == "/admin/v1/approval-requests" && method == http.MethodPost {
		return true
	}
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) >= 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "approval-requests" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		if len(parts) == 4 {
			return method == http.MethodGet || method == http.MethodPut
		}
		return len(parts) == 6 && parts[4] == "actions" && (parts[5] == "submit" || parts[5] == "withdraw" || parts[5] == "decide") && method == http.MethodPost
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "role-bindings" && method == http.MethodDelete {
		_, err := uuid.Parse(parts[3])
		return err == nil
	}
	if len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "registration-operations" && parts[4] == "actions" && (parts[5] == "retry" || parts[5] == "compensate") {
		_, err := uuid.Parse(parts[3])
		return err == nil && method == http.MethodPost
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "user-code-lookups" {
		return method == http.MethodGet && safeProxyPathID(parts[3])
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "audit-logs" && method == http.MethodGet {
		_, err := uuid.Parse(parts[3])
		return err == nil
	}
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" || (parts[2] != "users" && parts[2] != "organizations") {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	if method == http.MethodDelete && len(parts) == 4 {
		return true
	}
	if parts[2] == "users" && len(parts) == 5 && parts[4] == "memberships" {
		return method == http.MethodGet
	}
	if method == http.MethodPatch && len(parts) == 4 && parts[2] == "organizations" {
		return true
	}
	if method == http.MethodPatch && len(parts) == 5 && parts[4] == "status" {
		return true
	}
	if parts[2] == "organizations" && len(parts) == 5 && parts[4] == "members" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if parts[2] == "organizations" && len(parts) == 7 && parts[4] == "members" {
		if _, err := uuid.Parse(parts[5]); err != nil || parts[6] != "roles" {
			return false
		}
		return method == http.MethodPut
	}
	if parts[2] == "organizations" && len(parts) == 6 && parts[4] == "members" {
		if _, err := uuid.Parse(parts[5]); err != nil {
			return false
		}
		return method == http.MethodDelete
	}
	if parts[2] == "organizations" && len(parts) == 5 && parts[4] == "ownership" {
		return method == http.MethodPost
	}
	if parts[2] != "organizations" || len(parts) < 5 || parts[4] != "units" {
		return false
	}
	if len(parts) == 5 {
		return method == http.MethodGet || method == http.MethodPost
	}
	if len(parts) == 6 {
		if _, err := uuid.Parse(parts[5]); err != nil {
			return false
		}
		return method == http.MethodPatch || method == http.MethodDelete
	}
	return false
}

func billingAdminPathAllowed(method, resource string) bool {
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/wallets", "/admin/v1/receivables", "/admin/v1/pricing-versions", "/admin/v1/payment-orders", "/admin/v1/adjustments", "/admin/v1/usage-holds", "/admin/v1/reconciliation-runs") {
		return true
	}
	if (resource == "/admin/v1/pricing-versions" || resource == "/admin/v1/reconciliation-runs" || resource == "/admin/v1/adjustments") && method == http.MethodPost {
		return true
	}
	if (resource == "/admin/v1/pricing" || resource == "/admin/v1/metering-units") && method == http.MethodGet || (resource == "/admin/v1/pricing/drafts" || resource == "/admin/v1/metering-units") && method == http.MethodPost {
		return true
	}
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "usage-holds" {
		_, err := uuid.Parse(parts[3])
		return err == nil && method == http.MethodGet
	}
	if len(parts) != 5 || parts[0] != "admin" || parts[1] != "v1" {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	switch parts[2] {
	case "pricing":
		return (parts[4] == "publish" || parts[4] == "transition") && method == http.MethodPost || parts[4] == "draft" && method == http.MethodPut || parts[4] == "preview" && method == http.MethodGet
	case "metering-units":
		return parts[4] == "deactivate" && method == http.MethodPost
	case "wallets":
		return method == http.MethodGet && parts[4] == "ledger"
	case "receivables":
		return method == http.MethodGet && parts[4] == "allocations"
	case "payment-orders":
		return method == http.MethodPost && (parts[4] == "sync" || parts[4] == "repair" || parts[4] == "refunds")
	case "adjustments":
		return method == http.MethodPost && (parts[4] == "approve" || parts[4] == "reject")
	case "usage-holds":
		return method == http.MethodPost && parts[4] == "release"
	case "reconciliation-runs":
		return method == http.MethodGet && parts[4] == "cases"
	default:
		return false
	}
}

func tokenAdminPathAllowed(method, resource string) bool {
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/token-accounts", "/admin/v1/usage-requests") {
		return true
	}
	if strings.Contains(resource, "..") {
		return false
	}
	if resource == "/admin/v1/protocols" {
		return method == http.MethodGet || method == http.MethodPost
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	if parts[2] == "usage-requests" {
		return (len(parts) == 4 && method == http.MethodGet) ||
			(len(parts) == 5 && parts[4] == "retry-reconciliation" && method == http.MethodPost)
	}
	if parts[2] == "protocols" {
		return len(parts) == 5 && ((parts[4] == "publish" || parts[4] == "formulas") && method == http.MethodPost)
	}
	if parts[2] == "formulas" {
		return len(parts) == 5 && (parts[4] == "publish" && method == http.MethodPost || parts[4] == "preview" && method == http.MethodGet)
	}
	if parts[2] != "token-accounts" {
		return false
	}
	if len(parts) == 4 {
		return method == http.MethodGet
	}
	if len(parts) == 5 {
		switch parts[4] {
		case "status":
			return method == http.MethodPatch
		case "routes":
			return method == http.MethodPut || method == http.MethodDelete
		}
	}
	if len(parts) == 7 && parts[4] == "credentials" && parts[6] == "validate" && method == http.MethodPost {
		_, err := uuid.Parse(parts[5])
		return err == nil
	}
	return false
}

func adminReadPathAllowed(method, resource string, allowed ...string) bool {
	if method != http.MethodGet || strings.Contains(resource, "..") {
		return false
	}
	for _, prefix := range allowed {
		if resource == prefix {
			return true
		}
	}
	return false
}
