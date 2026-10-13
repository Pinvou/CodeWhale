// Package newapitls supplies the private TLS entry point for New API, whose
// pinned server listens on HTTP. Its upstream remains in the same pod.
package newapitls

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// NewServer loads the mounted certificate before accepting any traffic. The
// private key is never needed by Token Gateway; it receives only the issuing CA.
func NewServer(address, certificateFile, keyFile string) (*http.Server, error) {
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		return nil, errors.New("New API TLS certificate and key are required")
	}
	upstream, _ := url.Parse("http://127.0.0.1:8080")
	return &http.Server{
		Addr:              address,
		Handler:           newHandler(upstream),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}},
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		// Model generation/SSE must not acquire an ordinary response deadline.
		WriteTimeout: 0,
		// Standard TLS/HTTP error logs can contain caller-controlled values.
		ErrorLog: log.New(io.Discard, "", 0),
	}, nil
}

func newHandler(upstream *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Host = upstream.Host
			// Forward only the headers used by Gateway/provider authentication and
			// request correlation, never browser cookies or arbitrary proxy headers.
			request.Out.Header = make(http.Header)
			for _, name := range []string{"Authorization", "New-Api-User", "Content-Type", "Accept", "X-Request-ID", "Traceparent", "Tracestate"} {
				for _, value := range request.In.Header.Values(name) {
					request.Out.Header.Add(name, value)
				}
			}
			request.Out.Header.Set("User-Agent", "pinvou-newapi-tls")
		},
		Transport: &http.Transport{
			// Never route the same-pod upstream through a process HTTP proxy.
			DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			// First model bytes may be delayed. Do not impose a new response
			// deadline on the existing generation/stream lifecycle.
			IdleConnTimeout: 90 * time.Second,
		},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "New API upstream is unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			// Preserve model/SSE content and New API's authoritative request ID;
			// this entry point cannot create browser sessions or redirect callers.
			response.Header.Del("Set-Cookie")
			response.Header.Del("Location")
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.URL.EscapedPath() != path {
			http.NotFound(w, r)
			return
		}
		if (path == "/health/live" || path == "/health/ready") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.WriteHeader(http.StatusOK)
			return
		}
		// These are the only existing Gateway and deployment acceptance routes.
		allowed := (path == "/v1/chat/completions" && r.Method == http.MethodPost) ||
			(path == "/api/log/" && r.Method == http.MethodGet) ||
			(path == "/v1/models" && r.Method == http.MethodGet) ||
			(path == "/api/status" && r.Method == http.MethodGet)
		if !allowed {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
