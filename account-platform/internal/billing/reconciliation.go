package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"strings"
	"time"
)

type ReconciliationDifference struct {
	MerchantOrderNo    string
	PaymentOrderID     *uuid.UUID
	CaseType           string
	Severity           string
	Provider           *ProviderBillEntry
	Internal           *PaymentOrder
	InternalSnapshot   json.RawMessage
	RefundSnapshot     json.RawMessage
	LedgerSnapshot     json.RawMessage
	IntegrityDigest    string
	InternalEntryType  string
	InternalReference  string
	InternalAmountFen  int64
	InternalCurrency   string
	InternalDateDigest string
}

type ReconciliationRun struct {
	ID               uuid.UUID  `json:"reconciliation_run_id"`
	ProviderID       string     `json:"provider_id"`
	ProviderVersion  string     `json:"provider_version"`
	Channel          string     `json:"channel"`
	BusinessDate     string     `json:"business_date"`
	SourceMode       string     `json:"source_mode"`
	RequestedBy      *uuid.UUID `json:"requested_by,omitempty"`
	InputDigest      *string    `json:"input_digest,omitempty"`
	Status           string     `json:"status"`
	ProviderRowCount int        `json:"provider_row_count"`
	CaseCount        int        `json:"case_count"`
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

func (p *PaymentCoordinator) RunReconciliation(ctx context.Context, channel string, businessDate time.Time) (ReconciliationRun, error) {
	return p.runProviderReconciliation(ctx, channel, businessDate, "MANUAL_API", nil)
}

func (p *PaymentCoordinator) RunAdminReconciliation(ctx context.Context, actorID uuid.UUID, channel string, businessDate time.Time) (ReconciliationRun, error) {
	if actorID == uuid.Nil {
		return ReconciliationRun{}, ErrConflict
	}
	return p.runProviderReconciliation(ctx, channel, businessDate, "MANUAL_API", &actorID)
}

func (p *PaymentCoordinator) RunScheduledReconciliation(ctx context.Context, channel string, businessDate time.Time) (ReconciliationRun, error) {
	return p.runProviderReconciliation(ctx, channel, businessDate, "SCHEDULED_API", nil)
}

func (p *PaymentCoordinator) runProviderReconciliation(ctx context.Context, channel string, businessDate time.Time, sourceMode string, actorID *uuid.UUID) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillSupported {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	if !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) {
		return ReconciliationRun{}, ErrConflict
	}
	date := normalizedReconciliationBusinessDate(businessDate)
	providerRows, err := providerCall(ctx, p, "BILL_QUERY", channel, false, ProviderCallScope{}, func() ([]ProviderBillEntry, error) {
		return p.provider.Bill(ctx, channel, date)
	})
	if err != nil {
		return ReconciliationRun{}, errors.Join(ErrPaymentProvider, err)
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, sourceMode, actorID)
}

func (p *PaymentCoordinator) ImportProviderBill(ctx context.Context, actorID uuid.UUID, channel string, businessDate time.Time, providerRows []ProviderBillEntry) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !p.Capabilities().BillFileImportSupported || actorID == uuid.Nil || !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) || len(providerRows) > 100000 {
		return ReconciliationRun{}, payment.ErrUnsupported
	}
	date := normalizedReconciliationBusinessDate(businessDate)
	start, end := reconciliationBusinessDateRange(date)
	for index := range providerRows {
		row := &providerRows[index]
		if strings.TrimSpace(row.Channel) == "" {
			row.Channel = channel
		}
		entryType := normalizedBillEntryType(*row)
		if strings.TrimSpace(row.MerchantOrderNo) == "" || billReference(*row) == "" || row.AmountFen <= 0 || row.Currency == "" || !strings.EqualFold(row.Channel, channel) || row.PaidAt.Before(start) || !row.PaidAt.Before(end) || (entryType != "PAYMENT" && entryType != "REFUND" && entryType != "CHARGEBACK") || !successfulBillEntryStatus(*row) {
			return ReconciliationRun{}, ErrConflict
		}
		row.Channel = channel
	}
	return p.persistReconciliation(ctx, channel, date, providerRows, "FILE_IMPORT", &actorID)
}

