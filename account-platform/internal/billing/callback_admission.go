package billing

import (
	"net"
	"net/http"
	"net/netip"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

// Callback evidence uses the same resolved ingress source as admission. Other
// user/payment source protocols and retained notification facts stay unchanged.
func callbackRequestSource(r *http.Request) string {
	if source, ok := httpserver.AdmittedRateLimitSource(r.Context()); ok {
		return source
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if peer, err := netip.ParseAddr(host); err == nil && peer.Zone() == "" {
			return peer.Unmap().String()
		}
	}
	return "source:unknown"
}

func (s *Service) callbackAdmission(r *http.Request) (httpserver.AdmissionClass, string) {
	if s.payments == nil || s.callbackAuth == nil || s.callbackProfiles == nil || r.Method != http.MethodPost {
		return httpserver.OrdinaryAdmission, ""
	}
	if r.URL.Path != "/api/v1/payment-callbacks" && r.URL.Path != "/api/v1/payment-callback/token" {
		return httpserver.OrdinaryAdmission, ""
	}
	if r.URL.Path == "/api/v1/payment-callback/token" {
		id, secret, ok := r.BasicAuth()
		if !ok {
			return httpserver.InvalidCallbackAdmission, ""
		}
		principal, err := s.callbackAuth.AuthenticateClient(id, secret)
		if err == nil {
			return httpserver.VerifiedCallbackTokenAdmission, principal.ClientID
		}
	} else {
		principal, err := s.callbackAuth.VerifyBearer(r.Header.Get("Authorization"))
		if err == nil {
			capabilities := s.payments.Capabilities()
			if principal.ProviderID == capabilities.ProviderID && principal.ProviderVersion == capabilities.Version {
				return httpserver.VerifiedCallbackAdmission, principal.ClientID
			}
		}
	}
	return httpserver.InvalidCallbackAdmission, ""
}
