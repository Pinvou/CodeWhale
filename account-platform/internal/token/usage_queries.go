package token

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Usage query foundations are private repository projections, not F04 public
// management routes. Consumers must enforce their own authenticated ownership
// and permission boundary before creating snapshots or reading their members.
// Keep these signatures stable for the subsequent personal-usage feature.
type usageQueryRepository interface {
	adminUsageByIDs(context.Context, []string) ([]AdminUsageSummary, error)
	adminReconciliationByIDs(context.Context, []string) ([]AdminReconciliationCase, error)
	adminEvidenceByIDs(context.Context, []string) ([]AdminUsageEvidence, error)
}

var _ usageQueryRepository = (*PostgresRepository)(nil)

const (
	adminUsageSort                        = "updated_at DESC,id ASC"
	adminReconciliationSort               = "updated_at DESC,id DESC"
	adminUsageEvidenceSort                = "observed_at ASC,source_kind ASC,source_id ASC"
	adminUsageReconciliationCasePredicate = "review.domain='TOKEN' AND review.case_type IN ('USAGE_RECONCILIATION_REVIEW') AND review.resource_type='USAGE_REQUEST' AND EXISTS(SELECT 1 FROM usage_requests request WHERE request.id=review.resource_id)"
)

type AdminReconciliationCase struct {
	ID                 uuid.UUID  `json:"case_id"`
	CaseType           string     `json:"case_type"`
	ResourceType       string     `json:"resource_type"`
	ResourceID         uuid.UUID  `json:"resource_id"`
	Severity           string     `json:"severity"`
	Status             string     `json:"status"`
	OwnerID            *uuid.UUID `json:"owner_id,omitempty"`
	ClaimDueAt         time.Time  `json:"claim_due_at"`
	FirstResponseDueAt time.Time  `json:"first_response_due_at"`
	MitigationDueAt    time.Time  `json:"mitigation_due_at"`
	ResolutionDueAt    time.Time  `json:"resolution_due_at"`
	EscalationLevel    int        `json:"escalation_level"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type AdminUsageEvidence struct {
	SourceKind string          `json:"source_kind"`
	SourceID   uuid.UUID       `json:"source_id"`
	Type       string          `json:"evidence_type"`
	Payload    json.RawMessage `json:"evidence"`
	ObservedAt time.Time       `json:"observed_at"`
}

type AdminUsageSummary struct {
	ID                     uuid.UUID  `json:"usage_request_id"`
	OrganizationID         *uuid.UUID `json:"organization_id,omitempty"`
	TokenAccountID         uuid.UUID  `json:"token_account_id"`
	Model                  string     `json:"model"`
	ModelID                *uuid.UUID `json:"model_id,omitempty"`
	CatalogVersion         *int64     `json:"catalog_version,omitempty"`
	ExecutionStatus        string     `json:"execution_status"`
	DeliveryStatus         string     `json:"delivery_status"`
	CancelStatus           string     `json:"cancel_status"`
	SettlementStatus       string     `json:"settlement_status"`
	MaximumMicrocredits    int64      `json:"maximum_microcredits"`
	ActualMicrocredits     *int64     `json:"actual_microcredits,omitempty"`
	ErrorClass             *string    `json:"error_class,omitempty"`
	ReconciliationAttempts int        `json:"reconciliation_attempts"`
	ReconciliationStopped  *time.Time `json:"reconciliation_stopped_at,omitempty"`
	StartedAt              time.Time  `json:"started_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

func usageSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 255)
	if err != nil {
		return nil, err
	}
	if q != "" {
		_, uuidErr := uuid.Parse(q)
		decoded, digestErr := hex.DecodeString(q)
		if uuidErr != nil && (digestErr != nil || len(decoded) != 32) {
			return nil, ErrAdminQueryInvalidFilter
		}
	}
	execution, err := filter.enum("execution_status", "RECEIVED", "HOLD_RESERVING", "HOLD_RESERVED", "DISPATCH_INTENT_PERSISTED", "UPSTREAM_DISPATCHING", "STREAMING", "SETTLEMENT_PENDING", "SUCCEEDED", "FAILED", "CANCELLED_BEFORE_DISPATCH", "REVIEW")
	if err != nil {
		return nil, err
	}
	settlement, err := filter.enum("settlement_status", "HOLD_PENDING", "HOLD_ACTIVE", "USAGE_PENDING", "SETTLED", "RELEASED", "REVIEW")
	if err != nil {
		return nil, err
	}
	model, err := filter.string("model", 255)
	if err != nil {
		return nil, err
	}
	accountID, err := filter.uuid("token_account_id")
	if err != nil {
		return nil, err
	}
	organizationID, err := filter.uuid("organization_id")
	if err != nil {
		return nil, err
	}
	from, err := filter.instant("started_from")
	if err != nil {
		return nil, err
	}
	to, err := filter.instant("started_to")
	if err != nil {
		return nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, ErrAdminQueryInvalidFilter
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT request.id::text AS resource_id,row_number() OVER (ORDER BY request.updated_at DESC,request.id ASC)-1 AS position FROM usage_requests request
WHERE request.updated_at<=$3 AND ($4='' OR request.id::text=$4 OR request.idempotency_key_digest=$4)
AND ($5='' OR request.execution_status=$5) AND ($6='' OR request.settlement_status=$6) AND ($7='' OR request.model=$7)
AND ($8::uuid IS NULL OR request.token_account_id=$8) AND ($9::uuid IS NULL OR request.organization_id=$9)
AND ($10::timestamptz IS NULL OR request.started_at >= $10) AND ($11::timestamptz IS NULL OR request.started_at < $11)
`, highWater, q, execution, settlement, model, accountID, organizationID, from, to)
	}, nil
}

func reconciliationSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "OPEN", "RESOLVED", "LONG_TERM_PLAN")
	if err != nil {
		return nil, err
	}
	severity, err := filter.enum("severity", "P0", "P1", "P2")
	if err != nil {
		return nil, err
	}
	caseType, err := filter.enum("case_type", "USAGE_RECONCILIATION_REVIEW")
	if err != nil {
		return nil, err
	}
	ownerID, err := filter.uuid("owner_id")
	if err != nil {
		return nil, err
	}
	overdue, err := filter.boolean("sla_overdue")
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT review.id::text AS resource_id,row_number() OVER (ORDER BY review.updated_at DESC,review.id DESC)-1 AS position FROM manual_review_cases review
WHERE `+adminUsageReconciliationCasePredicate+` AND review.updated_at<=$3
AND ($4='' OR review.id::text=$4 OR review.resource_id::text=$4) AND ($5='' OR review.status=$5)
AND ($6='' OR review.severity=$6) AND ($7='' OR review.case_type=$7) AND ($8::uuid IS NULL OR review.owner_id=$8)
AND ($9::boolean IS NULL OR (LEAST(review.claim_due_at,review.first_response_due_at,review.mitigation_due_at,review.resolution_due_at)<$3 AND review.status='OPEN')=$9)
`, highWater, q, status, severity, caseType, ownerID, overdue)
	}, nil
}

