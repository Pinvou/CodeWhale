package portal

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxyIPs accepts only explicit single IP identities, never CIDR,
// hostnames, wildcards or a default private-network trust policy.
func ParseTrustedProxyIPs(raw string) ([]netip.Addr, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var addresses []netip.Addr
	for _, item := range strings.Split(raw, ",") {
		address, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || address.Zone() != "" {
			return nil, errors.New("PORTAL_TRUSTED_PROXY_IPS must contain only comma-separated exact IP addresses")
		}
		addresses = append(addresses, address.Unmap())
	}
	return addresses, nil
}

func trustedProxySet(addresses []netip.Addr) (map[netip.Addr]struct{}, error) {
	result := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		if !address.IsValid() || address.Zone() != "" {
			return nil, errors.New("PORTAL_TRUSTED_PROXY_IPS contains an invalid IP address")
		}
		result[address.Unmap()] = struct{}{}
	}
	return result, nil
}

// Only the configured immediate edge can supply its strongly overwritten
// single client address. This is called only for existing Account risk routes;
// browser XFF/RealIP/authority headers remain stripped by forward.
func (s *Service) clientRiskSource(request *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	peer, parseErr := netip.ParseAddr(host)
	if err != nil || parseErr != nil || peer.Zone() != "" {
		return "", false
	}
	peer = peer.Unmap()
	if _, trusted := s.trustedProxyIPs[peer]; trusted {
		values := request.Header.Values("X-Pinvou-Client-Source")
		if len(values) == 1 {
			candidate, err := netip.ParseAddr(strings.TrimSpace(values[0]))
			if err == nil && candidate.Zone() == "" {
				return candidate.Unmap().String(), true
			}
		}
	}
	return peer.String(), true
}
