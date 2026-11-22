//go:build smoke

package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/bulkrefund"
	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-498 (R2-2, RA2-1). The replacement refund guard's Access read, on the Server that New
// builds. cmd/commerce passes srvHandler.Refunds() to the cancellation runner, so the staff
// endpoint and the bulk runner below drive the same refund service that production wires.

// wiringDependencies is payments (and Access, when `accessHere`) under one origin. It records
// the X-Internal-Token of every admission read and counts partial refunds.
type wiringDependencies struct {
	server        *httptest.Server
	mu            sync.Mutex
	accessTokens  []string
	refundsPosted int
}

func newWiringDependencies(t *testing.T) *wiringDependencies {
	t.Helper()
	d := &wiringDependencies{}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/admission"):
			d.accessTokens = append(d.accessTokens, r.Header.Get("X-Internal-Token"))
			_, _ = w.Write([]byte(`{"admitted":false,"issued_count":2}`))
		case strings.HasSuffix(r.URL.Path, "/internal/psp/partial-refund"):
			d.refundsPosted++
			_, _ = w.Write([]byte(`{"status":"refunded","fact_id":"` + uuid.NewString() + `","amount":1,"currency":"EUR","replay":false}`))
		case strings.HasSuffix(r.URL.Path, "/internal/facts"):
			_, _ = w.Write([]byte(`{"sequence":1,"hash":"00","replay":false}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *wiringDependencies) observed() (tokens []string, refunds int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.accessTokens...), d.refundsPosted
}

func productionWiredServer(t *testing.T, db *sql.DB, accessURL, paymentsURL string) *Server {
	t.Helper()
	return New(ServerConfig{
		DB: db, Client: &http.Client{Timeout: 5 * time.Second},
		AccessURL: accessURL, PaymentsURL: paymentsURL, InventoryURL: "http://inventory.invalid",
		InternalToken: internalTok, PaymentsToken: wiringPaymentsTok,
		StaffWriteToken: staffTok, OrganizerAssertions: testOrgVerifier(t),
	})
}

// refundReplacement drives one refund of the fixture's replacement order through the named
// production caller. The tests judge it by what payments received.
func refundReplacement(t *testing.T, ctx context.Context, caller string, s *Server, db *sql.DB, f replacementFixture) {
	t.Helper()
	switch caller {
	case "staff":
		staffRefund(t, s, f.organizer, f.replacementOrder, "wiring-"+f.exchange.String())
	case "bulk":
		if _, err := commercestore.BindCancellationRun(ctx, db, commercestore.CancellationRunRequest{
			OrganizerID: f.organizer, SlotID: f.targetSlot, IdempotencyKey: "cancel-" + f.targetSlot.String(),
			Actor: "ops", Reason: "target slot cancelled",
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE organizer_id=$1`, f.organizer)
			_, _ = db.Exec(`DELETE FROM cancellation_refund_runs WHERE organizer_id=$1`, f.organizer)
		})
		bulkrefund.New(bulkrefund.DBStore{DB: db}, s.Refunds(), time.Minute, 10, time.Minute).RunOnce(ctx)
	default:
		t.Fatalf("unknown caller %q", caller)
	}
}

// A redirect from Access leaves the replacement refund unresolved: the redirect target gets no
// request, and payments gets no partial refund. Without the no-redirect rule, the target's
// "two issued tickets" answer would let the refund reach payments.
func TestReplacementRefundAdmissionReadNeverFollowsARedirect(t *testing.T) {
	for _, caller := range []string{"staff", "bulk"} {
		t.Run(caller, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			f := seedSettledReplacement(t, db, ctx)
			target := newRecordingServer(t, `{"admitted":false,"issued_count":2}`)
			access := redirectingAccess(t, http.StatusTemporaryRedirect, target.URL)
			payments := newWiringDependencies(t)
			s := productionWiredServer(t, db, access.URL, payments.server.URL)

			refundReplacement(t, ctx, caller, s, db, f)
			if got := target.received(); len(got) != 0 {
				t.Fatalf("the redirect target received %d request(s) with tokens %q, want 0", len(got), got)
			}
			if _, n := payments.observed(); n != 0 {
				t.Fatalf("payments received %d partial refund(s), want 0", n)
			}
		})
	}
}

// Access configured under the payments origin, so its URL starts with the payments URL: the
// admission read still carries the internal token, never the payments credential.
func TestReplacementRefundAdmissionReadSendsTheInternalTokenUnderASharedPrefix(t *testing.T) {
	for _, caller := range []string{"staff", "bulk"} {
		t.Run(caller, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			f := seedSettledReplacement(t, db, ctx)
			deps := newWiringDependencies(t)
			s := productionWiredServer(t, db, deps.server.URL+"/access", deps.server.URL)

			refundReplacement(t, ctx, caller, s, db, f)
			tokens, refunds := deps.observed()
			if len(tokens) != 1 || tokens[0] != internalTok {
				t.Fatalf("Access received tokens %q, want exactly [%q] (never the payments credential)", tokens, internalTok)
			}
			if refunds != 1 {
				t.Fatalf("payments received %d partial refund(s), want 1", refunds)
			}
		})
	}
}
