package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

func TestSubcommandsRegisterValidateRules(t *testing.T) {
	if _, ok := subcommands()["validate-rules"]; !ok {
		t.Fatal(`subcommands() lacks "validate-rules"`)
	}
}

func fakeFindings() []store.RuleCurrencyMismatch {
	return []store.RuleCurrencyMismatch{
		{
			Kind:               store.RuleKindPrice,
			RuleID:             uuid.MustParse("11111111-1111-4111-8111-111111111111"),
			OrganizerID:        uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			TicketTypeID:       uuid.MustParse("33333333-3333-4333-8333-333333333333"),
			ScopeLevel:         "venue",
			ScopeID:            uuid.MustParse("44444444-4444-4444-8444-444444444444"),
			RuleCurrency:       "USD",
			TicketTypeCurrency: "EUR",
			ChannelCode:        ptrString("pos"),
		},
		{
			Kind:               store.RuleKindFee,
			RuleID:             uuid.MustParse("55555555-5555-4555-8555-555555555555"),
			OrganizerID:        uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			TicketTypeID:       uuid.MustParse("33333333-3333-4333-8333-333333333333"),
			ScopeLevel:         "event",
			ScopeID:            uuid.MustParse("66666666-6666-4666-8666-666666666666"),
			FeeCode:            "service",
			RuleCurrency:       "GBP",
			TicketTypeCurrency: "EUR",
		},
	}
}

func ptrString(s string) *string { return &s }

// The report is what an operator acts on, so every field they need to FIND the
// offending row has to be in it: which table, which rule, which ticket type, and
// the two currencies that disagree.
func TestValidateRulesPrintsEveryFieldNeededToFindTheRow(t *testing.T) {
	var out bytes.Buffer
	err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return fakeFindings(), nil },
		stdout: &out,
	}, nil)
	if err != nil {
		t.Fatalf("validate-rules: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"price", "fee",
		"11111111-1111-4111-8111-111111111111",
		"55555555-5555-4555-8555-555555555555",
		"33333333-3333-4333-8333-333333333333",
		"venue", "event",
		"USD", "GBP", "EUR",
		"pos", "service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report omits %q, which an operator needs to find the row:\n%s", want, got)
		}
	}
}

// COS (c): it reports, an operator decides. Findings are a successful run — a
// non-nil error here would make main() exit 1 and turn the sweep into a gate,
// which is exactly what the ticket rules out.
func TestValidateRulesTreatsFindingsAsSuccess(t *testing.T) {
	var out bytes.Buffer
	err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return fakeFindings(), nil },
		stdout: &out,
	}, nil)
	if err != nil {
		t.Fatalf("findings must not be an error — the sweep gates nothing (COS c): %v", err)
	}
}

// An empty result is a real answer and must say so, rather than printing nothing
// and leaving the operator unsure whether the command ran.
func TestValidateRulesReportsACleanSweepExplicitly(t *testing.T) {
	var out bytes.Buffer
	if err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return nil, nil },
		stdout: &out,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no currency mismatches") {
		t.Errorf("a clean sweep must say so explicitly:\n%s", out.String())
	}
}

// ADR-021: name the adversary. This sweep reads the same database an attacker
// with write access would write, so it is an operator aid under honest-writer
// assumptions and must never be mistaken for an integrity control. The
// disclaimer prints on every run, findings or not — a scope statement that only
// appears when something is wrong is absent exactly when it is being relied on.
func TestValidateRulesAlwaysStatesItsTrustBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings []store.RuleCurrencyMismatch
	}{
		{"with findings", fakeFindings()},
		{"clean", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := validateRules(validateRulesDeps{
				list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return tc.findings, nil },
				stdout: &out,
			}, nil); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, "NOT covered") || !strings.Contains(got, "ADR-021") {
				t.Errorf("every run must state what the sweep does not cover (ADR-021):\n%s", got)
			}
		})
	}
}

// A database failure is an operational failure: it must surface as an error so
// main() exits 1, and it must NOT be reported as a clean sweep. Swallowing it
// would tell an operator "no mismatches" when the truth is "nothing was read" —
// the most dangerous output this command could produce.
func TestValidateRulesPropagatesOperationalFailure(t *testing.T) {
	boom := errors.New("connection refused")
	var out bytes.Buffer
	err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return nil, boom },
		stdout: &out,
	}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("a read failure must propagate, got %v", err)
	}
	if strings.Contains(out.String(), "no currency mismatches") {
		t.Errorf("a failed read must never print a clean-sweep verdict:\n%s", out.String())
	}
}

// failingWriter fails every write, standing in for a closed pipe — an operator
// piping the report into `head` is the ordinary way this happens.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A report that could not be delivered is not a successful run. Exiting 0 after
// failing to write would tell the operator's shell the sweep succeeded while
// they hold none of its output.
func TestValidateRulesFailsWhenTheReportCannotBeWritten(t *testing.T) {
	broken := errors.New("broken pipe")
	err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return fakeFindings(), nil },
		stdout: failingWriter{err: broken},
	}, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("a failed write must propagate, got %v", err)
	}
}

func TestValidateRulesRejectsUnexpectedArguments(t *testing.T) {
	var out bytes.Buffer
	err := validateRules(validateRulesDeps{
		list:   func(context.Context) ([]store.RuleCurrencyMismatch, error) { return nil, nil },
		stdout: &out,
	}, []string{"unexpected"})
	if err == nil {
		t.Fatal("unexpected arguments must be refused")
	}
}
