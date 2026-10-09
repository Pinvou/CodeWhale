package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestKeycloakAtomicProfileChangeFencesLateRequestsAcrossDirectoryInstances(t *testing.T) {
	type changeRequest struct {
		OperationID uuid.UUID `json:"operationId"`
		Fence       int64     `json:"fenceVersion"`
		Field       string    `json:"field"`
		Value       string    `json:"value"`
	}
	var stateMu sync.Mutex
	username := "old-name"
	mobile := "+8613800138000"
	var appliedOperation uuid.UUID
	var appliedFence int64
	oldReceived := make(chan struct{})
	releaseOld := make(chan struct{})
	oldOperation := uuid.Must(uuid.NewV7())
	newOperation := uuid.Must(uuid.NewV7())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":300}`))
			return
		}
		if r.URL.Path != "/realms/pinvou/pinvou-identity-security/users/subject/profile-change" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request changeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.OperationID == oldOperation {
			close(oldReceived)
			<-releaseOld
		}
		stateMu.Lock()
		defer stateMu.Unlock()
		if appliedFence > request.Fence || appliedFence == request.Fence && appliedOperation != uuid.Nil && appliedOperation != request.OperationID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "IDENTITY_SECURITY_FENCE_CONFLICT"})
			return
		}
		if appliedFence == request.Fence && appliedOperation == request.OperationID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Field == "USERNAME" {
			username = request.Value
		} else {
			mobile = request.Value
		}
		appliedOperation = request.OperationID
		appliedFence = request.Fence
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	newDirectory := func() *KeycloakDirectory {
		admin, _ := url.Parse(server.URL)
		token, _ := url.Parse(server.URL + "/token")
		return &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: "pinvou", clientID: "client", clientSecret: "secret", client: &http.Client{Timeout: 2 * time.Second}}
	}
	firstInstance, secondInstance := newDirectory(), newDirectory()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- firstInstance.UpdateMobile(context.Background(), "subject", oldOperation, 1, "+8613800138001")
	}()
	<-oldReceived
	if err := secondInstance.UpdateUsername(context.Background(), "subject", newOperation, 2, "new-name"); err != nil {
		t.Fatal(err)
	}
	close(releaseOld)
	if err := <-oldDone; !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("late old operation error=%v", err)
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if username != "new-name" || mobile != "+8613800138000" || appliedOperation != newOperation || appliedFence != 2 {
		t.Fatalf("atomic profile state username=%q mobile=%q operation=%s fence=%d", username, mobile, appliedOperation, appliedFence)
	}
}

func TestKeycloakAtomicProfileChangeCannotOverwriteNewerPasswordFence(t *testing.T) {
	type command struct {
		OperationID uuid.UUID `json:"operationId"`
		Fence       int64     `json:"fenceVersion"`
		Value       string    `json:"value"`
		Password    *string   `json:"password"`
	}
	var stateMu sync.Mutex
	username, password := "old-name", "old-password"
	var appliedOperation uuid.UUID
	var appliedFence int64
	oldReceived := make(chan struct{})
	releaseOld := make(chan struct{})
	oldOperation := uuid.Must(uuid.NewV7())
	newOperation := uuid.Must(uuid.NewV7())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":300}`))
			return
		}
		var request command
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.OperationID == oldOperation {
			close(oldReceived)
			<-releaseOld
		}
		stateMu.Lock()
		defer stateMu.Unlock()
		if appliedFence > request.Fence {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			code := "IDENTITY_SECURITY_FENCE_CONFLICT"
			if strings.HasSuffix(r.URL.Path, "/password-reset") {
				code = "PASSWORD_REFERENCE_CONFLICT"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/profile-change") {
			username = request.Value
		} else if strings.HasSuffix(r.URL.Path, "/password-reset") && request.Password != nil {
			password = *request.Password
		} else {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		appliedOperation, appliedFence = request.OperationID, request.Fence
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	newDirectory := func() *KeycloakDirectory {
		admin, _ := url.Parse(server.URL)
		token, _ := url.Parse(server.URL + "/token")
		return &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: "pinvou", clientID: "client", clientSecret: "secret", client: server.Client()}
	}
	firstInstance, secondInstance := newDirectory(), newDirectory()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- firstInstance.UpdateUsername(context.Background(), "subject", oldOperation, 1, "stale-name")
	}()
	<-oldReceived
	if err := secondInstance.SetPasswordForOperation(context.Background(), "subject", newOperation, 2, "new-password"); err != nil {
		t.Fatal(err)
	}
	close(releaseOld)
	if err := <-oldDone; !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("late profile operation error=%v", err)
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if username != "old-name" || password != "new-password" || appliedOperation != newOperation || appliedFence != 2 {
		t.Fatalf("atomic mixed state username=%q password=%q operation=%s fence=%d", username, password, appliedOperation, appliedFence)
	}
}

