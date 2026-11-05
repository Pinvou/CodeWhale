package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"known":true,"unknown":true}`))
	var body struct {
		Known bool `json:"known"`
	}
	if err := DecodeJSON(recorder, request, &body); err == nil {
		t.Fatal("expected an unknown-field error")
	}
}

func TestDecodeJSONRejectsTrailingValue(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{} {}`))
	var body struct{}
	if err := DecodeJSON(recorder, request, &body); err == nil {
		t.Fatal("expected a trailing-value error")
	}
}
