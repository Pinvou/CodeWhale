package httpserver

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type httpMetrics struct {
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inFlight  prometheus.Gauge
	readiness prometheus.Gauge
	registry  *prometheus.Registry
}

func newHTTPMetrics(service string, collectors ...prometheus.Collector) *httpMetrics {
	labels := prometheus.Labels{"service": service}
	metrics := &httpMetrics{
		requests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_http_requests_total", Help: "Completed account platform HTTP requests.", ConstLabels: labels}, []string{"method", "route", "status"}),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pinvou_http_request_duration_seconds", Help: "Account platform HTTP request latency.", ConstLabels: labels, Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2, 5}}, []string{"method", "route"}),
		inFlight:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_http_in_flight_requests", Help: "Account platform HTTP requests currently executing.", ConstLabels: labels}),
		readiness: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_service_readiness", Help: "Whether the service's critical request dependencies are ready.", ConstLabels: labels}),
		registry:  prometheus.NewRegistry(),
	}
	metrics.registry.MustRegister(metrics.requests, metrics.duration, metrics.inFlight, metrics.readiness)
	if len(collectors) > 0 {
		metrics.registry.MustRegister(collectors...)
	}
	return metrics
}

func (metrics *httpMetrics) handler() http.Handler {
	return promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func (metrics *httpMetrics) middleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		metrics.inFlight.Inc()
		defer metrics.inFlight.Dec()
		wrapped := &metricResponseWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			status := strconv.Itoa(wrapped.status)
			method := metricMethod(r.Method)
			requestCounter := metrics.requests.WithLabelValues(method, route, status)
			durationObserver := metrics.duration.WithLabelValues(method, route)
			elapsed := time.Since(start)
			traceID := TraceID(r.Context())
			if traceID != "" {
				requestCounter.(prometheus.ExemplarAdder).AddWithExemplar(1, prometheus.Labels{"trace_id": traceID})
				durationObserver.(prometheus.ExemplarObserver).ObserveWithExemplar(elapsed.Seconds(), prometheus.Labels{"trace_id": traceID})
			} else {
				requestCounter.Inc()
				durationObserver.Observe(elapsed.Seconds())
			}
			logger.InfoContext(r.Context(), "http request completed", "method", r.Method, "route", route, "status", wrapped.status, "duration_ms", elapsed.Milliseconds())
		}()
		next.ServeHTTP(wrapped, r)
	})
}

// Arbitrary HTTP method tokens must not create unbounded metric dimensions.
func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

type metricResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (writer *metricResponseWriter) WriteHeader(status int) {
	if writer.wroteHeader {
		return
	}
	writer.status, writer.wroteHeader = status, true
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *metricResponseWriter) Write(body []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}

func (writer *metricResponseWriter) Flush() {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *metricResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (writer *metricResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := writer.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (writer *metricResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := writer.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}
	return io.Copy(writer.ResponseWriter, reader)
}

func (writer *metricResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }
