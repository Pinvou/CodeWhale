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
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

type Config struct {
	Environment                string
	IssuerURL                  *url.URL
	JWKSURL                    *url.URL
	PublicURL                  *url.URL
	AuthorizationURL           *url.URL
	TokenURL                   *url.URL
	AccountURL                 *url.URL
	TokenGatewayURL            *url.URL
	BillingURL                 *url.URL
	ClientID                   string
	HandoffClientID            string
	HandoffClientSecret        string
	PrivateCAFile              string
	HandoffSubjectTokenEnabled bool
	SecureCookies              bool
	SessionHandoffTTL          time.Duration
	TrustedProxyIPs            []netip.Addr
}
type Service struct {
	config          Config
	store           Store
	client          *http.Client
	now             func() time.Time
	verifier        *oidc.IDTokenVerifier
	trustedProxyIPs map[netip.Addr]struct{}
}
type oidcTokenEndpointError struct {
	StatusCode int
	Code       string
}

func (e *oidcTokenEndpointError) Error() string {
	return fmt.Sprintf("OIDC token endpoint returned HTTP %d", e.StatusCode)
}

// Logout retention fences outstanding ten-minute login flows; it does not cap
// provider-issued session lifetimes. Existing lineage TTLs also extend the Redis marker.
const loginFlowLifetime = 10 * time.Minute

