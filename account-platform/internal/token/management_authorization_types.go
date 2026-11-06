package token

import (
	"github.com/google/uuid"
)

type UnitAuthorizationBinding struct {
	UnitID      uuid.UUID `json:"unit_id"`
	Role        string    `json:"role"`
	Permissions []string  `json:"permissions"`
}
