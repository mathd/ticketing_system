//go:build smoke

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	commercestore "ticketing/services/commerce/internal/store"
)

// Resuming an exchange interrupted after the money moved (TKT-167, ADR-039).
//
// THE TIER IS THE POINT. TKT-158 persists the settlement basis before calling payments and
// then does not use it on the one path that needs it: an unsettled replay re-prices through
// catalog and re-submits the target hold before it ever loads the basis. Neither half of
// that is visible anywhere else:
//
//   - the store tier has the row but no handler, so it cannot observe that a retry made a
//     catalog call at all — the defect is a SEQUENCE of external calls, not a row state;
//   - the existing api tier (exchange_target_test.go) has a handler but no database, so it
//     cannot construct "basis_at set, settled_at NULL", which is the entire precondition;
//   - the gateway suite in smoke/ drives the real stack, where catalog cannot be made to
//     fail on the second request only and provider calls cannot be counted.
//
// So this file is the first DB-backed test under commerce's internal/api. scripts/smoke.sh
// already runs `./internal/...` for exactly this eventuality — the comment there said
// "nothing lives there for commerce yet" — and now also creates this package its own
// database, which the same script's history says is not optional (see exchangeAPIDB).
//
// THE INTERRUPTION IS DRIVEN THROUGH THE HANDLER, never seeded. Writing basis_at into
// order_exchanges directly would produce a green test whose precondition the code under
// test never created — the handler's own basis write would go unexercised, and a fixture
// that constructs the state it is supposed to observe cannot fail for the right reason.
// Here the first request runs the real forward path and is interrupted by a payments stub
// that refuses the journal call, which is a real 503 branch in the handler.

// exchangeStack is a commerce server wired to real PostgreSQL and three counting stubs.
type exchangeStack struct {
	db      *sql.DB
	handler http.Handler
	token   string

	catalog   *countingStub
	inventory *countingStub
	payments  *countingStub
}

// countingStub is one dependency. Every stub counts its calls, because on the resume path
// the COUNT is the assertion: "resumes from the basis" means precisely "catalog was not
// asked again and no second hold was taken".
type countingStub struct {
	server *httptest.Server

	mu     sync.Mutex
	counts map[string]int
}

func (c *countingStub) hit(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name]++
	return c.counts[name]
}

func (c *countingStub) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

func newCountingStub(t *testing.T, h func(*countingStub, http.ResponseWriter, *http.Request)) *countingStub {
	t.Helper()
	c := &countingStub{counts: map[string]int{}}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(c, w, r)
	}))
	t.Cleanup(c.server.Close)
	return c
}

// exchangeAPIDB opens this package's OWN smoke database and migrates it.
//
// Its own, not ./internal/store's, and the rule is the repo's rather than this ticket's:
// `go test ./internal/...` runs packages concurrently, so a second store.Migrate caller
// sharing one database races the first's migration. scripts/smoke.sh records that lesson
// three times over (TKT-226, TKT-198) and dsn_isolation_smoke_test.go now fails if a
// package is added without its own DSN.
//
// Sharing also cost a real, misleading failure before it was fixed: these tests settle
// exchanges, and ./internal/store's TestExchangeSettlementOwesTheSwitchEvent counts
// `completion_outbox` rows for subject `order.exchanged` across the WHOLE table. In one
// database that count was 6 rather than 1 — which reads exactly like the outbox's
// deterministic event id having broken, in a test this ticket never touched.
func exchangeAPIDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_API_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := commercestore.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate commerce api-smoke database: %v", err)
	}
	return db, ctx
}

// exchangeFixture is one completed source order ready to be exchanged.
type exchangeFixture struct {
	organizer, order, reservation, buyer, slot, sourceType, targetType uuid.UUID
	quantity                                                           int32
	unit                                                               int64
}

