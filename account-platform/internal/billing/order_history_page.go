package billing

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
)

type orderHistoryCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type OrderHistoryPage struct {
	RequesterID uuid.UUID                 `json:"requester_id"`
	WalletID    uuid.UUID                 `json:"wallet_id"`
	Items       []PaymentOrderHistoryItem `json:"items"`
	NextCursor  *string                   `json:"next_cursor"`
}

func parseOrderHistoryCursor(raw string) (*orderHistoryCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 256 || strings.IndexFunc(raw, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return nil, ErrConflict
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	parts := strings.Split(string(decoded), "|")
	if err != nil || len(parts) != 2 {
		return nil, ErrConflict
	}
	at, timeErr := time.Parse(time.RFC3339Nano, parts[0])
	id, idErr := uuid.Parse(parts[1])
	if timeErr != nil || at.IsZero() || idErr != nil || id == uuid.Nil || id.String() != parts[1] {
		return nil, ErrConflict
	}
	return &orderHistoryCursor{CreatedAt: at, ID: id}, nil
}

// Every page retains both ownership predicates. A cursor is a position, never
// authority to select another requester or wallet. No payment query is sent.
func (p *PaymentCoordinator) OrderHistoryPage(ctx context.Context, requesterID, walletID uuid.UUID, limit int, rawCursor string) (OrderHistoryPage, error) {
	if limit < 1 || limit > 100 {
		return OrderHistoryPage{}, ErrConflict
	}
	cursor, err := parseOrderHistoryCursor(rawCursor)
	if err != nil {
		return OrderHistoryPage{}, err
	}
	items, err := p.listOrderHistory(ctx, requesterID, walletID, limit+1, cursor)
	if err != nil {
		return OrderHistoryPage{}, err
	}
	page := OrderHistoryPage{RequesterID: requesterID, WalletID: walletID, Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := base64.RawURLEncoding.EncodeToString([]byte(last.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID.String()))
		page.NextCursor = &next
	}
	return page, nil
}
