// Package cachetier is the single source of ADR-004's volatility tiers.
//
// ADR-004 rule 1 says every public read endpoint declares a TTL tiered by data
// volatility. Before TKT-204 those tiers were string literals spread across two
// services, each correct in isolation, none linked to the tier it claimed to be.
// That is fine while the header is the only artifact; it stops being fine the
// moment something caches on it, because the cache's lifetime and the header's
// advertised lifetime are then two independently-editable copies of one number.
//
// So a tier IS its duration, and its Cache-Control string is rendered from that
// duration rather than stored beside it. The two cannot disagree.
//
// This package fixes what the tiers ARE. It does not decide which endpoint gets
// which tier — that stays with the endpoint, and with ADR-004's indicative table.
package cachetier

import (
	"fmt"
	"time"
)

// Tier is an ADR-004 volatility tier, identified by its lifetime.
//
// Only the values declared below are tiers. A Tier cast from some other duration
// is not registered and cannot render a header — see CacheControl.
type Tier time.Duration

// The four ADR-004 tiers. Adding a fifth is an ADR-004 change, not a code change:
// TestRegisteredTiers pins the count, and the spec audit validates every declared
// Cache-Control value against exactly this set.
const (
	// Never is the no-store tier: hold, order and scan state, authentication
	// responses, and resolved prices (a correctness tier, not a performance one).
	// It is a real registered tier with a zero lifetime rather than the absence of
	// one, which is what lets the spec audit tell "declared never" apart from
	// "declared nothing".
	Never Tier = 0
	// Seconds is remaining-capacity/availability — the tier where staleness is most
	// load-bearing during an on-sale.
	Seconds Tier = Tier(5 * time.Second)
	// Minutes is event lists and event/season/festival detail.
	Minutes Tier = Tier(5 * time.Minute)
	// Hours is venue geometry, and published seat-map geometry (TKT-107: only when
	// the whole payload is published — a draft-bearing response takes Never).
	Hours Tier = Tier(time.Hour)
)

// All returns the registered tiers. The spec audit validates declared
// Cache-Control values against this set, so growing it widens what the contract
// may declare.
func All() []Tier { return []Tier{Never, Seconds, Minutes, Hours} }

// Duration is the tier's lifetime.
func (t Tier) Duration() time.Duration { return time.Duration(t) }

// CacheControl renders the tier's Cache-Control header from its duration.
//
// It panics on an unregistered Tier. That is deliberate: every call site is a
// package-level var initializer, so an unregistered tier is a process-start
// failure — loud, and before any traffic. A sentinel return would let a caller
// ignore it and emit an empty Cache-Control, which ADR-028's response validator
// would then turn into a 500 on a public read. If a future caller ever computes a
// tier at request time, this contract has to be revisited.
func (t Tier) CacheControl() string {
	for _, r := range All() {
		if r == t {
			if t == Never {
				return "no-store"
			}
			s := int(t.Duration() / time.Second)
			return fmt.Sprintf("public, max-age=%d, s-maxage=%d", s, s)
		}
	}
	panic(fmt.Sprintf("cachetier: %v is not a registered ADR-004 tier", t.Duration()))
}

// FromCacheControl resolves a declared Cache-Control value back to its tier.
// Reports false for anything that is not exactly one registered tier's rendering
// — including a near miss like "public, max-age=300" with no s-maxage.
func FromCacheControl(v string) (Tier, bool) {
	for _, t := range All() {
		if t.CacheControl() == v {
			return t, true
		}
	}
	return Never, false
}
