package token

import ()

func NewHTTPUpstreamClient() *HTTPUpstreamClient {
	return newHTTPUpstreamClient("openai-compatible-v1", []string{"X-Request-ID", "Request-ID"})
}
