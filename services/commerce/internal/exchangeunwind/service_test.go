package exchangeunwind

// The DECISION tier. No database, no HTTP: these pin which payments record the unwind
// consults and what it concludes, which is the single highest-risk judgement in TKT-255.
//
// Why it deserves its own tier: the sign bug is invisible everywhere else. An
// implementation that asks the operations endpoint about every exchange gets 404 for every
// downgrade — because a downgrade's money lives in a different payments table — and 404 is
// the one answer that means "safe to unwind". So the defect presents as a clean success and
// the buyer whose refund already settled loses their binding. A store test cannot see it
// (no payments), and an api smoke test would need both endpoints wired to catch it.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// recordingPayments answers from a script and records what it was asked. Both halves
// matter: the answer drives the decision, and the RECORD is what proves the right endpoint
// was consulted — a test asserting only the verdict passes when both methods return the
// same thing, which is exactly the arrangement the sign bug produces.
type recordingPayments struct {
	chargeCalls []string
	refundCalls [][2]string

	chargeEvidence, refundEvidence MoneyEvidence
	chargeErr, refundErr           error
}

func (p *recordingPayments) LookupChargeOperation(_ context.Context, _ uuid.UUID, key string) (MoneyEvidence, error) {
	p.chargeCalls = append(p.chargeCalls, key)
	return p.chargeEvidence, p.chargeErr
}

func (p *recordingPayments) LookupRefundLeg(_ context.Context, _ uuid.UUID, sourceKey, refundKey string) (MoneyEvidence, error) {
	p.refundCalls = append(p.refundCalls, [2]string{sourceKey, refundKey})
	return p.refundEvidence, p.refundErr
}

func wedged(delta int64, basis bool) store.WedgedExchange {
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	return store.WedgedExchange{
		OrganizerID:      org,
		ID:               uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		BasisRecorded:    basis,
		DeltaAmount:      delta,
		PaymentSourceKey: "source-checkout-key",
		Currency:         "EUR",
	}
}

// An upgrade is asked about at the OPERATIONS endpoint, and the refund-leg endpoint is not
// consulted at all.
//
// The negative half is the load-bearing one. Asserting only "the charge lookup happened"
// stays green for an implementation that calls BOTH endpoints and refuses if either answers
// — which would refuse every downgrade whose source order carries an unrelated refund leg.
func TestAnUpgradeIsAskedAboutAtTheOperationsEndpoint(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Absent, refundEvidence: Present}
	s := New(nil, p)

	got, err := s.Evidence(context.Background(), wedged(+1000, true))
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if got != Absent {
		t.Errorf("evidence = %v, want absent — the operations endpoint said absent and it is "+
			"the one that governs an upgrade", got)
	}
	want := "exchange-charge:22222222-2222-2222-2222-222222222222"
	if len(p.chargeCalls) != 1 || p.chargeCalls[0] != want {
		t.Errorf("charge lookups = %v, want exactly [%s]", p.chargeCalls, want)
	}
	if len(p.refundCalls) != 0 {
		t.Errorf("the refund-leg endpoint was consulted %d time(s) for an UPGRADE: %v. An "+
			"upgrade's money is a charge operation; consulting the refund leg as well makes an "+
			"unrelated leg on the source order refuse a safe unwind", len(p.refundCalls), p.refundCalls)
	}
}

// A downgrade is asked about at the REFUND-LEG endpoint, with both keys, and the operations
// endpoint is not consulted.
//
// THIS IS THE TEST THAT CATCHES THE SIGN BUG. The fixture is deliberately adversarial: the
// operations endpoint answers Absent (the answer that permits an unwind) while the
// refund-leg endpoint answers Present. An implementation that consults the wrong endpoint
// therefore concludes "no money moved" and returns success — so this test fails on the
// verdict, not merely on the call record, which is what makes it robust to a rewrite that
// stops recording calls.
func TestADowngradeIsAskedAboutAtTheRefundLegEndpointWithBothKeys(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Absent, refundEvidence: Present}
	s := New(nil, p)

	got, err := s.Evidence(context.Background(), wedged(-1000, true))
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if got != Present {
		t.Fatalf("evidence = %v, want present. A downgrade's money is a REFUND LEG, in a "+
			"different payments table reached by a different endpoint. If this reads absent, the "+
			"implementation asked /internal/operations — which answers 404 for a refund key, and "+
			"404 is the one answer that permits deleting the binding of a buyer already refunded", got)
	}
	if len(p.chargeCalls) != 0 {
		t.Errorf("the operations endpoint was consulted %d time(s) for a DOWNGRADE: %v", len(p.chargeCalls), p.chargeCalls)
	}
	wantSource, wantRefund := "source-checkout-key", "exchange-refund:22222222-2222-2222-2222-222222222222"
	if len(p.refundCalls) != 1 {
		t.Fatalf("refund-leg lookups = %v, want exactly one", p.refundCalls)
	}
	if p.refundCalls[0][0] != wantSource || p.refundCalls[0][1] != wantRefund {
		t.Errorf("refund-leg keys = %v, want [%s %s]. Payments requires BOTH: (organizer, source "+
			"key) identifies a charge that may carry many legs, and only the refund key picks this one",
			p.refundCalls[0], wantSource, wantRefund)
	}
}

