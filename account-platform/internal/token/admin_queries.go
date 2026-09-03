package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrAdminQueryInvalidFilter = errors.New("invalid admin query filter")
var ErrAdminQueryRateLimited = errors.New("admin query rate limited")
var ErrAdminSnapshotResourceChanged = errors.New("admin snapshot resource changed")

type adminQueryRateLimitError struct{ RetryAfter int }

func (e adminQueryRateLimitError) Error() string { return ErrAdminQueryRateLimited.Error() }
func (e adminQueryRateLimitError) Unwrap() error { return ErrAdminQueryRateLimited }

const (
	adminModelsSort          = "normalized_name ASC,model_id ASC"
	adminProtocolsSort       = "protocol_code ASC,version DESC,id ASC"
	adminFormulasSort        = "protocol_code ASC,version DESC,id ASC"
	adminTokenAccountsSort   = "updated_at DESC,id ASC"
	adminProvisioningsSort   = "updated_at DESC,id DESC"
	adminUsageSort           = "updated_at DESC,id ASC"
	adminReconciliationSort  = "updated_at DESC,id DESC"
	adminResourceAuditSort   = "occurred_at DESC,id DESC"
	adminOwnerCandidatesSort = "normalized_display_label ASC,owner_type ASC,owner_id ASC"
	adminUsageEvidenceSort   = "observed_at ASC,source_kind ASC,source_id ASC"
)

type AdminQueryEnvelope struct {
	Items             any       `json:"items"`
	NextCursor        *string   `json:"next_cursor"`
	HasMore           bool      `json:"has_more"`
	Limit             int       `json:"limit"`
	AsOf              time.Time `json:"as_of"`
	SnapshotExpiresAt time.Time `json:"snapshot_expires_at"`
	RequestID         string    `json:"request_id"`
}

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

type AdminResourceAudit struct {
	ID           uuid.UUID       `json:"audit_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   uuid.UUID       `json:"resource_id"`
	ActorID      uuid.UUID       `json:"actor_id"`
	Summary      json.RawMessage `json:"summary"`
	OccurredAt   time.Time       `json:"occurred_at"`
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

type adminQueryFilter struct {
	values  map[string]string
	present map[string]struct{}
}

func (r *PostgresRepository) reserveAdminQueryRate(ctx context.Context, subjectID uuid.UUID, resource, source string, owner bool) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-query-rate'))`); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	window := now.Truncate(time.Minute)
	if _, err = tx.Exec(ctx, `DELETE FROM token_admin_pagination_creation_windows WHERE window_start<$1`, window.Add(-2*time.Minute)); err != nil {
		return err
	}
	sourceDigest := sha256.Sum256([]byte(strings.TrimSpace(source)))
	subjectLimit, sourceLimit := 60, 120
	if owner {
		subjectLimit, sourceLimit = 30, 60
	}
	limited := false
	for _, quota := range []struct {
		key   string
		limit int
	}{
		{key: "query:subject:" + subjectID.String() + ":" + resource, limit: subjectLimit},
		{key: "query:source:" + hex.EncodeToString(sourceDigest[:]) + ":" + resource, limit: sourceLimit},
	} {
		var count int
		if err = tx.QueryRow(ctx, `INSERT INTO token_admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES($1,$2,1)
ON CONFLICT(rate_key,window_start) DO UPDATE SET creation_count=token_admin_pagination_creation_windows.creation_count+1 RETURNING creation_count`, quota.key, window).Scan(&count); err != nil {
			return err
		}
		limited = limited || count > quota.limit
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if limited {
		retry := int(window.Add(time.Minute).Sub(now).Seconds())
		if retry < 1 || retry > 60 {
			retry = 1
		}
		return adminQueryRateLimitError{RetryAfter: retry}
	}
	return nil
}

