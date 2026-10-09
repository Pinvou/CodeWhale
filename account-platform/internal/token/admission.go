package token

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

var usageInflightStates = []string{"RECEIVED", "HOLD_RESERVING", "HOLD_RESERVED", "DISPATCH_INTENT_PERSISTED", "UPSTREAM_DISPATCHING", "STREAMING", "SETTLEMENT_PENDING"}

func admitUsage(ctx context.Context, tx pgx.Tx, request UsageRequest, now time.Time) error {
	locks := []string{"usage:global", "usage:model:" + request.Model, "usage:route:" + request.TokenAccountID.String(), "usage:user:" + request.UserID.String()}
	if request.OrganizationID != nil {
		locks = append(locks, "usage:organization:"+request.OrganizationID.String(), "usage:member:"+request.OrganizationID.String()+":"+request.UserID.String())
	}
	sort.Strings(locks)
	for _, lock := range locks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return err
		}
	}
	var userMinute, userInflight, memberInflight, organizationMinute, organizationInflight int
	var modelMinute, modelInflight, routeMinute, routeInflight, globalMinute, globalInflight int
	var userOldest, organizationOldest, modelOldest, routeOldest, globalOldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE user_id=$1 AND started_at>$5::timestamptz-interval '1 minute'), min(started_at) FILTER (WHERE user_id=$1 AND started_at>$5::timestamptz-interval '1 minute'),
count(*) FILTER (WHERE user_id=$1 AND execution_status=ANY($6)),
count(*) FILTER (WHERE organization_id=$2 AND user_id=$1 AND execution_status=ANY($6)),
count(*) FILTER (WHERE organization_id=$2 AND started_at>$5::timestamptz-interval '1 minute'), min(started_at) FILTER (WHERE organization_id=$2 AND started_at>$5::timestamptz-interval '1 minute'),
count(*) FILTER (WHERE organization_id=$2 AND execution_status=ANY($6)),
count(*) FILTER (WHERE model=$3 AND started_at>$5::timestamptz-interval '1 minute'), min(started_at) FILTER (WHERE model=$3 AND started_at>$5::timestamptz-interval '1 minute'),
count(*) FILTER (WHERE model=$3 AND execution_status=ANY($6)),
count(*) FILTER (WHERE token_account_id=$4 AND started_at>$5::timestamptz-interval '1 minute'), min(started_at) FILTER (WHERE token_account_id=$4 AND started_at>$5::timestamptz-interval '1 minute'),
count(*) FILTER (WHERE token_account_id=$4 AND execution_status=ANY($6)),
count(*) FILTER (WHERE started_at>$5::timestamptz-interval '1 minute'), min(started_at) FILTER (WHERE started_at>$5::timestamptz-interval '1 minute'),
count(*) FILTER (WHERE execution_status=ANY($6))
FROM usage_requests
WHERE started_at>$5::timestamptz-interval '1 minute' OR execution_status=ANY($6)`, request.UserID, request.OrganizationID, request.Model, request.TokenAccountID, now, usageInflightStates).Scan(
		&userMinute, &userOldest, &userInflight, &memberInflight, &organizationMinute, &organizationOldest, &organizationInflight,
		&modelMinute, &modelOldest, &modelInflight, &routeMinute, &routeOldest, &routeInflight, &globalMinute, &globalOldest, &globalInflight,
	); err != nil {
		return err
	}
	retryAfter := time.Duration(0)
	for _, rate := range []struct {
		reached bool
		oldest  *time.Time
	}{
		{userMinute >= 60, userOldest}, {request.OrganizationID != nil && organizationMinute >= 600, organizationOldest},
		{modelMinute >= 300, modelOldest}, {routeMinute >= 300, routeOldest}, {globalMinute >= 1000, globalOldest},
	} {
		if rate.reached && rate.oldest != nil {
			retryAfter = max(retryAfter, rate.oldest.Add(time.Minute).Sub(now))
		}
	}
	concurrencyReached := userInflight >= 5 || request.OrganizationID != nil && (memberInflight >= 5 || organizationInflight >= 100) || modelInflight >= 20 || routeInflight >= 50 || globalInflight >= 200
	if retryAfter > 0 || concurrencyReached {
		return &RateLimitError{RetryAfter: max(retryAfter, time.Second)}
	}
	return nil
}
