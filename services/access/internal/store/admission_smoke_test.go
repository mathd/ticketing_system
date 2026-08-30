//go:build smoke

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The admission union's rule, stated one vocabulary item at a time (TKT-299, ADR-025 §D2).
//
// Asserted here rather than only through SwitchExchange, because the exchange path can only
// reach the states an exchange fixture can build, and two of the six cases below — a bare
// `duplicate_admit` with no prior admission, and a reconciliation-learned quarantine row —
// are awkward or impossible to construct that way. A test that manufactured them through an
// exchange would be describing a state no writer produces, which is the shape that reads as
// a guarantee and proves nothing.
//
// Each case names the rule it comes from. The value being pinned is the RULE — "did a person
// go through a door?" — never what the current query happens to return.
func TestTicketAdmittedUnionCountsEachVocabularyCorrectly(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	for name, tc := range map[string]struct {
		// trail is appended through the real path, so the chain stays verifiable.
		trail []string
		// quarantine, when set, inserts one row; admitted decides whether it records a
		// live degraded admission or a reconciliation-learned occurrence.
		quarantine bool
		admitted   bool
		want       bool
		why        string
	}{
		"nothing at all": {
			want: false,
			why:  "a freshly issued ticket has not been anywhere",
		},
		"a single-entry redemption": {
			trail: []string{"redeemed"},
			want:  true,
			why:   "`redeemed` is the single-entry admission vocabulary (ADR-005)",
		},
		"a pass entry": {
			trail: []string{"entry"},
			want:  true,
			why:   "`entry` is the pass admission vocabulary (ADR-005); checking only `redeemed` would leave passes uncovered",
		},
		"a pass exit alone": {
			trail: []string{"exit"},
			want:  false,
			why:   "leaving is not entering — an `exit` without an `entry` is a broken stream, not an admission",
		},
		"a refused duplicate scan": {
			trail: []string{"duplicate_admit"},
			want:  false,
			why: "`duplicate_admit` records a REFUSED entry, so it does not make a ticket admitted " +
				"for a DECISION — counting it would deny an exchange to someone whose second scan " +
				"was correctly turned away. (admissionFacts separately counts it toward PASS " +
				"ALLOWANCE, because the person did walk in; that is a different question.)",
		},
		"a degraded admission, quarantine-side only": {
			quarantine: true, admitted: true,
			want: true,
			why: "ADR-021 §D6 admits once on a broken chain and records it ONLY on the quarantine " +
				"side; this is the case the exchange guard was blind to (TKT-299)",
		},
		"a reconciliation-learned occurrence": {
			quarantine: true, admitted: false,
			want: false,
			why: "a NULL admitted_at row records that an occurrence physically happened offline. " +
				"ADR-025 §D2: reconciliation is recording, not deciding — nothing was admitted here",
		},
		"a redemption and a later refused duplicate": {
			trail: []string{"redeemed", "duplicate_admit"},
			want:  true,
			why:   "admitted because of the `redeemed`; the `duplicate_admit` neither adds nor removes anything",
		},
	} {
		t.Run(name, func(t *testing.T) {
			org := uuid.New()
			_, _, seeds := issueOrder(t, ctx, st, org, 1)
			ticketID := seeds[0].ticketID

			for _, eventType := range tc.trail {
				if _, err := st.appendLifecycleForTest(ctx, ticketID, seeds[0].id.OrderID, org, eventType); err != nil {
					t.Fatalf("append %s: %v", eventType, err)
				}
			}
			if tc.quarantine {
				var admittedAt, occurredAt any
				if tc.admitted {
					admittedAt = time.Now().UTC()
				} else {
					occurredAt = time.Now().UTC()
				}
				if _, err := db.ExecContext(ctx, `
					INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at,occurrence_id,occurred_at,event_type)
					VALUES($1,$2,'test fixture',$3,$4,$5,'redeemed')`,
					ticketID, org, admittedAt, uuid.New(), occurredAt); err != nil {
					t.Fatalf("seed quarantine row: %v", err)
				}
			}

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			got, err := ticketAdmittedUnion(ctx, tx, ticketID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("admitted = %t, want %t — %s", got, tc.want, tc.why)
			}
		})
	}
}