func parseAdminQuery(values url.Values, allowed []string) (adminQueryFilter, error) {
	allowed = append(append([]string{}, allowed...), "cursor", "limit")
	result := adminQueryFilter{values: map[string]string{}, present: map[string]struct{}{}}
	for key, all := range values {
		if !slices.Contains(allowed, key) || len(all) != 1 {
			return adminQueryFilter{}, ErrAdminQueryInvalidFilter
		}
		result.present[key] = struct{}{}
		value := strings.TrimSpace(all[0])
		if value != "" {
			result.values[key] = value
		}
	}
	return result, nil
}

func (f adminQueryFilter) string(key string, maximum int) (string, error) {
	value := norm.NFKC.String(strings.TrimSpace(f.values[key]))
	if maximum > 0 && len(value) > maximum {
		return "", ErrAdminQueryInvalidFilter
	}
	if value != "" {
		f.values[key] = value
	}
	return value, nil
}

func (f adminQueryFilter) search(key string, maximum int) (string, error) {
	value, err := f.string(key, maximum)
	if err != nil {
		return "", err
	}
	value = cases.Fold().String(value)
	if value != "" {
		f.values[key] = value
	}
	return value, nil
}

func (f adminQueryFilter) upperString(key string, maximum int) (string, error) {
	value, err := f.string(key, maximum)
	if err != nil {
		return "", err
	}
	value = strings.ToUpper(value)
	if value != "" {
		f.values[key] = value
	}
	return value, nil
}

func (f adminQueryFilter) enum(key string, values ...string) (string, error) {
	value := strings.ToUpper(strings.TrimSpace(f.values[key]))
	if value != "" && !slices.Contains(values, value) {
		return "", ErrAdminQueryInvalidFilter
	}
	if value != "" {
		f.values[key] = value
	}
	return value, nil
}

func (f adminQueryFilter) boolean(key string) (*bool, error) {
	value := strings.TrimSpace(f.values[key])
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, ErrAdminQueryInvalidFilter
	}
	f.values[key] = strconv.FormatBool(parsed)
	return &parsed, nil
}

func (f adminQueryFilter) uuid(key string) (*uuid.UUID, error) {
	value := strings.TrimSpace(f.values[key])
	if value == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, ErrAdminQueryInvalidFilter
	}
	f.values[key] = parsed.String()
	return &parsed, nil
}

func (f adminQueryFilter) instant(key string) (*time.Time, error) {
	value := strings.TrimSpace(f.values[key])
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, ErrAdminQueryInvalidFilter
	}
	parsed = parsed.UTC()
	f.values[key] = parsed.Format(time.RFC3339Nano)
	return &parsed, nil
}

func (f adminQueryFilter) cursor() string { return f.values["cursor"] }

func (f adminQueryFilter) limit(defaultLimit, maximum int) (int, error) {
	if f.values["limit"] == "" {
		return NormalizeAdminPaginationLimit(0, defaultLimit, maximum)
	}
	value, err := strconv.Atoi(f.values["limit"])
	if err != nil {
		return 0, ErrAdminPaginationLimitInvalid
	}
	return NormalizeAdminPaginationLimit(value, defaultLimit, maximum)
}

func (f adminQueryFilter) paginationValues() map[string]string {
	result := make(map[string]string, len(f.values))
	for key, value := range f.values {
		if key != "cursor" && key != "limit" {
			result[key] = value
		}
	}
	return result
}

