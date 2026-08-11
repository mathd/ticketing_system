package main

// Scanner device enrolment, the operator surface (ai-review S1).
//
// A CLI rather than an HTTP endpoint, and that is a scope decision worth stating.
// An enrolment endpoint needs an authenticated OPERATOR, and access has no staff
// identity — catalog owns staff (ADR-042) and the back office deliberately holds
// no service credential (TKT-191). Adding an enrolment API here would therefore
// mean either inventing a second staff story in access or handing the
// internet-facing back office the shared token, and neither belongs in this
// change. A CLI run by whoever already has access to the deployment needs no new
// trust at all, and pairing a physical gate device is an in-person act anyway:
// the token is read off a terminal and typed into the phone at the door.
//
// The HTTP enrolment endpoint (and the back-office screen in front of it) is the
// next increment, and it wants the staff-credential question answered first.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	accessstore "ticketing/services/access/internal/store"
)

// devicesStore builds a store with no signer and no keyring. Enrolment writes no
// lifecycle events, so it needs neither — and a CLI that cannot sign the trail
// cannot damage it, which is the same reason `verify-lifecycle` holds public keys
// only (ADR-021 §D4).
func devicesStore() (*accessstore.Postgres, func(), error) {
	db, err := openDB()
	if err != nil {
		return nil, nil, err
	}
	return accessstore.New(db, accessstore.Config{}), func() { _ = db.Close() }, nil
}

// enrolScanner registers one gate device and prints its token ONCE.
//
// Printed rather than stored anywhere retrievable: the token is hashed at rest
// (see migration 0009), so this output is the only time the plaintext exists.
// An operator who loses it revokes the device and enrols a new one, which is the
// correct workflow anyway — a credential that can be re-read is a credential a
// database read yields.
func enrolScanner(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: access enrol-scanner <organizer-id> <label>")
	}
	organizerID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, closeDB, err := devicesStore()
	if err != nil {
		return err
	}
	defer closeDB()

	device, token, err := st.EnrolScannerDevice(ctx, organizerID, args[1])
	if err != nil {
		return err
	}
	fmt.Printf("access enrol-scanner: device %s (%q) enrolled for organizer %s\n", device.ID, device.Label, device.OrganizerID)
	fmt.Printf("  X-Scanner-Token: %s\n", token)
	fmt.Println("  This is the only time this token is shown. Pair the device now; revoke and re-enrol if it is lost.")
	return nil
}

// revokeScanner retires one device. The answer to a phone leaving the venue.
func revokeScanner(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: access revoke-scanner <device-id>")
	}
	deviceID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("device id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, closeDB, err := devicesStore()
	if err != nil {
		return err
	}
	defer closeDB()

	if err := st.RevokeScannerDevice(ctx, deviceID); err != nil {
		return err
	}
	fmt.Printf("access revoke-scanner: device %s can no longer scan\n", deviceID)
	return nil
}

// listScanners shows one organizer's gates, revoked rows included: the question
// after a phone goes missing is "which one did we revoke, and when".
func listScanners(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: access list-scanners <organizer-id>")
	}
	organizerID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, closeDB, err := devicesStore()
	if err != nil {
		return err
	}
	defer closeDB()

	devices, err := st.ListScannerDevices(ctx, organizerID)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Printf("access list-scanners: organizer %s has no enrolled devices\n", organizerID)
		return nil
	}
	for _, d := range devices {
		status := "live"
		if d.RevokedAt != nil {
			status = "revoked " + d.RevokedAt.UTC().Format(time.RFC3339)
		}
		seen := "never seen"
		if d.LastSeenAt != nil {
			seen = "last seen " + d.LastSeenAt.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%s  %-24q  %s  %s\n", d.ID, d.Label, status, seen)
	}
	return nil
}
