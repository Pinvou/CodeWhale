package billing

import "github.com/prometheus/client_golang/prometheus"

type modelPriceProjectionMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newModelPriceProjectionMetrics() *modelPriceProjectionMetrics {
	return &modelPriceProjectionMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_billing_model_price_projection_requests_total", Help: "Billing model price projection requests by outcome."}, []string{"outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pinvou_billing_model_price_projection_duration_seconds", Help: "Billing model price projection request duration."}, []string{"outcome"}),
	}
}

func (s *Service) AdminModelPriceProjectionCollectors() []prometheus.Collector {
	if s == nil || s.modelPriceProjectionMetrics == nil {
		return nil
	}
	return []prometheus.Collector{s.modelPriceProjectionMetrics.requests, s.modelPriceProjectionMetrics.duration}
}
