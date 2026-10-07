package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	ErrTokenOwnerCandidateCursorExpired = errors.New("token owner candidate cursor expired")
	ErrTokenOwnerCandidateRateLimited   = errors.New("token owner candidate query rate limited")
	ErrInvalidTokenOwnerCandidateCursor = errors.New("invalid token owner candidate cursor")
	ErrInvalidTokenOwnerCandidateFilter = errors.New("invalid token owner candidate filter")
	ErrInvalidTokenOwnerCandidateLimit  = errors.New("invalid token owner candidate limit")
)

type TokenOwnerCandidate struct {
	ID           uuid.UUID `json:"id"`
	Type         string    `json:"type"`
	DisplayLabel string    `json:"display_label"`
	Status       string    `json:"status"`
	OwnerVersion int64     `json:"-"`
}

type tokenOwnerCandidateCursor struct {
	Version              int       `json:"version"`
	Resource             string    `json:"resource"`
	CallerService        string    `json:"caller_service"`
	CallerSubject        string    `json:"caller_subject"`
	ActorIssuer          string    `json:"actor_issuer"`
	ActorSubject         string    `json:"actor_subject"`
	AuthorizationVersion int64     `json:"authorization_version"`
	FilterFingerprint    string    `json:"filter_fingerprint"`
	ScanID               uuid.UUID `json:"scan_id"`
	HighWater            time.Time `json:"high_water"`
	OwnerType            string    `json:"owner_type"`
	OwnerID              uuid.UUID `json:"owner_id"`
	IssuedAt             time.Time `json:"issued_at"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type TokenOwnerCandidateFilter struct {
	OwnerType      string
	OrganizationID *uuid.UUID
	Limit          int
	Cursor         *tokenOwnerCandidateCursor
	CursorErr      error
}

type TokenOwnerCandidateActor struct {
	Issuer        string
	Subject       string
	CallerService string
	CallerSubject string
}

type TokenOwnerCandidatePage struct {
	Items                []TokenOwnerCandidate `json:"items"`
	NextCursor           *string               `json:"next_cursor"`
	HasMore              bool                  `json:"has_more"`
	Limit                int                   `json:"limit"`
	AuthorizationVersion int64                 `json:"authorization_version"`
	OwnerVersions        map[string]int64      `json:"owner_versions"`
	next                 *tokenOwnerCandidateCursor
}

type tokenOwnerCandidateRepository interface {
	TokenOwnerCandidates(context.Context, TokenOwnerCandidateActor, TokenOwnerCandidateFilter) (TokenOwnerCandidatePage, error)
}

func parseTokenOwnerCandidateFilter(r *http.Request) (TokenOwnerCandidateFilter, error) {
	query := r.URL.Query()
	for key := range query {
		if !slices.Contains([]string{"actor_issuer", "actor_subject", "owner_type", "organization_id", "limit", "cursor"}, key) {
			return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
		}
	}
	issuer, subject := strings.TrimSpace(query.Get("actor_issuer")), strings.TrimSpace(query.Get("actor_subject"))
	if issuer == "" || subject == "" || len(issuer) > 512 || len(subject) > 255 || strings.ContainsAny(issuer+subject, "\r\n") {
		return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
	}
	filter := TokenOwnerCandidateFilter{
		OwnerType: strings.ToUpper(strings.TrimSpace(query.Get("owner_type"))),
		Limit:     20000,
	}
	if filter.OwnerType != "" && filter.OwnerType != "USER" {
		return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
	}
	if query.Get("organization_id") != "" {
		return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateFilter
	}

	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > 50000 {
			return TokenOwnerCandidateFilter{}, ErrInvalidTokenOwnerCandidateLimit
		}
		filter.Limit = limit
	}
	return filter, nil
}

func tokenOwnerCandidateFilterFingerprint(filter TokenOwnerCandidateFilter) string {
	organizationID := ""
	if filter.OrganizationID != nil {
		organizationID = filter.OrganizationID.String()
	}
	body := strings.Join([]string{filter.OwnerType, organizationID, strconv.Itoa(filter.Limit)}, "\x00")
	digest := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", digest[:])
}

func (s *Service) internalTokenOwnerCandidates(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(tokenOwnerCandidateRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || strings.TrimSpace(workload.Service) == "" || strings.TrimSpace(workload.Subject) == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "trusted service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	if len(s.ownerCandidateCursorKey) < 32 {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory cursor signing is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	actor := TokenOwnerCandidateActor{
		Issuer:        strings.TrimSpace(r.URL.Query().Get("actor_issuer")),
		Subject:       strings.TrimSpace(r.URL.Query().Get("actor_subject")),
		CallerService: workload.Service,
		CallerSubject: workload.Subject,
	}
	filter, err := parseTokenOwnerCandidateFilter(r)
	if err != nil {
		status, code, message := http.StatusBadRequest, "INVALID_OWNER_CANDIDATE_FILTER", "owner candidate filters are invalid"
		if errors.Is(err, ErrInvalidTokenOwnerCandidateCursor) {
			code, message = "INVALID_CURSOR", "the owner candidate cursor is invalid"
		} else if errors.Is(err, ErrInvalidTokenOwnerCandidateLimit) {
			code, message = "INVALID_LIMIT", "owner candidate limit must be between 1 and 50000"
		}
		api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
		return
	}
	if rawCursor := strings.TrimSpace(r.URL.Query().Get("cursor")); rawCursor != "" {
		if len(rawCursor) > 4096 {
			filter.CursorErr = ErrInvalidTokenOwnerCandidateCursor
		} else {
			cursor, cursorErr := decodeTokenOwnerCandidateCursor(rawCursor, s.ownerCandidateCursorKey, actor, filter, s.now())
			if cursorErr != nil {
				filter.CursorErr = cursorErr
			} else {
				filter.Cursor = cursor
			}
		}
	}
	page, err := repository.TokenOwnerCandidates(r.Context(), actor, filter)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenOwnerCandidateRateLimited):
			w.Header().Set("Retry-After", "60")
			api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "the owner candidate query rate was exceeded", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrTokenOwnerCandidateCursorExpired):
			api.WriteError(w, http.StatusGone, "CURSOR_EXPIRED", "the owner candidate cursor no longer matches the current authorization", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrInvalidTokenOwnerCandidateCursor):
			api.WriteError(w, http.StatusBadRequest, "INVALID_CURSOR", "the owner candidate cursor is invalid", httpserver.RequestID(r.Context()))
		case errors.Is(err, ErrForbidden):
			api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		default:
			s.writeError(w, r, err)
		}
		return
	}
	if page.next != nil {
		cursor := *page.next
		cursor.Version = 1
		cursor.Resource = tokenOwnerCandidateCursorResource
		cursor.CallerService = actor.CallerService
		cursor.CallerSubject = actor.CallerSubject
		cursor.ActorIssuer = actor.Issuer
		cursor.ActorSubject = actor.Subject
		cursor.FilterFingerprint = tokenOwnerCandidateFilterFingerprint(filter)
		encoded, cursorErr := encodeTokenOwnerCandidateCursor(cursor, s.ownerCandidateCursorKey)
		if cursorErr != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "OWNER_DIRECTORY_UNAVAILABLE", "owner directory cursor signing is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		page.NextCursor = &encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, page)
}
