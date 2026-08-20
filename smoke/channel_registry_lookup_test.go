//go:build smoke

package smoke_test

// The sales-channel registry is a LOOKUP, NOT A CONSTRAINT — proven end to end
// (TKT-241, epic TKT-17, ADR-024).
//
// WHAT THIS FILE IS FOR. TKT-235 shipped catalog's `channels` registry with the
// rule that an unregistered — or disabled — channel code must sell exactly as it
// did before the registry existed. Two tests guarded that rule and both were
// narrower than it:
//
//   - services/inventory/internal/store/channel_allocations_smoke_test.go's
//     TestAnUnregisteredChannelCodeStillSells calls the inventory STORE directly.
//     A registry lookup added in commerce orchestration, in catalog's price, fee
//     or split resolution, or at any API boundary would refuse the sale EARLIER
//     and leave that test green. Its own scope note (:470-497) names TKT-241 as
//     the owner of the gateway-level version. This is it.
//   - services/catalog/internal/store/channels_smoke_test.go's
//     TestNothingReferencesTheChannelRegistry greps information_schema for foreign
//     keys pointing at `channels`. That proves FK absence in CATALOG'S SCHEMA and
//     says nothing about application-level gating anywhere.
//
// So the gap was never a defect in the code — nothing on the sale path reads the
// registry today — but a missing guard against a defect a FUTURE ticket could
// introduce. These tests buy that guard, at the only tier that can hold it: a real
// purchase through the gateway against the running stack, traversing catalog price
// resolution, fee resolution (ADR-046), split resolution (ADR-047), commerce
// orchestration, inventory and payments.
//
// TWO TABLES, NOT ONE — the distinction these tests exist to pin. Inventory's claim
// path DOES gate on a channel: four ordered predicates, `window -> seller -> code ->
// capacity` (services/inventory/internal/store/store.go:500-600, ADR-054 / TKT-246 /
// ADR-064 / ADR-024). Every one of them reads inventory's OWN `channel_allocations`
// table. NONE reads catalog's `channels` registry. Conflating the two is how a test
// like this ends up green and vacuous, so each fixture below is built to satisfy all
// four predicates — a fixture refused by an earlier one never reaches the INSERT it
// means to assert on, and proves nothing while looking like it proves everything.
//
// IF YOU ARE HERE BECAUSE ONE OF THESE FAILED: a deliberate decision to make the
// registry authoritative on a sale path must CHANGE these tests and say why in the
// commit — see ADR-024 for the reasoning it would be overturning (historical
// attribution must survive a channel being retired, which is why there is no FK).
// Do not delete them to make a build green.
//
// TKT-248 UPDATE — the public half was narrowed, and this is the "say why".
//
// Under TKT-248, public reservations may not name a channel; the public half is
// therefore narrowed to the refusal boundary, while the partner half remains the
// end-to-end proof that an unregistered credential channel sells verbatim.
// ADR-024's registry-as-lookup decision is unchanged.
//
// The distinction that makes this a narrowing rather than an overturning: there
// were always two questions behind one field, and only the second one moved.
//
//	Is the code REGISTERED?  -> do not gate on it. Retired channels keep selling,
//	                            historical attribution survives. ADR-024. UNCHANGED.
//	Is the caller ENTITLED?  -> no, unless authenticated. ADR-060. THIS is new.
//
// Nothing on the sale path reads catalog's `channels` table, before or after. The
// public half could not keep its old assertion because the capability it exercised
// — a public caller naming a channel — was itself the defect (catalog resolves fee
// AND price rules on the channel, so an `absorbed` rule undercharged the buyer and
// the organizer bore it). The claim it used to prove now lives entirely in the
// partner half below, which is untouched.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// tkt241Code mints a per-run channel code that is guaranteed absent from the
// registry, so "unregistered" is a property of the code rather than of a shared
// fixture that another test could create behind our back.
func tkt241Code(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tkt241-" + prefix + "-" + hex.EncodeToString(b)
}

