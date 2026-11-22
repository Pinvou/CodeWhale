package httpserver

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RateLimitSourceHeader is exclusively an ingress admission hint. It grants no
// authentication or authorization authority. A configured proxy
// must overwrite it with exactly one validated address on every request.
const RateLimitSourceHeader = "X-Pinvou-RateLimit-Source"

type rateLimitSourceContextKey struct{}

// AdmittedRateLimitSource exposes only the outer limiter's resolved source to
// a BFF hop and callback source accounting. It is not an identity attestation.
func AdmittedRateLimitSource(ctx context.Context) (string, bool) {
	source, ok := ctx.Value(rateLimitSourceContextKey{}).(string)
	return source, ok
}

func withAdmittedRateLimitSource(r *http.Request, source string) *http.Request {
	if address, err := netip.ParseAddr(source); err == nil && address.Zone() == "" {
		return r.WithContext(context.WithValue(r.Context(), rateLimitSourceContextKey{}, address.Unmap().String()))
	}
	return r
}

func rateLimitSource(r *http.Request, trusted []netip.Addr) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return remoteHost(r.RemoteAddr)
	}
	peer = peer.Unmap()
	for _, configured := range trusted {
		if configured.IsValid() && configured.Zone() == "" && configured.Unmap() == peer {
			values := r.Header.Values(RateLimitSourceHeader)
			if len(values) == 1 {
				address, err := netip.ParseAddr(strings.TrimSpace(values[0]))
				if err == nil && address.Zone() == "" {
					return address.Unmap().String()
				}
			}
			break
		}
	}
	return peer.String()
}
