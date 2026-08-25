package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Service) adminPaymentProviderControls(w http.ResponseWriter, req *http.Request) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok || s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CONTROL_UNAVAILABLE", "payment control state is unavailable", httpserver.RequestID(req.Context()))
		return
	}
	capabilities := s.payments.Capabilities()
	var providerEnabled bool
	var providerVersion int64
	var reason *string
	var providerUpdatedAt time.Time
	err := repository.pool.QueryRow(req.Context(), `SELECT new_orders_enabled,version,reason,updated_at FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, capabilities.ProviderID, capabilities.Version).Scan(&providerEnabled, &providerVersion, &reason, &providerUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		api.WriteJSON(w, http.StatusOK, map[string]any{"provider_id": capabilities.ProviderID, "provider_version": capabilities.Version, "new_orders_enabled": true, "provider_control_version": 0, "reason_code": "AVAILABLE", "products": []any{}})
		return
	}
	if err != nil {
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
	productVersions := map[string]int64{}
	for rows.Next() {
		var productID, productReasonCode, productReason string
		var enabled bool
		var version int64
		var updatedAt time.Time
		if err := rows.Scan(&productID, &enabled, &version, &productReasonCode, &productReason, &updatedAt); err != nil {
			s.writeError(w, req, err)
			return
		}
		products = append(products, map[string]any{"product_id": productID, "new_orders_enabled": enabled, "version": version, "reason_code": productReasonCode, "reason": productReason, "updated_at": updatedAt})
		if !enabled {
			productVersions[productID] = version
		}
	}
	var probeOrderID uuid.UUID
	_ = repository.pool.QueryRow(req.Context(), `SELECT confirmation.payment_order_id FROM payment_provider_failure_confirmations confirmation JOIN payment_orders payment ON payment.id=confirmation.payment_order_id WHERE payment.provider_id=$1 AND payment.provider_version=$2 ORDER BY confirmation.confirmed_at DESC LIMIT 1`, capabilities.ProviderID, capabilities.Version).Scan(&probeOrderID)
	reasonCode := "AUTOMATIC_STOP_LOSS"
	if providerEnabled {
		reasonCode = "AVAILABLE"
	}
	response := map[string]any{"provider_id": capabilities.ProviderID, "provider_version": capabilities.Version, "new_orders_enabled": providerEnabled, "provider_control_version": providerVersion, "reason_code": reasonCode, "reason": reason, "updated_at": providerUpdatedAt, "products": products}
	if !providerEnabled && probeOrderID != uuid.Nil && len(productVersions) > 0 {
		versions := ProviderResumeVersions{ProviderID: capabilities.ProviderID, ProviderVersion: capabilities.Version, ProviderControlVersion: providerVersion, ProductControlVersions: productVersions, ProbeOrderID: probeOrderID}
		response["resume_draft"] = map[string]any{"command_type": "PAYMENT_PROVIDER_RESUME", "target_resource_type": "PAYMENT_PROVIDER_CONTROL", "target_resource_id": capabilities.ProviderID, "normalized_command": versions, "critical_resource_versions": versions}
	}
	api.WriteJSON(w, http.StatusOK, response)
}

type ProviderResumeVersions struct {
	ProviderID             string           `json:"provider_id"`
	ProviderVersion        string           `json:"provider_version"`
	ProviderControlVersion int64            `json:"provider_control_version"`
	ProductControlVersions map[string]int64 `json:"product_control_versions"`
	ProbeOrderID           uuid.UUID        `json:"probe_order_id"`
}

type ProviderResumeResult struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	ProviderID  string    `json:"provider_id"`
	Status      string    `json:"status"`
}

func (p *PaymentCoordinator) probeProviderFailure(ctx context.Context, orderID uuid.UUID) (PaymentOrder, ProviderOrder, error) {
	if !p.Capabilities().OrderQuerySupported {
		return PaymentOrder{}, ProviderOrder{}, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ProviderOrder{}, ErrNotFound
	}
	if err != nil || order.ProviderID != p.Capabilities().ProviderID || order.ProviderVersion != p.Capabilities().Version {
		return PaymentOrder{}, ProviderOrder{}, ErrConflict
	}
	providerOrder, err := providerCall(p, "ORDER_QUERY", order.Channel, false, func() (ProviderOrder, error) {
		return p.provider.Get(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentOrder{}, ProviderOrder{}, errors.Join(ErrPaymentProvider, err)
	}
	if !providerFailureMatchesOrder(providerOrder, order) {
		return PaymentOrder{}, ProviderOrder{}, ErrConflict
	}
	return order, providerOrder, nil
}

func (r *PostgresRepository) providerResumeVersions(ctx context.Context, requested ProviderResumeVersions) (ProviderResumeVersions, error) {
	actual := ProviderResumeVersions{ProviderID: strings.TrimSpace(requested.ProviderID), ProviderVersion: strings.TrimSpace(requested.ProviderVersion), ProbeOrderID: requested.ProbeOrderID, ProductControlVersions: map[string]int64{}}
	if actual.ProviderID == "" || actual.ProviderVersion == "" || actual.ProbeOrderID == uuid.Nil || len(requested.ProductControlVersions) == 0 {
		return ProviderResumeVersions{}, ErrConflict
	}
	var enabled bool
	if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, actual.ProviderID, actual.ProviderVersion).Scan(&enabled, &actual.ProviderControlVersion); err != nil {
		return ProviderResumeVersions{}, err
	}
	if enabled {
		return ProviderResumeVersions{}, ErrConflict
	}
	for productID := range requested.ProductControlVersions {
		var productEnabled bool
		var version int64
		if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3`, actual.ProviderID, actual.ProviderVersion, productID).Scan(&productEnabled, &version); err != nil {
			return ProviderResumeVersions{}, err
		}
		if productEnabled {
			return ProviderResumeVersions{}, ErrConflict
		}
		actual.ProductControlVersions[productID] = version
	}
	return actual, nil
}

