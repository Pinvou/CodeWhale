package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OrganizationLifecycleBlocker struct {
	Code         string `json:"code"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type OrganizationLifecycleAssessment struct {
	OrganizationID uuid.UUID                      `json:"organization_id"`
	Status         string                         `json:"status"`
	AccountVersion int64                          `json:"account_version"`
	Ready          bool                           `json:"ready"`
	Blockers       []OrganizationLifecycleBlocker `json:"blockers"`
}

func (r *PostgresRepository) ApplyOrganizationLifecycle(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64, preflightOnly bool) (OrganizationLifecycleAssessment, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	if organizationID == uuid.Nil || accountVersion < 1 || (status != "ACTIVE" && status != "DISABLED" && status != "DELETED") {
		return OrganizationLifecycleAssessment{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-lifecycle:"+organizationID.String()); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	var currentStatus string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,account_version FROM organization_lifecycle_states WHERE organization_id=$1 FOR UPDATE`, organizationID).Scan(&currentStatus, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationLifecycleAssessment{}, err
	}
	if err == nil {
		if currentStatus == "DELETED" && status != "DELETED" {
			return OrganizationLifecycleAssessment{}, ErrConflict
		}
		if accountVersion < currentVersion || (accountVersion == currentVersion && status != currentStatus) {
			return OrganizationLifecycleAssessment{}, ErrConflict
		}
		if accountVersion == currentVersion && status == currentStatus {
			return OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}, tx.Commit(ctx)
		}
	}

	assessment := OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	if status == "ACTIVE" {
		var closedWallet bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wallets WHERE owner_type='ORGANIZATION' AND owner_id=$1 AND status='CLOSED')`, organizationID).Scan(&closedWallet); err != nil {
			return OrganizationLifecycleAssessment{}, err
		}
		if closedWallet {
			assessment.Ready = false
			assessment.Blockers = append(assessment.Blockers, OrganizationLifecycleBlocker{Code: "ORGANIZATION_WALLET_CLOSED", ResourceType: "wallet", ResourceID: organizationID.String()})
			return assessment, tx.Commit(ctx)
		}
	}
	if status == "DELETED" {
		if currentStatus != "DISABLED" {
			assessment.Ready = false
			assessment.Blockers = append(assessment.Blockers, OrganizationLifecycleBlocker{Code: "ORGANIZATION_NOT_DISABLED", ResourceType: "organization", ResourceID: organizationID.String()})
			return assessment, tx.Commit(ctx)
		}
		assessment.Blockers, err = organizationDeletionBlockers(ctx, tx, organizationID)
		if err != nil {
			return OrganizationLifecycleAssessment{}, err
		}
		assessment.Ready = len(assessment.Blockers) == 0
		if !assessment.Ready {
			return assessment, tx.Commit(ctx)
		}
	}
	if preflightOnly {
		return assessment, tx.Commit(ctx)
	}

	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO organization_lifecycle_states(organization_id,status,account_version,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO UPDATE SET status=EXCLUDED.status,account_version=EXCLUDED.account_version,updated_at=EXCLUDED.updated_at`, organizationID, status, accountVersion, now); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	walletStatus := "FROZEN"
	if status == "ACTIVE" {
		walletStatus = "ACTIVE"
	} else if status == "DELETED" {
		walletStatus = "CLOSED"
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET status=$2,version=version+1,updated_at=$3 WHERE owner_type='ORGANIZATION' AND owner_id=$1 AND status<>$2 AND NOT (status='CLOSED' AND $2<>'CLOSED')`, organizationID, walletStatus, now); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.organization.lifecycle-applied.v1", organizationID, accountVersion, map[string]any{"organization_id": organizationID, "status": status, "account_version": accountVersion}); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	return assessment, tx.Commit(ctx)
}

func organizationDeletionBlockers(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	queries := []struct {
		code, resourceType, query string
	}{
		{"WALLET_BALANCE_NOT_ZERO", "wallet", `SELECT w.id::text FROM wallets w WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND w.posted_microcredits<>0`},
		{"WALLET_RESERVED_BALANCE", "wallet", `SELECT w.id::text FROM wallets w WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND w.reserved_microcredits<>0`},
		{"WALLET_RECEIVABLE_OPEN", "receivable", `SELECT r.id::text FROM receivables r JOIN wallets w ON w.id=r.wallet_id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND r.outstanding_microcredits>0`},
		{"EFFECTIVE_ENTITLEMENT_REMAINS", "entitlement_package", `SELECT DISTINCT p.id::text FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND (c.available_microcredits+c.reserved_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits)>0`},
		{"UNSETTLED_USAGE_HOLD", "usage_hold", `SELECT h.id::text FROM wallets w JOIN usage_holds h ON h.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND h.status IN ('ACTIVE','REVIEW')`},
		{"UNFINISHED_PAYMENT", "payment_order", `SELECT o.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND (o.status IN ('CREATED','PENDING','PAID','PAID_LATE','MANUAL_REVIEW') OR o.fulfillment_status IN ('PENDING','MANUAL_REVIEW'))`},
		{"UNFINISHED_REFUND", "refund", `SELECT f.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id JOIN refunds f ON f.payment_order_id=o.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND f.status IN ('REQUESTED','MANUAL_REVIEW')`},
		{"UNRESOLVED_CHARGEBACK", "chargeback_case", `SELECT c.id::text FROM wallets w JOIN chargeback_cases c ON c.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND c.status<>'RESOLVED'`},
		{"UNRESOLVED_FORCED_REFUND", "forced_refund_case", `SELECT c.id::text FROM wallets w JOIN forced_refund_cases c ON c.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND c.status='REVIEW_REQUIRED'`},
		{"OPEN_RECONCILIATION_DIFFERENCE", "reconciliation_case", `SELECT c.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id JOIN reconciliation_cases c ON c.payment_order_id=o.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND c.status='OPEN'`},
		{"PENDING_USAGE_REVIEW", "usage_loss_review", `SELECT review.id::text FROM wallets w JOIN usage_loss_reviews review ON review.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND review.status='REVIEW_REQUIRED'`},
		{"PENDING_FINANCIAL_ADJUSTMENT", "manual_adjustment", `SELECT a.id::text FROM wallets w JOIN manual_adjustments a ON a.wallet_id=w.id WHERE w.owner_type='ORGANIZATION' AND w.owner_id=$1 AND a.status IN ('PENDING_APPROVAL','APPROVED')`},
		{"REGISTERED_GOVERNANCE_BLOCKER", "governance", `SELECT blocker_code||':'||resource_type||':'||resource_id FROM organization_governance_blockers WHERE organization_id=$1 AND active`},
	}
	blockers := make([]OrganizationLifecycleBlocker, 0)
	for _, item := range queries {
		rows, err := tx.Query(ctx, item.query, organizationID)
		if err != nil {
			return nil, fmt.Errorf("query %s blockers: %w", item.code, err)
		}
		for rows.Next() {
			var resourceID string
			if err := rows.Scan(&resourceID); err != nil {
				rows.Close()
				return nil, err
			}
			blockers = append(blockers, OrganizationLifecycleBlocker{Code: item.code, ResourceType: item.resourceType, ResourceID: resourceID})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return blockers, nil
}

func (r *PostgresRepository) RegisterOrganizationGovernanceBlocker(ctx context.Context, organizationID uuid.UUID, code, resourceType, resourceID string, active bool, details []byte, now time.Time) error {
	if organizationID == uuid.Nil || strings.TrimSpace(code) == "" || strings.TrimSpace(resourceType) == "" || strings.TrimSpace(resourceID) == "" {
		return ErrConflict
	}
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO organization_governance_blockers(organization_id,blocker_code,resource_type,resource_id,active,details,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,blocker_code,resource_type,resource_id) DO UPDATE SET active=EXCLUDED.active,details=EXCLUDED.details,updated_at=EXCLUDED.updated_at`, organizationID, strings.TrimSpace(code), strings.TrimSpace(resourceType), strings.TrimSpace(resourceID), active, details, now.UTC())
	return err
}
