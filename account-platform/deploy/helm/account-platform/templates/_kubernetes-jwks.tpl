{{/* Kubernetes credentials exist only for the exact built-in API JWKS URL. */}}
{{- define "pinvou.kubernetesJWKS" -}}
{{- if eq .Values.workloadIdentity.jwksUrl "https://kubernetes.default.svc/openid/v1/jwks" -}}true{{- end -}}
{{- end -}}

{{- define "pinvou.validateKubernetesJWKS" -}}
{{- if and (include "pinvou.kubernetesJWKS" .) .Values.networkPolicy.enabled -}}
{{- range $cidr := .Values.external.workloadJwksCidrs -}}
{{- if not (include "pinvou.validKubernetesAPICIDR" $cidr) -}}{{ fail "Kubernetes JWKS requires valid bounded API CIDRs, never an all-address prefix" }}{{- end -}}
{{- end -}}
{{- if or (eq (len .Values.external.workloadJwksCidrs) 0) (eq (len .Values.workloadIdentity.kubernetesApiServerPorts) 0) -}}
{{- fail "Kubernetes JWKS requires explicit external.workloadJwksCidrs and workloadIdentity.kubernetesApiServerPorts for the actual API/CNI path" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pinvou.kubernetesJWKSEnv" -}}
{{- if include "pinvou.kubernetesJWKS" .root }}
- { name: WORKLOAD_IDENTITY_CA_FILE, value: /var/run/secrets/pinvou/jwks-ca/ca.crt }
- { name: WORKLOAD_IDENTITY_JWKS_AUTH_MODE, value: kubernetes }
- { name: WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE, value: /var/run/secrets/pinvou/jwks-api/token }
- { name: WORKLOAD_IDENTITY_JWKS_API_SUBJECT, value: {{ printf "system:serviceaccount:%s:%s-%s" .root.Release.Namespace (include "pinvou.fullname" .root) .component | quote }} }
{{- end -}}
{{- end -}}

{{- define "pinvou.kubernetesJWKSMounts" -}}
{{- if include "pinvou.kubernetesJWKS" . }}
# CA subPath is a regular immutable snapshot; CA replacement requires rollout.
- { name: kubernetes-jwks-ca, mountPath: /var/run/secrets/pinvou/jwks-ca/ca.crt, subPath: ca.crt, readOnly: true }
# Keep this whole directory so kubelet's atomic token rotation stays visible.
- { name: kubernetes-jwks-api, mountPath: /var/run/secrets/pinvou/jwks-api, readOnly: true }
{{- end -}}
{{- end -}}

{{- define "pinvou.kubernetesJWKSVolumes" -}}
{{- if include "pinvou.kubernetesJWKS" . }}
- name: kubernetes-jwks-ca
  configMap: { name: kube-root-ca.crt, defaultMode: 292, items: [{ key: ca.crt, path: ca.crt }] }
- name: kubernetes-jwks-api
  projected:
    defaultMode: 288
    sources:
      # Omit audience: kubelet requests the API server's default API audience.
      # This is a separate credential from the internal business-audience token.
      - serviceAccountToken: { path: token, expirationSeconds: 600 }
{{- end -}}
{{- end -}}

{{/* Validate IPv4/IPv6 prefixes without DNS or cluster lookups. */}}
{{- define "pinvou.validKubernetesAPICIDR" -}}
{{- $valid := false -}}
{{- $parts := splitList "/" . -}}
{{- if eq (len $parts) 2 -}}
{{- $address := index $parts 0 -}}{{- $mask := index $parts 1 -}}
{{- $bits := atoi $mask -}}
{{- $v4 := "^((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])[.]){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])$" -}}
{{- if and (regexMatch "^[0-9]+$" $mask) (gt $bits 0) -}}
{{- if not (contains ":" $address) -}}
{{- $valid = and (le $bits 32) (regexMatch $v4 $address) -}}
{{- else -}}
{{- $compressed := contains "::" $address -}}
{{- $valid = and (le $bits 128) (not (contains ":::" $address)) (le (len (splitList "::" $address)) 2) (or (not (hasPrefix ":" $address)) (hasPrefix "::" $address)) (or (not (hasSuffix ":" $address)) (hasSuffix "::" $address)) -}}
{{- $components := splitList ":" $address -}}{{- $units := 0 -}}
{{- range $index, $group := $components -}}
{{- if ne $group "" -}}
{{- if contains "." $group -}}
{{- $valid = and $valid (eq $index (sub (len $components) 1)) (regexMatch $v4 $group) -}}
{{- $units = add $units 2 -}}
{{- else -}}
{{- $valid = and $valid (regexMatch "^[0-9A-Fa-f]{1,4}$" $group) -}}
{{- $units = add $units 1 -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $valid = and $valid (or (and $compressed (lt $units 8)) (and (not $compressed) (eq $units 8))) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $valid -}}true{{- end -}}
{{- end -}}
