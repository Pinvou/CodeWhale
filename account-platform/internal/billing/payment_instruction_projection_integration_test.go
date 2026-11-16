package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

// Real public migrations, restricted billing_service, production payment merge,
// order projection and ledger. Provider timing/facts are controlled, not a live
// supplier or payment-channel acceptance test.
func TestPaymentInstructionProjectionPostgres(t *testing.T) {
	t.Run("inflight_create_bounds", instructionInflightCreateBounds)
	instructionCreateRecoveryProjection(t)
	t.Run("frozen_create_deadline_survives_delayed_later_query", instructionFrozenCreateDeadline)
	t.Run("external_observation_keeps_archive_upper_bound", instructionExternalArchiveBoundary)
	t.Run("first_closed_deadline_preserves_archive_bound", func(t *testing.T) { instructionFirstTerminalDeadline(t, "CLOSED", true) })
	t.Run("first_failed_deadline_preserves_archive_bound", func(t *testing.T) { instructionFirstTerminalDeadline(t, "FAILED", true) })
	t.Run("non_authoritative_closed_deadline_never_authorizes_archive", func(t *testing.T) { instructionFirstTerminalDeadline(t, "CLOSED", false) })
	t.Run("delayed_pending_after_success", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, initial, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		base := c.provider.(*retainedFinancialProvider)
		success := base.result
		before := instructionStoredOrder(t, r, ctx, initial.ID)
		stale := success
		stale.Status, stale.ChannelTransactionID, stale.PaidAt = "PENDING", nil, nil
		stale.PaymentURL, stale.QRData = "https://provider.invalid/stale", "stale-qr"
		future := before.ExpiresAt.Add(24 * time.Hour)
		stale.ProviderPayableUntil = &future
		blocked := &instructionBlockedProvider{retainedFinancialProvider: base, response: stale, entered: make(chan struct{}, 1), release: make(chan struct{})}
		c.provider = blocked
		result := make(chan instructionSyncResult, 1)
		go func() { order, err := c.SyncOrder(ctx, initial.ID); result <- instructionSyncResult{order, err} }()
		select {
		case <-blocked.entered:
		case <-ctx.Done():
			t.Fatal("query did not reach external provider")
		}
		paid, err := c.applyQueriedPayment(ctx, initial.ID, success, false)
		close(blocked.release)
		if err != nil || paid.Status != "PAID" || paid.FulfillmentStatus != "CREDITED" {
			t.Fatal("production success merge", paid.Status, err)
		}
		var got instructionSyncResult
		select {
		case got = <-result:
		case <-ctx.Done():
			t.Fatal("delayed query did not finish")
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		stored := instructionStoredOrder(t, r, ctx, initial.ID)
		for _, order := range []PaymentOrder{got.order, stored} {
			if order.Status != "PAID" || order.FulfillmentStatus != "CREDITED" || order.CloseStatus != "OPEN" || order.PaymentURL != "" || order.QRData != "" {
				t.Fatalf("stale query restored paid instructions/state: %s/%s/%s url=%t qr=%t", order.Status, order.FulfillmentStatus, order.CloseStatus, order.PaymentURL != "", order.QRData != "")
			}
			if !order.ExpiresAt.Equal(before.ExpiresAt) || !order.ProviderPayableUntil.Equal(before.ProviderPayableUntil) || !order.FinalArchiveEligibleAt.Equal(before.FinalArchiveEligibleAt) {
				t.Fatal("stale query changed confirmed payment deadlines")
			}
		}
		read, err := c.GetOrder(ctx, initial.ID, initial.RequesterID)
		if err != nil || read.Status != "PAID" || read.PaymentURL != "" || read.QRData != "" {
			t.Fatal("GetOrder exposed stale instructions", err)
		}
		recent, err := c.RecentOrder(ctx, initial.RequesterID, initial.WalletID)
		if err != nil || recent == nil || recent.Status != "PAID" || recent.PaymentURL != "" || recent.QRData != "" {
			t.Fatal("RecentOrder exposed stale instructions", err)
		}
		if _, err = c.GetOrder(ctx, initial.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatal("requester isolation changed", err)
		}
		replay, err := c.SyncOrder(ctx, initial.ID)
		if err != nil || replay.Status != "PAID" || replay.PaymentURL != "" || replay.QRData != "" {
			t.Fatal("replayed stale query", err)
		}
		instructionLedger(t, r, ctx, initial.WalletID, initial.CreditMicrocredits, 1)
	})

	for _, tc := range []struct {
		name, status, close string
		offset              time.Duration
		advance             bool
		newDeadline         bool
		archived            bool
		payable             bool
	}{
		{"created_payable", "CREATED", "OPEN", time.Hour, false, false, false, true},
		{"pending_payable", "PENDING", "OPEN", time.Hour, false, false, false, true},
		{"deadline_equal", "PENDING", "OPEN", 0, false, false, false, false},
		{"deadline_passes_during_query", "PENDING", "OPEN", time.Second, true, false, false, false},
		{"later_provider_deadline_does_not_reopen", "PENDING", "OPEN", -time.Second, false, true, false, false},
		{"earlier_provider_deadline_expires", "PENDING", "OPEN", time.Hour, false, true, false, false},
		{"close_requested", "PENDING", "CLOSE_REQUESTED", time.Hour, false, true, false, false},
		{"locally_closed", "PENDING", "LOCALLY_CLOSED_EXTERNAL_PENDING", time.Hour, false, true, false, false},
		{"provider_closed", "CLOSED", "PROVIDER_CLOSED", time.Hour, false, true, false, false},
		{"failed_open", "FAILED", "OPEN", time.Hour, false, true, false, false},
		{"expired_open", "EXPIRED", "OPEN", -time.Second, false, true, false, false},
		{"review_open", "MANUAL_REVIEW", "OPEN", time.Hour, false, true, false, false},
		{"archived_unpaid", "EXPIRED", "LOCALLY_CLOSED_EXTERNAL_PENDING", -time.Second, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			now := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			c.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			deadline := now.Add(tc.offset)
			// Unpaid state fixtures only; no paid balances or transactions are fabricated.
			_, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status=$2,close_status=$3,expires_at=$4,provider_payable_until=$4,payment_url='old-url',qr_data='old-qr',archive_status=CASE WHEN $5 THEN 'ARCHIVED_CONFIRMED_UNPAID' ELSE 'ACTIVE' END,archived_at=CASE WHEN $5 THEN $6::timestamptz END WHERE id=$1`, order.ID, tc.status, tc.close, deadline, tc.archived, now)
			if err != nil {
				t.Fatal(err)
			}
			before := instructionStoredOrder(t, r, ctx, order.ID)
			provider := c.provider.(*retainedFinancialProvider)
			provider.result.Status, provider.result.ChannelTransactionID, provider.result.PaidAt = "PENDING", nil, nil
			provider.result.PaymentURL, provider.result.QRData = "https://provider.invalid/current", "current-qr"
			future := now.Add(2 * time.Hour)
			if tc.name == "earlier_provider_deadline_expires" {
				future = now.Add(-time.Second)
			}
			if tc.newDeadline {
				provider.result.ProviderPayableUntil = &future
			}
			if tc.advance {
				blocked := &instructionBlockedProvider{retainedFinancialProvider: provider, response: provider.result, entered: make(chan struct{}, 1), release: make(chan struct{})}
				c.provider = blocked
				go func() {
					select {
					case <-blocked.entered:
						clock.Store(now.Add(2 * time.Second).UnixNano())
						close(blocked.release)
					case <-ctx.Done():
					}
				}()
			}
			read, err := c.GetOrder(ctx, order.ID, order.RequesterID)
			if err != nil {
				t.Fatal(err)
			}
			readPayable := (tc.status == "CREATED" || tc.status == "PENDING") && tc.close == "OPEN" && deadline.After(now)
			if (read.PaymentURL != "") != readPayable || (read.QRData != "") != readPayable {
				t.Fatal("GetOrder did not enforce current state/deadline")
			}
			recent, err := c.RecentOrder(ctx, order.RequesterID, order.WalletID)
			if err != nil || recent == nil || (recent.PaymentURL != "") != readPayable || (recent.QRData != "") != readPayable {
				t.Fatal("RecentOrder did not enforce current state/deadline", err)
			}
			got, err := c.SyncOrder(ctx, order.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored := instructionStoredOrder(t, r, ctx, order.ID)
			for _, actual := range []PaymentOrder{got, stored} {
				if (actual.PaymentURL != "") != tc.payable || (actual.QRData != "") != tc.payable {
					t.Fatalf("payable=%t but status=%s close=%s url=%t qr=%t", tc.payable, actual.Status, actual.CloseStatus, actual.PaymentURL != "", actual.QRData != "")
				}
				if actual.ArchiveStatus != before.ArchiveStatus || !actual.LateReviewBoundaryAt.Equal(before.LateReviewBoundaryAt) {
					t.Fatal("query changed archive/review facts")
				}
				wantExpires, wantExternal, wantArchive := before.ExpiresAt, before.ProviderPayableUntil, before.FinalArchiveEligibleAt
				if tc.newDeadline && !tc.archived {
					if (tc.status == "CREATED" || tc.status == "PENDING") && tc.close == "OPEN" && future.Before(wantExpires) {
						wantExpires = future
					}
					if future.After(wantExternal) {
						wantExternal = future
					}
					if archive := future.Add(latePaymentReviewGrace); archive.After(wantArchive) {
						wantArchive = archive
					}
				}
				if !actual.ExpiresAt.Equal(wantExpires) || !actual.ProviderPayableUntil.Equal(wantExternal) || !actual.FinalArchiveEligibleAt.Equal(wantArchive) {
					t.Fatal("query changed frozen user deadline or lost external/archive bound")
				}
			}
			if tc.name == "deadline_equal" || tc.advance {
				if got.Status != "EXPIRED" || got.CloseStatus != "LOCALLY_CLOSED_EXTERNAL_PENDING" {
					t.Fatal("local expiration did not converge", got.Status, got.CloseStatus)
				}
			}
			if tc.archived {
				var retained bool
				if err = r.pool.QueryRow(ctx, `SELECT archived_at=$2 FROM payment_orders WHERE id=$1`, order.ID, now).Scan(&retained); err != nil || !retained {
					t.Fatal("archive timestamp changed", err)
				}
			}
			instructionLedger(t, r, ctx, order.WalletID, 0, 0)
		})
	}

	t.Run("review_evidence_clears_storage", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		provider := c.provider.(*retainedFinancialProvider)
		provider.result.Status = "REVIEW_REQUIRED"
		provider.result.PaymentURL, provider.result.QRData = "bad-url", "bad-qr"
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET payment_url='old-url',qr_data='old-qr' WHERE id=$1`, order.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SyncOrder(ctx, order.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("review evidence must retain conflict", err)
		}
		stored := instructionStoredOrder(t, r, ctx, order.ID)
		if stored.Status != "MANUAL_REVIEW" || stored.PaymentURL != "" || stored.QRData != "" {
			t.Fatal("review persisted payment instructions")
		}
		instructionLedger(t, r, ctx, order.WalletID, 0, 0)
	})

	t.Run("success_later_deadline_keeps_frozen_late_classification", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,provider_payable_until=$2 WHERE id=$1`, order.ID, now.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		provider := c.provider.(*retainedFinancialProvider)
		future := now.Add(time.Hour)
		provider.result.ProviderPayableUntil, provider.result.PaidAt = &future, &now
		got, err := c.SyncOrder(ctx, order.ID)
		if err != nil || got.Status != "MANUAL_REVIEW" || got.FulfillmentStatus != "MANUAL_REVIEW" || !got.ExpiresAt.Equal(now.Add(-time.Second)) || !got.ProviderPayableUntil.Equal(future) || got.PaymentURL != "" || got.QRData != "" {
			t.Fatal("later observation reclassified payment across frozen deadline", got.Status, got.FulfillmentStatus, err)
		}
		instructionLedger(t, r, ctx, order.WalletID, 0, 0)
	})

	t.Run("late_success_after_local_expiry", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		provider := c.provider.(*retainedFinancialProvider)
		c.provider = &instructionLateProvider{provider}
		now := time.Now().UTC().Truncate(time.Microsecond)
		deadline := now.Add(-time.Second)
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,provider_payable_until=$2 WHERE id=$1`, order.ID, deadline); err != nil {
			t.Fatal(err)
		}
		if _, err := c.markExpiredOrderLocallyClosed(ctx, order.ID); err != nil {
			t.Fatal(err)
		}
		provider.result.PaidAt = &now
		future := now.Add(time.Hour)
		provider.result.ProviderPayableUntil = &future
		got, err := c.SyncOrder(ctx, order.ID)
		if err != nil || got.Status != "PAID_LATE" || got.FulfillmentStatus != "CREDITED" || !got.ExpiresAt.Equal(deadline) || !got.ProviderPayableUntil.Equal(future) || got.PaymentURL != "" || got.QRData != "" {
			t.Fatal("late success/frozen close deadline changed", got.Status, got.FulfillmentStatus, err)
		}
		if _, err = c.SyncOrder(ctx, order.ID); err != nil {
			t.Fatal(err)
		}
		instructionLedger(t, r, ctx, order.WalletID, order.CreditMicrocredits, 1)
	})

	for _, paid := range []bool{false, true} {
		name := "create_receipt_expired"
		if paid {
			name = "create_receipt_paid"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
			key := "original-create-" + uuid.NewString()
			first, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
			if err != nil || !created || first.PaymentURL == "" || first.QRData == "" {
				t.Fatal("payable Create positive control", err)
			}
			var beforeHash string
			var beforeBody []byte
			if err = r.pool.QueryRow(ctx, `SELECT request_hash,response_body FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key).Scan(&beforeHash, &beforeBody); err != nil {
				t.Fatal(err)
			}
			if paid {
				fact := provider.result
				fact.Status = "SUCCESS"
				transaction := uuid.NewString()
				now := time.Now().UTC()
				fact.ChannelTransactionID, fact.PaidAt = &transaction, &now
				if _, err = c.applyQueriedPayment(ctx, first.ID, fact, false); err != nil {
					t.Fatal(err)
				}
			} else {
				c.now = func() time.Time { return first.ExpiresAt }
			}
			replay, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
			if err != nil || created || replay.ID != first.ID || replay.MerchantOrderNo != first.MerchantOrderNo || replay.RequesterID != owner || replay.PaymentURL != "" || replay.QRData != "" {
				t.Fatal("receipt returned stale instructions/changed identity", err)
			}
			if paid && (replay.Status != "PAID" || replay.FulfillmentStatus != "CREDITED") {
				t.Fatal("receipt replay lost latest paid facts")
			}
			var afterHash string
			var afterBody []byte
			if err = r.pool.QueryRow(ctx, `SELECT request_hash,response_body FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key).Scan(&afterHash, &afterBody); err != nil || beforeHash != afterHash || !bytes.Equal(beforeBody, afterBody) {
				t.Fatal("original immutable receipt changed", err)
			}
			if provider.creates != 1 {
				t.Fatal("receipt replay dispatched a new provider command")
			}
			if _, _, err = c.CreateOrder(ctx, uuid.New(), wallet, product, "ALIPAY", key); !errors.Is(err, ErrConflict) {
				t.Fatal("receipt key accepted other owner", err)
			}
			expected, journals := int64(0), 0
			if paid {
				expected, journals = first.CreditMicrocredits, 1
			}
			instructionLedger(t, r, ctx, wallet, expected, journals)
		})
	}

	for _, name := range []string{"create_deadline_before", "create_deadline_equal", "create_deadline_after", "create_instruction_fallback", "create_deadline_precedence", "create_deadline_passes_during_call"} {
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
			now := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			c.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			deadline := now.Add(time.Minute)
			payable := true
			switch name {
			case "create_deadline_before":
				deadline = now.Add(-time.Second)
				payable = false
			case "create_deadline_equal":
				deadline = now
				payable = false
			case "create_deadline_passes_during_call":
				provider.onCreate = func() { clock.Store(deadline.UnixNano()) }
				payable = false
			}
			if name == "create_instruction_fallback" {
				provider.instructionExpires = &deadline
			} else {
				provider.payableUntil = &deadline
			}
			if name == "create_deadline_precedence" {
				instruction := now.Add(-time.Second)
				provider.instructionExpires = &instruction
			}
			key := "first-deadline-" + uuid.NewString()
			got, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
			if err != nil || !created {
				t.Fatal("first Create", err)
			}
			stored := instructionStoredOrder(t, r, ctx, got.ID)
			for _, actual := range []PaymentOrder{got, stored} {
				if !actual.ExpiresAt.Equal(deadline) || !actual.ProviderPayableUntil.Equal(deadline) || !actual.FinalArchiveEligibleAt.Equal(deadline.Add(7*24*time.Hour)) {
					t.Fatal("first Create ignored authoritative deadline")
				}
				if (actual.PaymentURL != "") != payable || (actual.QRData != "") != payable {
					t.Fatal("first Create persisted/returned unavailable instructions")
				}
			}
			var before []byte
			if err = r.pool.QueryRow(ctx, `SELECT response_body FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key).Scan(&before); err != nil {
				t.Fatal(err)
			}
			clock.Store(deadline.Add(time.Second).UnixNano())
			replay, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
			if err != nil || created || replay.ID != got.ID || !replay.ExpiresAt.Equal(deadline) || replay.PaymentURL != "" || replay.QRData != "" || provider.creates != 1 {
				t.Fatal("deadline receipt replay extended/reopened order", err)
			}
			var after []byte
			if err = r.pool.QueryRow(ctx, `SELECT response_body FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key).Scan(&after); err != nil || !bytes.Equal(before, after) {
				t.Fatal("deadline receipt bytes changed", err)
			}
			instructionLedger(t, r, ctx, wallet, 0, 0)
		})
	}
}

