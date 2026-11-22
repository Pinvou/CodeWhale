package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorSecretJSONKeepsExistingStringSemantics(t *testing.T) {
	for _, raw := range []string{`"escaped\u002dmaterial"`, `"line\nmaterial"`, `null`, `42`, `["x"]`, `true`, `"\ufffd"`} {
		t.Run(raw, func(t *testing.T) {
			var old string
			var owned secretBytes
			oldErr, newErr := json.Unmarshal([]byte(raw), &old), json.Unmarshal([]byte(raw), &owned)
			defer clear(owned)
			if (oldErr == nil) != (newErr == nil) || (oldErr == nil && !bytes.Equal(owned, []byte(old))) {
				t.Fatal("JSON string/null contract changed")
			}
		})
	}
	t.Run("replacement-clears-previous-owned-buffer", func(t *testing.T) {
		owned := secretBytes([]byte("old-owned-buffer"))
		alias := owned
		if err := json.Unmarshal([]byte(`"x"`), &owned); err != nil {
			t.Fatal("decode rejected")
		}
		// The reused first byte now holds the new value; the old suffix is erased.
		if !bytes.Equal(alias[1:], make([]byte, len(alias)-1)) {
			t.Fatal("old retained suffix survived replacement")
		}
		clear(owned)
	})
}

func TestUnsealMaterialListReplacementClearsDiscardedKeys(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `["replacement"]`, `["first",null]`, `[null]`, `[42]`} {
		t.Run(raw, func(t *testing.T) {
			original := secretBytes([]byte("previous-owned-key"))
			alias := original
			list := secretList{values: []secretBytes{original}}
			old := []string{"previous-owned-key"}
			oldErr, newErr := json.Unmarshal([]byte(raw), &old), json.Unmarshal([]byte(raw), &list)
			if (oldErr == nil) != (newErr == nil) {
				list.clear()
				t.Fatal("array replacement contract changed")
			}
			if oldErr == nil {
				if len(old) != len(list.values) {
					list.clear()
					t.Fatal("array size contract changed")
				}
				for i := range old {
					if !bytes.Equal(list.values[i], []byte(old[i])) {
						list.clear()
						t.Fatal("array string value contract changed")
					}
				}
			}
			list.clear()
			if !bytes.Equal(alias, make([]byte, len(alias))) {
				t.Fatal("discarded buffer survived parsing owner cleanup")
			}
		})
	}
}

