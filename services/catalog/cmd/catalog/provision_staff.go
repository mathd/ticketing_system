package main

// `catalog provision-staff` — the bootstrap path for back-office sign-in
// (TKT-190 / US-B1). Deliberately a CLI and not a seeded migration: seeding
// would put a working credential in the repository, which is precisely what
// TKT-83 removed when it deleted the checked-in INTERNAL_SERVICE_TOKEN default.

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// provisionStaffDeps is the injectable seam: the outer command owns Postgres and
// the process's real streams, the core owns argument parsing and the rule that
// the password only ever comes from stdin.
type provisionStaffDeps struct {
	create func(ctx context.Context, in store.StaffAccountInput) (store.StaffAccount, error)
	stdin  io.Reader
	stdout io.Writer
}

// provisionStaff creates exactly one staff account and prints its id.
//
// The password is read from stdin, never from a flag. A --password flag writes
// the credential into shell history, the process table and any audit log that
// captures argv — none of which can be un-written afterwards.
func provisionStaff(deps provisionStaffDeps, args []string) error {
	fs := flag.NewFlagSet("provision-staff", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage text goes through the returned error, not a side channel
	var (
		organizer  = fs.String("organizer-id", "", "organizer uuid the account administers")
		identifier = fs.String("identifier", "", "sign-in identifier (an email address)")
		role       = fs.String("role", "", "role name; stored but not yet enforced (TKT-191)")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("provision-staff: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("provision-staff: unexpected arguments %v", fs.Args())
	}
	organizerID, err := uuid.Parse(*organizer)
	if err != nil {
		return fmt.Errorf("provision-staff: --organizer-id must be a uuid: %w", err)
	}
	if strings.TrimSpace(*identifier) == "" {
		return fmt.Errorf("provision-staff: --identifier is required")
	}
	if strings.TrimSpace(*role) == "" {
		return fmt.Errorf("provision-staff: --role is required")
	}

	password, err := readPasswordLine(deps.stdin)
	if err != nil {
		return fmt.Errorf("provision-staff: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acct, err := deps.create(ctx, store.StaffAccountInput{
		OrganizerID: organizerID,
		Identifier:  *identifier,
		Role:        *role,
		Password:    password,
	})
	if err != nil {
		// Returned unwrapped-in-message: the caller prints it, and neither the
		// password nor the identifier appears in any store error.
		return err
	}
	// The id and nothing else. This line gets pasted into tickets.
	_, err = fmt.Fprintf(deps.stdout, "%s\n", acct.ID)
	return err
}

// readPasswordLine takes the first line of stdin as the password. Trailing \n
// and \r are stripped so a heredoc and a `printf` behave the same; nothing else
// is trimmed, because leading and trailing spaces are legitimate password
// characters and silently eating them would make the account unopenable.
func readPasswordLine(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no stdin to read the password from")
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", fmt.Errorf("empty password on stdin: pipe the password in, e.g. `printf '%%s' \"$PW\" | catalog provision-staff …`")
	}
	return password, nil
}

// provisionStaffCommand wires the real Postgres store. Migrations are NOT run
// here — ADR-022 keeps them out-of-band in the catalog-migrate job, and a CLI
// that quietly migrated would be a second migration path.
func provisionStaffCommand(args []string) error {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	return provisionStaff(provisionStaffDeps{
		create: store.NewPostgres(db).CreateStaffAccount,
		stdin:  os.Stdin,
		stdout: os.Stdout,
	}, args)
}