func evidenceSnapshotSource(filter adminQueryFilter, usageID uuid.UUID) (AdminSnapshotMemberSource, error) {
	evidenceType, err := filter.upperString("evidence_type", 120)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT floor(extract(epoch FROM observed_at)*1000000)::bigint::text||':'||source_kind||':'||source_id::text AS resource_id,row_number() OVER (ORDER BY evidence.observed_at ASC,evidence.source_kind ASC,evidence.source_id ASC)-1 AS position FROM (
SELECT 'CANCELLATION'::text source_kind,id source_id,evidence_type,occurred_at observed_at FROM usage_cancellation_evidence WHERE usage_request_id=$3
UNION ALL SELECT 'CONVERGENCE',id,evidence_type,observed_at FROM usage_convergence_evidence WHERE usage_request_id=$3
) evidence WHERE evidence.observed_at<=$4 AND ($5='' OR evidence.evidence_type=$5)
`, usageID, highWater, evidenceType)
	}, nil
}

func (r *PostgresRepository) adminUsageByIDs(ctx context.Context, resourceIDs []string) ([]AdminUsageSummary, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminUsageSummary{}, err
	}
	// This private collection projection excludes provider identifiers,
	// credentials, request fingerprints, sessions, upstream references and raw
	// evidence. Any subsequent feature exposing it must enforce authorization;
	// F04 does not register a public usage collection/detail/evidence route.
	rows, err := r.pool.Query(ctx, `SELECT request.id,request.organization_id,request.token_account_id,
COALESCE(request.model_snapshot->>'display_name',request.model),request.model_id,request.catalog_version,
request.execution_status,request.delivery_status,request.cancel_status,request.settlement_status,
request.maximum_microcredits,request.actual_microcredits,request.error_class,request.reconciliation_attempts,
request.reconciliation_stopped_at,request.started_at,request.updated_at
FROM usage_requests request WHERE request.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]AdminUsageSummary{}
	for rows.Next() {
		var item AdminUsageSummary
		if err = rows.Scan(&item.ID, &item.OrganizationID, &item.TokenAccountID, &item.Model, &item.ModelID,
			&item.CatalogVersion, &item.ExecutionStatus, &item.DeliveryStatus, &item.CancelStatus,
			&item.SettlementStatus, &item.MaximumMicrocredits, &item.ActualMicrocredits, &item.ErrorClass,
			&item.ReconciliationAttempts, &item.ReconciliationStopped, &item.StartedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminReconciliationByIDs(ctx context.Context, resourceIDs []string) ([]AdminReconciliationCase, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminReconciliationCase{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT review.id,review.case_type,review.resource_type,review.resource_id,review.severity,review.status,review.owner_id,review.claim_due_at,review.first_response_due_at,review.mitigation_due_at,review.resolution_due_at,review.escalation_level,review.updated_at FROM manual_review_cases review WHERE `+adminUsageReconciliationCasePredicate+` AND review.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]AdminReconciliationCase{}
	for rows.Next() {
		var item AdminReconciliationCase
		if err = rows.Scan(&item.ID, &item.CaseType, &item.ResourceType, &item.ResourceID, &item.Severity, &item.Status, &item.OwnerID, &item.ClaimDueAt, &item.FirstResponseDueAt, &item.MitigationDueAt, &item.ResolutionDueAt, &item.EscalationLevel, &item.UpdatedAt); err != nil {
			return nil, err
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminEvidenceByIDs(ctx context.Context, resourceIDs []string) ([]AdminUsageEvidence, error) {
	if len(resourceIDs) == 0 {
		return []AdminUsageEvidence{}, nil
	}
	cancellation, convergence := []uuid.UUID{}, []uuid.UUID{}
	for _, key := range resourceIDs {
		parts := strings.SplitN(key, ":", 3)
		if len(parts) != 3 {
			return nil, ErrAdminPaginationCursorInvalid
		}
		if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
			return nil, ErrAdminPaginationCursorInvalid
		}
		id, err := uuid.Parse(parts[2])
		if err != nil {
			return nil, err
		}
		if parts[1] == "CANCELLATION" {
			cancellation = append(cancellation, id)
		} else if parts[1] == "CONVERGENCE" {
			convergence = append(convergence, id)
		} else {
			return nil, ErrAdminPaginationCursorInvalid
		}
	}
	rows, err := r.pool.Query(ctx, `SELECT source_kind,source_id,evidence_type,evidence,observed_at FROM (SELECT 'CANCELLATION'::text source_kind,id source_id,evidence_type,sanitized_evidence evidence,occurred_at observed_at FROM usage_cancellation_evidence WHERE id=ANY($1::uuid[]) UNION ALL SELECT 'CONVERGENCE',id,evidence_type,sanitized_snapshot,observed_at FROM usage_convergence_evidence WHERE id=ANY($2::uuid[])) source`, cancellation, convergence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]AdminUsageEvidence{}
	for rows.Next() {
		var item AdminUsageEvidence
		if err = rows.Scan(&item.SourceKind, &item.SourceID, &item.Type, &item.Payload, &item.ObservedAt); err != nil {
			return nil, err
		}
		positionMicros := item.ObservedAt.UnixMicro()
		values[strconv.FormatInt(positionMicros, 10)+":"+item.SourceKind+":"+item.SourceID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}