func (s *Service) logoutMarkerExpiry(session Session) time.Time {
	expiry := s.now().Add(loginFlowLifetime)
	if session.RefreshExpiresAt.After(expiry) {
		expiry = session.RefreshExpiresAt
	}
	return expiry
}
func NewService(config Config, store Store) (*Service, error) {
	trustedProxies, err := trustedProxySet(config.TrustedProxyIPs)
	if err != nil {
		return nil, err
	}
	config.ClientID = strings.TrimSpace(config.ClientID)
	if config.ClientID == "" || store == nil {
		return nil, errors.New("invalid portal configuration")
	}
	development := config.Environment == "development" || config.Environment == "test"
	for _, endpoint := range []*url.URL{config.PublicURL, config.AuthorizationURL, config.TokenURL, config.AccountURL, config.TokenGatewayURL, config.BillingURL, config.IssuerURL, config.JWKSURL} {
		if endpoint == nil || endpoint.Hostname() == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" {
			return nil, errors.New("portal endpoints must be absolute HTTP(S) URLs without credentials, query or fragments")
		}
	}
	if config.PublicURL.Path != "" && config.PublicURL.Path != "/" {
		return nil, errors.New("portal public URL must be an origin")
	}
	if config.SecureCookies && config.PublicURL.Scheme != "https" {
		return nil, errors.New("secure portal cookies require HTTPS")
	}
	if !development {
		if !config.SecureCookies {
			return nil, errors.New("portal cookies must be secure outside development and test")
		}
		for _, endpoint := range []*url.URL{config.AuthorizationURL, config.TokenURL, config.IssuerURL, config.JWKSURL} {
			if endpoint.Scheme != "https" {
				return nil, errors.New("portal OIDC endpoints require HTTPS")
			}
		}
	}
	if config.SessionHandoffTTL == 0 && !config.HandoffSubjectTokenEnabled {
		config.SessionHandoffTTL = 30 * time.Second
	}
	if config.SessionHandoffTTL <= 0 || config.SessionHandoffTTL > time.Minute {
		return nil, errors.New("session handoff TTL must be positive and at most one minute")
	}
	config.HandoffClientID = strings.TrimSpace(config.HandoffClientID)
	if config.HandoffSubjectTokenEnabled && (config.HandoffClientID == "" || config.HandoffClientID == config.ClientID || strings.TrimSpace(config.HandoffClientSecret) == "") {
		return nil, errors.New("session handoff requires a dedicated confidential client")
	}
	if strings.TrimSpace(config.PrivateCAFile) != "" && config.TokenURL.Scheme != "https" {
		return nil, errors.New("portal private CA requires HTTPS token transport")
	}
	client, err := newPortalHTTPClient(config.PrivateCAFile)
	if err != nil {
		return nil, err
	}
	keyContext := oidc.ClientContext(context.Background(), client)
	verifier := oidc.NewVerifier(strings.TrimRight(config.IssuerURL.String(), "/"), oidc.NewRemoteKeySet(keyContext, config.JWKSURL.String()), &oidc.Config{ClientID: config.ClientID})
	return &Service{config: config, store: store, client: client, verifier: verifier, trustedProxyIPs: trustedProxies, now: func() time.Time { return time.Now().UTC() }}, nil
}
func (s *Service) Handler(static http.Handler) http.Handler {
	router := httpserver.NewRouter()
	router.GET("/auth/login", httpserver.AdaptFunc(s.login))
	router.GET("/auth/callback", httpserver.AdaptFunc(s.callback))
	router.POST("/portal-session-handoff", httpserver.AdaptFunc(s.createSessionHandoff))
	router.POST("/portal-session-handoff/consume", httpserver.AdaptFunc(s.consumeSessionHandoff))
	router.GET("/health/session-lineage", httpserver.AdaptFunc(s.sessionLineageStatus))
	router.GET("/portal-session-handoff", httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		if static == nil {
			http.NotFound(w, r)
			return
		}
		copy := r.Clone(r.Context())
		u := *copy.URL
		u.Path = "/portal-session-handoff.html"
		copy.URL = &u
		static.ServeHTTP(w, copy)
	}))
	router.GET("/api/session", httpserver.AdaptFunc(s.sessionStatus))
	router.POST("/api/logout", httpserver.AdaptFunc(s.logout))
	router.Any("/api/account/*resource", httpserver.AdaptFunc(s.proxyAccount))
	router.Any("/api/token/*resource", httpserver.AdaptFunc(s.proxyToken))
	router.Any("/api/admin/account/*resource", httpserver.AdaptFunc(s.proxyAdminAccount))
	router.Any("/api/admin/billing/*resource", httpserver.AdaptFunc(s.proxyAdminBilling))
	router.GET("/api/admin/approvals/capability", httpserver.AdaptFunc(s.adminApprovalCapability))
	router.GET("/api/admin/approvals/directory", httpserver.AdaptFunc(s.adminApprovalDirectory))
	router.GET("/api/admin/approvals/requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	router.POST("/api/admin/approvals/requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	router.Any("/api/admin/approvals/requests/*resource", httpserver.AdaptFunc(s.adminApprovalRequests))
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
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/portal-session-handoff") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/health/") || strings.HasPrefix(r.URL.Path, "/portal-session-handoff") || static == nil {
			router.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if _, err := s.authenticatedSession(r.Context(), r); errors.Is(err, ErrSessionNotFound) {
				copy := r.Clone(r.Context())
				u := *r.URL
				q := u.Query()
				q.Set("return_to", safeReturnTo(r.URL.RequestURI()))
				u.RawQuery = q.Encode()
				copy.URL = &u
				s.login(w, copy)
				return
			} else if err != nil {
				s.writeSessionError(w, r, err)
				return
			}
		}
		static.ServeHTTP(w, r)
	})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	reauthenticate := r.URL.Query().Get("reauthenticate") == "1"
	forceLoginRequested := r.URL.Query().Get("force_login") == "1"
	loggedOutCookie, loggedOutErr := r.Cookie(s.loggedOutCookieName())
	loggedOut := loggedOutErr == nil && loggedOutCookie.Value == "1"
	forceLogin := forceLoginRequested || loggedOut
	locale := portalLocale(r.URL.Query().Get("locale"))
	previousSession := ""
	lineageRoot := ""
	if cookie, cookieErr := r.Cookie(s.sessionCookieName()); cookieErr == nil {
		previousSession = strings.TrimSpace(cookie.Value)
		if previousSession != "" {
			resolvedRoot, previousErr := s.store.ResolveSessionLineageRoot(r.Context(), previousSession)
			switch {
			case previousErr == nil:
				lineageRoot = resolvedRoot
			case errors.Is(previousErr, ErrSessionNotFound):
				lineageRoot = previousSession
			case previousErr != nil:
				api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_UNAVAILABLE", "the previous login session could not be checked", httpserver.RequestID(r.Context()))
				return
			}
		}
	}
	if reauthenticate {
		session, sessionErr := s.authenticatedSession(r.Context(), r)
		if sessionErr != nil {
			api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required before reauthentication", httpserver.RequestID(r.Context()))
			return
		}
		previousSession = session.ID
		lineageRoot = sessionLineageRoot(session)
	} else if forceLoginRequested {
		session, sessionErr := s.authenticatedSession(r.Context(), r)
		switch {
		case sessionErr == nil:
			previousSession = session.ID
			lineageRoot = sessionLineageRoot(session)
		case errors.Is(sessionErr, ErrSessionNotFound):
			// A forced desktop fallback must also work in a fresh browser.
		case sessionErr != nil:
			api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_UNAVAILABLE", "the previous login session could not be checked", httpserver.RequestID(r.Context()))
			return
		}
	}
	state, err := randomToken(32)
	verifier, verifierErr := randomToken(48)
	nonce, nonceErr := randomToken(32)
	err = errors.Join(err, verifierErr, nonceErr)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, "LOGIN_UNAVAILABLE", "login could not be started", httpserver.RequestID(r.Context()))
		return
	}
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	flow := LoginFlow{Verifier: verifier, Nonce: nonce, ReturnTo: returnTo, PreviousSession: previousSession, LineageRoot: lineageRoot, ExpiresAt: s.now().Add(loginFlowLifetime)}
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
	query.Set("scope", "openid profile email")
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	if locale != "" {
		query.Set("ui_locales", locale)
	}
	if reauthenticate || forceLogin {
		query.Set("prompt", "login")
	}
	if reauthenticate {
		query.Set("max_age", "900")
	}
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
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_UNAVAILABLE", "the login store is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || !flow.ExpiresAt.After(s.now()) {
		api.WriteError(w, http.StatusBadRequest, "LOGIN_STATE_INVALID", "login state is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	exchangeStartedAt := s.now()
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
	if !validTokenResponse(tokens, true) {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_EXCHANGE_FAILED", "the identity provider returned an invalid session", httpserver.RequestID(r.Context()))
		return
	}
	subject, err := s.verifyIDToken(r.Context(), tokens.IDToken, tokens.AccessToken, flow.Nonce, "", true)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_EXCHANGE_FAILED", "login identity could not be verified", httpserver.RequestID(r.Context()))
		return
	}
	now := s.now()
	sessionID, sessionIDErr := randomToken(32)
	csrfToken, csrfErr := randomToken(32)
	session := Session{ApprovalV2Eligible: true, ID: sessionID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, AccessExpiresAt: exchangeStartedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second), RefreshExpiresAt: exchangeStartedAt.Add(time.Duration(tokens.RefreshExpiresIn) * time.Second), CSRFToken: csrfToken, CreatedAt: now, Subject: subject, Nonce: flow.Nonce}
	session.LineageRoot = strings.TrimSpace(flow.LineageRoot)
	if session.LineageRoot == "" && flow.PreviousSession != "" {
		session.LineageRoot = flow.PreviousSession
	}
	if session.LineageRoot == "" {
		session.LineageRoot = session.ID
	}
	session.OIDCClientID = s.config.ClientID
	if sessionIDErr != nil || csrfErr != nil || !session.AccessExpiresAt.After(now) || !session.RefreshExpiresAt.After(now) || session.AccessToken == "" || session.RefreshToken == "" || tokens.ExpiresIn <= 0 || !strings.EqualFold(tokens.TokenType, "Bearer") {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_SESSION_FAILED", "login session could not be created", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.store.CommitLoginSession(r.Context(), flow.PreviousSession, session); err != nil {
		s.expireCookie(w, s.flowCookieName(), true)
		if errors.Is(err, ErrLoginFlowInvalidated) {
			api.WriteError(w, http.StatusConflict, "LOGIN_FLOW_INVALIDATED", "login was cancelled by an explicit logout", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_SESSION_FAILED", "login session could not be created", httpserver.RequestID(r.Context()))
		return
	}
	s.expireCookie(w, s.flowCookieName(), true)
	s.expireCookie(w, s.loggedOutCookieName(), true)
	s.setCookie(w, s.sessionCookieName(), session.ID, session.RefreshExpiresAt, true)
	s.setCookie(w, s.csrfCookieName(), session.CSRFToken, session.RefreshExpiresAt, false)
	http.Redirect(w, r, flow.ReturnTo, http.StatusFound)
}

func (s *Service) sessionStatus(w http.ResponseWriter, r *http.Request) {
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			api.WriteJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		} else {
			s.writeSessionError(w, r, err)
		}
		return
	}
	s.setSessionCookies(w, session)
	api.WriteJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": session.CSRFToken})
}
func (s *Service) sessionLineageStatus(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, map[string]any{"schema_version": 1, "capable": true, "enabled": true})
}
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.validLogoutRequest(r) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	sessionCookie, err := r.Cookie(s.sessionCookieName())
	if err != nil || strings.TrimSpace(sessionCookie.Value) == "" {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	sessionID := strings.TrimSpace(sessionCookie.Value)
	session, err := s.store.GetSession(r.Context(), sessionID)
	lineageRoot := ""
	switch {
	case err == nil:
		lineageRoot = sessionLineageRoot(session)
		csrfHeader := r.Header.Get("X-CSRF-Token")
		if subtle.ConstantTimeCompare([]byte(csrfHeader), []byte(session.CSRFToken)) != 1 {
			api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
			return
		}
	case errors.Is(err, ErrSessionNotFound):
		// A previous logout may have deleted the session while its response was
		// lost. The same-origin double-submit proof still authorizes only the
		// idempotent cleanup of the opaque session ID already in this browser.
	case err != nil:
		api.WriteError(w, http.StatusServiceUnavailable, "LOGOUT_UNAVAILABLE", "the login session could not be checked", httpserver.RequestID(r.Context()))
		return
	}
	logoutErr := s.store.LogoutSession(r.Context(), sessionID, lineageRoot, s.logoutMarkerExpiry(session))
	if logoutErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGOUT_UNAVAILABLE", "the login session could not be ended", httpserver.RequestID(r.Context()))
		return
	}
	s.expireCookie(w, s.sessionCookieName(), true)
	s.expireCookie(w, s.csrfCookieName(), false)
	s.setCookie(w, s.loggedOutCookieName(), "1", s.logoutMarkerExpiry(session), true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) validLogoutRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	parsed, err := url.Parse(origin)
	if err != nil || origin == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.Scheme != s.config.PublicURL.Scheme || parsed.Host != s.config.PublicURL.Host {
		return false
	}
	csrfCookie, err := r.Cookie(s.csrfCookieName())
	header := r.Header.Get("X-CSRF-Token")
	return err == nil && len(header) >= 32 && subtle.ConstantTimeCompare([]byte(header), []byte(csrfCookie.Value)) == 1
}

