package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Existing partial installations keep their old SLA worker contract. The new
// recovery API itself fails closed until its official migration is installed.
func (r *PostgresRepository) syncGrantRecoveryCases(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var installed bool
	if err := r.pool.QueryRow(bounded, `SELECT to_regprocedure('public.pending_approval_grant_recovery(integer)') IS NOT NULL`).Scan(&installed); err != nil {
		return 0, err
	}
	if !installed {
		return 0, nil
	}
	rows, err := r.pool.Query(bounded, `SELECT * FROM public.pending_approval_grant_recovery($1)`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		event, request uuid.UUID
		durable        string
	}
	items := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.event, &item.durable, &item.request); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	created := 0
	for _, item := range items {
		admitted, e := r.admitGrantRecoveryCase(bounded, item.event, item.durable, item.request)
		if e != nil {
			return created, e
		}
		if admitted {
			created++
		}
	}
	return created, nil
}

func (r *PostgresRepository) admitGrantRecoveryCase(ctx context.Context, eventID uuid.UUID, durable string, requestID uuid.UUID) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM approval_requests WHERE id=$1 FOR UPDATE SKIP LOCKED`, requestID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	deadlines, err := manualreview.DeadlinesFor(now, "P1")
	if err != nil {
		return false, err
	}
	id := uuid.Must(uuid.NewV7())
	err = tx.QueryRow(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
 VALUES($1,'ACCOUNT','APPROVAL_GRANT_RECOVERY','APPROVAL_AUTHORIZATION',$2,'P1','OPEN',$3,$4,$5,$6,$7,$3)
 ON CONFLICT(domain,case_type,resource_type,resource_id) DO UPDATE SET updated_at=manual_review_cases.updated_at RETURNING id`, id, eventID, now, deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution).Scan(&id)
	if err != nil {
		return false, err
	}
	var raw []byte
	var opened struct {
		Case    GrantRecoveryCase `json:"case"`
		Created bool              `json:"created"`
	}
	err = tx.QueryRow(ctx, `SELECT public.open_approval_grant_recovery($1,$2,$3)`, id, eventID, durable).Scan(&raw)
	if err != nil {
		// A concurrent Begin or result may legitimately win after discovery. This
		// is not zero-effect evidence and does not create a partial generic case.
		if errors.Is(classifyGrantRecoveryError(err), ErrConflict) {
			return false, nil
		}
		return false, err
	}
	if err = json.Unmarshal(raw, &opened); err != nil {
		return false, err
	}
	if opened.Created {
		details := map[string]any{"original_event_id": eventID, "durable": durable, "approval_request_id": requestID, "effect_conclusion": "UNKNOWN", "automatic_reexecution": false}
		if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'CREATED',$3,$4)`, uuid.Must(uuid.NewV7()), id, details, now); err != nil {
			return false, err
		}
		if err = events.AppendOutbox(ctx, tx, "account.manual-review.created.v1", id, 1, map[string]any{"case_id": id, "case_type": "APPROVAL_GRANT_RECOVERY", "resource_type": "APPROVAL_AUTHORIZATION", "resource_id": eventID, "severity": "P1"}); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return opened.Created, nil
}