func (p *PaymentCoordinator) persistReconciliation(ctx context.Context, channel string, date time.Time, providerRows []ProviderBillEntry, sourceMode string, importActorID *uuid.UUID) (ReconciliationRun, error) {
	return p.runReconciliationPipeline(ctx, channel, date, providerRows, sourceMode, importActorID)
}

func (p *PaymentCoordinator) mergeProviderBillEvidenceWithinTransaction(ctx context.Context, tx pgx.Tx, row ProviderBillEntry, confirmed *ProviderOrder, receivedAt time.Time) error {
	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	entryType := normalizedBillEntryType(row)
	reference := billReference(row)
	evidenceKey := fmt.Sprintf("%s|%s|%s|%s", entryType, reference, strings.TrimSpace(row.MerchantOrderNo), row.PaidAt.UTC().Format(time.RFC3339Nano))
	order, orderErr := getPaymentOrderForUpdate(ctx, tx, strings.TrimSpace(row.MerchantOrderNo))
	matchedOrder := orderErr == nil && order.ProviderID == p.Capabilities().ProviderID && order.ProviderVersion == p.Capabilities().Version
	if orderErr != nil && !errors.Is(orderErr, pgx.ErrNoRows) {
		return orderErr
	}
	status := strings.ToUpper(strings.TrimSpace(row.Status))
	isSuccessfulPayment := entryType == "PAYMENT" && (status == "SUCCESS" || status == "PAID" || status == "COMPLETED") && strings.TrimSpace(row.ChannelTransactionID) != ""
	if matchedOrder && isSuccessfulPayment && confirmed != nil && providerBillMatchesQuery(row, *confirmed) && providerMatchesOrder(*confirmed, order) {
		// The query supplied customer/SKU authority; persist its real typed facts
		// separately from the original bill, which lacks that authority.
		_, _, mergeErr := p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "BILL_CONFIRMATION|"+evidenceKey, nil, *confirmed, false, receivedAt)
		if mergeErr != nil && !errors.Is(mergeErr, ErrConflict) {
			return mergeErr
		}
	}

	var orderID *uuid.UUID
	var transactionID *uuid.UUID
	mergeResult := "REVIEW_REQUIRED"
	if matchedOrder {
		orderID = &order.ID
		var found uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1 AND provider_transaction_id=$2 AND provider_id=$3 AND provider_version=$4 AND channel=$5 AND amount_fen=$6 AND currency=$7 AND status IN ('PAID','LATE_PAID')`, order.ID, strings.TrimSpace(row.ChannelTransactionID), p.Capabilities().ProviderID, p.Capabilities().Version, row.Channel, row.AmountFen, row.Currency).Scan(&found); err == nil {
			transactionID = &found
			if isSuccessfulPayment {
				mergeResult = "DUPLICATE"
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	digest := sha256.Sum256(payload)
	evidenceID := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO payment_evidence(
id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,provider_event_at,
received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'BILL',$4,$5,$6,$7,NULLIF($8,''),CASE WHEN $17='REFUND' THEN NULLIF($18,'') END,$9,$10,NULLIF($11,''),$12,$13,$14,$15,$16)
ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING`,
		evidenceID, orderID, transactionID, p.Capabilities().ProviderID, p.Capabilities().Version, evidenceKey,
		row.MerchantOrderNo, strings.TrimSpace(row.ChannelTransactionID), row.AmountFen, row.Currency, row.Status,
		row.PaidAt.UTC(), receivedAt, hex.EncodeToString(digest[:]), json.RawMessage(payload), mergeResult, entryType, reference)
	return err
}

