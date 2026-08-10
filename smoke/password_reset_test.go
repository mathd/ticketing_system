//go:build smoke

package smoke_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Password recovery through the live stack (TKT-226 / ADR-050).
//
// Scoped like customer_accounts_test.go: only what a unit test structurally cannot
// reach. The token logic, the enumeration parity of the HANDLER, and the drainer
// protocol are all proven closer to the code. What needs a real stack is:
//
//  1. The two operations are reachable through the gateway with NO credential and are
//     not under the edge-denied /internal/ namespace.
//  2. The whole loop actually closes: a request enqueues a real message, the running
//     commerce container's mail drainer picks it up and its OFFLINE FAKE accepts it
//     with no network, and the token in that message resets the password for real.
//
// (2) is the acceptance criterion in one test. It also proves the drainer is WIRED —
// a drainer that was never started leaves the row unsent forever and every unit test
// stays green.

// resetLinkToken pulls the token out of a composed message body. The pattern is the
// link commerce builds; a change to the copy that dropped the link fails here.
var resetLinkToken = regexp.MustCompile(`[?&]token=([A-Za-z0-9_-]+)`)

// awaitEnqueuedResetMail polls commerce's mail_outbox for the message addressed to
// `recipient` and returns its token and whether the drainer retired the row.
//
// Polling rather than a single read: the row is committed by the request, but `sent_at`
// is set by a background worker on its own interval.
func awaitEnqueuedResetMail(t *testing.T, recipient string) (token string, sent bool) {
	t.Helper()
	ctx := t.Context()
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://commerce:commerce@%s/commerce", pgHostPort))
	if err != nil {
		t.Fatalf("connect to commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var body string
		var sentAt *time.Time
		err := db.QueryRow(ctx,
			`SELECT body, sent_at FROM mail_outbox WHERE recipient=$1 ORDER BY created_at DESC LIMIT 1`,
			recipient).Scan(&body, &sentAt)
		if err == nil {
			if m := resetLinkToken.FindStringSubmatch(body); m != nil {
				token = m[1]
			}
			if token != "" && sentAt != nil {
				return token, true
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read mail_outbox: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return token, false
}

func TestPasswordResetPathsAreReachableWithoutACredentialAndNotEdgeDenied(t *testing.T) {
	for _, path := range []string{
		"/api/commerce/customers/password-reset",
		"/api/commerce/customers/password-reset/complete",
	} {
		// A deliberately invalid body: the point is WHICH layer refuses. The gateway's
		// edge-deny answers 404 before commerce sees anything; commerce's own validator
		// answers 400.
		status, body := customerPost(t, path, map[string]string{})
		if status == http.StatusNotFound {
			t.Fatalf("%s was refused at the gateway edge — it has drifted under /internal/: %s", path, body)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 from commerce's own validator: %s", path, status, body)
		}
	}
}

// A known and an unknown address must be indistinguishable ACROSS THE REAL STACK, not
// merely in the handler: the gateway, the contract response validator and the JSON
// encoder all sit between the handler and the buyer, and any of them could introduce a
// difference the handler test cannot see.
func TestAKnownAndAnUnknownAddressGetTheSameAnswerThroughTheGateway(t *testing.T) {
	known := "smoke-reset-" + uuid.NewString() + "@example.test"
	if status, body := customerPost(t, "/api/commerce/customers", map[string]string{
		"email": known, "password": "correct horse battery",
	}); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	unknown := "smoke-nobody-" + uuid.NewString() + "@example.test"

	knownStatus, knownBody := customerPost(t, "/api/commerce/customers/password-reset",
		map[string]string{"email": known})
	unknownStatus, unknownBody := customerPost(t, "/api/commerce/customers/password-reset",
		map[string]string{"email": unknown})

	if knownStatus != http.StatusAccepted || unknownStatus != http.StatusAccepted {
		t.Fatalf("statuses differ or are not 202: known=%d unknown=%d", knownStatus, unknownStatus)
	}
	if string(knownBody) != string(unknownBody) {
		t.Fatalf("bodies differ — an account-existence oracle survives to the edge:\n known:   %s\n unknown: %s",
			knownBody, unknownBody)
	}
}

// The acceptance criterion, end to end, against the running stack.
func TestAForgottenPasswordCanBeRecovered(t *testing.T) {
	email := "smoke-recover-" + uuid.NewString() + "@example.test"
	const forgotten = "the forgotten one"
	const chosen = "the remembered one"

	if status, body := customerPost(t, "/api/commerce/customers", map[string]string{
		"email": email, "password": forgotten,
	}); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}

	if status, body := customerPost(t, "/api/commerce/customers/password-reset",
		map[string]string{"email": email}); status != http.StatusAccepted {
		t.Fatalf("request reset: %d %s", status, body)
	}

	// The row is enqueued by the request and retired by the drainer's offline fake.
	// `sent` false means the drainer never ran — which no unit test can detect.
	token, sent := awaitEnqueuedResetMail(t, email)
	if token == "" {
		t.Fatal("no reset message was enqueued for a registered address")
	}
	if !sent {
		t.Fatal("the message was never retired — the mail drainer is not running, or its sender refused")
	}

	status, body := customerPost(t, "/api/commerce/customers/password-reset/complete",
		map[string]string{"token": token, "password": chosen})
	if status != http.StatusOK {
		t.Fatalf("complete: %d %s", status, body)
	}
	var completed struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(body, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.CustomerID == "" {
		t.Fatalf("completion must return the customer whose sessions to destroy: %s", body)
	}

	// The buyer is back in.
	if status, body := customerPost(t, "/api/commerce/customers/authenticate", map[string]string{
		"email": email, "password": chosen,
	}); status != http.StatusOK {
		t.Fatalf("the new password must sign in: %d %s", status, body)
	}
	// And the old one is gone.
	if status, _ := customerPost(t, "/api/commerce/customers/authenticate", map[string]string{
		"email": email, "password": forgotten,
	}); status != http.StatusUnauthorized {
		t.Fatalf("the forgotten password must stop working, got %d", status)
	}
	// Single-use, across the real stack.
	if status, _ := customerPost(t, "/api/commerce/customers/password-reset/complete",
		map[string]string{"token": token, "password": "a third password"}); status != http.StatusBadRequest {
		t.Fatalf("a redeemed token must be refused, got %d", status)
	}
}
