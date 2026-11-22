package token

// Runtime protocols are explicit publication facts, not display-name aliases.
// Empty preserves an existing, unconfigured capability: desktop invocation must
// fail closed until an administrator publishes a new capability with a profile.
// Neither discovery nor this allowlist guesses from a provider/upstream name.
func validRuntimeProtocol(profile string) bool {
	switch profile {
	case "", "OPENAI_CHAT_V1", "DEEPSEEK_V4_CHAT_V1":
		return true
	default:
		return false
	}
}
