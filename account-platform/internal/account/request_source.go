package account

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type trustedSourceKey struct{}

// NewTrustedSourceHandler trusts only explicitly configured immediate peers,
// never caller-supplied forwarding headers or a whole private subnet.
func NewTrustedSourceHandler(next http.Handler, rawPeers string) (http.Handler, error) {
	peers := make(map[netip.Addr]struct{})
	if strings.TrimSpace(rawPeers) != "" {
		for _, item := range strings.Split(rawPeers, ",") {
			peer, err := netip.ParseAddr(strings.TrimSpace(item))
			if err != nil || peer.Zone() != "" || peer.IsUnspecified() || peer.IsMulticast() {
				return nil, errors.New("ACCOUNT_TRUSTED_SOURCE_PEER_IPS must contain exact unicast IP addresses")
			}
			peers[peer.Unmap()] = struct{}{}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source := directRequestSource(r)
		peer, err := netip.ParseAddr(source)
		if err == nil {
			if _, trusted := peers[peer.Unmap()]; trusted {
				values := r.Header.Values("X-Pinvou-Client-Source")
				if len(values) == 1 {
					candidate, err := netip.ParseAddr(strings.TrimSpace(values[0]))
					if err == nil && candidate.Zone() == "" && !candidate.IsUnspecified() && !candidate.IsMulticast() {
						source = candidate.Unmap().String()
					}
				}
			}
		}
		ctx := context.WithValue(r.Context(), trustedSourceKey{}, source)
		next.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func requestSource(r *http.Request) string {
	if source, ok := r.Context().Value(trustedSourceKey{}).(string); ok {
		return source
	}
	return directRequestSource(r)
}

func directRequestSource(r *http.Request) string {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		if peer, err := netip.ParseAddr(host); err == nil && peer.Zone() == "" {
			return peer.Unmap().String()
		}
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
