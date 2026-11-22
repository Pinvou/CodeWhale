package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type governedRouteCommand struct {
	AccountID         uuid.UUID
	ModelID           uuid.UUID
	ProtocolVersionID uuid.UUID
	RouteID           uuid.UUID
	Priority          int
	DefaultScope      string
}

func isRouteGovernedCommand(commandType string) bool {
	return tokenCommandFamily(commandType) == "ROUTE"
}

func (s *Service) adminRouteCommandByIdempotencyKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return
	}
	commandType := r.URL.Query().Get("command_type")
	actor, _, err := s.authorizedTokenCommandActor(r.Context(), principal, commandType)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "idempotency key is invalid", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "command result is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	digest := sha256.Sum256([]byte(key))
	item, err := repository.tokenCommandByScopedKey(r.Context(), actor, commandType, hex.EncodeToString(digest[:]))
	if errors.Is(err, ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "command result is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func parseGovernedRouteCommand(commandType, target string, raw json.RawMessage) (governedRouteCommand, error) {
	var result governedRouteCommand
	id, err := uuid.Parse(target)
	if err != nil || id == uuid.Nil {
		return result, ErrTokenCommandInvalid
	}
	switch commandType {
	case "TOKEN_ROUTE_UPSERT":
		var command struct {
			AccountID         uuid.UUID `json:"token_account_id"`
			ModelID           uuid.UUID `json:"model_id"`
			ProtocolVersionID uuid.UUID `json:"protocol_version_id"`
			Priority          *int      `json:"priority"`
			DefaultScope      string    `json:"default_scope"`
		}
		if decodeCommand(raw, &command) != nil || command.AccountID != id || command.ModelID == uuid.Nil || command.ProtocolVersionID == uuid.Nil || command.Priority == nil || *command.Priority < 0 || *command.Priority > 10000 || !slices.Contains([]string{"NONE", "ORG", "UNIT"}, command.DefaultScope) {
			return result, ErrTokenCommandInvalid
		}
		result = governedRouteCommand{AccountID: command.AccountID, ModelID: command.ModelID, ProtocolVersionID: command.ProtocolVersionID, Priority: *command.Priority, DefaultScope: command.DefaultScope}
	case "TOKEN_ROUTE_DELETE":
		var command struct {
			AccountID uuid.UUID `json:"token_account_id"`
			RouteID   uuid.UUID `json:"route_id"`
		}
		if decodeCommand(raw, &command) != nil || command.AccountID == uuid.Nil || command.RouteID != id {
			return result, ErrTokenCommandInvalid
		}
		result.AccountID, result.RouteID = command.AccountID, command.RouteID
	default:
		return result, ErrTokenCommandInvalid
	}
	return result, nil
}

