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
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
	registry *prometheus.Registry
}

func newHTTPMetrics(service string) *httpMetrics {
	labels := prometheus.Labels{"service": service}
	metrics := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_http_requests_total", Help: "Completed account platform HTTP requests.", ConstLabels: labels}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "pinvou_http_request_duration_seconds", Help: "Account platform HTTP request latency.", ConstLabels: labels, Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2, 5}}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_http_in_flight_requests", Help: "Account platform HTTP requests currently executing.", ConstLabels: labels}),
		registry: prometheus.NewRegistry(),
	}
	metrics.registry.MustRegister(metrics.requests, metrics.duration, metrics.inFlight)
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
		next.ServeHTTP(wrapped, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		status := strconv.Itoa(wrapped.status)
		method := metricMethod(r.Method)
		metrics.requests.WithLabelValues(method, route, status).Inc()
		metrics.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		logger.Info("http request completed", "method", r.Method, "route", route, "status", wrapped.status, "duration_ms", time.Since(start).Milliseconds(), "request_id", RequestID(r.Context()), "trace_id", TraceID(r.Context()))
	})
}

// HTTP permits arbitrary method tokens. Keep the metric dimension bounded
// without changing the method delivered to the application or request logs.
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
