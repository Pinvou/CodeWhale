package account

import (
	"net"
	"net/http"
	"strings"
)

// requestSource preserves the Portal's validated source address and otherwise
// binds the SMS risk dimensions to the direct peer.
func requestSource(r *http.Request) string {
	if source := strings.TrimSpace(r.Header.Get("X-Pinvou-Client-Source")); net.ParseIP(source) != nil {
		return source
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
