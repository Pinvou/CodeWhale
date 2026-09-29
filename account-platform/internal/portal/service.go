package portal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

type Config struct {
	PublicURL                           *url.URL
	AuthorizationURL                    *url.URL
	TokenURL                            *url.URL
	PrivateCAFile                       string
	ClientID                            string
	HandoffClientID                     string
	HandoffClientSecret                 string
	AccountURL                          *url.URL
	BillingURL                          *url.URL
	TokenGatewayURL                     *url.URL
	HandoffSubjectTokenEnabled          bool
	SessionLineageLogoutEnabled         bool
	SessionHandoffTTL                   time.Duration
	ClientVersion                       string
	TokenAdminProxyHMACKey              []byte
	AdminModelTokenIAEnabled            bool
	AdminTokenResourceFormsEnabled      bool
	AdminModelOperationsDetailsEnabled  bool
	AdminModelDetailsEnabled            bool
	AdminModelPricingEditEnabled        bool
	AdminModelOperationsOverviewEnabled bool
	AdminModelTokenPageOverviewsEnabled bool
	AdminTokenDetailsEnabled            bool
	AdminModelTokenActionsEnabled       bool
	CredentialOrigin                    *url.URL
	SystemProbes                        []SystemProbe
	SecureCookies                       bool
	SessionMaxAge                       time.Duration
}

const defaultSessionMaxAge = 30 * 24 * time.Hour

type Service struct {
	config      Config
	store       Store
	client      *http.Client
	now         func() time.Time
	deadlineNow func() time.Time
}

type oidcTokenEndpointError struct {
	StatusCode int
	Code       string
}

func (e *oidcTokenEndpointError) Error() string {
	return fmt.Sprintf("OIDC token endpoint returned HTTP %d", e.StatusCode)
}

func NewService(config Config, store Store) (*Service, error) {
	if config.PublicURL == nil || config.AuthorizationURL == nil || config.TokenURL == nil || config.AccountURL == nil || config.BillingURL == nil || config.TokenGatewayURL == nil || strings.TrimSpace(config.ClientID) == "" || store == nil {
		return nil, errors.New("invalid portal configuration")
	}
	if config.PublicURL.Scheme != "https" && config.SecureCookies {
		return nil, errors.New("secure portal cookies require an HTTPS public URL")
	}
	if strings.TrimSpace(config.PrivateCAFile) != "" && config.TokenURL.Scheme != "https" {
		return nil, errPortalPrivateCA
	}
	if config.AdminTokenDetailsEnabled {
		if err := validateCredentialOrigin(config.CredentialOrigin, config.SecureCookies); err != nil {
			return nil, err
		}
	}
	if config.AdminModelTokenPageOverviewsEnabled && len(config.TokenAdminProxyHMACKey) < 32 {
		return nil, errors.New("model and Token page overviews require a proxy HMAC key with at least 32 bytes")
	}
	if config.HandoffSubjectTokenEnabled && (config.SessionHandoffTTL <= 0 || config.SessionHandoffTTL > time.Minute) {
		return nil, errors.New("desktop session handoff TTL must be between one nanosecond and one minute")
	}
	if config.HandoffSubjectTokenEnabled && (strings.TrimSpace(config.HandoffClientID) == "" || strings.TrimSpace(config.HandoffClientSecret) == "" || config.HandoffClientID == config.ClientID) {
		return nil, errors.New("desktop session handoff requires a dedicated confidential OIDC client")
	}
	if config.SessionMaxAge <= 0 {
		config.SessionMaxAge = defaultSessionMaxAge
	}
	client, err := newPortalHTTPClient(config.PrivateCAFile)
	if err != nil {
		return nil, err
	}
	return &Service{config: config, store: store, client: client, now: func() time.Time { return time.Now().UTC() }, deadlineNow: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Handler(static http.Handler) http.Handler {
	router := httpserver.NewRouter()
	router.GET("/auth/login", httpserver.AdaptFunc(s.login))
	router.GET("/auth/callback", httpserver.AdaptFunc(s.callback))
	router.GET("/portal-session-handoff", httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		if static == nil {
			http.NotFound(w, r)
			return
		}
		pageRequest := r.Clone(r.Context())
		pageURL := *r.URL
		pageURL.Path = "/portal-session-handoff.html"
		pageRequest.URL = &pageURL
		static.ServeHTTP(w, pageRequest)
	}))
	router.POST("/portal-session-handoff", httpserver.AdaptFunc(s.createSessionHandoff))
	router.POST("/portal-session-handoff/consume", httpserver.AdaptFunc(s.consumeSessionHandoff))
	router.GET("/api/session", httpserver.AdaptFunc(s.sessionStatus))
	router.GET("/api/admin/approvals/capability", httpserver.AdaptFunc(s.adminApprovalCapability))
	router.GET("/api/admin/approvals/directory", httpserver.AdaptFunc(s.adminApprovalDirectory))
	router.GET("/api/admin/approvals/requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	router.POST("/api/admin/approvals/requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	router.Any("/api/admin/approvals/requests/*resource", httpserver.AdaptFunc(s.adminApprovalRequests))
	router.POST("/api/admin/approvals/prechecks", httpserver.AdaptFunc(s.adminApprovalPrechecks))
	router.POST("/api/logout", httpserver.AdaptFunc(s.logout))
	router.GET("/health/session-lineage", httpserver.AdaptFunc(s.sessionLineageStatus))
	router.Any("/api/account/*resource", httpserver.AdaptFunc(s.proxyAccount))
	router.Any("/api/token/*resource", httpserver.AdaptFunc(s.proxyToken))
	router.Any("/api/admin/account/*resource", httpserver.AdaptFunc(s.proxyAdminAccount))
	router.Any("/api/admin/billing/*resource", httpserver.AdaptFunc(s.proxyAdminBilling))
	router.Any("/api/admin/token/*resource", httpserver.AdaptFunc(s.proxyAdminToken))
	router.GET("/api/admin/system/status", httpserver.AdaptFunc(s.adminSystemStatus))
	application := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/credential-handoff" {
			s.credentialHandoff(w, r)
			return
		}
		// Credential launch documents need a non-null browser Origin while the
		// Referer remains origin-only. The credential service still validates the
		// Origin independently; this response policy is not an authorization check.
		w.Header().Set("Referrer-Policy", "origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.config.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-store")
		} else if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		isSessionHandoffRoute := r.URL.Path == "/portal-session-handoff" || r.URL.Path == "/portal-session-handoff/consume"
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/health/") || isSessionHandoffRoute || static == nil {
			router.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			portalURL := *r.URL
			portalQuery := portalURL.Query()
			if strings.TrimSpace(portalQuery.Get("section")) == "" {
				portalQuery.Set("section", "creditsUsage")
				portalURL.RawQuery = portalQuery.Encode()
			}
			if _, err := s.authenticatedSession(r.Context(), r); errors.Is(err, ErrSessionNotFound) {
				loginRequest := r.Clone(r.Context())
				loginURL := *r.URL
				query := loginURL.Query()
				query.Set("return_to", safeReturnTo(portalURL.RequestURI()))
				loginURL.RawQuery = query.Encode()
				loginRequest.URL = &loginURL
				s.login(w, loginRequest)
				return
			} else if err != nil {
				api.WriteError(w, http.StatusServiceUnavailable, "SESSION_UNAVAILABLE", "the login session could not be checked", httpserver.RequestID(r.Context()))
				return
			}
			if portalURL.RawQuery != r.URL.RawQuery {
				http.Redirect(w, r, portalURL.RequestURI(), http.StatusFound)
				return
			}
		}
		static.ServeHTTP(w, r)
	})
	return s.tokenAdminIngressDeadline(application)
}