type ReconciliationInternalFact struct {
	EntryType           string
	ProviderReferenceID string
	MerchantOrderNo     string
	PaymentOrderID      uuid.UUID
	AmountFen           int64
	Currency            string
	InternalSnapshot    json.RawMessage
	RefundSnapshot      json.RawMessage
	LedgerSnapshot      json.RawMessage
	LedgerPresent       bool
	LedgerValid         bool
	IntegrityCaseType   string
	IntegrityDigest     string
	EventDateVerified   bool
	EventDateConflict   bool
	EventDateDigest     string
	EventDateSnapshot   json.RawMessage
}

func normalizedBillEntryType(item ProviderBillEntry) string {
	entryType := strings.ToUpper(strings.TrimSpace(item.EntryType))
	if entryType != "" {
		return entryType
	}
	status := strings.ToUpper(strings.TrimSpace(item.Status))
	if strings.Contains(status, "CHARGEBACK") {
		return "CHARGEBACK"
	}
	if strings.Contains(status, "REFUND") {
		return "REFUND"
	}
	return "PAYMENT"
}

func billReference(item ProviderBillEntry) string {
	if value := strings.TrimSpace(item.ProviderReferenceID); value != "" {
		return value
	}
	return strings.TrimSpace(item.ChannelTransactionID)
}

func successfulBillEntryStatus(item ProviderBillEntry) bool {
	status := strings.ToUpper(strings.TrimSpace(item.Status))
	switch normalizedBillEntryType(item) {
	case "PAYMENT":
		return status == "SUCCESS" || status == "PAID" || status == "COMPLETED"
	case "REFUND":
		return status == "SUCCESS" || status == "SUCCEEDED" || status == "REFUNDED" || status == "COMPLETED"
	case "CHARGEBACK":
		return status == "SUCCESS" || status == "CONFIRMED" || status == "CHARGEBACK" || status == "COMPLETED"
	default:
		return false
	}
}

func CompareFourWayReconciliation(provider []ProviderBillEntry, internal []ReconciliationInternalFact) []ReconciliationDifference {
	providerByKey := map[string]ProviderBillEntry{}
	differences := []ReconciliationDifference{}
	for index := range provider {
		item := provider[index]
		entryType, reference := normalizedBillEntryType(item), billReference(item)
		if (entryType != "PAYMENT" && entryType != "REFUND" && entryType != "CHARGEBACK") || !successfulBillEntryStatus(item) {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "UNSUPPORTED_PROVIDER_ENTRY", Severity: "P0", Provider: &copy})
			continue
		}
		key := entryType + "|" + reference
		if _, exists := providerByKey[key]; exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "DUPLICATE_TRANSACTION", Severity: "P0", Provider: &copy})
			continue
		}
		providerByKey[key] = item
	}
	internalByKey := map[string]ReconciliationInternalFact{}
	for index := range internal {
		fact := internal[index]
		key := fact.EntryType + "|" + fact.ProviderReferenceID
		if _, exists := internalByKey[key]; exists {
			differences = append(differences, differenceFromFact(fact, "DUPLICATE_TRANSACTION", "P0", nil))
			continue
		}
		internalByKey[key] = fact
		providerItem, exists := providerByKey[key]
		caseTypePrefix := map[string]string{"PAYMENT": "PAID", "REFUND": "REFUND", "CHARGEBACK": "CHARGEBACK"}[fact.EntryType]
		if !exists || providerItem.MerchantOrderNo != fact.MerchantOrderNo {
			caseType := "INTERNAL_" + caseTypePrefix + "_PROVIDER_MISSING"
			if fact.EntryType != "PAYMENT" && !fact.EventDateVerified {
				caseType = fact.EntryType + "_DATE_UNVERIFIED"
			}
			differences = append(differences, differenceFromFact(fact, caseType, "P1", nil))
		} else if providerItem.AmountFen != fact.AmountFen || !strings.EqualFold(providerItem.Currency, fact.Currency) {
			caseType := "AMOUNT_MISMATCH"
			if fact.EntryType == "REFUND" {
				caseType = "REFUND_AMOUNT_MISMATCH"
			}
			copy := providerItem
			differences = append(differences, differenceFromFact(fact, caseType, "P0", &copy))
		}
		if fact.EventDateConflict {
			differences = append(differences, differenceFromFact(fact, fact.EntryType+"_DATE_UNVERIFIED", "P1", nil))
		}
		if fact.IntegrityCaseType != "" {
			differences = append(differences, differenceFromFact(fact, fact.IntegrityCaseType, "P0", nil))
		}
		if !fact.LedgerPresent {
			caseType := map[string]string{"PAYMENT": "PAYMENT_LEDGER_MISSING", "REFUND": "REFUND_LEDGER_MISSING", "CHARGEBACK": "CHARGEBACK_LEDGER_MISSING"}[fact.EntryType]
			differences = append(differences, differenceFromFact(fact, caseType, "P0", nil))
		}
	}
	for key, item := range providerByKey {
		if fact, exists := internalByKey[key]; exists && fact.MerchantOrderNo == item.MerchantOrderNo {
			continue
		}
		entryType := normalizedBillEntryType(item)
		caseType := map[string]string{"PAYMENT": "PROVIDER_PAID_INTERNAL_MISSING", "REFUND": "PROVIDER_REFUND_INTERNAL_MISSING", "CHARGEBACK": "PROVIDER_CHARGEBACK_INTERNAL_MISSING"}[entryType]
		copy := item
		differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: caseType, Severity: "P0", Provider: &copy})
	}
	return differences
}

