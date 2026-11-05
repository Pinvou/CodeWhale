package token

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestNewAPIGatewayAdapterUsesOnlyItsConfiguredProviderCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-flash"}]}`))
	}))
	defer server.Close()

	caFile := filepath.Join(t.TempDir(), "provider-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewNewAPIGatewayAdapterWithCA(caFile)
	if err != nil {
		t.Fatal(err)
	}
	models, err := adapter.DiscoverModels(context.Background(), server.URL, []byte("service-token"))
	if err != nil || len(models) != 1 || models[0].UpstreamModelRef != "deepseek-flash" {
		t.Fatalf("provider-specific CA was not applied: models=%v err=%v", models, err)
	}
	if _, err := NewNewAPIGatewayAdapterWithCA(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("missing provider CA was accepted")
	}
}

func TestRouteHealthProbeTargetRequiresProviderAdapterAndAllowedURL(t *testing.T) {
	policy, err := NewProviderURLPolicy("development", []string{"provider.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	worker := &RouteHealthWorker{probe: routeHealthProbeStub{}, policy: policy}
	credential := Credential{Provider: "provider", BaseURL: "https://provider.invalid"}
	target := routeProbeTarget{Provider: "provider", AdapterVersion: "adapter-v1"}
	if !worker.safeProbeTarget(credential, target) {
		t.Fatal("compatible provider, adapter and allowlisted URL were rejected")
	}
	for name, mutate := range map[string]func(*Credential, *routeProbeTarget){
		"provider mismatch": func(credential *Credential, _ *routeProbeTarget) { credential.Provider = "other" },
		"adapter mismatch":  func(_ *Credential, target *routeProbeTarget) { target.AdapterVersion = "adapter-v2" },
		"URL rejected":      func(credential *Credential, _ *routeProbeTarget) { credential.BaseURL = "http://127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			candidateCredential, candidateTarget := credential, target
			mutate(&candidateCredential, &candidateTarget)
			if worker.safeProbeTarget(candidateCredential, candidateTarget) {
				t.Fatal("unsafe probe target was accepted")
			}
		})
	}
}

func TestHTTPGatewayAdapterDoesNotFollowModelDiscoveryRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"redirected-model"}]}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/v1/models")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	models, err := NewHTTPUpstreamClient().DiscoverModels(context.Background(), redirect.URL, []byte("secret"))
	if err == nil || len(models) != 0 {
		t.Fatalf("redirect response was accepted: models=%v err=%v", models, err)
	}
	if targetRequests.Load() != 0 {
		t.Fatal("authorization-bearing model discovery followed a redirect")
	}
}
