package billing

// A frozen precondition on the installed release route creates no new human
// authority or database identity. Original callers use
// Release; only the new narrow deadline client supplies this exact frozen tuple.
import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type OriginalHoldExpectation struct {
	Version   int             `json:"version"`
	WalletID  uuid.UUID       `json:"wallet_id"`
	OwnerID   uuid.UUID       `json:"owner_id"`
	PriceID   uuid.UUID       `json:"price_version_id"`
	Maximum   int64           `json:"maximum_microcredits"`
	ModelID   string          `json:"model_id"`
	Protocol  string          `json:"protocol_code"`
	FormulaID uuid.UUID       `json:"formula_version_id"`
	Price     json.RawMessage `json:"pricing_snapshot"`
	Request   json.RawMessage `json:"request_snapshot"`
}

func (expected OriginalHoldExpectation) valid() bool {
	return expected.Version == 1 && expected.WalletID != uuid.Nil && expected.OwnerID != uuid.Nil && expected.PriceID != uuid.Nil &&
		expected.Maximum >= 0 && expected.ModelID != "" && expected.Protocol == "CHAT" && expected.FormulaID != uuid.Nil &&
		json.Valid(expected.Price) && string(expected.Price) != "null" && json.Valid(expected.Request) && string(expected.Request) != "null"
}

func (r *PostgresRepository) ReleaseOriginalHold(ctx context.Context, usageID, holdID uuid.UUID, reason string, expected OriginalHoldExpectation) (Hold, error) {
	if !expected.valid() || usageID == uuid.Nil || holdID == uuid.Nil {
		return Hold{}, ErrConflict
	}
	return r.releaseHoldExpected(ctx, usageID, holdID, reason, false, &expected)
}

type originalHoldReleaseRepository interface {
	ReleaseOriginalHold(context.Context, uuid.UUID, uuid.UUID, string, OriginalHoldExpectation) (Hold, error)
}
