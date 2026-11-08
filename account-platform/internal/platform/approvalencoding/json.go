package approvalencoding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

type jsonKind uint8

const (
	jsonNull jsonKind = iota
	jsonBool
	jsonStringKind
	jsonNumber
	jsonArray
	jsonObject
)

type jsonMember struct {
	name  string
	value *jsonValue
}

type jsonValue struct {
	kind    jsonKind
	boolean bool
	text    string
	array   []*jsonValue
	object  []jsonMember
}

func parseDocument(raw []byte) (*jsonValue, error) {
	if len(raw) == 0 || len(raw) > MaximumDocumentBytes {
		if len(raw) > MaximumDocumentBytes {
			return nil, ErrDocumentTooLarge
		}
		return nil, ErrInvalidDocument
	}
	if !utf8.Valid(raw) || !validEscapedSurrogates(raw) {
		return nil, ErrInvalidDocument
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidDocument
	}
	return value, nil
}

func parseValue(decoder *json.Decoder, depth int) (*jsonValue, error) {
	if depth > maximumJSONDepth {
		return nil, ErrInvalidDocument
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, ErrInvalidDocument
	}
	switch typed := token.(type) {
	case nil:
		return &jsonValue{kind: jsonNull}, nil
	case bool:
		return &jsonValue{kind: jsonBool, boolean: typed}, nil
	case string:
		return &jsonValue{kind: jsonStringKind, text: typed}, nil
	case json.Number:
		return &jsonValue{kind: jsonNumber, text: typed.String()}, nil
	case json.Delim:
		switch typed {
		case '{':
			result := &jsonValue{kind: jsonObject}
			seen := make(map[string]struct{})
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return nil, ErrInvalidDocument
				}
				name, ok := nameToken.(string)
				if !ok {
					return nil, ErrInvalidDocument
				}
				if _, duplicate := seen[name]; duplicate {
					return nil, ErrInvalidDocument
				}
				seen[name] = struct{}{}
				value, err := parseValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				result.object = append(result.object, jsonMember{name: name, value: value})
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrInvalidDocument
			}
			return result, nil
		case '[':
			result := &jsonValue{kind: jsonArray}
			for decoder.More() {
				value, err := parseValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				result.array = append(result.array, value)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrInvalidDocument
			}
			return result, nil
		}
	}
	return nil, ErrInvalidDocument
}

func validEscapedSurrogates(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); {
		if !inString {
			if raw[i] == '"' {
				inString = true
			}
			i++
			continue
		}
		switch raw[i] {
		case '"':
			inString = false
			i++
		case '\\':
			if i+1 >= len(raw) {
				return false
			}
			if raw[i+1] != 'u' {
				i += 2
				continue
			}
			code, ok := parseHex16(raw, i+2)
			if !ok {
				return false
			}
			if code >= 0xd800 && code <= 0xdbff {
				if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
					return false
				}
				low, ok := parseHex16(raw, i+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 12
				continue
			}
			if code >= 0xdc00 && code <= 0xdfff {
				return false
			}
			i += 6
		default:
			i++
		}
	}
	return !inString
}

