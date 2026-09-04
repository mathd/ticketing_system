package api

import (
	"testing"
	"time"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

func newTestServer(journal *store.Journal, credential string) *Server {
	return newTestServerWithPSP(journal, credential, psp.NewFake())
}

func newTestServerWithPSP(journal *store.Journal, credential string, provider psp.PSP) *Server {
	return New(ServerConfig{
		Journal:    journal,
		Credential: credential,
		Provider:   provider,
	})
}

func TestNewWiresServerConfig(t *testing.T) {
	provider := psp.NewFake()
	server := New(ServerConfig{
		Credential:            "internal",
		Provider:              provider,
		StatusReplayRetention: 24 * time.Hour,
	})

	if server.credential != "internal" {
		t.Fatalf("credential = %q", server.credential)
	}
	if server.psp != provider {
		t.Fatal("configured PSP was not wired")
	}
	if server.statusReplayRetention != 24*time.Hour {
		t.Fatalf("status replay retention = %v", server.statusReplayRetention)
	}
}
