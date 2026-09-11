{{- define "pinvou.validateTokenGovernance" -}}
{{- $p := .Values.portal -}}
{{- $t := .Values.tokenGateway -}}
{{- if and $p.adminModelTokenIAEnabled (not $t.adminPaginationEnabled) -}}{{ fail "Token IA requires pagination" }}{{- end -}}
{{- if and (or $p.adminTokenResourceFormsEnabled $p.adminModelOperationsDetailsEnabled $p.adminModelTokenActionsEnabled $p.adminModelDetailsEnabled $p.adminTokenDetailsEnabled $p.adminModelOperationsOverviewEnabled $p.adminModelTokenPageOverviewsEnabled) (not $p.adminModelTokenIAEnabled) -}}{{ fail "Token child gates require IA" }}{{- end -}}
{{- if and $p.adminModelTokenActionsEnabled (or (not $t.impactPrechecksEnabled) (not .Values.tokenCommandExecutor.enabled)) -}}{{ fail "Token actions require prechecks and registered executor; persistent ACTIVE is separately verified" }}{{- end -}}
{{- if and $p.adminModelPricingEditEnabled (or (not $p.adminModelDetailsEnabled) (not .Values.billing.adminModelPriceProjectionEnabled)) -}}{{ fail "Model pricing requires details and Billing projection" }}{{- end -}}
{{- if and $p.adminModelOperationsOverviewEnabled (or (not $p.adminModelOperationsDetailsEnabled) (not $p.adminModelDetailsEnabled) (not $t.adminOperationsOverviewEnabled)) -}}{{ fail "Operations overview dependencies are missing" }}{{- end -}}
{{- if and $p.adminModelTokenPageOverviewsEnabled (not $t.adminPageOverviewsEnabled) -}}{{ fail "Page overviews require the Token page overview projection gate" }}{{- end -}}
{{- if and $t.credentialIsolationEnabled (or (not $p.adminTokenDetailsEnabled) (not $p.adminModelTokenActionsEnabled) (not .Values.tokenCredentialPortal.enabled)) -}}{{ fail "Credential isolation requires details, actions and credential service" }}{{- end -}}
{{- if or .Values.tokenCredentialPortal.enabled .Values.tokenCommandExecutor.enabled .Values.tokenGovernanceCheck.enabled -}}
{{- if not .Values.networkPolicy.enabled -}}{{ fail "Governed workloads require NetworkPolicy" }}{{- end -}}
{{- if not (has .Values.global.environment (list "staging" "production-new")) -}}{{ fail "Governed deployment requires an explicit supported non-development profile" }}{{- end -}}
{{- end -}}
{{- if or .Values.tokenCredentialPortal.enabled .Values.tokenCommandExecutor.enabled -}}
{{- if or (eq $t.runtimeSecret "") (eq $t.runtimeSecret .Values.global.runtimeSecret) -}}{{ fail "Governed workloads require a separate release-scoped gateway runtime Secret" }}{{- end -}}
{{- if or .Values.nats.enabled (not (hasPrefix "tls://" .Values.dependencies.natsUrl)) -}}{{ fail "Governed workloads require the existing authenticated external TLS NATS cluster; development NATS is not eligible" }}{{- end -}}
{{- if eq (len .Values.tokenCommandExecutor.natsCidrs) 0 -}}{{ fail "Governed event producers and executor require explicit shared NATS egress" }}{{- end -}}
{{- end -}}
{{- if .Values.tokenCredentialPortal.enabled -}}
{{- $c := .Values.tokenCredentialPortal -}}
{{- $origin := "https://credentials-uat.pinvou.com" -}}
{{- $portalOrigin := "https://account-uat.pinvou.com" -}}
{{- if hasPrefix "production" .Values.global.environment -}}{{- $origin = "https://credentials.pinvou.com" -}}{{- $portalOrigin = "https://account.pinvou.com" -}}{{- end -}}
{{- if ne $c.publicUrl $origin -}}{{ fail "Credential public origin does not match frozen environment" }}{{- end -}}
{{- if ne .Values.public.portalUrl $portalOrigin -}}{{ fail "Credential Portal origin does not match frozen environment" }}{{- end -}}
{{- $issuer := urlParse .Values.oidc.issuer -}}
{{- if or (ne $issuer.scheme "https") (eq $issuer.host "") (contains ":" $issuer.host) (contains "userinfo" (lower $issuer.path)) (ne $issuer.userinfo "") (ne $issuer.query "") (ne $issuer.fragment "") -}}{{ fail "Credential issuer must use HTTPS without credentials, query, fragment or explicit port" }}{{- end -}}
{{- range $endpoint := list $c.oidcAuthorizationUrl $c.oidcTokenUrl $c.oidcJwksUrl -}}
{{- $parsed := urlParse $endpoint -}}
{{- if or (ne $parsed.scheme "https") (ne $parsed.host $issuer.host) (eq $parsed.path "") (eq $parsed.path "/") (contains "userinfo" (lower $parsed.path)) (ne $parsed.userinfo "") (ne $parsed.fragment "") (ne $parsed.query "") -}}{{ fail "Credential OIDC endpoints must use exact issuer HTTPS origin without query or credentials" }}{{- end -}}
{{- end -}}
{{- if or (eq $c.runtimeSecret "") (eq $c.runtimeSecret .Values.global.runtimeSecret) (eq $c.runtimeSecret $t.runtimeSecret) -}}{{ fail "Credential portal requires its own release-scoped runtime Secret" }}{{- end -}}
{{- if or (eq $c.oidcClientId "") (eq $c.oidcAudience "") (eq $c.oidcClientId .Values.oidc.portalClientId) (eq $c.oidcAudience .Values.oidc.audience) (eq $c.oidcAuthorizationUrl "") (eq $c.oidcTokenUrl "") -}}{{ fail "Credential portal requires dedicated OIDC client, audience and endpoints" }}{{- end -}}
{{- if or (not (has $t.secretProvider (list "remote" "vault-transit"))) (eq (len $c.egressCidrs) 0) (eq (len .Values.external.postgresCidrs) 0) -}}{{ fail "Credential portal requires an external secret provider and explicit database/HTTPS egress" }}{{- end -}}
{{- if and (hasPrefix "production" .Values.global.environment) (lt (int $c.replicas) 2) -}}{{ fail "Production credential portal requires at least two replicas" }}{{- end -}}
{{- end -}}
{{- if .Values.tokenCommandExecutor.enabled -}}
{{- if or (eq .Values.tokenCommandExecutor.runtimeSecret "") (eq .Values.tokenCommandExecutor.runtimeSecret .Values.global.runtimeSecret) (eq .Values.tokenCommandExecutor.runtimeSecret $t.runtimeSecret) (eq .Values.tokenCommandExecutor.runtimeSecret .Values.tokenCredentialPortal.runtimeSecret) -}}{{ fail "Executor requires a separate NATS-only Secret" }}{{- end -}}
{{- if and (not .Values.nats.enabled) (eq (len .Values.tokenCommandExecutor.natsCidrs) 0) -}}{{ fail "External NATS requires explicit executor egress" }}{{- end -}}
{{- if and (hasPrefix "production" .Values.global.environment) (lt (int .Values.tokenCommandExecutor.replicas) 2) -}}{{ fail "Production executor requires at least two replicas" }}{{- end -}}
{{- end -}}
{{- if .Values.tokenGovernanceCheck.enabled -}}
{{- $s := .Values.tokenGovernanceCheck.operationsSecret -}}
{{- if or (eq $s "") (has $s (list .Values.global.runtimeSecret $t.runtimeSecret .Values.tokenCredentialPortal.runtimeSecret .Values.tokenCommandExecutor.runtimeSecret)) (eq (len .Values.external.postgresCidrs) 0) -}}{{ fail "Governance check requires isolated operations Secret and explicit database egress" }}{{- end -}}
{{- end -}}
{{- end -}}
