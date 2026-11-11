package portal

import (
	"bytes"
	"io"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const ordinaryAPIResponseLimit int64 = 32 << 20

func copyPortalResponseHeaders(w http.ResponseWriter, response *http.Response) {
	for _, name := range []string{"Content-Type", "Retry-After", "X-Context-Version", "X-Authorization-Version", "X-Personal-Authorization-Version", "X-Platform-Authorization-Version", "X-Organization-Authorization-Version"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
}

// Ordinary API responses are complete projections. Check the existing byte
// limit before committing their status; a prefix is not a successful result.
// One-use downloads have a separate streaming contract in Service.forward.
func forwardOrdinaryAPIResponse(w http.ResponseWriter, r *http.Request, response *http.Response) {
	if r.Method == http.MethodHead || response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified {
		copyPortalResponseHeaders(w, response)
		w.WriteHeader(response.StatusCode)
		return
	}
	if response.ContentLength > ordinaryAPIResponseLimit {
		writeUpstreamResponseTooLarge(w, r)
		return
	}
	body, size, err := readOrdinaryProxyBody(response.Body, response.ContentLength, ordinaryAPIResponseLimit)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if size > ordinaryAPIResponseLimit {
		writeUpstreamResponseTooLarge(w, r)
		return
	}
	copyPortalResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	for _, chunk := range body {
		if written, err := w.Write(chunk); err != nil || written != len(chunk) {
			panic(http.ErrAbortHandler)
		}
	}
}

func readOrdinaryProxyBody(body io.Reader, contentLength, limit int64) ([][]byte, int64, error) {
	limited := &io.LimitedReader{R: body, N: limit + 1}
	var buffers [][]byte
	chunkSize := int64(1024)
	if contentLength >= 0 {
		chunkSize = contentLength + 1
	}
	// Keep validated chunks without growing or flattening a large buffer.
	for limited.N > 0 {
		size := min(chunkSize, limited.N)
		var chunk bytes.Buffer
		chunk.Grow(int(size) + bytes.MinRead)
		n, err := chunk.ReadFrom(io.LimitReader(limited, size))
		if err != nil {
			return nil, 0, err
		}
		if n > 0 {
			buffers = append(buffers, chunk.Bytes())
		}
		if n < size {
			break
		}
		chunkSize = min(chunkSize*2, 1<<20)
	}
	return buffers, limit + 1 - limited.N, nil
}

func writeUpstreamResponseTooLarge(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusBadGateway, "UPSTREAM_RESPONSE_TOO_LARGE", "the account platform response is too large", httpserver.RequestID(r.Context()))
}
