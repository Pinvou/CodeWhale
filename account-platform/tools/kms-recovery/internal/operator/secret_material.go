package operator

import (
	"bytes"
	"encoding/json"
)

// secretBytes retains JSON string material in an owned mutable buffer. The
// temporary decoding string belongs to encoding/json/Go and cannot be promised
// erased. This type preserves the existing JSON string/null decoding behavior;
// []byte's default base64 JSON decoding would change the root/key protocol.
type secretBytes []byte

func (s *secretBytes) UnmarshalJSON(raw []byte) error {
	// encoding/json leaves an existing string unchanged for JSON null.
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	clear(*s)
	*s = append((*s)[:0], text...)
	return nil
}

// secretList delegates array reuse/null behavior to encoding/json exactly as
// the previous []string field did. Keep short-lived ownership of displaced
// buffers, including a dropped backing array, until material parsing returns.
// Clearing reused slots early would change duplicate-array/null semantics.
type secretList struct {
	values   []secretBytes
	retained map[*secretBytes][]secretBytes
}

// remember retains a view of each distinct backing array once, without copying
// its capacity on repeated fields. Existing slots are mutable decoder storage;
// replacing a nonnull string clears its old byte buffer in secretBytes first.
func (s *secretList) remember() {
	if cap(s.values) == 0 {
		return
	}
	backing := s.values[:cap(s.values)]
	if s.retained == nil {
		s.retained = make(map[*secretBytes][]secretBytes)
	}
	if _, exists := s.retained[&backing[0]]; !exists {
		s.retained[&backing[0]] = backing
	}
}

func (s *secretList) UnmarshalJSON(raw []byte) error {
	s.remember()
	return json.Unmarshal(raw, &s.values)
}

func (s *secretList) clear() {
	s.remember() // the current array may be newly allocated by the last field
	for _, backing := range s.retained {
		for _, key := range backing {
			clear(key)
		}
	}
	s.values = nil
	clear(s.retained)
	s.retained = nil
}

// close owns only the session's retained root bytes and HTTP idle resources.
// It neither revokes root nor changes the recoverable encrypted package.
func (s *bootstrapSession) close() {
	clear(s.root)
	s.root = nil
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}
