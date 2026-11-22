package account

import (
	"github.com/google/uuid"
	"net/http"
	"strings"
)

type adminMemberRolesBody struct {
	UnitIDs          []uuid.UUID `json:"unit_ids"`
	UserID           uuid.UUID   `json:"user_id"`
	Roles            []string    `json:"roles"`
	UnitAdminUnitIDs []uuid.UUID `json:"unit_admin_unit_ids"`
	IdempotencyKey   string      `json:"idempotency_key"`
}

func optionalQueryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
