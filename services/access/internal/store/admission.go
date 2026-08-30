package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// The admission union (ADR-025 §Decision 2).
//
// > authoritative admission history is the union of the lifecycle trace and the quarantine
// > record, and both readers *and admission decisions* must consult the union.
//
// A §D6 degraded admission is recorded ONLY on the quarantine side — appending onto an
// unverified predecessor would poison the chain — so a reader that consults the trace alone
// sees an unadmitted ticket where a person walked through a door.
//
// TWO QUESTIONS LIVE HERE, and conflating them is the mistake this file exists to prevent.
// They are keyed differently and they are not the same rule:
//
//   - **Ticket-level** — "has this ticket been admitted at all?" Keyed by ticket_id, joined
//     with quarantine rows carrying a non-null admitted_at. This is what an admission
//     DECISION asks (the exchange guard, the live scan's duplicate check).
//   - **Occurrence-level** — "is this specific occurrence already recorded?" Keyed by
//     occurrence_id. This is what a REPLAY asks, and its answer is a row, not a verdict:
//     each caller maps it onto its own result type with its own direction semantics.
//
// TKT-299's defect was entirely in the first; the drift it also fixes was entirely in the
// second. Two callers share the ticket-level predicate below — the exchange guard and
// reconciliation's prior-admission check. `redeemSingle` deliberately does NOT: it asks the
// narrower "has this ticket taken its one §D6 degraded admission?", so it keys on
// `admitted_at` alone. Three questions, one storage shape. What is shared is the STORAGE SHAPE — which tables hold admission evidence, and
// which quarantine rows count as an admission rather than a recording. What is not shared
// is the verdict. The three occurrence callers deliberately keep their own wrappers.
//
// Scope of the guarantee, per ADR-021: this is honest-writer consistency, not
// tamper-evidence. A writer with database access can insert or delete quarantine rows at
// will, and nothing here constrains that.

// admissionEvidence is one row of admission evidence, from either side of the union.
type admissionEvidence struct {
	TicketID   uuid.UUID
	EventType  string
	OccurredAt sql.NullTime
	// AdmittedAt is set only on a quarantine row recording a live degraded admission
	// (ADR-021 §D6). It is NULL on a reconciliation-learned row, which records that an
	// occurrence physically happened offline — a recording, not a live admission.
	AdmittedAt sql.NullTime
	Reason     string
	// Quarantined distinguishes the two sides of the union for callers that must report
	// them differently. A trail row and a quarantine row are both evidence; only one of
	// them is chained.
	Quarantined bool
}

// admittingEventTypes are the trail vocabularies that mean a person went through a door.
//
// `redeemed` is the single-entry vocabulary and `entry` the pass one (ADR-005); both count.
// Two types deliberately do NOT:
//
//   - `exit` is the other half of the pass stream, and leaving is not entering.
//   - `duplicate_admit` marks an occurrence the record treats as a CONFLICT rather than as
//     this ticket's admission — reconciliation appends it for an offline occurrence that
//     arrived after the ticket was already admitted, and a live denial appends nothing at
//     all. Counting it would refuse an exchange to someone whose second scan was correctly
//     turned away, punishing them for our own denial.
//
// That second exclusion is narrower than it looks, and the narrowness is the point.
// `admissionFacts` (scan.go) deliberately DOES consume a `duplicate_admit` as a physical
// entry when deriving pass allowance, because the person did walk in. Two different
// questions: *was a decision already made about this ticket?* is not *how many entries has
// this pass used?* This predicate answers only the first.
var admittingEventTypes = []string{"redeemed", "entry"}

// ticketAdmittedUnion answers the ticket-level question over the whole union: has this
// ticket been used to go through a door, by any route the system records?
//
// This is the single definition every ADMISSION DECISION consults. Deleting either arm
// below is a live defect, and both arms are pinned by tests that fail without them.
// The quarantine arm keys on WHAT WAS RECORDED, not on who decided it. Both kinds of
// quarantine row are evidence that a person went through a door:
//
//   - `admitted_at` SET — Access itself let them through, under §D6, on a chain that did
//     not verify. `event_type` may be absent on these rows; the admission is the fact.
//   - `admitted_at` NULL with an admitting `event_type` — an OFFLINE gate let them
//     through and Access learned of it later, while the chain happened to be broken.
//     ADR-025 §D2: "reconciliation of admissions that already physically happened is
//     recording, not deciding" — the person is already inside either way.
//
// An earlier version of this predicate keyed on `admitted_at IS NOT NULL`, reading the
// second kind as "nothing was admitted". That is the same defect this file exists to fix,
// one step further out: the exchange guard was still blind, just to offline admissions
// instead of degraded ones. Confirmed by running it — a ticket someone entered on at an
// offline gate was exchanged into a fresh unredeemed replacement (TKT-299 ai-review).
//
// A quarantined `exit` is still excluded, by the same `admittingEventTypes` filter the
// trail arm uses: leaving is not entering.
func ticketAdmittedUnion(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (bool, error) {
	var admitted bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM lifecycle_events
			WHERE ticket_id=$1 AND event_type = ANY($2)
			UNION ALL
			SELECT 1 FROM lifecycle_integrity_quarantine
			WHERE ticket_id=$1 AND (admitted_at IS NOT NULL OR event_type = ANY($2))
		)`, ticketID, admittingEventTypes).Scan(&admitted)
	return admitted, err
}

// quarantinedOccurrence reads the quarantine-side record for one occurrence id, if any.
//
// Callers map the row onto their own verdict; this owns only the storage shape. The
// distinction every caller must then make is `AdmittedAt`:
//
//   - VALID — a live degraded admission. Its retry replays as such, un-directioned: §D3's
//     identity rule extends to degraded admissions.
//   - NULL — a reconciliation-learned occurrence. Nothing was decided; a factual
//     entry/exit was recorded while the chain was broken, and its retry must be bound to
//     the stored direction, not labelled a degraded admission.
//
// `replayByOccurrence` used to ignore EventType entirely and label every quarantine row a
// degraded admission, including the NULL-admitted_at rows where no degraded admission ever
// happened (TKT-299).
func quarantinedOccurrence(ctx context.Context, tx *sql.Tx, occ uuid.UUID) (admissionEvidence, bool, error) {
	var row admissionEvidence
	var eventType sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT ticket_id,event_type,admitted_at,occurred_at
		FROM lifecycle_integrity_quarantine WHERE occurrence_id=$1`, occ).
		Scan(&row.TicketID, &eventType, &row.AdmittedAt, &row.OccurredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return admissionEvidence{}, false, nil
	}
	if err != nil {
		return admissionEvidence{}, false, err
	}
	row.EventType = eventType.String
	row.Quarantined = true
	return row, true, nil
}