func seedExchangeSource(t *testing.T, db *sql.DB, ctx context.Context, key string, quantity int32, unit int64) exchangeFixture {
	t.Helper()
	f := exchangeFixture{
		organizer: uuid.New(), order: uuid.New(), reservation: uuid.New(), buyer: uuid.New(),
		slot: uuid.New(), sourceType: uuid.New(), targetType: uuid.New(),
		quantity: quantity, unit: unit,
	}
	total := unit * int64(quantity)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,
		                         unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'EUR','completed')`,
		f.reservation, f.organizer, uuid.New(), f.slot, f.sourceType, f.buyer, quantity, unit, total); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
		f.order, f.reservation, key, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Cleanup is ordered by foreign key and, for completion_outbox, keyed the way that
	// table actually is: it has NO organizer_id column, so the obvious
	// `WHERE organizer_id=$1` is not merely useless — it errors, and these deliberately
	// ignore their errors, so it would fail silently and leave every row behind.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id IN
			(SELECT o.id FROM orders o JOIN reservations r ON r.id = o.reservation_id
			 WHERE r.organizer_id=$1)`, f.organizer)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN
			(SELECT id FROM reservations WHERE organizer_id=$1)`, f.organizer)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, f.organizer)
	})
	return f
}

// priceBody is a PriceResolution catalog would answer with. `validate` refuses anything it
// cannot fully trust, so this has to be a real one: matching organizer, a performance id, a
// currency shared by base and resolved, and exactly one of winner / fallback_reason.
func priceBody(f exchangeFixture, unit int64) string {
	return fmt.Sprintf(`{"resolver_version":1,"evaluated_at":"2026-08-17T10:00:00Z",
		"organizer_id":%q,"performance_id":%q,
		"base_price":{"amount":%d,"currency":"EUR"},
		"resolved_price":{"amount":%d,"currency":"EUR"},
		"winner":null,"fallback_reason":"no_eligible_rule","channel_code":null}`,
		f.organizer, f.slot, unit, unit)
}

// exchangeStackFor wires the server. The stubs' behaviour is per-test, driven by the two
// switches the resume cases need: whether catalog still answers, and what payments does
// with the journal call.
type stubPolicy struct {
	// catalogUnit is the price catalog answers with. Changing it between requests is how
	// the "the target price moved" case is driven.
	catalogUnit atomic.Int64
	// catalogDown makes catalog 500 — the "catalog unavailable" case.
	catalogDown atomic.Bool
	// factsFail makes payments refuse the journal call, which is the handler's 503 branch
	// and therefore the interruption: charged, not settled.
	factsFail atomic.Bool
	// finalizeFails makes inventory refuse the finalize transition, which is what a
	// TERMINAL target claim (expired or released) looks like to commerce — inventory
	// answers ErrConflict for any transition out of a terminal state.
	finalizeFails atomic.Bool
}

func exchangeStackFor(t *testing.T, db *sql.DB, f exchangeFixture, policy *stubPolicy) *exchangeStack {
	t.Helper()
	const token = "smoke-internal-token"

	catalog := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("price-resolution")
		if policy.catalogDown.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(priceBody(f, policy.catalogUnit.Load())))
	})

	// One inventory stub for three operations, distinguished by path. The HOLD counter is
	// the load-bearing one: "no new hold" is half of what resuming from the basis means.
	inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/seating"):
			c.hit("seating")
			_, _ = w.Write([]byte(`{"seated":false}`))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			c.hit("finalize")
			if policy.finalizeFails.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"conflicting terminal state"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"finalizing"}`))
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			c.hit("confirm")
			_, _ = w.Write([]byte(`{"status":"confirmed"}`))
		case strings.HasSuffix(r.URL.Path, "/holds"):
			// The stub returns a hold id DERIVED FROM THE CALL NUMBER, so a second hold is
			// a different id. Returning a constant would hide a re-hold: the resume would
			// finalize the same claim and every assertion downstream would still hold.
			//
			// Scoped by the fixture's organizer as well, because `reservations.hold_id` is
			// UNIQUE across the whole table and these tests share one database. A
			// call-number-only id made every test's first hold the same uuid, and the
			// second test to write a replacement died on the constraint — a fixture
			// collision that reads exactly like a product defect in the replacement write.
			n := c.hit("holds")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"hold_id":%q}`,
				uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, n)))
		default:
			c.hit("unexpected:" + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The payments stub converges repeats onto ONE movement per idempotency key, which is
	// what the real service does (ADR-032, payment_operations' PK). It counts DISTINCT
	// keys as well as calls, so "one movement" is measured the way payments measures it.
	payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/charges"):
			// Submissions, DISTINCT KEYS, and movements are three different numbers and
			// the test needs all three. A submission is what commerce sent; a movement is
			// what the provider did. The stub converges them the way payments does —
			// `payment_operations`' PK is (organizer, idempotency_key, ...), so a repeat
			// under one key is answered from the record and never reaches the PSP.
			c.hit("charge-submissions")
			if c.hit("charge-key:"+r.Header.Get("Idempotency-Key")) == 1 {
				c.hit("charge-movements")
			}
			_, _ = w.Write([]byte(`{"status":"captured"}`))
		case strings.HasSuffix(r.URL.Path, "/internal/psp/partial-refund"):
			c.hit("refund-submissions")
			_, _ = w.Write([]byte(`{"status":"refunded"}`))
		case strings.HasSuffix(r.URL.Path, "/internal/facts"):
			c.hit("facts")
			if policy.factsFail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			c.hit("unexpected:" + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := New(db, http.DefaultClient, catalog.server.URL, inventory.server.URL, payments.server.URL, token)
	// Mounted on a bare chi router rather than through Router(): this exercises the
	// HANDLER, and Router() would additionally impose the OpenAPI request/response
	// validator. That validator is worth having and it is not what this file is about —
	// the 202 it would police is pinned separately, in exchange_pending_test.go.
	r := chi.NewRouter()
	r.Post("/internal/orders/{id}/exchanges", srv.exchangeOrder)
	return &exchangeStack{db: db, handler: r, token: token,
		catalog: catalog, inventory: inventory, payments: payments}
}

// exchange posts one exchange request under `key`. The body is built here so every caller
// sends a byte-identical request — a retry that varies actor or reason changes the
// fingerprint and is refused as a conflict, never resumed.
func (s *exchangeStack) exchange(t *testing.T, f exchangeFixture, key string) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
		"actor":"support@example.test","reason":"wrong ticket type"}`, f.organizer, f.targetType)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// exchangeRow reads the durable facts the response cannot be trusted for.