func (s *Service) sessionLineageStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"schema_version": 1,
		"capable":        true,
		"enabled":        s.config.SessionLineageLogoutEnabled,
	})
}

func (s *Service) tokenAdminIngressDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Portal is the first trusted application ingress. Discard both
		// client-controlled transport headers before deriving one absolute chain.
		// X-Pinvou-Client-Source is always Portal-owned; public clients can never
		// supply the source identity used by Token's persistent rate limiter.
		r.Header.Del("X-Pinvou-Client-Source")
		r.Header.Del(requestdeadline.AbsoluteDeadlineHeader)
		r.Header.Del(requestdeadline.DeadlineClassHeader)
		const prefix = "/api/admin/token"
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		resource := strings.TrimPrefix(r.URL.Path, prefix)
		class, ok := requestdeadline.Classify(r.Method, resource, r.URL.Query().Get("cursor"))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		contract, err := requestdeadline.Start(s.deadlineNow().UTC(), class)
		if err != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "the Token management query could not be scheduled", httpserver.RequestID(r.Context()))
			return
		}
		ctx, cancel := requestdeadline.WithContract(r.Context(), contract)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
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
	flow := LoginFlow{Verifier: verifier, Nonce: nonce, ReturnTo: returnTo, PreviousSession: previousSession, LineageRoot: lineageRoot, ExpiresAt: s.now().Add(10 * time.Minute)}
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
	session := Session{ID: sessionID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, AccessExpiresAt: now.Add(time.Duration(tokens.ExpiresIn) * time.Second), RefreshExpiresAt: s.refreshExpiry(now, tokens.RefreshExpiresIn, now.Add(s.config.SessionMaxAge)), CSRFToken: csrfToken, CreatedAt: now, ApprovalV2Eligible: true}
	session.LineageRoot = strings.TrimSpace(flow.LineageRoot)
	if session.LineageRoot == "" && flow.PreviousSession != "" {
		session.LineageRoot = flow.PreviousSession
	}
	if session.LineageRoot == "" {
		session.LineageRoot = session.ID
	}
	session.OIDCClientID = s.config.ClientID
	if sessionIDErr != nil || csrfErr != nil || session.AccessToken == "" || session.RefreshToken == "" || tokens.ExpiresIn <= 0 || !strings.EqualFold(tokens.TokenType, "Bearer") {
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
		if !errors.Is(err, ErrSessionNotFound) {
			api.WriteError(w, http.StatusServiceUnavailable, "SESSION_UNAVAILABLE", "the login session could not be checked", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf_token":    session.CSRFToken,
		"feature_flags": map[string]bool{
			"admin_model_token_ia":             s.config.AdminModelTokenIAEnabled,
			"admin_token_resource_forms":       s.config.AdminTokenResourceFormsEnabled,
			"admin_model_operations_details":   s.config.AdminModelOperationsDetailsEnabled,
			"admin_model_details":              s.config.AdminModelDetailsEnabled,
			"admin_model_pricing_edit":         s.config.AdminModelPricingEditEnabled,
			"admin_model_operations_overview":  s.config.AdminModelOperationsOverviewEnabled,
			"admin_model_token_page_overviews": s.config.AdminModelTokenPageOverviewsEnabled,
			"admin_token_details":              s.config.AdminTokenDetailsEnabled,
			"admin_model_token_actions":        s.config.AdminModelTokenActionsEnabled,
		},
		"credential_origin": credentialOriginProjection(s.config),
	})
}

