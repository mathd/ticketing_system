package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/mail"
)

// The reset message in the language of the page that asked for it (TKT-366, ADR-050
// § TKT-366 amendment).
//
// The expected messages below are written out IN FULL, as literals, and never built from
// the production copy. A test that formats the production strings to get its expected
// value agrees with whatever the code says, including a swapped subject or a wrong path.

const resetTestOrigin = "https://tickets.example.test"

const wantSubjectEN = "Reset your password"

func wantBodyEN(link string) string {
	return "Someone asked to reset the password for this address.\n" +
		"\n" +
		"Open this link to choose a new one:\n" +
		"\n" +
		link + "\n" +
		"\n" +
		"The link works once and expires in one hour. If you did not ask for this, nothing has\n" +
		"changed and you can ignore this message.\n"
}

const wantSubjectFR = "Réinitialisez votre mot de passe"

func wantBodyFR(link string) string {
	return "Quelqu’un a demandé la réinitialisation du mot de passe associé à cette adresse.\n" +
		"\n" +
		"Ouvrez ce lien pour en choisir un nouveau :\n" +
		"\n" +
		link + "\n" +
		"\n" +
		"Ce lien ne fonctionne qu’une fois et expire dans une heure. Si vous n’êtes pas\n" +
		"à l’origine de cette demande, rien n’a changé et vous pouvez ignorer ce message.\n"
}

// resetToFake swaps the store seam for one that does what the store and the drainer do
// together: it resolves the address, composes the message through the handler's compose
// callback, and hands the result to an offline mail.Fake. The Fake runs Message.Validate,
// so a composed message that no sender would accept fails here too.
//
// It returns the Fake and a pointer to the number of store calls, so a test can prove a
// refusal never reached the account lookup.
func resetToFake(t *testing.T, known func(email string) bool) (*mail.Fake, *int) {
	t.Helper()
	fake := mail.NewFake()
	calls := 0
	swapResetStore(t, func(ctx context.Context, email string, compose func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		calls++
		if !known(email) {
			return false, nil
		}
		to, subject, body := compose(commercestore.IssuedResetToken{Raw: "the-token", Email: email})
		if err := fake.Send(ctx, mail.Message{To: to, Subject: subject, Body: body}); err != nil {
			t.Errorf("the composed message is not deliverable: %v", err)
		}
		return true, nil
	}, nil)
	return fake, &calls
}

func everyAddressKnown(string) bool { return true }

func onlySent(t *testing.T, fake *mail.Fake) mail.Message {
	t.Helper()
	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages handed to the sender = %d, want exactly 1: %+v", len(sent), sent)
	}
	return sent[0]
}

func TestAFrenchResetRequestMailsTheFrenchMessage(t *testing.T) {
	s := (&Server{}).WithPublicURL(resetTestOrigin)
	fake, _ := resetToFake(t, everyAddressKnown)

	rec := postTo(t, s.requestPasswordReset, "/customers/password-reset",
		`{"email":"buyer@example.test","locale":"fr"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	m := onlySent(t, fake)
	if m.To != "buyer@example.test" {
		t.Fatalf("recipient = %q", m.To)
	}
	if m.Subject != wantSubjectFR {
		t.Fatalf("subject = %q, want %q", m.Subject, wantSubjectFR)
	}
	if want := wantBodyFR(resetTestOrigin + "/fr/account/reset-password?token=the-token"); m.Body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", m.Body, want)
	}
}

func TestAnEnglishResetRequestKeepsTheEnglishMessage(t *testing.T) {
	s := (&Server{}).WithPublicURL(resetTestOrigin)
	fake, _ := resetToFake(t, everyAddressKnown)

	rec := postTo(t, s.requestPasswordReset, "/customers/password-reset",
		`{"email":"buyer@example.test","locale":"en"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	m := onlySent(t, fake)
	if m.Subject != wantSubjectEN {
		t.Fatalf("subject = %q, want %q", m.Subject, wantSubjectEN)
	}
	if want := wantBodyEN(resetTestOrigin + "/en/account/reset-password?token=the-token"); m.Body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", m.Body, want)
	}
}

// Absent means English. A caller built before TKT-366 sends no locale, and during a
// rolling deploy that caller is the storefront still running the old build. It must keep
// getting the message it got before, not a refusal (D1; the critique ran both orders).
func TestAResetRequestWithNoLocaleIsMailedInEnglish(t *testing.T) {
	s := (&Server{}).WithPublicURL(resetTestOrigin)
	fake, _ := resetToFake(t, everyAddressKnown)

	rec := postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"buyer@example.test"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	m := onlySent(t, fake)
	if m.Subject != wantSubjectEN {
		t.Fatalf("subject = %q, want %q", m.Subject, wantSubjectEN)
	}
	if want := wantBodyEN(resetTestOrigin + "/en/account/reset-password?token=the-token"); m.Body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", m.Body, want)
	}
}

