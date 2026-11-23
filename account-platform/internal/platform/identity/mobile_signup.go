package identity

// MobileSignupCredentialMode is the trusted Account-to-Identity registration
// contract for a verified mobile account with no password credential.
const MobileSignupCredentialMode = "sms-only-v1"

// Use a distinct route so older adapters cannot interpret an SMS-only
// dispatch as a password-backed request.
const MobileSignupCreatePath = "/internal/v1/users/mobile/sms-only"
