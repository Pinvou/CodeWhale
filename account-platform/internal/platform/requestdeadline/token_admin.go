package requestdeadline

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const (
	AbsoluteDeadlineHeader = "X-Pinvou-Absolute-Deadline"
	DeadlineClassHeader    = "X-Pinvou-Deadline-Class"
)

type Class string

const (
	SnapshotBuild Class = "snapshot-build"
	SnapshotRead  Class = "snapshot-read"
	Precheck      Class = "precheck"
)

type Budget struct {
	Business time.Duration
	BFF      time.Duration
	Ingress  time.Duration
}

func BudgetFor(class Class) (Budget, bool) {
	switch class {
	case SnapshotBuild:
		return Budget{Business: 10 * time.Second, BFF: 12 * time.Second, Ingress: 14 * time.Second}, true
	case SnapshotRead:
		return Budget{Business: 3 * time.Second, BFF: 5 * time.Second, Ingress: 7 * time.Second}, true
	case Precheck:
		return Budget{Business: 5 * time.Second, BFF: 7 * time.Second, Ingress: 9 * time.Second}, true
	default:
		return Budget{}, false
	}
}

type Contract struct {
	Class            Class
	StartedAt        time.Time
	BusinessDeadline time.Time
	BFFDeadline      time.Time
	IngressDeadline  time.Time
}

func Start(start time.Time, class Class) (Contract, error) {
	budget, ok := BudgetFor(class)
	if !ok {
		return Contract{}, errors.New("unsupported Token admin deadline class")
	}
	start = start.UTC()
	return Contract{
		Class: class, StartedAt: start,
		BusinessDeadline: start.Add(budget.Business),
		BFFDeadline:      start.Add(budget.BFF),
		IngressDeadline:  start.Add(budget.Ingress),
	}, nil
}

func Classify(method, resource, cursor string) (Class, bool) {
	resource = strings.ToLower(strings.TrimSpace(resource))
	if !strings.HasPrefix(resource, "/admin/v1/") {
		return "", false
	}
	if strings.Contains(resource, "/precheck") || strings.Contains(resource, "/impact") {
		return Precheck, true
	}
	if method != http.MethodGet && method != http.MethodHead {
		return "", false
	}
	if strings.TrimSpace(cursor) != "" {
		return SnapshotRead, true
	}
	return SnapshotBuild, true
}

type contractContextKey struct{}
type deadlineClassContextKey struct{}

func WithContract(parent context.Context, contract Contract) (context.Context, context.CancelFunc) {
	ctx := context.WithValue(parent, contractContextKey{}, contract)
	return context.WithDeadline(ctx, contract.IngressDeadline)
}

func FromContext(ctx context.Context) (Contract, bool) {
	contract, ok := ctx.Value(contractContextKey{}).(Contract)
	return contract, ok
}

func ApplyToUpstream(request *http.Request, contract Contract) {
	request.Header.Del(AbsoluteDeadlineHeader)
	request.Header.Del(DeadlineClassHeader)
	request.Header.Set(AbsoluteDeadlineHeader, contract.BusinessDeadline.Format(time.RFC3339Nano))
	request.Header.Set(DeadlineClassHeader, string(contract.Class))
}

// ConsumeBusinessDeadline accepts only an earlier deadline. A missing or
// client-forged future value can never enlarge the budget measured from the
// service receive time.
func ConsumeBusinessDeadline(parent context.Context, header http.Header, now time.Time, fallback Class) (context.Context, context.CancelFunc, error) {
	class := fallback
	if supplied := Class(strings.TrimSpace(header.Get(DeadlineClassHeader))); supplied != "" {
		if supplied == fallback {
			class = supplied
		}
	}
	budget, ok := BudgetFor(class)
	if !ok {
		return nil, nil, errors.New("invalid Token admin deadline class")
	}
	deadline := now.UTC().Add(budget.Business)
	if raw := strings.TrimSpace(header.Get(AbsoluteDeadlineHeader)); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err == nil && parsed.Before(deadline) {
			deadline = parsed
		}
	}
	if existing, ok := parent.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	if !deadline.After(now) {
		return nil, nil, context.DeadlineExceeded
	}
	ctx := context.WithValue(parent, deadlineClassContextKey{}, class)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	return ctx, cancel, nil
}

// TokenAdminMiddleware is the service-side guard. It removes the transport
// headers before application code and places the one business deadline on the
// request context; a later layer can only shorten it.
func TokenAdminMiddleware(next http.Handler, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class, applies := Classify(r.Method, r.URL.Path, r.URL.Query().Get("cursor"))
		if !applies {
			r.Header.Del(AbsoluteDeadlineHeader)
			r.Header.Del(DeadlineClassHeader)
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel, err := ConsumeBusinessDeadline(r.Context(), r.Header, now().UTC(), class)
		r.Header.Del(AbsoluteDeadlineHeader)
		r.Header.Del(DeadlineClassHeader)
		if err != nil {
			writeDeadlineError(w, r, class)
			return
		}
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeDeadlineError(w http.ResponseWriter, r *http.Request, class Class) {
	requestID := httpserver.RequestID(r.Context())
	switch class {
	case SnapshotBuild:
		w.Header().Set("Retry-After", "5")
		api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "the pagination snapshot could not be built in time", requestID)
	case Precheck:
		api.WriteError(w, http.StatusGatewayTimeout, "PRECHECK_TIMEOUT", "the impact precheck timed out", requestID)
	default:
		api.WriteError(w, http.StatusGatewayTimeout, "QUERY_TIMEOUT", "the pagination query timed out", requestID)
	}
}

// WriteContextDeadlineError lets a cancellable Token handler translate the
// one deadline installed by TokenAdminMiddleware into the stable error for
// that request class. It returns false outside the Token-admin deadline chain.
func WriteContextDeadlineError(w http.ResponseWriter, r *http.Request) bool {
	class, ok := r.Context().Value(deadlineClassContextKey{}).(Class)
	if !ok {
		return false
	}
	writeDeadlineError(w, r, class)
	return true
}