func adminSnapshotInsert(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, maximum int, selectSQL string, args ...any) error {
	arguments := []any{snapshotID, maximum}
	arguments = append(arguments, args...)
	_, err := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_members(snapshot_id,position,resource_id)
SELECT $1,source.position,source.resource_id FROM (`+selectSQL+`) source WHERE source.position<$2`, arguments...)
	return err
}

func modelSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	lifecycle, err := filter.enum("lifecycle_status", "PUBLISHED", "DEPRECATED", "REMOVED")
	if err != nil {
		return nil, err
	}
	protocol, err := filter.upperString("protocol_code", 80)
	if err != nil {
		return nil, err
	}
	adapter, err := filter.string("source_adapter", 120)
	if err != nil {
		return nil, err
	}
	missing, err := filter.boolean("source_missing")
	if err != nil {
		return nil, err
	}
	missingValue, hasMissing := false, missing != nil
	if missing != nil {
		missingValue = *missing
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `
SELECT entry.model_id::text AS resource_id,row_number() OVER (ORDER BY entry.normalized_name ASC,entry.model_id ASC)-1 AS position
FROM model_catalog_entries entry
JOIN model_identities identity_record ON identity_record.id=entry.model_id
JOIN protocol_definitions protocol ON protocol.id=entry.protocol_definition_id
WHERE entry.catalog_version=(SELECT COALESCE(max(version),0) FROM model_catalog_versions)
  AND ($3='' OR entry.normalized_name LIKE '%'||lower($3)||'%' OR entry.model_id::text=$3)
  AND ($4='' OR entry.lifecycle_status=$4)
  AND ($5='' OR protocol.protocol_code=upper($5))
  AND ($6='' OR identity_record.adapter_version=$6)
  AND (NOT $7 OR entry.source_available<>$8)
`, q, lifecycle, protocol, adapter, hasMissing, missingValue)
	}, nil
}

func protocolSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "DRAFT", "PUBLISHED", "DEPRECATED")
	if err != nil {
		return nil, err
	}
	code, err := filter.upperString("protocol_code", 80)
	if err != nil {
		return nil, err
	}
	published, err := filter.boolean("published")
	if err != nil {
		return nil, err
	}
	from, err := filter.instant("created_from")
	if err != nil {
		return nil, err
	}
	to, err := filter.instant("created_to")
	if err != nil {
		return nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, ErrAdminQueryInvalidFilter
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT protocol.id::text AS resource_id,row_number() OVER (ORDER BY protocol.protocol_code ASC,protocol.version DESC,protocol.id ASC)-1 AS position FROM protocol_definitions protocol
WHERE ($3='' OR protocol.protocol_code=upper($3) OR protocol.id::text=$3)
AND ($4='' OR protocol.status=$4) AND ($5='' OR protocol.protocol_code=upper($5))
AND ($6::boolean IS NULL OR (protocol.published_at IS NOT NULL)=$6)
AND ($7::timestamptz IS NULL OR protocol.created_at >= $7) AND ($8::timestamptz IS NULL OR protocol.created_at < $8)
AND protocol.created_at<=$9`, q, status, code, published, from, to, highWater)
	}, nil
}

func formulaSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "DRAFT", "PUBLISHED", "DEPRECATED")
	if err != nil {
		return nil, err
	}
	protocolID, err := filter.uuid("protocol_definition_id")
	if err != nil {
		return nil, err
	}
	unit, err := filter.upperString("metering_unit_code", 80)
	if err != nil {
		return nil, err
	}
	published, err := filter.boolean("published")
	if err != nil {
		return nil, err
	}
	from, err := filter.instant("created_from")
	if err != nil {
		return nil, err
	}
	to, err := filter.instant("created_to")
	if err != nil {
		return nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, ErrAdminQueryInvalidFilter
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT formula.id::text AS resource_id,row_number() OVER (ORDER BY protocol.protocol_code ASC,formula.version DESC,formula.id ASC)-1 AS position FROM protocol_formula_versions formula
JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id
WHERE ($3='' OR formula.id::text=$3 OR protocol.protocol_code=upper($3)) AND ($4='' OR formula.status=$4)
AND ($5::uuid IS NULL OR formula.protocol_definition_id=$5) AND ($6='' OR formula.required_unit_versions ? $6)
AND ($7::boolean IS NULL OR (formula.published_at IS NOT NULL)=$7)
AND ($8::timestamptz IS NULL OR formula.created_at >= $8) AND ($9::timestamptz IS NULL OR formula.created_at < $9)
AND formula.created_at<=$10`, q, status, protocolID, unit, published, from, to, highWater)
	}, nil
}

func tokenAccountSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	ownerType, err := filter.enum("owner_type", "USER", "ORG", "ORG_UNIT")
	if err != nil {
		return nil, err
	}
	ownerID, err := filter.uuid("owner_id")
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "ACTIVE", "DISABLED", "FROZEN")
	if err != nil {
		return nil, err
	}
	health, err := filter.enum("health_status", "HEALTHY", "DEGRADED", "UNHEALTHY", "UNKNOWN")
	if err != nil {
		return nil, err
	}
	credential, err := filter.enum("credential_status", "NONE", "PENDING", "VALID", "INVALID", "REVOKED")
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT account.id::text AS resource_id,row_number() OVER (ORDER BY account.updated_at DESC,account.id ASC)-1 AS position FROM token_accounts account
WHERE account.management_mode='PLATFORM' AND account.updated_at<=$3
AND ($4='' OR account.name ILIKE '%'||$4||'%' OR account.id::text=$4)
AND ($5='' OR account.owner_type=$5) AND ($6::uuid IS NULL OR account.owner_id=$6) AND ($7='' OR account.status=$7)
AND ($8='' OR ($8='UNKNOWN' AND NOT EXISTS(SELECT 1 FROM routing_policies route WHERE route.token_account_id=account.id)) OR EXISTS(SELECT 1 FROM routing_policies route WHERE route.token_account_id=account.id AND route.health_status=$8))
AND ($9='' OR ($9='NONE' AND NOT EXISTS(SELECT 1 FROM provider_credentials credential WHERE credential.token_account_id=account.id)) OR EXISTS(SELECT 1 FROM provider_credentials credential WHERE credential.token_account_id=account.id AND (($9 IN ('VALID','INVALID') AND credential.validation_status=$9) OR ($9 NOT IN ('VALID','INVALID','NONE') AND credential.status=$9))))
`, highWater, q, ownerType, ownerID, status, health, credential)
	}, nil
}