// requireNotInRegistry asserts the code has NO row in catalog's channels table.
//
// t.Fatal, never t.Skip: a skip is how a suite silently stops proving its point.
// And it never DELETES a row to force the precondition — manufacturing the state
// you claim to observe is the "repairing the precondition during setup" failure
// (docs/learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md).
// If a row exists, the fixture's assumption is broken and the test must say so.
func requireNotInRegistry(t *testing.T, ctx context.Context, code string) {
	t.Helper()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()
	var n int
	if err := cat.QueryRow(ctx, `SELECT count(*) FROM channels WHERE organizer_id=$1 AND code=$2`,
		organizerID, code).Scan(&n); err != nil {
		t.Fatalf("count registry rows for %q: %v", code, err)
	}
	if n != 0 {
		t.Fatalf("channel %q HAS %d registry row(s); this test proves nothing about an "+
			"UNREGISTERED code while one exists. Something now creates it (the smoke bootstrap? "+
			"another test?) — point this test at a code that is genuinely absent rather than "+
			"deleting the row.", code, n)
	}
}

// eventOf resolves the event a ticket type hangs off, which is the scope the fee
// rule and split schedule below are written at. Derived rather than returned by a
// new publishedSlot variant, so the shared helper stays untouched (precedent:
// smoke/checkout_test.go:887-891).
func eventOf(t *testing.T, ctx context.Context, ticketType string) string {
	t.Helper()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()
	var eventID string
	if err := cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
		JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, ticketType).Scan(&eventID); err != nil {
		t.Fatalf("resolve event for ticket type %s: %v", ticketType, err)
	}
	return eventID
}