func routeCommandPrecheckTx(ctx context.Context, tx pgx.Tx, commandType, target string, raw json.RawMessage) (commandPrecheckSnapshot, error) {
	command, err := parseGovernedRouteCommand(commandType, target, raw)
	if err != nil {
		return commandPrecheckSnapshot{}, err
	}
	action := "UPSERT"
	if commandType == "TOKEN_ROUTE_DELETE" {
		action = "DELETE"
	}
	evidence, err := stableTokenRouteImpactTx(ctx, tx, command.AccountID, StableRouteImpactInput{Action: action, ModelID: command.ModelID, ProtocolVersionID: command.ProtocolVersionID, RouteID: command.RouteID})
	if err != nil {
		return commandPrecheckSnapshot{}, err
	}
	expected := map[string]any{"token_account_id": command.AccountID, "expected_account_version": *evidence.CurrentVersion.AccountVersion, "expected_route_version": *evidence.CurrentVersion.RouteVersion}
	blockers := []string{}
	if action == "UPSERT" {
		expected["model_id"], expected["protocol_version_id"] = command.ModelID, command.ProtocolVersionID
		account, accountErr := getTokenAccount(ctx, tx, command.AccountID, false)
		if accountErr != nil {
			return commandPrecheckSnapshot{}, accountErr
		}
		if !defaultScopeAllowed(account.OwnerType, command.DefaultScope) {
			blockers = append(blockers, "ROUTE_SCOPE_INVALID")
		}
		reference, referenceErr := resolveStableRouteReference(ctx, tx, command.ModelID, command.ProtocolVersionID)
		if referenceErr != nil {
			return commandPrecheckSnapshot{}, referenceErr
		}
		var credentialAvailable bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials WHERE token_account_id=$1 AND provider=$2 AND status='ACTIVE' AND validation_status='VALID' AND $3=ANY(allowed_models))`, command.AccountID, reference.Provider, reference.UpstreamModel).Scan(&credentialAvailable); err != nil {
			return commandPrecheckSnapshot{}, err
		}
		if !credentialAvailable {
			blockers = append(blockers, "VALID_CREDENTIAL_MISSING")
		}
	} else {
		expected["route_id"] = command.RouteID
		if evidence.Dependencies.AffectedCoverageTuples > 0 {
			blockers = append(blockers, "READY_PROVISIONING_DEPENDS_ON_ROUTE")
		}
		if evidence.Dependencies.CoverageGaps > 0 {
			blockers = append(blockers, "READY_COVERAGE_WOULD_BE_LOST")
		}
		if evidence.Dependencies.InflightRequests > 0 {
			blockers = append(blockers, "INFLIGHT_REQUESTS_PRESENT")
		}
		if evidence.Dependencies.ActiveLeases > 0 {
			blockers = append(blockers, "ACTIVE_LEASES_PRESENT")
		}
	}
	// The generic envelope targets the account for create/upsert (a route ID
	// need not exist yet); retain precise selected-route evidence underneath.
	evidence.ResourceID = target
	return commandPrecheckSnapshot{Status: evidence.CurrentStatus, ExpectedVersions: expected, EvidenceValues: []string{evidence.CurrentVersion.EvidenceETag, evidence.CurrentVersion.EvidenceSetSHA256}, Blockers: blockers, LocalEvidence: &evidence}, nil
}

type stableRouteDeleteResult struct {
	RouteID             uuid.UUID `json:"route_id"`
	AccountID           uuid.UUID `json:"token_account_id"`
	ModelID             uuid.UUID `json:"model_id"`
	ProtocolVersionID   uuid.UUID `json:"protocol_version_id"`
	Deleted             bool      `json:"deleted"`
	DeletedRouteVersion int64     `json:"deleted_route_version"`
	RouteVersion        int64     `json:"route_version"`
	RouteGeneration     int64     `json:"route_generation"`
	AccountVersion      int64     `json:"account_version"`
	DeletedAt           time.Time `json:"deleted_at"`
}

func (r *PostgresRepository) executeGovernedRouteCommand(ctx context.Context, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	envelope := input.Envelope
	command, err := parseGovernedRouteCommand(envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil || input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	policy, err := approvalpolicy.Lookup(envelope.CommandType)
	if err != nil || policy.Mode != approvalpolicy.AuditOnly {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	// Lock before the serializable snapshot, including across UPSERT/DELETE.
	// Existing-first remains before mutable route lookup so a deleted route
	// still replays its original tombstone result after a lost HTTP response.
	tx, release, err := r.beginTokenSerializedTransaction(ctx, "TOKEN_ROUTE_ACCOUNT:"+command.AccountID.String())
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer release()
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	if _, err = getTokenAccount(ctx, tx, command.AccountID, true); err != nil {
		return TokenCommandResult{}, false, err
	}
	snapshot, err := routeCommandPrecheckTx(ctx, tx, envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	before, fingerprint, err := commandPrecheckFingerprint(envelope.CommandType, envelope.TargetResourceType, envelope.TargetResourceID, envelope.Command, snapshot)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if len(snapshot.Blockers) > 0 || fingerprint != envelope.PrecheckFingerprint || !tokenCommandExpectedVersionsMatch(envelope.ExpectedVersions, before) {
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	reference := uuid.Must(uuid.NewV7())
	if _, err = tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',$15,$16)`, reference, envelope.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, input.KeyDigest, input.IdempotencyFingerprint, input.RequestDigest, envelope.TargetResourceType, envelope.TargetResourceID, envelope.Reason, envelope.ExpectedVersions, envelope.PrecheckFingerprint, before, input.AuditRequestID); err != nil {
		return TokenCommandResult{}, false, err
	}
	accountVersion, routeVersion := *snapshot.LocalEvidence.CurrentVersion.AccountVersion, *snapshot.LocalEvidence.CurrentVersion.RouteVersion
	var redacted json.RawMessage
	if envelope.CommandType == "TOKEN_ROUTE_UPSERT" {
		result, effectErr := r.upsertStableRouteTx(ctx, tx, command.AccountID, StableRouteInput{ModelID: command.ModelID, ProtocolVersionID: command.ProtocolVersionID, Priority: command.Priority, DefaultScope: command.DefaultScope, ExpectedAccountVersion: accountVersion, ExpectedRouteVersion: routeVersion, Reason: envelope.Reason}, input.ActorUserID, false)
		if effectErr != nil {
			return TokenCommandResult{}, false, effectErr
		}
		redacted, err = json.Marshal(result)
	} else {
		if err = r.deleteStableRouteTx(ctx, tx, command.AccountID, StableRouteDeleteInput{RouteID: command.RouteID, ExpectedAccountVersion: accountVersion, ExpectedRouteVersion: routeVersion, Reason: envelope.Reason}, input.ActorUserID, false); err != nil {
			return TokenCommandResult{}, false, err
		}
		result := stableRouteDeleteResult{Deleted: true, RouteVersion: routeVersion + 1, AccountVersion: accountVersion + 1}
		if err = tx.QueryRow(ctx, `SELECT route_id,token_account_id,model_id,protocol_version_id,route_version,route_generation,deleted_at FROM routing_policy_tombstones WHERE route_id=$1`, command.RouteID).Scan(&result.RouteID, &result.AccountID, &result.ModelID, &result.ProtocolVersionID, &result.DeletedRouteVersion, &result.RouteGeneration, &result.DeletedAt); err != nil {
			return TokenCommandResult{}, false, err
		}
		redacted, err = json.Marshal(result)
	}
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, envelope, reference, input.IdempotencyFingerprint, before, redacted, redacted)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	// Preserve the reference when COMMIT's acknowledgement is lost.
	return item, true, tx.Commit(ctx)
}

