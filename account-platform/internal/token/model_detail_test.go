package token

import "testing"

func TestAdminProtocolSchemaFieldCount(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		schema  string
		count   int
		wantErr bool
	}{
		{name: "chat parameters", schema: `{"parameters":["estimated_input_tokens","max_output_tokens","image_count","request_count"]}`, count: 4},
		{name: "json schema properties", schema: `{"type":"object","properties":{"prompt":{"type":"string"},"count":{"type":"integer"}}}`, count: 2},
		{name: "empty properties", schema: `{"properties":{}}`, count: 0},
		{name: "malformed json", schema: `{`, wantErr: true},
		{name: "missing supported shape", schema: `{"type":"object"}`, wantErr: true},
		{name: "ambiguous shape", schema: `{"parameters":[],"properties":{}}`, wantErr: true},
		{name: "wrong parameter type", schema: `{"parameters":[1]}`, wantErr: true},
		{name: "null parameters", schema: `{"parameters":null}`, wantErr: true},
		{name: "blank parameter", schema: `{"parameters":[""]}`, wantErr: true},
		{name: "noncanonical parameter", schema: `{"parameters":[" prompt"]}`, wantErr: true},
		{name: "duplicate parameter", schema: `{"parameters":["prompt","prompt"]}`, wantErr: true},
		{name: "invalid properties", schema: `{"properties":[]}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			count, err := adminProtocolSchemaFieldCount([]byte(test.schema))
			if (err != nil) != test.wantErr || count != test.count {
				t.Fatalf("count=%d err=%v, want count=%d err=%v", count, err, test.count, test.wantErr)
			}
		})
	}
}
