package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func normalizedAuthorityStatus(status string) string {
	switch status {
	case "ACTIVE":
		return "ACTIVE"
	case "DELETED":
		return "NOT_FOUND"
	default:
		return "INACTIVE"
	}
}

func (s *Service) internalTokenOwnerAuthority(w http.ResponseWriter, r *http.Request) {
	var body tokenauthority.Request
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "AUTHORITY_REQUEST_INVALID", "authority request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err := body.Validate(); err != nil || body.OwnerType != nil && *body.OwnerType != "USER" {
		api.WriteError(w, http.StatusBadRequest, "AUTHORITY_REQUEST_INVALID", "authority request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_AUTHORITY_UNAVAILABLE", "owner authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	response, err := repository.tokenOwnerAuthority(r.Context(), body)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "OWNER_AUTHORITY_UNAVAILABLE", "owner authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, response)
}

func (r *PostgresRepository) tokenOwnerAuthority(ctx context.Context, input tokenauthority.Request) (tokenauthority.Response, error) {
	if err := input.Validate(); err != nil {
		return tokenauthority.Response{}, err
	}
	if input.OwnerType != nil && *input.OwnerType != "USER" {
		return tokenauthority.Response{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return tokenauthority.Response{}, err
	}
	defer tx.Rollback(ctx)

	var actorID uuid.UUID
	var actorRawStatus string
	var actorVersion, platformVersion int64
	var permissionCount int
	if input.Mode == tokenauthority.ModeT1Snapshot || input.Mode == tokenauthority.ModeOperatorAuthorize {
		err = tx.QueryRow(ctx, authorityIdentityActorSQL, *input.IdentityIssuer, *input.IdentitySubject, tokenauthority.RequiredPermissions).
			Scan(&actorID, &actorRawStatus, &actorVersion, &platformVersion, &permissionCount)
	} else {
		actorID, err = uuid.Parse(*input.ActorUserID)
		if err == nil {
			err = tx.QueryRow(ctx, authorityActorSQL, actorID, tokenauthority.RequiredPermissions).
				Scan(&actorRawStatus, &actorVersion, &platformVersion, &permissionCount)
		}
	}
	// A physical miss has no comparable lifecycle revision and therefore is
	// retryable/unavailable, never an authoritative NOT_FOUND decision.
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenauthority.Response{}, err
	}
	if err != nil {
		return tokenauthority.Response{}, err
	}

	actorStatus := normalizedAuthorityStatus(actorRawStatus)
	response := tokenauthority.Response{
		AuthoritySchemaVersion: input.AuthoritySchemaVersion,
		Mode:                   input.Mode, Decision: "ALLOW", ActorUserID: actorID.String(),
		ExpectedPlatformAuthorizationVersion: input.ExpectedPlatformAuthorizationVersion,
		ObservedPlatformAuthorizationVersion: strconv.FormatInt(platformVersion, 10),
		ExpectedActorVersion:                 input.ExpectedActorVersion,
		ExpectedActorIdentityDigest:          input.ExpectedActorIdentityDigest,
		ObservedActorVersion:                 strconv.FormatInt(actorVersion, 10),
		ActorStatus:                          actorStatus,
		ActorIdentityDigest:                  tokenauthority.ActorIdentityDigest(actorID, actorStatus, actorVersion),
		ExpectedOwnerVersion:                 input.ExpectedOwnerVersion,
		ExpectedOwnerIdentityDigest:          input.ExpectedOwnerIdentityDigest,
		CommandReference:                     input.CommandReference, CommandRequestDigest: input.CommandRequestDigest,
		AuthorityRequestSHA256: input.AuthorityRequestSHA256, ObservedAt: r.now().UTC(),
	}

	ownerStatus := ""
	if input.Mode != tokenauthority.ModeOperatorAuthorize {
		ownerID, parseErr := uuid.Parse(*input.OwnerID)
		if parseErr != nil {
			return tokenauthority.Response{}, parseErr
		}
		var ownerVersion int64
		switch *input.OwnerType {
		case "USER":
			err = tx.QueryRow(ctx, `SELECT status,version FROM users WHERE id=$1`, ownerID).Scan(&ownerStatus, &ownerVersion)
		default:
			return tokenauthority.Response{}, errors.New("invalid owner type")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return tokenauthority.Response{}, err
		}
		if err != nil {
			return tokenauthority.Response{}, err
		}
		ownerStatus = normalizedAuthorityStatus(ownerStatus)
		ownerDigest := tokenauthority.OwnerIdentityDigest(*input.OwnerType, ownerID, ownerStatus, ownerVersion)

		response.ObservedOwnerVersion = authorityString(strconv.FormatInt(ownerVersion, 10))
		response.OwnerStatus = authorityString(ownerStatus)
		response.OwnerIdentityDigest = authorityString(ownerDigest)
	}

	// Keep a single stable reason in the reviewed priority order.
	reason := ""
	switch {
	case actorStatus == "NOT_FOUND":
		reason = "ACTOR_NOT_FOUND"
	case actorStatus != "ACTIVE":
		reason = "ACTOR_INACTIVE"
	case permissionCount != len(tokenauthority.RequiredPermissions):
		reason = "AUTHORIZATION_REVOKED"
	case input.Mode != tokenauthority.ModeOperatorAuthorize && ownerStatus == "NOT_FOUND":
		reason = "OWNER_NOT_FOUND"
	case input.Mode != tokenauthority.ModeOperatorAuthorize && ownerStatus != "ACTIVE":
		reason = "OWNER_INACTIVE"
	}
	if reason != "" {
		response.Decision = "DENY"
		response.ReasonCode = authorityString(reason)
	}
	if err = tx.Commit(ctx); err != nil {
		return tokenauthority.Response{}, err
	}
	return response, nil
}

const authorityIdentityActorSQL = `
SELECT account_user.id,account_user.status,account_user.version,
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=account_user.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       (SELECT count(DISTINCT permission.permission_code)
          FROM role_assignments assignment
          JOIN roles role ON role.code=assignment.role_code AND role.scope='PLATFORM'
          JOIN role_permissions permission ON permission.role_code=role.code
         WHERE assignment.user_id=account_user.id AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL
           AND assignment.revoked_at IS NULL AND assignment.starts_at<=transaction_timestamp()
           AND (assignment.expires_at IS NULL OR assignment.expires_at>transaction_timestamp())
           AND permission.permission_code=ANY($3::text[]))
  FROM identities identity_record JOIN users account_user ON account_user.id=identity_record.user_id
 WHERE identity_record.issuer=$1 AND identity_record.subject=$2`

const authorityActorSQL = `
SELECT account_user.status,account_user.version,
       COALESCE((SELECT version FROM authorization_versions WHERE user_id=account_user.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
       (SELECT count(DISTINCT permission.permission_code)
          FROM role_assignments assignment
          JOIN roles role ON role.code=assignment.role_code AND role.scope='PLATFORM'
          JOIN role_permissions permission ON permission.role_code=role.code
         WHERE assignment.user_id=account_user.id AND assignment.organization_id IS NULL AND assignment.unit_id IS NULL
           AND assignment.revoked_at IS NULL AND assignment.starts_at<=transaction_timestamp()
           AND (assignment.expires_at IS NULL OR assignment.expires_at>transaction_timestamp())
           AND permission.permission_code=ANY($2::text[]))
  FROM users account_user WHERE account_user.id=$1`

func authorityString(value string) *string { return &value }
