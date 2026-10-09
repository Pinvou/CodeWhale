package token

import "github.com/prometheus/client_golang/prometheus"

type tokenOperationsOverviewMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newTokenOperationsOverviewMetrics() *tokenOperationsOverviewMetrics {
	return &tokenOperationsOverviewMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_token_operations_overview_requests_total", Help: "Token operations overview requests by outcome."}, []string{"outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pinvou_token_operations_overview_duration_seconds", Help: "Token operations overview request duration."}, []string{"outcome"}),
	}
}

func (s *Service) AdminOperationsOverviewCollectors() []prometheus.Collector {
	if s == nil || s.operationsOverviewMetrics == nil {
		return nil
	}
	return []prometheus.Collector{s.operationsOverviewMetrics.requests, s.operationsOverviewMetrics.duration}
}