// The production H3C adapter can query after an ambiguous Create. Its actual
// mapper may return a closed order with old QR data and a later payEndTime.
// Only its Mall client is controlled here; Billing's admitted catalog identity
// is the existing test fixture, not a claim of formal-channel acceptance.
type instructionMallClient struct {
	order            h3cmall.Order
	queryUnavailable bool
}

const instructionMallQR = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/eLkAAAAASUVORK5CYII="

func (m *instructionMallClient) CreateOrder(context.Context, string, string, string, string) error {
	return errors.New("controlled lost create acknowledgement")
}

func (m *instructionMallClient) QueryOrder(context.Context, string) (h3cmall.Order, error) {
	if m.queryUnavailable {
		return h3cmall.Order{}, errors.New("controlled query unavailable")
	}
	return m.order, nil
}

type instructionMallCreateProvider struct {
	*instructionCreateProvider
	native           *h3cpayment.Provider
	client           *instructionMallClient
	recover          bool
	status           string
	observedDeadline time.Time
}

func (p *instructionMallCreateProvider) Create(ctx context.Context, input payment.Order, callback string) (ProviderOrder, error) {
	p.creates++
	p.client = &instructionMallClient{queryUnavailable: p.recover, order: h3cmall.Order{
		OrderKey: input.MerchantOrderNo, MerchantCustomerKey: input.MerchantCustomerKey,
		Mobile: input.Mobile, SKU: input.ExternalSKU,
		NeedPayAmount: h3cmall.Decimal(fmt.Sprintf("%d.%02d", input.AmountFen/100, input.AmountFen%100)),
		Status:        p.status, AliQRData: instructionMallQR,
		PayEndTime: p.observedDeadline.Format(time.RFC3339Nano),
	}}
	var err error
	p.native, err = h3cpayment.New(p.client, func(context.Context, string) (h3cpayment.Product, error) {
		return h3cpayment.Product{SKU: input.ExternalSKU, AmountFen: input.AmountFen}, nil
	})
	if err != nil {
		return ProviderOrder{}, err
	}
	result, err := p.native.Create(ctx, input, callback)
	p.result = result
	return result, err
}

