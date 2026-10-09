package token

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

const routeHealthyTTL = 5 * time.Minute

type RouteHealthMetrics struct {
	probes   *prometheus.CounterVec
	state    *prometheus.GaugeVec
	backlog  prometheus.Gauge
	lag      prometheus.Gauge
	inflight prometheus.Gauge
}

func newRouteHealthMetrics() *RouteHealthMetrics {
	return &RouteHealthMetrics{
		probes:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_token_route_health_probes_total", Help: "Server route health probe outcomes."}, []string{"outcome"}),
		state:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "pinvou_token_route_health_routes", Help: "Current stable routes by server health state."}, []string{"state"}),
		backlog:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_route_health_backlog", Help: "Stable routes currently due for a health probe."}),
		lag:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_route_health_oldest_due_seconds", Help: "Database-authoritative age of the oldest due route health probe."}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_route_health_inflight", Help: "Health probes currently executing in this process."}),
	}
}

type routeProbeTarget struct {
	RouteID        uuid.UUID
	AccountID      uuid.UUID
	CredentialID   uuid.UUID
	RouteVersion   int64
	HealthVersion  int64
	FailureCount   int
	UpstreamModel  string
	Provider       string
	AdapterVersion string
}

type RouteHealthProbeAdapter interface {
	ModelCatalogAdapter
	AdapterVersion() string
}

type RouteHealthWorker struct {
	repository  *PostgresRepository
	probe       RouteHealthProbeAdapter
	policy      ProviderURLPolicy
	workerID    string
	interval    time.Duration
	concurrency int
}

