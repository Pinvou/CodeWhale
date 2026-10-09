package token

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const accountRecoveryQueueSort = "updated_at DESC,command_reference ASC"

// AccountRecoveryQueueItem is the operational projection of a command awaiting
// human recovery. Internal authority checkpoints, leases and wallet receipts
// stay in their owning services.
type AccountRecoveryQueueItem struct {
	CommandReference uuid.UUID `json:"command_reference"`
	CommandType      string    `json:"command_type"`
	Status           string    `json:"status"`
	RecoveryState    string    `json:"recovery_state"`
	RecoveryEpoch    int64     `json:"recovery_epoch"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s *Service) adminTokenAccountRecoveryQueue(w http.ResponseWriter, r *http.Request) {
	_, version, ok := s.requireTokenAccountCreateOperator(w, r, false)
	if !ok {
		return
	}
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(version, 10))
	w.Header().Set("Cache-Control", "no-store")
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	q, err := filter.search("q", 36)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
		return
	}
	source := func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		return adminSnapshotInsert(ctx, tx, snapshotID, maximum, `SELECT reservation.command_reference::text AS resource_id,row_number() OVER (ORDER BY reservation.updated_at DESC,reservation.command_reference ASC)-1 AS position
FROM token_account_provisioning_reservations reservation JOIN token_command_results result USING(command_reference)
WHERE reservation.status='MANUAL_REVIEW' AND result.status='PENDING' AND result.command_type='TOKEN_ACCOUNT_CREATE'
AND reservation.updated_at<=$3 AND ($4::text='' OR reservation.command_reference::text=$4::text)`, highWater, q)
	}
	if !s.adminQueryPage(w, r, filter, "token-account-recovery", "platform", accountRecoveryQueueSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) {
		return repository.accountRecoveryQueueByIDs(ctx, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (r *PostgresRepository) accountRecoveryQueueByIDs(ctx context.Context, resourceIDs []string) ([]AccountRecoveryQueueItem, error) {
	ids, err := parseResourceIDs(resourceIDs)
	if err != nil || len(ids) == 0 {
		return []AccountRecoveryQueueItem{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT reservation.command_reference,result.command_type,result.status,reservation.status,reservation.recovery_epoch,reservation.created_at,reservation.updated_at
FROM token_account_provisioning_reservations reservation JOIN token_command_results result USING(command_reference)
WHERE reservation.command_reference=ANY($1::uuid[]) AND reservation.status='MANUAL_REVIEW' AND result.status='PENDING' AND result.command_type='TOKEN_ACCOUNT_CREATE'`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]AccountRecoveryQueueItem, len(ids))
	for rows.Next() {
		var item AccountRecoveryQueueItem
		if err = rows.Scan(&item.CommandReference, &item.CommandType, &item.Status, &item.RecoveryState, &item.RecoveryEpoch, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items[item.CommandReference.String()] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return orderedValues(resourceIDs, items)
}
