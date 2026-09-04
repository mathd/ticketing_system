package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// The password is read from stdin and nowhere else. An --password flag would put
// the credential in the shell history, the process table and any command log
// that captures argv — three places it can never be removed from afterwards.
func TestProvisionStaffReadsThePasswordFromStdinOnly(t *testing.T) {
	p := newFakeProvisioner()
	var out bytes.Buffer
	err := provisionStaff(provisionStaffDeps{
		create: p.create,
		stdin:  strings.NewReader("hunter2\n"),
		stdout: &out,
	}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", "admin"})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if p.last.Password != "hunter2" {
		t.Fatalf("password = %q, want the stdin line with its newline stripped", p.last.Password)
	}
}

// Everything the command writes is read by an operator and often pasted into a
// ticket. It prints the pseudonymous id and nothing that identifies the human.
func TestProvisionStaffPrintsOnlyThePseudonymousID(t *testing.T) {
	p := newFakeProvisioner()
	var out bytes.Buffer
	if err := provisionStaff(provisionStaffDeps{
		create: p.create, stdin: strings.NewReader("hunter2\n"), stdout: &out,
	}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", "admin"}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	printed := out.String()
	if !strings.Contains(printed, p.created.ID.String()) {
		t.Fatalf("output must name the new staff id, got %q", printed)
	}
	for _, secret := range []string{"hunter2", "ada@example.test"} {
		if strings.Contains(printed, secret) {
			t.Fatalf("output leaks %q: %q", secret, printed)
		}
	}
}

// Create-only. A typo'd identifier that happens to match a live account must not
// quietly reset that account's password — the operator would have locked someone
// out and been told it worked.
func TestProvisionStaffRefusesToOverwriteAnExistingAccount(t *testing.T) {
	p := newFakeProvisioner()
	p.err = store.ErrStaffIdentifierTaken
	var out bytes.Buffer
	err := provisionStaff(provisionStaffDeps{
		create: p.create, stdin: strings.NewReader("hunter2\n"), stdout: &out,
	}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", "admin"})
	if !errors.Is(err, store.ErrStaffIdentifierTaken) {
		t.Fatalf("want ErrStaffIdentifierTaken, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused provision must print nothing, got %q", out.String())
	}
}

// An empty stdin is an operator who piped the wrong thing, not a request for an
// empty password.
func TestProvisionStaffRefusesAnEmptyPassword(t *testing.T) {
	p := newFakeProvisioner()
	err := provisionStaff(provisionStaffDeps{
		create: p.create, stdin: strings.NewReader(""), stdout: &bytes.Buffer{},
	}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", "admin"})
	if err == nil {
		t.Fatal("an empty password must be a usage error")
	}
	if p.calls != 0 {
		t.Fatal("nothing should have been written")
	}
}

func TestProvisionStaffRequiresEveryFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--identifier", "ada@example.test", "--role", "admin"},
		{"--organizer-id", orgIDForTest, "--role", "admin"},
		{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test"},
		{"--organizer-id", "not-a-uuid", "--identifier", "ada@example.test", "--role", "admin"},
	} {
		p := newFakeProvisioner()
		if err := provisionStaff(provisionStaffDeps{
			create: p.create, stdin: strings.NewReader("hunter2\n"), stdout: &bytes.Buffer{},
		}, args); err == nil {
			t.Fatalf("args %v must be a usage error", args)
		}
	}
}

const orgIDForTest = "00000000-0000-0000-0000-000000000001"

type fakeProvisioner struct {
	last    store.StaffAccountInput
	created store.StaffAccount
	err     error
	calls   int
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{created: store.StaffAccount{ID: uuid.New()}}
}

func (f *fakeProvisioner) create(_ context.Context, in store.StaffAccountInput) (store.StaffAccount, error) {
	f.calls++
	f.last = in
	if f.err != nil {
		return store.StaffAccount{}, f.err
	}
	return f.created, nil
}

// TKT-197. The role is validated here, against the same generated constants the
// service validates against — not a second vocabulary.
//
// Without this the store still fails closed: an account with `adminn` cannot
// sign in, because catalog refuses an unrecognised stored role. But it refuses
// with a deliberately generic 500 that points at nothing, later, probably to a
// different person. A typo is only cheap to fix while the operator is still
// looking at it.
func TestProvisionStaffRefusesARoleOutsideTheVocabulary(t *testing.T) {
	for _, role := range []string{"adminn", "Admin", "superuser", "box-office", "ADMIN"} {
		t.Run(role, func(t *testing.T) {
			p := newFakeProvisioner()
			err := provisionStaff(provisionStaffDeps{
				create: p.create, stdin: strings.NewReader("hunter2\n"), stdout: &bytes.Buffer{},
			}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", role})
			if err == nil {
				t.Fatalf("accepted %q as a staff role", role)
			}
			// The message must list the real vocabulary, or the operator is left
			// guessing at what they should have typed.
			for _, want := range []string{"admin", "box_office", "finance"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not name %q as an option: %v", want, err)
				}
			}
			if p.calls != 0 {
				t.Fatal("a rejected role must not reach the store")
			}
		})
	}
}

func TestProvisionStaffAcceptsEveryContractRole(t *testing.T) {
	for _, role := range []string{"admin", "box_office", "finance"} {
		t.Run(role, func(t *testing.T) {
			p := newFakeProvisioner()
			if err := provisionStaff(provisionStaffDeps{
				create: p.create, stdin: strings.NewReader("hunter2\n"), stdout: &bytes.Buffer{},
			}, []string{"--organizer-id", orgIDForTest, "--identifier", "ada@example.test", "--role", role}); err != nil {
				t.Fatalf("rejected the contract role %q: %v", role, err)
			}
			if p.last.Role != role {
				t.Fatalf("stored role = %q, want %q", p.last.Role, role)
			}
		})
	}
}
