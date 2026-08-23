package httpserver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func RequestID(ctx context.Context) string {
	return observability.RequestID(ctx)
}

func TraceID(ctx context.Context) string {
	if traceID := oteltrace.SpanContextFromContext(ctx).TraceID(); traceID.IsValid() {
		return traceID.String()
	}
	return observability.TraceID(ctx)
}

var (
	version            = "dev"
	commit             = "unknown"
	tracingPreparation struct {
		sync.Mutex
		service  string
		shutdown observability.Shutdown
	}
)

type statusResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func Handler(cfg config.Config) http.Handler {
	return HandlerWithApplication(cfg, nil, nil)
}

func CorrelationHandler(next http.Handler) http.Handler { return withRequestID(next) }

// HandlerWithApplication mounts a service handler behind the common health,
// request-correlation and cache-safety middleware. A readiness check should
// verify only dependencies required for safe request handling.
func HandlerWithApplication(cfg config.Config, application http.Handler, ready func(context.Context) error) http.Handler {
	handler, _ := handlerWithMetrics(cfg, application, ready)
	return handler
}

func handlerWithMetrics(cfg config.Config, application http.Handler, ready func(context.Context) error) (http.Handler, *httpMetrics) {
	health := NewRouter()
	metrics := newHTTPMetrics(cfg.ServiceName)
	status := func(state string, check func(context.Context) error) http.HandlerFunc {
		return func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			statusCode := http.StatusOK
			responseState := state
			if check != nil {
				ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
				defer cancel()
				if err := check(ctx); err != nil {
					responseState = "not_ready"
					statusCode = http.StatusServiceUnavailable
				}
			}
			if state == "ready" {
				if statusCode == http.StatusOK {
					metrics.readiness.Set(1)
				} else {
					metrics.readiness.Set(0)
				}
			}
			w.WriteHeader(statusCode)
			_ = json.NewEncoder(w).Encode(statusResponse{
				Service: cfg.ServiceName,
				Status:  responseState,
				Version: version,
				Commit:  commit,
			})
		}
	}
	health.GET("/health/live", AdaptFunc(status("ok", nil)))
	health.GET("/health/ready", AdaptFunc(status("ready", ready)))
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" || application == nil {
			health.ServeHTTP(w, r)
			return
		}
		application.ServeHTTP(w, r)
	})
	observed := metrics.middleware(withRateLimit(cfg, dispatch), newLogger(cfg))
	return observability.TraceHTTPHandler(withRequestID(withSecurityHeaders(cfg, observed))), metrics
}

func withSecurityHeaders(cfg config.Config, next http.Handler) http.Handler {
	production := cfg.Environment == "production" || cfg.Environment == "production-new"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		if production {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func Run(serviceName string) error {
	return RunWithApplication(serviceName, nil, nil)
}

func ConfigureProcessLogging(serviceName string) error {
	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	config := observability.Config{Service: serviceName, Environment: environment, Version: version, Commit: commit}
	observability.ConfigureDefaultLogger(config)
	shutdown, err := observability.SetupTracing(context.Background(), config)
	if err != nil {
		return err
	}
	tracingPreparation.Lock()
	tracingPreparation.service = serviceName
	tracingPreparation.shutdown = shutdown
	tracingPreparation.Unlock()
	return nil
}

func RunWithApplication(serviceName string, application http.Handler, ready func(context.Context) error) error {
	cfg, err := config.Load(serviceName)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tracingPreparation.Lock()
	shutdownTracing := observability.Shutdown(nil)
	if tracingPreparation.service == serviceName {
		shutdownTracing = tracingPreparation.shutdown
		tracingPreparation.service = ""
		tracingPreparation.shutdown = nil
	}
	tracingPreparation.Unlock()
	if shutdownTracing == nil {
		shutdownTracing, err = observability.SetupTracing(ctx, observability.Config{Service: cfg.ServiceName, Environment: cfg.Environment, Version: version, Commit: commit})
		if err != nil {
			return err
		}
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	applicationHandler, metrics := handlerWithMetrics(cfg, application, ready)
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           applicationHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsAddress := strings.TrimSpace(os.Getenv("METRICS_ADDRESS"))
	if metricsAddress == "" {
		metricsAddress = ":9090"
	}
	metricsServer := &http.Server{Addr: metricsAddress, Handler: metrics.handler(), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second}

	type serverError struct {
		name string
		err  error
	}
	errCh := make(chan serverError, 2)
	go func() {
		logger.Info("service listening", "address", cfg.Address)
		errCh <- serverError{name: "application", err: server.ListenAndServe()}
	}()
	go func() {
		logger.Info("metrics listening", "address", metricsAddress)
		errCh <- serverError{name: "metrics", err: metricsServer.ListenAndServe()}
	}()

	select {
	case result := <-errCh:
		if errors.Is(result.err, http.ErrServerClosed) {
			return nil
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = metricsServer.Shutdown(shutdownCtx)
		return fmt.Errorf("%s server: %w", result.name, result.err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		logger.Info("service shutting down")
		applicationErr := server.Shutdown(shutdownCtx)
		metricsErr := metricsServer.Shutdown(shutdownCtx)
		return errors.Join(applicationErr, metricsErr)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	return observability.ConfigureDefaultLogger(observability.Config{Service: cfg.ServiceName, Environment: cfg.Environment, Version: version, Commit: commit})
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if !observability.ValidRequestID(requestID) {
			requestID = observability.NewCorrelationID()
		}
		traceID := TraceID(r.Context())
		if traceID == "" {
			traceID = traceIDFromParent(r.Header.Get("traceparent"))
		}
		if traceID == "" {
			traceID = observability.NewCorrelationID()
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Trace-ID", traceID)
		ctx := observability.WithCorrelation(r.Context(), requestID, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func traceIDFromParent(value string) string {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || len(parts[1]) != 32 || parts[1] == strings.Repeat("0", 32) {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return strings.ToLower(parts[1])
}