func (s *Service) authenticatedSession(ctx context.Context, r *http.Request) (Session, error) {
	session, err := s.storedSession(ctx, r)
	if err != nil {
		return Session{}, err
	}
	if session.AccessExpiresAt.After(s.now().Add(30 * time.Second)) {
		return session, nil
	}
	return s.refreshSession(ctx, session)
}

func (s *Service) storedSession(ctx context.Context, r *http.Request) (Session, error) {
	cookie, err := r.Cookie(s.sessionCookieName())
	if err != nil || cookie.Value == "" {
		return Session{}, ErrSessionNotFound
	}
	session, err := s.store.GetSession(ctx, cookie.Value)
	if err != nil {
		return Session{}, err
	}
	if session.ID != cookie.Value || !session.RefreshExpiresAt.After(s.now()) {
		return Session{}, ErrSessionNotFound
	}
	return session, nil
}

func (s *Service) refreshSession(ctx context.Context, session Session) (Session, error) {
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	owner, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	for {
		locked, err := s.store.TryRefreshLock(waitCtx, session.ID, owner)
		if err != nil {
			return Session{}, err
		}
		if locked {
			defer func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = s.store.ReleaseRefreshLock(releaseCtx, session.ID, owner)
			}()
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			latest, err := s.store.GetSession(ctx, session.ID)
			if err != nil || latest.AccessExpiresAt.After(s.now().Add(30*time.Second)) {
				return latest, err
			}
			if latest.ID != session.ID || latest.RefreshToken == "" || latest.Subject == "" || latest.Nonce == "" || !latest.RefreshExpiresAt.After(s.now()) {
				return Session{}, ErrSessionNotFound
			}
			exchangeStartedAt := s.now()
			tokens, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {s.config.ClientID}, "refresh_token": {latest.RefreshToken}})
			if err == nil && !validTokenResponse(tokens, false) {
				err = errors.New("invalid refreshed token response")
			}
			if err == nil && tokens.IDToken != "" {
				_, err = s.verifyIDToken(ctx, tokens.IDToken, tokens.AccessToken, latest.Nonce, latest.Subject, false)
			}
			if err != nil {
				if errors.Is(err, ErrSessionNotFound) {
					deleteErr := s.store.DeleteRefreshedSession(ctx, latest, owner)
					if deleteErr != nil && !errors.Is(deleteErr, ErrSessionNotFound) {
						return Session{}, deleteErr
					}
				}
				if _, currentErr := s.store.GetSession(ctx, session.ID); errors.Is(currentErr, ErrSessionNotFound) {
					return Session{}, ErrSessionNotFound
				}
				return Session{}, err
			}
			now := s.now()
			updated := latest
			updated.AccessToken = tokens.AccessToken
			updated.AccessExpiresAt = exchangeStartedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)
			if tokens.RefreshToken != "" {
				updated.RefreshToken = tokens.RefreshToken
			}
			updated.RefreshExpiresAt = exchangeStartedAt.Add(time.Duration(tokens.RefreshExpiresIn) * time.Second)
			if !updated.AccessExpiresAt.After(now) || !updated.RefreshExpiresAt.After(now) {
				return Session{}, errors.New("refreshed session expires before receipt")
			}
			return updated, s.store.SaveRefreshedSession(ctx, latest, updated, owner)
		}
		select {
		case <-waitCtx.Done():
			return Session{}, waitCtx.Err()
		case <-time.After(100 * time.Millisecond):
			session, err = s.store.GetSession(ctx, session.ID)
			if err != nil {
				return Session{}, err
			}
			if err == nil && session.AccessExpiresAt.After(s.now().Add(30*time.Second)) {
				return session, nil
			}
		}
	}
}

