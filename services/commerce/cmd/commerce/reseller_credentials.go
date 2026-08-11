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