func NewRouteHealthWorker(repository *PostgresRepository, probe RouteHealthProbeAdapter, policy ProviderURLPolicy, interval time.Duration) (*RouteHealthWorker, error) {
	if repository == nil || probe == nil {
		return nil, ErrRouteReferenceInvalid
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &RouteHealthWorker{repository: repository, probe: probe, policy: policy, workerID: uuid.Must(uuid.NewV7()).String(), interval: interval, concurrency: 4}, nil
}

func (w *RouteHealthWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	completed := make(chan struct{}, w.concurrency)
	inflight := 0
	dispatch := func() {
		for inflight < w.concurrency {
			target, ok := w.claim(ctx)
			if !ok {
				break
			}
			inflight++
			if w.repository.routeHealthMetrics != nil {
				w.repository.routeHealthMetrics.inflight.Set(float64(inflight))
			}
			go func(target routeProbeTarget) {
				w.execute(ctx, target)
				select {
				case completed <- struct{}{}:
				case <-ctx.Done():
				}
			}(target)
		}
	}
	dispatch()
	for {
		select {
		case <-ctx.Done():
			return
		case <-completed:
			inflight--
			if w.repository.routeHealthMetrics != nil {
				w.repository.routeHealthMetrics.inflight.Set(float64(inflight))
			}
			dispatch()
		case <-ticker.C:
			dispatch()
		}
	}
}

func (w *RouteHealthWorker) claim(ctx context.Context) (routeProbeTarget, bool) {
	tx, err := w.repository.pool.Begin(ctx)
	if err != nil {
		return routeProbeTarget{}, false
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return routeProbeTarget{}, false
	}
	stale, err := tx.Exec(ctx, `UPDATE routing_policies SET health_status='STALE',health_diagnostic_code='HEALTH_EVIDENCE_EXPIRED',health_expires_at=NULL,next_probe_at=$1,health_version=health_version+1,updated_at=$1 WHERE reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at<=$1 AND (probe_lease_until IS NULL OR probe_lease_until<$1)`, now)
	if err != nil {
		return routeProbeTarget{}, false
	}
	if changed := stale.RowsAffected(); changed > 0 && w.repository.routeHealthMetrics != nil {
		w.repository.routeHealthMetrics.probes.WithLabelValues("expired").Add(float64(changed))
	}
	var backlog int64
	var oldest *time.Time
	if err = tx.QueryRow(ctx, `SELECT count(*),min(COALESCE(next_probe_at,health_expires_at,updated_at)) FROM routing_policies WHERE reference_status='RESOLVED' AND (probe_lease_until IS NULL OR probe_lease_until<$1) AND (next_probe_at IS NULL OR next_probe_at<=$1 OR health_expires_at<=$1)`, now).Scan(&backlog, &oldest); err != nil {
		return routeProbeTarget{}, false
	}
	if w.repository.routeHealthMetrics != nil {
		w.repository.routeHealthMetrics.backlog.Set(float64(backlog))
		lag := float64(0)
		if oldest != nil && oldest.Before(now) {
			lag = now.Sub(*oldest).Seconds()
		}
		w.repository.routeHealthMetrics.lag.Set(lag)
	}
	var target routeProbeTarget
	err = tx.QueryRow(ctx, `SELECT route.id,route.token_account_id,credential.id,route.route_version,route.health_version,route.health_failure_count,identity.upstream_model_ref,identity.provider_id,identity.adapter_version
FROM routing_policies route
JOIN model_identities identity ON identity.id=route.model_id
JOIN LATERAL (
    SELECT candidate.id FROM provider_credentials candidate
     WHERE candidate.token_account_id=route.token_account_id AND candidate.provider=identity.provider_id
       AND candidate.status='ACTIVE' AND candidate.validation_status='VALID'
       AND identity.upstream_model_ref=ANY(candidate.allowed_models)
     ORDER BY candidate.version DESC,candidate.id DESC LIMIT 1
) credential ON true
WHERE route.reference_status='RESOLVED'
  AND (route.probe_lease_until IS NULL OR route.probe_lease_until<$1)
  AND (route.next_probe_at IS NULL OR route.next_probe_at<=$1 OR route.health_expires_at<=$1)
ORDER BY route.next_probe_at NULLS FIRST,route.id
FOR UPDATE OF route SKIP LOCKED LIMIT 1`, now).Scan(&target.RouteID, &target.AccountID, &target.CredentialID, &target.RouteVersion, &target.HealthVersion, &target.FailureCount, &target.UpstreamModel, &target.Provider, &target.AdapterVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Commit(ctx)
		return routeProbeTarget{}, false
	}
	if err != nil {
		return routeProbeTarget{}, false
	}
	target.HealthVersion++
	lease := now.Add(30 * time.Second)
	command, err := tx.Exec(ctx, `UPDATE routing_policies SET health_status='PROBING',health_diagnostic_code='PROBE_IN_PROGRESS',health_expires_at=NULL,probe_lease_owner=$2,probe_lease_until=$3,health_version=$4,updated_at=$1 WHERE id=$5 AND route_version=$6 AND health_version=$7`, now, w.workerID, lease, target.HealthVersion, target.RouteID, target.RouteVersion, target.HealthVersion-1)
	if err != nil || command.RowsAffected() != 1 || tx.Commit(ctx) != nil {
		return routeProbeTarget{}, false
	}
	if w.repository.routeHealthMetrics != nil {
		w.repository.routeHealthMetrics.probes.WithLabelValues("claimed").Inc()
	}
	return target, true
}

func (w *RouteHealthWorker) execute(ctx context.Context, target routeProbeTarget) {
	started := time.Now()
	credential, secret, err := w.repository.CredentialSecret(ctx, target.AccountID, target.CredentialID)
	found := false
	diagnostic := "PROVIDER_UNAVAILABLE"
	if err == nil {
		defer clear(secret)
		safe := w.safeProbeTarget(credential, target)
		if !safe {
			diagnostic = "PROBE_TARGET_REJECTED"
		} else {
			probeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
			models, probeErr := w.probe.DiscoverModels(probeContext, credential.BaseURL, secret)
			cancel()
			if probeErr == nil {
				found = containsDiscoveredModel(models, target.UpstreamModel)
				diagnostic = "MODEL_NOT_DISCOVERED"
			} else if errors.Is(probeErr, context.DeadlineExceeded) || errors.Is(probeContext.Err(), context.DeadlineExceeded) {
				diagnostic = "PROBE_TIMEOUT"
			}
		}
	}
	if found {
		diagnostic = "PROBE_SUCCEEDED"
	}
	w.complete(ctx, target, found, diagnostic, time.Since(started))
}

func (w *RouteHealthWorker) safeProbeTarget(credential Credential, target routeProbeTarget) bool {
	return credential.Provider == target.Provider &&
		w.probe.AdapterVersion() == target.AdapterVersion &&
		w.policy.Validate(credential.BaseURL) == nil
}

func (w *RouteHealthWorker) complete(ctx context.Context, target routeProbeTarget, healthy bool, diagnostic string, duration time.Duration) {
	tx, err := w.repository.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return
	}
	status, failures := "HEALTHY", 0
	var expires any = now.Add(routeHealthyTTL)
	nextProbe := now.Add(4 * time.Minute)
	if !healthy {
		failures = target.FailureCount + 1
		status, expires = "DEGRADED", nil
		nextProbe = now.Add(time.Duration(min(failures, 5)) * 30 * time.Second)
		if failures >= 3 {
			status = "UNHEALTHY"
		}
	}
	command, err := tx.Exec(ctx, `UPDATE routing_policies SET health_status=$1,health_diagnostic_code=$2,health_observed_at=$3,health_expires_at=$4,health_failure_count=$5,next_probe_at=$6,probe_lease_owner=NULL,probe_lease_until=NULL,updated_at=$3 WHERE id=$7 AND route_version=$8 AND health_version=$9 AND probe_lease_owner=$10`, status, diagnostic, now, expires, failures, nextProbe, target.RouteID, target.RouteVersion, target.HealthVersion, w.workerID)
	if err != nil || command.RowsAffected() != 1 {
		if w.repository.routeHealthMetrics != nil {
			w.repository.routeHealthMetrics.probes.WithLabelValues("stale_completion").Inc()
		}
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO route_health_observations(id,route_id,route_version,health_version,status,diagnostic_code,duration_ms,observed_at,expires_at,worker_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), target.RouteID, target.RouteVersion, target.HealthVersion, status, diagnostic, max(int64(0), duration.Milliseconds()), now, expires, w.workerID); err != nil {
		return
	}
	if tx.Commit(ctx) != nil {
		return
	}
	if w.repository.routeHealthMetrics != nil {
		w.repository.routeHealthMetrics.probes.WithLabelValues(strings.ToLower(status)).Inc()
	}
	_ = w.repository.refreshRouteHealthMetrics(ctx)
}

func (r *PostgresRepository) refreshRouteHealthMetrics(ctx context.Context) error {
	if r.routeHealthMetrics == nil {
		return nil
	}
	rows, err := r.pool.Query(ctx, `SELECT health_status,count(*) FROM routing_policies WHERE reference_status='RESOLVED' GROUP BY health_status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := map[string]float64{}
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		counts[status] = count
	}
	for _, status := range []string{"UNKNOWN", "PROBING", "HEALTHY", "DEGRADED", "UNHEALTHY", "STALE"} {
		r.routeHealthMetrics.state.WithLabelValues(status).Set(counts[status])
	}
	return rows.Err()
}

func containsDiscoveredModel(models []DiscoveredModel, target string) bool {
	for _, model := range models {
		if strings.TrimSpace(model.UpstreamModelRef) == target {
			return true
		}
	}
	return false
}
