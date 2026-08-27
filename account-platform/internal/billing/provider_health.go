package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const paymentStopLossPolicyVersion = "payment-stop-loss/v1"

type ProviderCallScope struct {
	OrderID      uuid.UUID
	ProductID    string
	SubjectID    uuid.UUID
	TrustedProbe bool
}

type providerAttempt struct {
	ID, OrderID                 uuid.UUID
	ProviderAttemptID           string
	ProviderID, ProviderVersion string
	Capability, Channel, Route  string
	ProductID                   string
	SubjectHash                 *string
	CreatedAt                   time.Time
}

func providerCallScope(order PaymentOrder) ProviderCallScope {
	return ProviderCallScope{OrderID: order.ID, ProductID: order.ProductID, SubjectID: order.RequesterID}
}

func hashProviderSubject(subjectID uuid.UUID) *string {
	if subjectID == uuid.Nil {
		return nil
	}
	digest := sha256.Sum256([]byte(subjectID.String()))
	value := hex.EncodeToString(digest[:])
	return &value
}

func (r *PostgresRepository) beginProviderAttempt(ctx context.Context, providerID, providerVersion, capability, channel, route string, scope ProviderCallScope) (providerAttempt, error) {
	now := r.now()
	attempt := providerAttempt{
		ID: uuid.Must(uuid.NewV7()), OrderID: scope.OrderID, ProviderAttemptID: uuid.Must(uuid.NewV7()).String(),
		ProviderID: providerID, ProviderVersion: providerVersion, Capability: capability,
		Channel: channel, Route: route, ProductID: scope.ProductID, SubjectHash: hashProviderSubject(scope.SubjectID), CreatedAt: now,
	}
	fingerprintSource := strings.Join([]string{providerID, providerVersion, capability, channel, route, scope.OrderID.String(), scope.ProductID, scope.SubjectID.String()}, "|")
	fingerprint := sha256.Sum256([]byte(fingerprintSource))
	_, err := r.pool.Exec(ctx, "INSERT INTO payment_provider_attempts(id,provider_attempt_id,request_fingerprint,provider_id,provider_version,capability,channel,route,logical_order_id,product_id,subject_hash,trusted_probe,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9::uuid,'00000000-0000-0000-0000-000000000000'::uuid),NULLIF($10,''),$11,$12,$13)", attempt.ID, attempt.ProviderAttemptID, hex.EncodeToString(fingerprint[:]), providerID, providerVersion, capability, channel, route, scope.OrderID, scope.ProductID, attempt.SubjectHash, scope.TrustedProbe, now)
	return attempt, err
}

func (r *PostgresRepository) finishProviderAttempt(ctx context.Context, attempt providerAttempt, outcome string, providerErr error) error {
	now := r.now()
	evidenceClass := outcome
	if providerErr != nil {
		evidenceClass += "|provider_error"
	}
	digest := sha256.Sum256([]byte(evidenceClass))
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, "UPDATE payment_provider_attempts SET status='RESULT_RECORDED',result_recorded_at=$2 WHERE provider_attempt_id=$1 AND status='INTENT'", attempt.ProviderAttemptID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, "INSERT INTO payment_provider_health_observations(id,provider_attempt_id,provider_id,provider_version,capability,channel,route,logical_order_id,product_id,subject_hash,trusted_probe,outcome,classification_rule_version,evidence_digest,received_at) SELECT $1,provider_attempt_id,provider_id,provider_version,capability,channel,route,logical_order_id,product_id,subject_hash,trusted_probe,$2,'payment-adapter-outcome/v1',$3,$4 FROM payment_provider_attempts WHERE provider_attempt_id=$5 ON CONFLICT(provider_attempt_id) DO NOTHING", uuid.Must(uuid.NewV7()), outcome, hex.EncodeToString(digest[:]), now, attempt.ProviderAttemptID)
	if err != nil {
		return err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.payment-provider.health-observed.v1", attempt.ID, 1, map[string]any{"provider_attempt_id": attempt.ProviderAttemptID, "provider_id": attempt.ProviderID, "provider_version": attempt.ProviderVersion, "capability": attempt.Capability, "channel": attempt.Channel, "route": attempt.Route, "outcome": outcome}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type providerStopLossCandidate struct {
	ProviderID, ProviderVersion string
	Capability, Channel, Route  string
	TriggerType                 string
	WindowStart, WindowEnd      time.Time
	ObservationIDs              []uuid.UUID
	CaseIDs                     []uuid.UUID
	DistinctOrders              int
	DistinctSubjects            int
	FailureCount, SampleCount   int
}

