{{- define "pinvou.name" -}}pinvou-account-platform{{- end -}}
{{- define "pinvou.fullname" -}}{{ .Release.Name }}-account-platform{{- end -}}
{{- define "pinvou.labels" -}}
app.kubernetes.io/name: {{ include "pinvou.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}
{{- define "pinvou.selector" -}}
app.kubernetes.io/name: {{ include "pinvou.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "pinvou.validate" -}}
{{- if eq .Values.global.environment "production-new" -}}
{{- if or (not .Values.networkPolicy.enabled) (not .Values.ingress.enabled) -}}{{ fail "production-new requires NetworkPolicy and ingress" }}{{- end -}}
{{- if eq .Values.ingress.tlsSecretName "" -}}{{ fail "production-new requires an ingress TLS secret" }}{{- end -}}
{{- if not .Values.externalSecrets.enabled -}}{{ fail "production-new requires an external KMS/Vault-backed runtime secret" }}{{- end -}}
{{- if or (eq .Values.externalSecrets.secretStoreName "") (eq .Values.externalSecrets.remoteKey "") -}}{{ fail "production-new requires an external secret store and remote key" }}{{- end -}}
{{- if or (eq .Values.tokenGateway.secretProvider "") (eq .Values.tokenGateway.secretProvider "local") -}}{{ fail "production-new requires an external Token secret provider" }}{{- end -}}
{{- if or (contains "localhost" .Values.oidc.issuer) (contains "127.0.0.1" .Values.oidc.issuer) -}}{{ fail "production-new OIDC issuer must not be local" }}{{- end -}}
{{- if or (contains "simulator" .Values.public.paymentCallbackUrl) (contains "mock" .Values.public.paymentCallbackUrl) -}}{{ fail "production-new must not reference a payment simulator" }}{{- end -}}
{{- if and .Values.newApi.enabled (eq .Values.images.newApi.digest "") -}}{{ fail "production-new New API image must be pinned by digest" }}{{- end -}}
{{- if and .Values.newApi.enabled (not (has .Values.newApi.serviceName .Values.tokenGateway.providerAllowedHosts)) -}}{{ fail "Token Gateway provider allowlist must include the private New API service name" }}{{- end -}}
{{- if or (eq (len .Values.external.postgresCidrs) 0) (eq (len .Values.external.paymentProviderCidrs) 0) (eq (len .Values.external.secretProviderCidrs) 0) (eq (len .Values.external.modelProviderCidrs) 0) -}}{{ fail "production-new requires explicit external dependency CIDRs" }}{{- end -}}
{{- end -}}
{{- if and (not .Values.keycloak.enabled) (or (eq .Values.oidc.jwksUrl "") (eq .Values.oidc.tokenUrl "")) -}}{{ fail "external Keycloak requires explicit OIDC JWKS and token backchannel URLs" }}{{- end -}}
{{- end -}}