func TestKeycloakAtomicPasswordResetFencesLateRequestsAcrossDirectoryInstances(t *testing.T) {
	type resetRequest struct {
		OperationID uuid.UUID `json:"operationId"`
		Fence       int64     `json:"fenceVersion"`
		Password    *string   `json:"password"`
	}
	var stateMu sync.Mutex
	var appliedOperation uuid.UUID
	var appliedFence int64
	var appliedPassword string
	oldReceived := make(chan struct{})
	releaseOld := make(chan struct{})
	oldOperation := uuid.Must(uuid.NewV7())
	newOperation := uuid.Must(uuid.NewV7())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":300}`))
			return
		}
		if r.URL.Path != "/realms/pinvou/pinvou-identity-security/users/subject/password-reset" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request resetRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.OperationID == oldOperation && request.Password != nil {
			close(oldReceived)
			<-releaseOld
		}
		stateMu.Lock()
		defer stateMu.Unlock()
		if appliedFence > request.Fence || appliedFence == request.Fence && appliedOperation != uuid.Nil && appliedOperation != request.OperationID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "PASSWORD_REFERENCE_CONFLICT"})
			return
		}
		if appliedFence == request.Fence && appliedOperation == request.OperationID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Password == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "PASSWORD_RESET_NOT_APPLIED"})
			return
		}
		appliedOperation = request.OperationID
		appliedFence = request.Fence
		appliedPassword = *request.Password
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	newDirectory := func() *KeycloakDirectory {
		admin, _ := url.Parse(server.URL)
		token, _ := url.Parse(server.URL + "/token")
		return &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: "pinvou", clientID: "client", clientSecret: "secret", client: &http.Client{Timeout: 2 * time.Second}}
	}
	firstInstance, secondInstance := newDirectory(), newDirectory()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- firstInstance.SetPasswordForOperation(context.Background(), "subject", oldOperation, 1, "old-password")
	}()
	<-oldReceived
	if err := secondInstance.SetPasswordForOperation(context.Background(), "subject", newOperation, 2, "new-password"); err != nil {
		t.Fatal(err)
	}
	close(releaseOld)
	if err := <-oldDone; !errors.Is(err, ErrReferenceConflict) {
		t.Fatalf("late old operation error=%v", err)
	}
	applied, err := firstInstance.PasswordResetApplied(context.Background(), "subject", newOperation, 2)
	if err != nil || !applied {
		t.Fatalf("response-loss probe applied=%v err=%v", applied, err)
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if appliedOperation != newOperation || appliedFence != 2 || appliedPassword != "new-password" {
		t.Fatalf("atomic state operation=%s fence=%d password=%q", appliedOperation, appliedFence, appliedPassword)
	}
}

func TestKeycloakAtomicPasswordResetDoesNotTreatAMissingEndpointAsNotApplied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	admin, _ := url.Parse(server.URL)
	token, _ := url.Parse(server.URL + "/token")
	directory := &KeycloakDirectory{adminURL: admin, tokenURL: token, realm: "pinvou", clientID: "client", clientSecret: "secret", client: server.Client()}
	applied, err := directory.PasswordResetApplied(context.Background(), "subject", uuid.Must(uuid.NewV7()), 1)
	if err == nil || applied {
		t.Fatalf("missing atomic endpoint applied=%v err=%v", applied, err)
	}
}
