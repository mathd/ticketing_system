//go:build smoke

package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TKT-280. One durable state, two paths, two contradictory answers.
//
// `orders.status='release_pending'` WITH `recovery_parked_at` set is a row no worker will
// ever advance: ReleaseStuckOrder sets the marker and deliberately leaves the status alone,
// and ClaimStuckOrders excludes parked rows. Parked means a human must act.
//
// The replay branch says so (409 "order awaiting payment reconciliation"). answerRecovered
// said 202 for the same row, because its only query read `status` and never the marker —
// and 202 promises that something will advance this order, which is false once the row is
// parked.
//
// THE ASSERTION IS THE AGREEMENT, NOT THE LITERAL. Two separate checks that each path
// answers 409 would pass today and drift again tomorrow, which is exactly how this defect
// was born: the replay branch's comment asserted the two paths agreed, that was TRUE when
// written, and a fix applied to one branch only falsified it. So this seeds ONE row and
// compares the two responses to each other. The literal is asserted once, afterwards, and
// derived from the requirement (a parked order tells every caller a human must act) rather
// than from what either path currently returns.
//
// THE TIER IS THE POINT (AGENTS.md). answerRecovered reads the database. The ordinary API
// tests in this package build servers with a nil db and cannot construct
// "release_pending + recovery_parked_at set", which is the entire precondition. The store
// tier has the row but no handler, so it cannot observe what a caller is told. Only a
// DB-backed API test sees both.
//
// NOTE ON RUNNING IT: exchangeAPIDB skips when COMMERCE_API_TEST_DATABASE_URL is unset, so
// a plain `go test ./services/commerce/internal/api/` reports ok WITHOUT running this. The
// red observation for this ticket was made under the smoke stack, where the variable is set.
func TestParkedReleasePendingGetsTheSameAnswerFromBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parked bool
		want   int
	}{
		// Parked: no worker will ever advance this row, so 202 would be a false promise.
		{name: "parked", parked: true, want: http.StatusConflict},
		// UNPARKED CONTROL, and it is not decoration: it is the regression that would
		// silently break recovery. A fix that routed every release_pending order to
		// reconciliation would satisfy the parked case and destroy this one.
		{name: "unparked", parked: false, want: http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			f := seedExchangeSource(t, db, ctx, "parked-answer-"+tc.name+"-"+uuid.NewString(), 1, 2500)

			// Any non-empty token reaches claimOrder now (TKT-301): commerce treats the
			// payment token as opaque and payments alone judges it. This used to need
			// fakepsp.TokenSuccess, because checkout refused anything outside the local
			// simulator's vocabulary with 400 "invalid checkout" before claimOrder was
			// reached — which made a made-up token render the replay path unreachable and
			// the comparison vacuous. What still matters is that the token is NON-EMPTY;
			// that is the one thing commerce still refuses on its own.
			const name, email, token = "Parked Buyer", "parked@example.test", "pm_parked_instrument"
			fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s",
				f.reservation, name, strings.ToLower(email), token))))
			key := "checkout-" + tc.name + "-" + uuid.NewString()

			// The reservation must not be `completed`: this order is mid-recovery.
			if _, err := db.ExecContext(ctx,
				`UPDATE reservations SET status='finalizing' WHERE id=$1`, f.reservation); err != nil {
				t.Fatal(err)
			}

			// terminal_outcome is NOT optional padding. RecordTerminalOutcome writes it and
			// status='release_pending' in ONE statement, and the runner treats a
			// release_pending row without an outcome as a bug (server.go:1585-1587). A
			// fixture omitting it would pin behaviour on a row the system considers
			// impossible.
			parkedAt := "NULL"
			if tc.parked {
				parkedAt = "now()"
			}
			if _, err := db.ExecContext(ctx, `
				UPDATE orders SET status='release_pending', terminal_outcome='declined',
				    idempotency_key=$2, request_fingerprint=$3,
				    recovery_attempts=10, recovery_last_error='attempts exhausted',
				    recovery_claim_id=NULL, recovery_lease_until=NULL,
				    recovery_parked_at=`+parkedAt+`,
				    updated_at=now()-interval '10 minutes'
				WHERE id=$1`, f.order, key, fingerprint); err != nil {
				t.Fatal(err)
			}

			// Prove the fixture reaches the state it claims BEFORE asserting anything about
			// it. A fixture that silently failed to write the marker would make the parked
			// case pass for the wrong reason.
			var marker sql.NullTime
			if err := db.QueryRowContext(ctx,
				`SELECT recovery_parked_at FROM orders WHERE id=$1`, f.order).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if marker.Valid != tc.parked {
				t.Fatalf("fixture did not reach the %s state: recovery_parked_at valid=%v, want %v",
					tc.name, marker.Valid, tc.parked)
			}

			srv := newTestServer(db, http.DefaultClient, "", "", "", "tok")

			// Path A — answerRecovered, called directly. This is the guarded-write loser's
			// answer: the path a checkout takes when recovery won the race for its order.
			x, err := srv.load(ctx, f.reservation)
			if err != nil {
				t.Fatal(err)
			}
			recA := httptest.NewRecorder()
			if !srv.answerRecovered(ctx, recA, x, f.order) {
				t.Fatal("answerRecovered declined to answer for a release_pending order")
			}

			// Path B — the replay branch, reached through the real checkout handler with the
			// matching idempotency key and fingerprint, so claimOrder returns the existing
			// order rather than creating one.
			r := chi.NewRouter()
			r.Post("/reservations/{id}/checkout", srv.checkout)
			body := fmt.Sprintf(`{"reservation_id":%q,"name":%q,"email":%q,"payment_token":%q}`,
				f.reservation, name, email, token)
			req := httptest.NewRequest(http.MethodPost,
				"/reservations/"+f.reservation.String()+"/checkout", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			recB := httptest.NewRecorder()
			r.ServeHTTP(recB, req)

			// THE REQUIREMENT: one durable state, one answer. Compare the paths to each
			// other first — this is the assertion that cannot drift.
			if recA.Code != recB.Code {
				t.Fatalf("the two paths disagree about one durable state: answerRecovered=%d, replay=%d\n  A: %s\n  B: %s",
					recA.Code, recB.Code, recA.Body.String(), recB.Body.String())
			}
			if !sameJSONAnswer(t, recA.Body.Bytes(), recB.Body.Bytes()) {
				t.Fatalf("the two paths agree on the code and disagree on the body:\n  A: %s\n  B: %s",
					recA.Body.String(), recB.Body.String())
			}

			// And the shared answer is the one the requirement demands, derived from what
			// the state MEANS rather than from what either path happened to return.
			if recA.Code != tc.want {
				t.Fatalf("a %s release_pending order must be answered %d, both paths said %d: %s",
					tc.name, tc.want, recA.Code, recA.Body.String())
			}

			// The CODE is not the whole answer, and agreement alone does not settle it
			// (ai-review [medium]): two paths could consistently return 409 with an
			// unrelated body, or omit the message entirely, and everything above would
			// still pass. What a parked order owes the buyer is a statement that a human
			// must act — so assert the field that carries it.
			var answer map[string]any
			if err := json.Unmarshal(recA.Body.Bytes(), &answer); err != nil {
				t.Fatalf("response body is not JSON: %s", recA.Body.String())
			}
			if got := answer["status"]; got != "release_pending" {
				t.Fatalf("the answer must echo the durable status, got %v: %s", got, recA.Body.String())
			}
			if tc.parked {
				// The same message reconciliation_required gets, because it means the
				// same thing to a buyer: nothing will advance this without a human.
				if got := answer["error"]; got != "order awaiting payment reconciliation" {
					t.Fatalf("a parked order must tell the buyer a human must act, got %v: %s",
						got, recA.Body.String())
				}
			} else if _, present := answer["error"]; present {
				// The unparked control's other half: recovery is still working on this
				// order, so there is nothing to escalate and no error to report.
				t.Fatalf("an unparked release_pending order must not report an error: %s",
					recA.Body.String())
			}
		})
	}
}

