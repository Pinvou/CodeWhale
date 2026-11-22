package identityadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// Only the first password step of ActivateBootstrapAdmin can issue this proof.
// Never expose a constructor or infer it from a general password error.
type bootstrapPasswordRejected struct{}

func (*bootstrapPasswordRejected) Error() string {
	return "bootstrap password rejected by Keycloak policy"
}
func (*bootstrapPasswordRejected) Unwrap() error { return ErrPasswordInvalid }

// BootstrapActivationUnchanged reports a confirmed policy rejection before any
// password, enable or session change in this activation attempt.
func BootstrapActivationUnchanged(err error) bool {
	var rejected *bootstrapPasswordRejected
	return errors.As(err, &rejected)
}

func bootstrapPasswordPolicyRejection(response *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusBadRequest || err != nil || mediaType != "application/json" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return false
	}
	// Reject duplicate fields, incomplete bodies and multiple JSON values, even
	// when a permissive JSON decoder could extract a recognizable policy code.
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	fields := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "error" && key != "error_description") {
			return false
		}
		if _, duplicate := fields[key]; duplicate {
			return false
		}
		var value string
		if decoder.Decode(&value) != nil || value == "" {
			return false
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != 2 {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	// Keycloak 26.7.0 built-in policy errors: PasswordCredentialProvider validates
	// before encoding/storing credentials; UserResource.resetPassword converts
	// these ModelExceptions into ErrorResponseException, whose getResponse marks
	// the Identity transaction rollback-only. General ModelExceptions/4xx are not
	// proof. Localized descriptions are never interpreted or returned to callers.
	switch fields["error"] {
	case "invalidPasswordMinLengthMessage", "invalidPasswordMinDigitsMessage",
		"invalidPasswordMinLowerCaseCharsMessage", "invalidPasswordMinUpperCaseCharsMessage",
		"invalidPasswordMinSpecialCharsMessage", "invalidPasswordNotUsernameMessage",
		"invalidPasswordNotEmailMessage", "invalidPasswordRegexPatternMessage",
		"invalidPasswordHistoryMessage":
		return true
	}
	return false
}
