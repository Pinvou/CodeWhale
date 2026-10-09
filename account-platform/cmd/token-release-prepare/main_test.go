package main

import (
	"strings"
	"testing"
)

func TestReleasePreparationClosedInputs(t *testing.T) {
	base := []string{"--expected-database", "token_test"}
	r, err := parseRequest(base)
	if err != nil || r.Operation != "inventory" || r.Apply || r.Terminate {
		t.Fatalf("default must remain read only: %v", err)
	}
	valid := []string{"--operation", "prepare", "--expected-database", "token_test", "--approval-reference", "approval-1", "--maintenance-reference", "window-1", "--candidate-roles", "token_a,token_b"}
	if _, err = parseRequest(valid); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		append(append([]string{}, base...), "--apply"),
		append(append([]string{}, base...), "--terminate-sessions"),
		{"--expected-database", "wrong database"},
		{"--expected-database", strings.Repeat("a", 64)},
		{"--expected-database", "token_test", "--operation", "activate"},
		{"--expected-database", "token_test", "--password", "not-a-real-secret"},
		{"--expected-database", "token_test", "--operation", "prepare"},
	} {
		if _, err = parseRequest(args); err == nil || strings.Contains(err.Error(), "not-a-real-secret") {
			t.Fatalf("unsafe input accepted or disclosed: %v", err)
		}
	}
	for _, roles := range []string{"token_b,token_a", "token_a,token_a", "token_gateway,token_new", "token_a,token_migrator", "token_a,token_schema_owner", "token_a,token-b", "token_a"} {
		args := append([]string{}, valid...)
		args[len(args)-1] = roles
		if _, err = parseRequest(args); err == nil {
			t.Fatal("invalid candidate set accepted")
		}
	}
}

func TestReleaseWriterCoverageIncludesOldSessionsAndNoInherit(t *testing.T) {
	writers := []writerIdentity{{Name: "dba", Login: true, Super: true}, {Name: "old_writer", Login: true}, {Name: "old_member", Sessions: 1}}
	if err := noUnexpectedWriters(writers, "dba", []string{"old_writer"}, false); err == nil {
		t.Fatal("unlisted NOLOGIN session accepted")
	}
	if err := noUnexpectedWriters(writers, "dba", []string{"old_member", "old_writer"}, false); err != nil {
		t.Fatal(err)
	}
	if err := noUnexpectedWriters(writers, "dba", []string{"old_member", "old_writer"}, true); err == nil {
		t.Fatal("live writer considered quiesced")
	}
	writers[1].Login = false
	writers[2].Sessions = 0
	if err := noUnexpectedWriters(writers, "dba", nil, true); err != nil {
		t.Fatal(err)
	}
	writers[0].Sessions = 1
	if err := noUnexpectedWriters(writers, "dba", nil, true); err == nil {
		t.Fatal("same-name DBA connection silently excluded")
	}
	writers[0].Sessions = 0
	writers = append(writers, writerIdentity{Name: "other_dba", Super: true, Sessions: 1})
	if err := noUnexpectedWriters(writers, "dba", nil, true); err == nil {
		t.Fatal("other DBA session silently excluded")
	}
}
