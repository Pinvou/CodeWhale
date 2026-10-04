package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

type requestIDContextKey struct{}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}

var (
	version = "dev"
	commit  = "unknown"
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

// HandlerWithApplication mounts a service handler behind the common health,
// request-correlation and cache-safety middleware. A readiness check should
// verify only dependencies required for safe request handling.
func HandlerWithApplication(cfg config.Config, application http.Handler, ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	status := func(state string, check func(context.Context) error) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			statusCode := http.StatusOK
			responseState := state
			if check != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := check(ctx); err != nil {
					responseState = "not_ready"
					statusCode = http.StatusServiceUnavailable
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
	mux.HandleFunc("GET /health/live", status("ok", nil))
	mux.HandleFunc("GET /health/ready", status("ready", ready))
	if application != nil {
		mux.Handle("/", application)
	}
	return withRequestID(mux)
}

func Run(serviceName string) error {
	return RunWithApplication(serviceName, nil, nil)
}

func RunWithApplication(serviceName string, application http.Handler, ready func(context.Context) error) error {
	cfg, err := config.Load(serviceName)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})).With(
		"service", cfg.ServiceName,
		"environment", cfg.Environment,
	)
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           HandlerWithApplication(cfg, application, ready),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("service listening", "address", cfg.Address)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		logger.Info("service shutting down")
		return server.Shutdown(shutdownCtx)
	}
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		// Event contracts share a 1..64 character trace ID. Preserve valid
		// caller correlation, and replace values the event envelope cannot carry.
		if requestID == "" || !utf8.ValidString(requestID) || utf8.RuneCountInString(requestID) > 64 {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID)))
	})
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(value[:])
}
