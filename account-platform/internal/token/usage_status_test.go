package token

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUsageDisplayStatusKeepsSettlementPendingDistinct(t *testing.T) {
	cases := map[string]string{
		"CREATED": "IN_PROGRESS", "RESERVED": "IN_PROGRESS", "UPSTREAM_ACTIVE": "IN_PROGRESS",
		"COMMITTED": "SETTLED", "SETTLEMENT_PENDING": "SETTLEMENT_PENDING", "RELEASED": "RELEASED", "REVIEW": "MANUAL_REVIEW",
	}
	for status, expected := range cases {
		if actual := usageDisplayStatus(UsageRequest{Status: status}); actual != expected {
			t.Fatalf("usageDisplayStatus(%q) = %q, want %q", status, actual, expected)
		}
	}
}

func TestUsageDisplayStatusTrustsTerminalSettlementAfterUserStopped(t *testing.T) {
	cases := []struct {
		name  string
		usage UsageRequest
		want  string
	}{
		{name: "settled", usage: UsageRequest{Status: "COMMITTED", SettlementStatus: "SETTLED", DeliveryStatus: "USER_STOPPED"}, want: "SETTLED"},
		{name: "released", usage: UsageRequest{Status: "RELEASED", SettlementStatus: "RELEASED", DeliveryStatus: "USER_STOPPED"}, want: "RELEASED"},
		{name: "cancelled before dispatch", usage: UsageRequest{Status: "RELEASED", SettlementStatus: "RELEASED", CancelStatus: "LOCAL_CONFIRMED", DeliveryStatus: "USER_STOPPED"}, want: "CANCELLED_BEFORE_DISPATCH"},
		{name: "review", usage: UsageRequest{Status: "SETTLEMENT_PENDING", ExecutionStatus: "REVIEW", SettlementStatus: "REVIEW", DeliveryStatus: "USER_STOPPED"}, want: "MANUAL_REVIEW"},
		{name: "still executing", usage: UsageRequest{Status: "UPSTREAM_ACTIVE", SettlementStatus: "HOLD_ACTIVE", DeliveryStatus: "USER_STOPPED"}, want: "DISPLAY_STOPPED_EXECUTION_CONTINUES"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := usageDisplayStatus(testCase.usage); got != testCase.want {
				t.Fatalf("usageDisplayStatus() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPaginateUsageHistoryRecordsKeepsLogicalTurnAtBoundary(t *testing.T) {
	base := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	records := make([]UserUsageTurnRecord, 0, 22)
	for index := 0; index < 19; index++ {
		records = append(records, UserUsageTurnRecord{
			UsageRequestID: uuid.MustParse("00000000-0000-0000-0000-" + fmt.Sprintf("%012d", index+1)),
			LogicalTurnID:  fmt.Sprintf("turn-%02d", index+1),
			StartedAt:      base.Add(-time.Duration(index) * time.Minute),
		})
	}
	boundaryHead := UserUsageTurnRecord{UsageRequestID: uuid.MustParse("00000000-0000-0000-0000-000000000020"), LogicalTurnID: "turn-20", StartedAt: base.Add(-19 * time.Minute)}
	boundaryTail := UserUsageTurnRecord{UsageRequestID: uuid.MustParse("00000000-0000-0000-0000-000000000021"), LogicalTurnID: "turn-20", StartedAt: base.Add(-20 * time.Minute)}
	extra := UserUsageTurnRecord{UsageRequestID: uuid.MustParse("00000000-0000-0000-0000-000000000022"), LogicalTurnID: "turn-21", StartedAt: base.Add(-21 * time.Minute)}
	records = append(records, boundaryHead, boundaryTail, extra)

	page, cursor := paginateUsageHistoryRecords(records, 20)
	if len(page) != 21 {
		t.Fatalf("page contains %d requests, want all 21 requests from 20 logical turns", len(page))
	}
	if page[len(page)-1].UsageRequestID != boundaryTail.UsageRequestID {
		t.Fatalf("boundary turn was split: last request = %s", page[len(page)-1].UsageRequestID)
	}
	if cursor == nil || cursor.ID != boundaryHead.UsageRequestID || !cursor.StartedAt.Equal(boundaryHead.StartedAt) {
		t.Fatalf("cursor = %#v, want boundary turn head %#v", cursor, boundaryHead)
	}
}

func TestPaginateUsageHistoryRecordsTreatsLegacyRequestsIndependently(t *testing.T) {
	base := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	collidingID := uuid.New()
	first := UserUsageTurnRecord{UsageRequestID: uuid.New(), LogicalTurnID: collidingID.String(), StartedAt: base}
	second := UserUsageTurnRecord{UsageRequestID: collidingID, StartedAt: base.Add(-time.Minute)}
	page, cursor := paginateUsageHistoryRecords([]UserUsageTurnRecord{first, second}, 1)
	if len(page) != 1 || page[0].UsageRequestID != first.UsageRequestID {
		t.Fatalf("legacy page = %#v, want first request only", page)
	}
	if cursor == nil || cursor.ID != first.UsageRequestID {
		t.Fatalf("legacy cursor = %#v, want first request", cursor)
	}
}

func TestPaginateUsageHistoryRecordsUsesProjectedGroupHeadForCursor(t *testing.T) {
	base := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	headID := uuid.New()
	records := []UserUsageTurnRecord{
		{
			UsageRequestID:  headID,
			LogicalTurnID:   "turn-projected",
			StartedAt:       base.Add(-time.Minute),
			cursorStartedAt: base,
		},
		{UsageRequestID: uuid.New(), LogicalTurnID: "turn-next", StartedAt: base.Add(-2 * time.Minute)},
	}

	page, cursor := paginateUsageHistoryRecords(records, 1)
	if len(page) != 1 || cursor == nil || cursor.ID != headID || !cursor.StartedAt.Equal(base) {
		t.Fatalf("projected page/cursor = %#v / %#v, want cursor on durable group head", page, cursor)
	}
}

func TestUsageHistoryCursorRoundTripAndRejectsInvalidInput(t *testing.T) {
	want := UsageHistoryCursor{
		StartedAt: time.Date(2026, time.August, 18, 9, 30, 0, 123, time.UTC),
		ID:        uuid.MustParse("0198f8e3-2d27-7a2b-93aa-4f1a8f796e8c"),
	}
	got, err := decodeUsageCursor(encodeUsageCursor(want))
	if err != nil || !got.StartedAt.Equal(want.StartedAt) || got.ID != want.ID {
		t.Fatalf("cursor round trip = %#v, %v", got, err)
	}
	for _, invalid := range []string{"not-base64!", "YWJj", string(make([]byte, 257))} {
		if _, err := decodeUsageCursor(invalid); err == nil {
			t.Fatalf("accepted invalid cursor %q", invalid)
		}
	}
}

func TestUsageHistorySchemasStayExplicitlyVersioned(t *testing.T) {
	for _, forbidden := range []string{"model_id", "catalog_version", "internal_status"} {
		if strings.Contains(usageTurnProjectionQuery, forbidden) {
			t.Fatalf("logical-turns-v1 SQL projects internal %s", forbidden)
		}
	}
	legacy, err := json.Marshal(UserUsageRecord{UsageRequestID: uuid.New(), LogicalTurnID: "legacy-compatible-turn", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"usage_request_count", "charged_microcredits", "cancel_usage_request_id", "model_id", "catalog_version"} {
		if strings.Contains(string(legacy), `"`+forbidden+`"`) {
			t.Fatalf("legacy per-request schema unexpectedly contains %s: %s", forbidden, legacy)
		}
	}
	turn, err := json.Marshal(UserUsageTurnRecord{UsageRequestID: uuid.New(), LogicalTurnID: "logical-turn-version-one", UsageRequestCount: 2, Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"usage_request_count", "charged_microcredits"} {
		if !strings.Contains(string(turn), `"`+required+`"`) {
			t.Fatalf("logical-turns-v1 schema is missing %s: %s", required, turn)
		}
	}
	for _, forbidden := range []string{"model_id", "catalog_version"} {
		if strings.Contains(string(turn), `"`+forbidden+`"`) {
			t.Fatalf("logical-turns-v1 schema leaks %s: %s", forbidden, turn)
		}
	}
}

func TestUsageStatusRequiresSameSessionAndActiveContext(t *testing.T) {
	userID := uuid.New()
	organizationID := uuid.New()
	usage := UsageRequest{UserID: userID, SessionID: "session", OrganizationID: &organizationID}
	context := AuthorizationContext{UserID: userID, SessionID: "session", ContextType: "ORGANIZATION", OrganizationID: &organizationID}
	if !usageContextMatches(context, usage) {
		t.Fatal("same session and organization should be authorized")
	}
	otherUser := context
	otherUser.UserID = uuid.New()
	if usageContextMatches(otherUser, usage) {
		t.Fatal("usage leaked across users")
	}
	personal := context
	personal.ContextType, personal.OrganizationID = "PERSONAL", nil
	if usageContextMatches(personal, usage) {
		t.Fatal("organization usage leaked into personal context")
	}
	otherOrganization := context
	otherOrganizationID := uuid.New()
	otherOrganization.OrganizationID = &otherOrganizationID
	if usageContextMatches(otherOrganization, usage) {
		t.Fatal("usage leaked across organizations")
	}
	otherSession := context
	otherSession.SessionID = "other"
	if usageContextMatches(otherSession, usage) {
		t.Fatal("usage leaked across sessions")
	}
}
