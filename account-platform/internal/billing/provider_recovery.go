package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type recoveryQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type ProviderResumeVersions struct {
	ProviderID                  string     `json:"provider_id"`
	ProviderVersion             string     `json:"provider_version"`
	ProviderControlVersion      int64      `json:"provider_control_version"`
	ProbeOrderID                uuid.UUID  `json:"probe_order_id"`
	PolicyVersion               string     `json:"policy_version"`
	ObservationHighWatermark    *uuid.UUID `json:"observation_high_watermark"`
	EvaluationHighWatermark     *uuid.UUID `json:"evaluation_high_watermark"`
	ActiveMappingSetFingerprint string     `json:"active_mapping_set_fingerprint"`
}

type ProductResumeVersions struct {
	ProviderID               string     `json:"provider_id"`
	ProviderVersion          string     `json:"provider_version"`
	ProductID                string     `json:"product_id"`
	ProviderControlVersion   int64      `json:"provider_control_version"`
	ProductControlVersion    int64      `json:"product_control_version"`
	CatalogItemID            uuid.UUID  `json:"catalog_item_id"`
	CatalogVersion           int64      `json:"catalog_version"`
	AdmissionVersion         int64      `json:"admission_version"`
	MappingID                uuid.UUID  `json:"mapping_id"`
	MappingVersion           int64      `json:"mapping_version"`
	ProbeOrderID             uuid.UUID  `json:"probe_order_id"`
	PolicyVersion            string     `json:"policy_version"`
	ObservationHighWatermark *uuid.UUID `json:"observation_high_watermark"`
	EvaluationHighWatermark  *uuid.UUID `json:"evaluation_high_watermark"`
}

type ProviderResumeResult struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	ProviderID  string    `json:"provider_id"`
	Status      string    `json:"status"`
}

type ProductResumeResult struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	ProviderID  string    `json:"provider_id"`
	ProductID   string    `json:"product_id"`
	Status      string    `json:"status"`
}