func (s *exchangeStack) exchangeRow(t *testing.T, ctx context.Context, org uuid.UUID, key string) (settled bool, basis bool, total, delta int64, unit int64) {
	t.Helper()
	id := commercestore.ExchangeID(org, key)
	var settledAt, basisAt sql.NullTime
	var t2, d2, u2 sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT settled_at, basis_at, target_total, delta_amount, target_unit_amount
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&settledAt, &basisAt, &t2, &d2, &u2); err != nil {
		t.Fatalf("read exchange row: %v", err)
	}
	return settledAt.Valid, basisAt.Valid, t2.Int64, d2.Int64, u2.Int64
}

// interrupt drives the FIRST request to the charged-but-unsettled state, through the
// handler, and asserts it actually got there.
//
// Returned so each case can assert on the counts the forward pass produced, which is what
// makes the resume's counts meaningful: "one catalog call" only says something if the
// forward pass is known to have made exactly that one.
func interruptAfterTheMoneyMoved(t *testing.T, s *exchangeStack, ctx context.Context, f exchangeFixture, policy *stubPolicy, key string) {
	t.Helper()
	policy.factsFail.Store(true)
	code, _ := s.exchange(t, f, key)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("the interrupted attempt answered %d, want 503 (journal unavailable). "+
			"Without that failure the exchange settles and this test proves nothing about resuming", code)
	}
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("the interruption did not produce the state under test: basis=%t settled=%t, "+
			"want basis=true settled=false — charged, not settled", basis, settled)
	}
	if got := s.payments.count("charge-submissions"); got != 1 {
		t.Fatalf("the forward pass made %d charge submissions, want exactly 1 — "+
			"the money must actually have moved for the resume to be the thing under test", got)
	}
	policy.factsFail.Store(false)
}

