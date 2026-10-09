package newapitls

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// NewStatusServer exposes the existing same-pod New API status response over
// TLS, without admitting provider operations or forwarding caller credentials.
func NewStatusServer(address, certificateFile, keyFile string) (*http.Server, error) {
	server, err := NewServer(address, certificateFile, keyFile)
	if err != nil {
		return nil, err
	}
	upstream, _ := url.Parse("http://127.0.0.1:8080")
	server.Handler = newStatusHandler(upstream)
	return server, nil
}

func newStatusHandler(upstream *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Host = upstream.Host
			request.Out.URL.Path = "/api/status"
			request.Out.URL.RawPath = ""
			request.Out.URL.RawQuery = ""
			request.Out.URL.ForceQuery = false
			request.Out.Header = make(http.Header)
			request.Out.Trailer = nil
			request.Out.Header.Set("Accept", "application/json")
			request.Out.Header.Set("User-Agent", "pinvou-newapi-status")
		},
		Transport: &http.Transport{
			// The upstream is fixed by NewStatusServer; process proxy settings
			// cannot redirect this same-pod request to an external endpoint.
			DialContext:     (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			IdleConnTimeout: 90 * time.Second,
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "New API upstream is unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Del("Location")
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/status" ||
			r.URL.EscapedPath() != "/api/status" || r.URL.RawQuery != "" || r.URL.ForceQuery ||
			r.ContentLength != 0 || len(r.TransferEncoding) != 0 ||
			(r.Body != nil && r.Body != http.NoBody) {
			http.NotFound(w, r)
			return
		}
		// ReverseProxy preserves the real upstream status, including non-2xx,
		// and never follows redirects. Listener readiness is not provider health.
		proxy.ServeHTTP(w, r)
	})
}