// The locale picks a language and a fixed path. It must not open a way to pick the host:
// every locale builds on the configured origin, whatever Host the request carries.
func TestTheResetLinkUsesTheConfiguredOriginInEveryLocale(t *testing.T) {
	for _, locale := range []string{"en", "fr"} {
		t.Run(locale, func(t *testing.T) {
			s := (&Server{}).WithPublicURL(resetTestOrigin + "/")
			fake, _ := resetToFake(t, everyAddressKnown)

			req := httptest.NewRequest(http.MethodPost, "/customers/password-reset",
				strings.NewReader(`{"email":"buyer@example.test","locale":"`+locale+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Host = "attacker.example.test"
			req.Header.Set("X-Forwarded-Host", "attacker.example.test")
			req.Header.Set("Origin", "https://attacker.example.test")
			s.requestPasswordReset(httptest.NewRecorder(), req)

			body := onlySent(t, fake).Body
			want := "\n" + resetTestOrigin + "/" + locale + "/account/reset-password?token=the-token\n"
			if !strings.Contains(body, want) {
				t.Fatalf("the link is not the configured origin plus the fixed %s path:\n%s", locale, body)
			}
			if strings.Contains(body, "attacker") {
				t.Fatalf("the request host reached the mailed link:\n%s", body)
			}
		})
	}
}

// The HANDLER's own check, without the contract in front of it. The contract refuses the
// same values on the real router (below), but a handler that trusts the validator is one
// refactor away from mailing whatever a caller typed into a path.
func TestTheHandlerRefusesAnUnsupportedLocaleWithoutReachingTheStore(t *testing.T) {
	for _, value := range []string{`""`, `"de"`, `"FR"`, `"fr\r\nBcc: x@example.test"`, `"../fr"`, `"fr/"`, `" fr"`, `"en-CA"`} {
		t.Run(value, func(t *testing.T) {
			s := (&Server{}).WithPublicURL(resetTestOrigin)
			fake, calls := resetToFake(t, everyAddressKnown)

			rec := postTo(t, s.requestPasswordReset, "/customers/password-reset",
				`{"email":"buyer@example.test","locale":`+value+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if *calls != 0 {
				t.Fatal("an unsupported locale reached the account lookup")
			}
			if len(fake.Sent()) != 0 {
				t.Fatal("an unsupported locale produced a message")
			}
		})
	}
}

// The check runs BEFORE the recovery budget. A refused locale enqueues nothing, so it has
// no mail cost to bound, and charging it would let a caller spend a victim's budget with
// requests that could never send them anything. It also keeps the handler in step with
// the contract tier, where the validator refuses before any limiter runs.
func TestARefusedLocaleDoesNotSpendTheRecoveryBudget(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)
	resetToFake(t, everyAddressKnown)

	for range customerSubjectBurst + 5 {
		if rec := postAs(s.requestPasswordReset, "203.0.113.20", "/customers/password-reset",
			`{"email":"victim@example.test","locale":"de"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("an unsupported locale answered %d, want 400 — it is being charged to the budget", rec.Code)
		}
	}
	rec := postAs(s.requestPasswordReset, "203.0.113.20", "/customers/password-reset",
		`{"email":"victim@example.test","locale":"fr"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a valid request after refused ones answered %d, want 202 — refusals spent the budget", rec.Code)
	}
}

// One address has ONE recovery budget, whatever language it asks in. A budget keyed by
// locale would let a caller mail-bomb an address twice as hard by alternating.
func TestOneAddressHasOneRecoveryBudgetAcrossLocales(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)
	resetToFake(t, everyAddressKnown)

	for i := range customerSubjectBurst {
		locale := []string{"en", "fr"}[i%2]
		if rec := postAs(s.requestPasswordReset, "203.0.113.21", "/customers/password-reset",
			`{"email":"bombed@example.test","locale":"`+locale+`"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d (%s) refused inside the budget: %d", i+1, locale, rec.Code)
		}
	}
	for _, locale := range []string{"en", "fr"} {
		if rec := postAs(s.requestPasswordReset, "203.0.113.21", "/customers/password-reset",
			`{"email":"bombed@example.test","locale":"`+locale+`"}`); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s after the shared budget: status = %d, want 429 — the budget is split by locale", locale, rec.Code)
		}
	}
}

func resetRouter(t *testing.T, s *Server) http.Handler {
	t.Helper()
	return s.Router(slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), true)
}

func routerPost(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/customers/password-reset", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.22:51000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The real router: the contract is in front of the handler, so this is where `null`,
// a wrong type and an array are refused. Go decodes `null` into an absent pointer, so the
// handler alone cannot tell `null` from no field at all — that refusal is decided HERE,
// and asserted here.
func TestTheContractDecidesWhichLocalesReachTheHandler(t *testing.T) {
	s := (&Server{}).WithPublicURL(resetTestOrigin)
	fake, calls := resetToFake(t, everyAddressKnown)
	h := resetRouter(t, s)

	accepted := []struct{ name, body, path string }{
		{"absent", `{"email":"buyer@example.test"}`, "/en/"},
		{"en", `{"email":"buyer@example.test","locale":"en"}`, "/en/"},
		{"fr", `{"email":"buyer@example.test","locale":"fr"}`, "/fr/"},
	}
	for _, tc := range accepted {
		before := len(fake.Sent())
		rec := routerPost(h, tc.body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202: %s", tc.name, rec.Code, rec.Body.String())
		}
		sent := fake.Sent()
		if len(sent) != before+1 || !strings.Contains(sent[len(sent)-1].Body, resetTestOrigin+tc.path+"account/reset-password?token=") {
			t.Fatalf("%s: no message with the %s link was handed to the sender", tc.name, tc.path)
		}
	}

	for _, value := range []string{`"de"`, `""`, `null`, `1`, `true`, `"FR"`, `"fr\r\nBcc: x@example.test"`, `"../fr"`, `["fr"]`, `{"fr":true}`} {
		before := *calls
		rec := routerPost(h, `{"email":"buyer@example.test","locale":`+value+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("locale %s: status = %d, want 400: %s", value, rec.Code, rec.Body.String())
		}
		if *calls != before {
			t.Fatalf("locale %s reached the account lookup", value)
		}
	}
}

// Whether the address holds an account must not change the answer — for a refused locale
// as much as for an accepted one. The SAME address string is used for both halves and the
// seam decides whether it exists: the contract's 400 body repeats the request, so two
// different addresses would differ in bytes for a reason that discloses nothing.
func TestALocaleAnswersTheSameWhetherOrNotTheAddressHasAnAccount(t *testing.T) {
	for _, body := range []string{
		`{"email":"probe@example.test","locale":"fr"}`,
		`{"email":"probe@example.test","locale":"en"}`,
		`{"email":"probe@example.test","locale":"de"}`,
		`{"email":"probe@example.test","locale":""}`,
	} {
		answer := func(known bool) *httptest.ResponseRecorder {
			s := (&Server{}).WithPublicURL(resetTestOrigin)
			resetToFake(t, func(string) bool { return known })
			return routerPost(resetRouter(t, s), body)
		}
		k, u := answer(true), answer(false)
		if k.Code != u.Code {
			t.Fatalf("%s: status differs: known=%d unknown=%d", body, k.Code, u.Code)
		}
		if !bytes.Equal(k.Body.Bytes(), u.Body.Bytes()) {
			t.Fatalf("%s: bodies differ — an account-existence oracle:\n known:   %s\n unknown: %s", body, k.Body, u.Body)
		}
		if k.Header().Get("Content-Type") != u.Header().Get("Content-Type") {
			t.Fatalf("%s: Content-Type differs", body)
		}
	}
}

// The copy states the lifetime in words ("one hour", "une heure"), because a Go duration
// string such as 1h0m0s is not something a buyer should read. Words do not follow the
// constant, so the message selects its phrase BY ResetTokenTTL, and this test requires
// every locale to have one: change ResetTokenTTL and this fails until a phrase for the
// new value is written.
func TestTheResetCopyStatesTheTokenLifetime(t *testing.T) {
	for locale, c := range resetCopies {
		if c.lifetime[commercestore.ResetTokenTTL] == "" {
			t.Errorf("locale %s has no lifetime phrase for ResetTokenTTL %s.\n"+
				"Add one to its resetCopies entry in password_reset.go, AND update the storefront's\n"+
				"resetRequested strings in web/storefront/src/lib/locales.ts (en: \"expires in an hour\",\n"+
				"fr: \"expire dans une heure\").", locale, commercestore.ResetTokenTTL)
		}
	}
}
