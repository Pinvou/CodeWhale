package events

import (
	"context"
	"encoding/json"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func AppendOutbox(ctx context.Context, tx pgx.Tx, eventType string, aggregateID uuid.UUID, aggregateVersion int64, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	traceID := httpserver.TraceID(ctx)
	if traceID == "" {
		traceID = httpserver.RequestID(ctx)
	}
	if traceID == "" {
		traceID = "internal"
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES ($1,$2,$3,$4,$5,$6,now())`, uuid.Must(uuid.NewV7()), eventType, aggregateID, aggregateVersion, body, traceID)
	return err
}
