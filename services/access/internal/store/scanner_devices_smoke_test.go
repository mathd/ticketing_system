//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ai-review S1, at the tier the mechanism lives.
//
// The API tests drive the scan routes against a FAKE device port, which proves
// the handler consults it and refuses when it says no. It cannot prove the two
// things that actually protect a door, because both are SQL: that revocation is
// a WHERE clause rather than a field someone remembers to check, and that the
// stored form is a hash rather than the token. A fake enforces those in Go and
// stays green with the predicate deleted (AGENTS.md — "a test must live at the
// tier its mechanism does").
func TestScannerDeviceEnrolmentAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := New(db, Config{})
	organizer := uuid.New()

	device, token, err := st.EnrolScannerDevice(ctx, organizer, "north gate")
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if token == "" || device.ID == uuid.Nil {
		t.Fatal("enrolment returned no token or no device")
	}

	// The plaintext is not in the row. A database read must not yield a working
	// credential — the same property the password reset tokens have.
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT token_hash FROM scanner_devices WHERE id=$1`, device.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored device: %v", err)
	}
	if stored == token {
		t.Fatal("the enrolment token is stored in plaintext")
	}

	got, err := st.AuthenticateScannerDevice(ctx, token)
	if err != nil {
		t.Fatalf("a freshly enrolled device could not authenticate: %v", err)
	}
	if got.ID != device.ID || got.OrganizerID != organizer {
		t.Fatalf("authenticated as %+v, want device %s of organizer %s", got, device.ID, organizer)
	}

	// A token that is not enrolled, and one that differs by a byte.
	for _, bad := range []string{"", " ", "not-enrolled", token[:len(token)-1] + "0"} {
		if _, err := st.AuthenticateScannerDevice(ctx, bad); !errors.Is(err, ErrScannerDeviceUnknown) {
			t.Errorf("token %q authenticated (err=%v)", bad, err)
		}
	}

	// Surrounding whitespace survives a pairing field and an HTTP header, and a
	// correct token refused for that reason looks exactly like a revoked device.
	if _, err := st.AuthenticateScannerDevice(ctx, "  "+token+"\t"); err != nil {
		t.Errorf("a padded copy of a correct token was refused: %v", err)
	}

	// Revocation is what makes a lost phone answerable. It must take effect on
	// the AUTHENTICATION path, not merely set a column.
	if err := st.RevokeScannerDevice(ctx, device.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.AuthenticateScannerDevice(ctx, token); !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Fatalf("a revoked device still authenticates (err=%v) — the revocation is decoration", err)
	}

	// Idempotent, and it does not move the revocation time: an operator revoking
	// twice must not rewrite when the device stopped being trusted.
	var firstRevocation time.Time
	if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM scanner_devices WHERE id=$1`, device.ID).Scan(&firstRevocation); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	if err := st.RevokeScannerDevice(ctx, device.ID); err != nil {
		t.Errorf("revoking twice failed: %v", err)
	}
	var secondRevocation time.Time
	if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM scanner_devices WHERE id=$1`, device.ID).Scan(&secondRevocation); err != nil {
		t.Fatalf("re-read revoked_at: %v", err)
	}
	if !firstRevocation.Equal(secondRevocation) {
		t.Errorf("a second revocation moved revoked_at from %s to %s", firstRevocation, secondRevocation)
	}
	if err := st.RevokeScannerDevice(ctx, uuid.New()); !errors.Is(err, ErrScannerDeviceUnknown) {
		t.Errorf("revoking an unknown device reported %v, want ErrScannerDeviceUnknown", err)
	}

	// A second device of the same organizer is unaffected — revocation is per
	// device, which is the entire reason the credential is per device.
	second, secondToken, err := st.EnrolScannerDevice(ctx, organizer, "south gate")
	if err != nil {
		t.Fatalf("enrol second: %v", err)
	}
	if _, err := st.AuthenticateScannerDevice(ctx, secondToken); err != nil {
		t.Fatalf("revoking one gate disabled another: %v", err)
	}

	// The operator listing shows both, revoked included: "which one did we revoke"
	// is the question asked after a phone goes missing.
	devices, err := st.ListScannerDevices(ctx, organizer)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("listed %d devices, want 2", len(devices))
	}
	seen := map[uuid.UUID]bool{}
	for _, d := range devices {
		seen[d.ID] = true
		if d.ID == device.ID && d.RevokedAt == nil {
			t.Error("the revoked device is listed as live")
		}
		if d.ID == second.ID && d.RevokedAt != nil {
			t.Error("the live device is listed as revoked")
		}
	}
	if !seen[device.ID] || !seen[second.ID] {
		t.Errorf("listing missed a device: %+v", devices)
	}

	// Another organizer's listing is empty — the enrolment is scoped.
	if other, err := st.ListScannerDevices(ctx, uuid.New()); err != nil || len(other) != 0 {
		t.Errorf("another organizer sees %d devices (err=%v)", len(other), err)
	}

	// An empty or blank label is refused: a device nobody can identify is a device
	// nobody will revoke.
	if _, _, err := st.EnrolScannerDevice(ctx, organizer, "   "); err == nil {
		t.Error("a blank label was accepted")
	}
	if _, _, err := st.EnrolScannerDevice(ctx, uuid.Nil, "nowhere"); err == nil {
		t.Error("an enrolment with no organizer was accepted")
	}
}
