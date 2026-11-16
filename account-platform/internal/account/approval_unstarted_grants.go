package account

import (
	"context"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type UnstartedGrantCursor struct {
	EventID uuid.UUID `json:"event_id"`
	Durable string    `json:"durable"`
}

// UnstartedGrant is an Account snapshot observation, not an execution or lease.
// It deliberately has no synthetic execution ID, retry right or terminal verdict.
type UnstartedGrant struct {
	OriginalEventID    uuid.UUID `json:"original_event_id"`
	Durable            string    `json:"durable"`
	RequestID          uuid.UUID `json:"approval_request_id"`
	CommandType        string    `json:"command_type"`
	PolicyVersion      int64     `json:"policy_version"`
	RequestStatus      string    `json:"request_status"`
	RequestVersion     int64     `json:"request_version"`
	CommandFingerprint string    `json:"command_fingerprint"`
	ExecutionDeadline  time.Time `json:"execution_deadline"`
	DeliveryVersion    int64     `json:"delivery_version"`
	DomainAttemptCount int       `json:"domain_attempt_count"`
	EffectConclusion   string    `json:"effect_conclusion"`
	NextAction         string    `json:"next_action"`
}

type UnstartedGrantInventory struct {
	SchemaVersion int                   `json:"schema_version"`
	ObservedAt    time.Time             `json:"observed_at"`
	Items         []UnstartedGrant      `json:"items"`
	Next          *UnstartedGrantCursor `json:"next,omitempty"`
}

// InspectUnstartedApprovalGrants does not contact a domain or mutate state.
// Request status is intentionally not filtered: expiry/invalidation cannot
// erase an unknown granted attempt from the operator's inventory.
func (r *PostgresRepository) InspectUnstartedApprovalGrants(ctx context.Context, limit int, after *UnstartedGrantCursor) (UnstartedGrantInventory, error) {
	if limit < 1 || limit > 200 || (after != nil && (after.EventID == uuid.Nil || after.Durable == "")) {
		return UnstartedGrantInventory{}, ErrApprovalRecoveryPlanInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return UnstartedGrantInventory{}, err
	}
	defer tx.Rollback(bounded)
	result := UnstartedGrantInventory{SchemaVersion: 1, Items: make([]UnstartedGrant, 0)}
	if err = tx.QueryRow(bounded, "SELECT transaction_timestamp()").Scan(&result.ObservedAt); err != nil {
		return UnstartedGrantInventory{}, err
	}
	var event any
	var durable any
	if after != nil {
		event = after.EventID
		durable = after.Durable
	}
	rows, err := tx.Query(bounded, `SELECT * FROM public.list_approval_unstarted_grants($1,$2,$3)`, limit+1, event, durable)
	if err != nil {
		return UnstartedGrantInventory{}, err
	}
	for rows.Next() {
		var item UnstartedGrant
		if err = rows.Scan(&item.OriginalEventID, &item.Durable, &item.RequestID, &item.CommandType, &item.PolicyVersion, &item.RequestStatus, &item.RequestVersion, &item.CommandFingerprint, &item.ExecutionDeadline, &item.DeliveryVersion, &item.DomainAttemptCount); err != nil {
			rows.Close()
			return UnstartedGrantInventory{}, err
		}
		descriptor, ok := approvalconsumer.LookupDurable(item.Durable)
		if !ok || !approvalconsumer.MatchRoute(descriptor, item.CommandType, item.PolicyVersion, approvalconsumer.BillingWorkload) || item.RequestID == uuid.Nil || item.OriginalEventID == uuid.Nil || !approvaleffect.ValidFingerprint(item.CommandFingerprint) || item.DomainAttemptCount < 1 || item.DomainAttemptCount > approvalconsumer.MaxDomainAttempts || item.ExecutionDeadline.IsZero() || item.DeliveryVersion < 1 || item.RequestVersion < 1 {
			rows.Close()
			return UnstartedGrantInventory{}, ErrApprovalRecoveryPlanInvalid
		}
		item.EffectConclusion = "UNKNOWN"
		item.NextAction = "QUERY_ONLY_NO_REEXECUTION"
		item.ExecutionDeadline = item.ExecutionDeadline.UTC()
		if len(result.Items) < limit {
			result.Items = append(result.Items, item)
		} else {
			last := result.Items[len(result.Items)-1]
			result.Next = &UnstartedGrantCursor{EventID: last.OriginalEventID, Durable: last.Durable}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return UnstartedGrantInventory{}, err
	}
	if err = tx.Commit(bounded); err != nil {
		return UnstartedGrantInventory{}, err
	}
	result.ObservedAt = result.ObservedAt.UTC()
	return result, nil
}