func (r *PostgresRepository) observationCandidates(ctx context.Context, now time.Time) ([]providerStopLossCandidate, error) {
	rows, err := r.pool.Query(ctx, "SELECT DISTINCT provider_id,provider_version,capability,channel,route FROM payment_provider_health_observations WHERE capability IN ('ORDER_CREATE','ORDER_QUERY','PAYMENT_STATUS_QUERY') AND received_at>$1::timestamptz-interval '5 minutes'", now)
	if err != nil {
		return nil, err
	}
	type partition struct{ providerID, providerVersion, capability, channel, route string }
	partitions := make([]partition, 0)
	for rows.Next() {
		var item partition
		if err := rows.Scan(&item.providerID, &item.providerVersion, &item.capability, &item.channel, &item.route); err != nil {
			rows.Close()
			return nil, err
		}
		partitions = append(partitions, item)
	}
	rows.Close()
	candidates := make([]providerStopLossCandidate, 0)
	for _, partition := range partitions {
		lastRows, err := r.pool.Query(ctx, "SELECT id,outcome,logical_order_id FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND capability=$3 AND channel=$4 AND route=$5 AND outcome<>'BUSINESS_REJECTION' AND received_at>$6::timestamptz-interval '5 minutes' ORDER BY received_at DESC,id DESC LIMIT 5", partition.providerID, partition.providerVersion, partition.capability, partition.channel, partition.route, now)
		if err != nil {
			return nil, err
		}
		lastIDs := make([]uuid.UUID, 0, 5)
		orders := map[uuid.UUID]struct{}{}
		allFailures := true
		for lastRows.Next() {
			var id uuid.UUID
			var outcome string
			var orderID *uuid.UUID
			if err := lastRows.Scan(&id, &outcome, &orderID); err != nil {
				lastRows.Close()
				return nil, err
			}
			lastIDs = append(lastIDs, id)
			if orderID != nil {
				orders[*orderID] = struct{}{}
			}
			allFailures = allFailures && (outcome == "INFRASTRUCTURE_FAILURE" || outcome == "UNCERTAIN_RESULT")
		}
		lastRows.Close()
		if len(lastIDs) == 5 && allFailures && len(orders) >= 3 {
			candidates = append(candidates, providerStopLossCandidate{ProviderID: partition.providerID, ProviderVersion: partition.providerVersion, Capability: partition.capability, Channel: partition.channel, Route: partition.route, TriggerType: "CONSECUTIVE_FAILURES", WindowStart: now.Add(-5 * time.Minute), WindowEnd: now, ObservationIDs: lastIDs, DistinctOrders: len(orders), FailureCount: 5, SampleCount: 5})
			continue
		}
		rateRows, err := r.pool.Query(ctx, "SELECT id,outcome,logical_order_id FROM payment_provider_health_observations WHERE provider_id=$1 AND provider_version=$2 AND capability=$3 AND channel=$4 AND route=$5 AND outcome<>'BUSINESS_REJECTION' AND received_at>$6::timestamptz-interval '1 minute' ORDER BY received_at,id", partition.providerID, partition.providerVersion, partition.capability, partition.channel, partition.route, now)
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, 0)
		failures := 0
		orders = map[uuid.UUID]struct{}{}
		for rateRows.Next() {
			var id uuid.UUID
			var outcome string
			var orderID *uuid.UUID
			if err := rateRows.Scan(&id, &outcome, &orderID); err != nil {
				rateRows.Close()
				return nil, err
			}
			ids = append(ids, id)
			if outcome == "INFRASTRUCTURE_FAILURE" || outcome == "UNCERTAIN_RESULT" {
				failures++
			}
			if orderID != nil {
				orders[*orderID] = struct{}{}
			}
		}
		rateRows.Close()
		if len(ids) >= 20 && failures*2 >= len(ids) && len(orders) >= 5 {
			candidates = append(candidates, providerStopLossCandidate{ProviderID: partition.providerID, ProviderVersion: partition.providerVersion, Capability: partition.capability, Channel: partition.channel, Route: partition.route, TriggerType: "FAILURE_RATE", WindowStart: now.Add(-time.Minute), WindowEnd: now, ObservationIDs: ids, DistinctOrders: len(orders), FailureCount: failures, SampleCount: len(ids)})
		}
	}
	return candidates, nil
}

