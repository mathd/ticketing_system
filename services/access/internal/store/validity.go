package store

// ValidityStatus is the status of a ticket's presentation verdict (TKT-355).
type ValidityStatus string

const (
	ValidityValid  ValidityStatus = "valid"
	ValidityVoided ValidityStatus = "voided"
)

// VoidReason says why a voided ticket is voided. It is set only when the status is
// ValidityVoided.
type VoidReason string

const (
	VoidRefunded  VoidReason = "refunded"
	VoidExchanged VoidReason = "exchanged"
)

// TicketValidity is the presentation verdict the guest bundle carries for one ticket.
//
// It controls what a ticket view shows, and nothing else. The gate decides admission
// from Access state on its own path (scan.go); this verdict is not a revocation, and the
// QR endpoint still serves a voided ticket's image to a holder of a live link. It is
// derived each time the bundle is served and never stored, so there is no second
// mutable flag that can disagree with the trail. It is also no integrity claim: it reads
// the history rows as they are, and ADR-021 says what that history can and cannot prove.
type TicketValidity struct {
	Status ValidityStatus `json:"status"`
	Reason VoidReason     `json:"reason,omitempty"`
}

// lifecycleEffect is what one lifecycle event type does to a ticket's verdict.
type lifecycleEffect int

const (
	keepsVerdict lifecycleEffect = iota + 1
	voidsByRefund
	voidsByExchange
)

// lifecycleEffects classifies EVERY event type the lifecycle_events CHECK admits.
// TestEveryLifecycleTypeHasAValidityEffect reads that CHECK from a migrated database. It
// fails when the CHECK is not a plain IN list of text literals, and when this table and
// the CHECK differ in either direction, so a migration that adds a type cannot ship until
// the type is classified here.
//
// The voiding entries match the gate. Both scan paths refuse a refunded ticket first and
// an exchanged ticket second (scan.go), and no other type voids a ticket there.
var lifecycleEffects = map[string]lifecycleEffect{
	"issued":          keepsVerdict,
	"delivered":       keepsVerdict,
	"redelivered":     keepsVerdict,
	"redeemed":        keepsVerdict,
	"entry":           keepsVerdict,
	"exit":            keepsVerdict,
	"duplicate_admit": keepsVerdict,
	"refunded":        voidsByRefund,
	"exchanged":       voidsByExchange,
}

// DeriveTicketValidity returns the presentation verdict for one ticket's history.
//
// A refund anywhere in the history voids the ticket with reason refunded. Otherwise an
// exchange anywhere voids it with reason exchanged. Otherwise it is valid. The order of
// the events does not matter, and a later event never restores validity: a delivery, a
// resend or an admission recorded after a void is a fact about a dead ticket, not a
// reinstatement (all three writers accept a voided ticket).
//
// Refund before exchange is the scan path's order. Commerce today refuses to refund an
// exchanged order and to exchange a refunded one, so a history with both is reachable
// only through Access's own writers; the precedence still decides it, so that the page
// and the gate agree if it ever happens.
//
// A type that is not in lifecycleEffects keeps the verdict, as the gate does. The
// forcing test makes that case unreachable through a migration.
func DeriveTicketValidity(history []LifecycleEvent) TicketValidity {
	var refunded, exchanged bool
	for _, event := range history {
		switch lifecycleEffects[event.Type] {
		case voidsByRefund:
			refunded = true
		case voidsByExchange:
			exchanged = true
		}
	}
	switch {
	case refunded:
		return TicketValidity{Status: ValidityVoided, Reason: VoidRefunded}
	case exchanged:
		return TicketValidity{Status: ValidityVoided, Reason: VoidExchanged}
	default:
		return TicketValidity{Status: ValidityValid}
	}
}
