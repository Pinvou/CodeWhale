package portal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// ApprovalClassification is a short-lived, server-authenticated proof used by
// the Portal to classify an approval action without trusting browser input or
// contacting Account after the model/Token action gate has been disabled.
type ApprovalClassification struct {
	SessionID             string    `json:"session_id"`
	ActorTokenFingerprint string    `json:"actor_token_fingerprint"`
	ApprovalRequestID     string    `json:"approval_request_id"`
	CommandType           string    `json:"command_type"`
	ExpiresAt             time.Time `json:"expires_at"`
}

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

func approvalActorTokenFingerprint(accessToken string) string {
	digest := sha256.Sum256([]byte("portal-approval-actor-v1\x00" + accessToken))
	return hex.EncodeToString(digest[:])
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

func (s *RedisStore) SaveApprovalClassification(ctx context.Context, proof ApprovalClassification) error {
	body, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	ttl := time.Until(proof.ExpiresAt)
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Set(ctx, approvalClassificationKey(proof.SessionID, proof.ApprovalRequestID), body, ttl).Err()
}

func (s *RedisStore) GetApprovalClassification(ctx context.Context, sessionID, approvalRequestID string) (ApprovalClassification, error) {
	value, err := s.client.Get(ctx, approvalClassificationKey(sessionID, approvalRequestID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ApprovalClassification{}, ErrSessionNotFound
	}
	var proof ApprovalClassification
	if err == nil {
		err = json.Unmarshal(value, &proof)
	}
	return proof, err
}

func approvalClassificationKey(sessionID, approvalRequestID string) string {
	digest := sha256.Sum256([]byte(sessionID + "\x00" + approvalRequestID))
	return "portal:approval-classification:" + hex.EncodeToString(digest[:])
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

// The version-neutral BFF classifies a draft before any Account mutation. Existing
// requests require the same session-bound proof captured by an authoritative detail.
func rejectDisabledBFFTokenDraft(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var input struct {
		CommandType string `json:"command_type"`
	}
	if json.Unmarshal(body, &input) != nil || !canonicalApprovalCommandType(input.CommandType) {
		api.WriteError(w, 503, "APPROVAL_CLASSIFICATION_UNAVAILABLE", "the approval request could not be classified", httpserver.RequestID(r.Context()))
		return true
	}
	if strings.HasPrefix(input.CommandType, "TOKEN_") {
		api.WriteError(w, 404, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return true
	}
	return false
}