// An even exchange calls nobody, because settleExchangeDelta calls nobody.
func TestAnEvenExchangeAsksPaymentsNothing(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Present, refundEvidence: Present}
	s := New(nil, p)

	got, err := s.Evidence(context.Background(), wedged(0, true))
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if got != Absent {
		t.Errorf("evidence = %v, want absent — a zero delta makes no provider call at all, so "+
			"there is nothing for payments to have recorded", got)
	}
	if len(p.chargeCalls)+len(p.refundCalls) != 0 {
		t.Errorf("payments was consulted for a zero-delta exchange: charges=%v legs=%v. Both stubs "+
			"answer Present here precisely so that consulting either one fails this test",
			p.chargeCalls, p.refundCalls)
	}
}

// An exchange with NO BASIS asks payments nothing, and the proof is an ordering argument
// rather than a read: RecordExchangeBasis commits before settleExchangeDelta is called
// (ADR-039 §3c), so a row with no basis never reached the provider.
//
// Both stubs answer Present, so an implementation that asks anyway fails here rather than
// silently making a network call during an incident.
func TestAnExchangeWithNoBasisAsksPaymentsNothing(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Present, refundEvidence: Present}
	s := New(nil, p)

	got, err := s.Evidence(context.Background(), wedged(0, false))
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if got != Absent {
		t.Errorf("evidence = %v, want absent — the basis is persisted BEFORE any provider call, "+
			"so an exchange that never recorded one never reached payments", got)
	}
	if len(p.chargeCalls)+len(p.refundCalls) != 0 {
		t.Errorf("payments was consulted for an exchange with no basis: charges=%v legs=%v",
			p.chargeCalls, p.refundCalls)
	}
}

// A basis-less exchange carries a stale delta on the struct and must still ask nothing.
//
// This is a real hazard rather than a hypothetical: WedgedExchange.DeltaAmount is scanned
// from a NULL column into an int64, so it reads as 0 — but a caller constructing the struct
// by hand, or a future column default, could present a non-zero delta with no basis. The
// basis check must come FIRST. An implementation that switches on the sign before checking
// the basis makes a provider call for a row that provably never had one.
func TestTheBasisCheckPrecedesTheSignSwitch(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Present, refundEvidence: Present}
	s := New(nil, p)

	w := wedged(+1000, false) // a positive delta the row never legitimately holds
	got, err := s.Evidence(context.Background(), w)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if got != Absent {
		t.Errorf("evidence = %v, want absent. With no basis there is nothing to ask about, "+
			"whatever the delta field says", got)
	}
	if len(p.chargeCalls) != 0 {
		t.Errorf("the sign was switched on before the basis was checked: charges=%v", p.chargeCalls)
	}
}

// A payments error is NEVER absence. Every read failure resolves to Indeterminate, which
// refuses — the distinction between "payments says no operation exists" and "payments could
// not be reached" is what this whole mechanism turns on.
func TestAPaymentsErrorIsIndeterminateNeverAbsent(t *testing.T) {
	p := &recordingPayments{chargeEvidence: Indeterminate, chargeErr: errors.New("connection refused")}
	s := New(nil, p)

	got, err := s.Evidence(context.Background(), wedged(+1000, true))
	if err == nil {
		t.Fatal("Evidence returned no error though payments could not be reached")
	}
	if got == Absent {
		t.Error("a payments failure read as ABSENT. An outage would then permit deleting the " +
			"binding of a charged buyer, which is the failure this evidence type exists to prevent")
	}
}

// Indeterminate is the ZERO VALUE, so a caller that forgets to decide refuses.
func TestTheZeroEvidenceValueRefuses(t *testing.T) {
	var m MoneyEvidence
	if m != Indeterminate {
		t.Fatalf("the zero MoneyEvidence is %v, want Indeterminate. The guard must fail closed "+
			"by construction: a future branch that falls through without deciding has to refuse, "+
			"not permit", m)
	}
	if Absent == Indeterminate || Present == Indeterminate {
		t.Fatal("the three outcomes must be distinct")
	}
}

// The derived keys are exactly the ones settleExchangeDelta uses. If these drift, the unwind
// asks payments about an operation that does not exist and reads its absence as safety.
func TestTheDerivedKeysMatchTheSettlementPath(t *testing.T) {
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	if got, want := ChargeKey(id), "exchange-charge:22222222-2222-2222-2222-222222222222"; got != want {
		t.Errorf("ChargeKey = %q, want %q (api/exchanges.go builds \"exchange-charge:\"+ex.ID)", got, want)
	}
	if got, want := RefundKey(id), "exchange-refund:22222222-2222-2222-2222-222222222222"; got != want {
		t.Errorf("RefundKey = %q, want %q (api/exchanges.go builds \"exchange-refund:\"+ex.ID)", got, want)
	}
}