// sameJSONAnswer compares two response bodies as JSON values, ignoring key order and
// whitespace. Comparing raw bytes would make this brittle against a formatting change that
// is not what the test is about.
func sameJSONAnswer(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("path A body is not JSON: %s", a)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("path B body is not JSON: %s", b)
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
}

// TKT-280, ai-review [high]. The test above proves answerRecovered ANSWERS correctly; it
// calls it directly, so deleting every call site in checkout would leave it green while
// real buyers went back to the optimistic 202. That is the AGENTS.md wiring rule (TKT-202):
// ask which edit your test catches -- breaking the mechanism, or REMOVING IT FROM THE PLACE
// THAT USES IT -- and assert at the boundary the value crosses on its way out.
//
// So this one never mentions answerRecovered. It drives the real checkout handler and
// constructs the actual race: the order is `created` when claimOrder reads it, and the
// payments stub parks it as release_pending mid-request -- exactly what a recovery pass
// winning the race does. The guarded UPDATE that follows is scoped to
// ('created','payment_unknown','confirmation_pending'), so it matches zero rows, and the
// handler must consult the durable truth instead of answering its optimistic 202.
//
// The assertion is on the HANDLER's response. Nothing here can be satisfied by editing
// instrumentation, and removing the answerRecovered call at the guarded-write site turns
// it red.
func TestCheckoutConsultsTheParkedTruthWhenItsGuardedWriteLoses(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "wiring-"+uuid.NewString(), 1, 2500)

	const name, email, token = "Racing Buyer", "racing@example.test", "pm_racing_instrument"
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s",
		f.reservation, name, strings.ToLower(email), token))))
	key := "checkout-wiring-" + uuid.NewString()

	if _, err := db.ExecContext(ctx,
		`UPDATE reservations SET status='finalizing' WHERE id=$1`, f.reservation); err != nil {
		t.Fatal(err)
	}
	// `created`, NOT release_pending: claimOrder must find a live order and let the
	// request proceed. If it were already parked the replay branch would answer first and
	// this test would prove nothing about the guarded-write path.
	if _, err := db.ExecContext(ctx, `
		UPDATE orders SET status='created', idempotency_key=$2, request_fingerprint=$3,
		    updated_at=now()
		WHERE id=$1`, f.order, key, fingerprint); err != nil {
		t.Fatal(err)
	}

	// Payments answers the charge, and in the same instant recovery wins the race: the
	// order becomes release_pending with the park marker set. This is the durable state
	// the handler will meet after its own guarded write matches nothing.
	payments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.ExecContext(r.Context(), `
			UPDATE orders SET status='release_pending', terminal_outcome='declined',
			    recovery_attempts=10, recovery_last_error='attempts exhausted',
			    recovery_parked_at=now()
			WHERE id=$1`, f.order); err != nil {
			t.Errorf("park the order mid-request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"captured"}`))
	}))
	defer payments.Close()

	// Inventory must ACCEPT the finalize and refuse only the confirm. Refusing both
	// answers 409 "hold expired" from a branch that sits BEFORE payments is ever called
	// (server.go:1666) -- the right status code from the wrong branch, which is how the
	// first run of this test passed its code assertion while proving nothing. Refusing
	// the confirm is what routes the handler into the guarded UPDATE whose loss is the
	// subject here.
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/finalize") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"finalized"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer inventory.Close()

	srv := newTestServer(db, http.DefaultClient, "", inventory.URL, payments.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)

	body := fmt.Sprintf(`{"reservation_id":%q,"name":%q,"email":%q,"payment_token":%q}`,
		f.reservation, name, email, token)
	req := httptest.NewRequest(http.MethodPost,
		"/reservations/"+f.reservation.String()+"/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Prove the race actually happened before judging the answer: if the stub never ran,
	// the row is still `created` and a 202 would be correct rather than a defect.
	var parked sql.NullTime
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status,recovery_parked_at FROM orders WHERE id=$1`, f.order).Scan(&status, &parked); err != nil {
		t.Fatal(err)
	}
	if status != "release_pending" || !parked.Valid {
		t.Fatalf("the race did not happen: status=%q parked=%v -- this test proves nothing",
			status, parked.Valid)
	}

	// THE BOUNDARY ASSERTION. A parked order tells every caller a human must act, and the
	// handler is a caller. 202 here would be the optimistic fallback answering for a row
	// no worker will ever advance.
	if rec.Code != http.StatusConflict {
		t.Fatalf("checkout answered %d for a parked order its guarded write missed; "+
			"the durable truth is release_pending+parked, which owes the buyer 409: %s",
			rec.Code, rec.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("response body is not JSON: %s", rec.Body.String())
	}
	if got := answer["error"]; got != "order awaiting payment reconciliation" {
		t.Fatalf("a parked order must tell the buyer a human must act, got %v: %s",
			got, rec.Body.String())
	}
}
