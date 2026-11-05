package token

import (
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

// reference is the durable command returned by BeginTokenCommand, not a new
// identifier inferred from a failed probe or its possibly empty result.
func writeCredentialValidationCommandResponse(w http.ResponseWriter, r *http.Request, reference uuid.UUID, completed TokenCommandResult, executeErr error) {
	if errors.Is(executeErr, ErrCredentialProbeClaimed) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "2")
		api.WriteJSON(w, http.StatusAccepted, completed)
		return
	}
	if executeErr != nil {
		// BeginTokenCommand already persisted this identity. An empty result
		// after a probe/commit error must not force a caller to invent a key or
		// mistake the uncertain command for a proven zero-effect failure.
		if reference != uuid.Nil {
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the same command reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": reference, "result_query": "/token/admin/v1/commands/" + reference.String()}})
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "query the same command reference before retrying", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, completed)
}
