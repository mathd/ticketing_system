package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/access/internal/delivery"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

type constructorAddressBook struct{}

func (constructorAddressBook) DeliveryEmail(context.Context, uuid.UUID) (string, error) {
	return "buyer@example.test", nil
}

type constructorZones struct{ name string }

func (constructorZones) PerformanceZones(context.Context, []uuid.UUID) map[uuid.UUID]string {
	return nil
}

type constructorMailer struct{}

func (constructorMailer) Send(context.Context, uuid.UUID, string, string) error { return nil }

func newTestServer(st *store.Postgres, verifier *ticket.Verifier, token ...string) *Server {
	config := ServerConfig{Store: st, Verifier: verifier}
	if len(token) > 0 {
		config.InternalToken = token[0]
	}
	return New(config)
}

func (s *Server) WithRedelivery(addresses delivery.AddressBook, mailer delivery.Mailer, publicURL string) *Server {
	s.addresses, s.mailer, s.publicURL = addresses, mailer, publicURL
	return s
}

func (s *Server) WithQRLinkKey(key string) *Server {
	s.qrLinks = qrLinkSigner{key: []byte(key)}
	return s
}

func (s *Server) WithFeedCursorKey(key string) *Server {
	s.cursors = feedCursorSigner{key: []byte(key)}
	return s
}

func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

func (s *Server) WithScannerTelemetry(telemetry *scannerTelemetry) *Server {
	s.telemetry = telemetry
	return s
}

// WithAdmissions replaces the admission port (TKT-557). Tests only.
func (s *Server) WithAdmissions(admissions scannerAdmissionStore) *Server {
	s.admissions = admissions
	return s
}

// withSpec makes Router enforce an isolated cloned contract (D9). Tests only;
// production always uses the embedded spec.
func (s *Server) withSpec(spec []byte) *Server {
	s.spec = spec
	return s
}

func TestNewWiresServerConfig(t *testing.T) {
	st := &store.Postgres{}
	telemetry := &scannerTelemetry{}
	addresses := constructorAddressBook{}
	mailer := constructorMailer{}
	zones := constructorZones{name: "catalog"}
	server := New(ServerConfig{
		Store:             st,
		InternalToken:     "internal",
		StaffWriteToken:   "staff",
		QRLinkKey:         "qr-key",
		FeedCursorKey:     "feed-key",
		ScannerTelemetry:  telemetry,
		RedeliveryAddress: addresses,
		RedeliveryMailer:  mailer,
		PublicURL:         "https://tickets.example.test",
		PerformanceZones:  zones,
	})

	if server.st != st || server.devices != st || server.redeliveries != st || server.admissions != st {
		t.Fatal("store was not wired to every production store dependency")
	}
	if server.spec != nil {
		t.Fatal("New set a contract override: production must enforce the embedded spec")
	}
	if server.token != "internal" || server.staffWriteToken != "staff" {
		t.Fatal("credentials were not wired from ServerConfig")
	}
	if string(server.qrLinks.key) != "qr-key" || string(server.cursors.key) != "feed-key" {
		t.Fatal("signing keys were not wired from ServerConfig")
	}
	if server.telemetry != telemetry || server.addresses != addresses || server.mailer != mailer {
		t.Fatal("runtime dependencies were not wired from ServerConfig")
	}
	if server.zones != zones {
		t.Fatal("the performance zone reader was not wired from ServerConfig")
	}
	if server.publicURL != "https://tickets.example.test" {
		t.Fatalf("publicURL = %q", server.publicURL)
	}
}

// No store, or a nil one, leaves every store port a true nil interface — never a
// typed nil that passes a != nil check and then panics (TKT-557, D8/D16).
func TestNewLeavesPortsNilWithoutAStore(t *testing.T) {
	for name, config := range map[string]ServerConfig{
		"absent":    {},
		"typed nil": {Store: (*store.Postgres)(nil)},
	} {
		server := New(config)
		if server.admissions != nil || server.devices != nil || server.redeliveries != nil {
			t.Fatalf("%s store: ports = %v/%v/%v, want true nil interfaces", name, server.admissions, server.devices, server.redeliveries)
		}
	}
}