func (r *PostgresRepository) crossProductCandidates(ctx context.Context, now time.Time) ([]providerStopLossCandidate, error) {
	rows, err := r.pool.Query(ctx, "SELECT evaluation.provider_id,evaluation.provider_version,anomaly.anomaly_type,array_agg(DISTINCT anomaly.id),COUNT(DISTINCT evaluation.payment_product_id),COUNT(DISTINCT payment.id),COUNT(DISTINCT payment.requester_id) FROM manual_review_stop_loss_evaluations evaluation JOIN payment_anomaly_cases anomaly ON anomaly.id=evaluation.case_id JOIN payment_orders payment ON payment.id=anomaly.payment_order_id WHERE evaluation.evaluated_at>$1::timestamptz-interval '15 minutes' AND anomaly.status='OPEN' AND COALESCE((evaluation.action_snapshot->>'payment_product_paused')::boolean,FALSE) GROUP BY evaluation.provider_id,evaluation.provider_version,anomaly.anomaly_type HAVING COUNT(DISTINCT evaluation.payment_product_id)>=2 AND COUNT(DISTINCT payment.id)>=3 AND COUNT(DISTINCT payment.requester_id)>=2", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]providerStopLossCandidate, 0)
	for rows.Next() {
		var candidate providerStopLossCandidate
		var signature string
		var products int
		if err := rows.Scan(&candidate.ProviderID, &candidate.ProviderVersion, &signature, &candidate.CaseIDs, &products, &candidate.DistinctOrders, &candidate.DistinctSubjects); err != nil {
			return nil, err
		}
		candidate.TriggerType = "CROSS_PRODUCT_COMMON_FAILURE"
		candidate.Capability = "ORDER_CREATE"
		candidate.Route = signature
		candidate.WindowStart, candidate.WindowEnd = now.Add(-15*time.Minute), now
		candidate.FailureCount, candidate.SampleCount = len(candidate.CaseIDs), len(candidate.CaseIDs)
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func (r *PostgresRepository) activeProviderRoutes(ctx context.Context, providerID, providerVersion string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(DISTINCT channel) FROM payment_product_mappings mapping CROSS JOIN LATERAL unnest(mapping.channels) channel WHERE mapping.provider_id=$1 AND mapping.provider_version=$2 AND mapping.status IN ('FROZEN','TEST_ONLY') AND mapping.effective_from<=now() AND (mapping.effective_to IS NULL OR mapping.effective_to>now())", providerID, providerVersion).Scan(&count)
	return count, err
}

func candidateDedupeKey(candidate providerStopLossCandidate, controlVersion int64) string {
	members := make([]string, 0, len(candidate.ObservationIDs)+len(candidate.CaseIDs))
	for _, id := range candidate.ObservationIDs {
		members = append(members, id.String())
	}
	for _, id := range candidate.CaseIDs {
		members = append(members, id.String())
	}
	sort.Strings(members)
	parts := append([]string{candidate.ProviderID, candidate.ProviderVersion, candidate.TriggerType, paymentStopLossPolicyVersion, strconv.FormatInt(controlVersion, 10)}, members...)
	digest := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) applyProviderStopLoss(ctx context.Context, candidate providerStopLossCandidate) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-provider.stop-loss", candidate.ProviderID+":"+candidate.ProviderVersion); err != nil {
		return false, err
	}
	var enabled bool
	var controlVersion int64
	err = tx.QueryRow(ctx, "SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE", candidate.ProviderID, candidate.ProviderVersion).Scan(&enabled, &controlVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		enabled, controlVersion, err = true, 0, nil
	}
	if err != nil {
		return false, err
	}
	dedupeKey := candidateDedupeKey(candidate, controlVersion)
	evaluationID := uuid.Must(uuid.NewV7())
	metrics, _ := json.Marshal(map[string]any{"capability": candidate.Capability, "channel": candidate.Channel, "route": candidate.Route, "sample_count": candidate.SampleCount, "failure_count": candidate.FailureCount, "distinct_orders": candidate.DistinctOrders, "distinct_subjects": candidate.DistinctSubjects})
	var highWater *uuid.UUID
	if len(candidate.ObservationIDs) > 0 {
		highWater = &candidate.ObservationIDs[len(candidate.ObservationIDs)-1]
	}
	command, err := tx.Exec(ctx, "INSERT INTO payment_stop_loss_evaluations(id,scope_type,provider_id,provider_version,trigger_type,policy_version,control_version_before,window_start,window_end,observation_high_watermark,metrics,dedupe_key,action_applied,evaluated_at) VALUES($1,'PROVIDER',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$8) ON CONFLICT(dedupe_key) DO NOTHING", evaluationID, candidate.ProviderID, candidate.ProviderVersion, candidate.TriggerType, paymentStopLossPolicyVersion, controlVersion, candidate.WindowStart, candidate.WindowEnd, highWater, metrics, dedupeKey, enabled)
	if err != nil || command.RowsAffected() == 0 {
		return false, err
	}
	for _, observationID := range candidate.ObservationIDs {
		if _, err := tx.Exec(ctx, "INSERT INTO payment_stop_loss_evaluation_members(id,evaluation_id,observation_id) VALUES($1,$2,$3)", uuid.Must(uuid.NewV7()), evaluationID, observationID); err != nil {
			return false, err
		}
	}
	for _, caseID := range candidate.CaseIDs {
		if _, err := tx.Exec(ctx, "INSERT INTO payment_stop_loss_evaluation_members(id,evaluation_id,case_id) VALUES($1,$2,$3)", uuid.Must(uuid.NewV7()), evaluationID, caseID); err != nil {
			return false, err
		}
	}
	reason := "provider persistent stop-loss threshold reached: " + candidate.TriggerType
	if controlVersion == 0 {
		_, err = tx.Exec(ctx, "INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,reason_code,updated_at) VALUES($1,$2,FALSE,$3,'AUTOMATIC_STOP_LOSS',$4)", candidate.ProviderID, candidate.ProviderVersion, reason, candidate.WindowEnd)
	} else if enabled {
		_, err = tx.Exec(ctx, "UPDATE payment_provider_controls SET new_orders_enabled=FALSE,reason=$3,reason_code='AUTOMATIC_STOP_LOSS',version=version+1,updated_at=$4 WHERE provider_id=$1 AND provider_version=$2", candidate.ProviderID, candidate.ProviderVersion, reason, candidate.WindowEnd)
	}
	if err != nil {
		return false, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.payment-provider.stop-loss-applied.v1", evaluationID, 1, map[string]any{"evaluation_id": evaluationID, "provider_id": candidate.ProviderID, "provider_version": candidate.ProviderVersion, "trigger_type": candidate.TriggerType, "policy_version": paymentStopLossPolicyVersion}); err != nil {
		return false, err
	}
	return enabled, tx.Commit(ctx)
}

