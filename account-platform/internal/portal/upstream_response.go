package portal

import (
	"io"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const ordinaryAPIResponseLimit int64 = 32 << 20

// Ordinary API responses are complete projections. Check the existing byte
// limit before committing their status; a prefix is not a successful result.
// One-use downloads have a separate streaming contract in Service.forward.
func forwardOrdinaryAPIResponse(w http.ResponseWriter, r *http.Request, response *http.Response) {
	if r.Method != http.MethodHead && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotModified && response.ContentLength > ordinaryAPIResponseLimit {
		writeUpstreamResponseTooLarge(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ordinaryAPIResponseLimit+1))
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the account platform is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if int64(len(body)) > ordinaryAPIResponseLimit {
		writeUpstreamResponseTooLarge(w, r)
		return
	}
	w.WriteHeader(response.StatusCode)
	if len(body) == 0 {
		return
	}
	if written, err := w.Write(body); err != nil || written != len(body) {
		panic(http.ErrAbortHandler)
	}
}

func writeUpstreamResponseTooLarge(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusBadGateway, "UPSTREAM_RESPONSE_TOO_LARGE", "the account platform response is too large", httpserver.RequestID(r.Context()))
}
