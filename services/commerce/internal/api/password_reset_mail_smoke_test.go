//go:build smoke

package api

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/mailer"
	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/mail"
)

// The French reset message, from the request to the sender, through real Postgres
// (TKT-366).
//
// The handler tests prove which copy is chosen: they run production composition through
// the store seam and hand the result straight to the Fake. The drainer smoke tests prove a
// row reaches the sender unchanged, with ASCII text. None of them carries a composed,
// non-ASCII message through the real store, mail_outbox and the drainer. This one does:
// the real router (contract in front), the real store writing the outbox row, the real
// drainer, and the offline Fake.
//
// The drainer claims every claimable row in the database, not only this test's, so the
// assertion filters the Fake by recipient.

var smokeResetToken = regexp.MustCompile(`\?token=([A-Za-z0-9_-]+)\n`)

func TestAFrenchResetMessageReachesTheSenderByteForByte(t *testing.T) {
	db, ctx := exchangeAPIDB(t)

	email := "reset-fr-" + uuid.NewString()[:8] + "@example.test"
	if _, err := commercestore.RegisterCustomer(ctx, db, email, "correct horse battery"); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	s := (&Server{db: db}).WithPublicURL(resetTestOrigin)
	rec := routerPost(resetRouter(t, s), `{"email":"`+email+`","locale":"fr"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	fake := mail.NewFake()
	d := mailer.New(db, fake, time.Second, 32, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.DrainOnce(ctx)

	var got []mail.Message
	for _, m := range fake.Sent() {
		if m.To == email {
			got = append(got, m)
		}
	}
	if len(got) != 1 {
		t.Fatalf("messages handed to the sender for %s = %d, want 1", email, len(got))
	}
	m := got[0]
	if m.Subject != wantSubjectFR {
		t.Fatalf("subject = %q, want %q", m.Subject, wantSubjectFR)
	}
	match := smokeResetToken.FindStringSubmatch(m.Body)
	if match == nil {
		t.Fatalf("no reset link in the delivered body:\n%s", m.Body)
	}
	token := match[1]
	if want := wantBodyFR(resetTestOrigin + "/fr/account/reset-password?token=" + token); m.Body != want {
		t.Fatalf("the delivered body is not the French message byte for byte:\n%q\nwant:\n%q", m.Body, want)
	}
	if !strings.Contains(m.Body, "Quelqu’un") || !strings.Contains(m.Body, "à l’origine") {
		t.Fatalf("non-ASCII text did not survive the outbox:\n%q", m.Body)
	}
	// And the link in the delivered message is the live one: its token redeems.
	if _, err := commercestore.CompletePasswordReset(ctx, db, token, "a brand new password"); err != nil {
		t.Fatalf("the delivered token does not redeem: %v", err)
	}
}