func TestOperatorRepeatedJSONFieldsMatchOriginalDecoder(t *testing.T) {
	for name, raw := range map[string]string{
		"root-string-null":             `{"root_token":"root-canary-must-not-be-sent","root_token":null}`,
		"escaped-root-and-field":       `{"root_\u0074oken":"root\u002dcanary-must-not-be-sent"}`,
		"root-invalid-type":            `{"root_token":"root-canary-must-not-be-sent","root_token":42}`,
		"keys-string-null-element":     `{"keys_base64":["first"],"keys_base64":[null]}`,
		"keys-whole-null":              `{"keys_base64":["first"],"keys_base64":null}`,
		"keys-shrink-expand-null":      `{"keys_base64":["first","second"],"keys_base64":["replacement"],"keys_base64":[null,null]}`,
		"keys-empty-then-null-element": `{"keys_base64":["first"],"keys_base64":[],"keys_base64":[null]}`,
		"keys-escaped-field-and-value": `{"keys_base\u00364":["\u0041\u0042\u0043"]}`,
		"keys-invalid-type":            `{"keys_base64":["first"],"keys_base64":[42]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var old struct {
				Root string   `json:"root_token"`
				Keys []string `json:"keys_base64"`
			}
			var current struct {
				Root secretBytes `json:"root_token"`
				Keys secretList  `json:"keys_base64"`
			}
			defer func() { clear(current.Root); current.Keys.clear() }()
			oldErr, newErr := json.Unmarshal([]byte(raw), &old), json.Unmarshal([]byte(raw), &current)
			if (oldErr == nil) != (newErr == nil) {
				t.Fatal("original JSON acceptance changed")
			}
			if oldErr == nil {
				if !bytes.Equal(current.Root, []byte(old.Root)) || len(current.Keys.values) != len(old.Keys) {
					t.Fatal("original repeated/null field semantics changed")
				}
				for i := range old.Keys {
					if !bytes.Equal(current.Keys.values[i], []byte(old.Keys[i])) {
						t.Fatal("original key slot/null semantics changed")
					}
				}
			}
		})
	}
}

func TestBootstrapSessionRootBufferCleared(t *testing.T) {
	for _, kind := range []string{"success", "sealed-preflight-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := newBootstrapFixture(t)
			session, err := newBootstrapSession(context.Background(), f.cfg, f.bundle(t), f.id)
			if err != nil || len(session.root) == 0 {
				t.Fatal("session did not own root")
			}
			alias := session.root
			if kind != "success" {
				f.sealed.Store(true)
			}
			err = session.fresh()
			if (err == nil) != (kind == "success") {
				session.close()
				t.Fatal("TLS preflight outcome changed")
			}
			session.close()
			if len(session.root) != 0 || !bytes.Equal(alias, make([]byte, len(alias))) {
				t.Fatal("retained root bytes survived session close")
			}
			session.close() // best effort cleanup is safe when repeated
		})
	}
}

func TestUnsealOwnedBufferClearedOnEveryExit(t *testing.T) {
	for _, kind := range []string{"success", "already-unsealed", "preflight-failure", "error", "redirect", "disconnect", "truncated", "still-sealed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if kind == "already-unsealed" {
				f.sealed.Store(false)
			}
			if kind == "preflight-failure" {
				f.stateBody = `{}`
			}
			if kind != "success" && kind != "already-unsealed" && kind != "preflight-failure" && kind != "cancelled" {
				f.postMode = kind
			}
			c, ca, err := client(f.cfg)
			if err != nil {
				t.Fatal("TLS client failed")
			}
			defer c.CloseIdleConnections()
			owned, err := material(f.cfg, ca, f.bundle(t), f.id)
			if err != nil || len(owned) == 0 {
				t.Fatal("validated material unavailable")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			changed, err := unsealWithMaterial(ctx, c, f.cfg, owned)
			if !bytes.Equal(owned, make([]byte, len(owned))) {
				t.Fatal("owned unseal bytes survived exit")
			}
			switch kind {
			case "success":
				if err != nil || !changed || f.writes.Load() != 1 {
					t.Fatal("success protocol changed")
				}
			case "already-unsealed":
				if err != nil || changed || f.writes.Load() != 0 {
					t.Fatal("already-unsealed protocol changed")
				}
			case "preflight-failure", "cancelled":
				if err != ErrPreflight || changed || f.writes.Load() != 0 {
					t.Fatal("preflight protocol changed")
				}
			default:
				if err != ErrUncertain || changed || f.writes.Load() != 1 || strings.Contains(err.Error(), "canary") {
					t.Fatal("unknown protocol/retry/secrecy changed")
				}
			}
		})
	}
}

func TestUnsealMaterialStrictBase64BoundariesUnchanged(t *testing.T) {
	for name, key := range map[string]string{
		"minimum":          base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)),
		"maximum":          base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 128)),
		"short":            base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 31)),
		"long":             base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 129)),
		"bad-padding-bits": strings.Repeat("A", 42) + "B=",
		"bad-alphabet":     "!invalid!",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.envelope.Response, _ = json.Marshal(map[string]any{"keys_base64": []string{key}, "root_token": "ignored-by-unseal"})
			c, ca, err := client(f.cfg)
			if err != nil {
				t.Fatal("TLS configuration failed")
			}
			defer c.CloseIdleConnections()
			owned, err := material(f.cfg, ca, f.bundle(t), f.id)
			defer clear(owned)
			old, oldErr := base64.StdEncoding.Strict().DecodeString(key)
			defer clear(old)
			want := oldErr == nil && len(old) >= 32 && len(old) <= 128
			if (err == nil) != want || (want && !bytes.Equal(owned, []byte(key))) || f.requests.Load() != 0 {
				t.Fatal("strict encoded material contract changed")
			}
		})
	}
}

func TestOperatorRepeatedMaterialUsesOriginalTLSProtocol(t *testing.T) {
	root := `"root-canary-must-not-be-sent"`
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"root-string-then-null", `{"root_token":` + root + `,"root_token":null}`, true},
		{"escaped-root-token", `{"root_\u0074oken":"root\u002dcanary-must-not-be-sent"}`, true},
		{"root-invalid-type", `{"root_token":` + root + `,"root_token":42}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootstrapFixture(t)
			f.envelope.Response = json.RawMessage(tc.body)
			session, err := newBootstrapSession(context.Background(), f.cfg, f.bundle(t), f.id)
			if (err == nil) != tc.valid {
				t.Fatal("original root parse outcome changed")
			}
			if err == nil {
				alias := session.root
				if session.fresh() != nil {
					session.close()
					t.Fatal("original TLS root identity changed")
				}
				session.close()
				if !bytes.Equal(alias, make([]byte, len(alias))) {
					t.Fatal("root alias survived cleanup")
				}
			} else if f.requests.Load() != 0 {
				t.Fatal("invalid root reached network")
			}
		})
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	quoted, _ := json.Marshal(key)
	escaped := `"\u004b` + key[1:] + `"`
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"key-slot-string-then-null", `{"keys_base64":[` + string(quoted) + `],"keys_base64":[null]}`, true},
		{"escaped-key-field-and-value", `{"keys_base\u00364":[` + escaped + `],"root_token":42}`, true},
		{"whole-list-null", `{"keys_base64":[` + string(quoted) + `],"keys_base64":null}`, false},
		{"key-invalid-type", `{"keys_base64":[` + string(quoted) + `],"keys_base64":[42]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.envelope.Response = json.RawMessage(tc.body)
			changed, err := Unseal(context.Background(), f.cfg, f.bundle(t), f.id)
			if (err == nil) != tc.valid || changed != tc.valid {
				t.Fatal("original unseal parse outcome changed")
			}
			want := int32(0)
			if tc.valid {
				want = 1
			}
			if f.writes.Load() != want {
				t.Fatal("unseal writes/retry changed")
			}
		})
	}
}

func TestSecretListRepeatedFieldsKeepBoundedOwners(t *testing.T) {
	const width = 4096
	const repeats = 6000
	first := make([]string, width)
	for i := range first {
		first[i] = "controlled-owned-key"
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	var body strings.Builder
	body.WriteString(`{"keys_base64":`)
	body.Write(encoded)
	for range repeats {
		body.WriteString(`,"keys_base64":[null]`)
	}
	body.WriteByte('}')
	if body.Len() > 1<<20 {
		t.Fatal("fixture must stay within original 1MiB bound")
	}
	var old struct {
		Keys []string `json:"keys_base64"`
	}
	var current struct {
		Keys secretList `json:"keys_base64"`
	}
	defer current.Keys.clear()
	if json.Unmarshal([]byte(body.String()), &old) != nil || json.Unmarshal([]byte(body.String()), &current) != nil {
		t.Fatal("accepted repeated input changed")
	}
	if len(old.Keys) != 1 || len(current.Keys.values) != 1 || !bytes.Equal(current.Keys.values[0], []byte(old.Keys[0])) {
		t.Fatal("repeated null result changed")
	}
	current.Keys.remember()
	if len(current.Keys.retained) != 1 {
		t.Fatal("same backing array retained more than once", len(current.Keys.retained))
	}
	slots := 0
	var aliases []secretBytes
	for _, backing := range current.Keys.retained {
		slots += len(backing)
		aliases = append(aliases, backing...)
	}
	if slots != cap(current.Keys.values) || slots > width*2 {
		t.Fatal("retained owner headers grew with repeated fields", slots)
	}
	// Force a separate growing array, then whole-null and empty fields. Old
	// arrays must remain owned exactly once until cleanup without input rejection.
	for _, raw := range []string{`null`, `["replacement"]`, `["replacement","growth","other"]`, `[]`, `["later",null]`} {
		if json.Unmarshal([]byte(raw), &old.Keys) != nil || json.Unmarshal([]byte(raw), &current.Keys) != nil {
			t.Fatal("array replacement changed")
		}
		if len(old.Keys) != len(current.Keys.values) {
			t.Fatal("array replacement size changed")
		}
		for i := range old.Keys {
			if !bytes.Equal(current.Keys.values[i], []byte(old.Keys[i])) {
				t.Fatal("array replacement value changed")
			}
		}
	}
	current.Keys.remember()
	if len(current.Keys.retained) > 4 {
		t.Fatal("owner count exceeds distinct nonempty arrays", len(current.Keys.retained))
	}
	for _, backing := range current.Keys.retained {
		aliases = append(aliases, backing...)
	}
	current.Keys.clear()
	if len(current.Keys.retained) != 0 || len(current.Keys.values) != 0 {
		t.Fatal("cleanup retained owners")
	}
	for _, alias := range aliases {
		if !bytes.Equal(alias, make([]byte, len(alias))) {
			t.Fatal("owned alias survived cleanup")
		}
	}
}