func parseHex16(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var result uint16
	for _, current := range raw[start : start+4] {
		result <<= 4
		switch {
		case current >= '0' && current <= '9':
			result += uint16(current - '0')
		case current >= 'a' && current <= 'f':
			result += uint16(current-'a') + 10
		case current >= 'A' && current <= 'F':
			result += uint16(current-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

func canonicalJSON(value *jsonValue) ([]byte, error) {
	var output bytes.Buffer
	if err := appendCanonical(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func appendCanonical(output *bytes.Buffer, value *jsonValue) error {
	switch value.kind {
	case jsonNull:
		output.WriteString("null")
	case jsonBool:
		output.WriteString(strconv.FormatBool(value.boolean))
	case jsonStringKind:
		appendJSONString(output, value.text)
	case jsonNumber:
		integer, err := strconv.ParseInt(value.text, 10, 64)
		if err != nil || strconv.FormatInt(integer, 10) != value.text {
			return ErrInvalidDocument
		}
		output.WriteString(strconv.FormatInt(integer, 10))
	case jsonArray:
		output.WriteByte('[')
		for index, item := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := appendCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case jsonObject:
		members := append([]jsonMember(nil), value.object...)
		sort.Slice(members, func(left, right int) bool {
			return compareUTF16(members[left].name, members[right].name) < 0
		})
		output.WriteByte('{')
		for index, member := range members {
			if index > 0 {
				output.WriteByte(',')
			}
			appendJSONString(output, member.name)
			output.WriteByte(':')
			if err := appendCanonical(output, member.value); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return ErrInvalidDocument
	}
	return nil
}

func appendJSONString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(current)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if current < 0x20 {
				fmt.Fprintf(output, `\u%04x`, current)
			} else {
				output.WriteRune(current)
			}
		}
	}
	output.WriteByte('"')
}

func compareUTF16(left, right string) int {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	limit := len(leftUnits)
	if len(rightUnits) < limit {
		limit = len(rightUnits)
	}
	for index := 0; index < limit; index++ {
		if leftUnits[index] < rightUnits[index] {
			return -1
		}
		if leftUnits[index] > rightUnits[index] {
			return 1
		}
	}
	switch {
	case len(leftUnits) < len(rightUnits):
		return -1
	case len(leftUnits) > len(rightUnits):
		return 1
	default:
		return 0
	}
}

func objectValue(value *jsonValue) (map[string]*jsonValue, error) {
	if value == nil || value.kind != jsonObject {
		return nil, ErrInvalidDocument
	}
	result := make(map[string]*jsonValue, len(value.object))
	for _, member := range value.object {
		result[member.name] = member.value
	}
	return result, nil
}

func requireObject(value *jsonValue, keys ...string) (map[string]*jsonValue, error) {
	object, err := objectValue(value)
	if err != nil || len(object) != len(keys) {
		return nil, ErrInvalidDocument
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return nil, ErrInvalidDocument
		}
	}
	return object, nil
}

func stringValue(value *jsonValue) (string, error) {
	if value == nil || value.kind != jsonStringKind {
		return "", ErrInvalidDocument
	}
	return value.text, nil
}

func integerNumber(value *jsonValue) (int64, error) {
	if value == nil || value.kind != jsonNumber {
		return 0, ErrInvalidDocument
	}
	result, err := strconv.ParseInt(value.text, 10, 64)
	if err != nil || strconv.FormatInt(result, 10) != value.text {
		return 0, ErrInvalidDocument
	}
	return result, nil
}

func decimalString(value *jsonValue) (int64, error) {
	text, err := stringValue(value)
	if err != nil || text == "" || text == "-0" || text[0] == '+' {
		return 0, ErrInvalidDocument
	}
	if text != "0" {
		start := 0
		if text[0] == '-' {
			start = 1
		}
		if start == len(text) || text[start] == '0' {
			return 0, ErrInvalidDocument
		}
		for _, digit := range text[start:] {
			if digit < '0' || digit > '9' {
				return 0, ErrInvalidDocument
			}
		}
	}
	result, parseErr := strconv.ParseInt(text, 10, 64)
	if parseErr != nil || strconv.FormatInt(result, 10) != text {
		return 0, ErrInvalidDocument
	}
	return result, nil
}

func convertLegacyInteger(object map[string]*jsonValue, field string) (int64, error) {
	value, ok := object[field]
	if !ok {
		return 0, ErrInvalidDocument
	}
	integer, err := integerNumber(value)
	if err != nil {
		return 0, err
	}
	*value = jsonValue{kind: jsonStringKind, text: strconv.FormatInt(integer, 10)}
	return integer, nil
}

func cloneValue(value *jsonValue) *jsonValue {
	if value == nil {
		return nil
	}
	result := &jsonValue{kind: value.kind, boolean: value.boolean, text: value.text}
	for _, item := range value.array {
		result.array = append(result.array, cloneValue(item))
	}
	for _, member := range value.object {
		result.object = append(result.object, jsonMember{name: member.name, value: cloneValue(member.value)})
	}
	return result
}

func newObject(values map[string]*jsonValue) *jsonValue {
	result := &jsonValue{kind: jsonObject}
	for name, value := range values {
		result.object = append(result.object, jsonMember{name: name, value: value})
	}
	return result
}

func jsonString(value string) *jsonValue { return &jsonValue{kind: jsonStringKind, text: value} }
func jsonInteger(value int64) *jsonValue {
	return &jsonValue{kind: jsonNumber, text: strconv.FormatInt(value, 10)}
}
func jsonBoolean(value bool) *jsonValue { return &jsonValue{kind: jsonBool, boolean: value} }
func jsonNullValue() *jsonValue         { return &jsonValue{kind: jsonNull} }
