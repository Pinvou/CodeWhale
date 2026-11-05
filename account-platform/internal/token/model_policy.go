package token

import (
	"context"
	"encoding/json"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OrganizationModelPolicy struct {
	OrganizationID       uuid.UUID   `json:"organization_id"`
	Version              int64       `json:"version"`
	Mode                 string      `json:"mode"`
	ModelIDs             []uuid.UUID `json:"model_ids"`
	AuthorizationVersion int64       `json:"authorization_version"`
	UpdatedBy            *uuid.UUID  `json:"updated_by,omitempty"`
	UpdatedAt            *time.Time  `json:"updated_at,omitempty"`
}

func getOrganizationPolicyVersionTx(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, version int64) (OrganizationModelPolicy, error) {
	var item OrganizationModelPolicy
	var snapshot []byte
	var actor uuid.UUID
	var createdAt time.Time
	err := tx.QueryRow(ctx, `SELECT actor_id,authorization_version,snapshot,created_at FROM organization_model_policy_audit WHERE organization_id=$1 AND policy_version=$2`, organizationID, version).Scan(&actor, &item.AuthorizationVersion, &snapshot, &createdAt)
	if err != nil {
		return OrganizationModelPolicy{}, err
	}
	var value struct {
		Mode     string      `json:"mode"`
		ModelIDs []uuid.UUID `json:"model_ids"`
	}
	if err := json.Unmarshal(snapshot, &value); err != nil {
		return OrganizationModelPolicy{}, err
	}
	item.OrganizationID, item.Version, item.Mode, item.ModelIDs, item.UpdatedBy, item.UpdatedAt = organizationID, version, value.Mode, value.ModelIDs, &actor, &createdAt
	return item, nil
}