func (s *Service) adminPaymentProviderControls(w http.ResponseWriter, req *http.Request) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CONTROL_UNAVAILABLE", "payment control state is unavailable", httpserver.RequestID(req.Context()))
		return
	}
	capabilities := s.payments.Capabilities()
	var providerEnabled bool
	var providerControlVersion int64
	var providerReasonCode string
	var providerReason *string
	var providerUpdatedAt time.Time
	err := repository.pool.QueryRow(req.Context(), `SELECT new_orders_enabled,version,reason_code,reason,updated_at FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, capabilities.ProviderID, capabilities.Version).Scan(&providerEnabled, &providerControlVersion, &providerReasonCode, &providerReason, &providerUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		providerEnabled, providerControlVersion, providerReasonCode, providerUpdatedAt = true, 0, "AVAILABLE", repository.now()
	} else if err != nil {
		s.writeError(w, req, err)
		return
	}

	rows, err := repository.pool.Query(req.Context(), `SELECT product_id,new_orders_enabled,version,reason_code,reason,updated_at FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 ORDER BY product_id`, capabilities.ProviderID, capabilities.Version)
	if err != nil {
		s.writeError(w, req, err)
		return
	}
	defer rows.Close()
	products := make([]map[string]any, 0)
	for rows.Next() {
		var productID, reasonCode, reason string
		var enabled bool
		var version int64
		var updatedAt time.Time
		if err := rows.Scan(&productID, &enabled, &version, &reasonCode, &reason, &updatedAt); err != nil {
			s.writeError(w, req, err)
			return
		}
		product := map[string]any{"product_id": productID, "new_orders_enabled": enabled, "version": version, "reason_code": reasonCode, "reason": reason, "updated_at": updatedAt}
		if !enabled && providerEnabled {
			var probeOrderID uuid.UUID
			_ = repository.pool.QueryRow(req.Context(), `SELECT confirmation.payment_order_id FROM payment_provider_failure_confirmations confirmation JOIN payment_orders payment ON payment.id=confirmation.payment_order_id WHERE payment.provider_id=$1 AND payment.provider_version=$2 AND payment.product_id=$3 ORDER BY confirmation.created_at DESC,confirmation.id DESC LIMIT 1`, capabilities.ProviderID, capabilities.Version, productID).Scan(&probeOrderID)
			if probeOrderID != uuid.Nil {
				if versions, err := repository.productResumeVersions(req.Context(), ProductResumeVersions{ProviderID: capabilities.ProviderID, ProviderVersion: capabilities.Version, ProductID: productID, ProbeOrderID: probeOrderID}); err == nil {
					product["resume_draft"] = recoveryDraft("PAYMENT_PRODUCT_RESUME", "PAYMENT_PRODUCT_CONTROL", productID, versions)
				}
			}
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, req, err)
		return
	}
	evidence, err := repository.paymentControlEvidence(req.Context(), capabilities.ProviderID, capabilities.Version)
	if err != nil {
		s.writeError(w, req, err)
		return
	}
	response := map[string]any{
		"provider_id": capabilities.ProviderID, "provider_version": capabilities.Version,
		"new_orders_enabled": providerEnabled, "provider_control_version": providerControlVersion,
		"reason_code": providerReasonCode, "reason": providerReason, "updated_at": providerUpdatedAt,
		"products":           products,
		"stop_loss_evidence": evidence,
	}
	if !providerEnabled {
		var probeOrderID uuid.UUID
		_ = repository.pool.QueryRow(req.Context(), `SELECT confirmation.payment_order_id FROM payment_provider_failure_confirmations confirmation JOIN payment_orders payment ON payment.id=confirmation.payment_order_id WHERE payment.provider_id=$1 AND payment.provider_version=$2 ORDER BY confirmation.created_at DESC,confirmation.id DESC LIMIT 1`, capabilities.ProviderID, capabilities.Version).Scan(&probeOrderID)
		if probeOrderID != uuid.Nil {
			if versions, err := repository.providerResumeVersions(req.Context(), ProviderResumeVersions{ProviderID: capabilities.ProviderID, ProviderVersion: capabilities.Version, ProbeOrderID: probeOrderID}); err == nil {
				response["resume_draft"] = recoveryDraft("PAYMENT_PROVIDER_RESUME", "PAYMENT_PROVIDER_CONTROL", capabilities.ProviderID, versions)
			}
		}
	}
	api.WriteJSON(w, http.StatusOK, response)
}

func recoveryDraft(commandType, resourceType, resourceID string, versions any) map[string]any {
	return map[string]any{"command_type": commandType, "target_resource_type": resourceType, "target_resource_id": resourceID, "normalized_command": versions, "critical_resource_versions": versions}
}

func (r *PostgresRepository) paymentControlEvidence(ctx context.Context, providerID, providerVersion string) (map[string]any, error) {
	var gapCount int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM payment_observation_gaps WHERE provider_id=$1 AND provider_version=$2`, providerID, providerVersion).Scan(&gapCount); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT scope_type,product_id,trigger_type,policy_version,window_start,window_end,metrics,observation_high_watermark,action_applied,evaluated_at FROM payment_stop_loss_evaluations WHERE provider_id=$1 AND provider_version=$2 ORDER BY evaluated_at DESC,id DESC LIMIT 20`, providerID, providerVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evaluations := make([]map[string]any, 0)
	for rows.Next() {
		var scopeType, triggerType, policyVersion string
		var productID *string
		var windowStart, windowEnd, evaluatedAt time.Time
		var metrics json.RawMessage
		var observationHighWatermark *uuid.UUID
		var actionApplied bool
		if err := rows.Scan(&scopeType, &productID, &triggerType, &policyVersion, &windowStart, &windowEnd, &metrics, &observationHighWatermark, &actionApplied, &evaluatedAt); err != nil {
			return nil, err
		}
		{
			evaluations = append(evaluations, map[string]any{"scope_type": scopeType, "product_id": productID, "trigger_type": triggerType, "policy_version": policyVersion, "window_start": windowStart, "window_end": windowEnd, "metrics": metrics, "observation_high_watermark": observationHighWatermark, "action_applied": actionApplied, "evaluated_at": evaluatedAt})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	productRows, err := r.pool.Query(ctx, `SELECT evaluation.id,evaluation.case_id,evaluation.severity,evaluation.payment_product_id,evaluation.action_snapshot,evaluation.evaluated_at FROM manual_review_stop_loss_evaluations evaluation WHERE evaluation.provider_id=$1 AND evaluation.provider_version=$2 AND evaluation.payment_product_id IS NOT NULL AND COALESCE((evaluation.action_snapshot->>'payment_product_paused')::boolean,FALSE) ORDER BY evaluation.evaluated_at DESC,evaluation.id DESC LIMIT 20`, providerID, providerVersion)
	if err != nil {
		return nil, err
	}
	{
		defer productRows.Close()
		for productRows.Next() {
			var evaluationID, caseID uuid.UUID
			var severity, productID string
			var metrics json.RawMessage
			var evaluatedAt time.Time
			if err := productRows.Scan(&evaluationID, &caseID, &severity, &productID, &metrics, &evaluatedAt); err != nil {
				return nil, err
			}
			{
				evaluations = append(evaluations, map[string]any{"evaluation_id": evaluationID, "scope_type": "PRODUCT", "product_id": productID, "trigger_type": "MANUAL_REVIEW_SLA", "severity": severity, "source_case_id": caseID, "policy_version": "product-stop-loss/v1", "metrics": metrics, "action_applied": true, "evaluated_at": evaluatedAt})
			}
		}
	}
	if err := productRows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"observation_gap_count": gapCount, "evaluations": evaluations}, nil
}

func latestObservationHighWatermark(ctx context.Context, q recoveryQuerier, providerID, providerVersion, productID string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND ($3='' OR product_id=$3) ORDER BY received_at DESC,id DESC LIMIT 1`, providerID, providerVersion, productID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func latestEvaluationHighWatermark(ctx context.Context, q recoveryQuerier, providerID, providerVersion, productID string) (*uuid.UUID, error) {
	var id uuid.UUID
	var err error
	if productID == "" {
		err = q.QueryRow(ctx, `SELECT id FROM payment_stop_loss_evaluations WHERE scope_type='PROVIDER' AND provider_id=$1 AND provider_version=$2 ORDER BY evaluated_at DESC,id DESC LIMIT 1`, providerID, providerVersion).Scan(&id)
	} else {
		err = q.QueryRow(ctx, `SELECT case_id FROM manual_review_stop_loss_evaluations WHERE provider_id=$1 AND provider_version=$2 AND payment_product_id=$3 AND COALESCE((action_snapshot->>'payment_product_paused')::boolean,FALSE) ORDER BY evaluated_at DESC,case_id DESC LIMIT 1`, providerID, providerVersion, productID).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func pointerUUIDEqual(left, right *uuid.UUID) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func (r *PostgresRepository) providerMappingFingerprint(ctx context.Context, q recoveryQuerier, providerID, providerVersion string) (string, error) {
	var count int
	var fingerprint string
	err := q.QueryRow(ctx, `SELECT COUNT(*),md5(COALESCE(string_agg(concat_ws(':',mapping.id,mapping.mapping_version,item.catalog_item_id,item.version,item.admission_version,binding.evidence_id),',' ORDER BY mapping.product_id,mapping.mapping_version),'')) FROM payment_product_mappings mapping JOIN recharge_product_catalog_items item ON item.product_id=mapping.product_id JOIN product_admission_bindings binding ON binding.catalog_item_id=item.catalog_item_id AND binding.mapping_id=mapping.id AND binding.effective_to IS NULL JOIN product_admission_evidence evidence ON evidence.evidence_id=binding.evidence_id WHERE mapping.provider_id=$1 AND mapping.provider_version=$2 AND mapping.status='FROZEN' AND mapping.effective_from<=now() AND (mapping.effective_to IS NULL OR mapping.effective_to>now()) AND item.lifecycle_status='ON_SALE' AND evidence.expires_at>now() AND NOT EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id)`, providerID, providerVersion).Scan(&count, &fingerprint)
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "", ErrConflict
	}
	return fingerprint, nil
}

func (r *PostgresRepository) hasObservationGap(ctx context.Context, q recoveryQuerier, providerID, providerVersion, productID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_observation_gaps WHERE provider_id=$1 AND provider_version=$2 AND ($3='' OR product_id=$3))`, providerID, providerVersion, productID).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) providerResumeVersions(ctx context.Context, requested ProviderResumeVersions) (ProviderResumeVersions, error) {
	return r.providerResumeVersionsFrom(ctx, r.pool, requested)
}

func (r *PostgresRepository) providerResumeVersionsFrom(ctx context.Context, source recoveryQuerier, requested ProviderResumeVersions) (ProviderResumeVersions, error) {
	actual := ProviderResumeVersions{ProviderID: strings.TrimSpace(requested.ProviderID), ProviderVersion: strings.TrimSpace(requested.ProviderVersion), ProbeOrderID: requested.ProbeOrderID, PolicyVersion: paymentStopLossPolicyVersion}
	if actual.ProviderID == "" || actual.ProviderVersion == "" || actual.ProbeOrderID == uuid.Nil {
		return ProviderResumeVersions{}, ErrConflict
	}
	var enabled bool
	if err := source.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, actual.ProviderID, actual.ProviderVersion).Scan(&enabled, &actual.ProviderControlVersion); err != nil {
		return ProviderResumeVersions{}, err
	}
	if enabled {
		return ProviderResumeVersions{}, ErrConflict
	}
	var err error
	if actual.ObservationHighWatermark, err = latestObservationHighWatermark(ctx, source, actual.ProviderID, actual.ProviderVersion, ""); err != nil {
		return ProviderResumeVersions{}, err
	}
	if actual.EvaluationHighWatermark, err = latestEvaluationHighWatermark(ctx, source, actual.ProviderID, actual.ProviderVersion, ""); err != nil {
		return ProviderResumeVersions{}, err
	}
	if actual.ActiveMappingSetFingerprint, err = r.providerMappingFingerprint(ctx, source, actual.ProviderID, actual.ProviderVersion); err != nil {
		return ProviderResumeVersions{}, err
	}
	gap, err := r.hasObservationGap(ctx, source, actual.ProviderID, actual.ProviderVersion, "")
	if err != nil || gap {
		return ProviderResumeVersions{}, ErrConflict
	}
	return actual, nil
}

func (r *PostgresRepository) productResumeVersions(ctx context.Context, requested ProductResumeVersions) (ProductResumeVersions, error) {
	return r.productResumeVersionsFrom(ctx, r.pool, requested)
}

func (r *PostgresRepository) productResumeVersionsFrom(ctx context.Context, source recoveryQuerier, requested ProductResumeVersions) (ProductResumeVersions, error) {
	actual := ProductResumeVersions{ProviderID: strings.TrimSpace(requested.ProviderID), ProviderVersion: strings.TrimSpace(requested.ProviderVersion), ProductID: strings.TrimSpace(requested.ProductID), ProbeOrderID: requested.ProbeOrderID, PolicyVersion: paymentStopLossPolicyVersion}
	if actual.ProviderID == "" || actual.ProviderVersion == "" || actual.ProductID == "" || actual.ProbeOrderID == uuid.Nil {
		return ProductResumeVersions{}, ErrConflict
	}
	var providerEnabled, productEnabled bool
	if err := source.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, actual.ProviderID, actual.ProviderVersion).Scan(&providerEnabled, &actual.ProviderControlVersion); err != nil || !providerEnabled {
		return ProductResumeVersions{}, ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3`, actual.ProviderID, actual.ProviderVersion, actual.ProductID).Scan(&productEnabled, &actual.ProductControlVersion); err != nil || productEnabled {
		return ProductResumeVersions{}, ErrConflict
	}
	if err := source.QueryRow(ctx, `SELECT item.catalog_item_id,item.version,item.admission_version,mapping.id,mapping.mapping_version FROM recharge_product_catalog_items item JOIN product_admission_bindings binding ON binding.catalog_item_id=item.catalog_item_id AND binding.effective_to IS NULL JOIN product_admission_evidence evidence ON evidence.evidence_id=binding.evidence_id JOIN payment_product_mappings mapping ON mapping.id=binding.mapping_id WHERE item.product_id=$1 AND item.lifecycle_status='ON_SALE' AND mapping.provider_id=$2 AND mapping.provider_version=$3 AND mapping.status='FROZEN' AND mapping.effective_from<=now() AND (mapping.effective_to IS NULL OR mapping.effective_to>now()) AND evidence.expires_at>now() AND NOT EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id)`, actual.ProductID, actual.ProviderID, actual.ProviderVersion).Scan(&actual.CatalogItemID, &actual.CatalogVersion, &actual.AdmissionVersion, &actual.MappingID, &actual.MappingVersion); err != nil {
		return ProductResumeVersions{}, err
	}
	var err error
	if actual.ObservationHighWatermark, err = latestObservationHighWatermark(ctx, source, actual.ProviderID, actual.ProviderVersion, actual.ProductID); err != nil {
		return ProductResumeVersions{}, err
	}
	if actual.EvaluationHighWatermark, err = latestEvaluationHighWatermark(ctx, source, actual.ProviderID, actual.ProviderVersion, actual.ProductID); err != nil {
		return ProductResumeVersions{}, err
	}
	gap, err := r.hasObservationGap(ctx, source, actual.ProviderID, actual.ProviderVersion, actual.ProductID)
	if err != nil || gap {
		return ProductResumeVersions{}, ErrConflict
	}
	return actual, nil
}

