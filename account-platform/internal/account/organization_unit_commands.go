package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func organizationUnitIdempotencyKey(r *http.Request, bodyKey, operation string) string {
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(bodyKey); key != "" {
		return key
	}
	principal, _ := identity.FromContext(r.Context())
	digest := sha256.Sum256([]byte(principal.Issuer + "\x00" + principal.Subject + "\x00" + principal.SessionID + "\x00" + operation))
	return "session:" + hex.EncodeToString(digest[:16])
}

func validOrganizationUnitIdempotencyKey(value string) bool {
	length := len(strings.TrimSpace(value))
	return length >= 8 && length <= 120
}

func organizationUnitRequestHash(commandType string, unitID uuid.UUID, parentID *uuid.UUID, name string, expectedStructureVersion int64) string {
	canonical, _ := json.Marshal(struct {
		Version                  int64      `json:"version"`
		CommandType              string     `json:"command_type"`
		UnitID                   uuid.UUID  `json:"unit_id"`
		ParentID                 *uuid.UUID `json:"parent_id"`
		Name                     string     `json:"name"`
		ExpectedStructureVersion int64      `json:"expected_structure_version"`
	}{Version: 1, CommandType: commandType, UnitID: unitID, ParentID: parentID, Name: strings.TrimSpace(name), ExpectedStructureVersion: expectedStructureVersion})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func lockOrganizationUnitCommand(ctx context.Context, tx pgx.Tx, actorID, organizationID uuid.UUID, commandType, idempotencyKey, requestHash string) (*UnitMutationResult, error) {
	lockKey := fmt.Sprintf("organization-unit:%s:%s:%s:%s", actorID, organizationID, commandType, idempotencyKey)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return nil, err
	}
	var storedHash string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,result FROM organization_unit_commands WHERE actor_user_id=$1 AND organization_id=$2 AND command_type=$3 AND idempotency_key=$4`, actorID, organizationID, commandType, idempotencyKey).Scan(&storedHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedHash != requestHash {
		return nil, ErrConflict
	}
	var result UnitMutationResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	result.Replayed = true
	return &result, nil
}

func saveOrganizationUnitCommand(ctx context.Context, tx pgx.Tx, actorID, organizationID uuid.UUID, commandType, idempotencyKey, requestHash string, result UnitMutationResult, now time.Time) error {
	result.Replayed = false
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO organization_unit_commands(actor_user_id,organization_id,command_type,idempotency_key,request_hash,unit_id,result,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, actorID, organizationID, commandType, idempotencyKey, requestHash, result.Unit.ID, raw, now)
	return err
}

func nextOrganizationStructureVersion(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, now time.Time) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `UPDATE organizations SET structure_version=structure_version+1,updated_at=$2 WHERE id=$1 RETURNING structure_version`, organizationID, now).Scan(&version)
	return version, err
}