func (s *Service) exchange(ctx context.Context, form url.Values) (TokenResponse, error) {
	requestForm := make(url.Values, len(form)+1)
	for key, values := range form {
		requestForm[key] = append([]string(nil), values...)
	}
	form = requestForm
	if s.config.HandoffSubjectTokenEnabled && form.Get("client_id") == s.config.HandoffClientID {
		form.Set("client_secret", s.config.HandoffClientSecret)
	}
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
		var body struct {
			Error string `json:"error"`
		}
		if err := decodePortalJSON(response.Body, &body, 1<<20); err != nil {
			return TokenResponse{}, errors.New("OIDC token error response is invalid")
		}
		if response.StatusCode == http.StatusBadRequest && body.Error == "invalid_grant" {
			return TokenResponse{}, ErrSessionNotFound
		}
		return TokenResponse{}, &oidcTokenEndpointError{StatusCode: response.StatusCode, Code: body.Error}
	}
	var tokens TokenResponse
	if err := decodePortalJSON(response.Body, &tokens, 1<<20); err != nil {
		return TokenResponse{}, err
	}
	return tokens, nil
}

func validTokenResponse(tokens TokenResponse, initial bool) bool {
	const maximumSeconds = int64((1<<63 - 1) / time.Second)
	if strings.TrimSpace(tokens.AccessToken) == "" || !strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > maximumSeconds || tokens.RefreshExpiresIn <= 0 || tokens.RefreshExpiresIn > maximumSeconds {
		return false
	}
	return !initial || strings.TrimSpace(tokens.RefreshToken) != "" && tokens.RefreshExpiresIn > 0
}