func (p *instructionMallCreateProvider) Get(ctx context.Context, key string) (ProviderOrder, error) {
	result, err := p.native.Get(ctx, key)
	p.result = result
	return result, err
}

func instructionCreateRecoveryProjection(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name, status string
		recover      bool
	}{
		{"h3c_first_create_closed", "PAY_CLOSED", false},
		{"h3c_recovered_create_closed", "PAY_CLOSED", true},
		{"h3c_recovered_create_pending", "WAIT_PAY", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !h3cmall.ValidQRData(instructionMallQR) {
				t.Fatal("Mall fixture must satisfy the production client's PNG data URI validator")
			}
			r, ctx := retainedFinanceRepository(t)
			c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
			clock := time.Now().UTC().Truncate(time.Microsecond)
			c.now = func() time.Time { return clock }
			r.now = c.now
			external := clock.Add(time.Hour)
			wrapped := &instructionMallCreateProvider{instructionCreateProvider: provider,
				recover: tc.recover, status: tc.status, observedDeadline: external}
			c.provider = wrapped
			if wrapped.Capabilities().AuthoritativeUnpaidTerminal {
				t.Fatal("fixture must preserve H3C's non-authoritative terminal boundary")
			}
			key := "h3c-create-projection-" + uuid.NewString()
			create := func() (PaymentOrder, bool, error) {
				return c.CreateMobileOrder(ctx, owner, wallet, product, "ALIPAY", "+8613800138000", key)
			}
			got, created, err := create()
			if tc.recover {
				if err == nil {
					t.Fatal("lost acknowledgement plus unavailable query must remain ambiguous")
				}
				wrapped.client.queryUnavailable = false
				got, created, err = create()
			}
			if err != nil || created == tc.recover {
				t.Fatal("real Create/recovery identity", created, err)
			}
			before := instructionCommandFactsSnapshot(t, r, ctx)
			stored := instructionStoredOrder(t, r, ctx, got.ID)
			read, err := c.GetOrder(ctx, got.ID, owner)
			if err != nil {
				t.Fatal(err)
			}
			recent, err := c.RecentOrder(ctx, owner, wallet)
			if err != nil || recent == nil {
				t.Fatal("recent order", err)
			}
			replay, again, err := create()
			if err != nil || again || provider.creates != 1 {
				t.Fatal("same key must never dispatch Create twice", provider.creates, err)
			}
			for _, order := range []PaymentOrder{got, stored, read, *recent, replay} {
				if order.ID != got.ID || order.Status != "PENDING" || order.CloseStatus != "OPEN" {
					t.Fatal("observation gained terminal authority or changed identity")
				}
				if !order.ExpiresAt.Equal(clock.Add(c.payableWindow())) || !order.ProviderPayableUntil.Equal(external) || !order.FinalArchiveEligibleAt.Equal(external.Add(latePaymentReviewGrace)) {
					t.Error("recovered observation lost external bounds or extended the frozen user window")
				}
				if tc.status == "PAY_CLOSED" && (order.QRData != "" || order.PaymentURL != "") {
					t.Error("closed Create/recovery exposed unavailable payment instructions")
				}
				if tc.status == "WAIT_PAY" && order.QRData != instructionMallQR {
					t.Error("valid pending instructions were discarded")
				}
			}
			if !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) {
				t.Fatal("projection or replay changed immutable receipts or financial facts")
			}
			instructionLedger(t, r, ctx, wallet, 0, 0)
		})
	}
}

