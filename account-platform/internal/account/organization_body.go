package account

import (
	"github.com/google/uuid"
	"time"
)

type roleBindingGrantBody struct {
	UserID         uuid.UUID  `json:"user_id"`
	RoleCode       string     `json:"role_code"`
	OrganizationID *uuid.UUID `json:"organization_id"`
	UnitID         *uuid.UUID `json:"unit_id"`
	StartsAt       *time.Time `json:"starts_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	IdempotencyKey string     `json:"idempotency_key"`
	Reason         string     `json:"reason"`
}
