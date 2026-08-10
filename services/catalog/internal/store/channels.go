package store

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The sales-channel registry (TKT-235 / epic TKT-17).
//
// A channel finally has a definition. Before this, `channel_code` was an exact
// opaque string stored independently by inventory's claims and
// channel_allocations and catalog's fee_rules and split_schedules, and nothing
// said what a channel was called or whether it was still offered.
//
// The registry is a LOOKUP, NOT A CONSTRAINT. Nothing in this file, and nothing
// downstream of it, may gate a sale on a channel being registered: no FK points
// here from any of those four columns (ADR-024 refuses one from claims so
// historical attribution survives a channel being retired, and the same
// argument covers the rest). An unregistered code sells exactly as it did
// before this table existed.
//
// Codes are exact and case-sensitive (ADR-024, ADR-046 §4). Nothing here trims,
// lowercases or otherwise normalizes them, and a future caller must not either:
// a normalized registry would disagree with four unnormalized columns.

var (
	// ErrChannelCodeTaken: this organizer already has a channel with that code.
	// Per organizer, not global — two organizers may both define 'pos'.
	ErrChannelCodeTaken = errors.New("organizer already has a channel with that code")
	// ErrChannelCodeImmutable: an update submitted a code differing from the
	// stored one. Renaming would orphan the code already recorded on live
	// claims, fee rules and split schedules — none of which reference this
	// table, so nothing would cascade and nothing would complain. A rename is a
	// new channel plus disabling the old one.
	ErrChannelCodeImmutable = errors.New("channel code is immutable")
	// ErrChannelInvalidInput: the input fails a bound the schema also enforces.
	// Checked here so a bad write is refused before it reaches Postgres and
	// comes back as an opaque constraint violation.
	ErrChannelInvalidInput = errors.New("invalid channel input")
)

// ChannelKind is what kind of sales channel this is. Closed: the four values
// are the ones the PRD names, and a fifth changes what the platform DOES (a
// reseller channel means a partner credential; a presale channel means unlock
// codes), so it lands with code — the OpenAPI enum, the generated types and the
// SQL CHECK together — rather than as a row someone can INSERT.
type ChannelKind string

const (
	ChannelKindWeb      ChannelKind = "web"
	ChannelKindPOS      ChannelKind = "pos"
	ChannelKindPresale  ChannelKind = "presale"
	ChannelKindReseller ChannelKind = "reseller"
)

// ValidChannelKind reports whether k is one of the four. Used by the write gate
// so an unknown kind is ErrChannelInvalidInput rather than a CHECK violation.
func ValidChannelKind(k ChannelKind) bool {
	switch k {
	case ChannelKindWeb, ChannelKindPOS, ChannelKindPresale, ChannelKindReseller:
		return true
	}
	return false
}

// Bounds are in CHARACTERS, matching PostgreSQL's length(text) and OpenAPI's
// maxLength — see validateChannelWrite for why the distinction is load-bearing.
//
// They mirror the SQL CHECKs in 0018_channels.sql, which in turn mirror
// claims.channel_code / channel_allocations.channel_code (inventory 0004) and
// fee_rules.channel_code (0016) / split_schedules.channel_code (0017). All of
// them must agree or a code legal in one place is unusable in another.
const (
	maxChannelCodeLen        = 100
	maxChannelDisplayNameLen = 200
)

// Channel is a registered sales channel.
type Channel struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Code        string
	DisplayName string
	Kind        ChannelKind
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PublicChannel is what a storefront selector needs: a label to show and the
// opaque code to submit with a later sale. No id, kind or enabled flag — only
// enabled channels are ever returned here, so the flag would be a constant, and
// the rest is operator configuration.
type PublicChannel struct {
	Code        string
	DisplayName string
}

// ChannelInput creates a channel.
type ChannelInput struct {
	OrganizerID uuid.UUID
	Code        string
	DisplayName string
	Kind        ChannelKind
	Enabled     bool
}

// ChannelUpdate replaces a channel's mutable fields.
//
// Code is present and required even though it cannot change: the caller states
// which code it believes it is updating, and a mismatch is refused with
// ErrChannelCodeImmutable. That turns a misidentified channel into an error
// instead of a silent write to the wrong row.
type ChannelUpdate struct {
	Code        string
	DisplayName string
	Kind        ChannelKind
	Enabled     bool
}