func validateCredentialOrigin(origin *url.URL, secure bool) error {
	if origin == nil || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("credential origin must be an absolute origin without path, query, fragment, or credentials")
	}
	host := strings.ToLower(origin.Hostname())
	if secure {
		if origin.Scheme != "https" || (host != "credentials.pinvou.com" && host != "credentials-uat.pinvou.com") || origin.Port() != "" {
			return errors.New("credential origin is not in the fixed production or UAT allowlist")
		}
		return nil
	}
	if origin.Scheme != "http" || host != "127.0.0.1" && host != "localhost" {
		return errors.New("development credential origin must use loopback HTTP")
	}
	return nil
}

func credentialOriginProjection(config Config) string {
	if !config.AdminTokenDetailsEnabled || config.CredentialOrigin == nil {
		return ""
	}
	return config.CredentialOrigin.Scheme + "://" + config.CredentialOrigin.Host
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
	var logoutErr error
	if s.config.SessionLineageLogoutEnabled {
		logoutErr = s.store.LogoutSession(r.Context(), sessionID, lineageRoot, s.now().Add(s.config.SessionMaxAge))
	} else {
		logoutErr = s.store.DeleteSession(r.Context(), sessionID)
	}
	if logoutErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGOUT_UNAVAILABLE", "the login session could not be ended", httpserver.RequestID(r.Context()))
		return
	}
	s.expireCookie(w, s.sessionCookieName(), true)
	s.expireCookie(w, s.csrfCookieName(), false)
	s.setCookie(w, s.loggedOutCookieName(), "1", s.now().Add(s.config.SessionMaxAge), true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) validLogoutRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	parsed, err := url.Parse(origin)
	if err != nil || origin == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Scheme != s.config.PublicURL.Scheme || parsed.Host != s.config.PublicURL.Host {
		return false
	}
	csrfCookie, err := r.Cookie(s.csrfCookieName())
	header := r.Header.Get("X-CSRF-Token")
	return err == nil && len(header) >= 32 && subtle.ConstantTimeCompare([]byte(header), []byte(csrfCookie.Value)) == 1
}

func (s *Service) proxyAccount(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.AccountURL, "/"+r.PathValue("resource"), accountPathAllowed)
}

func (s *Service) proxyToken(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenPathAllowed)
}

func (s *Service) proxyAdminAccount(w http.ResponseWriter, r *http.Request) {
	if s.rejectDisabledModelTokenApproval(w, r) {
		return
	}
	s.proxy(w, r, s.config.AccountURL, "/"+r.PathValue("resource"), accountAdminPathAllowed)
}

func (s *Service) proxyAdminBilling(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.BillingURL, "/"+r.PathValue("resource"), billingAdminPathAllowed)
}

func (s *Service) proxyAdminToken(w http.ResponseWriter, r *http.Request) {
	rawResource := "/" + r.PathValue("resource")
	kind, exactPageOverview := tokenPageOverviewKind(r.Method, rawResource)
	if (!exactPageOverview && tokenPageOverviewAlias(r.Method, rawResource)) || (exactPageOverview && strings.Contains(r.URL.EscapedPath(), "%")) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	if exactPageOverview && len(s.config.TokenAdminProxyHMACKey) < 32 {
		writeTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	resource := path.Clean(rawResource)
	credentialCleanup := tokenCredentialRevokeWindowPath(r.Method, resource)
	if tokenCredentialIsolationPath(r.Method, resource) && !credentialCleanup && (!s.config.AdminTokenDetailsEnabled || s.config.CredentialOrigin == nil) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !credentialCleanup && !s.config.AdminModelTokenActionsEnabled {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenAdminPathAllowed)
}

func tokenPageOverviewKind(method, resource string) (string, bool) {
	if method != http.MethodGet && method != http.MethodHead {
		return "", false
	}
	switch resource {
	case "/admin/v1/overviews/model-catalog":
		return "MODEL_CATALOG_OVERVIEW", true
	case "/admin/v1/overviews/token-resources":
		return "TOKEN_RESOURCES_OVERVIEW", true
	case "/admin/v1/overviews/model-operations":
		return "MODEL_OPERATIONS_OVERVIEW", true
	default:
		return "", false
	}
}

func tokenPageOverviewAlias(method, resource string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	if _, ok := tokenPageOverviewKind(method, path.Clean(resource)); ok {
		return true
	}
	decoded, err := url.PathUnescape(resource)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(resource), "/admin/v1/overviews/")
	}
	if _, ok := tokenPageOverviewKind(method, path.Clean(decoded)); ok {
		return true
	}
	return strings.HasPrefix(strings.ToLower(resource), "/admin/v1/overviews/") || strings.HasPrefix(strings.ToLower(decoded), "/admin/v1/overviews/")
}

