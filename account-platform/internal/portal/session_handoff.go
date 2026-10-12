package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

const sessionHandoffBodyLimit = 16 * 1024

var errAccountIdentityRejected = errors.New("account identity rejected")

func (s *Service) createSessionHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.config.HandoffSubjectTokenEnabled {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "desktop session handoff is not enabled", httpserver.RequestID(r.Context()))
		return
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "a desktop access token is required", httpserver.RequestID(r.Context()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sessionHandoffBodyLimit+1))
	if err != nil || len(body) > sessionHandoffBodyLimit {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "the session handoff request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if len(strings.TrimSpace(string(body))) != 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "the session handoff request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	access := strings.TrimSpace(authorization[len(prefix):])
	expectedUser, err := s.accountUserID(r.Context(), access)
	if err != nil {
		if errors.Is(err, errAccountIdentityRejected) {
			api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "the desktop session could not be verified", httpserver.RequestID(r.Context()))
		} else {
			api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the account identity could not be verified", httpserver.RequestID(r.Context()))
		}
		return
	}
	exchangeStartedAt := s.now()
	tokens, exchangeErr := s.exchange(r.Context(), url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":          {s.config.HandoffClientID},
		"subject_token":      {access},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":              {"openid"},
	})
	if exchangeErr != nil {
		if definiteHandoffAuthRejection(exchangeErr) {
			api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "a portal session could not be exchanged", httpserver.RequestID(r.Context()))
		} else {
			api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the identity provider could not exchange the portal session", httpserver.RequestID(r.Context()))
		}
		return
	}
	refreshedUser, err := s.accountUserID(r.Context(), tokens.AccessToken)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the exchanged portal identity could not be verified", httpserver.RequestID(r.Context()))
		return
	}
	if refreshedUser != expectedUser {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the exchanged portal identity did not match", httpserver.RequestID(r.Context()))
		return
	}
	now := s.now()
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > int64((1<<63-1)/time.Second) || !strings.EqualFold(tokens.TokenType, "Bearer") {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the identity provider returned an invalid portal session", httpserver.RequestID(r.Context()))
		return
	}
	accessExpiresAt := exchangeStartedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	handoffExpiresAt := now.Add(s.config.SessionHandoffTTL)
	latestUsableHandoff := accessExpiresAt.Add(-30 * time.Second)
	if handoffExpiresAt.After(latestUsableHandoff) {
		handoffExpiresAt = latestUsableHandoff
	}
	if !handoffExpiresAt.After(now) {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the exchanged portal session expires too soon", httpserver.RequestID(r.Context()))
		return
	}
	sessionID, sessionIDErr := randomToken(32)
	csrfToken, csrfTokenErr := randomToken(32)
	ticket, ticketErr := randomToken(48)
	digest := sha256.Sum256([]byte(ticket))
	session := Session{ApprovalV2Eligible: true,
		ID: sessionID, AccessToken: tokens.AccessToken,
		AccessExpiresAt:  accessExpiresAt,
		RefreshExpiresAt: accessExpiresAt,
		CSRFToken:        csrfToken, CreatedAt: now,
	}
	session.LineageRoot = session.ID
	session.OIDCClientID = s.config.HandoffClientID
	if sessionIDErr != nil || csrfTokenErr != nil || ticketErr != nil {
		api.WriteError(w, http.StatusInternalServerError, "SESSION_HANDOFF_FAILED", "the session handoff could not be created", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.store.SaveSessionHandoff(r.Context(), hex.EncodeToString(digest[:]), SessionHandoff{Session: session, ExpiresAt: handoffExpiresAt}); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the session handoff could not be stored", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"ticket": ticket, "ticket_expires_in": int(handoffExpiresAt.Sub(now) / time.Second),
	})
}

func (s *Service) consumeSessionHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.config.HandoffSubjectTokenEnabled {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "desktop session handoff is not enabled", httpserver.RequestID(r.Context()))
		return
	}
	if !s.sameOrigin(r) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin is invalid", httpserver.RequestID(r.Context()))
		return
	}
	mediaType, _, mediaTypeErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaTypeErr != nil || mediaType != "application/json" {
		api.WriteError(w, http.StatusUnsupportedMediaType, "INVALID_REQUEST", "the request must be JSON", httpserver.RequestID(r.Context()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sessionHandoffBodyLimit+1))
	if err != nil || len(body) > sessionHandoffBodyLimit {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "the session handoff request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var request struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Ticket) < 16 || len(request.Ticket) > 256 {
		api.WriteError(w, http.StatusBadRequest, "SESSION_HANDOFF_INVALID", "the session handoff ticket is invalid", httpserver.RequestID(r.Context()))
		return
	}
	digest := sha256.Sum256([]byte(request.Ticket))
	handoff, err := s.store.TakeSessionHandoff(r.Context(), hex.EncodeToString(digest[:]))
	if errors.Is(err, ErrSessionNotFound) {
		api.WriteError(w, http.StatusUnauthorized, "SESSION_HANDOFF_INVALID", "the session handoff ticket is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the session handoff store is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !handoff.ExpiresAt.After(s.now()) {
		api.WriteError(w, http.StatusUnauthorized, "SESSION_HANDOFF_INVALID", "the session handoff ticket is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	previousSession, previousErr := r.Cookie(s.sessionCookieName())
	previousSessionID := ""
	if previousErr == nil && previousSession.Value != handoff.Session.ID {
		previousSessionID = strings.TrimSpace(previousSession.Value)
	}
	replaceErr := s.store.ReplaceSessionFromHandoff(r.Context(), previousSessionID, handoff.Session, s.logoutMarkerExpiry(Session{}))
	if replaceErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SESSION_HANDOFF_UNAVAILABLE", "the previous portal session could not be replaced", httpserver.RequestID(r.Context()))
		return
	}
	s.setCookie(w, s.sessionCookieName(), handoff.Session.ID, handoff.Session.RefreshExpiresAt, true)
	s.setCookie(w, s.csrfCookieName(), handoff.Session.CSRFToken, handoff.Session.RefreshExpiresAt, false)
	s.expireCookie(w, s.loggedOutCookieName(), true)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]string{"redirect_to": "/"})
}

func (s *Service) sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "same-origin")
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	public := *s.config.PublicURL
	return parsed.Scheme == public.Scheme && parsed.Host == public.Host
}

func (s *Service) accountUserID(ctx context.Context, token string) (string, error) {
	endpoint := *s.config.AccountURL
	endpoint.Path = "/v1/me"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return "", errAccountIdentityRejected
	}
	if response.StatusCode != http.StatusOK {
		return "", errors.New("account identity verification failed")
	}
	var identity struct {
		UserID string `json:"user_id"`
	}
	if err := decodePortalJSON(response.Body, &identity, 64<<10); err != nil {
		return "", err
	}
	if id, err := uuid.Parse(identity.UserID); id == uuid.Nil || err != nil {
		return "", errors.New("account identity is invalid")
	}
	return identity.UserID, nil
}

func definiteHandoffAuthRejection(err error) bool {
	if errors.Is(err, ErrSessionNotFound) {
		return true
	}
	var endpointError *oidcTokenEndpointError
	if !errors.As(err, &endpointError) {
		return false
	}
	return (endpointError.StatusCode == http.StatusBadRequest || endpointError.StatusCode == http.StatusUnauthorized) && (endpointError.Code == "invalid_grant" || endpointError.Code == "invalid_token" || endpointError.Code == "expired_token")
}
