package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"net/http"
)

func signAdminQuerySource(request *http.Request, key []byte, source string) {
	request.Header.Set("X-Pinvou-Client-Source", source)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(request.Method + "\n" + request.URL.RequestURI() + "\n" + source))
	request.Header.Set("X-Pinvou-Client-Source-Signature", hex.EncodeToString(mac.Sum(nil)))
}
