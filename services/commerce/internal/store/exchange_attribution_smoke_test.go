//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Exchange sales attribution (TKT-246).
//
// An exchange is the same purchase in a different seat, so WHO SOLD IT does not change.
// Before this ticket the replacement carried neither channel_code nor reseller_id, so
// exchanging a reseller-attributed order produced a public, unattributed one --
// irreversibly, because nothing else records who sold a ticket. Settlement (TKT-23)
// would then pay the wrong party, or nobody.
//
// This test is at the STORE tier because that is where the mechanism is: the projection
// that reads the source's attribution. A test one tier up would prove the handler and a
// fake agree (AGENTS.md).

// seedAttributedCompleted is seedCompleted with sales attribution on the reservation.
func seedAttributedCompleted(t *testing.T, db *sql.DB, ctx context.Context, key string,
	channel *string, reseller *uuid.UUID) (Completion, uuid.UUID) {
	t.Helper()
	c := Completion{
		ReservationID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(),
		BuyerID: uuid.New(), SlotID: uuid.New(), TicketTypeID: uuid.New(), Quantity: 2,
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status,channel_code,reseller_id)
		VALUES($1,$2,$3,$4,$5,$6,2,2500,5000,5000,'EUR','completed',$7,$8)`,
		c.ReservationID, c.OrganizerID, uuid.New(), c.SlotID, c.TicketTypeID, c.BuyerID,
		channel, reseller); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref,channel_code,reseller_id)
		VALUES($1,$2,'completed',$3,'fingerprint',$4,$5,$6)`,
		c.OrderID, c.ReservationID, key, uuid.New(), channel, reseller); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, c.ReservationID)
	})
	return c, c.OrderID
}

// LoadExchangeSource projects the source's channel AND reseller.
//
// The projection is the whole mechanism: holdExchangeTarget uses it to consume the right
// allocation, and persistExchangeReplacement uses it to inherit attribution. A source
// that reads NULL for either is how both defects happened -- the field was simply not in
// the SELECT list, so every layer above it was correct about a value that never arrived.
func TestExchangeSourceProjectsItsSalesAttribution(t *testing.T) {
	db, ctx := outboxDB(t)
	channel := "reseller-acme"
	reseller := uuid.New()
	c, order := seedAttributedCompleted(t, db, ctx, "attr-key", &channel, &reseller)

	src, err := LoadExchangeSource(ctx, db, c.OrganizerID, order)
	if err != nil {
		t.Fatal(err)
	}
	if src.ChannelCode == nil || *src.ChannelCode != channel {
		t.Fatalf("source channel = %v, want %q — without it the exchange target consumes PUBLIC "+
			"stock while repricing on the source's channel, so the money and the inventory "+
			"describe different sales", src.ChannelCode, channel)
	}
	if src.ResellerID == nil || *src.ResellerID != reseller {
		t.Fatalf("source reseller = %v, want %s — without it the replacement is unattributed and "+
			"the fact of who sold the ticket is gone for good", src.ResellerID, reseller)
	}
}

// A PUBLIC source projects no attribution, and that must stay distinguishable from a
// reseller sale rather than becoming a zero uuid.
//
// uuid.Nil is a legal-looking value that compares equal to nothing, and inventory treats
// it as "no identity proven". If the projection turned NULL into uuid.Nil the difference
// between "sold by nobody in particular" and "sold by reseller 000...0" would vanish at
// the type level, and the exchange target would present an identity it does not have.
func TestAPublicExchangeSourceProjectsNoAttribution(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedAttributedCompleted(t, db, ctx, "attr-public", nil, nil)

	src, err := LoadExchangeSource(ctx, db, c.OrganizerID, order)
	if err != nil {
		t.Fatal(err)
	}
	if src.ChannelCode != nil {
		t.Fatalf("a public source projected channel %v, want nil", *src.ChannelCode)
	}
	if src.ResellerID != nil {
		t.Fatalf("a public source projected reseller %v, want nil — NULL must stay NULL rather "+
			"than becoming a zero uuid that looks like an identity", *src.ResellerID)
	}
}

// THE STATE THE FIRST VERSION OF THIS FILE COULD NOT REACH (ai-review [high] F2).
//
// A public reserve is UNAUTHENTICATED and still persists whatever channel_code its body
// named — the field feeds fee resolution and reporting, and only the inventory forward
// was withheld. So `channel_code != NULL AND reseller_id IS NULL` is a perfectly legal,
// routinely-produced row, and it is the dangerous one: an exchange of that order would
// have presented the channel to inventory with no reseller identity and consumed an
// allocation nobody authorized. Every allocation is unbound today, so it was reachable
// immediately.
//
// The original public-source test defined "public" as BOTH fields NULL, so its fixture
// could not construct this state at all — a test that names the right case and is
// structurally incapable of failing (AGENTS.md). This is the missing half.
func TestAPublicSourceWithAChannelStillProjectsNoSeller(t *testing.T) {
	db, ctx := outboxDB(t)
	channel := "reseller-acme" // a channel the BUYER typed; no credential was involved
	c, order := seedAttributedCompleted(t, db, ctx, "attr-channel-no-reseller", &channel, nil)

	src, err := LoadExchangeSource(ctx, db, c.OrganizerID, order)
	if err != nil {
		t.Fatal(err)
	}
	// The channel is projected — it is real, and repricing needs it.
	if src.ChannelCode == nil || *src.ChannelCode != channel {
		t.Fatalf("source channel = %v, want %q", src.ChannelCode, channel)
	}
	// But there is NO seller, and that is what the exchange must key on. The forward
	// decision lives in holdExchangeTarget and is asserted there; what this pins is
	// that the projection keeps the two facts INDEPENDENT, so a caller cannot infer
	// authorization from the presence of a channel.
	if src.ResellerID != nil {
		t.Fatalf("an unauthenticated public sale projected reseller %v — a channel in a request "+
			"body is not evidence of a seller, and treating it as one is the bypass TKT-240 was "+
			"reverted for, arriving one exchange later", *src.ResellerID)
	}
}