func writeTokenPageOverviewUnavailable(w http.ResponseWriter, r *http.Request, kind string) {
	labels := map[string]string{
		"MODEL_CATALOG_OVERVIEW":    "model catalog overview",
		"TOKEN_RESOURCES_OVERVIEW":  "Token resources overview",
		"MODEL_OPERATIONS_OVERVIEW": "model operations overview",
	}
	label, ok := labels[kind]
	if !ok {
		label = "Token overview"
	}
	api.WriteError(w, http.StatusServiceUnavailable, kind+"_UNAVAILABLE", "the "+label+" is temporarily unavailable", httpserver.RequestID(r.Context()))
}

func tokenCredentialRevokeWindowPath(method, resource string) bool {
	parts := strings.Split(strings.Trim(path.Clean(resource), "/"), "/")
	if len(parts) != 5 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "credential-results" || parts[4] != "revoke-window" || method != http.MethodPost {
		return false
	}
	_, err := uuid.Parse(parts[3])
	return err == nil
}

func tokenCredentialIsolationPath(method, resource string) bool {
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(path.Clean(resource), "/"), "/")
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v2" && parts[2] == "token-accounts" && method == http.MethodGet {
		_, err := uuid.Parse(parts[3])
		return err == nil
	}
	if len(parts) >= 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "credential-results" {
		return len(parts) == 4 && method == http.MethodGet || len(parts) == 5 && parts[4] == "revoke-window" && method == http.MethodPost
	}
	if len(parts) < 5 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "token-accounts" {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	return len(parts) == 5 && parts[4] == "credentials" && method == http.MethodGet ||
		len(parts) == 6 && parts[4] == "credential-launches" && (parts[5] == "enroll" || parts[5] == "rotate") && method == http.MethodPost
}

func (s *Service) rejectDisabledModelTokenApproval(w http.ResponseWriter, r *http.Request) bool {
	if s.config.AdminModelTokenActionsEnabled || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	resource := path.Clean("/" + r.PathValue("resource"))
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	isDraftWrite := resource == "/admin/v1/approval-requests" && r.Method == http.MethodPost
	isDraftUpdate := len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "approval-requests" && r.Method == http.MethodPut
	isExistingAction := len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "approval-requests" && parts[4] == "actions" && (parts[5] == "submit" || parts[5] == "withdraw" || parts[5] == "decide") && r.Method == http.MethodPost
	if isDraftUpdate || isExistingAction {
		requestID, err := uuid.Parse(parts[3])
		if err != nil {
			api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
			return true
		}
		return s.rejectDisabledExistingTokenApprovalAction(w, r, requestID)
	}
	if !isDraftWrite {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "the approval request could not be read", httpserver.RequestID(r.Context()))
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > 4<<20 {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "INVALID_REQUEST", "the approval request is too large", httpserver.RequestID(r.Context()))
		return true
	}
	var input struct {
		CommandType string `json:"command_type"`
	}
	if json.Unmarshal(body, &input) != nil || strings.TrimSpace(input.CommandType) == "" {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_CLASSIFICATION_UNAVAILABLE", "the approval request could not be classified", httpserver.RequestID(r.Context()))
		return true
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(input.CommandType)), "TOKEN_") {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return true
	}
	return false
}

// rejectDisabledExistingTokenApprovalAction uses the short-lived classification
// proof captured when this same Portal session read the authoritative Account
// approval projection. The action request never calls Account merely to decide
// whether it may call Account: missing, expired, cross-session, or malformed
// proof fails closed and therefore cannot be used to bypass the feature gate.
func (s *Service) rejectDisabledExistingTokenApprovalAction(w http.ResponseWriter, r *http.Request, approvalRequestID uuid.UUID) bool {
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return true
	}
	if !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return true
	}
	proof, err := s.store.GetApprovalClassification(r.Context(), session.ID, approvalRequestID.String())
	actorFingerprint := approvalActorTokenFingerprint(session.AccessToken)
	if err != nil || proof.SessionID != session.ID || proof.ActorTokenFingerprint == "" || subtle.ConstantTimeCompare([]byte(proof.ActorTokenFingerprint), []byte(actorFingerprint)) != 1 || proof.ApprovalRequestID != approvalRequestID.String() || !proof.ExpiresAt.After(s.now()) || strings.TrimSpace(proof.CommandType) == "" {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_CLASSIFICATION_UNAVAILABLE", "the approval request could not be classified", httpserver.RequestID(r.Context()))
		return true
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(proof.CommandType)), "TOKEN_") {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return true
	}
	return false
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
	if approvalV1WriteResource(r.Method, resource) && s.v2LockedApprovalSession(r.Context(), session) {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE", "approval protocol is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	target := *upstream
	target.Path = strings.TrimRight(target.Path, "/") + resource
	target.RawQuery = r.URL.RawQuery
	requestContext := r.Context()
	var cancel context.CancelFunc
	if contract, ok := requestdeadline.FromContext(r.Context()); ok {
		requestContext, cancel = context.WithDeadline(requestContext, contract.BFFDeadline)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, r.Method, target.String(), io.LimitReader(r.Body, 4<<20))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	s.applyClientCompatibilityHeaders(request, portalClientSource(r))
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if token := r.Header.Get("X-Pinvou-Download-Token"); token != "" {
		request.Header.Set("X-Pinvou-Download-Token", token)
	}
	if contract, ok := requestdeadline.FromContext(r.Context()); ok {
		requestdeadline.ApplyToUpstream(request, contract)
	}
	response, err := s.client.Do(request)
	if err != nil {
		if contract, ok := requestdeadline.FromContext(r.Context()); ok && (errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded)) {
			writeTokenAdminDeadlineError(w, r, contract.Class)
			return
		}
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	if upstream == s.config.AccountURL && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		s.captureApprovalClassification(r.Context(), session, r.Method, resource, response)
	}
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	if disposition := response.Header.Get("Content-Disposition"); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	if contextVersion := response.Header.Get("X-Context-Version"); contextVersion != "" {
		w.Header().Set("X-Context-Version", contextVersion)
	}
	if authorizationVersion := response.Header.Get("X-Authorization-Version"); authorizationVersion != "" {
		w.Header().Set("X-Authorization-Version", authorizationVersion)
	}
	for _, header := range []string{
		"X-Personal-Authorization-Version",
		"X-Platform-Authorization-Version",
		"X-Organization-Authorization-Version",
	} {
		if version := response.Header.Get(header); version != "" {
			w.Header().Set(header, version)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 32<<20))
}