func differenceFromFact(fact ReconciliationInternalFact, caseType, severity string, provider *ProviderBillEntry) ReconciliationDifference {
	orderID := fact.PaymentOrderID
	return ReconciliationDifference{MerchantOrderNo: fact.MerchantOrderNo, PaymentOrderID: &orderID, CaseType: caseType, Severity: severity, Provider: provider, InternalSnapshot: fact.InternalSnapshot, RefundSnapshot: fact.RefundSnapshot, LedgerSnapshot: fact.LedgerSnapshot, IntegrityDigest: fact.IntegrityDigest, InternalEntryType: fact.EntryType, InternalReference: fact.ProviderReferenceID, InternalAmountFen: fact.AmountFen, InternalCurrency: fact.Currency, InternalDateDigest: fact.EventDateDigest}
}

type reconciliationQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (p *PaymentCoordinator) listFourWayReconciliationFacts(ctx context.Context, querier reconciliationQuerier, channel string, start, end time.Time) ([]ReconciliationInternalFact, error) {
	return listFourWayFactsForProvider(ctx, querier, channel, start, end, p.Capabilities().ProviderID, p.Capabilities().Version)
}

func listFourWayFactsForProvider(ctx context.Context, querier reconciliationQuerier, channel string, start, end time.Time, providerID, providerVersion string, externalRows ...[]ProviderBillEntry) ([]ReconciliationInternalFact, error) {
	var supplied []ProviderBillEntry
	if len(externalRows) > 0 {
		supplied = externalRows[0]
	}
	references := reconciliationReversalReferences(supplied)
	rows, err := querier.Query(ctx, `
SELECT 'PAYMENT',transaction.provider_transaction_id,payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order',to_jsonb(payment),'payment_transaction',to_jsonb(transaction)),NULL,
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference IN ('payment-transaction:'||transaction.id::text,'repair-payment-transaction:'||transaction.id::text)),'[]'::jsonb)
FROM payment_transactions transaction JOIN payment_orders payment ON payment.id=transaction.payment_order_id
WHERE transaction.channel=$1 AND transaction.provider_paid_at>=$2 AND transaction.provider_paid_at<$3 AND transaction.status IN ('PAID','LATE_PAID') AND transaction.provider_id=$4 AND transaction.provider_version=$5
UNION ALL
SELECT 'REFUND',refund.channel_refund_id,payment.merchant_order_no,payment.id,refund.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',refund.payment_transaction_id),to_jsonb(refund),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='refund:'||refund.id::text),'[]'::jsonb)
FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id
WHERE payment.channel=$1 AND ((refund.updated_at>=$2 AND refund.updated_at<$3) OR (refund.channel_refund_id,payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='REFUND')) AND refund.status='SUCCEEDED' AND refund.channel_refund_id IS NOT NULL AND payment.provider_id=$4 AND payment.provider_version=$5
UNION ALL
SELECT 'REFUND',forced.provider_refund_id,payment.merchant_order_no,payment.id,forced.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',forced.payment_transaction_id),to_jsonb(forced),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='forced-refund:'||forced.id::text),'[]'::jsonb)
FROM forced_refund_cases forced JOIN payment_orders payment ON payment.id=forced.payment_order_id
WHERE payment.channel=$1 AND ((forced.resolved_at>=$2 AND forced.resolved_at<$3) OR (forced.provider_refund_id,payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='REFUND')) AND forced.resolution_code='CONFIRM_FORCED_REFUND' AND payment.provider_id=$4 AND payment.provider_version=$5
UNION ALL
SELECT 'CHARGEBACK',chargeback.provider_chargeback_id,payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',chargeback.payment_transaction_id),to_jsonb(chargeback),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='payment-transaction:'||chargeback.payment_transaction_id::text||':chargeback:'||chargeback.provider_chargeback_id),'[]'::jsonb)
FROM chargeback_cases chargeback JOIN payment_orders payment ON payment.id=chargeback.payment_order_id JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
WHERE payment.channel=$1 AND ((chargeback.resolved_at>=$2 AND chargeback.resolved_at<$3) OR (chargeback.provider_chargeback_id,payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='CHARGEBACK')) AND chargeback.status='CONFIRMED' AND transaction.provider_id=$4 AND transaction.provider_version=$5
ORDER BY 1,2`, channel, start, end, providerID, providerVersion, references)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []ReconciliationInternalFact{}
	for rows.Next() {
		var fact ReconciliationInternalFact
		if err := rows.Scan(&fact.EntryType, &fact.ProviderReferenceID, &fact.MerchantOrderNo, &fact.PaymentOrderID, &fact.AmountFen, &fact.Currency, &fact.InternalSnapshot, &fact.RefundSnapshot, &fact.LedgerSnapshot); err != nil {
			return nil, err
		}
		fact.LedgerPresent = len(fact.LedgerSnapshot) > 0 && string(fact.LedgerSnapshot) != "[]"
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return classifyReversalEventDates(ctx, querier, facts, channel, start, end, providerID, providerVersion, supplied)
}

type scheduledPaymentReconciler interface {
	Capabilities() PaymentCapabilities
	RunScheduledReconciliation(context.Context, string, time.Time) (ReconciliationRun, error)
}

const (
	reconciliationRecoveryInterval    = time.Minute
	reconciliationObservationInterval = time.Hour
	reconciliationLookbackDays        = 7
	reconciliationRecoverySlice       = 10 * time.Second
)

type DailyPaymentReconciliationWorker struct {
	payments        scheduledPaymentReconciler
	logger          *slog.Logger
	now             func() time.Time
	interval        time.Duration
	lastScheduledAt time.Time
}

func NewDailyPaymentReconciliationWorker(payments scheduledPaymentReconciler) *DailyPaymentReconciliationWorker {
	return &DailyPaymentReconciliationWorker{payments: payments, logger: slog.Default(), now: time.Now, interval: reconciliationRecoveryInterval}
}

func reconciliationBusinessDate(now time.Time) time.Time {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(location).AddDate(0, 0, -1)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func normalizedReconciliationBusinessDate(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func reconciliationBusinessDateRange(date time.Time) (time.Time, time.Time) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location)
	return start, start.AddDate(0, 0, 1)
}

func (w *DailyPaymentReconciliationWorker) Run(ctx context.Context) {
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *DailyPaymentReconciliationWorker) runOnce(ctx context.Context) {
	if recovery, ok := w.payments.(interface{ recoverReconciliationRuns(context.Context) error }); ok {
		if err := recovery.recoverReconciliationRuns(ctx); err != nil {
			w.logger.Error("reconciliation recovery deferred", "code", "RECOVERY_REQUIRED")
		}
	}
	if w.payments == nil || !w.payments.Capabilities().BillSupported {
		return
	}
	now := w.now()
	if !w.lastScheduledAt.IsZero() && now.Sub(w.lastScheduledAt) < reconciliationObservationInterval {
		return
	}
	w.lastScheduledAt = now
	for lookback := 0; lookback < reconciliationLookbackDays; lookback++ {
		date := reconciliationBusinessDate(now).AddDate(0, 0, -lookback)
		for _, channel := range w.payments.Capabilities().Channels {
			if ctx.Err() != nil {
				return
			}
			if _, err := w.payments.RunScheduledReconciliation(ctx, channel, date); err != nil {
				w.logger.Error("scheduled payment reconciliation failed", "channel", channel, "business_date", date.Format("2006-01-02"), "error", safePaymentReconciliationError(err))
			}
		}
	}
}

func billPaymentKey(row ProviderBillEntry) string {
	return strings.TrimSpace(row.MerchantOrderNo) + "|" + strings.TrimSpace(row.ChannelTransactionID)
}

func providerBillMatchesQuery(row ProviderBillEntry, fact ProviderOrder) bool {
	return normalizedBillEntryType(row) == "PAYMENT" && fact.Status == "SUCCESS" &&
		fact.ChannelTransactionID != nil && fact.PaidAt != nil &&
		strings.TrimSpace(row.ChannelTransactionID) == *fact.ChannelTransactionID &&
		row.MerchantOrderNo == fact.MerchantOrderNo && row.Channel == fact.Channel &&
		row.AmountFen == fact.AmountFen && row.Currency == fact.Currency &&
		row.PaidAt.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02") == fact.PaidAt.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
}

func (p *PaymentCoordinator) confirmBillPaymentFacts(ctx context.Context, rows []ProviderBillEntry) (map[string]*ProviderOrder, error) {
	confirmed := map[string]*ProviderOrder{}
	if !p.Capabilities().OrderQuerySupported {
		return confirmed, nil
	}
	queried := map[string]bool{}
	for _, row := range rows {
		if normalizedBillEntryType(row) != "PAYMENT" || !successfulBillEntryStatus(row) {
			continue
		}
		key := billPaymentKey(row)
		if queried[key] {
			continue
		}
		queried[key] = true
		var id uuid.UUID
		err := p.repository.pool.QueryRow(ctx, `SELECT id FROM payment_orders WHERE merchant_order_no=$1 AND provider_id=$2 AND provider_version=$3`, row.MerchantOrderNo, p.Capabilities().ProviderID, p.Capabilities().Version).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		order, err := getPaymentOrderByID(ctx, p.repository.pool, id, false)
		if err != nil {
			return nil, err
		}
		// Fully credited facts are already durable. A repeated supplier bill is not
		// a new authority to regress that payment or to create a second journal.
		if order.FulfillmentStatus == "CREDITED" {
			continue
		}
		fact, err := providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) { return p.provider.Get(ctx, order.MerchantOrderNo) })
		if err != nil {
			return nil, errors.Join(ErrPaymentProvider, err)
		}
		if providerBillMatchesQuery(row, fact) && providerMatchesOrder(fact, order) {
			confirmed[key] = &fact
		}
	}
	return confirmed, nil
}