// COS 1 — a charged-but-unsettled exchange resumes from the persisted basis.
//
// Zero further catalog calls and zero further holds: the two external dependencies whose
// re-use is the defect. And the money it settles is the money the row already recorded.
func TestAnInterruptedExchangeResumesFromItsPersistedBasis(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-basis-src", 2, 1000) // face 2000
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // target 2 × 1500 = 3000, delta +1000: an UPGRADE
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-basis-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	catalogBefore := s.catalog.count("price-resolution")
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200 — an exchange whose delta was charged must finish", code, out)
	}

	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry made %d catalog calls (was %d). A resume must not re-price: the money "+
			"already moved against the persisted basis, and catalog is a dependency this exchange no longer needs",
			got-catalogBefore, catalogBefore)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds (was %d). The target claim is already finalizing "+
			"against the basis; a second hold is a second claim on the same capacity",
			got-holdsBefore, holdsBefore)
	}

	settled, _, total, delta, unit := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled {
		t.Fatal("settled_at is still NULL after a 200 — the retry reported success without settling")
	}
	// Derived from the REQUIREMENT, not from a run: source face is 2 × 1000 and the basis
	// priced the target at 2 × 1500, so the settlement is 3000 against 2000 and the buyer
	// owes the difference, 1000. Reading these back off the row is what pins that the
	// resume used the persisted numbers rather than any it could have recomputed.
	if total != 3000 || delta != 1000 || unit != 1500 {
		t.Errorf("settled total=%d delta=%d unit=%d, want 3000/1000/1500 — the persisted basis", total, delta, unit)
	}
	if out["replay"] != true {
		t.Errorf("replay = %v, want true — a resume is not a first settlement, and an operator "+
			"reconciling a charged exchange must be able to tell the difference", out["replay"])
	}

	// The replacement is written from the basis: quantity × the basis unit, not a fresh price.
	var repUnit, repFace int64
	if err := db.QueryRowContext(ctx, `
		SELECT unit_amount, face_value_amount FROM reservations
		WHERE organizer_id=$1 AND ticket_type_id=$2`, f.organizer, f.targetType).
		Scan(&repUnit, &repFace); err != nil {
		t.Fatalf("the replacement reservation was never written: %v", err)
	}
	if repUnit != 1500 || repFace != 3000 {
		t.Errorf("replacement unit=%d face=%d, want 1500/3000 from the persisted basis", repUnit, repFace)
	}
}

// COS 2a — the resume works with CATALOG DOWN.
//
// This is the case that makes the resume necessary rather than merely tidy. Before it, an
// unsettled replay hit repriceExchangeTarget and answered 502, so a charged buyer could not
// be made whole while catalog was unreachable — recovery blocked by a dependency the
// exchange had already finished with.
func TestAnInterruptedExchangeResumesWithCatalogUnavailable(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-nocat-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-nocat-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// Catalog is now gone. Nothing the resume needs lives there any more.
	policy.catalogDown.Store(true)
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v with catalog down, want 200. A 502 here is the "+
			"pre-TKT-167 behaviour: the charged buyer stays stranded until catalog returns", code, out)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds, want 0", got-holdsBefore)
	}
	settled, _, total, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled || total != 3000 || delta != 1000 {
		t.Errorf("settled=%t total=%d delta=%d, want true/3000/1000", settled, total, delta)
	}
}

// COS 2b — the resume works when the TARGET PRICE HAS CHANGED.
//
// The second way the old path failed, and the quieter one. The target hold is keyed
// `exchange-target:<exchange id>`, and inventory's claim fingerprint covers unit_amount, so
// re-submitting the same key at a new price is ErrIdempotency rather than a replay — the
// retry is refused by a guard that is working exactly as designed.
//
// The price is moved here to a value that would settle a DIFFERENT delta if it were used,
// so the assertion distinguishes "did not re-price" from "re-priced and got lucky".
func TestAnInterruptedExchangeResumesWhenTheTargetPriceChanged(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-reprice-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // basis: 2 × 1500 = 3000, delta +1000
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-reprice-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// The organizer re-priced the target between the charge and the retry.
	policy.catalogUnit.Store(4000) // would be 2 × 4000 = 8000, delta +6000
	catalogBefore := s.catalog.count("price-resolution")
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v after a price change, want 200", code, out)
	}
	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry re-priced (%d new catalog calls) — at the NEW price, which is not the "+
			"price the buyer was charged against", got-catalogBefore)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds; inventory refuses the same key at a new unit_amount, "+
			"so this is also how the retry would have failed outright", got-holdsBefore)
	}
	settled, _, total, delta, unit := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled {
		t.Fatal("settled_at is still NULL after a 200")
	}
	if total != 3000 || delta != 1000 || unit != 1500 {
		t.Errorf("settled total=%d delta=%d unit=%d, want the BASIS 3000/1000/1500. "+
			"8000/6000/4000 would mean the resume settled the new price — money the buyer never agreed to, "+
			"and 5000 more than the provider actually moved", total, delta, unit)
	}
}

