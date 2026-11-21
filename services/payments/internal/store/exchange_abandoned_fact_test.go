package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-498. The `order.exchange.abandoned` compensating fact: its exact payload allowlist and
// the arithmetic its values must satisfy.
//
// Each case starts from a VALID fact of the right delta sign and breaks exactly one predicate,
// so a refusal for another reason cannot keep a case green. The expected outcomes come from the
// fact's definition (plan v2, Build order 6), not from observed output:
//
//	amount        the target gross the abandonment removes (zero is legal)
//	source_gross  the source gross it restores
//	delta_amount  amount - source_gross, signed
//	refund_fact_id      upgrade only (delta > 0): the payments fact of the delta refund
//	written_off_amount  downgrade only (delta < 0): exactly -delta_amount
//	neither             even exchange (delta = 0)

const abandonedType = "order.exchange.abandoned"

// abandonedFact returns a valid fact for the given target gross and source gross. The delta
// sign follows from the two, and the sign-specific evidence is filled in to match.
func abandonedFact(targetGross, sourceGross int64) Fact {
	delta := targetGross - sourceGross
	p := map[string]string{
		"order_id":     uuid.NewString(),
		"exchange_id":  uuid.NewString(),
		"source_gross": itoa(sourceGross),
		"delta_amount": itoa(delta),
	}
	switch {
	case delta > 0:
		p["refund_fact_id"] = uuid.NewString()
	case delta < 0:
		p["written_off_amount"] = itoa(-delta)
	}
	return Fact{
		ID: uuid.New(), OrganizerID: uuid.New(), BuyerID: uuid.New(),
		Type: abandonedType, Amount: targetGross, Currency: "EUR",
		OccurredAt: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), Payload: p,
	}
}

