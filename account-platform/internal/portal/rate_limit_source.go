package portal

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type rateLimitSourceContextKey struct{}

// Keep admission hints separate from the existing path-specific business source
// and HMAC protocol. Browser headers are never copied to internal requests.
func (s *Service) withRateLimitSource(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source, ok := httpserver.AdmittedRateLimitSource(r.Context())
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), rateLimitSourceContextKey{}, source))
		}
		next.ServeHTTP(w, r)
	})
}

// Called only for the existing Account/Billing/Token BFF API requests, never
// OIDC, third-party probes, background calls or arbitrary client destinations.
func setRateLimitSource(request *http.Request) {
	request.Header.Del(httpserver.RateLimitSourceHeader)
	if source, ok := request.Context().Value(rateLimitSourceContextKey{}).(string); ok {
		request.Header.Set(httpserver.RateLimitSourceHeader, source)
	}
}
