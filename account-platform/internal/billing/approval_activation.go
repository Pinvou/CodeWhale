package billing

import (
	"context"
	"errors"
	"time"
)

var ErrApprovalActivationUnavailable = errors.New("billing approval command activation is unavailable")

const approvalActivationSchemaVersion = 1

type ApprovalCommandActivation struct {
	CommandType           string    `json:"command_type"`
	PolicyVersion         int64     `json:"policy_version"`
	SnapshotSchemaVersion int       `json:"snapshot_schema_version"`
	State                 string    `json:"state"`
	Version               int64     `json:"version"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type ApprovalCommandActivationSnapshot struct {
	SchemaVersion int                         `json:"schema_version"`
	Activations   []ApprovalCommandActivation `json:"activations"`
}

type approvalActivationRepository interface {
	ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error)
}

type activationSpec struct {
	policyVersion         int64
	snapshotSchemaVersion int
	mayBeActive           bool
}

var billingApprovalActivationSpecs = map[string]activationSpec{
	"CHARGEBACK_FINANCIAL_REPAIR":      {1, 1, false},
	"ENTITLEMENT_COMPONENT_ADJUSTMENT": {1, 1, false},
	"LEDGER_REPAIR":                    {1, 1, false},
	"PAYMENT_FINANCIAL_REPAIR":         {1, 1, false},
	"PAYMENT_PRODUCT_RESUME":           {1, 1, true},
	"PAYMENT_PROVIDER_RESUME":          {2, 1, true},
	"RECEIVABLE_WRITE_OFF":             {1, 1, false},
	"RECONCILIATION_FINANCIAL_REPAIR":  {1, 1, false},
	"REFUND_EXCEPTION_REPAIR":          {1, 1, false},
	"USAGE_SETTLEMENT_REPAIR":          {1, 1, false},
	"WALLET_ADJUSTMENT":                {1, 1, true},
}

var billingApprovalActivationOrder = []string{
	"CHARGEBACK_FINANCIAL_REPAIR",
	"ENTITLEMENT_COMPONENT_ADJUSTMENT",
	"LEDGER_REPAIR",
	"PAYMENT_FINANCIAL_REPAIR",
	"PAYMENT_PRODUCT_RESUME",
	"PAYMENT_PROVIDER_RESUME",
	"RECEIVABLE_WRITE_OFF",
	"RECONCILIATION_FINANCIAL_REPAIR",
	"REFUND_EXCEPTION_REPAIR",
	"USAGE_SETTLEMENT_REPAIR",
	"WALLET_ADJUSTMENT",
}

// validateApprovalCommandActivationSnapshot is the single server-side contract
// gate used at both the storage and HTTP boundaries. In particular, a test or
// alternate repository cannot make the workload endpoint emit a partial or
// future-incompatible projection.
func validateApprovalCommandActivationSnapshot(snapshot ApprovalCommandActivationSnapshot, now time.Time) error {
	if snapshot.SchemaVersion != approvalActivationSchemaVersion || len(snapshot.Activations) != len(billingApprovalActivationOrder) {
		return ErrApprovalActivationUnavailable
	}
	for index, commandType := range billingApprovalActivationOrder {
		item := snapshot.Activations[index]
		spec := billingApprovalActivationSpecs[commandType]
		if item.CommandType != commandType || item.PolicyVersion != spec.policyVersion || item.SnapshotSchemaVersion != spec.snapshotSchemaVersion ||
			item.Version < 1 || item.UpdatedAt.IsZero() || item.UpdatedAt.After(now.Add(time.Second)) ||
			(item.State != "ACTIVE" && item.State != "INACTIVE") || (!spec.mayBeActive && item.State != "INACTIVE") {
			return ErrApprovalActivationUnavailable
		}
	}
	return nil
}

func (r *PostgresRepository) ApprovalCommandActivations(ctx context.Context) (ApprovalCommandActivationSnapshot, error) {
	rows, err := r.pool.Query(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activations ORDER BY command_type`)
	if err != nil {
		return ApprovalCommandActivationSnapshot{}, ErrApprovalActivationUnavailable
	}
	defer rows.Close()

	result := ApprovalCommandActivationSnapshot{SchemaVersion: approvalActivationSchemaVersion, Activations: make([]ApprovalCommandActivation, 0, len(billingApprovalActivationSpecs))}
	for rows.Next() {
		var item ApprovalCommandActivation
		if err := rows.Scan(&item.CommandType, &item.PolicyVersion, &item.SnapshotSchemaVersion, &item.State, &item.Version, &item.UpdatedAt); err != nil {
			return ApprovalCommandActivationSnapshot{}, ErrApprovalActivationUnavailable
		}
		result.Activations = append(result.Activations, item)
	}
	if rows.Err() != nil || validateApprovalCommandActivationSnapshot(result, r.now()) != nil {
		return ApprovalCommandActivationSnapshot{}, ErrApprovalActivationUnavailable
	}
	return result, nil
}