func provisioningSnapshotSource(filter adminQueryFilter) (AdminSnapshotMemberSource, error) {
	q, err := filter.search("q", 160)
	if err != nil {
		return nil, err
	}
	status, err := filter.enum("status", "PENDING", "READY", "FAILED", "REPAIR_REQUIRED")
	if err != nil {
		return nil, err
	}
	modelID, err := filter.uuid("model_id")
	if err != nil {
		return nil, err
	}
	accountID, err := filter.uuid("token_account_id")
	if err != nil {
		return nil, err
	}
	scopeType, err := filter.enum("scope_type", "PERSONAL", "ORGANIZATION")
	if err != nil {
		return nil, err
	}
	scopeID, err := filter.uuid("scope_id")
	if err != nil {
		return nil, err
	}
	overdue, err := filter.boolean("sla_overdue")
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT provisioning.id::text AS resource_id,row_number() OVER (ORDER BY provisioning.updated_at DESC,provisioning.id DESC)-1 AS position FROM route_provisionings provisioning
WHERE provisioning.updated_at<=$3 AND ($4='' OR provisioning.id::text=$4)
AND ($5='' OR provisioning.status=$5) AND ($6::uuid IS NULL OR provisioning.model_id=$6)
AND ($7::uuid IS NULL OR provisioning.active_token_account_id=$7) AND ($8='' OR provisioning.scope_type=$8)
AND ($9::uuid IS NULL OR provisioning.scope_id=$9)
AND ($10::boolean IS NULL OR (provisioning.sla_deadline_at<$3 AND provisioning.status<>'READY')=$10)
`, highWater, q, status, modelID, accountID, scopeType, scopeID, overdue)
	}, nil
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
	caseType, err := filter.upperString("case_type", 120)
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
WHERE review.domain='TOKEN' AND (review.case_type ILIKE '%USAGE%' OR review.case_type ILIKE '%RECONCIL%') AND review.updated_at<=$3
AND ($4='' OR review.id::text=$4 OR review.resource_id::text=$4) AND ($5='' OR review.status=$5)
AND ($6='' OR review.severity=$6) AND ($7='' OR review.case_type=$7) AND ($8::uuid IS NULL OR review.owner_id=$8)
AND ($9::boolean IS NULL OR (LEAST(review.claim_due_at,review.first_response_due_at,review.mitigation_due_at,review.resolution_due_at)<$3 AND review.status='OPEN')=$9)
`, highWater, q, status, severity, caseType, ownerID, overdue)
	}, nil
}