func (s *Service) verifyIDToken(ctx context.Context, raw, accessToken, nonce, subject string, initial bool) (string, error) {
	if raw == "" {
		return "", errors.New("OIDC ID token is required")
	}
	token, err := s.verifier.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	if token.Subject == "" || subject != "" && token.Subject != subject {
		return "", errors.New("OIDC ID token subject is invalid")
	}
	if token.AccessTokenHash != "" {
		if err := token.VerifyAccessToken(accessToken); err != nil {
			return "", err
		}
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err := token.Claims(&claims); err != nil || len(token.Audience) > 1 && claims.AuthorizedParty == "" || claims.AuthorizedParty != "" && claims.AuthorizedParty != s.config.ClientID {
		return "", errors.New("OIDC ID token authorized party is invalid")
	}
	// Refresh ID tokens may omit nonce, but must not change it when present.
	if initial && (nonce == "" || token.Nonce == "") || (initial || token.Nonce != "") && subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(nonce)) != 1 {
		return "", errors.New("OIDC ID token nonce is invalid")
	}
	return token.Subject, nil
}

func (s *Service) writeSessionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrSessionNotFound) {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteError(w, http.StatusServiceUnavailable, "SESSION_UNAVAILABLE", "the login session is temporarily unavailable", httpserver.RequestID(r.Context()))
}

