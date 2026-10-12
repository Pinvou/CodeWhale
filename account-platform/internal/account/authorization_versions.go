package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/jackc/pgx/v5"
)

// AuthorizationVersions is the live header projection, without profile or
// role aggregation. Identity and session binding match GetMe.
type AuthorizationVersions struct {
	ContextVersion                   int64
	AuthorizationVersion             int64
	PersonalAuthorizationVersion     int64
	PlatformAuthorizationVersion     int64
	OrganizationAuthorizationVersion int64
}

type authorizationVersionsRepository interface {
	GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error)
}

func readAuthorizationVersions(ctx context.Context, repository Repository, principal identity.Principal) (AuthorizationVersions, error) {
	if narrow, ok := repository.(authorizationVersionsRepository); ok {
		return narrow.GetAuthorizationVersions(ctx, principal)
	}
	// Preserve existing third-party repository implementations and fixtures.
	me, err := repository.GetMe(ctx, principal)
	return AuthorizationVersions{me.ActiveContext.ContextVersion, me.ActiveContext.AuthorizationVersion, me.ActiveContext.PersonalAuthorizationVersion, me.ActiveContext.PlatformAuthorizationVersion, me.ActiveContext.OrganizationAuthorizationVersion}, err
}

func (r *PostgresRepository) GetAuthorizationVersions(ctx context.Context, principal identity.Principal) (AuthorizationVersions, error) {
	var versions AuthorizationVersions
	err := r.pool.QueryRow(ctx, `SELECT context.context_version,
 COALESCE((SELECT av.version FROM authorization_versions av WHERE av.user_id=u.id AND av.scope=CASE WHEN context.organization_id IS NULL THEN 'PERSONAL' ELSE 'ORGANIZATION' END AND av.scope_id=COALESCE(context.organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),1),
 COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PERSONAL' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
 COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid),1),
 COALESCE((SELECT version FROM authorization_versions WHERE user_id=u.id AND scope='ORGANIZATION' AND scope_id=context.organization_id),1)
 FROM identities i JOIN users u ON u.id=i.user_id
 JOIN user_sessions s ON s.user_id=u.id AND s.issuer=i.issuer AND s.session_id=$3
 JOIN user_active_contexts context ON context.user_id=u.id
 WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, principal.SessionID).Scan(&versions.ContextVersion, &versions.AuthorizationVersion, &versions.PersonalAuthorizationVersion, &versions.PlatformAuthorizationVersion, &versions.OrganizationAuthorizationVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationVersions{}, ErrNotFound
	}
	return versions, err
}
