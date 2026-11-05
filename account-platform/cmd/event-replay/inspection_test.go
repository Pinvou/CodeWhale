package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"testing"
	"time"
)

type failingInspectionWriter struct{}

func (failingInspectionWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func TestInspectionRetainsCompleteMetadata(t *testing.T) {
	candidate := events.DeadLetterCandidate{EventID: uuid.MustParse("00000000-0000-4000-8000-000000000001"), EventType: "synthetic.event.v1", AggregateID: uuid.MustParse("00000000-0000-4000-8000-000000000002"), AggregateVersion: 9007199254740993, EventDigest: "synthetic-digest", Attempts: 3, LastError: "synthetic prior outcome", DeadLetteredAt: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), Replayable: true}
	var output bytes.Buffer
	if err := writeInspection(&output, candidate); err != nil {
		t.Fatal(err)
	}
	var got events.DeadLetterCandidate
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != candidate {
		t.Fatalf("inspection metadata changed: %#v", got)
	}
	if !bytes.Contains(output.Bytes(), []byte("9007199254740993")) {
		t.Fatal("version lost precision")
	}
}

func TestInspectionRejectsEncodingFailure(t *testing.T) {
	var output bytes.Buffer
	err := writeInspection(&output, events.DeadLetterCandidate{DeadLetteredAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err == nil || output.Len() != 0 {
		t.Fatal("unencodable metadata appeared successful")
	}
}

func TestInspectionRejectsOutputFailure(t *testing.T) {
	if err := writeInspection(failingInspectionWriter{}, events.DeadLetterCandidate{}); err == nil {
		t.Fatal("failed output appeared successful")
	}
}
