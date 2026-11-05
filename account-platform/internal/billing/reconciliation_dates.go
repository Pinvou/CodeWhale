package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

func reconciliationReversalReferences(rows []ProviderBillEntry) []byte {
	supplied := []map[string]string{}
	seen := map[string]bool{}
	for _, row := range rows {
		kind := normalizedBillEntryType(row)
		if kind != "REFUND" && kind != "CHARGEBACK" {
			continue
		}
		key := kind + "\x00" + billReference(row) + "\x00" + row.MerchantOrderNo
		if seen[key] {
			continue
		}
		seen[key] = true
		supplied = append(supplied, map[string]string{"reference": billReference(row), "merchant": row.MerchantOrderNo, "kind": kind})
	}
	raw, _ := json.Marshal(supplied)
	return raw
}

// Settlement timestamps cannot be relabelled as supplier occurrence timestamps.
// A matching immutable API bill gives reporting authority only, never authority
// to refund or charge back. Unverified dates remain explicit human review.
func classifyReversalEventDates(ctx context.Context, q reconciliationQuerier, facts []ReconciliationInternalFact, channel string, start, end time.Time, providerID, providerVersion string, incoming []ProviderBillEntry) ([]ReconciliationInternalFact, error) {
	requests := []map[string]any{}
	for index, fact := range facts {
		if fact.EntryType == "PAYMENT" {
			facts[index].EventDateVerified = true
			continue
		}
		requests = append(requests, map[string]any{"i": index, "kind": fact.EntryType, "reference": fact.ProviderReferenceID, "merchant": fact.MerchantOrderNo, "amount": fact.AmountFen, "currency": fact.Currency})
	}
	if len(requests) == 0 {
		return facts, nil
	}
	raw, _ := json.Marshal(requests)
	rows, err := q.Query(ctx, `SELECT request.i,min(bill.paid_at),count(DISTINCT (bill.paid_at AT TIME ZONE 'Asia/Shanghai')::date),
 jsonb_build_object('provider_id',$2::text,'provider_version',$3::text,'channel',$4::text,'entry_type',request.kind,'reference',request.reference,'merchant',request.merchant,'amount_fen',request.amount,'currency',request.currency,'source_mode',CASE WHEN count(bill.reconciliation_run_id)=0 THEN 'UNVERIFIED' WHEN count(DISTINCT (bill.paid_at AT TIME ZONE 'Asia/Shanghai')::date)>1 THEN 'CONFLICTING_TRUSTED_API_BILLS' ELSE 'TRUSTED_API_BILL' END,'occurrence_dates',COALESCE(jsonb_agg(DISTINCT (bill.paid_at AT TIME ZONE 'Asia/Shanghai')::date ORDER BY (bill.paid_at AT TIME ZONE 'Asia/Shanghai')::date) FILTER(WHERE bill.paid_at IS NOT NULL),'[]'::jsonb),'source_run_ids',COALESCE(jsonb_agg(DISTINCT bill.reconciliation_run_id) FILTER(WHERE bill.reconciliation_run_id IS NOT NULL),'[]'::jsonb))
 FROM jsonb_to_recordset($1::jsonb) request(i int,kind text,reference text,merchant text,amount bigint,currency text)
 LEFT JOIN reconciliation_provider_rows bill ON bill.entry_type=request.kind AND bill.provider_reference_id=request.reference AND bill.merchant_order_no=request.merchant AND bill.amount_fen=request.amount AND bill.currency=request.currency AND bill.channel=$4
 AND upper(trim(bill.status)) IN ('SUCCESS','SUCCEEDED','REFUNDED','COMPLETED','CONFIRMED','CHARGEBACK')
 AND EXISTS(SELECT 1 FROM reconciliation_runs run WHERE run.id=bill.reconciliation_run_id AND run.provider_id=$2 AND run.provider_version=$3 AND run.source_mode IN ('MANUAL_API','SCHEDULED_API'))
 GROUP BY request.i,request.kind,request.reference,request.merchant,request.amount,request.currency`, raw, providerID, providerVersion, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type knownDate struct {
		date     *time.Time
		days     int
		snapshot []byte
	}
	known := map[int]knownDate{}
	for rows.Next() {
		var index, count int
		var date *time.Time
		var snapshot []byte
		if err := rows.Scan(&index, &date, &count, &snapshot); err != nil {
			return nil, err
		}
		known[index] = knownDate{date, count, snapshot}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, row := range incoming {
		present[normalizedBillEntryType(row)+"\x00"+billReference(row)+"\x00"+row.MerchantOrderNo] = true
	}
	result := make([]ReconciliationInternalFact, 0, len(facts))
	for index, fact := range facts {
		date := known[index]
		if fact.EntryType != "PAYMENT" {
			fact.EventDateSnapshot = date.snapshot
			fact.EventDateConflict = date.days > 1
			var proof struct {
				Dates []string `json:"occurrence_dates"`
			}
			_ = json.Unmarshal(date.snapshot, &proof)
			raw, _ := json.Marshal(proof.Dates)
			sum := sha256.Sum256(raw)
			fact.EventDateDigest = fmt.Sprintf("%x", sum)
		}
		if fact.EntryType != "PAYMENT" && date.days == 1 && date.date != nil {
			fact.EventDateVerified = true
			// The source belongs to another verified business day unless this incoming
			// batch itself explicitly contains the same frozen source reference.
			if (date.date.Before(start) || !date.date.Before(end)) && !present[fact.EntryType+"\x00"+fact.ProviderReferenceID+"\x00"+fact.MerchantOrderNo] {
				continue
			}
		}
		result = append(result, fact)
	}
	return result, nil
}