// seedChannelFeeAndSplit inserts a channel-scoped fee rule and a matching split
// schedule straight into catalog, because there is no HTTP API for either
// (precedent: smoke/checkout_test.go:895-905).
//
// WHY THE SEED IS LOAD-BEARING, not fixture ceremony. Commerce treats "no rule
// matched" as a SUCCESSFUL resolution with an empty fee set — deliberately, and
// says so at services/commerce/internal/api/server.go:805-808. So a sale with no
// fee rule completes identically whether fee resolution ran correctly, ran
// degenerately, or was skipped entirely. Without this seed these tests would
// traverse ADR-046 and ADR-047 and OBSERVE NOTHING about them, which is the exact
// vacuity TKT-241 exists to eliminate — reproduced inside its own fix.
//
// With it, the sale's fee outcome is a function of the channel code, so a registry
// gate added in fee resolution changes an asserted number rather than nothing.
// Returns the seeded payee id, which is what the split assertion recognises the
// winning schedule BY: asserting the fee alone cannot see split resolution at all
// (ai-review pass 1, [high]) — see assertSplitAwardedTo.
func seedChannelFeeAndSplit(t *testing.T, ctx context.Context, eventID, channel string) string {
	t.Helper()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()
	if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
		(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
		VALUES($1,'event',$2,'service','per_ticket_fixed',$3,'EUR','passed_on',$4)`,
		organizerID, eventID, tkt241FeePerTicket, channel); err != nil {
		t.Fatalf("seed fee rule for %q: %v", channel, err)
	}
	// The split rides INSIDE the fee-resolution response rather than in a call of
	// its own (ADR-047; services/catalog/internal/store/splits_postgres.go:47-66),
	// so seeding it is how the split resolver gets traversed at all.
	//
	// Header AND parts in ONE transaction, summing to 10000 bps. Not a style
	// choice: a schedule is unbalanced for the whole of its own creating
	// transaction, so 0017_payees_and_split_schedules.sql defers the balance check
	// to COMMIT and rejects a header with no parts outright ("split schedule % has
	// no parts"). A single-statement seed cannot satisfy it. There is no HTTP API
	// for payees or schedules, so SQL is the only authoring path here — the same
	// reason the fee rule above is seeded directly.
	tx, err := cat.Begin(ctx)
	if err != nil {
		t.Fatalf("begin split seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var payeeID string
	if err := tx.QueryRow(ctx, `INSERT INTO payees (organizer_id, kind, display_name)
		VALUES($1,'system',$2) RETURNING id`,
		organizerID, "TKT-241 payee "+channel).Scan(&payeeID); err != nil {
		t.Fatalf("seed payee for %q: %v", channel, err)
	}
	var scheduleID string
	if err := tx.QueryRow(ctx, `INSERT INTO split_schedules
		(organizer_id, scope_level, scope_id, fee_code, channel_code)
		VALUES($1,'event',$2,'service',$3) RETURNING id`,
		organizerID, eventID, channel).Scan(&scheduleID); err != nil {
		t.Fatalf("seed split schedule for %q: %v", channel, err)
	}
	// One payee taking the whole fee: the shares are not what this ticket is
	// about, and 10000 to a single payee is the simplest set the balance rule
	// accepts. What matters is that the schedule EXISTS and is channel-scoped, so
	// the split resolver has something to select on this channel.
	if _, err := tx.Exec(ctx, `INSERT INTO split_schedule_parts
		(schedule_id, payee_id, organizer_id, share_bps) VALUES($1,$2,$3,10000)`,
		scheduleID, payeeID, organizerID); err != nil {
		t.Fatalf("seed split part for %q: %v", channel, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit split seed for %q: %v", channel, err)
	}
	return payeeID
}

// assertSplitAwardedTo checks the persisted fee-resolution snapshot attributes the
// fee to the seeded payee, in full.
//
// WHY THIS EXISTS. The first version of these tests seeded a channel-scoped split
// schedule and then asserted only the fee's code and amount — which observe nothing
// about split resolution, because a fee with NO split is forwarded with no parts and
// payments records it as collected-and-unattributed rather than refusing
// (services/commerce/internal/api/catalog_fees.go:648-652). Deleting the split seed,
// or making the split resolver drop this channel, left all three tests green. The
// ADR-047 half of the claim was decorative. Found by ai-review pass 1 [high].
//
// The snapshot is the right place to look: splits ride inside the fee-resolution
// response rather than in a call of their own, and commerce persists the whole
// document on the reservation, then settles from THAT rather than re-reading catalog.
// So the snapshot is both what the resolver decided and what the money follows.
func assertSplitAwardedTo(t *testing.T, ctx context.Context, reservationID, payeeID, channel string) {
	t.Helper()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT fee_resolution_snapshot FROM reservations WHERE id=$1`,
		reservationID).Scan(&raw); err != nil {
		t.Fatalf("read fee_resolution_snapshot: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("reservation %s has NO fee resolution snapshot; the sale never resolved fees at "+
			"all, so nothing below could have been proven", reservationID)
	}
	var snap struct {
		Resolution struct {
			Fees []struct {
				FeeCode string `json:"fee_code"`
				Split   struct {
					Mode   string `json:"mode"`
					Winner *struct {
						Parts []struct {
							Payee struct {
								PayeeID string `json:"payee_id"`
							} `json:"payee"`
							ShareBps int32 `json:"share_bps"`
						} `json:"parts"`
					} `json:"winner"`
				} `json:"split"`
			} `json:"fees"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode fee snapshot: %v", err)
	}
	for _, f := range snap.Resolution.Fees {
		if f.FeeCode != "service" {
			continue
		}
		if f.Split.Winner == nil {
			t.Fatalf("fee 'service' resolved with NO winning split on channel %q, but a "+
				"channel-scoped schedule was seeded for it. Split resolution is channel-selective "+
				"(ADR-047) — if it now filters on the channel registry, that is the regression "+
				"this test exists to catch. snapshot: %s", channel, raw)
		}
		if len(f.Split.Winner.Parts) != 1 {
			t.Fatalf("winning split has %d parts, want 1: %s", len(f.Split.Winner.Parts), raw)
		}
		p := f.Split.Winner.Parts[0]
		// Both halves asserted, and from the SEED rather than from a run: the payee
		// identifies WHICH schedule won (a different one would name a different
		// payee), and the share is the value the seed wrote.
		if p.Payee.PayeeID != payeeID {
			t.Fatalf("split awarded to payee %s, want the seeded %s — a different schedule won",
				p.Payee.PayeeID, payeeID)
		}
		if p.ShareBps != 10000 {
			t.Fatalf("share_bps = %d, want 10000 as seeded", p.ShareBps)
		}
		return
	}
	t.Fatalf("no 'service' fee in the resolution snapshot: %s", raw)
}

// tkt241FeePerTicket is the seeded per-ticket fee, in minor units.
//
// Every fee assertion below is derived from THIS CONSTANT and the quantity — never
// from what a run happened to produce. An expectation written by observing the code
// pins the behaviour instead of the rule, and every mutant then dies for the wrong
// reason (AGENTS.md; TKT-239).
const tkt241FeePerTicket = 300

// assertPassedOnFee checks the reservation charged exactly the seeded fee.
//
// The invariant in one sentence, without naming the implementation: a sale on a
// channel with a fee rule is charged that rule's fee, whether or not the channel is
// in the registry.
func assertPassedOnFee(t *testing.T, body []byte, quantity int64) {
	t.Helper()
	var res struct {
		Amount       int64 `json:"amount"`
		FaceValue    int64 `json:"face_value"`
		PassedOnFees int64 `json:"passed_on_fees"`
		FeeBreakdown []struct {
			FeeCode string `json:"fee_code"`
			Amount  int64  `json:"amount"`
		} `json:"fee_breakdown"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	want := tkt241FeePerTicket * quantity
	if res.PassedOnFees != want {
		t.Fatalf("passed_on_fees = %d, want %d (%d/ticket x %d) — the channel-scoped fee rule did "+
			"NOT apply, so this sale never really traversed fee resolution and the test proves "+
			"nothing about ADR-046", res.PassedOnFees, want, tkt241FeePerTicket, quantity)
	}
	if res.Amount-res.FaceValue != want {
		t.Fatalf("amount(%d) - face_value(%d) = %d, want %d", res.Amount, res.FaceValue,
			res.Amount-res.FaceValue, want)
	}
	var found bool
	for _, f := range res.FeeBreakdown {
		if f.FeeCode == "service" {
			found = true
			if f.Amount != want {
				t.Fatalf("fee_breakdown[service].amount = %d, want %d", f.Amount, want)
			}
		}
	}
	if !found {
		t.Fatalf("fee_breakdown has no 'service' entry: %s", body)
	}
}

// commerceAttribution reads what the sale actually persisted, which is the only
// place "recorded verbatim" can be observed: GET /api/commerce/orders/{id} returns
// {order_id, status} and nothing else (services/commerce/internal/api/server.go:1818),
// so the API cannot prove attribution even in principle. The assertion also belongs
// at the tier the mechanism lives at (AGENTS.md).
func commerceAttribution(t *testing.T, ctx context.Context, reservationID string) (resChannel, orderChannel *string) {
	t.Helper()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	if err := db.QueryRow(ctx, `SELECT channel_code FROM reservations WHERE id=$1`,
		reservationID).Scan(&resChannel); err != nil {
		t.Fatalf("read reservations.channel_code: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT channel_code FROM orders WHERE reservation_id=$1`,
		reservationID).Scan(&orderChannel); err != nil {
		t.Fatalf("read orders.channel_code: %v", err)
	}
	return resChannel, orderChannel
}

// claimAttribution reads the inventory claim's channel for a reservation's hold.
func claimAttribution(t *testing.T, ctx context.Context, holdID string) *string {
	t.Helper()
	inv, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatalf("connect inventory: %v", err)
	}
	defer func() { _ = inv.Close(ctx) }()
	var channel *string
	if err := inv.QueryRow(ctx, `SELECT channel_code FROM claims WHERE id=$1`, holdID).Scan(&channel); err != nil {
		t.Fatalf("read claims.channel_code for hold %s: %v", holdID, err)
	}
	return channel
}

func mustString(t *testing.T, p *string, what string) string {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is NULL, want a channel code", what)
	}
	return *p
}

// checkoutReservation confirms a reservation into a completed order.
func checkoutReservation(t *testing.T, reservationID, key string) {
	t.Helper()
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", key,
		map[string]any{"reservation_id": reservationID, "name": "TKT-241 Buyer",
			"email": "tkt241@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout: %d %s", code, body)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Fatalf("order status %q, want completed: %s", out.Status, body)
	}
}