func (s *Service) setSessionCookies(w http.ResponseWriter, session Session) {
	s.setCookie(w, s.sessionCookieName(), session.ID, session.RefreshExpiresAt, true)
	s.setCookie(w, s.csrfCookieName(), session.CSRFToken, session.RefreshExpiresAt, false)
}

func (s *Service) validMutation(r *http.Request, session Session) bool {
	return s.validLogoutRequest(r) && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) == 1
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

func (s *Service) loggedOutCookieName() string {
	return cookieName(s.config.SecureCookies, "portal_logged_out")
}

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

func portalLocale(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "zh", "zh-cn", "zh-hans":
		return "zh-CN"
	case "en", "en-us", "en-gb":
		return "en"
	case "ja", "ja-jp":
		return "ja"
	default:
		return ""
	}
}

func decodePortalJSON(reader io.Reader, target any, limit int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(body)) > limit {
		return errors.New("portal dependency response is incomplete or too large")
	}
	if err = json.Unmarshal(body, target); err != nil {
		return errors.New("portal dependency response is invalid")
	}
	return nil
}
func safeReturnTo(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\r\n") {
		return "/"
	}
	return value
}
func canonicalProxyResource(resource string) bool {
	return strings.HasPrefix(resource, "/") && !strings.ContainsAny(resource, "\\%") && path.Clean(resource) == resource
}
func pathUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
func publicAccountPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodPost {
		switch resource {
		case "/v1/auth/registration/precheck", "/v1/auth/captcha/challenges", "/v1/auth/sms/challenges", "/v1/auth/register", "/v1/auth/password-resets":
			return true
		}
	}
	prefix := "/v1/auth/registration-operations/"
	return method == http.MethodGet && strings.HasPrefix(resource, prefix) && pathUUID(strings.TrimPrefix(resource, prefix))
}
func accountPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodPost {
		switch resource {
		case "/v1/account/bootstrap", "/v1/me/password-changes", "/v1/me/profile-change-requests":
			return true
		}
	}
	if method == http.MethodGet && (resource == "/v1/me" || resource == "/v1/me/authorization-context") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "me" || parts[2] != "profile-change-requests" || !pathUUID(parts[3]) {
		return false
	}
	return (len(parts) == 4 && method == http.MethodGet) || (len(parts) == 5 && parts[4] == "mobile-challenges" && method == http.MethodPost) || (len(parts) == 6 && parts[4] == "actions" && parts[5] == "complete" && method == http.MethodPost)
}
func tokenPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet {
		switch resource {
		case "/v1/me/balance", "/v1/recharge/products", "/v1/payment-capabilities", "/v1/recharge/orders", "/v1/recharge/orders/recent", "/v1/me/entitlement-packages":
			return true
		}
	}
	if method == http.MethodPost && resource == "/v1/recharge/orders" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "recharge" || parts[2] != "orders" || !pathUUID(parts[3]) {
		return false
	}
	return method == http.MethodGet && len(parts) == 4 || method == http.MethodPost && len(parts) == 5 && (parts[4] == "close" || parts[4] == "payment-claims")
}
func accountAdminPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet && resource == "/admin/v1/users" {
		return true
	}
	if resource == "/admin/v1/sms-configuration" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if method == http.MethodGet && (resource == "/admin/v1/registration-operations" || resource == "/admin/v1/profile-change-requests") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if method == http.MethodPost && len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && pathUUID(parts[3]) && parts[4] == "actions" {
		return parts[2] == "registration-operations" && (parts[5] == "retry" || parts[5] == "compensate") || parts[2] == "profile-change-requests" && parts[5] == "retry"
	}
	return method == http.MethodPatch && len(parts) == 5 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "users" && pathUUID(parts[3]) && parts[4] == "status"
}
func (s *Service) proxyAccount(w http.ResponseWriter, r *http.Request) {
	resource := "/" + r.PathValue("resource")
	if canonicalProxyResource(resource) && publicAccountPathAllowed(r.Method, resource) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin is invalid", httpserver.RequestID(r.Context()))
			return
		}
		s.forward(w, r, s.config.AccountURL, resource, Session{}, false)
		return
	}
	s.proxy(w, r, s.config.AccountURL, resource, accountPathAllowed)
}
func (s *Service) proxyToken(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenPathAllowed)
}
func (s *Service) proxyAdminAccount(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.AccountURL, "/"+r.PathValue("resource"), accountAdminPathAllowed)
}
func (s *Service) proxy(w http.ResponseWriter, r *http.Request, upstream *url.URL, resource string, allowed func(string, string) bool) {
	if !canonicalProxyResource(resource) || !allowed(r.Method, resource) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.storedSession(r.Context(), r)
	if err != nil {
		s.writeSessionError(w, r, err)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	session, err = s.authenticatedSession(r.Context(), r)
	if err != nil {
		s.writeSessionError(w, r, err)
		return
	}
	s.forward(w, r, upstream, resource, session, true)
}
func (s *Service) forward(w http.ResponseWriter, r *http.Request, upstream *url.URL, resource string, session Session, authenticated bool) {
	limit := int64(4 << 20)
	if authenticated && upstream == s.config.BillingURL && r.Method == http.MethodPost && resource == "/admin/v1/reconciliation-imports" {
		if !s.allowReconciliationImport(w, r, session) {
			return
		}
		limit = reconciliationImportBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "INVALID_REQUEST", "the request body is invalid or too large", httpserver.RequestID(r.Context()))
		return
	}
	target := *upstream
	target.Path = strings.TrimRight(target.Path, "/") + resource
	target.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), strings.NewReader(string(body)))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+session.AccessToken)
		s.setSessionCookies(w, session)
	}
	request.Header.Set("Accept", "application/json")
	for _, name := range []string{"Content-Type", "Idempotency-Key"} {
		if value := r.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	if upstream == s.config.AccountURL {
		for _, name := range []string{"X-Registration-Query-Token", "X-Device-ID"} {
			if accountProofHeaderAllowed(r.Method, resource, name) {
				request.Header.Set(name, r.Header.Get(name))
			}
		}
		if accountRiskSourceAllowed(r.Method, resource) {
			source, valid := s.clientRiskSource(r)
			if !valid {
				api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_SOURCE", "the request source is invalid", httpserver.RequestID(r.Context()))
				return
			}
			request.Header.Set("X-Pinvou-Client-Source", source)
		}
	}
	if upstream == s.config.TokenGatewayURL && strings.HasPrefix(resource, "/v1/recharge/") {
		source, valid := s.clientRiskSource(r)
		if !valid {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_SOURCE", "the request source is invalid", httpserver.RequestID(r.Context()))
			return
		}
		request.Header.Set("X-Forwarded-For", source)
	}
	response, err := s.client.Do(request)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Retry-After", "X-Context-Version", "X-Authorization-Version", "X-Personal-Authorization-Version", "X-Platform-Authorization-Version", "X-Organization-Authorization-Version"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 32<<20))
}