// COS 4 — the resume makes no SECOND provider movement.
//
// Stated as what it actually is, because the obvious phrasing is false. The resume DOES
// re-submit the charge, and it must: commerce cannot know whether the first submission
// reached the provider — that is the entire predicament a charged-but-unsettled exchange is
// in. What makes the second submission harmless is that it carries the SAME deterministic
// key, `exchange-charge:<exchange id>`, so payments answers it from the record it already
// holds instead of moving money again.
//
// So the invariant this tier owns is: **every submission across the interruption and every
// retry carries one key**. The other half — one key means one provider movement — is
// payments' and is proven where that mechanism lives, in
// services/payments/internal/api/settlement_charge_smoke_test.go, which counts countingPSP's
// Authorize calls and the payment_operations rows for a single key. The two facts compose.
//
// The stub here converges on the key the way payments does, so `charge-movements` reads as
// what the provider would have done; it is a MODEL of payments, not evidence about it, and
// must not be quoted as the latter. Asserting "commerce submits exactly one charge" instead
// would have been a green test asserting a defect: it fails against correct code, and the
// only way to make it pass is to have commerce decide for itself that the money already
// moved — which is precisely the guess this ticket exists to stop it making.
func TestResumingAnExchangeMakesNoSecondProviderMovement(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-once-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-once-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	if code, out := s.exchange(t, f, key); code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200", code, out)
	}
	// And a THIRD request, after the exchange is settled: the settled-replay branch must
	// not move money either. Two retries is the realistic shape of a recovery loop.
	if code, out := s.exchange(t, f, key); code != http.StatusOK {
		t.Fatalf("the second retry answered %d %v, want 200", code, out)
	}

	// ONE movement, however many submissions.
	if got := s.payments.count("charge-movements"); got != 1 {
		t.Errorf("the provider moved money %d times across one interruption and two retries, want 1", got)
	}
	// And the reason there is one: every submission named the same operation. This is the
	// assertion that would go red if the resume derived its key from anything but the
	// exchange identity — a per-request key would submit under a name payments has never
	// seen, and each one would be a fresh charge.
	expected := "exchange-charge:" + commercestore.ExchangeID(f.organizer, key).String()
	submissions := s.payments.count("charge-submissions")
	if submissions < 1 {
		t.Fatal("no charge was submitted at all — the fixture never moved money")
	}
	if got := s.payments.count("charge-key:" + expected); got != submissions {
		t.Errorf("%d of %d charge submissions carried the key %q. A submission under any other "+
			"key is one payments cannot converge, so it is a SECOND movement",
			got, submissions, expected)
	}
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("%d refund legs on an UPGRADE — the resume settled the wrong direction", got)
	}
}

// COS 4, the other money direction: an EVEN exchange calls no provider at all, and its
// resume must still complete.
//
// delta == 0 short-circuits settleExchangeDelta before any provider call, so an
// upgrade-only fixture cannot show the resume branch is reachable when nothing moved —
// every assertion above would hold with the branch deleted for this case. The interruption
// is the same journal failure; what differs is that there was never a charge to repeat.
func TestAnInterruptedEvenExchangeResumesWithoutCallingAnyProvider(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-even-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1000) // target 2 × 1000 = 2000 == source face: delta 0
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-even-1"
	policy.factsFail.Store(true)
	if code, _ := s.exchange(t, f, key); code != http.StatusServiceUnavailable {
		t.Fatalf("the interrupted even exchange answered %d, want 503", code)
	}
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("basis=%t settled=%t, want basis=true settled=false", basis, settled)
	}
	policy.factsFail.Store(false)
	policy.catalogDown.Store(true)

	catalogBefore := s.catalog.count("price-resolution")
	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200", code, out)
	}
	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry made %d catalog calls, want 0", got-catalogBefore)
	}
	settled, _, total, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled || total != 2000 || delta != 0 {
		t.Errorf("settled=%t total=%d delta=%d, want true/2000/0", settled, total, delta)
	}
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Errorf("%d charges on an even exchange, want 0 — a zero delta settles nothing", got)
	}
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("%d refunds on an even exchange, want 0", got)
	}
}