// TestAPublicSaleMayNotNameAChannelAtAll is the public half, NARROWED by TKT-248.
//
// It used to assert that a public sale on an UNREGISTERED channel priced, reserved,
// charged and ordered exactly as a registered one would -- TKT-241's
// registry-is-a-lookup claim, proven end to end through the gateway.
//
// That scenario no longer exists. ADR-060 made naming a channel an ENTITLEMENT
// question: `channel_code` is gone from ReservationCreate, so a public caller
// cannot name any channel, registered or not. The old assertion could only be
// preserved by keeping the capability that was the defect.
//
// WHAT THIS DOES NOT MEAN. ADR-024 is NOT overturned and the registry is still a
// lookup, not a sale constraint. Nothing on the sale path reads catalog's
// `channels` table, and an unregistered or retired code still sells verbatim --
// that claim moved intact to the PARTNER half below, which is unchanged and is now
// the sole end-to-end proof of it. The difference between the two halves is
// authorization, not registration.
//
// So this half now pins the boundary that replaced it: the public route refuses the
// field. If it ever goes green with a channel accepted, the entitlement is gone and
// the fee/price leak is back.
func TestAPublicSaleMayNotNameAChannelAtAll(t *testing.T) {
	ctx := t.Context()
	channel := tkt241Code(t, "public")
	requireNotInRegistry(t, ctx, channel)

	_, ticketType := publishedSlot(t, "TKT-248 Public Hall", 20)
	// NO fee/split seed, and that is deliberate rather than an omission: the
	// request is refused before catalog is consulted at all, so a seeded rule could
	// not affect the outcome and would be a decorative fixture implying this test
	// observes a layer it never reaches (TKT-241's own lesson). The economics are
	// asserted where they are decided, in
	// services/commerce/internal/api/catalog_fees_test.go.
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt248-public-"+channel,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType,
			"quantity": 2, "channel_code": channel})
	if code != http.StatusBadRequest {
		t.Fatalf("a public reserve naming channel %q answered %d, want 400: %s\n"+
			"The public route is unauthenticated, and catalog resolves both fee (ADR-046) and "+
			"price (TKT-237) rules on the channel -- so a body-supplied channel lets a caller "+
			"choose their own price basis, and an `absorbed` fee makes that a SMALLER charge with "+
			"the organizer bearing the difference. A partner's channel comes from its credential "+
			"(ADR-056) and a public sale has none. TKT-248/ADR-060.", channel, code, body)
	}
}

