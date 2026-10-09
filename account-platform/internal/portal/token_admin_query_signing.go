package portal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
)

func tokenPageQuerySigningPath(method, resource string) bool {
	if method != http.MethodGet {
		return false
	}
	switch resource {
	case "/admin/v1/overviews/model-catalog", "/admin/v1/overviews/token-resources", "/admin/v1/overviews/model-operations":
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
