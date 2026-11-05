package operator

import (
	_ "embed"
	"strings"
)

//go:embed plans/runtime-policy.hcl
var runtimePolicy string

const keyPath = "transit/keys/pinvou-token-data"
const selfPolicy = `
path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "auth/token/revoke-self" { capabilities = ["update"] }
`

var roleNames = [...]string{"pinvou-token-gateway", "pinvou-credential-portal", "pinvou-kms-backup", "pinvou-kms-operations"}

func rolePolicy(name string) string {
	switch name {
	case roleNames[0], roleNames[1]:
		return runtimePolicy
	case roleNames[2]:
		return `path "sys/storage/raft/snapshot" { capabilities = ["read", "sudo"] }` + selfPolicy
	case roleNames[3]:
		var p strings.Builder
		p.WriteString(`path "transit/keys/pinvou-token-data" { capabilities = ["read"] }
path "transit/keys/pinvou-token-data/rotate" { capabilities = ["update"] }
`)
		for _, role := range roleNames {
			p.WriteString(`path "auth/approle/role/` + role + `/role-id" { capabilities = ["read"] }` + "\n")
			p.WriteString(`path "auth/approle/role/` + role + `/secret-id" { capabilities = ["update", "list"] }` + "\n")
			p.WriteString(`path "auth/approle/role/` + role + `/secret-id-accessor/lookup" { capabilities = ["update"] }` + "\n")
			p.WriteString(`path "auth/approle/role/` + role + `/secret-id-accessor/destroy" { capabilities = ["update"] }` + "\n")
		}
		p.WriteString(selfPolicy)
		return p.String()
	}
	return ""
}

func roleSettings(name string) map[string]any {
	return map[string]any{"bind_secret_id": true, "secret_id_ttl": 0, "secret_id_num_uses": 0,
		"token_type": "service", "token_ttl": 300, "token_max_ttl": 900, "token_explicit_max_ttl": 900,
		"token_no_default_policy": true, "token_num_uses": 0, "token_policies": []string{name}}
}
