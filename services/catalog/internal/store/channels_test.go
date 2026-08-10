package store

import (
	"strings"
	"testing"
)

// TKT-235. The registry's pure write gate, and the property the whole epic
// rests on: codes are EXACT.

func TestChannelCodesAreExactAndNeverNormalized(t *testing.T) {
	// ADR-024 fixes channel codes as exact opaque strings — no normalization,
	// no case folding — because four other columns in three services store them
	// verbatim. A registry that folded them would disagree with all four.
	//
	// This asserts on the code the gate HANDS BACK, not merely that it accepts
	// the input. That distinction is the whole test: an implementation that
	// lowercases and trims accepts exactly the same inputs as one that does not,
	// so a gate returning only `error` makes this property untestable and the
	// test passes against a normalizing mutant. Asserting byte-identity is what
	// kills it.
	//
	// These are five DIFFERENT channels and must survive as five.
	distinct := []string{"pos", "POS", "Pos", " pos", "pos "}
	accepted := map[string]bool{}
	for _, code := range distinct {
		got, err := validateChannelWrite(code, "Box office", ChannelKindPOS)
		if err != nil {
			t.Fatalf("validateChannelWrite(%q) = %v, want nil — every one of these is a legal, distinct code", code, err)
		}
		if got != code {
			t.Fatalf("validateChannelWrite(%q) returned %q — the code was normalized; ADR-024 requires it verbatim", code, got)
		}
		accepted[got] = true
	}
	if len(accepted) != len(distinct) {
		t.Fatalf("normalization collapsed the fixture: %d distinct codes became %d (%v)", len(distinct), len(accepted), accepted)
	}
}

func TestChannelWriteGateBounds(t *testing.T) {
	tests := []struct {
		name        string
		code        string
		displayName string
		kind        ChannelKind
		wantErr     bool
	}{
		{name: "minimal legal", code: "w", displayName: "W", kind: ChannelKindWeb},
		{name: "code at 100", code: strings.Repeat("c", 100), displayName: "Long", kind: ChannelKindWeb},
		{name: "code at 101", code: strings.Repeat("c", 101), displayName: "Long", kind: ChannelKindWeb, wantErr: true},
		{name: "empty code", code: "", displayName: "Web", kind: ChannelKindWeb, wantErr: true},
		{name: "display name at 200", code: "web", displayName: strings.Repeat("d", 200), kind: ChannelKindWeb},
		{name: "display name at 201", code: "web", displayName: strings.Repeat("d", 201), kind: ChannelKindWeb, wantErr: true},
		{name: "empty display name", code: "web", displayName: "", kind: ChannelKindWeb, wantErr: true},
		{name: "unknown kind", code: "web", displayName: "Web", kind: ChannelKind("partner"), wantErr: true},
		{name: "empty kind", code: "web", displayName: "Web", kind: ChannelKind(""), wantErr: true},
		{name: "kind is case sensitive", code: "web", displayName: "Web", kind: ChannelKind("WEB"), wantErr: true},
		{name: "reseller kind", code: "partner-a", displayName: "Partner A", kind: ChannelKindReseller},
		{name: "presale kind", code: "presale", displayName: "Presale", kind: ChannelKindPresale},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateChannelWrite(tc.code, tc.displayName, tc.kind)
			if tc.wantErr && err == nil {
				t.Fatalf("validateChannelWrite(%q, %q, %q) = nil, want ErrChannelInvalidInput", tc.code, tc.displayName, tc.kind)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateChannelWrite(%q, %q, %q) = %v, want nil", tc.code, tc.displayName, tc.kind, err)
			}
		})
	}
}

func TestValidChannelKindIsClosed(t *testing.T) {
	// The four the PRD names, and nothing else. A fifth value is a coordinated
	// contract + schema + code change, never a data edit — so this test failing
	// means somebody widened the enum in one place only.
	for _, k := range []ChannelKind{ChannelKindWeb, ChannelKindPOS, ChannelKindPresale, ChannelKindReseller} {
		if !ValidChannelKind(k) {
			t.Errorf("ValidChannelKind(%q) = false, want true", k)
		}
	}
	for _, k := range []ChannelKind{"", "partner", "WEB", "web ", "kiosk"} {
		if ValidChannelKind(k) {
			t.Errorf("ValidChannelKind(%q) = true, want false", k)
		}
	}
}

// Bounds count CHARACTERS, not bytes — the invariant the whole 1..100 bound
// exists to hold.
//
// Found at ai-review. The gate used Go's `len()`, which counts UTF-8 bytes,
// while PostgreSQL's `length(text)` and OpenAPI's `maxLength` both count
// characters. So a 60-character code of two-byte characters is 120 bytes:
// accepted by the request validator, accepted by every SQL channel_code CHECK
// including fee_rules' and split_schedules', and rejected only by the store.
// That is precisely "a code legal in one place is unusable in another", which
// is the thing the five agreeing bounds exist to prevent.
//
// Every pre-existing fixture was ASCII, where bytes and characters agree — so
// the defect was invisible to a suite that looked thorough.
func TestChannelBoundsCountCharactersNotBytes(t *testing.T) {
	// 'é' is two bytes, one character. 100 of them is a legal code (100 chars)
	// that a byte-counting gate would see as 200 and refuse.
	code := strings.Repeat("é", maxChannelCodeLen)
	got, err := validateChannelWrite(code, "Box office", ChannelKindPOS)
	if err != nil {
		t.Fatalf("validateChannelWrite(100 two-byte chars) = %v, want nil — "+
			"PostgreSQL's length() and OpenAPI's maxLength both count characters, so this code is legal there", err)
	}
	if got != code {
		t.Fatalf("returned %q, want the input verbatim", got)
	}
	// And one character over is still refused, so the bound did not simply move.
	if _, err := validateChannelWrite(strings.Repeat("é", maxChannelCodeLen+1), "Box office", ChannelKindPOS); err == nil {
		t.Fatal("101 two-byte characters accepted, want ErrChannelInvalidInput — the bound must still bind")
	}

	// Same for display names, and with a 4-byte character so a UTF-16-unit
	// count (2 units per emoji) is also distinguishable from a rune count.
	name := strings.Repeat("🎫", maxChannelDisplayNameLen)
	if _, err := validateChannelWrite("pos", name, ChannelKindPOS); err != nil {
		t.Fatalf("validateChannelWrite(200 four-byte chars as display_name) = %v, want nil", err)
	}
	if _, err := validateChannelWrite("pos", strings.Repeat("🎫", maxChannelDisplayNameLen+1), ChannelKindPOS); err == nil {
		t.Fatal("201 four-byte characters accepted as a display name, want ErrChannelInvalidInput")
	}
}