// The only waiting clock in this case is the real server wall clock. The order,
// receipt and initial precise deadline come from the actual Create path.
func instructionFrozenCreateDeadline(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
	provider.maximumLateSeconds = 24 * 60 * 60
	t1 := time.Now().UTC().Add(2 * time.Second).Truncate(time.Microsecond)
	provider.payableUntil = &t1
	key := "frozen-deadline-" + uuid.NewString()
	first, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
	if err != nil || !created || !first.ExpiresAt.Equal(t1) || first.PaymentURL == "" || first.QRData == "" || !time.Now().Before(t1) {
		t.Fatal("precise first Create did not establish a payable frozen T1", err)
	}
	before := instructionCommandFactsSnapshot(t, r, ctx)
	immutableOrder := instructionImmutableOrderFacts(t, r, ctx, first.ID)
	t2 := t1.Add(time.Hour)
	response := provider.result
	response.ProviderPayableUntil = &t2
	capabilities := provider.Capabilities()
	blocked := &instructionBlockedProvider{retainedFinancialProvider: provider.retainedFinancialProvider, response: response, entered: make(chan struct{}, 1), release: make(chan struct{}), capabilities: &capabilities}
	c.provider = blocked
	result := make(chan instructionSyncResult, 1)
	go func() { order, err := c.SyncOrder(ctx, first.ID); result <- instructionSyncResult{order, err} }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("pending query never entered provider")
	}
	timer := time.NewTimer(time.Until(t1.Add(20 * time.Millisecond)))
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		close(blocked.release)
		t.Fatal("deadline wait cancelled")
	}
	close(blocked.release)
	var resultValue instructionSyncResult
	select {
	case resultValue = <-result:
	case <-ctx.Done():
		t.Fatal("delayed query never returned")
	}
	if resultValue.err != nil {
		t.Fatal(resultValue.err)
	}
	stored := instructionStoredOrder(t, r, ctx, first.ID)
	afterQueryFacts := instructionCommandFactsSnapshot(t, r, ctx)
	afterQueryOrder := instructionImmutableOrderFacts(t, r, ctx, first.ID)
	if !reflect.DeepEqual(before, afterQueryFacts) || immutableOrder != afterQueryOrder {
		for table, prior := range before {
			if prior != afterQueryFacts[table] {
				t.Logf("changed command/financial table %s: before=%s after=%s", table, prior, afterQueryFacts[table])
			}
		}
		if immutableOrder != afterQueryOrder {
			t.Logf("changed order columns: before=%s after=%s", immutableOrder, afterQueryOrder)
		}
		t.Fatal("pending query changed immutable command/owner/financial facts")
	}
	t.Logf("frozen query facts: T1=%s T2=%s wall=%s returned=%s stored=%s status=%s close=%s url=%t qr=%t creates=%d full_financial_and_receipts_unchanged=true", t1.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), resultValue.order.ExpiresAt.Format(time.RFC3339Nano), stored.ExpiresAt.Format(time.RFC3339Nano), stored.Status, stored.CloseStatus, stored.PaymentURL != "", stored.QRData != "", provider.creates)
	for _, actual := range []PaymentOrder{resultValue.order, stored} {
		if !actual.ExpiresAt.Equal(t1) || actual.Status != "EXPIRED" || actual.CloseStatus != "LOCALLY_CLOSED_EXTERNAL_PENDING" || actual.PaymentURL != "" || actual.QRData != "" {
			t.Fatal("frozen Create deadline extended/reopened after delayed query")
		}
		if !actual.ProviderPayableUntil.Equal(t2) || !actual.FinalArchiveEligibleAt.Equal(t2.Add(24*time.Hour+latePaymentReviewGrace)) {
			t.Fatal("later external observation/archive safety bound lost")
		}
	}
	var evidenceRetained bool
	if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND (payload_snapshot->>'provider_payable_until')::timestamptz=$2)`, first.ID, t2).Scan(&evidenceRetained); err != nil || !evidenceRetained {
		t.Fatal("exact later channel deadline absent from immutable evidence", err)
	}
	// A subsequently returned shorter old observation stays exact in evidence,
	// but cannot reduce the already known external/archive safety bound.
	c.provider = provider
	provider.result = response
	earlier := t1.Add(-time.Minute)
	provider.result.ProviderPayableUntil = &earlier
	shorter, err := c.SyncOrder(ctx, first.ID)
	if err != nil || !shorter.ExpiresAt.Equal(t1) || !shorter.ProviderPayableUntil.Equal(t2) || !shorter.FinalArchiveEligibleAt.Equal(t2.Add(24*time.Hour+latePaymentReviewGrace)) || shorter.Status != "EXPIRED" {
		t.Fatal("old shorter query erased known external bound/reopened closed order", err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND (payload_snapshot->>'provider_payable_until')::timestamptz=$2)`, first.ID, earlier).Scan(&evidenceRetained); err != nil || !evidenceRetained {
		t.Fatal("exact shorter observation was discarded", err)
	}
	read, err := c.GetOrder(ctx, first.ID, owner)
	if err != nil || read.PaymentURL != "" || read.QRData != "" || !read.ExpiresAt.Equal(t1) {
		t.Fatal("GetOrder reopened frozen deadline", err)
	}
	replay, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", key)
	if err != nil || created || replay.ID != first.ID || !replay.ExpiresAt.Equal(t1) || replay.PaymentURL != "" || replay.QRData != "" || provider.creates != 1 || !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) {
		t.Fatal("original key/receipt reopened deadline or dispatched again", err)
	}
	// The local freeze does not discard a real payment after local closure.
	c.provider = provider
	provider.result = response
	provider.result.Status = "SUCCESS"
	transaction := uuid.NewString()
	paidAt := time.Now().UTC()
	provider.result.ChannelTransactionID, provider.result.PaidAt = &transaction, &paidAt
	paid, err := c.SyncOrder(ctx, first.ID)
	if err != nil || paid.Status != "PAID_LATE" || paid.FulfillmentStatus != "CREDITED" || !paid.ExpiresAt.Equal(t1) || !paid.ProviderPayableUntil.Equal(t2) || paid.CloseStatus != "LOCALLY_CLOSED_EXTERNAL_PENDING" || paid.PaymentURL != "" || paid.QRData != "" {
		t.Fatal("closed order lost/rewrote late success", paid.Status, paid.FulfillmentStatus, err)
	}
	paidFacts := instructionCommandFactsSnapshot(t, r, ctx)
	if _, err = c.SyncOrder(ctx, first.ID); err != nil || !reflect.DeepEqual(paidFacts, instructionCommandFactsSnapshot(t, r, ctx)) {
		t.Fatal("successful replay changed financial facts", err)
	}
	if before["idempotency_records"] != paidFacts["idempotency_records"] || provider.creates != 1 {
		t.Fatal("late success rewrote original Create receipts/identity")
	}
	instructionLedger(t, r, ctx, wallet, first.CreditMicrocredits, 1)
}

