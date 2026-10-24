package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These are real concurrent coordinator requests over public migrations and
// billing_service. Only the Mall transport and its response ordering are
// controlled; the actual H3C adapter maps both Create recovery and Get facts.
func instructionInflightCreateBounds(t *testing.T) {
	for _, mode := range []string{"recovery_later_before_create", "recovery_shorter_before_create", "recovery_missing_before_create", "query_later_before_create", "success_projection_before_create", "failed_success_merge_blocks_archive", "create_before_later_recovery", "paid_recovery_before_create"} {
		t.Run(mode, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, admitted, owner, wallet, product := instructionCreateFixture(t, r, ctx)
			if mode == "failed_success_merge_blocks_archive" {
				admitted.authoritative = true
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			c.now = func() time.Time { return now }
			r.now = c.now
			t1, t2 := now.Add(5*time.Minute), now.Add(time.Hour)
			if mode == "recovery_shorter_before_create" {
				t2 = now.Add(2 * time.Minute)
			}
			var amount int64
			if err := r.pool.QueryRow(ctx, `SELECT amount_fen FROM recharge_products WHERE product_id=$1`, product).Scan(&amount); err != nil {
				t.Fatal(err)
			}
			client := &instructionInflightMall{first: t1, later: &t2, entered: make(chan struct{}), release: make(chan struct{}), secondEntered: make(chan struct{}), secondRelease: make(chan struct{}),
				base: h3cmall.Order{MerchantCustomerKey: owner.String(), SKU: product, Mobile: "+8613800138000", NeedPayAmount: h3cmall.Decimal(fmt.Sprintf("%d.%02d", amount/100, amount%100)), Status: "WAIT_PAY", AliQRData: instructionMallQR}}
			if !h3cmall.ValidQRData(client.base.AliQRData) {
				t.Fatal("fixture QR rejected by the production Mall client validator")
			}
			if mode == "recovery_missing_before_create" {
				client.later = nil
			}
			if mode == "paid_recovery_before_create" || instructionSuccessCutpoint(mode) {
				client.paid = true
			}
			native, err := h3cpayment.New(client, func(context.Context, string) (h3cpayment.Product, error) {
				return h3cpayment.Product{SKU: product, AmountFen: amount}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c.provider = &instructionInflightProvider{instructionCreateProvider: admitted, native: native}
			key := "inflight-bound-" + uuid.NewString()
			create := func() instructionCreateOutcome {
				o, made, e := c.CreateMobileOrder(ctx, owner, wallet, product, "ALIPAY", client.base.Mobile, key)
				return instructionCreateOutcome{o, made, e}
			}
			first := make(chan instructionCreateOutcome, 1)
			go func() { first <- create() }()
			instructionAwait(t, ctx, client.entered, "first Create's actual adapter query")
			// Every failure path releases only this fixture's controlled calls.
			defer func() {
				select {
				case <-client.release:
				default:
					close(client.release)
				}
				select {
				case <-client.secondRelease:
				default:
					close(client.secondRelease)
				}
			}()
			var initial PaymentOrder
			if err = r.pool.QueryRow(ctx, `SELECT id FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2`, owner, wallet).Scan(&initial.ID); err != nil {
				t.Fatal(err)
			}
			initial = instructionStoredOrder(t, r, ctx, initial.ID)
			if !initial.ExpiresAt.Equal(now.Add(c.payableWindow())) {
				t.Fatal("initial estimate not established")
			}
			if mode != "create_before_later_recovery" {
				close(client.secondRelease)
			}
			if mode == "failed_success_merge_blocks_archive" {
				_, e := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION inflight_reject_recharge() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled recharge journal failure' USING ERRCODE='23514'; END $$; CREATE TRIGGER inflight_reject_recharge BEFORE INSERT ON journal_entries FOR EACH ROW EXECUTE FUNCTION inflight_reject_recharge()`)
				if e != nil {
					t.Fatal(e)
				}
			}
			second := make(chan instructionCreateOutcome, 1)
			mergeEntered, mergeRelease := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-mergeRelease:
				default:
					close(mergeRelease)
				}
			}()
			if mode == "query_later_before_create" {
				go func() { o, e := c.SyncOrder(ctx, initial.ID); second <- instructionCreateOutcome{o, false, e} }()
			} else if instructionSuccessCutpoint(mode) {
				// The real Sync commits its projection, then waits only when it
				// acquires a connection for the subsequent financial transaction.
				// The gate checks the old production timestamp, independently of
				// the new evidence, and never replaces a domain/repository method.
				cfg := r.pool.Config()
				var gate atomic.Bool
				cfg.BeforeAcquire = func(callCtx context.Context, conn *pgx.Conn) bool {
					if callCtx.Value(instructionMergeGateKey{}) != true {
						return true
					}
					var committed bool
					if e := conn.QueryRow(callCtx, `SELECT last_successful_query_at IS NOT NULL FROM payment_orders WHERE id=$1`, initial.ID).Scan(&committed); e != nil {
						return false
					}
					if committed && gate.CompareAndSwap(false, true) {
						close(mergeEntered)
						select {
						case <-mergeRelease:
						case <-callCtx.Done():
							return false
						}
					}
					return true
				}
				pool, e := pgxpool.NewWithConfig(ctx, cfg)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(pool.Close)
				r.pool = pool // All later uses are ordered by the provider barriers.
				go func() {
					o, e := c.SyncOrder(context.WithValue(ctx, instructionMergeGateKey{}, true), initial.ID)
					second <- instructionCreateOutcome{o, false, e}
				}()

			} else {
				go func() { second <- create() }()
			}
			instructionAwait(t, ctx, client.secondEntered, "second request's actual adapter Get")
			var prior instructionCreateOutcome
			var originalReceipt []byte
			var before map[string]string
			if mode == "create_before_later_recovery" {
				close(client.release)
				prior = instructionAwaitCreate(t, ctx, first)
				if prior.err != nil || !prior.created {
					t.Fatal("first Create positive control", prior.err)
				}
				originalReceipt = instructionCreateReceipt(t, r, ctx, key)
				close(client.secondRelease)
				later := instructionAwaitCreate(t, ctx, second)
				if later.err != nil || later.created || later.order.ID != initial.ID {
					t.Fatal("late recovery result", later.err)
				}
			} else {
				if instructionSuccessCutpoint(mode) {
					instructionAwait(t, ctx, mergeEntered, "real Sync committed projection before financial transaction")
					prior.order = instructionStoredOrder(t, r, ctx, initial.ID)
				} else {
					prior = instructionAwaitCreate(t, ctx, second)
				}
				if prior.err != nil || prior.created || prior.order.ID != initial.ID {
					t.Fatal("query/recovery result", prior.err)
				}
				observed := instructionStoredOrder(t, r, ctx, initial.ID)
				if client.later != nil && observed.ProviderPayableUntil.Before(t2) {
					t.Fatal("T2 not committed before releasing first Create")
				}
				if mode != "query_later_before_create" && !instructionSuccessCutpoint(mode) {
					originalReceipt = instructionCreateReceipt(t, r, ctx, key)
				}
				before = instructionCommandFactsSnapshot(t, r, ctx)
				close(client.release)
				late := instructionAwaitCreate(t, ctx, first)
				if late.err != nil || late.order.ID != initial.ID || (mode != "query_later_before_create" && !instructionSuccessCutpoint(mode) && late.created) {
					t.Fatal("late first Create result", late.err)
				}
			}
			stored := instructionStoredOrder(t, r, ctx, initial.ID)
			wantUser, wantBound := t1, t1
			if client.later != nil && t2.After(wantBound) {
				wantBound = t2
			}
			if client.paid && !instructionSuccessCutpoint(mode) {
				wantUser = prior.order.ExpiresAt
				wantBound = prior.order.ProviderPayableUntil
			}
			t.Logf("mode=%s initial=%s T1=%s T2=%v stored_user=%s external=%s archive=%s create_calls=%d query_calls=%d", mode, initial.ExpiresAt.Format(time.RFC3339Nano), t1.Format(time.RFC3339Nano), client.later, stored.ExpiresAt.Format(time.RFC3339Nano), stored.ProviderPayableUntil.Format(time.RFC3339Nano), stored.FinalArchiveEligibleAt.Format(time.RFC3339Nano), client.creates.Load(), client.queries.Load())
			if !stored.ExpiresAt.Equal(wantUser) || !stored.ProviderPayableUntil.Equal(wantBound) || !stored.FinalArchiveEligibleAt.Equal(wantBound.Add(latePaymentReviewGrace)) {
				t.Error("late Create lost an observed deadline or preserved an unobserved estimate")
			}
			if len(originalReceipt) > 0 && !bytes.Equal(originalReceipt, instructionCreateReceipt(t, r, ctx, key)) {
				t.Error("first delivered immutable Create receipt changed")
			}
			if before != nil && mode != "query_later_before_create" && !instructionSuccessCutpoint(mode) && !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) {
				t.Error("late Create altered immutable receipts or financial facts")
			}
			replay := create()
			if replay.err != nil || replay.created || replay.order.ID != initial.ID || !replay.order.ExpiresAt.Equal(wantUser) || !replay.order.ProviderPayableUntil.Equal(wantBound) || client.creates.Load() != 1 || client.queries.Load() != 2 {
				t.Error("same key replay changed current projection or repeated external dispatch", replay.err)
			}
			posted, journals := int64(0), 0
			if instructionSuccessCutpoint(mode) {
				instructionLedger(t, r, ctx, wallet, 0, 0)
				rollbackFacts := entitlementFinancialFactsSnapshot(t, ctx, r)
				rollbackReceipts := instructionCommandFactsSnapshot(t, r, ctx)["idempotency_records"]
				close(mergeRelease)
				merged := instructionAwaitCreate(t, ctx, second)
				if mode == "failed_success_merge_blocks_archive" {
					var pgErr *pgconn.PgError
					if !errors.As(merged.err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "controlled recharge journal failure" {
						t.Fatal("real financial transaction did not hit controlled SQL failure", merged.err)
					}
					if _, e := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER inflight_reject_recharge ON journal_entries; DROP FUNCTION inflight_reject_recharge()`); e != nil {
						t.Fatal(e)
					}
					instructionLedger(t, r, ctx, wallet, 0, 0)
					if !reflect.DeepEqual(rollbackFacts, entitlementFinancialFactsSnapshot(t, ctx, r)) || rollbackReceipts != instructionCommandFactsSnapshot(t, r, ctx)["idempotency_records"] {
						t.Fatal("failed financial transaction changed full financial facts/receipts")
					}
					var transactions int
					if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1`, initial.ID).Scan(&transactions); e != nil || transactions != 0 {
						t.Fatal("failed financial transaction leaked payment facts", e)
					}
					now = t2.Add(latePaymentReviewGrace + time.Second)
					instructionUnmergedSuccessArchive(t, r, c, ctx, admitted, initial, t2)
					return
				}
				if merged.err != nil || merged.order.Status != "PAID" || merged.order.FulfillmentStatus != "CREDITED" {
					t.Fatal("real Sync financial merge", merged.err)
				}
				stored = merged.order
				var payload []byte
				if e := r.pool.QueryRow(ctx, `SELECT payload_snapshot FROM payment_evidence WHERE payment_order_id=$1 AND evidence_key LIKE '%|deadline'`, initial.ID).Scan(&payload); e != nil {
					t.Fatal(e)
				}
				if e := json.Unmarshal(payload, &admitted.result); e != nil {
					t.Fatal(e)
				}
				c.provider = admitted
				if _, e := c.SyncOrder(ctx, initial.ID); e != nil {
					t.Fatal("duplicate success query", e)
				}
				var count int
				if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_key LIKE '%|deadline'`, initial.ID).Scan(&count); e != nil || count != 1 {
					t.Fatal("deadline evidence was not deduplicated", e)
				}
			}
			if client.paid {
				posted, journals = stored.CreditMicrocredits, 1
				if stored.Status != "PAID" || stored.FulfillmentStatus != "CREDITED" || stored.QRData != "" || stored.PaymentURL != "" {
					t.Error("late Create restored paid instructions/state")
				}
			}
			instructionLedger(t, r, ctx, wallet, posted, journals)
		})
	}
	t.Run("unverified_evidence_does_not_establish_bounds", instructionUnverifiedBounds)
}

type instructionMergeGateKey struct{}

type instructionCreateOutcome struct {
	order   PaymentOrder
	created bool
	err     error
}

func instructionAwaitCreate(t *testing.T, ctx context.Context, ch <-chan instructionCreateOutcome) instructionCreateOutcome {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		t.Fatal("coordinator did not finish")
		return instructionCreateOutcome{}
	}
}
func instructionAwait(t *testing.T, ctx context.Context, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("did not reach " + label)
	}
}
func instructionCreateReceipt(t *testing.T, r *PostgresRepository, ctx context.Context, key string) []byte {
	t.Helper()
	var body []byte
	if err := r.pool.QueryRow(ctx, `SELECT convert_to(to_jsonb(x)::text,'UTF8') FROM idempotency_records x WHERE scope='payment.create' AND idempotency_key=$1`, key).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

type instructionInflightProvider struct {
	*instructionCreateProvider
	native *h3cpayment.Provider
}

func (p *instructionInflightProvider) Create(ctx context.Context, input payment.Order, callback string) (ProviderOrder, error) {
	return p.native.Create(ctx, input, callback)
}
func (p *instructionInflightProvider) Get(ctx context.Context, key string) (ProviderOrder, error) {
	return p.native.Get(ctx, key)
}

type instructionInflightMall struct {
	base                                           h3cmall.Order
	first                                          time.Time
	later                                          *time.Time
	paid                                           bool
	creates, queries                               atomic.Int32
	entered, release, secondEntered, secondRelease chan struct{}
}

func (m *instructionInflightMall) CreateOrder(context.Context, string, string, string, string) error {
	m.creates.Add(1)
	return errors.New("controlled lost Create acknowledgement")
}
func (m *instructionInflightMall) QueryOrder(ctx context.Context, key string) (h3cmall.Order, error) {
	order := m.base
	order.OrderKey = key
	if m.queries.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return h3cmall.Order{}, ctx.Err()
		}
		order.PayEndTime = m.first.Format(time.RFC3339Nano)
	} else {
		close(m.secondEntered)
		select {
		case <-m.secondRelease:
		case <-ctx.Done():
			return h3cmall.Order{}, ctx.Err()
		}
		if m.later != nil {
			order.PayEndTime = m.later.Format(time.RFC3339Nano)
		}
		if m.paid {
			order.Status = "PAID"
			order.PaymentStatus = "PAY_SUCCESS"
			order.PaidAmount = order.NeedPayAmount
			order.PayUniqueKey = "inflight-real-transaction"
			order.PayTime = m.first.Add(-time.Minute).Format(time.RFC3339Nano)
		}
	}
	return order, nil
}

// Synthetic retained evidence exercises the provenance guard, not a claim that
// the provider can write arbitrary database rows. Runtime writes stay restricted.
func instructionUnverifiedBounds(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, p, owner, wallet, product := instructionCreateFixture(t, r, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	c.now = func() time.Time { return now }
	t1, t2 := now.Add(time.Minute), now.Add(time.Hour)
	p.payableUntil = &t1
	p.onCreate = func() {
		var order PaymentOrder
		if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_orders WHERE wallet_id=$1`, wallet).Scan(&order.ID); err != nil {
			t.Fatal(err)
		}
		order = instructionStoredOrder(t, r, ctx, order.ID)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, field := range []string{"customer", "sku", "merchant", "channel", "amount", "currency", "provider", "version", "review", "legacy", "invalid_time"} {
			observation := p.result
			observation.ProviderPayableUntil = &t2
			header := order
			result := "RECORDED"
			var payload []byte
			switch field {
			case "customer":
				observation.ExternalCustomerKey = uuid.NewString()
			case "sku":
				observation.ExternalSKU = "OTHER"
			case "merchant":
				observation.MerchantOrderNo = "other-order"
			case "channel":
				observation.Channel = "WECHAT"
			case "amount":
				observation.AmountFen++
			case "currency":
				observation.Currency = "USD"
			case "provider":
				header.ProviderID = "other-provider"
			case "version":
				header.ProviderVersion = "other-version"
			case "review":
				result = "REVIEW_REQUIRED"
			case "legacy":
				payload = []byte(`{"payEndTime":"` + t2.Format(time.RFC3339Nano) + `"}`)
			case "invalid_time":
				payload, _ = json.Marshal(observation)
				var fields map[string]any
				json.Unmarshal(payload, &fields)
				fields["provider_payable_until"] = "invalid"
				payload, _ = json.Marshal(fields)
			}
			if _, err = c.insertPaymentEvidence(ctx, tx, header, nil, "QUERY", "untrusted-bound-"+field, result, observation, payload, now); err != nil {
				t.Fatal(field, err)
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", "guard-evidence-"+uuid.NewString())
	if err != nil || !got.ExpiresAt.Equal(t1) || !got.ProviderPayableUntil.Equal(t1) || !got.FinalArchiveEligibleAt.Equal(t1.Add(latePaymentReviewGrace)) {
		t.Fatal("unverified/foreign/legacy evidence replaced the initial estimate", err)
	}
	instructionLedger(t, r, ctx, wallet, 0, 0)
}

func instructionSuccessCutpoint(mode string) bool {
	return mode == "success_projection_before_create" || mode == "failed_success_merge_blocks_archive"
}

func instructionUnmergedSuccessArchive(t *testing.T, r *PostgresRepository, c *PaymentCoordinator, ctx context.Context, provider *instructionCreateProvider, order PaymentOrder, deadline time.Time) {
	t.Helper()
	if _, err := c.markExpiredOrderLocallyClosed(ctx, order.ID); err != nil {
		t.Fatal(err)
	}
	item, err := r.EscalateLatePaymentReview(ctx, order.ID, "PENDING")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err = r.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
		t.Fatal(err)
	}
	// Exercise the generic declared-authority archive contract. The production
	// H3C adapter still declares false; this is not formal H3C archive acceptance.
	native := h3cmall.Order{OrderKey: order.MerchantOrderNo, MerchantCustomerKey: order.RequesterID.String(), SKU: order.ProviderSKU, NeedPayAmount: h3cmall.Decimal(fmt.Sprintf("%d.%02d", order.AmountFen/100, order.AmountFen%100)), Status: "PAY_CLOSED", PayEndTime: deadline.Format(time.RFC3339Nano)}
	terminal, err := h3cpayment.MapMallOrder(native, &h3cpayment.Product{SKU: order.ProviderSKU, AmountFen: order.AmountFen})
	if err != nil {
		t.Fatal(err)
	}
	provider.result = terminal
	c.provider = provider
	if _, err = c.SyncOrder(ctx, order.ID); err != nil {
		t.Fatal(err)
	}
	var otherwiseEligible bool
	if err = r.pool.QueryRow(ctx, `SELECT $2>=final_archive_eligible_at AND last_successful_query_at>=final_archive_eligible_at AND last_authoritative_unpaid_at>=final_archive_eligible_at AND NOT EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1) AND NOT EXISTS(SELECT 1 FROM payment_evidence WHERE payment_order_id=$1 AND merge_result='REVIEW_REQUIRED') FROM payment_orders WHERE id=$1`, order.ID, r.now()).Scan(&otherwiseEligible); err != nil || !otherwiseEligible {
		t.Fatal("archive positive preconditions not reached", err)
	}
	before := instructionCommandFactsSnapshot(t, r, ctx)
	archiveBefore := instructionArchiveSnapshot(t, r, ctx)
	if _, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor); !errors.Is(err, ErrConflict) {
		t.Fatal("unmerged verified SUCCESS permitted unpaid archive", err)
	}
	if stored := instructionStoredOrder(t, r, ctx, order.ID); stored.ArchiveStatus != "ACTIVE" {
		t.Fatal("rejected archive changed order")
	}
	if !reflect.DeepEqual(before, instructionCommandFactsSnapshot(t, r, ctx)) || archiveBefore != instructionArchiveSnapshot(t, r, ctx) {
		t.Fatal("rejected archive wrote financial/receipt/review facts")
	}
}

func instructionArchiveSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_cases x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_case_events x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM late_payment_review_cases x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM late_payment_review_evidence x),(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_evidence x))::text`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}
