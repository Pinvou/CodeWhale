package account

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type MemberListQuery struct {
	Limit  int
	Cursor string
}

type MemberPage struct {
	Items      []Member `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

type QuotaMemberPage struct {
	Items      []QuotaMember `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// A cursor is only a position, never authority. Each page still checks the
// current actor and scope, and applies the same SQL visibility predicates.
type memberListCursor struct {
	Resource       string     `json:"resource"`
	OrganizationID uuid.UUID  `json:"organization_id"`
	UnitID         *uuid.UUID `json:"unit_id"`
	Name           string     `json:"name"`
	UserID         uuid.UUID  `json:"user_id"`
}

func memberListPosition(query MemberListQuery, resource string, organizationID uuid.UUID, unitID *uuid.UUID) (int, *memberListCursor, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if query.Cursor == "" {
		return limit, nil, nil
	}
	if len(query.Cursor) > 8192 {
		return 0, nil, ErrConflict
	}
	raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	if err != nil {
		return 0, nil, ErrConflict
	}
	var cursor memberListCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.UserID == uuid.Nil || cursor.OrganizationID != organizationID || cursor.Resource != resource || !uuidPointerEqual(cursor.UnitID, unitID) {
		return 0, nil, ErrConflict
	}
	return limit, &cursor, nil
}

func encodeMemberListCursor(resource string, organizationID uuid.UUID, unitID *uuid.UUID, name string, userID uuid.UUID) string {
	raw, _ := json.Marshal(memberListCursor{resource, organizationID, unitID, name, userID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func memberListRequest(r *http.Request, resource string, organizationID uuid.UUID, unitID *uuid.UUID) (MemberListQuery, error) {
	query := MemberListQuery{Limit: adminLimit(r), Cursor: strings.TrimSpace(r.URL.Query().Get("cursor"))}
	_, _, err := memberListPosition(query, resource, organizationID, unitID)
	return query, err
}

func memberListSortName(displayName, email *string, userID uuid.UUID) string {
	if displayName != nil {
		return *displayName
	}
	if email != nil {
		return *email
	}
	return userID.String()
}