func instructionExternalArchiveBoundary(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
	provider.maximumLateSeconds = 24 * 60 * 60
	provider.authoritative = true
	clock := time.Now().UTC().Truncate(time.Microsecond)
	c.now = func() time.Time { return clock }
	r.now = c.now
	t1 := clock.Add(time.Minute)
	t2 := t1.Add(time.Hour)
	provider.payableUntil = &t1
	first, _, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", "archive-bound-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	originalReceipts := instructionCommandFactsSnapshot(t, r, ctx)["idempotency_records"]
	// Equal and missing query deadlines preserve the original first freeze.
	for _, hasDeadline := range []bool{true, false} {
		provider.result.ProviderPayableUntil = nil
		if hasDeadline {
			provider.result.ProviderPayableUntil = &t1
		}
		unchanged, queryErr := c.SyncOrder(ctx, first.ID)
		if queryErr != nil || !unchanged.ExpiresAt.Equal(t1) || !unchanged.ProviderPayableUntil.Equal(t1) || !unchanged.FinalArchiveEligibleAt.Equal(first.FinalArchiveEligibleAt) || unchanged.PaymentURL == "" || unchanged.QRData == "" {
			t.Fatal("equal/missing query deadline changed first freeze", queryErr)
		}
	}
	provider.result.ProviderPayableUntil = &t2
	if _, err = c.SyncOrder(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	clock = t1.Add(time.Second)
	if _, err = c.SyncOrder(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	item, err := r.EscalateLatePaymentReview(ctx, first.ID, "PENDING")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err = r.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
		t.Fatal(err)
	}
	// A controlled scheduler clock passes the original archive bound, then a
	// real authoritative unpaid query meets every other existing archive guard.
	oldArchive := t1.Add(24*time.Hour + latePaymentReviewGrace)
	newArchive := t2.Add(24*time.Hour + latePaymentReviewGrace)
	clock = oldArchive.Add(time.Second)
	provider.result.Status = "CLOSED"
	provider.result.PaymentURL, provider.result.QRData = "", ""
	if _, err = c.SyncOrder(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	stored := instructionStoredOrder(t, r, ctx, first.ID)
	if !stored.ExpiresAt.Equal(t1) || !stored.ProviderPayableUntil.Equal(t2) || !stored.FinalArchiveEligibleAt.Equal(newArchive) {
		t.Fatal("closed/reviewed order lost distinct deadline bounds")
	}
	if _, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
		t.Fatal("later external deadline allowed premature archive", err)
	}
	clock = newArchive.Add(time.Second)
	if _, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
		t.Fatal("new archive boundary accepted old query evidence", err)
	}
	if _, err = c.SyncOrder(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString())
	if err != nil || archived.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" {
		t.Fatal("fresh authoritative terminal query did not allow lawful archive", err)
	}
	if instructionCommandFactsSnapshot(t, r, ctx)["idempotency_records"] != originalReceipts {
		t.Fatal("archive changed original Create receipt")
	}
	instructionLedger(t, r, ctx, wallet, 0, 0)
}

