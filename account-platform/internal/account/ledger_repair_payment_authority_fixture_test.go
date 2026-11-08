package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This fixture uses the public Billing query and HTTP paths; its provider and
// human administrator authorization are controlled test actors.
type issue2016E2EProvider struct {
	fact payment.ProviderOrder
	rows []payment.BillEntry
}

func (p *issue2016E2EProvider) Capabilities() payment.Capabilities {
	return payment.Capabilities{ProviderID: "controlled", Version: "v1", Channels: []string{"ALIPAY"}, OrderQuerySupported: true, MinimumQueryIntervalSeconds: 1, BillSupported: true, ManualRepairSupported: true, TestOnly: true}
}
func (p *issue2016E2EProvider) Create(context.Context, payment.Order, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (p *issue2016E2EProvider) Get(_ context.Context, merchant string) (payment.ProviderOrder, error) {
	if merchant != p.fact.MerchantOrderNo {
		return payment.ProviderOrder{}, errors.New("unexpected merchant")
	}
	return p.fact, nil
}
func (p *issue2016E2EProvider) Close(context.Context, string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{}, payment.ErrUnsupported
}
func (p *issue2016E2EProvider) Refund(context.Context, string, string, int64) (payment.Refund, error) {
	return payment.Refund{}, payment.ErrUnsupported
}
func (p *issue2016E2EProvider) Bill(_ context.Context, channel string, _ time.Time) ([]payment.BillEntry, error) {
	if channel != p.fact.Channel {
		return nil, errors.New("unexpected channel")
	}
	return append([]payment.BillEntry(nil), p.rows...), nil
}

type issue2016E2EFixture struct {
	WalletID, JournalID, PostingID, WrongAccountID, CorrectAccountID uuid.UUID
	Amount, OriginalDelta                                            int64
	Coordinator                                                      *billingdomain.PaymentCoordinator
	Order                                                            billingdomain.PaymentOrder
	Date                                                             time.Time
}

func issue2016BuildApprovalFixture(ctx context.Context, r *billingdomain.PostgresRepository, pool *pgxpool.Pool) (issue2016E2EFixture, error) {
	var f issue2016E2EFixture
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		return f, err
	}
	now := time.Now().UTC()
	order := billingdomain.PaymentOrder{ID: uuid.New(), MerchantOrderNo: "approval-fixture-" + uuid.NewString(), RequesterID: owner, WalletID: wallet.ID, ProductID: "CNY_1", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CreditMicrocredits: 1000000, Status: "PENDING", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(time.Hour), ProviderID: "controlled", ProviderVersion: "v1"}
	snapshot, err := json.Marshal(billingdomain.Product{ProductID: order.ProductID, PlanName: "controlled legacy credit", AmountFen: 100, CreditMicrocredits: 1000000, BasePoints: 1, ValidityDays: 30, Currency: "CNY"})
	if err != nil {
		return f, err
	}
	_, err = pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','PENDING',$11,$11,$11::timestamptz+interval '72 hours',$11::timestamptz+interval '8 days',$12,$12,$13,$14)`, order.ID, order.MerchantOrderNo, owner, wallet.ID, order.ProductID, snapshot, order.Channel, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ExpiresAt, now, order.ProviderID, order.ProviderVersion)
	if err != nil {
		return f, err
	}
	providerID := uuid.NewString()
	fact := payment.ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &providerID, PaidAt: &now, ExternalCustomerKey: owner.String()}
	row := payment.BillEntry{EntryType: "PAYMENT", ProviderReferenceID: providerID, ChannelTransactionID: providerID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	c, err := billingdomain.NewPaymentCoordinatorWithProvider(r, &issue2016E2EProvider{fact: fact, rows: []payment.BillEntry{row}}, "test")
	if err != nil {
		return f, err
	}
	order, err = c.SyncOrder(ctx, order.ID)
	if err != nil {
		return f, err
	}
	if order.Status != "PAID" || order.FulfillmentStatus != "CREDITED" {
		return f, fmt.Errorf("public payment query did not complete: %s/%s", order.Status, order.FulfillmentStatus)
	}
	var transaction uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&transaction); err != nil {
		return f, err
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code='platform:adjustment' AND wallet_id IS NULL`).Scan(&f.WrongAccountID); err != nil {
		return f, err
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE account_type='PAYMENT_CLEARING' AND wallet_id IS NULL`).Scan(&f.CorrectAccountID); err != nil {
		return f, err
	}
	if err = pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "payment-transaction:"+transaction.String(), f.CorrectAccountID).Scan(&f.JournalID, &f.PostingID, &f.OriginalDelta); err != nil {
		return f, err
	}
	f.Amount = f.OriginalDelta
	if f.Amount < 0 {
		f.Amount = -f.Amount
	}
	f.WalletID = wallet.ID
	f.Coordinator = c
	f.Order = order
	local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	f.Date = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return f, nil
}

func issue2016CaseCommand(ctx context.Context, c *billingdomain.PaymentCoordinator, r *billingdomain.PostgresRepository, id, actor uuid.UUID, action, key string) (billingdomain.AdminReconciliationCase, error) {
	var item billingdomain.AdminReconciliationCase
	// Human administrator permissions are controlled separately from the real
	// Account-to-Billing signed workload identity exercised by the test.
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var request struct{ Permission, Subject string }
		if json.NewDecoder(q.Body).Decode(&request) != nil || request.Subject != actor.String() || request.Permission != "platform.billing.reconciliation.manage" {
			http.Error(w, "unexpected permission", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-authority")
	if err != nil {
		return item, err
	}
	handler := billingdomain.NewService(r).WithPayments(c).WithAuthorizer(authorizer).Handler("controlled-authority", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled-reconciliation", Subject: actor.String()}, nil
	})
	body, err := json.Marshal(map[string]any{"action": action, "reason": "controlled financial reconciliation command", "review_result": "controlled evidence"})
	if err != nil {
		return item, err
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-cases/"+id.String()+"/commands", strings.NewReader(string(body))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer controlled-human")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return item, fmt.Errorf("actual Billing command HTTP status %d", response.Code)
	}
	if err = json.Unmarshal(response.Body.Bytes(), &item); err != nil {
		return item, err
	}
	return item, nil
}
func issue2016ClaimApprovalCase(ctx context.Context, c *billingdomain.PaymentCoordinator, r *billingdomain.PostgresRepository, id, actor uuid.UUID) error {
	_, err := issue2016CaseCommand(ctx, c, r, id, actor, "CLAIM", "claim-"+uuid.NewString())
	return err
}
func issue2016ResolveApprovalCaseWithKey(ctx context.Context, c *billingdomain.PaymentCoordinator, r *billingdomain.PostgresRepository, id, actor uuid.UUID, key string) (billingdomain.AdminReconciliationCase, error) {
	return issue2016CaseCommand(ctx, c, r, id, actor, "RESOLVE", key)
}
