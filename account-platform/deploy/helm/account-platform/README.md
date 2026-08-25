# Account Platform Helm Chart

This chart deploys Account Service, Billing Service, Token Gateway, Token Portal,
Keycloak, Redis, and NATS. PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

Install External Secrets Operator and create the least-privilege referenced
`SecretStore` or `ClusterSecretStore` before install. Production values create
an `ExternalSecret` that owns the runtime Secret; non-production environments
may create that Secret out of band. It must contain separate
database URLs (`account-service-database-url`, `billing-service-database-url`,
`token-gateway-database-url`, and Keycloak database keys), the internal service
token, payment adapter URL/HMAC keys, the environment-specific external
secret-provider material, `token-default-provider-api-key`, and the New API
reconciliation credentials (`token-reconciliation-newapi-admin-token` and
`token-reconciliation-newapi-admin-user-id`). Mobile registration additionally
requires `registration-query-hmac-key-base64`, isolated from the Identity password
and SMS verification HMAC keys. Production authentication risk admission also
requires an independent `auth-risk-hmac-key-base64`. Controlled audit export
requires `audit-export-master-key-base64`, the standard Base64 encoding of 32
independent random bytes; keep it in the external secret store, separate from
registration, authentication, payment, and model keys. Account human-operation
delegations additionally require `human-delegation-signing-key-id` and an isolated
Ed25519 seed/private key in unpadded Base64URL as
`human-delegation-signing-key-base64url`; publish only the derived public key to
authorized consumers as `human-delegation-verification-keys-json` and overlap key IDs
during rotation. The exact public OIDC token
path is routed through Identity Adapter and fails closed when its external Redis
or Keycloak dependency is unavailable. Never place those values in a
values file or rendered manifest.

Production-new deliberately excludes Payment Simulator and Mock New API. Use the
dedicated production values only after the real payment adapter, real New API,
and external KMS/Vault gates pass. The chart validation rejects local secret
providers, local issuers, disabled NetworkPolicy, and simulator callback URLs.
It also rejects production without an enabled ExternalSecret, configured TLS
Secret, and HTTPS public URLs. During key rotation, keep the previous key
decryptable, update `tokenGateway.secretKeyId`, and wait until every Token
Gateway readiness probe reports zero old-key ciphertext before disabling the
previous key. Run `npm --prefix account-platform run
security:production-readiness` only after the old key is disabled and archive
its scrubbed JSON evidence with the release record.
Registry names, provider hosts, and CIDRs in the production values file are
documentation examples; replace every one with the reviewed deployment values
before rendering a release manifest.

```sh
helm lint deploy/helm/account-platform \
  -f deploy/helm/account-platform/values-production-new.yaml
helm template pinvou deploy/helm/account-platform \
  -f deploy/helm/account-platform/values-production-new.yaml > rendered.yaml
```

Migration Jobs are pre-install/pre-upgrade hooks. Rollback does not delete PVCs;
database rollback remains an explicit, separately approved operation.
