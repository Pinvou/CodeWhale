package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type MemberLifecycleConsumer struct {
	repository *PostgresRepository
}

func NewMemberLifecycleConsumer(repository *PostgresRepository) *MemberLifecycleConsumer {
	return &MemberLifecycleConsumer{repository: repository}
}

func (consumer *MemberLifecycleConsumer) Handle(ctx context.Context, event events.Event) error {
	if consumer == nil || consumer.repository == nil || event.Type != "account.membership.changed.v1" || event.Validate() != nil {
		return events.ErrInvalidEvent
	}
	var payload struct {
		OrganizationID    uuid.UUID `json:"organization_id"`
		UserID            uuid.UUID `json:"user_id"`
		MembershipStatus  string    `json:"membership_status"`
		MembershipVersion int64     `json:"membership_version"`
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF || payload.OrganizationID == uuid.Nil || payload.UserID == uuid.Nil || payload.MembershipVersion != event.AggregateVersion || (payload.MembershipStatus != "ACTIVE" && payload.MembershipStatus != "SUSPENDED" && payload.MembershipStatus != "REVOKED") {
		return events.ErrInvalidEvent
	}
	// Membership events carry complete state snapshots. This receiver can begin
	// at a later version after stream retention, unlike incremental consumers
	// using events.ProcessInbox's strict contiguous-version rule.
	tx, err := consumer.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockMemberQuotaWallet(ctx, tx, payload.OrganizationID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-member:"+payload.OrganizationID.String()+":"+payload.UserID.String()); err != nil {
		return err
	}
	var currentVersion int64
	var currentStatus string
	var aggregateID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT status,membership_version,membership_aggregate_id FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, payload.OrganizationID, payload.UserID).Scan(&currentStatus, &currentVersion, &aggregateID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if aggregateID != nil && *aggregateID != event.AggregateID {
		return events.ErrInvalidEvent
	}
	if currentVersion == payload.MembershipVersion && currentStatus != payload.MembershipStatus {
		return ErrConflict
	}
	var boundElsewhere bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_member_lifecycle_states WHERE membership_aggregate_id=$1 AND (organization_id<>$2 OR user_id<>$3))`, event.AggregateID, payload.OrganizationID, payload.UserID).Scan(&boundElsewhere); err != nil {
		return err
	}
	if boundElsewhere {
		return events.ErrInvalidEvent
	}
	command, err := tx.Exec(ctx, `INSERT INTO inbox_messages(event_id,event_type,aggregate_id,aggregate_version,received_at) VALUES($1,$2,$3,$4,now()) ON CONFLICT(event_id) DO NOTHING`, event.ID, event.Type, event.AggregateID, event.AggregateVersion)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if _, err := consumer.repository.applyOrganizationMemberLifecycle(ctx, tx, payload.OrganizationID, payload.UserID, payload.MembershipStatus, payload.MembershipVersion, true); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_member_lifecycle_states SET membership_aggregate_id=$3 WHERE organization_id=$1 AND user_id=$2`, payload.OrganizationID, payload.UserID, event.AggregateID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inbox_aggregate_versions(event_type,aggregate_id,last_version,updated_at) VALUES($1,$2,$3,now()) ON CONFLICT(event_type,aggregate_id) DO UPDATE SET last_version=GREATEST(inbox_aggregate_versions.last_version,EXCLUDED.last_version),updated_at=EXCLUDED.updated_at`, event.Type, event.AggregateID, event.AggregateVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE inbox_messages SET processed_at=now(),last_error=NULL WHERE event_id=$1`, event.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