// A later exact external deadline may first appear in the terminal response.
// It must protect the archive bound without reopening the local payment window
// or granting authority that the provider did not declare. The clock below is
// a controlled scheduler clock, not a claim of eight days of wall-clock waiting.
func instructionFirstTerminalDeadline(t *testing.T, status string, authoritative bool) {
	r, ctx := retainedFinanceRepository(t)
	c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
	provider.maximumLateSeconds = 24 * 60 * 60
	provider.authoritative = authoritative
	clock := time.Now().UTC().Truncate(time.Microsecond)
	c.now = func() time.Time { return clock }
	r.now = c.now
	t1 := clock.Add(time.Minute)
	t2 := t1.Add(time.Hour)
	provider.payableUntil = &t1
	first, created, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", "first-terminal-deadline-"+uuid.NewString())
	if err != nil || !created || !first.ExpiresAt.Equal(t1) {
		t.Fatal("actual first Create deadline", err)
	}
	clock = t1.Add(time.Second)
	expired, err := c.SyncOrder(ctx, first.ID)
	if err != nil || expired.Status != "EXPIRED" || expired.CloseStatus != "LOCALLY_CLOSED_EXTERNAL_PENDING" || !expired.ExpiresAt.Equal(t1) || expired.PaymentURL != "" || expired.QRData != "" {
		t.Fatal("original pending did not converge at frozen deadline", err)
	}
	item, err := r.EscalateLatePaymentReview(ctx, first.ID, "PENDING")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err = r.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
		t.Fatal(err)
	}
	before := instructionCommandFactsSnapshot(t, r, ctx)
	immutableOrder := instructionImmutableOrderFacts(t, r, ctx, first.ID)
	oldArchive := t1.Add(24*time.Hour + latePaymentReviewGrace)
	newArchive := t2.Add(24*time.Hour + latePaymentReviewGrace)
	clock = oldArchive.Add(time.Second)
	// No earlier PENDING observation carried T2. Its first appearance is here.
	provider.result.Status = status
	provider.result.ProviderStatus = status
	if status == "CLOSED" {
		provider.result.ProviderStatus = "PAY_CLOSED"
	}
	provider.result.ProviderPayableUntil = &t2
	provider.result.PaymentURL, provider.result.QRData = "", ""
	observed, err := c.SyncOrder(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) || immutableOrder != instructionImmutableOrderFacts(t, r, ctx, first.ID) {
		t.Fatal("terminal observation changed immutable owner/receipt/financial facts")
	}
	if !observed.ExpiresAt.Equal(t1) || observed.PaymentURL != "" || observed.QRData != "" || observed.Status != "MANUAL_REVIEW" || observed.FulfillmentStatus != "MANUAL_REVIEW" || observed.CloseStatus != "LOCALLY_CLOSED_EXTERNAL_PENDING" {
		t.Fatal("terminal deadline restored payment window or overwrote review/close facts")
	}
	var exactEvidence, hasAuthority bool
	if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND payload_snapshot->>'status'=$3 AND (payload_snapshot->>'provider_payable_until')::timestamptz=$2),(SELECT last_authoritative_unpaid_at IS NOT NULL FROM payment_orders WHERE id=$1)`, first.ID, t2, status).Scan(&exactEvidence, &hasAuthority); err != nil || !exactEvidence || hasAuthority != authoritative {
		t.Fatal("exact terminal deadline evidence/authority contract", err)
	}
	archived, archiveErr := r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString())
	t.Logf("first terminal deadline facts: status=%s declared_authority=%t T1=%s T2=%s now=%s external=%s archive_bound=%s required_bound=%s exact_terminal_T2_evidence=%t full_financial_and_receipts_unchanged=true archive_status=%s archive_error=%v", status, authoritative, t1.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano), clock.Format(time.RFC3339Nano), observed.ProviderPayableUntil.Format(time.RFC3339Nano), observed.FinalArchiveEligibleAt.Format(time.RFC3339Nano), newArchive.Format(time.RFC3339Nano), exactEvidence, archived.ReviewStatus, archiveErr)
	if !errors.Is(archiveErr, ErrConflict) {
		t.Fatal("first terminal T2 allowed premature archive", archiveErr)
	}
	if !observed.ProviderPayableUntil.Equal(t2) || !observed.FinalArchiveEligibleAt.Equal(newArchive) {
		t.Fatal("first terminal observation lost later external/archive bound")
	}
	clock = newArchive.Add(time.Second)
	if _, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString()); !errors.Is(err, ErrConflict) {
		t.Fatal("new boundary accepted pre-boundary query", err)
	}
	if _, err = c.SyncOrder(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if authoritative && status == "CLOSED" {
		foreign := provider.result
		foreign.Status, foreign.ExternalCustomerKey = "SUCCESS", uuid.NewString()
		foreignTransaction := uuid.NewString()
		foreign.ChannelTransactionID, foreign.PaidAt = &foreignTransaction, &clock
		tx, beginErr := r.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback(ctx)
		if _, err = c.insertPaymentEvidence(ctx, tx, first, nil, "QUERY", "foreign-success|deadline", "RECORDED", foreign, nil, clock); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	archived, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor, "archive-"+uuid.NewString())
	if authoritative {
		if err != nil || archived.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" {
			t.Fatal("fresh declared-authoritative terminal query could not archive lawfully", err)
		}
	} else if !errors.Is(err, ErrConflict) {
		t.Fatal("non-authoritative terminal observation gained archive permission", err)
	}
	if !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) || provider.creates != 1 {
		t.Fatal("terminal/archive flow changed original receipt or financial effects")
	}
	instructionLedger(t, r, ctx, wallet, 0, 0)
}

func instructionCommandFactsSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) map[string]string {
	t.Helper()
	snapshot := entitlementFinancialFactsSnapshot(t, ctx, r)
	delete(snapshot, "payment_orders") // Query/expiry legitimately change its projection; immutable columns are checked separately.
	// Every real provider call appends its existing health audit event. Keep all
	// financial/effect outbox events in the invariant while allowing that audit.
	var financialOutbox string
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM outbox_events x WHERE event_type<>'billing.payment-provider.health-observed.v1'`).Scan(&financialOutbox); err != nil {
		t.Fatal(err)
	}
	snapshot["outbox_events"] = financialOutbox
	var receipts string
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM idempotency_records x`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	snapshot["idempotency_records"] = receipts
	return snapshot
}

func instructionImmutableOrderFacts(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(ctx, `SELECT (to_jsonb(o)-ARRAY['status','close_status','payment_url','qr_data','expires_at','provider_payable_until','final_archive_eligible_at','last_successful_query_at','last_authoritative_unpaid_at','provider_status','provider_trace_id','updated_at','close_requested_at','locally_closed_at','attention_summary','summary_version'])::text FROM payment_orders o WHERE id=$1`, id).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}

type instructionSyncResult struct {
	order PaymentOrder
	err   error
}

type instructionLateProvider struct{ *retainedFinancialProvider }

func (p *instructionLateProvider) Capabilities() PaymentCapabilities {
	capabilities := p.retainedFinancialProvider.Capabilities()
	capabilities.MaximumLateConfirmationSeconds = 24 * 60 * 60
	return capabilities
}

type instructionBlockedProvider struct {
	*retainedFinancialProvider
	response     ProviderOrder
	entered      chan struct{}
	release      chan struct{}
	capabilities *PaymentCapabilities
}

func (p *instructionBlockedProvider) Capabilities() PaymentCapabilities {
	if p.capabilities != nil {
		return *p.capabilities
	}
	return p.retainedFinancialProvider.Capabilities()
}

func (p *instructionBlockedProvider) Get(ctx context.Context, _ string) (ProviderOrder, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return p.response, nil
	case <-ctx.Done():
		return ProviderOrder{}, ctx.Err()
	}
}
func instructionStoredOrder(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) PaymentOrder {
	t.Helper()
	order, err := getPaymentOrderByID(ctx, r.pool, id, false)
	if err != nil {
		t.Fatal(err)
	}
	return order
}
func instructionLedger(t *testing.T, r *PostgresRepository, ctx context.Context, wallet uuid.UUID, posted int64, journals int) {
	t.Helper()
	balance, err := r.GetWallet(ctx, wallet)
	if err != nil || balance.PostedMicrocredits != posted || balance.ReservedMicrocredits != 0 {
		t.Fatal("wallet conservation", err)
	}
	var count int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&count); err != nil || count != journals {
		t.Fatal("recharge conservation", err)
	}
}

type instructionCreateProvider struct {
	*retainedFinancialProvider
	creates            int
	payableUntil       *time.Time
	instructionExpires *time.Time
	onCreate           func()
	maximumLateSeconds int64
	authoritative      bool
}

func (p *instructionCreateProvider) Capabilities() PaymentCapabilities {
	capabilities := p.retainedFinancialProvider.Capabilities()
	capabilities.MaximumLateConfirmationSeconds = p.maximumLateSeconds
	capabilities.AuthoritativeUnpaidTerminal = p.authoritative
	return capabilities
}

func (p *instructionCreateProvider) Create(_ context.Context, input payment.Order, _ string) (ProviderOrder, error) {
	p.creates++
	p.result = ProviderOrder{MerchantOrderNo: input.MerchantOrderNo, ExternalCustomerKey: input.MerchantCustomerKey, ExternalSKU: input.ExternalSKU, Channel: input.Channel, AmountFen: input.AmountFen, Currency: input.Currency, Status: "PENDING", PaymentURL: "https://provider.invalid/current", QRData: "current-qr", ProviderPayableUntil: p.payableUntil, PaymentInstructionExpiresAt: p.instructionExpires}
	if p.onCreate != nil {
		p.onCreate()
	}
	return p.result, nil
}
func instructionCreateFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (*PaymentCoordinator, *instructionCreateProvider, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	input := validRechargeProductForEditTests()
	input.ProductID = "INSTRUCTION_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ProviderID = "controlled"
	input.Mapping.ExternalSKUBOM = input.ProductID
	item, err := r.CreateRechargeProduct(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled", "controlled-admission", "instruction-probe", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence); err != nil {
		t.Fatal(err)
	}
	provider := &instructionCreateProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
	c, err := newBillingFixturePaymentCoordinator(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.AllowTentativePrices(true)
	return c, provider, owner, wallet.ID, input.ProductID
}
