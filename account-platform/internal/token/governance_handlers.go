package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type tokenGovernanceHandler struct {
	family    string
	transport string
	handle    func(*Service, http.ResponseWriter, *http.Request)
}

// This is the dispatch table itself, not a parallel readiness inventory.
// Removing an entry removes its HTTP dispatch and changes the manifest.
var tokenGovernanceHandlers map[string]tokenGovernanceHandler

func init() {
	tokenGovernanceHandlers = map[string]tokenGovernanceHandler{
		"TOKEN_USAGE_RECONCILIATION_RETRY": {"RUNTIME", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_PROTOCOL_DRAFT_CREATE":      {"PROTOCOL_FORMULA", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_PROTOCOL_PUBLISH":           {"PROTOCOL_FORMULA", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_FORMULA_DRAFT_CREATE":       {"PROTOCOL_FORMULA", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_FORMULA_PUBLISH":            {"PROTOCOL_FORMULA", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_MODEL_DRAFT_SAVE":           {"MODEL_ACCOUNT", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_MODEL_PUBLISH":              {"MODEL_ACCOUNT", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_MODEL_LIFECYCLE_CHANGE":     {"LIFECYCLE", "APPROVED", approvedHandler("TOKEN_MODEL_LIFECYCLE_CHANGE")},
		"TOKEN_MODEL_CATALOG_SYNC":         {"CATALOG_SYNC", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_MODEL_CATALOG_ROLLBACK":     {"CATALOG_ROLLBACK", "APPROVED", approvedHandler("TOKEN_MODEL_CATALOG_ROLLBACK")},
		"TOKEN_ACCOUNT_CREATE":             {"MODEL_ACCOUNT", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_ACCOUNT_STATUS_CHANGE":      {"MODEL_ACCOUNT", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_ROUTE_UPSERT":               {"ROUTE", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_ROUTE_DELETE":               {"ROUTE", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_CREDENTIAL_ENROLL":          {"CREDENTIAL_ENROLL", "ISOLATED", isolatedHandler("ENROLL")},
		"TOKEN_CREDENTIAL_ROTATE":          {"CREDENTIAL_ROTATE", "ISOLATED", isolatedHandler("ROTATE")},
		"TOKEN_CREDENTIAL_REVOKE":          {"CREDENTIAL_REVOKE", "APPROVED", approvedHandler("TOKEN_CREDENTIAL_REVOKE")},
		"TOKEN_CREDENTIAL_VALIDATE":        {"CREDENTIAL_VALIDATE", "COMMAND", (*Service).adminCreateTokenCommandBody},
		"TOKEN_ROUTE_PROVISIONING_REPAIR":  {"RUNTIME", "COMMAND", (*Service).adminCreateTokenCommandBody},
	}
}

func approvedHandler(command string) func(*Service, http.ResponseWriter, *http.Request) {
	return func(s *Service, w http.ResponseWriter, r *http.Request) { s.executeApprovedTokenCommand(w, r, command) }
}

func isolatedHandler(action string) func(*Service, http.ResponseWriter, *http.Request) {
	return func(s *Service, w http.ResponseWriter, r *http.Request) {
		s.adminCreateCredentialLaunchFor(action)(w, r)
	}
}

func tokenCommandFamily(command string) string { return tokenGovernanceHandlers[command].family }

type TokenCommandHandlerDescriptor struct {
	CommandType   string   `json:"command_type"`
	Family        string   `json:"family"`
	Transport     string   `json:"transport"`
	Version       int      `json:"handler_contract_version"`
	Permissions   []string `json:"permissions"`
	RecentAuth    bool     `json:"recent_auth"`
	Approval      bool     `json:"approval"`
	RequiredFacts []string `json:"required_facts"`
}

func TokenCommandHandlerManifest() ([]TokenCommandHandlerDescriptor, error) {
	// F05 installs the complete management set and the actual AuditOnly usage retry.
	if len(tokenGovernanceHandlers) != 19 || len(tokenGovernanceHandlers) != len(tokenCommandSpecs) {
		return nil, fmt.Errorf("Token command handler set is incomplete")
	}
	result := make([]TokenCommandHandlerDescriptor, 0, len(tokenGovernanceHandlers))
	for command, handler := range tokenGovernanceHandlers {
		spec, ok := tokenCommandSpecs[command]
		if !ok || handler.handle == nil || handler.family == "" || (spec.Approval != (handler.transport == "APPROVED")) {
			return nil, fmt.Errorf("Token command handler is invalid: %s", command)
		}
		result = append(result, TokenCommandHandlerDescriptor{command, handler.family, handler.transport, 1, slices.Clone(spec.Permissions), spec.RecentAuth, spec.Approval, slices.Clone(spec.RequiredFacts)})
	}
	slices.SortFunc(result, func(a, b TokenCommandHandlerDescriptor) int { return strings.Compare(a.CommandType, b.CommandType) })
	return result, nil
}

func TokenCommandHandlerDigest() (string, error) {
	manifest, err := TokenCommandHandlerManifest()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) registeredTokenHandler(command, transport string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler, ok := tokenGovernanceHandlers[command]
		if !ok || handler.transport != transport || handler.handle == nil {
			api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_EXECUTOR_UNAVAILABLE", "the governed command handler is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		handler.handle(s, w, r)
	}
}

func (s *Service) adminCreateTokenCommand(w http.ResponseWriter, r *http.Request) {
	// Decode once to select the actual registered transport. The existing
	// authoritative parser still validates the complete closed envelope.
	var envelope TokenCommandEnvelope
	if api.DecodeJSON(w, r, &envelope) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed command body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	command := strings.ToUpper(strings.TrimSpace(envelope.CommandType))
	if command == "TOKEN_CREDENTIAL_VALIDATE" {
		deadline, selected := r.Context().Value(credentialEntryDeadlineKey{}).(time.Time)
		if !selected || !deadline.After(time.Now()) {
			api.WriteError(w, http.StatusServiceUnavailable, "CREDENTIAL_REQUEST_DEADLINE_EXCEEDED", "the credential request deadline is exhausted", httpserver.RequestID(r.Context()))
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		r = r.WithContext(ctx)
	}
	handler, ok := tokenGovernanceHandlers[command]
	if !ok || handler.transport != "COMMAND" || handler.handle == nil {
		api.WriteError(w, http.StatusForbidden, "DIRECT_APPROVAL_COMMAND_FORBIDDEN", "use the command's approved or isolated workflow", httpserver.RequestID(r.Context()))
		return
	}
	raw, _ := json.Marshal(envelope)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	handler.handle(s, w, r)
}
