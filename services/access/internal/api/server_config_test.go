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

func TestNewWiresServerConfig(t *testing.T) {
	st := &store.Postgres{}
	telemetry := &scannerTelemetry{}
	addresses := constructorAddressBook{}
	mailer := constructorMailer{}
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
	})

	if server.st != st || server.devices != st || server.redeliveries != st {
		t.Fatal("store was not wired to every production store dependency")
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
	if server.publicURL != "https://tickets.example.test" {
		t.Fatalf("publicURL = %q", server.publicURL)
	}
}
