//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TKT-290 COS5: at the cap, the REAL CLI refuses, prints no token, and names the remedy.
// The cap has no override, so an operator who hits it mid-incident must be told exactly
// which command shows what to revoke. Run through the deployed binary, the path an
// operator takes, with a fresh reseller so nothing else in the suite shares its count.
func TestEnrolResellerAtCapNamesTheRemedy(t *testing.T) {
	organizer, reseller := "00000000-0000-0000-0000-000000000001", uuid.NewString()
	enrol := func(channel string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("docker", "exec", project+"-commerce-1", "/app", "enrol-reseller",
			organizer, reseller, channel, "cap smoke")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run enrol-reseller: %v", err)
		}
		return code, stdout.String(), stderr.String()
	}
	for _, channel := range []string{"reseller-cap-1", "reseller-cap-2"} {
		if code, out, errOut := enrol(channel); code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("enrolment %s: exit %d, stdout %q, stderr %q", channel, code, out, errOut)
		}
	}
	code, out, errOut := enrol("reseller-cap-3")
	if code == 0 {
		t.Fatal("a third live enrolment succeeded through the CLI")
	}
	if strings.TrimSpace(out) != "" {
		t.Fatal("a refused enrolment printed something on stdout (where the token goes)")
	}
	for _, want := range []string{"cap", "revoke one first", "list-resellers"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not name %q: %s", want, errOut)
		}
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	var live int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM reseller_credentials
		WHERE organizer_id=$1 AND reseller_id=$2 AND revoked_at IS NULL`, organizer, reseller).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 2 {
		t.Fatalf("live credentials after the refusal = %d, want 2", live)
	}
}
