{{/* Operator-provided image and Ready TLS resources; no generated credentials. */}}
{{- define "pinvou.requireNewApiTLS" -}}
{{- if .Values.newApi.enabled -}}
{{- $_ := required "newApi.tlsSecretName must name a Ready TLS Secret" .Values.newApi.tlsSecretName -}}
{{- $_ := required "newApi.caConfigMap must name a Ready public CA ConfigMap" .Values.newApi.caConfigMap -}}
{{- $_ := required "images.newApiTLS.repository is required" .Values.images.newApiTLS.repository -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.images.newApiTLS.digest) -}}{{ fail "images.newApiTLS.digest must pin the approved TLS image" }}{{- end -}}
{{- end -}}
{{- end -}}
{{- define "pinvou.newApiProviderURL" -}}
{{- if .Values.tokenGateway.defaultProviderBaseUrl -}}{{ .Values.tokenGateway.defaultProviderBaseUrl }}
{{- else if .Values.newApi.enabled -}}{{ printf "https://%s:8443" .Values.newApi.serviceName }}
{{- end -}}
{{- end -}}
{{- define "pinvou.newApiReconciliationURL" -}}
{{- default (include "pinvou.newApiProviderURL" .) .Values.tokenGateway.reconciliationNewApiBaseUrl -}}
{{- end -}}
