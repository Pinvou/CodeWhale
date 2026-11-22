package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type privateMarshalFailure struct{}

func (privateMarshalFailure) MarshalJSON() ([]byte, error) {
	return []byte(`{"private":"must-not-escape"}`), errors.New("private encoder failure")
}

func TestWriteJSONRejectsEncodingFailureBeforeSuccessHeaders(t *testing.T) {
	for _, value := range []any{func() {}, math.NaN(), privateMarshalFailure{}} {
		response := httptest.NewRecorder()
		response.Header().Set("Content-Length", "999")
		WriteJSON(response, http.StatusCreated, value)
		if response.Code != http.StatusInternalServerError || response.Body.String() != "{\"code\":\"INTERNAL_ERROR\",\"message\":\"internal server error\"}\n" || response.Header().Get("Content-Length") != "" {
			t.Fatal("encoding failure was published as success or exposed private material", response.Code)
		}
	}
	response := httptest.NewRecorder()
	WriteJSON(response, http.StatusCreated, map[string]string{"ok": "<value>"})
	if response.Code != http.StatusCreated || response.Body.String() != "{\"ok\":\"\\u003cvalue\\u003e\"}\n" || response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("ordinary JSON status, escaping or headers changed")
	}
}

type failedJSONWriter struct {
	header http.Header
	status int
	writes int
	err    error
}

func (w *failedJSONWriter) Header() http.Header    { return w.header }
func (w *failedJSONWriter) WriteHeader(status int) { w.status = status }
func (w *failedJSONWriter) Write(body []byte) (int, error) {
	w.writes++
	return len(body) / 2, w.err
}

func TestWriteJSONAbortsFailedOrShortWritesWithoutSecondResponse(t *testing.T) {
	for _, failure := range []error{io.ErrClosedPipe, nil} {
		writer := &failedJSONWriter{header: http.Header{}, err: failure}
		func() {
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Fatal("partial JSON response was not aborted")
				}
			}()
			WriteJSON(writer, http.StatusCreated, map[string]bool{"ok": true})
		}()
		if writer.status != http.StatusCreated || writer.writes != 1 {
			t.Fatal("failed delivery changed wire status or appended a second response")
		}
	}
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		response := httptest.NewRecorder()
		WriteJSON(response, status, privateMarshalFailure{})
		if response.Code != status || response.Body.Len() != 0 {
			t.Fatal("bodyless HTTP status attempted to encode or write a response body")
		}
	}
	response := httptest.NewRecorder()
	WriteJSON(response, http.StatusOK, map[string]bool{"next": true})
	var next map[string]bool
	if json.Unmarshal(response.Body.Bytes(), &next) != nil || !next["next"] {
		t.Fatal("next response did not remain usable")
	}
}

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
