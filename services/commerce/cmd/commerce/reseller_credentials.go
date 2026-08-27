package main

// Operator provisioning for reseller credentials (TKT-240 / ADR-056).
//
// A CLI subcommand and not an HTTP surface, because partner self-service
// onboarding is an explicit non-goal of TKT-240 and because issuing a credential
// is a deliberate operator act. Whoever adds an onboarding surface later inherits
// the store functions unchanged; what they must not inherit is the assumption
// that this check is enough — see the registry note at the bottom.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	store "ticketing/services/commerce/internal/store"
)

// enrolReseller issues one credential and prints the plaintext token ONCE.
//
// Usage: commerce enrol-reseller <organizer-id> <reseller-id> <channel-code> <label>
//
// The token goes to STDOUT alone, with everything else on stderr, so an operator
// can pipe it into a secret store without also capturing the prose. It is not
// recoverable afterwards — the database holds only its hash — and re-running this
// issues a DIFFERENT credential rather than reprinting the old one.
func enrolReseller(args []string) error {
	if len(args) != 4 {
		return errors.New("usage: commerce enrol-reseller <organizer-id> <reseller-id> <channel-code> <label>")
	}
	organizer, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	reseller, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("reseller id: %w", err)
	}
	channel, label := args[2], args[3]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	cred, token, err := store.EnrolResellerCredential(ctx, db, organizer, reseller, channel, label)
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	fmt.Fprintf(os.Stderr, "credential %s issued for organizer %s, channel %q, reseller %s\n",
		cred.ID, cred.OrganizerID, cred.ChannelCode, cred.ResellerID)
	fmt.Fprintln(os.Stderr, "the token below is shown ONCE and is not recoverable:")
	fmt.Println(token)
	return nil
}

// revokeReseller retires one credential by id.
//
// Usage: commerce revoke-reseller <credential-id>
//
// Takes the credential id rather than the token: an operator revoking after a
// leak has the id from `enrol-reseller`'s output or from a list, and asking them
// to paste the leaked secret back in would be the wrong instinct to build.
func revokeReseller(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: commerce revoke-reseller <credential-id>")
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("credential id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := store.RevokeResellerCredential(ctx, db, id); err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	fmt.Fprintf(os.Stderr, "credential %s revoked; it is refused on the next request\n", id)
	return nil
}

// NOTE for whoever adds partner onboarding. This command does NOT validate that
// the channel exists in catalog's registry or that it is of reseller kind. That
// is a deliberate omission with a stated reason rather than an oversight: the
// registry lives in catalog's database, this binary holds only commerce's
// credentials, and a cross-service call here would put an availability
// dependency on an operator command that must work during an incident.
//
// What limits the damage is that the channel code is not authority by itself — a
// credential naming a channel with no allocation simply cannot sell, because
// inventory's allocation lookup refuses it. A typo produces a partner that gets
// 409s, not a partner that reaches someone else's stock.

// listResellers enumerates one organizer's credentials so an operator can find the
// id that `revoke-reseller` needs.
//
// Usage: commerce list-resellers <organizer-id>
//
// This closes the half of ADR-056's known gap that made revocation unreliable:
// `revoke-reseller` takes a credential id, and after the one-time token print there
// was no supported way to obtain one. Revoked rows are listed too — the question
// asked after a leak is "which credential did we revoke, and when", and a listing
// that hid them would answer a different one.
//
// Rows go to stdout and prose to stderr, following enrolReseller, so an operator can
// pipe the listing into a filter and feed an id straight to revoke-reseller.
//
// No STORED secret is emitted: the plaintext exists once, in enrolment's return value,
// and ListResellerCredentials selects neither it nor token_hash. That is the whole of
// the guarantee. `label` and `channel_code` are free text an operator chose at
// enrolment and are printed back verbatim; %q makes them safe to PARSE — it stops a
// newline or terminal escape from breaking the one-row-per-line contract — and redacts
// nothing. Credential metadata is non-secret by contract (ADR-056).
func listResellers(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: commerce list-resellers <organizer-id>")
	}
	organizer, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	creds, err := store.ListResellerCredentials(ctx, db, organizer)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(creds) == 0 {
		fmt.Fprintf(os.Stderr, "no reseller credentials for organizer %s\n", organizer)
		return nil
	}
	for _, c := range creds {
		fmt.Printf("id=%s reseller_id=%s organizer_id=%s channel_code=%q label=%q created_at=%s revoked_at=%s\n",
			c.ID, c.ResellerID, c.OrganizerID, c.ChannelCode, c.Label,
			c.CreatedAt.UTC().Format(time.RFC3339), revokedAtField(c.RevokedAt))
	}
	fmt.Fprintf(os.Stderr, "\n%d reseller credential(s) for organizer %s. A row with revoked_at=<none> still sells; "+
		"pass its id to `commerce revoke-reseller` to retire it.\n", len(creds), organizer)
	return nil
}

// revokedAtField renders "never revoked" distinguishably from a zero timestamp, for
// the reason nullableField gives in recovery_operations.go: an operator deciding
// whether a credential still sells must not have to guess what an empty field meant.
func revokedAtField(t *time.Time) string {
	if t == nil {
		return "<none>"
	}
	return t.UTC().Format(time.RFC3339)
}
