package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func paymentRequestSource(r *http.Request) string {
	if source := strings.TrimSpace(r.Header.Get("X-Pinvou-Client-Source")); net.ParseIP(source) != nil {
		return source
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		if source := strings.TrimSpace(forwarded[index]); net.ParseIP(source) != nil {
			return source
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return "source:unknown"
}

func (p *PaymentCoordinator) AdmitCreate(ctx context.Context, payer uuid.UUID, organization *uuid.UUID, source string) error {
	return p.admitPaymentAction(ctx, "CREATE", payer, organization, nil, nil, nil, source)
}

func (p *PaymentCoordinator) AdmitRefund(ctx context.Context, payer, transaction, packageID uuid.UUID, source string) error {
	return p.admitPaymentAction(ctx, "REFUND", payer, nil, nil, &transaction, &packageID, source)
}

func (p *PaymentCoordinator) admitPaymentAction(ctx context.Context, action string, payer uuid.UUID, organization, order, transaction, packageID *uuid.UUID, source string) error {
	if payer == uuid.Nil {
		return ErrConflict
	}
	now := p.now()
	sourceHash := sha256.Sum256([]byte(strings.TrimSpace(source)))
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locks := []string{"payment:" + action + ":payer:" + payer.String(), "payment:" + action + ":provider:" + p.Capabilities().ProviderID, paymentSourceLockKey(action, sourceHash)}
	if organization != nil {
		locks = append(locks, "payment:"+action+":organization:"+organization.String())
	}
	sort.Strings(locks)
	for _, lock := range locks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return err
		}
	}
	var payerMinute, payerHour, payerDay, organizationHour, sourceMinute, providerMinute int
	var payerMinuteOldest, payerHourOldest, payerDayOldest, organizationOldest, sourceOldest, providerOldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 minute'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 minute'),
count(*) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 hour'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 hour'),
count(*) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 day'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND payer_user_id=$1 AND occurred_at>$4-interval '1 day'),
count(*) FILTER (WHERE result='ACCEPTED' AND beneficiary_organization_id=$2 AND occurred_at>$4-interval '1 hour'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND beneficiary_organization_id=$2 AND occurred_at>$4-interval '1 hour'),
count(*) FILTER (WHERE result='ACCEPTED' AND source_hash=$3 AND occurred_at>$4-interval '1 minute'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND source_hash=$3 AND occurred_at>$4-interval '1 minute'),
count(*) FILTER (WHERE result='ACCEPTED' AND provider_id=$5 AND occurred_at>$4-interval '1 minute'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND provider_id=$5 AND occurred_at>$4-interval '1 minute')
FROM payment_risk_attempts WHERE action=$6 AND occurred_at>$4-interval '1 day'`, payer, organization, sourceHash[:], now, p.Capabilities().ProviderID, action).Scan(
		&payerMinute, &payerMinuteOldest, &payerHour, &payerHourOldest, &payerDay, &payerDayOldest, &organizationHour, &organizationOldest, &sourceMinute, &sourceOldest, &providerMinute, &providerOldest,
	); err != nil {
		return err
	}
	retryAfter := time.Duration(0)
	rates := []struct {
		reached bool
		oldest  *time.Time
		window  time.Duration
	}{}
	if action == "CREATE" {
		rates = append(rates, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{payerMinute >= 5, payerMinuteOldest, time.Minute}, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{payerDay >= 20, payerDayOldest, 24 * time.Hour}, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{organization != nil && organizationHour >= 30, organizationOldest, time.Hour}, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{sourceMinute >= 20, sourceOldest, time.Minute}, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{providerMinute >= 80, providerOldest, time.Minute})
	} else {
		rates = append(rates, struct {
			reached bool
			oldest  *time.Time
			window  time.Duration
		}{payerHour >= 3, payerHourOldest, time.Hour})
	}
	for _, rate := range rates {
		if rate.reached && rate.oldest != nil {
			retryAfter = max(retryAfter, rate.oldest.Add(rate.window).Sub(now))
		}
	}
	result := "ACCEPTED"
	if retryAfter > 0 {
		result = "RATE_LIMITED"
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,beneficiary_organization_id,source_hash,order_id,payment_transaction_id,entitlement_package_id,provider_id,result,provider_dispatched,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,$11)`, uuid.Must(uuid.NewV7()), action, payer, organization, sourceHash[:], order, transaction, packageID, p.Capabilities().ProviderID, result, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if result == "RATE_LIMITED" {
		return &PaymentRateLimitError{RetryAfter: max(retryAfter, time.Second)}
	}
	return nil
}

func paymentSourceLockKey(action string, sourceHash [sha256.Size]byte) string {
	return "payment:" + action + ":source:" + hex.EncodeToString(sourceHash[:])
}

func (p *PaymentCoordinator) AdmitOrderQuery(ctx context.Context, order PaymentOrder, source string) (bool, error) {
	now := p.now()
	sourceHash := sha256.Sum256([]byte(strings.TrimSpace(source)))
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "payment:query:"+order.ID.String()); err != nil {
		return false, err
	}
	var count int
	var oldest, lastDispatch *time.Time
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE result='ACCEPTED' AND occurred_at>$2-interval '1 minute'),min(occurred_at) FILTER (WHERE result='ACCEPTED' AND occurred_at>$2-interval '1 minute'),max(occurred_at) FILTER (WHERE provider_dispatched) FROM payment_risk_attempts WHERE action='ORDER_QUERY' AND order_id=$1`, order.ID, now).Scan(&count, &oldest, &lastDispatch); err != nil {
		return false, err
	}
	if count >= 12 && oldest != nil {
		retry := max(oldest.Add(time.Minute).Sub(now), time.Second)
		_, _ = tx.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,source_hash,order_id,provider_id,result,provider_dispatched,occurred_at) VALUES($1,'ORDER_QUERY',$2,$3,$4,$5,'RATE_LIMITED',false,$6)`, uuid.Must(uuid.NewV7()), order.RequesterID, sourceHash[:], order.ID, order.ProviderID, now)
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, &PaymentRateLimitError{RetryAfter: retry}
	}
	interval := max(5*time.Second, time.Duration(p.Capabilities().MinimumQueryIntervalSeconds)*time.Second)
	dispatch := lastDispatch == nil || !lastDispatch.Add(interval).After(now)
	_, err = tx.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,source_hash,order_id,provider_id,result,provider_dispatched,occurred_at) VALUES($1,'ORDER_QUERY',$2,$3,$4,$5,'ACCEPTED',$6,$7)`, uuid.Must(uuid.NewV7()), order.RequesterID, sourceHash[:], order.ID, order.ProviderID, dispatch, now)
	if err != nil {
		return false, err
	}
	return dispatch, tx.Commit(ctx)
}
