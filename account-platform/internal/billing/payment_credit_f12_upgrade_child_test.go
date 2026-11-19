package billing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type f12RealAccountAdmission struct{ repository *account.PostgresRepository }

func (a f12RealAccountAdmission) Admit(ctx context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
	return a.repository.AdmitPaymentCredit(ctx, r)
}

type f12RealBillingResolver struct{ repository *PostgresRepository }

func (r f12RealBillingResolver) Resolve(ctx context.Context, q paymentcredit.Request) (paymentcredit.Decision, error) {
	return r.repository.ResolvePaymentCredit(ctx, q)
}
func (r f12RealBillingResolver) Acknowledge(ctx context.Context, q paymentcredit.Request, state string) (paymentcredit.Decision, error) {
	return r.repository.AcknowledgePaymentCredit(ctx, q, state)
}

type f12UpgradeRecord struct {
	OldInput, NewInput                               PaymentFinancialRepairExecutionInput
	OldReceipt                                       PaymentFinancialRepairResult
	OldOrder, OldWallet, NewOrder, NewWallet, UserID uuid.UUID
	OldFacts, PendingFacts                           string
	AccountUser                                      json.RawMessage
}

// Parent exclusively owns both UUID databases and their real migration/runtime
// actors. This bridge never creates roles, patches schema or admits a fake user.
func TestF12PaymentCreditUpgradeChildPostgres(t *testing.T) {
	phase, recordPath := os.Getenv("PINVOU_F12_UPGRADE_PHASE"), os.Getenv("PINVOU_F12_UPGRADE_RECORD")
	if phase == "" {
		t.Skip("owned F12 parent bridge required")
	}
	if phase != "historical" && phase != "current" || !filepath.IsAbs(recordPath) {
		t.Fatal("invalid owned F12 phase/record")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	r, err := NewPostgresRepository(ctx, os.Getenv("PINVOU_F12_UPGRADE_BILLING_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	provider := &paymentRepairStubProvider{}
	c, err := NewPaymentCoordinatorWithProvider(r, provider)
	if err != nil {
		t.Fatal(err)
	}
	if phase == "historical" {
		var activation, policy, schema int64
		var state string
		if err := r.pool.QueryRow(ctx, `SELECT state,policy_version,snapshot_schema_version,version FROM billing_approval_command_activations WHERE command_type='PAYMENT_FINANCIAL_REPAIR'`).Scan(&state, &policy, &schema, &activation); err != nil || state != "ACTIVE" || policy != 1 || schema != 1 || activation < 1 {
			t.Fatal("parent-owned F12 activation is not the exact active contract")
		}

		inputFor := func(order, evidence uuid.UUID) PaymentFinancialRepairExecutionInput {
			command := PaymentFinancialRepairCommandInput{PaymentOrderID: order, PaymentEvidenceID: evidence}
			if _, err := r.PaymentFinancialRepairProofInput(ctx, command); err != nil {
				t.Fatal("public original F12 proof input", err)
			}
			plan, err := paymentFinancialRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
			if err != nil {
				t.Fatal(err)
			}
			return PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: uuid.New(), RequesterUserID: uuid.New(), ExecutionKey: "real-upgrade:" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: command, Frozen: PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}}
		}
		var data f12UpgradeRecord
		now := time.Now().UTC()
		var evidence uuid.UUID
		data.OldOrder, data.OldWallet, evidence = seedF12Repair(t, ctx, r, now, 100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
		data.OldInput = inputFor(data.OldOrder, evidence)
		data.OldReceipt, err = c.applyHistoricalF12PaymentRepair(ctx, data.OldInput, 900038)
		if err != nil {
			t.Fatal(err)
		}
		data.NewOrder, data.NewWallet, evidence = seedF12Repair(t, ctx, r, now, 100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
		data.NewInput = inputFor(data.NewOrder, evidence)
		if err := r.pool.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1`, data.NewOrder).Scan(&data.UserID); err != nil {
			t.Fatal(err)
		}
		ac, err := pgx.Connect(ctx, os.Getenv("PINVOU_F12_UPGRADE_ACCOUNT_URL"))
		if err != nil {
			t.Fatal(err)
		}
		defer ac.Close(context.Background())
		// A legal historical fixture user: this is synthetic data, not a claim that
		// modern registration/Keycloak ran before the observed Account14 upgrade.
		if _, err := ac.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$3 FROM capability`, data.UserID, "f12_"+strings.ReplaceAll(data.UserID.String(), "-", "")[:16], now); err != nil {
			t.Fatal("historical Account runtime user seed", err)
		}
		if err := ac.QueryRow(ctx, `SELECT to_jsonb(record) FROM users record WHERE id=$1`, data.UserID).Scan(&data.AccountUser); err != nil {
			t.Fatal("capture original Account user", err)
		}
		data.OldFacts = captureF12PaymentFacts(t, ctx, r, data.OldOrder, data.OldWallet)()
		data.PendingFacts = captureF12PaymentFacts(t, ctx, r, data.NewOrder, data.NewWallet)()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(recordPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal("exclusive synthetic carrier", err)
		}
		_, writeErr := file.Write(raw)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("synthetic carrier write failed")
		}
		return
	}
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var data f12UpgradeRecord
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	assertF12OriginalProjection(t, data.OldFacts, captureF12PaymentFacts(t, ctx, r, data.OldOrder, data.OldWallet)())
	assertF12OriginalProjection(t, data.PendingFacts, captureF12PaymentFacts(t, ctx, r, data.NewOrder, data.NewWallet)())
	ar, err := account.NewPostgresRepository(ctx, os.Getenv("PINVOU_F12_UPGRADE_ACCOUNT_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	userConnection, err := pgx.Connect(ctx, os.Getenv("PINVOU_F12_UPGRADE_ACCOUNT_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var currentUser json.RawMessage
	queryErr := userConnection.QueryRow(ctx, `SELECT to_jsonb(record) FROM users record WHERE id=$1`, data.UserID).Scan(&currentUser)
	closeErr := userConnection.Close(context.Background())
	if queryErr != nil || closeErr != nil {
		t.Fatal("read preserved Account user")
	}
	assertF12OriginalProjection(t, "users:["+string(data.AccountUser)+"]", "users:["+string(currentUser)+"]")
	c.WithPaymentCreditAdmission(f12RealAccountAdmission{ar})
	old, err := c.ApplyPaymentFinancialRepair(ctx, data.OldInput)
	if err != nil || old != data.OldReceipt {
		t.Fatal("real current F12 lost original historical receipt", err)
	}
	if _, err := r.PaymentFinancialRepairProofInput(ctx, data.NewInput.Command); err != nil {
		t.Fatal("public current F12 proof input", err)
	}
	result, err := c.ApplyPaymentFinancialRepair(ctx, data.NewInput)
	if err != nil {
		t.Fatal("real Account admission/current F12 repair", err)
	}
	currentFacts := captureF12PaymentFacts(t, ctx, r, data.NewOrder, data.NewWallet)
	after := currentFacts()
	replay, err := c.ApplyPaymentFinancialRepair(ctx, data.NewInput)
	if err != nil || replay != result || currentFacts() != after {
		t.Fatal("real current F12 repeat changed money or receipt", err)
	}
	if err := ar.ReconcilePaymentCredits(ctx, f12RealBillingResolver{r}); err != nil {
		t.Fatal("real cross-domain resolution/ACK", err)
	}
	var state string
	var acknowledged bool
	var recordCount int
	if err := r.pool.QueryRow(ctx, `SELECT state,account_acknowledged_at IS NOT NULL,count(*) OVER() FROM payment_credit_attempts WHERE payment_order_id=$1`, data.NewOrder).Scan(&state, &acknowledged, &recordCount); err != nil || state != "APPLIED" || !acknowledged || recordCount != 1 {
		t.Fatal("Billing actual credit/ACK missing", err)
	}
	ac, err := pgx.Connect(ctx, os.Getenv("PINVOU_F12_UPGRADE_ACCOUNT_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close(context.Background())
	if err := ac.QueryRow(ctx, `SELECT state,billing_acknowledged_at IS NOT NULL,count(*) OVER() FROM account_payment_credit_admissions WHERE payment_order_id=$1 AND user_id=$2`, data.NewOrder, data.UserID).Scan(&state, &acknowledged, &recordCount); err != nil || state != "APPLIED" || !acknowledged || recordCount != 1 {
		t.Fatal("Account actual credit/ACK missing", err)
	}
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, data.NewWallet).Scan(&posted); err != nil || posted != 100 {
		t.Fatal("real F12 conservation differs", err)
	}
	assertF12OriginalProjection(t, data.OldFacts, captureF12PaymentFacts(t, ctx, r, data.OldOrder, data.OldWallet)())
}

func assertF12OriginalProjection(t *testing.T, before, after string) {
	t.Helper()
	original, current := strings.Split(strings.TrimSpace(before), "\n"), strings.Split(strings.TrimSpace(after), "\n")
	if len(original) != len(current) {
		t.Fatal("financial projection table set changed")
	}
	for i, line := range original {
		name, raw, ok := strings.Cut(line, ":")
		next, now, okNext := strings.Cut(current[i], ":")
		if !ok || !okNext || name != next {
			t.Fatal("financial projection identity differs")
		}
		decode := func(raw string) []map[string]json.RawMessage {
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &rows); err != nil {
				t.Fatal(err)
			}
			return rows
		}
		oldRows, newRows := decode(raw), decode(now)
		if len(oldRows) != len(newRows) {
			t.Fatal("original financial row count differs")
		}
		if len(oldRows) == 0 {
			continue
		}
		keys := oldRows[0]
		canonical := func(rows []map[string]json.RawMessage) []string {
			var values []string
			for _, row := range rows {
				projected := map[string]json.RawMessage{}
				for key := range keys {
					value, present := row[key]
					if !present {
						t.Fatal("original financial column disappeared")
					}
					projected[key] = value
				}
				raw, err := json.Marshal(projected)
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, string(raw))
			}
			sort.Strings(values)
			return values
		}
		if !slices.Equal(canonical(oldRows), canonical(newRows)) {
			t.Fatal("upgrade changed original financial row values")
		}
	}
}
