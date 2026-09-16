package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

type ApprovalV2Page struct {
	Items             []ApprovalV2Summary `json:"items"`
	NextCursor        *string             `json:"next_cursor"`
	HasMore           bool                `json:"has_more"`
	Limit             int                 `json:"limit"`
	AsOf              string              `json:"as_of"`
	SnapshotExpiresAt string              `json:"snapshot_expires_at"`
}

// ListApprovalRequestsV2 uses the bounded membership snapshot for ordering,
// then reads only presentation columns. A revoked/missing member invalidates
// the entire page rather than returning a partial or unverified response.
func (r *PostgresRepository) ListApprovalRequestsV2(ctx context.Context, principal identity.Principal, filter ApprovalFilter, cursor string, limit int, sourceIP string) (ApprovalV2Page, error) {
	budget := ApprovalSnapshotBuildBudget
	if strings.TrimSpace(cursor) != "" {
		budget = ApprovalSnapshotReadBudget
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	actorID, err := r.userIDByPrincipal(bounded, principal)
	if err != nil {
		return ApprovalV2Page{}, err
	}
	version, err := platformAuthorizationVersionForActor(bounded, r.pool, actorID)
	if err != nil {
		return ApprovalV2Page{}, err
	}
	page, err := r.ApprovalPaginationPage(bounded, principal, filter, cursor, limit, sourceIP)
	if err != nil {
		return ApprovalV2Page{}, approvalV2ListError(err, bounded, cursor)
	}
	items := make([]ApprovalV2Summary, 0, len(page.ApprovalRequestIDs))
	if len(page.ApprovalRequestIDs) != 0 {
		rows, err := r.pool.Query(bounded, `
SELECT ar.id,ar.command_type,ar.policy_version,s.snapshot_schema_version,p.risk_class,
       ar.target_resource_type,ar.target_resource_id,ar.requester_user_id,ar.status,
       ar.submitted_at,ar.expires_at,ar.updated_at,ar.supersedes_request_id
FROM unnest($1::uuid[]) WITH ORDINALITY AS ids(id,ordinal)
JOIN approval_requests ar ON ar.id=ids.id
JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version
JOIN approval_request_snapshots s ON s.approval_request_id=ar.id
WHERE ar.requester_user_id=$2 OR EXISTS(
  SELECT 1 FROM role_assignments ra JOIN role_permissions rp
    ON rp.role_code=ra.role_code AND rp.permission_code=p.approver_permission
  WHERE ra.user_id=$2 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
    AND ra.starts_at<=transaction_timestamp() AND (ra.expires_at IS NULL OR ra.expires_at>transaction_timestamp()))
ORDER BY ids.ordinal`, page.ApprovalRequestIDs, actorID)
		if err != nil {
			return ApprovalV2Page{}, approvalV2ListError(err, bounded, cursor)
		}
		for rows.Next() {
			var item ApprovalRequest
			err = rows.Scan(&item.ID, &item.CommandType, &item.PolicyVersion, &item.SnapshotSchemaVersion, &item.RiskClass,
				&item.TargetResourceType, &item.TargetResourceID, &item.RequesterUserID, &item.Status,
				&item.SubmittedAt, &item.ExpiresAt, &item.UpdatedAt, &item.SupersedesRequestID)
			if err != nil {
				break
			}
			items = append(items, approvalV2Summary(item))
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return ApprovalV2Page{}, approvalV2ListError(err, bounded, cursor)
		}
		if len(items) != len(page.ApprovalRequestIDs) {
			return ApprovalV2Page{}, ErrApprovalPaginationCursorExpired
		}
		for i, id := range page.ApprovalRequestIDs {
			if items[i].ApprovalRequestID != id {
				return ApprovalV2Page{}, ErrApprovalPaginationCursorExpired
			}
		}
	}
	if err := ensureApprovalAuthorizationVersion(bounded, r.pool, actorID, version); err != nil {
		if errors.Is(err, ErrApprovalAuthorizationChanged) {
			return ApprovalV2Page{}, ErrApprovalPaginationCursorExpired
		}
		return ApprovalV2Page{}, approvalV2ListError(err, bounded, cursor)
	}
	return ApprovalV2Page{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore,
		Limit: page.Limit, AsOf: page.AsOf.UTC().Format(time.RFC3339Nano),
		SnapshotExpiresAt: page.SnapshotExpiresAt.UTC().Format(time.RFC3339Nano)}, nil
}

func approvalV2ListError(err error, ctx context.Context, cursor string) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if strings.TrimSpace(cursor) != "" {
			return ErrApprovalPaginationQueryTimeout
		}
		return ErrApprovalPaginationSnapshotUnavailable
	}
	return err
}