func auditSnapshotSource(filter adminQueryFilter, resourceType string, resourceID uuid.UUID) (AdminSnapshotMemberSource, error) {
	action, err := filter.upperString("action", 120)
	if err != nil {
		return nil, err
	}
	actorID, err := filter.uuid("actor_id")
	if err != nil {
		return nil, err
	}
	from, err := filter.instant("occurred_from")
	if err != nil {
		return nil, err
	}
	to, err := filter.instant("occurred_to")
	if err != nil {
		return nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, ErrAdminQueryInvalidFilter
	}
	return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		if resourceType == "MODEL" {
			return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT audit.id::text AS resource_id,row_number() OVER (ORDER BY audit.created_at DESC,audit.id DESC)-1 AS position FROM model_catalog_audit audit
WHERE audit.model_id=$3 AND audit.created_at<=$4 AND ($5='' OR audit.action=$5) AND ($6::uuid IS NULL OR audit.actor_id=$6)
AND ($7::timestamptz IS NULL OR audit.created_at >= $7) AND ($8::timestamptz IS NULL OR audit.created_at < $8)
`, resourceID, highWater, action, actorID, from, to)
		}
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT audit.id::text AS resource_id,row_number() OVER (ORDER BY audit.occurred_at DESC,audit.id DESC)-1 AS position FROM token_audit_logs audit
WHERE audit.resource_type='TOKEN_ACCOUNT' AND audit.resource_id=$3 AND audit.occurred_at<=$4
AND ($5='' OR audit.action=$5) AND ($6::uuid IS NULL OR audit.actor_user_id=$6)
AND ($7::timestamptz IS NULL OR audit.occurred_at >= $7) AND ($8::timestamptz IS NULL OR audit.occurred_at < $8)
`, resourceID, highWater, action, actorID, from, to)
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

func parseResourceIDs(values []string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(values))
	for i, value := range values {
		id, err := uuid.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("invalid snapshot member: %w", err)
		}
		ids[i] = id
	}
	return ids, nil
}

