package billing

import (
	"encoding/json"

	"github.com/google/uuid"
)

type HoldPricingContext struct {
	ModelID          string          `json:"model_id"`
	ProtocolCode     string          `json:"protocol_code"`
	FormulaVersionID uuid.UUID       `json:"formula_version_id"`
	PricingSnapshot  json.RawMessage `json:"pricing_snapshot"`
	RequestSnapshot  json.RawMessage `json:"request_snapshot"`
}