func itoa(v int64) string {
	// strconv is the implementation's tool; the test builds strings by hand to avoid using
	// the code under test to produce its own expected encoding.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// Upgrade (target 4200 > source 3200), downgrade (2200 < 3200), even (3200 = 3200) and a zero
// target gross (an exchange down to a comp with no retained fee) all validate.
func TestExchangeAbandonedFactAcceptsEachDeltaSign(t *testing.T) {
	for name, f := range map[string]Fact{
		"upgrade":           abandonedFact(4200, 3200),
		"downgrade":         abandonedFact(2200, 3200),
		"even":              abandonedFact(3200, 3200),
		"zero target gross": abandonedFact(0, 3200),
	} {
		if err := validate(f); err != nil {
			t.Errorf("%s: a valid abandonment fact was refused: %v", name, err)
		}
	}
}

func TestExchangeAbandonedFactRefusesEachBrokenPredicate(t *testing.T) {
	type tc struct {
		name   string
		base   Fact
		mutate func(*Fact)
		want   string
	}
	up := func() Fact { return abandonedFact(4200, 3200) }
	down := func() Fact { return abandonedFact(2200, 3200) }
	even := func() Fact { return abandonedFact(3200, 3200) }
	cases := []tc{
		// Key allowlist: every required key, and no extra key.
		{"missing order_id", up(), func(f *Fact) { delete(f.Payload, "order_id") }, "order_id"},
		{"missing exchange_id", up(), func(f *Fact) { delete(f.Payload, "exchange_id") }, "exchange_id"},
		{"missing source_gross", up(), func(f *Fact) { delete(f.Payload, "source_gross") }, "source_gross"},
		{"missing delta_amount", up(), func(f *Fact) { delete(f.Payload, "delta_amount") }, "delta_amount"},
		{"unknown key", up(), func(f *Fact) { f.Payload["note"] = "x" }, `"note"`},
		// UUIDs: canonical, non-nil.
		{"malformed order_id", up(), func(f *Fact) { f.Payload["order_id"] = "not-a-uuid" }, "order_id"},
		{"non-canonical order_id", up(), func(f *Fact) { f.Payload["order_id"] = strings.ToUpper(f.Payload["order_id"]) }, "order_id"},
		{"nil exchange_id", up(), func(f *Fact) { f.Payload["exchange_id"] = uuid.Nil.String() }, "exchange_id"},
		{"braced exchange_id", up(), func(f *Fact) { f.Payload["exchange_id"] = "{" + f.Payload["exchange_id"] + "}" }, "exchange_id"},
		{"malformed refund_fact_id", up(), func(f *Fact) { f.Payload["refund_fact_id"] = "re_123" }, "refund_fact_id"},
		{"nil refund_fact_id", up(), func(f *Fact) { f.Payload["refund_fact_id"] = uuid.Nil.String() }, "refund_fact_id"},
		// Integers: base-10, canonical, in range.
		{"plus-signed source_gross", up(), func(f *Fact) { f.Payload["source_gross"] = "+3200" }, "source_gross"},
		{"zero-padded source_gross", up(), func(f *Fact) { f.Payload["source_gross"] = "03200" }, "source_gross"},
		{"decimal source_gross", up(), func(f *Fact) { f.Payload["source_gross"] = "3200.00" }, "source_gross"},
		{"negative source_gross", up(), func(f *Fact) {
			f.Amount = 800
			f.Payload["source_gross"] = "-200"
			f.Payload["delta_amount"] = "1000"
		}, "source_gross"},
		{"hex delta_amount", up(), func(f *Fact) { f.Payload["delta_amount"] = "0x3e8" }, "delta_amount"},
		{"negative zero delta_amount", even(), func(f *Fact) { f.Payload["delta_amount"] = "-0" }, "delta_amount"},
		{"overflowing delta_amount", up(), func(f *Fact) { f.Payload["delta_amount"] = "9223372036854775808" }, "delta_amount"},
		// Gross arithmetic: amount - source_gross = delta_amount.
		{"delta disagrees with the grosses", up(), func(f *Fact) { f.Payload["delta_amount"] = "999" }, "delta_amount"},
		{"swapped grosses", up(), func(f *Fact) { f.Amount, f.Payload["source_gross"] = 3200, "4200" }, "delta_amount"},
		// Sign-specific evidence.
		{"upgrade without refund_fact_id", up(), func(f *Fact) { delete(f.Payload, "refund_fact_id") }, "refund_fact_id"},
		{"upgrade with written_off_amount", up(), func(f *Fact) { f.Payload["written_off_amount"] = "1000" }, "written_off_amount"},
		{"downgrade without written_off_amount", down(), func(f *Fact) { delete(f.Payload, "written_off_amount") }, "written_off_amount"},
		{"downgrade with refund_fact_id", down(), func(f *Fact) { f.Payload["refund_fact_id"] = uuid.NewString() }, "refund_fact_id"},
		{"downgrade write-off other than -delta", down(), func(f *Fact) { f.Payload["written_off_amount"] = "999" }, "written_off_amount"},
		{"downgrade write-off malformed", down(), func(f *Fact) { f.Payload["written_off_amount"] = "+1000" }, "written_off_amount"},
		{"even with refund_fact_id", even(), func(f *Fact) { f.Payload["refund_fact_id"] = uuid.NewString() }, "refund_fact_id"},
		{"even with written_off_amount", even(), func(f *Fact) { f.Payload["written_off_amount"] = "0" }, "written_off_amount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.base
			if err := validate(f); err != nil {
				t.Fatalf("the base fact is invalid before the mutation: %v", err)
			}
			c.mutate(&f)
			err := validate(f)
			if err == nil {
				t.Fatalf("the fact was accepted with %s", c.name)
			}
			if !IsRefusal(err) {
				t.Fatalf("err = %v is not a caller-safe refusal", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to name %s", err, c.want)
			}
		})
	}
}

// The new keys belong to the new type only. Every existing type keeps the `order_id`-only
// payload rule, including the two exchange legs this fact compensates.
func TestExistingFactTypesKeepTheOrderIDOnlyPayload(t *testing.T) {
	for _, typ := range []string{"order.exchange.sold", "order.exchange.reversed", "order.refunded", "payment.refunded"} {
		f := Fact{
			ID: uuid.New(), OrganizerID: uuid.New(), BuyerID: uuid.New(), Type: typ, Amount: 1000, Currency: "EUR",
			Payload: map[string]string{"order_id": uuid.NewString()},
		}
		if err := validate(f); err != nil {
			t.Fatalf("%s with order_id only was refused: %v", typ, err)
		}
		for _, key := range []string{"exchange_id", "source_gross", "delta_amount", "refund_fact_id", "written_off_amount"} {
			g := f
			g.Payload = map[string]string{"order_id": f.Payload["order_id"], key: "1"}
			if err := validate(g); err == nil {
				t.Errorf("%s accepted the abandonment payload key %q", typ, key)
			}
		}
	}
}
