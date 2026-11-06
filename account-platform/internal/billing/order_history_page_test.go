package billing

import (
	"encoding/base64"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrderHistoryCursorRejectsMalformedPositions(t *testing.T) {
	for _, raw := range []string{" ", "bad=", "bad\n", base64.RawURLEncoding.EncodeToString([]byte("invalid|11111111-1111-4111-8111-111111111111")), base64.RawURLEncoding.EncodeToString([]byte("2026-11-06T00:00:00Z|00000000-0000-0000-0000-000000000000"))} {
		if _, err := parseOrderHistoryCursor(raw); err == nil {
			t.Fatalf("accepted malformed cursor %q", raw)
		}
	}
	raw := base64.RawURLEncoding.EncodeToString([]byte("2026-11-06T00:00:00.123456Z|11111111-1111-4111-8111-111111111111"))
	position, err := parseOrderHistoryCursor(raw)
	if err != nil || position.CreatedAt.Nanosecond() != 123456000 || position.CreatedAt.Location() != time.UTC {
		t.Fatalf("lost exact database timestamp position: %+v, %v", position, err)
	}
}

func TestRetainedOrganizationOrderHistoryPagination(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	coordinator, first, _, _ := retainedOrganizationFinanceOrder(t, r, ctx, 10000, false)
	at := time.Date(2026, 11, 6, 0, 0, 0, 123456000, time.UTC)
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET created_at=$2 WHERE id=$1`, first.ID, at); err != nil {
		t.Fatal(err)
	}
	clone := func(requester, wallet uuid.UUID, createdAt time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := r.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,beneficiary_organization_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version)
		 SELECT $1::uuid,$1::uuid::text,$3,$3,beneficiary_organization_id,$4,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,$5,$5,provider_id,provider_version FROM payment_orders WHERE id=$2`, id, first.ID, requester, wallet, createdAt)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	expected := []string{first.ID.String()}
	for range 4 {
		expected = append(expected, clone(first.RequesterID, first.WalletID, at).String())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(expected)))
	foreign, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	clone(first.RequesterID, foreign.ID, at.Add(time.Hour))
	clone(uuid.New(), first.WalletID, at.Add(time.Hour))
	t.Run("same-timestamp-cursor-and-owner-isolation", func(t *testing.T) {
		cursor := ""
		var got []string
		for range 4 {
			page, err := coordinator.OrderHistoryPage(ctx, first.RequesterID, first.WalletID, 2, cursor)
			if err != nil || len(page.Items) > 2 || page.RequesterID != first.RequesterID || page.WalletID != first.WalletID {
				t.Fatalf("invalid owned page %+v: %v", page, err)
			}
			for _, item := range page.Items {
				got = append(got, item.ID.String())
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = *page.NextCursor
		}
		if len(got) != len(expected) {
			t.Fatalf("lost or foreign orders: got %v want %v", got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("unstable tied order position: got %v want %v", got, expected)
			}
		}
	})
	t.Run("concurrent-new-order-does-not-shift-older-pages", func(t *testing.T) {
		page, err := coordinator.OrderHistoryPage(ctx, first.RequesterID, first.WalletID, 2, "")
		if err != nil || page.NextCursor == nil {
			t.Fatal("first page", err)
		}
		latest := clone(first.RequesterID, first.WalletID, at.Add(time.Second))
		older, err := coordinator.OrderHistoryPage(ctx, first.RequesterID, first.WalletID, 2, *page.NextCursor)
		if err != nil || len(older.Items) != 2 || older.Items[0].ID.String() != expected[2] {
			t.Fatal("new order shifted existing cursor", older, err)
		}
		refreshed, err := coordinator.OrderHistoryPage(ctx, first.RequesterID, first.WalletID, 2, "")
		if err != nil || refreshed.Items[0].ID != latest {
			t.Fatal("refresh did not discover new order", err)
		}
	})
}