func sameProviderResumeVersions(left, right ProviderResumeVersions) bool {
	return left.ProviderID == right.ProviderID && left.ProviderVersion == right.ProviderVersion && left.ProviderControlVersion == right.ProviderControlVersion && left.ProbeOrderID == right.ProbeOrderID && left.PolicyVersion == right.PolicyVersion && left.ActiveMappingSetFingerprint == right.ActiveMappingSetFingerprint && pointerUUIDEqual(left.ObservationHighWatermark, right.ObservationHighWatermark) && pointerUUIDEqual(left.EvaluationHighWatermark, right.EvaluationHighWatermark)
}

func sameProductResumeVersions(left, right ProductResumeVersions) bool {
	return left.ProviderID == right.ProviderID && left.ProviderVersion == right.ProviderVersion && left.ProductID == right.ProductID && left.ProviderControlVersion == right.ProviderControlVersion && left.ProductControlVersion == right.ProductControlVersion && left.CatalogItemID == right.CatalogItemID && left.CatalogVersion == right.CatalogVersion && left.AdmissionVersion == right.AdmissionVersion && left.MappingID == right.MappingID && left.MappingVersion == right.MappingVersion && left.ProbeOrderID == right.ProbeOrderID && left.PolicyVersion == right.PolicyVersion && pointerUUIDEqual(left.ObservationHighWatermark, right.ObservationHighWatermark) && pointerUUIDEqual(left.EvaluationHighWatermark, right.EvaluationHighWatermark)
}

