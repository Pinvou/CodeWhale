package token

import (
	"net/http"
)

func PrivateClosureHTTPHandler(r *PostgresRepository) http.Handler {
	s := &Service{repository: r}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /internal/v1/users/{user_id}/replay-payloads", s.anonymizeUserReplayPayloads)
	mux.HandleFunc("GET /internal/v1/users/{user_id}/account-closure-assessment", s.accountClosureAssessment)
	return mux
}