// Risk attribution is separate from device proofs: public admission endpoints
// need the trusted client source without gaining any additional browser proof.
func accountRiskSourceAllowed(method, resource string) bool {
	return method == http.MethodPost && (resource == "/v1/auth/registration/precheck" || resource == "/v1/auth/captcha/challenges" || accountProofHeaderAllowed(method, resource, "X-Device-ID"))
}

// These proofs belong to the individual registration or SMS operation, not to
// the browser's authentication authority. All other request headers stay stripped.
func accountProofHeaderAllowed(method, resource, name string) bool {
	if name == "X-Registration-Query-Token" {
		parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
		return (method == http.MethodGet || method == http.MethodHead) && len(parts) == 4 && parts[0] == "v1" && parts[1] == "auth" && parts[2] == "registration-operations" && pathUUID(parts[3])
	}
	if name != "X-Device-ID" || method != http.MethodPost {
		return false
	}
	if resource == "/v1/auth/sms/challenges" || resource == "/v1/auth/register" || resource == "/v1/auth/password-resets" || resource == "/v1/me/password-changes" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	return len(parts) >= 5 && parts[0] == "v1" && parts[1] == "me" && parts[2] == "profile-change-requests" && pathUUID(parts[3]) && (len(parts) == 5 && parts[4] == "mobile-challenges" || len(parts) == 6 && parts[4] == "actions" && parts[5] == "complete")
}
