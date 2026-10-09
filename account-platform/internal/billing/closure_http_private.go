package billing

import (
	"net/http"
)

func PrivateClosureHTTPHandler(r *PostgresRepository) http.Handler {
	s := &Service{repository: r}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/users/{user_id}/account-closure-grants/isolate", s.isolateAccountClosedGrants)
	mux.HandleFunc("GET /internal/v1/users/{user_id}/account-closure-assessment", s.accountClosureAssessment)
	return mux
}