func (p *PaymentCoordinator) probeProviderRecovery(ctx context.Context, orderID uuid.UUID) (PaymentOrder, ProviderOrder, uuid.UUID, error) {
	if !p.Capabilities().OrderQuerySupported {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, ErrNotFound
	}
	if err != nil || order.ProviderID != p.Capabilities().ProviderID || order.ProviderVersion != p.Capabilities().Version {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, ErrConflict
	}
	before, err := latestTrustedProbeForOrder(ctx, p.repository.pool, orderID)
	if err != nil {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, err
	}
	scope := providerCallScope(order)
	scope.TrustedProbe = true
	providerOrder, err := providerCall(ctx, p, "PAYMENT_STATUS_QUERY", order.Channel, false, scope, func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, errors.Join(ErrPaymentProvider, err)
	}
	if !providerFailureMatchesOrder(providerOrder, order) {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, ErrConflict
	}
	after, err := latestTrustedProbeForOrder(ctx, p.repository.pool, orderID)
	if err != nil || after == nil || (before != nil && *before == *after) {
		return PaymentOrder{}, ProviderOrder{}, uuid.Nil, ErrConflict
	}
	return order, providerOrder, *after, nil
}

func latestTrustedProbeForOrder(ctx context.Context, q recoveryQuerier, orderID uuid.UUID) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM payment_provider_health_observations WHERE logical_order_id=$1 AND trusted_probe AND outcome='SUCCESS' ORDER BY received_at DESC,id DESC LIMIT 1`, orderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func (s *Service) executeApprovedProviderResumeCommand(ctx context.Context, approvalRequestID uuid.UUID, jobRunID, targetProviderID string, observedRaw json.RawMessage) (ProviderResumeResult, error) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || s.approvalGate == nil || s.payments == nil {
		return ProviderResumeResult{}, ErrApprovalExecutionUnavailable
	}
	var observed ProviderResumeVersions
	if json.Unmarshal(observedRaw, &observed) != nil || observed.ProviderID != strings.TrimSpace(targetProviderID) {
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	actual, err := repository.providerResumeVersions(ctx, observed)
	if err != nil {
		return ProviderResumeResult{}, err
	}
	actualRaw, _ := json.Marshal(actual)
	if !sameProviderResumeVersions(observed, actual) {
		_, _ = s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, actualRaw)
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	command, err := s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, observedRaw)
	if err != nil {
		if approvalGateUnavailable(err) {
			return ProviderResumeResult{}, ErrApprovalExecutionUnavailable
		}
		return ProviderResumeResult{}, ErrApprovalNotExecutable
	}
	fail := func(class string) {
		_ = s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), class)
	}
	var normalized ProviderResumeVersions
	if command.CommandType != "PAYMENT_PROVIDER_RESUME" || command.TargetResourceType != "PAYMENT_PROVIDER_CONTROL" || command.TargetResourceID != observed.ProviderID || json.Unmarshal(command.NormalizedCommand, &normalized) != nil || !sameProviderResumeVersions(normalized, observed) || !validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		fail("COMMAND_SNAPSHOT_MISMATCH")
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	order, providerOrder, probeObservationID, err := s.payments.probeProviderRecovery(ctx, observed.ProbeOrderID)
	if err != nil {
		fail("PROVIDER_PROBE_FAILED")
		return ProviderResumeResult{}, err
	}
	result, err := repository.executeProviderResume(ctx, observed, probeObservationID, approvalRequestID, command.RequesterUserID, command.ApproverUserID, command.DecisionReason, order, providerOrder)
	if err != nil {
		fail("DOMAIN_EXECUTION_FAILED")
		return ProviderResumeResult{}, err
	}
	domainResult, _ := json.Marshal(result)
	if err := s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "EXECUTED", domainResult, json.RawMessage(`{}`), ""); err != nil {
		return ProviderResumeResult{}, ErrApprovalCompletionPending
	}
	return result, nil
}

func (s *Service) executeApprovedProductResumeCommand(ctx context.Context, approvalRequestID uuid.UUID, jobRunID, targetProductID string, observedRaw json.RawMessage) (ProductResumeResult, error) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || s.approvalGate == nil || s.payments == nil {
		return ProductResumeResult{}, ErrApprovalExecutionUnavailable
	}
	var observed ProductResumeVersions
	if json.Unmarshal(observedRaw, &observed) != nil || observed.ProductID != strings.TrimSpace(targetProductID) {
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	actual, err := repository.productResumeVersions(ctx, observed)
	if err != nil {
		return ProductResumeResult{}, err
	}
	actualRaw, _ := json.Marshal(actual)
	if !sameProductResumeVersions(observed, actual) {
		_, _ = s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, actualRaw)
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	command, err := s.approvalGate.Begin(ctx, approvalRequestID, jobRunID, observedRaw)
	if err != nil {
		if approvalGateUnavailable(err) {
			return ProductResumeResult{}, ErrApprovalExecutionUnavailable
		}
		return ProductResumeResult{}, ErrApprovalNotExecutable
	}
	fail := func(class string) {
		_ = s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), class)
	}
	var normalized ProductResumeVersions
	if command.CommandType != "PAYMENT_PRODUCT_RESUME" || command.TargetResourceType != "PAYMENT_PRODUCT_CONTROL" || command.TargetResourceID != observed.ProductID || json.Unmarshal(command.NormalizedCommand, &normalized) != nil || !sameProductResumeVersions(normalized, observed) || !validIndependentApproval(command.RequesterUserID, command.ApproverUserID, command.DecisionReason) {
		fail("COMMAND_SNAPSHOT_MISMATCH")
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	order, providerOrder, probeObservationID, err := s.payments.probeProviderRecovery(ctx, observed.ProbeOrderID)
	if err != nil || order.ProductID != observed.ProductID {
		fail("PRODUCT_PROBE_FAILED")
		if err == nil {
			err = ErrConflict
		}
		return ProductResumeResult{}, err
	}
	result, err := repository.executeProductResume(ctx, observed, probeObservationID, approvalRequestID, command.RequesterUserID, command.ApproverUserID, command.DecisionReason, order, providerOrder)
	if err != nil {
		fail("DOMAIN_EXECUTION_FAILED")
		return ProductResumeResult{}, err
	}
	domainResult, _ := json.Marshal(result)
	if err := s.approvalGate.Complete(ctx, approvalRequestID, jobRunID, "EXECUTED", domainResult, json.RawMessage(`{}`), ""); err != nil {
		return ProductResumeResult{}, ErrApprovalCompletionPending
	}
	return result, nil
}

func validIndependentApproval(requesterID, approverID uuid.UUID, reason string) bool {
	return requesterID != uuid.Nil && approverID != uuid.Nil && requesterID != approverID && len([]rune(strings.TrimSpace(reason))) >= 10
}

func (r *PostgresRepository) openBlockingPaymentCase(ctx context.Context, q recoveryQuerier, providerID, providerVersion, productID string) (bool, error) {
	var blocking bool
	err := q.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM manual_review_cases review JOIN payment_anomaly_cases anomaly ON anomaly.id=review.id JOIN payment_orders payment ON payment.id=anomaly.payment_order_id WHERE review.severity IN ('P0','P1') AND review.status<>'RESOLVED' AND payment.provider_id=$1 AND payment.provider_version=$2 AND ($3='' OR payment.product_id=$3)
UNION ALL SELECT 1 FROM manual_review_cases review JOIN late_payment_review_cases late_case ON late_case.id=review.id JOIN payment_orders payment ON payment.id=late_case.payment_order_id WHERE review.severity IN ('P0','P1') AND review.status<>'RESOLVED' AND payment.provider_id=$1 AND payment.provider_version=$2 AND ($3='' OR payment.product_id=$3)
UNION ALL SELECT 1 FROM manual_review_cases review JOIN reconciliation_cases reconciliation ON reconciliation.id=review.id JOIN payment_orders payment ON payment.id=reconciliation.payment_order_id WHERE review.severity IN ('P0','P1') AND review.status<>'RESOLVED' AND payment.provider_id=$1 AND payment.provider_version=$2 AND ($3='' OR payment.product_id=$3)
)`, providerID, providerVersion, productID).Scan(&blocking)
	return blocking, err
}