// TestAPartnerSaleOnAnUnregisteredChannelCompletesEndToEnd is the partner half.
//
// It is a separate test, not a table row, because it proves the part the public
// path structurally CANNOT: that an unregistered code is recorded verbatim on the
// inventory CLAIM. The channel here comes from the credential rather than a request
// body, which is the only way a channel is allowed to reach inventory (TKT-246), so
// this is where "the claim records it verbatim" can be asserted at all.
//
// The fixture must clear all four claim-path predicates to reach the INSERT:
// window (no opens_at/closes_at -> always open), seller (sold_by = this reseller),
// code (requires_code defaults false), capacity (cap 4 and pool 20 both exceed 2).
func TestAPartnerSaleOnAnUnregisteredChannelCompletesEndToEnd(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set: the partner half would silently prove nothing")
	}
	if partnerReseller() == "" {
		t.Fatal("SMOKE_PARTNER_RESELLER_ID is not set: the allocation could not be bound")
	}
	ctx := t.Context()
	channel := partnerChannel()
	if channel == "" {
		t.Fatal("SMOKE_PARTNER_CHANNEL is not set")
	}
	// The partner channel is fixed by the credential, so unlike the public halves
	// its unregistered-ness is a property of the STACK rather than of a code we
	// minted. That makes it exactly the thing that can drift silently, so it is
	// checked rather than assumed.
	requireNotInRegistry(t, ctx, channel)

	slot, ticketType := publishedSlot(t, "TKT-241 Partner Hall", 20)
	payeeID := seedChannelFeeAndSplit(t, ctx, eventOf(t, ctx, ticketType), channel)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": channel, "cap": 4, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations",
		"tkt241-partner-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("partner reserve on unregistered channel %q: %d %s — the credential's channel "+
			"has no registry row, and must sell anyway (ADR-024)", channel, code, body)
	}
	assertPassedOnFee(t, body, 2)

	var reservation struct {
		ID     string `json:"reservation_id"`
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	// The split half of the traversal, observed rather than assumed (ai-review
	// pass 1, [high]): the fee assertion above cannot see split resolution at all.
	assertSplitAwardedTo(t, ctx, reservation.ID, payeeID, channel)
	checkoutReservation(t, reservation.ID, "tkt241-partner-order-"+slot)

	// The claim: the half the public path cannot reach.
	if got := mustString(t, claimAttribution(t, ctx, reservation.HoldID), "claims.channel_code"); got != channel {
		t.Fatalf("claims.channel_code = %q, want %q verbatim", got, channel)
	}
	resChannel, orderChannel := commerceAttribution(t, ctx, reservation.ID)
	if got := mustString(t, resChannel, "reservations.channel_code"); got != channel {
		t.Fatalf("reservations.channel_code = %q, want %q verbatim", got, channel)
	}
	if got := mustString(t, orderChannel, "orders.channel_code"); got != channel {
		t.Fatalf("orders.channel_code = %q, want %q verbatim", got, channel)
	}
}

