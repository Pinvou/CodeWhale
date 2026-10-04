package portal

import (
	"bytes"
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
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/coreos/go-oidc/v3/oidc"
)

type Config struct {
	Environment      string
	IssuerURL        *url.URL
	JWKSURL          *url.URL
	PublicURL        *url.URL
	AuthorizationURL *url.URL
	TokenURL         *url.URL
	ClientID         string
	AccountURL       *url.URL
	TokenGatewayURL  *url.URL
	SecureCookies    bool
}

type Service struct {
	config   Config
	store    Store
	client   *http.Client
	now      func() time.Time
	verifier *oidc.IDTokenVerifier
}

func NewService(config Config, store Store) (*Service, error) {
	config.ClientID = strings.TrimSpace(config.ClientID)
	if config.PublicURL == nil || config.AuthorizationURL == nil || config.TokenURL == nil || config.AccountURL == nil || config.TokenGatewayURL == nil || config.IssuerURL == nil || config.JWKSURL == nil || strings.TrimSpace(config.ClientID) == "" || store == nil {
		return nil, errors.New("invalid portal configuration")
	}
	if config.PublicURL.Scheme != "https" && config.SecureCookies {
		return nil, errors.New("secure portal cookies require an HTTPS public URL")
	}
	development := config.Environment == "development" || config.Environment == "test"
	for _, endpoint := range []*url.URL{config.PublicURL, config.AuthorizationURL, config.TokenURL, config.AccountURL, config.TokenGatewayURL, config.IssuerURL, config.JWKSURL} {
		if endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.Fragment != "" {
			return nil, errors.New("portal endpoints must be absolute HTTP(S) URLs without credentials or fragments")
		}
	}
	if !development {
		if !config.SecureCookies {
			return nil, errors.New("portal cookies must be secure outside development and test")
		}
		for _, endpoint := range []*url.URL{config.AuthorizationURL, config.TokenURL, config.IssuerURL, config.JWKSURL} {
			if endpoint.Scheme != "https" {
				return nil, errors.New("portal OIDC endpoints must use HTTPS outside development and test")
			}
		}
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	keyClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: client.CheckRedirect}
	keyContext := oidc.ClientContext(context.Background(), keyClient)
	verifier := oidc.NewVerifier(strings.TrimRight(config.IssuerURL.String(), "/"), oidc.NewRemoteKeySet(keyContext, config.JWKSURL.String()), &oidc.Config{ClientID: config.ClientID})
	return &Service{config: config, store: store, client: client, verifier: verifier, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("GET /api/session", s.sessionStatus)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("/api/account/{resource...}", s.proxyAccount)
	mux.HandleFunc("/api/token/{resource...}", s.proxyToken)
	if static != nil {
		mux.Handle("/", static)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		mux.ServeHTTP(w, r)
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
	query.Set("scope", "openid profile email")
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
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGIN_UNAVAILABLE", "login state is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrSessionNotFound) || !flow.ExpiresAt.After(s.now()) {
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
	subject, err := s.verifyIDToken(r.Context(), tokens.IDToken, tokens.AccessToken, flow.Nonce, "", true)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "LOGIN_EXCHANGE_FAILED", "login identity could not be verified", httpserver.RequestID(r.Context()))
		return
	}
	now := s.now()
	sessionID, sessionIDErr := randomToken(32)
	csrfToken, csrfErr := randomToken(32)
	session := Session{ID: sessionID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, AccessExpiresAt: now.Add(time.Duration(tokens.ExpiresIn) * time.Second), RefreshExpiresAt: now.Add(time.Duration(tokens.RefreshExpiresIn) * time.Second), CSRFToken: csrfToken, CreatedAt: now, Subject: subject, Nonce: flow.Nonce}
	if sessionIDErr != nil || csrfErr != nil || !validTokenResponse(tokens, true) || s.store.SaveSession(r.Context(), session) != nil {
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
		if errors.Is(err, ErrSessionNotFound) {
			api.WriteJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		} else {
			api.WriteError(w, http.StatusServiceUnavailable, "SESSION_UNAVAILABLE", "the login session is temporarily unavailable", httpserver.RequestID(r.Context()))
		}
		return
	}
	s.setSessionCookies(w, session)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": session.CSRFToken})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	// Logout does not refresh tokens. Refresh writes are fenced against this
	// deletion, so an in-flight exchange cannot recreate the session.
	session, err := s.storedSession(r.Context(), r)
	if err != nil {
		s.writeSessionError(w, r, err)
		return
	}
	if !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.store.DeleteSession(r.Context(), session.ID); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "LOGOUT_UNAVAILABLE", "logout could not be completed", httpserver.RequestID(r.Context()))
		return
	}
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

func (s *Service) proxy(w http.ResponseWriter, r *http.Request, upstream *url.URL, resource string, allowed func(string, string) bool) {
	resource = path.Clean(resource)
	if !allowed(r.Method, resource) {
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
	if !session.AccessExpiresAt.After(s.now().Add(30 * time.Second)) {
		session, err = s.refreshSession(r.Context(), session)
		if err != nil {
			s.writeSessionError(w, r, err)
			return
		}
	}
	s.setSessionCookies(w, session)
	target := *upstream
	target.Path = strings.TrimRight(target.Path, "/") + resource
	target.RawQuery = r.URL.RawQuery
	payload, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil || len(payload) > 4<<20 {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "the portal request body is incomplete or too large", httpserver.RequestID(r.Context()))
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(payload))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
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
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil || len(responseBody) > 32<<20 {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the platform response is incomplete or too large", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
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
			if latest.ID != session.ID || latest.Subject == "" || latest.Nonce == "" || !latest.RefreshExpiresAt.After(s.now()) {
				return Session{}, ErrSessionNotFound
			}
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
			updated.AccessExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
			if tokens.RefreshToken != "" {
				updated.RefreshToken = tokens.RefreshToken
			}
			updated.RefreshExpiresAt = now.Add(time.Duration(tokens.RefreshExpiresIn) * time.Second)
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
		if response.StatusCode == http.StatusBadRequest {
			var failure struct {
				Code string `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&failure) == nil && failure.Code == "invalid_grant" {
				return TokenResponse{}, ErrSessionNotFound
			}
		}
		return TokenResponse{}, fmt.Errorf("OIDC token endpoint returned HTTP %d", response.StatusCode)
	}
	var tokens TokenResponse
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return TokenResponse{}, errors.New("OIDC token response is incomplete or too large")
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
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
	origin := r.Header.Get("Origin")
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
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") {
		return "/"
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7F {
			return "/"
		}
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
	if !strings.HasPrefix(resource, "/v1/") || resource == "/v1/chat/completions" || strings.Contains(resource, "..") {
		return false
	}
	return method == http.MethodGet || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}
