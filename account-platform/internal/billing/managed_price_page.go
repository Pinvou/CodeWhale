package billing

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type ManagedPricePage struct {
	Items      []ManagedPriceVersion
	HasMore    bool
	NextCursor string
}

// The cursor is a bounded seek position, not a durable snapshot or authority.
// Binding page size prevents accidentally changing page boundaries mid-walk.
type managedPriceCursor struct {
	Version int       `json:"v"`
	Created time.Time `json:"created"`
	ID      uuid.UUID `json:"id"`
	Limit   int       `json:"limit"`
}

func (c managedPriceCursor) timeParameter() any {
	if c.Created.IsZero() {
		return nil
	}
	return c.Created
}
func encodeManagedPriceCursor(created time.Time, id uuid.UUID, limit int) string {
	data, _ := json.Marshal(managedPriceCursor{1, created.UTC(), id, limit})
	return base64.RawURLEncoding.EncodeToString(data)
}
func decodeManagedPriceCursor(raw string, limit int) (managedPriceCursor, error) {
	var cursor managedPriceCursor
	if raw == "" {
		return cursor, nil
	}
	if len(raw) > 512 {
		return cursor, ErrConflict
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Created.IsZero() || cursor.Created.Year() < 1 || cursor.Created.Year() > 9999 || cursor.Created.Nanosecond()%1000 != 0 || cursor.ID == uuid.Nil || cursor.Limit != limit || encodeManagedPriceCursor(cursor.Created, cursor.ID, limit) != raw {
		return managedPriceCursor{}, ErrConflict
	}
	return cursor, nil
}