// approvalV1WriteResource reports whether a proxied resource is one of the
// legacy v1 approval write entries: create, update, submit, withdraw or decide.
//
// The frozen wire contract keeps v1 reads available for the whole audit window
// and deliberately leaves the v1 write entries in the allowlist, because a
// session that never entered the approval centre is still a v1 session. What
// must not happen is a session that already locked v2 completing a write
// through the v1 wire, and that is the single case the gate below closes.
func approvalV1WriteResource(method, resource string) bool {
	if resource == "/admin/v1/approval-requests" {
		return method == http.MethodPost
	}
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(path.Clean(resource), "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "approval-requests" {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	if len(parts) == 4 {
		return method == http.MethodPut
	}
	return len(parts) == 6 && parts[4] == "actions" &&
		(parts[5] == "submit" || parts[5] == "withdraw" || parts[5] == "decide") && method == http.MethodPost
}

// v2LockedApprovalSession reports whether this session must stay off the v1
// approval write entries. The selection is written once by the approval BFF and
// is authoritative for the whole session lifetime, so a v2 session keeps using
// the v2 entries instead of silently falling back to v1. Only an explicit v1
// selection - or no selection at all, which the frozen contract reads as v1 -
// may write through v1; an unreadable or unexpected selection fails closed,
// like the other governed approval surfaces.
func (s *Service) v2LockedApprovalSession(ctx context.Context, session Session) bool {
	reader, ok := s.store.(ApprovalWireLocker)
	if !ok {
		return true
	}
	wire, err := reader.ApprovalWireVersion(ctx, session)
	return err != nil || wire != 1
}

