package httpserver

import "net/http"

// AdmissionClass is selected only by a trusted application after pure credential
// verification. Neither path nor an unverified request claim grants a budget.
type AdmissionClass uint8

const (
	OrdinaryAdmission AdmissionClass = iota
	InvalidCallbackAdmission
	VerifiedCallbackAdmission
	VerifiedCallbackTokenAdmission
)

type AdmissionPolicy interface {
	RateLimitAdmission(*http.Request) (AdmissionClass, string)
}

type admissionHandler struct {
	http.Handler
	classify func(*http.Request) (AdmissionClass, string)
}

func (h admissionHandler) RateLimitAdmission(r *http.Request) (AdmissionClass, string) {
	return h.classify(r)
}

// WithAdmissionPolicy adds no authentication or persistence side effects. The
// application handler must still enforce all original credential/body checks.
func WithAdmissionPolicy(next http.Handler, classify func(*http.Request) (AdmissionClass, string)) http.Handler {
	return admissionHandler{Handler: next, classify: classify}
}