func orderedValues[T any](keys []string, values map[string]T) ([]T, error) {
	items := make([]T, 0, len(keys))
	for _, key := range keys {
		item, ok := values[key]
		if !ok {
			return nil, ErrAdminSnapshotResourceChanged
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *PostgresRepository) adminModelsByIDs(ctx context.Context, resourceIDs []string) ([]AdminCatalogModel, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminCatalogModel{}, err
	}
	rows, err := r.pool.Query(ctx, catalogSelect+` WHERE e.catalog_version=(SELECT COALESCE(max(version),0) FROM model_catalog_versions) AND e.model_id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]AdminCatalogModel{}
	for rows.Next() {
		item, scanErr := scanCatalogModel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values[item.ModelID.String()] = AdminCatalogModel{CatalogModel: item}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	draftRows, err := r.pool.Query(ctx, `SELECT model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at FROM model_catalog_drafts WHERE model_id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer draftRows.Close()
	for draftRows.Next() {
		var draft CatalogDraft
		var modalities []byte
		if err = draftRows.Scan(&draft.ModelID, &draft.BaseCatalogVersion, &draft.DraftVersion, &draft.DisplayName, &draft.Description, &draft.Aliases, &draft.ProtocolCode, &draft.ProtocolVersion, &draft.ContextLength, &draft.MaximumOutputTokens, &modalities, &draft.TokenizerVersion, &draft.UpdatedBy, &draft.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(modalities, &draft.SupportedModalities)
		item, ok := values[draft.ModelID.String()]
		if ok {
			item.Draft = &draft
			values[draft.ModelID.String()] = item
		}
	}
	if err = draftRows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminProtocolsByIDs(ctx context.Context, resourceIDs []string) ([]ProtocolDefinition, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []ProtocolDefinition{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id,protocol_code,version,status,request_schema,created_at,published_at FROM protocol_definitions WHERE id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]ProtocolDefinition{}
	for rows.Next() {
		var item ProtocolDefinition
		if err = rows.Scan(&item.ID, &item.ProtocolCode, &item.Version, &item.Status, &item.RequestSchema, &item.CreatedAt, &item.PublishedAt); err != nil {
			return nil, err
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminFormulasByIDs(ctx context.Context, resourceIDs []string) ([]ProtocolFormulaVersion, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []ProtocolFormulaVersion{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT formula.id,formula.protocol_definition_id,protocol.protocol_code,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at FROM protocol_formula_versions formula JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id WHERE formula.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]ProtocolFormulaVersion{}
	for rows.Next() {
		var item ProtocolFormulaVersion
		var required, expression []byte
		if err = rows.Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(required, &item.RequiredUnitVersions)
		_ = json.Unmarshal(expression, &item.Expression)
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminTokenAccountsByIDs(ctx context.Context, resourceIDs []string) ([]AdminTokenAccount, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminTokenAccount{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.owner_unit_id,a.wallet_id,a.name,a.status,COALESCE(s.scope,'OWNER_UNIT_ONLY'),a.management_mode,count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE' AND c.validation_status='VALID'),count(DISTINCT p.id),a.updated_at FROM token_accounts a LEFT JOIN token_share_policies s ON s.token_account_id=a.id LEFT JOIN provider_credentials c ON c.token_account_id=a.id LEFT JOIN routing_policies p ON p.token_account_id=a.id WHERE a.management_mode='PLATFORM' AND a.id=ANY($1::uuid[]) GROUP BY a.id,s.scope`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]AdminTokenAccount{}
	for rows.Next() {
		var item AdminTokenAccount
		if err = rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Name, &item.Status, &item.ShareScope, &item.ManagementMode, &item.ActiveCredentialCount, &item.RouteCount, &item.UpdatedAt); err != nil {
			return nil, err
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminProvisioningsByIDs(ctx context.Context, resourceIDs []string) ([]RouteProvisioning, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []RouteProvisioning{}, err
	}
	rows, err := r.pool.Query(ctx, provisioningSelect+` WHERE id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]RouteProvisioning{}
	for rows.Next() {
		item, scanErr := scanProvisioning(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values[item.ID.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, values)
}

func (r *PostgresRepository) adminUsageByIDs(ctx context.Context, resourceIDs []string) ([]AdminUsageSummary, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminUsageSummary{}, err
	}
	// The collection projection is intentionally narrow. Provider identifiers,
	// credentials, request fingerprints, sessions, upstream references and raw
	// evidence are available only from the separately authorized detail/evidence
	// endpoints and can never leak through a paginated collection response.
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
	rows, err := r.pool.Query(ctx, `SELECT id,case_type,resource_type,resource_id,severity,status,owner_id,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,escalation_level,updated_at FROM manual_review_cases WHERE domain='TOKEN' AND id=ANY($1::uuid[])`, ids)
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

func (r *PostgresRepository) adminAuditByIDs(ctx context.Context, resourceType string, resourceIDs []string) ([]AdminResourceAudit, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AdminResourceAudit{}, err
	}
	values := map[string]AdminResourceAudit{}
	var rows pgx.Rows
	if resourceType == "MODEL" {
		rows, err = r.pool.Query(ctx, `SELECT id,action,'MODEL',model_id,actor_id,jsonb_build_object('before_status',before_status,'after_status',after_status,'catalog_version',catalog_version),created_at FROM model_catalog_audit WHERE id=ANY($1::uuid[])`, ids)
	} else {
		rows, err = r.pool.Query(ctx, `SELECT id,action,resource_type,resource_id,actor_user_id,summary,occurred_at FROM token_audit_logs WHERE id=ANY($1::uuid[])`, ids)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminResourceAudit
		if err = rows.Scan(&item.ID, &item.Action, &item.ResourceType, &item.ResourceID, &item.ActorID, &item.Summary, &item.OccurredAt); err != nil {
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