func sameProviderResumeVersions(left, right ProviderResumeVersions) bool {
	if left.ProviderID != right.ProviderID || left.ProviderVersion != right.ProviderVersion || left.ProviderControlVersion != right.ProviderControlVersion || left.ProbeOrderID != right.ProbeOrderID || len(left.ProductControlVersions) != len(right.ProductControlVersions) {
		return false
	}
	for productID, version := range left.ProductControlVersions {
		if right.ProductControlVersions[productID] != version {
			return false
		}
	}
	return true
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
	if command.CommandType != "PAYMENT_PROVIDER_RESUME" || command.TargetResourceType != "PAYMENT_PROVIDER_CONTROL" || command.TargetResourceID != observed.ProviderID || json.Unmarshal(command.NormalizedCommand, &normalized) != nil || !sameProviderResumeVersions(normalized, observed) || command.RequesterUserID == uuid.Nil || command.ApproverUserID == uuid.Nil || command.RequesterUserID == command.ApproverUserID || len([]rune(strings.TrimSpace(command.DecisionReason))) < 10 {
		fail("COMMAND_SNAPSHOT_MISMATCH")
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	order, providerOrder, err := s.payments.probeProviderFailure(ctx, observed.ProbeOrderID)
	if err != nil {
		fail("PROVIDER_PROBE_FAILED")
		return ProviderResumeResult{}, err
	}
	result, err := repository.executeProviderResume(ctx, observed, approvalRequestID, command.RequesterUserID, command.ApproverUserID, command.DecisionReason, order, providerOrder)
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

func (r *PostgresRepository) executeProviderResume(ctx context.Context, expected ProviderResumeVersions, approvalRequestID, requesterID, approverID uuid.UUID, reason string, order PaymentOrder, provider ProviderOrder) (ProviderResumeResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProviderResumeResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-provider.resume", expected.ProviderID+":"+expected.ProviderVersion); err != nil {
		return ProviderResumeResult{}, err
	}
	var enabled bool
	var providerVersion int64
	if err := tx.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE`, expected.ProviderID, expected.ProviderVersion).Scan(&enabled, &providerVersion); err != nil || enabled || providerVersion != expected.ProviderControlVersion {
		return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
	}
	for productID, version := range expected.ProductControlVersions {
		var productEnabled bool
		var actualVersion int64
		if err := tx.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3 FOR UPDATE`, expected.ProviderID, expected.ProviderVersion, productID).Scan(&productEnabled, &actualVersion); err != nil || productEnabled || actualVersion != version {
			return ProviderResumeResult{}, ErrApprovalSnapshotMismatch
		}
		var mappingExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_product_mappings WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3 AND status='FROZEN' AND effective_from<=now() AND (effective_to IS NULL OR effective_to>now()))`, expected.ProviderID, expected.ProviderVersion, productID).Scan(&mappingExists); err != nil || !mappingExists {
			return ProviderResumeResult{}, ErrConflict
		}
	}
	var blockingCase, approverConfirmed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM manual_review_cases review JOIN payment_orders payment ON payment.id=review.resource_id WHERE review.severity IN ('P0','P1') AND review.status NOT IN ('RESOLVED','CLOSED') AND payment.provider_id=$1 AND payment.provider_version=$2)`, expected.ProviderID, expected.ProviderVersion).Scan(&blockingCase); err != nil || blockingCase {
		return ProviderResumeResult{}, ErrConflict
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_provider_failure_confirmations confirmation JOIN payment_orders payment ON payment.id=confirmation.payment_order_id WHERE confirmation.actor_id=$1 AND payment.provider_id=$2 AND payment.provider_version=$3)`, approverID, expected.ProviderID, expected.ProviderVersion).Scan(&approverConfirmed); err != nil || approverConfirmed {
		return ProviderResumeResult{}, ErrConflict
	}
	if order.ID != expected.ProbeOrderID || order.ProviderID != expected.ProviderID || order.ProviderVersion != expected.ProviderVersion {
		return ProviderResumeResult{}, ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE payment_provider_controls SET new_orders_enabled=TRUE,reason='',version=version+1,updated_at=$3 WHERE provider_id=$1 AND provider_version=$2`, expected.ProviderID, expected.ProviderVersion, now); err != nil {
		return ProviderResumeResult{}, err
	}
	productIDs := make([]string, 0, len(expected.ProductControlVersions))
	for productID := range expected.ProductControlVersions {
		productIDs = append(productIDs, productID)
		if _, err := tx.Exec(ctx, `UPDATE payment_product_controls SET new_orders_enabled=TRUE,reason_code='AVAILABLE',reason='',source_case_id=NULL,version=version+1,updated_by=$4,updated_at=$5 WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3`, expected.ProviderID, expected.ProviderVersion, productID, approverID, now); err != nil {
			return ProviderResumeResult{}, err
		}
	}
	sort.Strings(productIDs)
	executionID := uuid.Must(uuid.NewV7())
	productsJSON, _ := json.Marshal(productIDs)
	versionsJSON, _ := json.Marshal(expected)
	if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_resume_executions(id,approval_request_id,provider_id,provider_version,probe_order_id,requester_user_id,approver_user_id,decision_reason,provider_status,provider_trace_id,resumed_product_ids,control_versions_before,executed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, executionID, approvalRequestID, expected.ProviderID, expected.ProviderVersion, expected.ProbeOrderID, requesterID, approverID, strings.TrimSpace(reason), provider.ProviderStatus, provider.ProviderTraceID, productsJSON, versionsJSON, now); err != nil {
		return ProviderResumeResult{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.payment-provider.resumed.v1", executionID, 1, map[string]any{"execution_id": executionID, "approval_request_id": approvalRequestID, "provider_id": expected.ProviderID, "provider_version": expected.ProviderVersion, "product_ids": productIDs}); err != nil {
		return ProviderResumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProviderResumeResult{}, err
	}
	return ProviderResumeResult{ExecutionID: executionID, ProviderID: expected.ProviderID, Status: "RESUMED"}, nil
}