func (r *PostgresRepository) EvaluateProviderStopLosses(ctx context.Context, limit int) (int, error) {
	now := r.now()
	candidates, err := r.observationCandidates(ctx, now)
	if err != nil {
		return 0, err
	}
	crossProduct, err := r.crossProductCandidates(ctx, now)
	if err != nil {
		return 0, err
	}
	candidates = append(candidates, crossProduct...)
	if limit < 1 || limit > 500 {
		limit = 100
	}
	count := 0
	for _, candidate := range candidates {
		if count >= limit {
			break
		}
		if candidate.TriggerType != "CROSS_PRODUCT_COMMON_FAILURE" {
			routes, err := r.activeProviderRoutes(ctx, candidate.ProviderID, candidate.ProviderVersion)
			if err != nil {
				return count, err
			}
			if routes > 1 {
				independent := false
				for _, other := range candidates {
					if other.ProviderID == candidate.ProviderID && other.ProviderVersion == candidate.ProviderVersion && (other.Route != candidate.Route || other.Capability != candidate.Capability) {
						independent = true
						break
					}
				}
				if !independent {
					continue
				}
			}
		}
		applied, err := r.applyProviderStopLoss(ctx, candidate)
		if err != nil {
			return count, err
		}
		if applied {
			count++
		}
	}
	return count, nil
}