func (s *Service) executeRouteCommandHTTP(w http.ResponseWriter, r *http.Request, repository *PostgresRepository, input runtimeTokenCommandBegin) {
	item, created, err := repository.executeGovernedRouteCommand(r.Context(), input)
	// Different accounts have independent domain locks, but the common key
	// scope is actor/type/key. Resolve only a real uniqueness/serialization
	// race after the failed transaction has rolled back and released its
	// snapshot; arbitrary SQL failures must not masquerade as key conflicts.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "40001") {
		existing, lookupErr := repository.TokenCommandByActorKey(r.Context(), input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint)
		if lookupErr == nil {
			item, created, err = existing, false, nil
		} else if errors.Is(lookupErr, ErrTokenCommandResultMismatch) {
			err = lookupErr
		}
	}
	if err != nil && item.CommandReference != uuid.Nil {
		api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": item.CommandReference, "result_query": "/token/admin/v1/commands/" + item.CommandReference.String()}})
		return
	}
	switch {
	case errors.Is(err, ErrTokenCommandResultMismatch):
		api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different command", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrTokenCommandInvalid):
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "the governed route command is invalid", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrTokenRouteActivationChanged):
		writeTokenCommandGovernanceInactiveHTTP(w, r)
	case errors.Is(err, ErrCatalogVersionConflict), errors.Is(err, ErrRouteVersionConflict), errors.Is(err, ErrRouteReferenceInvalid), errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		api.WriteError(w, http.StatusConflict, "COMMAND_PRECHECK_CHANGED", "the route or its dependencies changed", httpserver.RequestID(r.Context()))
	case err != nil:
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RETRYABLE", "no command was persisted; retry with the same idempotency key", httpserver.RequestID(r.Context()))
	default:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, status, item)
	}
}