// validateChannelWrite gates both create and update, and RETURNS THE CODE THE
// STORE WILL PERSIST. Pure, so the bounds are unit-testable without Postgres.
//
// Returning the code rather than only an error is deliberate and is the reason
// this function has the shape it does. The property that matters most here is a
// NEGATIVE one — that nothing trims, lowercases or otherwise normalizes the
// code (ADR-024, ADR-046 §4; four columns in three services store codes
// verbatim, and a registry that folded them would disagree with all four). A
// gate that returns only `error` cannot express that property: a normalizing
// implementation returns nil for exactly the inputs a non-normalizing one does,
// so every test over it passes either way. Handing back the accepted code makes
// the absence of normalization observable, and a mutation that adds
// `strings.ToLower(strings.TrimSpace(code))` fails the test that pins it.
//
// Callers must persist the returned code, not their input. Today they are
// identical by construction; the point is that a future change making them
// differ is caught rather than silently shipped.
func validateChannelWrite(code, displayName string, kind ChannelKind) (string, error) {
	// Storability first, then length. The order matters — see storableText.
	if err := storableText(code, maxChannelCodeLen); err != nil {
		return "", err
	}
	if err := storableText(displayName, maxChannelDisplayNameLen); err != nil {
		return "", err
	}
	if !ValidChannelKind(kind) {
		return "", ErrChannelInvalidInput
	}
	return code, nil
}

// storableText refuses anything PostgreSQL's `text` cannot hold, then bounds the
// length in CHARACTERS.
//
// Both halves are the residue of a bug and its fix, and both are easy to get
// wrong in the same place:
//
//   - LENGTH IS IN CHARACTERS, NOT BYTES. `len()` counts UTF-8 bytes while
//     PostgreSQL's `length(text)` counts characters, so a 60-character code of
//     two-byte characters is 120 bytes: accepted by the request validator and by
//     every SQL channel_code CHECK (fee_rules', split_schedules', inventory's),
//     and rejected only here. That breaks the invariant the bound exists to hold
//     — a code legal in one of the five places that store one must be legal in
//     all of them. (ai-review pass 1; invisible to an all-ASCII fixture, where
//     the two counts agree.)
//
//   - BUT RUNE-COUNTING ALONE ADMITS WHAT POSTGRES REFUSES, which is the defect
//     the first fix introduced. `RuneCountInString` counts each malformed byte
//     as one RuneError and counts NUL as an ordinary rune, so `"é"×99 + "\x00"`
//     is 100 runes (passes) and 199 bytes (the old byte gate rejected it) — and
//     PostgreSQL answers `ERROR: null character not permitted`, unmapped, which
//     surfaces as a 500 where the caller should have got a 400. A JSON
//     "\u0000" is
//     reachable through ordinary JSON. Invalid UTF-8 fails the same way against
//     a UTF8-encoded database. (ai-review pass 2 — a defect in the fix, found
//     only because the fix diff got its own review.)
//
// Checking validity BEFORE counting is deliberate: counting runes in invalid
// UTF-8 yields a number that means nothing, so a length verdict derived from it
// would be arbitrary either way.
//
// All three layers agree at 100 CODE POINTS, including astral-plane ones, and
// the reason is worth writing down because the library's own comment says
// otherwise.
//
// kin-openapi's maxLength check announces "JSON schema string lengths are
// UTF-16!" and adds 2 per surrogate — but it obtains `r` by ranging over a Go
// string, which yields whole code points. `utf16.IsSurrogate` is therefore never
// true for valid UTF-8, and the branch is dead: it counts code points. Measured
// against v0.142.0, not inferred — 100 astral characters count as 100, not 200.
//
// So an astral code passes the validator, passes this gate, and PostgreSQL's
// `length()` also calls it 100. Three layers, one answer. Do not "fix" one of
// them to match the library's comment rather than its behaviour: that would
// reintroduce exactly the cross-layer disagreement these bounds exist to
// prevent. If kin-openapi is ever corrected totrue UTF-16 units, this gate becomes
// the more permissive one and the validator starts rejecting first — safe in
// direction, but the test below is what would notice.
func storableText(s string, maxChars int) error {
	if !utf8.ValidString(s) {
		return ErrChannelInvalidInput
	}
	if strings.ContainsRune(s, 0) {
		return ErrChannelInvalidInput
	}
	if l := utf8.RuneCountInString(s); l < 1 || l > maxChars {
		return ErrChannelInvalidInput
	}
	return nil
}

// ValidateChannelWriteForTest exposes the write gate to the api package's
// in-memory fake store, so the fake refuses exactly what Postgres refuses. A
// fake that accepted an over-long code or an unknown kind would let the handler
// tests agree with a store the real one would reject.
func ValidateChannelWriteForTest(code, displayName string, kind ChannelKind) (string, error) {
	return validateChannelWrite(code, displayName, kind)
}
