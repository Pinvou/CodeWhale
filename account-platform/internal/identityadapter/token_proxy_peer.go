package identityadapter

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// NewTokenProxyWithTrustedPeers adds only exact immediate-peer source trust.
// The existing constructors trust no forwarding headers by default.
func NewTokenProxyWithTrustedPeers(endpoint string, admission tokenAdmission, caFile string, production bool, rawPeers string) (*TokenProxy, error) {
	peers, err := parseTrustedTokenProxyIPs(rawPeers)
	if err != nil {
		return nil, err
	}
	proxy, err := NewTokenProxyWithTrust(endpoint, admission, caFile, production)
	if err != nil {
		return nil, err
	}
	proxy.trustedProxyIPs = peers
	return proxy, nil
}

func parseTrustedTokenProxyIPs(raw string) (map[netip.Addr]struct{}, error) {
	peers := make(map[netip.Addr]struct{})
	if strings.TrimSpace(raw) == "" {
		return peers, nil
	}
	for _, value := range strings.Split(raw, ",") {
		peer, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || peer.Zone() != "" {
			return nil, errors.New("IDENTITY_TRUSTED_PROXY_IPS must contain only exact IP addresses without zones")
		}
		peers[peer.Unmap()] = struct{}{}
	}
	return peers, nil
}

func (p *TokenProxy) tokenClientSource(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return "", false
	}
	peer = peer.Unmap()
	if _, trusted := p.trustedProxyIPs[peer]; trusted {
		// The configured edge overwrites XFF with one connection address. Neither
		// caller chains nor duplicate header fields establish an admitted source.
		values := r.Header.Values("X-Forwarded-For")
		if len(values) == 1 {
			source, err := netip.ParseAddr(strings.TrimSpace(values[0]))
			if err == nil && source.Zone() == "" {
				return source.Unmap().String(), true
			}
		}
	}
	return peer.String(), true
}
