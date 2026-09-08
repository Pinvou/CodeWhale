package events

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/nats-io/nkeys"
)

func fixedNATSRoleNames() []string {
	return []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
}

// parseNATSPublicKeys accepts only the closed public identity artifact, never
// caller-supplied permissions. Token parsing rejects duplicate JSON keys.
func parseNATSPublicKeys(raw []byte) (map[string]string, error) {
	if len(raw) == 0 || len(raw) > 32<<10 {
		return nil, ErrNATSResources
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrNATSResources
	}
	seen := map[string]bool{}
	keys := map[string]string{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, ErrNATSResources
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return nil, ErrNATSResources
		}
		seen[name] = true
		switch name {
		case "version":
			var version int
			if d.Decode(&version) != nil || version != 1 {
				return nil, ErrNATSResources
			}
		case "purpose":
			var purpose string
			if d.Decode(&purpose) != nil || purpose != "pinvou-nats-service-identities" {
				return nil, ErrNATSResources
			}
		case "public_keys":
			start, err := d.Token()
			if err != nil || start != json.Delim('{') {
				return nil, ErrNATSResources
			}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return nil, ErrNATSResources
				}
				role, ok := token.(string)
				if !ok {
					return nil, ErrNATSResources
				}
				if _, exists := keys[role]; exists {
					return nil, ErrNATSResources
				}
				var public string
				if d.Decode(&public) != nil || !nkeys.IsValidPublicUserKey(public) {
					return nil, ErrNATSResources
				}
				keys[role] = public
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrNATSResources
			}
		default:
			return nil, ErrNATSResources
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 3 || len(keys) != len(fixedNATSRoleNames()) || d.Decode(new(any)) != io.EOF {
		return nil, ErrNATSResources
	}
	unique := map[string]bool{}
	for _, role := range fixedNATSRoleNames() {
		public := keys[role]
		if public == "" || unique[public] {
			return nil, ErrNATSResources
		}
		unique[public] = true
	}
	return keys, nil
}

type natsPermissions struct {
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
}
type natsAuthorizedUser struct {
	NKey        string          `json:"nkey"`
	Permissions natsPermissions `json:"permissions"`
}
type natsAuthorization struct {
	Users []natsAuthorizedUser `json:"users"`
}

func fixedNATSPermissions(role string) natsPermissions {
	pub := []string{"$JS.API.STREAM.INFO." + platformStream}
	switch role {
	case "account-outbox-relay", "billing-outbox-relay", "token-outbox-relay":
		source := role[:len(role)-len("-outbox-relay")]
		pub = append(pub, source+".>", "account-platform.dead-letter."+source+".>")
	case "event-resource-operator":
		pub = append(pub, "$JS.API.STREAM.CREATE."+platformStream)
		for _, consumer := range provisionedConsumers() {
			pub = append(pub, "$JS.API.CONSUMER.INFO."+platformStream+"."+consumer.Name, "$JS.API.CONSUMER.CREATE."+platformStream+"."+consumer.Name+"."+consumer.FilterSubject)
		}
	default:
		for _, consumer := range provisionedConsumers() {
			if !consumerOwnedBy(role, consumer) {
				continue
			}
			pub = append(pub, "$JS.API.CONSUMER.INFO."+platformStream+"."+consumer.Name, "$JS.API.CONSUMER.MSG.NEXT."+platformStream+"."+consumer.Name, "$JS.ACK."+platformStream+"."+consumer.Name+".>")
			if role == "billing-command-executor" {
				pub = append(pub, "account-platform.dead-letter."+consumer.FilterSubject)
			}
		}
	}
	return natsPermissions{Publish: pub, Subscribe: []string{"_INBOX." + role + ".>"}}
}

// RenderNATSAuthorization is pure: public artifacts in, fixed broker policy out.
// The caller must independently bind file bytes to trusted artifact hashes.
// A previous artifact is only for an explicitly selected, bounded overlap.
func RenderNATSAuthorization(current, previous []byte) ([]byte, error) {
	primary, err := parseNATSPublicKeys(current)
	if err != nil {
		return nil, err
	}
	sets := []map[string]string{primary}
	if len(previous) > 0 {
		old, err := parseNATSPublicKeys(previous)
		if err != nil {
			return nil, err
		}
		sets = append(sets, old)
	}
	seen := map[string]bool{}
	authorization := natsAuthorization{}
	for _, keys := range sets {
		for _, role := range fixedNATSRoleNames() {
			public := keys[role]
			if seen[public] {
				return nil, ErrNATSResources
			}
			seen[public] = true
			authorization.Users = append(authorization.Users, natsAuthorizedUser{NKey: public, Permissions: fixedNATSPermissions(role)})
		}
	}
	var output bytes.Buffer
	d := json.NewEncoder(&output)
	d.SetEscapeHTML(false)
	if d.Encode(struct {
		Authorization natsAuthorization `json:"authorization"`
	}{authorization}) != nil {
		return nil, ErrNATSResources
	}
	return output.Bytes(), nil
}
