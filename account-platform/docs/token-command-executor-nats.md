# Token command executor NATS identity

The privileged executor now passes `ResourceMode: "provisioned"`, `RequireExplicitAuthentication: true` and both existing explicit identity-file environment inputs (`NATS_NKEY_SEED_FILE` / `NATS_CREDENTIALS_FILE`) to the existing broker. Exactly one of those files must be selected; no resource auto-provisioning or implicit process-identity fallback is permitted. Existing Compose/Helm credential mounts and broker ACLs are unchanged.

Before this correction, production already resolved the omitted resource mode to provisioned and selected `_INBOX.token-command-executor` through that path. The missing explicit options alone did not demonstrate a production ACL or inbox bypass. This change makes the privileged assembly explicit for every environment and matches existing Billing/outbox runtime boundaries.

Run `go test -tags=nomsgpack ./cmd/token-command-executor -run '^TestTokenExecutor' -count=1`. Tests inspect the actual main configuration and call the actual broker constructor with missing/conflicting credentials against a loopback listener, proving rejection before any connection. No real NATS server binary was available; zero real broker/TLS/ACL/consumer cases were executed in this batch, and successful provisioning/consumption remain separate acceptance.