// The resume is reachable ONLY for the same request, and that is not incidental.
//
// LookupExchangeFor compares a fingerprint over source order, target type, actor and reason.
// A recovery caller that reconstructs the request from memory and gets the actor wrong is
// refused as a conflict, not resumed — so "the resume works" is a claim about a
// byte-identical retry and this pins the boundary. Every other test in this file retries
// through the same helper for exactly this reason.
func TestAResumeRequiresTheSameRequestNotJustTheSameKey(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-fp-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-fp-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// Same key, different actor.
	body := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
		"actor":"someone-else@example.test","reason":"wrong ticket type"}`, f.organizer, f.targetType)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a different request under the same key answered %d, want 409. A key names one "+
			"request or it names nothing — resuming somebody else's exchange settles money against it", rec.Code)
	}
	settled, _, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if settled {
		t.Error("the mismatched request settled the exchange")
	}
}

// A TERMINAL target claim wedges the exchange — and this test PINS that, rather than
// claiming it is fixed (ai-review pass 1, [high]).
//
// The state: the basis is recorded and the target claim has since gone `expired` or
// `released`. Inventory refuses any transition out of a terminal state, so `finalize` is a
// conflict, and the handler answers 409 forever. The `order_exchanges_one_per_source` index
// then blocks a corrected exchange, and the refund path treats any exchange row as a live
// exchange — so the source order is stuck.
//
// **It is real, and it is not this ticket's, because it is not this ticket's doing.** Before
// TKT-167 the same replay fell through to the forward path, where `holdExchangeTarget`
// replays the same `exchange-target:` key, inventory's CreateHold hits `existing.expired()`
// and returns ErrConflict, and the handler answered — verbatim — the same 409 "exchange
// target is unavailable". Same wedge, same status, same message, both before and after.
// The resume changes WHICH call refuses, not whether one does.
//
// **No money is stranded by it.** The two are ordered `finalize` then settle, and finalize
// takes the claim out of the expiry predicate (`liveClaims` counts `finalizing`
// unconditionally). So a claim that expires before finalize was never charged against, and a
// charge implies finalize already succeeded and the claim can no longer expire. The only
// other route to terminal is an explicit release, and the exchange path issues exactly one —
// `releaseExchangeHold` on a BIND failure, before any basis exists and before any money
// moves. The review's "released after capture" case has no producer in this code.
//
// So what is left is a WEDGED ORDER with no money in flight, which wants an unwind path
// (delete the exchange binding, free the order for a corrected attempt) — a new capability,
// not a fix to this diff. Pinned here in the shape ADR-021's rollback-gap test uses: if this
// test ever fails, the gap was closed and this test should be replaced by one asserting the
// new behaviour, not deleted.
func TestATerminalTargetClaimWedgesTheExchangeAndStrandsNoMoney(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "wedge-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)
	const key = "wedge-1"

	// The claim went terminal before finalize: inventory refuses the transition.
	policy.finalizeFails.Store(true)

	code, _ := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("first attempt answered %d, want 409 — a terminal target claim is refused at finalize", code)
	}
	// The basis IS durable at this point, which is what makes every retry take the resume
	// branch rather than the forward path.
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("basis=%t settled=%t, want basis=true settled=false", basis, settled)
	}

	// The retry takes the RESUME branch and reaches the same refusal.
	code, out := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("the retry answered %d %v, want 409. If this now succeeds, the terminal-claim "+
			"gap was closed — update this test to assert the new behaviour rather than deleting it", code, out)
	}

	// THE PART THAT MATTERS: no money moved, in either attempt. The wedge costs a stuck
	// order, not a stranded charge, and that is the whole reason it is not a blocker.
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Errorf("%d charge submissions against an exchange that never finalized, want 0. "+
			"finalize precedes settlement precisely so a target that cannot be secured is "+
			"refused before the buyer is charged", got)
	}
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("%d refund submissions, want 0", got)
	}
	settled, _, _, _, _ = s.exchangeRow(t, ctx, f.organizer, key)
	if settled {
		t.Error("the exchange settled despite never finalizing its target claim")
	}
}
