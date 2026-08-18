# Account Platform Helm Chart

This chart deploys Account Service, Billing Service, Token Gateway, Token Portal,
Keycloak, Redis, and NATS. PostgreSQL and payment/model providers stay external so
their backup, certificate, and merchant controls remain environment-owned.

Create the referenced runtime Secret before install. It must contain separate
database URLs (`account-service-database-url`, `billing-service-database-url`,
`token-gateway-database-url`, and Keycloak database keys), the internal service
token, payment adapter URL/HMAC keys, the environment-specific external
secret-provider material, `token-default-provider-api-key`, and the New API
reconciliation credentials (`token-reconciliation-newapi-admin-token` and
`token-reconciliation-newapi-admin-user-id`). Never place those values in a
values file.

Production-new deliberately excludes Payment Simulator and Mock New API. Use the
dedicated production values only after the real payment adapter, real New API,
and external KMS/Vault gates pass. The chart validation rejects local secret
providers, local issuers, disabled NetworkPolicy, and simulator callback URLs.
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
