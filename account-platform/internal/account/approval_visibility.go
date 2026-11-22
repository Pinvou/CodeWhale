package account

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrApprovalRequestNotFound      = errors.New("approval request was not found")
	ErrApprovalAuthorizationChanged = errors.New("approval authorization changed while reading")
)

type ApprovalDirectoryItem struct {
	CommandType           string `json:"command_type"`
	PolicyVersion         int64  `json:"policy_version"`
	SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
	RiskClass             string `json:"risk_class"`
	Visible               bool   `json:"visible"`
	Requestable           bool   `json:"requestable"`
}

type ApprovalDirectoryProjection struct {
	SchemaVersion                int                     `json:"schema_version"`
	PlatformAuthorizationVersion int64                   `json:"platform_authorization_version"`
	Items                        []ApprovalDirectoryItem `json:"items"`
}

type approvalVisibility struct {
	Owned                bool
	AuthorizationVersion int64
}

// ApprovalDirectory returns only entries whose exact presenter version is
// installed and for which the current actor still owns requester or approver
// permission. It never exposes permission names or derives an approvable bit.
func (r *PostgresRepository) ApprovalDirectory(ctx context.Context, principal identity.Principal) (ApprovalDirectoryProjection, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalDirectoryProjection{}, ErrForbidden
	}
	version, permissions, err := r.approvalDirectoryAuthorization(ctx, principal)
	if err != nil {
		return ApprovalDirectoryProjection{}, err
	}

	type candidate struct {
		policy     approvalpolicy.Policy
		canRequest bool
		canApprove bool
	}
	candidates := make([]candidate, 0)
	for _, policy := range approvalpolicy.All() {
		if policy.Mode != approvalpolicy.ApprovalRequired || r.approvalPresenterReady == nil || !r.approvalPresenterReady(policy) {
			continue
		}
		_, canRequest := permissions[policy.RequesterPermission]
		_, canApprove := permissions[policy.ApproverPermission]
		if !canRequest && !canApprove {
			continue
		}
		candidates = append(candidates, candidate{policy: policy, canRequest: canRequest, canApprove: canApprove})
	}

	canAdvance := r.approvalAdvancementProjection(ctx)
	recentAuthentication := recentlyAuthenticated(principal.AuthenticatedAt, r.now())
	items := make([]ApprovalDirectoryItem, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, ApprovalDirectoryItem{
			CommandType:           candidate.policy.CommandType,
			PolicyVersion:         candidate.policy.PolicyVersion,
			SnapshotSchemaVersion: candidate.policy.SnapshotSchemaVersion,
			RiskClass:             candidate.policy.RiskClass,
			Visible:               true,
			Requestable:           candidate.canRequest && recentAuthentication && canAdvance(candidate.policy),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CommandType == items[j].CommandType {
			return items[i].PolicyVersion < items[j].PolicyVersion
		}
		return items[i].CommandType < items[j].CommandType
	})

	// Domain activation can take several seconds. A role mutation during that
	// interval invalidates the whole projection; a caller must fetch again.
	currentVersion, _, err := r.approvalDirectoryAuthorization(ctx, principal)
	if err != nil {
		return ApprovalDirectoryProjection{}, err
	}
	if currentVersion != version {
		return ApprovalDirectoryProjection{}, ErrApprovalAuthorizationChanged
	}
	return ApprovalDirectoryProjection{SchemaVersion: 1, PlatformAuthorizationVersion: version, Items: items}, nil
}

func (r *PostgresRepository) approvalDirectoryAuthorization(ctx context.Context, principal identity.Principal) (int64, map[string]struct{}, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	var actorID uuid.UUID
	var version int64
	err = tx.QueryRow(ctx, `
SELECT u.id,COALESCE((SELECT av.version FROM authorization_versions av
 WHERE av.user_id=u.id AND av.scope='PLATFORM' AND av.scope_id='00000000-0000-0000-0000-000000000000'::uuid),1)
FROM identities i JOIN users u ON u.id=i.user_id AND u.status='ACTIVE'
	WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject).Scan(&actorID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrForbidden
	}
	if err != nil {
		return 0, nil, err
	}
	if version < 1 {
		return 0, nil, ErrApprovalAuthorizationChanged
	}
	rows, err := tx.Query(ctx, `
SELECT DISTINCT rp.permission_code
FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code
WHERE ra.user_id=$1 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
  AND ra.starts_at<=transaction_timestamp()
  AND (ra.expires_at IS NULL OR ra.expires_at>transaction_timestamp())
ORDER BY rp.permission_code`, actorID)
	if err != nil {
		return 0, nil, err
	}
	permissions := make(map[string]struct{})
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			rows.Close()
			return 0, nil, err
		}
		permissions[strings.TrimSpace(permission)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return version, permissions, nil
}

// approvalRequestVisibility is deliberately a narrow existence query. Missing
// and currently invisible requests take the same SQL and return the same public
// sentinel before normalized command, snapshots, presenters, proof or domain
// data are read.
func approvalRequestVisibilityForActor(ctx context.Context, database queryable, actorID, requestID uuid.UUID) (approvalVisibility, error) {
	var owned, visible bool
	var authorizationVersion int64
	err := database.QueryRow(ctx, `
SELECT ar.requester_user_id=$2,
       ar.requester_user_id=$2 OR EXISTS(
         SELECT 1 FROM role_assignments ra
         JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p.approver_permission
         WHERE ra.user_id=$2 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
           AND ra.starts_at<=transaction_timestamp()
           AND (ra.expires_at IS NULL OR ra.expires_at>transaction_timestamp())),
       COALESCE((SELECT av.version FROM authorization_versions av
         WHERE av.user_id=$2 AND av.scope='PLATFORM'
           AND av.scope_id='00000000-0000-0000-0000-000000000000'::uuid),1)
FROM approval_requests ar
JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version
WHERE ar.id=$1`, requestID, actorID).Scan(&owned, &visible, &authorizationVersion)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !visible {
		return approvalVisibility{}, ErrApprovalRequestNotFound
	}
	if err != nil {
		return approvalVisibility{}, err
	}
	if authorizationVersion < 1 {
		return approvalVisibility{}, ErrApprovalAuthorizationChanged
	}
	return approvalVisibility{Owned: owned, AuthorizationVersion: authorizationVersion}, nil
}

func platformAuthorizationVersionForActor(ctx context.Context, database queryable, actorID uuid.UUID) (int64, error) {
	var version int64
	if err := database.QueryRow(ctx, `SELECT COALESCE((SELECT av.version FROM authorization_versions av
 WHERE av.user_id=$1 AND av.scope='PLATFORM'
   AND av.scope_id='00000000-0000-0000-0000-000000000000'::uuid),1)`, actorID).Scan(&version); err != nil {
		return 0, err
	}
	if version < 1 {
		return 0, ErrApprovalAuthorizationChanged
	}
	return version, nil
}

func ensureApprovalAuthorizationVersion(ctx context.Context, database queryable, actorID uuid.UUID, expected int64) error {
	current, err := platformAuthorizationVersionForActor(ctx, database, actorID)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrApprovalAuthorizationChanged
	}
	return nil
}

func approvalVisibleRequestReadError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrApprovalRequestNotFound
	}
	return err
}
