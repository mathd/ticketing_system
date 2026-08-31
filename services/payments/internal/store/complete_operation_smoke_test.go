//go:build smoke

package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TKT-298 finding 2. CompleteOperation discarded its UPDATE's result entirely, while both
// its siblings re-read on zero rows and answer two different questions differently.
//
// The UPDATE is guarded on `status IS NULL`, so it writes nothing whenever the operation is
// already resolved. That has TWO causes and they need OPPOSITE answers — the distinction
// CompleteCompensation documents at length (store.go, its own zero-row branch) and
// CompleteRefundLeg repeats:
//
//   - BENIGN: a concurrent duplicate. Two requests both pass the handler's completed-check,
//     both append the same deterministic fact (the journal dedupes them), and both arrive
//     here. One wins on `status IS NULL`. The loser's work is DONE — same fact, same
//     provider operation — so an error would turn a successful duplicate into a 500 and a
//     pointless recovery retry.
//   - DANGEROUS: the row is missing, still unresolved, or was completed under a DIFFERENT
//     fact. There the caller has appended to an append-only journal and believes the money
//     is durably recorded while nothing records it — or reports another completion's result
//     as its own.
//
// Discarding the result collapsed both onto "success", so the charge handler answered 200
// with its own status while the row held another outcome, or none.
//
// TIER. This is a store test because the mechanism is `RowsAffected` under a partial-index
// guard plus a re-read — a fake journal would enforce in Go whatever it was written to
// enforce, which is the shape AGENTS.md names as a green test proving only that the fake
// and the caller agree.

func TestCompleteOperationDistinguishesTheDuplicateFromTheUnrecordedFact(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))

	// A bound-but-unresolved row, the state BindOperation leaves before a provider answers.
	seed := func(t *testing.T, org uuid.UUID, key string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,order_id,buyer_id,
			                               request_amount,request_currency,payment_method_ref)
			VALUES($1,$2,'fingerprint',$3,$4,2500,'EUR','fake-ok')`,
			org, key, uuid.New(), uuid.New()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org) })
	}
	prov := ProviderResult{State: "captured", AuthorizedAmount: 2500, CapturedAmount: 2500,
		ConfirmedCapturedAmount: func() *int64 { v := int64(2500); return &v }(), ConfirmedCurrency: "EUR"}

	// The BENIGN case. Two completions of the same operation under the SAME fact: the
	// second writes no row and must still be reported as success, because its work is done.
	t.Run("duplicate under the same fact converges", func(t *testing.T) {
		org, key, factID := uuid.New(), "complete-dup", uuid.New()
		seed(t, org, key)
		if err := j.CompleteOperation(ctx, org, key, "captured", factID, prov); err != nil {
			t.Fatalf("first completion: %v", err)
		}
		if err := j.CompleteOperation(ctx, org, key, "captured", factID, prov); err != nil {
			t.Fatalf("a duplicate completion under the same fact must converge, got: %v", err)
		}
		// And it converged on the FIRST completion rather than overwriting it.
		op, found, err := j.LookupOperation(ctx, org, key)
		if err != nil || !found {
			t.Fatalf("lookup after duplicate: found=%v err=%v", found, err)
		}
		if op.FactID != factID || op.Status != "captured" {
			t.Fatalf("operation = status %q fact %s, want captured/%s", op.Status, op.FactID, factID)
		}
	})

	// The DANGEROUS case, first half: the operation does not exist at all. The caller has
	// appended a fact to an append-only journal and nothing durable records it.
	t.Run("a missing operation is an error", func(t *testing.T) {
		org, key := uuid.New(), "complete-missing"
		err := j.CompleteOperation(ctx, org, key, "captured", uuid.New(), prov)
		if !errors.Is(err, ErrOperationNotCompleted) {
			t.Fatalf("completing a missing operation = %v, want ErrOperationNotCompleted", err)
		}
	})

	// The DANGEROUS case, second half, and the one a naive "row exists → fine" check would
	// miss: the row IS completed, but by somebody else's fact. Converging here would report
	// another completion's result as this call's.
	t.Run("completion under a different fact is an error", func(t *testing.T) {
		org, key := uuid.New(), "complete-other-fact"
		seed(t, org, key)
		winner := uuid.New()
		if err := j.CompleteOperation(ctx, org, key, "captured", winner, prov); err != nil {
			t.Fatalf("first completion: %v", err)
		}
		loser := uuid.New()
		err := j.CompleteOperation(ctx, org, key, "declined", loser, ProviderResult{State: "declined"})
		if !errors.Is(err, ErrOperationNotCompleted) {
			t.Fatalf("completing an operation already resolved under fact %s = %v, want ErrOperationNotCompleted",
				winner, err)
		}
		// The refusal changed nothing: the winner's completion stands untouched. An error
		// that still clobbered the row would be worse than the silent no-op it replaced.
		op, found, err := j.LookupOperation(ctx, org, key)
		if err != nil || !found {
			t.Fatalf("lookup after refusal: found=%v err=%v", found, err)
		}
		if op.FactID != winner || op.Status != "captured" {
			t.Fatalf("operation = status %q fact %s, want the winner's captured/%s",
				op.Status, op.FactID, winner)
		}
	})
}
