package portal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

func tokenPageQuerySigningPath(method, resource string) bool {
	if method != http.MethodGet {
		return false
	}
	switch resource {
	case "/admin/v1/overview", "/admin/v1/overviews/model-catalog", "/admin/v1/overviews/token-resources", "/admin/v1/overviews/model-operations":
		return true
	}
	return false
}

// The source is derived from the current exact trusted-edge policy, never from
// a browser signature. Sign the final upstream URI, including its original query.
func (s *Service) signTokenPageQuery(request *http.Request, source string) bool {
	ip := net.ParseIP(source)
	if ip == nil || len(s.config.TokenAdminProxyHMACKey) < 32 {
		return false
	}
	canonical := ip.String()
	mac := hmac.New(sha256.New, s.config.TokenAdminProxyHMACKey)
	_, _ = mac.Write([]byte(request.Method + "\n" + request.URL.RequestURI() + "\n" + canonical))
	request.Header.Set("X-Pinvou-Client-Source", canonical)
	request.Header.Set("X-Pinvou-Client-Source-Signature", hex.EncodeToString(mac.Sum(nil)))
	return true
}

// Only create and provider-query handlers consume a trusted payment source.
// Product/order lists, balances, and close/claim operations retain their
// existing contracts and do not acquire a new key requirement.
func paymentSourceSigningPath(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "recharge" && parts[2] == "orders" {
		return method == http.MethodPost && len(parts) == 3 || method == http.MethodGet && len(parts) == 4 && pathUUID(parts[3])
	}
	if len(parts) >= 5 && parts[0] == "v1" && parts[1] == "organizations" && parts[2] != "" && parts[3] == "recharge" && parts[4] == "orders" {
		return method == http.MethodPost && len(parts) == 5 || method == http.MethodGet && len(parts) == 6 && pathUUID(parts[5])
	}
	return false
}