func (s *Service) captureApprovalClassification(ctx context.Context, session Session, method, resource string, response *http.Response) {
	parts := strings.Split(strings.Trim(path.Clean(resource), "/"), "/")
	if len(parts) < 3 || parts[0] != "admin" || (parts[1] != "v1" && parts[1] != "v2") || parts[2] != "approval-requests" {
		return
	}
	isCreate := parts[1] == "v1" && method == http.MethodPost && len(parts) == 3
	isDetail := method == http.MethodGet && len(parts) == 4
	isUpdate := parts[1] == "v1" && method == http.MethodPut && len(parts) == 4
	if !isCreate && !isDetail && !isUpdate {
		return
	}
	var expectedID uuid.UUID
	if !isCreate {
		var err error
		expectedID, err = uuid.Parse(parts[3])
		if err != nil {
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > 32<<20 {
		return
	}
	var projection struct {
		ID          string `json:"approval_request_id"`
		CommandType string `json:"command_type"`
	}
	if json.Unmarshal(body, &projection) != nil {
		return
	}
	requestID, err := uuid.Parse(projection.ID)
	commandType := strings.TrimSpace(projection.CommandType)
	if err != nil || requestID == uuid.Nil || (!isCreate && requestID != expectedID) || !canonicalApprovalCommandType(commandType) {
		return
	}
	expiresAt := s.now().Add(2 * time.Minute)
	if session.RefreshExpiresAt.Before(expiresAt) {
		expiresAt = session.RefreshExpiresAt
	}
	_ = s.store.SaveApprovalClassification(ctx, ApprovalClassification{
		SessionID:             session.ID,
		ActorTokenFingerprint: approvalActorTokenFingerprint(session.AccessToken),
		ApprovalRequestID:     requestID.String(),
		CommandType:           commandType,
		ExpiresAt:             expiresAt,
	})
}

func canonicalApprovalCommandType(value string) bool {
	if value == "" || len(value) > 80 || value != strings.ToUpper(value) {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' || index > 0 && character == '_' {
			continue
		}
		return false
	}
	return true
}

func approvalActorTokenFingerprint(accessToken string) string {
	digest := sha256.Sum256([]byte("portal-approval-actor-v1\x00" + accessToken))
	return hex.EncodeToString(digest[:])
}

func writeTokenAdminDeadlineError(w http.ResponseWriter, r *http.Request, class requestdeadline.Class) {
	switch class {
	case requestdeadline.SnapshotBuild:
		w.Header().Set("Retry-After", "5")
		api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "the pagination snapshot could not be built in time", httpserver.RequestID(r.Context()))
	case requestdeadline.Precheck:
		api.WriteError(w, http.StatusGatewayTimeout, "PRECHECK_TIMEOUT", "the impact precheck timed out", httpserver.RequestID(r.Context()))
	case requestdeadline.OverviewRead:
		code, message, ok := requestdeadline.OverviewTimeoutProblem(r.URL.Path)
		if !ok {
			api.WriteError(w, http.StatusGatewayTimeout, "QUERY_TIMEOUT", "the Token administration query timed out", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusGatewayTimeout, code, message, httpserver.RequestID(r.Context()))
	default:
		api.WriteError(w, http.StatusGatewayTimeout, "QUERY_TIMEOUT", "the pagination query timed out", httpserver.RequestID(r.Context()))
	}
}

func (s *Service) applyClientCompatibilityHeaders(request *http.Request, source string) {
	if source = strings.TrimSpace(source); source != "" {
		request.Header.Set("X-Pinvou-Client-Source", source)
		if len(s.config.TokenAdminProxyHMACKey) >= 32 {
			mac := hmac.New(sha256.New, s.config.TokenAdminProxyHMACKey)
			_, _ = mac.Write([]byte(request.Method + "\n" + request.URL.RequestURI() + "\n" + source))
			request.Header.Set("X-Pinvou-Client-Source-Signature", hex.EncodeToString(mac.Sum(nil)))
		}
	}
	if version := strings.TrimSpace(s.config.ClientVersion); version != "" {
		request.Header.Set(clientcompat.ClientVersionHeader, version)
	}
}

func (s *Service) authenticatedSession(ctx context.Context, r *http.Request) (Session, error) {
	cookie, err := r.Cookie(s.sessionCookieName())
	if err != nil || cookie.Value == "" {
		return Session{}, ErrSessionNotFound
	}
	session, err := s.store.GetSession(ctx, cookie.Value)
	if err != nil {
		return Session{}, err
	}
	if !session.RefreshExpiresAt.After(s.now()) {
		return Session{}, ErrSessionNotFound
	}
	if session.AccessExpiresAt.After(s.now().Add(30 * time.Second)) {
		return session, nil
	}
	if session.RefreshToken == "" {
		return Session{}, ErrSessionNotFound
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
			oidcClientID := latest.OIDCClientID
			if oidcClientID == "" {
				oidcClientID = s.config.ClientID
			}
			tokens, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {oidcClientID}, "refresh_token": {latest.RefreshToken}})
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
	requestForm := make(url.Values, len(form)+1)
	for key, values := range form {
		requestForm[key] = append([]string(nil), values...)
	}
	form = requestForm
	if form.Get("client_id") == s.config.HandoffClientID {
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
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body)
		return TokenResponse{}, &oidcTokenEndpointError{StatusCode: response.StatusCode, Code: body.Error}
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

func safeReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\r\n") {
		return "/"
	}
	return value
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

func accountPathAllowed(method, resource string) bool {
	if !strings.HasPrefix(resource, "/v1/") || strings.Contains(resource, "..") {
		return false
	}
	return method == http.MethodGet || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func tokenPathAllowed(method, resource string) bool {
	if method == http.MethodGet {
		switch resource {
		case "/v1/models", "/v1/recharge/products", "/v1/recharge/orders", "/v1/recharge/orders/recent", "/v1/me/balance", "/v1/me/entitlement-packages", "/v1/me/usage-requests", "/v1/me/usage-turns":
			return true
		}
		if orderID, ok := strings.CutPrefix(resource, "/v1/recharge/orders/"); ok {
			return safeProxyPathID(orderID)
		}
	}
	if method == http.MethodPost && resource == "/v1/recharge/orders" {
		return true
	}
	if method == http.MethodPost {
		if suffix, ok := strings.CutPrefix(resource, "/v1/recharge/orders/"); ok {
			parts := strings.Split(suffix, "/")
			_, parseErr := uuid.Parse(parts[0])
			return len(parts) == 2 && parseErr == nil && (parts[1] == "close" || parts[1] == "payment-claims")
		}
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "organizations" || !safeProxyPathID(parts[2]) {
		return false
	}
	if len(parts) == 4 && parts[3] == "quota-grants" {
		return method == http.MethodGet
	}
	if len(parts) == 4 && (parts[3] == "balance" || parts[3] == "entitlement-packages") {
		return method == http.MethodGet
	}
	if len(parts) == 4 && parts[3] == "model-policy" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if len(parts) == 4 && parts[3] == "quota-grants:batch" {
		return method == http.MethodPost
	}
	if len(parts) == 4 && parts[3] == "account-closed-grants:reclaim-batch" {
		return method == http.MethodPost
	}
	if len(parts) == 5 && parts[3] == "recharge" && parts[4] == "orders" {
		return method == http.MethodPost
	}
	if len(parts) == 6 && parts[3] == "recharge" && parts[4] == "orders" {
		return method == http.MethodGet && safeProxyPathID(parts[5])
	}
	return len(parts) == 6 && method == http.MethodPost && safeProxyPathID(parts[4]) && ((parts[3] == "quota-grants" && parts[5] == "revoke") || (parts[3] == "account-closed-grants" && parts[5] == "reclaim"))
}

func safeProxyPathID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.Contains(value, "/") && strings.IndexFunc(value, func(char rune) bool {
		return !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_')
	}) == -1
}

func portalClientSource(r *http.Request) string {
	peerText := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(peerText); err == nil {
		peerText = host
	}
	peer := net.ParseIP(peerText)
	// Forwarding headers are transport assertions made by the configured
	// private ingress, never by an Internet peer. This keeps forged public
	// X-Real-IP/X-Forwarded-For values out of the rate-limit identity.
	if peer != nil && (peer.IsLoopback() || peer.IsPrivate()) {
		if source := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); source != nil {
			return source.String()
		}
		if forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ","); len(forwarded) > 0 {
			if source := net.ParseIP(strings.TrimSpace(forwarded[0])); source != nil {
				return source.String()
			}
		}
	}
	if peer != nil {
		return peer.String()
	}
	return "untrusted"
}