func (r *PostgresRepository) validateFreshProbe(ctx context.Context, q recoveryQuerier, providerID, providerVersion, productID string, frozen *uuid.UUID, probe uuid.UUID) error {
	if probe == uuid.Nil {
		return ErrConflict
	}
	var latest uuid.UUID
	if err := q.QueryRow(ctx, `SELECT id FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND ($3='' OR product_id=$3) ORDER BY received_at DESC,id DESC LIMIT 1`, providerID, providerVersion, productID).Scan(&latest); err != nil || latest != probe {
		return ErrApprovalSnapshotMismatch
	}
	var unexpected int
	if frozen == nil {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND ($3='' OR product_id=$3) AND id<>$4`, providerID, providerVersion, productID, probe).Scan(&unexpected); err != nil {
			return err
		}
	} else if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND ($3='' OR product_id=$3) AND id>$4 AND id<>$5`, providerID, providerVersion, productID, *frozen, probe).Scan(&unexpected); err != nil {
		return err
	}
	if unexpected != 0 {
		return ErrApprovalSnapshotMismatch
	}
	return nil
}

func (r *PostgresRepository) approverConfirmedFailure(ctx context.Context, q recoveryQuerier, approverID uuid.UUID, providerID, providerVersion, productID string) (bool, error) {
	var confirmed bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_provider_failure_confirmations confirmation JOIN payment_orders payment ON payment.id=confirmation.payment_order_id WHERE confirmation.actor_id=$1 AND payment.provider_id=$2 AND payment.provider_version=$3 AND ($4='' OR payment.product_id=$4))`, approverID, providerID, providerVersion, productID).Scan(&confirmed)
	return confirmed, err
}

func (r *PostgresRepository) executeProviderResume(ctx context.Context, expected ProviderResumeVersions, probeObservationID uuid.UUID, approvalRequestID, requesterID, approverID uuid.UUID, reason string, order PaymentOrder, provider ProviderOrder) (ProviderResumeResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProviderResumeResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-provider.resume", expected.ProviderID+":"+expected.ProviderVersion); err != nil {
		return ProviderResumeResult{}, err
	}
	var enabled bool
	var controlVersion int64
	if err := tx.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE`, expected.ProviderID, expected.ProviderVersion).Scan(&enabled, &controlVersion); err != nil || enabled || controlVersion != expected.ProviderControlVersion {
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	fingerprint, err := r.providerMappingFingerprint(ctx, tx, expected.ProviderID, expected.ProviderVersion)
	if err != nil || fingerprint != expected.ActiveMappingSetFingerprint {
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	evaluation, err := latestEvaluationHighWatermark(ctx, tx, expected.ProviderID, expected.ProviderVersion, "")
	if err != nil || !pointerUUIDEqual(evaluation, expected.EvaluationHighWatermark) {
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	if gap, err := r.hasObservationGap(ctx, tx, expected.ProviderID, expected.ProviderVersion, ""); err != nil || gap {
		return ProviderResumeResult{}, ErrConflict
	}
	if err := r.validateFreshProbe(ctx, tx, expected.ProviderID, expected.ProviderVersion, "", expected.ObservationHighWatermark, probeObservationID); err != nil {
		return ProviderResumeResult{}, err
	}
	if blocking, err := r.openBlockingPaymentCase(ctx, tx, expected.ProviderID, expected.ProviderVersion, ""); err != nil || blocking {
		return ProviderResumeResult{}, ErrConflict
	}
	if confirmed, err := r.approverConfirmedFailure(ctx, tx, approverID, expected.ProviderID, expected.ProviderVersion, ""); err != nil || confirmed {
		return ProviderResumeResult{}, ErrConflict
	}
	if order.ID != expected.ProbeOrderID || order.ProviderID != expected.ProviderID || order.ProviderVersion != expected.ProviderVersion {
		return ProviderResumeResult{}, ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE payment_provider_controls SET new_orders_enabled=TRUE,reason_code='AVAILABLE',reason='',version=version+1,updated_at=$3 WHERE provider_id=$1 AND provider_version=$2`, expected.ProviderID, expected.ProviderVersion, now); err != nil {
		return ProviderResumeResult{}, err
	}
	executionID := uuid.Must(uuid.NewV7())
	versionsJSON, _ := json.Marshal(expected)
	if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_resume_executions(id,approval_request_id,provider_id,provider_version,probe_order_id,requester_user_id,approver_user_id,decision_reason,provider_status,provider_trace_id,resumed_product_ids,control_versions_before,executed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'[]'::jsonb,$11,$12)`, executionID, approvalRequestID, expected.ProviderID, expected.ProviderVersion, expected.ProbeOrderID, requesterID, approverID, strings.TrimSpace(reason), provider.ProviderStatus, provider.ProviderTraceID, versionsJSON, now); err != nil {
		return ProviderResumeResult{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.payment-provider.resumed.v1", executionID, 1, map[string]any{"execution_id": executionID, "approval_request_id": approvalRequestID, "provider_id": expected.ProviderID, "provider_version": expected.ProviderVersion}); err != nil {
		return ProviderResumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProviderResumeResult{}, err
	}
	return ProviderResumeResult{ExecutionID: executionID, ProviderID: expected.ProviderID, Status: "RESUMED"}, nil
}

func (r *PostgresRepository) executeProductResume(ctx context.Context, expected ProductResumeVersions, probeObservationID uuid.UUID, approvalRequestID, requesterID, approverID uuid.UUID, reason string, order PaymentOrder, provider ProviderOrder) (ProductResumeResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProductResumeResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-provider.resume", expected.ProviderID+":"+expected.ProviderVersion); err != nil {
		return ProductResumeResult{}, err
	}
	var providerEnabled, productEnabled bool
	var providerVersion, productVersion int64
	if err := tx.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE`, expected.ProviderID, expected.ProviderVersion).Scan(&providerEnabled, &providerVersion); err != nil || !providerEnabled || providerVersion != expected.ProviderControlVersion {
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	if err := tx.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3 FOR UPDATE`, expected.ProviderID, expected.ProviderVersion, expected.ProductID).Scan(&productEnabled, &productVersion); err != nil || productEnabled || productVersion != expected.ProductControlVersion {
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	var catalogItemID, mappingID uuid.UUID
	var catalogVersion, admissionVersion, mappingVersion int64
	if err := tx.QueryRow(ctx, `SELECT item.catalog_item_id,item.version,item.admission_version,mapping.id,mapping.mapping_version FROM recharge_product_catalog_items item JOIN product_admission_bindings binding ON binding.catalog_item_id=item.catalog_item_id AND binding.effective_to IS NULL JOIN product_admission_evidence evidence ON evidence.evidence_id=binding.evidence_id JOIN payment_product_mappings mapping ON mapping.id=binding.mapping_id WHERE item.product_id=$1 AND item.lifecycle_status='ON_SALE' AND mapping.provider_id=$2 AND mapping.provider_version=$3 AND mapping.status='FROZEN' AND mapping.effective_from<=now() AND (mapping.effective_to IS NULL OR mapping.effective_to>now()) AND evidence.expires_at>now() AND NOT EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id) FOR UPDATE OF item`, expected.ProductID, expected.ProviderID, expected.ProviderVersion).Scan(&catalogItemID, &catalogVersion, &admissionVersion, &mappingID, &mappingVersion); err != nil || catalogItemID != expected.CatalogItemID || catalogVersion != expected.CatalogVersion || admissionVersion != expected.AdmissionVersion || mappingID != expected.MappingID || mappingVersion != expected.MappingVersion {
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	evaluation, err := latestEvaluationHighWatermark(ctx, tx, expected.ProviderID, expected.ProviderVersion, expected.ProductID)
	if err != nil || !pointerUUIDEqual(evaluation, expected.EvaluationHighWatermark) {
		return ProductResumeResult{}, ErrApprovalSnapshotMismatch
	}
	if gap, err := r.hasObservationGap(ctx, tx, expected.ProviderID, expected.ProviderVersion, expected.ProductID); err != nil || gap {
		return ProductResumeResult{}, ErrConflict
	}
	if err := r.validateFreshProbe(ctx, tx, expected.ProviderID, expected.ProviderVersion, expected.ProductID, expected.ObservationHighWatermark, probeObservationID); err != nil {
		return ProductResumeResult{}, err
	}
	if blocking, err := r.openBlockingPaymentCase(ctx, tx, expected.ProviderID, expected.ProviderVersion, expected.ProductID); err != nil || blocking {
		return ProductResumeResult{}, ErrConflict
	}
	if confirmed, err := r.approverConfirmedFailure(ctx, tx, approverID, expected.ProviderID, expected.ProviderVersion, expected.ProductID); err != nil || confirmed {
		return ProductResumeResult{}, ErrConflict
	}
	if order.ID != expected.ProbeOrderID || order.ProductID != expected.ProductID || order.ProviderID != expected.ProviderID || order.ProviderVersion != expected.ProviderVersion {
		return ProductResumeResult{}, ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE payment_product_controls SET new_orders_enabled=TRUE,reason_code='AVAILABLE',reason='',source_case_id=NULL,version=version+1,updated_by=$4,updated_at=$5 WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3`, expected.ProviderID, expected.ProviderVersion, expected.ProductID, approverID, now); err != nil {
		return ProductResumeResult{}, err
	}
	executionID := uuid.Must(uuid.NewV7())
	versionsJSON, _ := json.Marshal(expected)
	if _, err := tx.Exec(ctx, `INSERT INTO payment_product_resume_executions(id,approval_request_id,provider_id,provider_version,product_id,probe_order_id,requester_user_id,approver_user_id,decision_reason,provider_status,provider_trace_id,control_versions_before,executed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, executionID, approvalRequestID, expected.ProviderID, expected.ProviderVersion, expected.ProductID, expected.ProbeOrderID, requesterID, approverID, strings.TrimSpace(reason), provider.ProviderStatus, provider.ProviderTraceID, versionsJSON, now); err != nil {
		return ProductResumeResult{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.payment-product.resumed.v1", executionID, 1, map[string]any{"execution_id": executionID, "approval_request_id": approvalRequestID, "provider_id": expected.ProviderID, "provider_version": expected.ProviderVersion, "product_id": expected.ProductID}); err != nil {
		return ProductResumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProductResumeResult{}, err
	}
	return ProductResumeResult{ExecutionID: executionID, ProviderID: expected.ProviderID, ProductID: expected.ProductID, Status: "RESUMED"}, nil
}
