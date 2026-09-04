package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	commerceevents "ticketing/services/commerce/internal/events"
	commercestore "ticketing/services/commerce/internal/store"
)

func newTestServer(db *sql.DB, client *http.Client, catalog, inventory, payments, token string, publishers ...commerceevents.Publisher) *Server {
	var publisher commerceevents.Publisher
	if len(publishers) > 0 {
		publisher = publishers[0]
	}
	return New(ServerConfig{
		DB:            db,
		Client:        client,
		CatalogURL:    catalog,
		InventoryURL:  inventory,
		PaymentsURL:   payments,
		InternalToken: token,
		Publisher:     publisher,
	})
}

func (s *Server) WithPaymentsToken(token string) *Server {
	s.paymentsToken = token
	return s
}

func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

func (s *Server) WithPublicURL(public string) *Server {
	s.publicURL = strings.TrimSuffix(public, "/")
	return s
}

func TestNewWiresAccessIntoRefundReversal(t *testing.T) {
	var requestedPath string
	access := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(access.Close)

	server := New(ServerConfig{
		Client:    access.Client(),
		AccessURL: access.URL + "/",
	})
	orderID := uuid.New()
	server.Refunds().DriveReversal(t.Context(), commercestore.Refund{
		ID:          uuid.New(),
		OrderID:     orderID,
		OrganizerID: uuid.New(),
	})

	want := "/internal/orders/" + orderID.String() + "/refunds"
	if requestedPath != want {
		t.Fatalf("access request path = %q, want %q", requestedPath, want)
	}
}