// TestASaleOnADISABLEDRegisteredChannelCompletesEndToEnd is the half that is easiest
// to get wrong by intuition.
//
// Disabling a channel is a DISPLAY decision, not a sellability one: it removes the
// channel from GET /public/channels — the storefront's selector — and nothing else.
// Nothing on the sale path consults `enabled`, and this test is what stops that
// changing silently. The temptation it guards against is real and reads as a bug
// fix: "the channel is disabled, why did a sale go through?"
//
// Deliberately NOT asserted here: that the code is absent from /public/channels.
// That is the storefront selector's behaviour, catalog's own tests cover it, and
// mixing it in would blur the very line this test draws — invisible in the picker
// and unsellable are different claims, and only the first one is true.
//
// TKT-248 moved it to the PARTNER path, and only the path changed. It used to sell
// on a freshly-minted disabled code through the public route; public reservations
// can no longer name any channel (ADR-060), so the vehicle is gone while the claim
// is untouched. It now disables the PARTNER's own registry row, which is a sharper
// version of the same test: the channel a credential actually sells on is switched
// off, and the sale must still complete.
func TestASaleOnADISABLEDRegisteredChannelCompletesEndToEnd(t *testing.T) {
	ctx := t.Context()
	channel := partnerChannel()
	if channel == "" {
		t.Fatal("SMOKE_PARTNER_CHANNEL is not set: the credential's channel is the only channel a " +
			"sale can name, so this test cannot be constructed without it")
	}
	requireNotInRegistry(t, ctx, channel)

	// A registry row that EXISTS and is switched off — the case the unregistered
	// tests above cannot make, since they turn on the row's absence.
	created := created(t, gatewayURL+"/api/catalog/channels", map[string]any{
		"code": channel, "display_name": "TKT-248 disabled", "kind": "reseller", "enabled": false,
	})
	if enabled, ok := created["enabled"].(bool); !ok || enabled {
		t.Fatalf("channel %q was created enabled=%v, want false — the fixture cannot show what it "+
			"claims to show if the row is not actually disabled", channel, created["enabled"])
	}

	slot, ticketType := publishedSlot(t, "TKT-248 Disabled Hall", 20)
	payeeID := seedChannelFeeAndSplit(t, ctx, eventOf(t, ctx, ticketType), channel)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": channel, "cap": 4, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations",
		"tkt248-disabled-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("reserve on DISABLED channel %q: %d %s — disabling a channel removes it from the "+
			"storefront selector and must not make it unsellable. If this refusal is deliberate "+
			"it overturns ADR-024 and TKT-235's lookup-not-constraint rule: change this test and "+
			"say why, do not delete it.", channel, code, body)
	}
	assertPassedOnFee(t, body, 2)

	var reservation struct {
		ID     string `json:"reservation_id"`
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	// The split half of the traversal, observed rather than assumed (ai-review
	// pass 1, [high]): the fee assertion above cannot see split resolution at all.
	assertSplitAwardedTo(t, ctx, reservation.ID, payeeID, channel)
	checkoutReservation(t, reservation.ID, "tkt248-disabled-order-"+slot)

	_, orderChannel := commerceAttribution(t, ctx, reservation.ID)
	if got := mustString(t, orderChannel, "orders.channel_code"); got != channel {
		t.Fatalf("orders.channel_code = %q, want %q verbatim", got, channel)
	}
}