func accountAdminPathAllowed(method, resource string) bool {
	if resource == "/admin/v1/sms-configuration" && (method == http.MethodGet || method == http.MethodPut) {
		return true
	}
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/roles", "/admin/v1/role-bindings", "/admin/v1/users", "/admin/v1/organizations", "/admin/v1/audit-logs", "/admin/v1/registration-operations", "/admin/v1/profile-change-requests", "/admin/v1/account-closure-requests", "/admin/v1/approval-policies", "/admin/v1/approval-requests") {
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
	if resource == "/admin/v1/audit-exports" && method == http.MethodPost {
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
	if len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "account-closure-requests" && parts[4] == "actions" && parts[5] == "retry" && method == http.MethodPost {
		_, err := uuid.Parse(parts[3])
		return err == nil
	}
	if len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "registration-operations" && parts[4] == "actions" && (parts[5] == "retry" || parts[5] == "compensate") {
		_, err := uuid.Parse(parts[3])
		return err == nil && method == http.MethodPost
	}
	if len(parts) == 6 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "profile-change-requests" && parts[4] == "actions" && parts[5] == "retry" {
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
	if len(parts) >= 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "audit-exports" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		if len(parts) == 4 {
			return method == http.MethodGet
		}
		if len(parts) == 5 && parts[4] == "download-tickets" {
			return method == http.MethodPost
		}
		return len(parts) == 5 && parts[4] == "download" && method == http.MethodGet
	}
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" || (parts[2] != "users" && parts[2] != "organizations") {
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	if method == http.MethodDelete && len(parts) == 4 && parts[2] == "organizations" {
		return true
	}
	if method == http.MethodGet && len(parts) == 5 && parts[2] == "organizations" && parts[4] == "deletion-readiness" {
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
	if resource == "/admin/v1/recharge-product-create-options" && method == http.MethodGet {
		return true
	}
	if resource == "/admin/v1/recharge-products" && (method == http.MethodGet || method == http.MethodPost) {
		return true
	}
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/finance-overviews/accounts", "/admin/v1/finance-overviews/payments", "/admin/v1/finance-overviews/pricing", "/admin/v1/finance-overviews/products", "/admin/v1/wallets", "/admin/v1/receivables", "/admin/v1/pricing-versions", "/admin/v1/payment-orders", "/admin/v1/refunds", "/admin/v1/adjustments", "/admin/v1/usage-holds", "/admin/v1/reconciliation-runs", "/admin/v1/late-payment-reviews", "/admin/v1/payment-anomalies") {
		return true
	}
	if (resource == "/admin/v1/reconciliation-runs" || resource == "/admin/v1/reconciliation-imports" || resource == "/admin/v1/adjustments") && method == http.MethodPost {
		return true
	}
	if (resource == "/admin/v1/pricing" || resource == "/admin/v1/metering-units") && method == http.MethodGet || (resource == "/admin/v1/pricing/drafts" || resource == "/admin/v1/metering-units") && method == http.MethodPost {
		return true
	}
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) == 5 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "models" && parts[4] == "price-projection" && method == http.MethodGet {
		_, err := uuid.Parse(parts[3])
		return err == nil
	}
	if len(parts) >= 5 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "recharge-products" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		if len(parts) == 5 {
			if parts[4] == "draft" {
				return method == http.MethodPut
			}
			if parts[4] == "admission-verifications" {
				return method == http.MethodGet || method == http.MethodPost
			}
			return method == http.MethodPost && (parts[4] == "publish" || parts[4] == "unpublish" || parts[4] == "delete" || parts[4] == "display-names")
		}
		if len(parts) == 7 && parts[4] == "admission-verifications" {
			if _, err := uuid.Parse(parts[5]); err != nil {
				return false
			}
			return method == http.MethodPost && (parts[6] == "retry" || parts[6] == "revoke")
		}
		return false
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && (parts[2] == "usage-holds" || parts[2] == "late-payment-reviews" || parts[2] == "payment-anomalies") {
		_, err := uuid.Parse(parts[3])
		return err == nil && method == http.MethodGet
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "payment-orders" {
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
	case "late-payment-reviews":
		return method == http.MethodGet || method == http.MethodPost && (parts[4] == "claim" || parts[4] == "query" || parts[4] == "schedule" || parts[4] == "archive" || parts[4] == "provider-blind")
	case "payment-anomalies":
		return method == http.MethodPost && (parts[4] == "claim" || parts[4] == "resolve")
	case "reconciliation-cases":
		return method == http.MethodPost && parts[4] == "commands"
	default:
		return false
	}
}

func tokenAdminPathAllowed(method, resource string) bool {
	if resource == "/admin/v1/token-resources/readiness" {
		return method == http.MethodGet
	}
	if resource == "/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness" {
		return method == http.MethodGet
	}
	if adminReadPathAllowed(method, resource, "/admin/v1/overview", "/admin/v1/operations-overview", "/admin/v1/overviews/model-catalog", "/admin/v1/overviews/token-resources", "/admin/v1/overviews/model-operations", "/admin/v1/token-accounts", "/admin/v1/usage-requests", "/admin/v1/models", "/admin/v1/provisionings", "/admin/v1/protocols", "/admin/v1/formulas", "/admin/v1/reconciliation-cases", "/admin/v1/owner-candidates", "/admin/v1/resource-audit") {
		return true
	}
	if strings.Contains(resource, "..") {
		return false
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) >= 5 && parts[0] == "admin" && parts[1] == "v2" && parts[2] == "token-accounts" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		if len(parts) == 5 && parts[4] == "routes" {
			return method == http.MethodPut || method == http.MethodDelete
		}
		if len(parts) == 6 && parts[4] == "impact" && parts[5] == "route" {
			return method == http.MethodGet
		}
		return false
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v2" && parts[2] == "token-accounts" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		return method == http.MethodGet
	}
	if len(parts) == 4 && parts[0] == "admin" && parts[1] == "v2" && parts[2] == "provisionings" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		return method == http.MethodGet
	}
	if len(parts) >= 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "credential-results" {
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		return len(parts) == 4 && method == http.MethodGet || len(parts) == 5 && parts[4] == "revoke-window" && method == http.MethodPost
	}
	if resource == "/admin/v1/protocols" {
		return method == http.MethodGet
	}
	if resource == "/admin/v1/commands" {
		return method == http.MethodPost
	}
	if resource == "/admin/v1/commands:precheck" {
		return method == http.MethodPost
	}
	if resource == "/admin/v1/commands/by-idempotency-key" || resource == "/admin/v1/commands/recovery-queue" {
		return method == http.MethodGet
	}
	if resource == "/admin/v1/model-catalog/impact/rollback" {
		return method == http.MethodGet
	}
	parts = strings.Split(strings.Trim(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" {
		return false
	}
	if parts[2] == "commands" {
		if len(parts) < 4 {
			return false
		}
		if _, err := uuid.Parse(parts[3]); err != nil {
			return false
		}
		if len(parts) == 4 {
			return method == http.MethodGet
		}
		if len(parts) == 5 && parts[4] == "recovery-actions" {
			return method == http.MethodPost
		}
		if len(parts) == 6 && parts[4] == "recovery-actions" && parts[5] == "by-idempotency-key" {
			return method == http.MethodGet
		}
		if len(parts) == 6 && parts[4] == "recovery-actions" {
			_, err := uuid.Parse(parts[5])
			return method == http.MethodGet && err == nil
		}
		return false
	}
	if _, err := uuid.Parse(parts[3]); err != nil {
		return false
	}
	if parts[2] == "usage-requests" {
		return (len(parts) == 4 && method == http.MethodGet) ||
			(len(parts) == 5 && parts[4] == "evidence" && method == http.MethodGet)
	}
	if parts[2] == "protocols" {
		return false
	}
	if parts[2] == "formulas" {
		return len(parts) == 5 && parts[4] == "preview" && method == http.MethodGet
	}
	if parts[2] == "models" {
		return (len(parts) == 4 && method == http.MethodGet) ||
			(len(parts) == 5 && parts[4] == "versions" && method == http.MethodGet) ||
			(len(parts) == 5 && parts[4] == "lifecycle-plans" && (method == http.MethodGet || method == http.MethodPost)) ||
			(len(parts) == 6 && parts[4] == "lifecycle-plans" && method == http.MethodGet && func() bool { _, err := uuid.Parse(parts[5]); return err == nil }()) ||
			(len(parts) == 7 && parts[4] == "lifecycle-plans" && parts[6] == "cancel" && method == http.MethodPost && func() bool { _, err := uuid.Parse(parts[5]); return err == nil }()) ||
			(len(parts) == 6 && parts[4] == "impact" && parts[5] == "lifecycle" && method == http.MethodGet)
	}
	if parts[2] == "provisionings" {
		return len(parts) == 4 && method == http.MethodGet ||
			len(parts) == 5 && parts[4] == "attempts" && method == http.MethodGet
	}
	if parts[2] != "token-accounts" {
		return false
	}
	if len(parts) == 4 {
		return method == http.MethodGet
	}
	if len(parts) == 5 {
		switch parts[4] {
		case "routes":
			return method == http.MethodGet || method == http.MethodPut || method == http.MethodDelete
		case "credentials":
			return method == http.MethodGet
		}
	}
	if len(parts) == 6 && parts[4] == "credential-launches" && (parts[5] == "enroll" || parts[5] == "rotate") {
		return method == http.MethodPost
	}
	if len(parts) == 6 && parts[4] == "impact" && (parts[5] == "status" || parts[5] == "route" || parts[5] == "credential") {
		return method == http.MethodGet
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
