//go:build smoke

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestVerifierBranchAssignsItsCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	tests := []struct {
		name   string
		want   AlarmReason
		mutate func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID)
	}{
		{
			name: "missing integrity row", want: AlarmReasonMissingIntegrityRow,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_event_integrity WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsupported integrity canonical version", want: AlarmReasonUnsupportedCanonicalVersion,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET canonical_version=99 WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "sequence gap", want: AlarmReasonSequenceGap,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET sequence=2 WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "broken chain link", want: AlarmReasonBrokenChainLink,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET previous_hash=decode(repeat('11',32),'hex') WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "entry hash mismatch", want: AlarmReasonEntryHashMismatch,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_event_integrity SET entry_hash=decode(repeat('00',32),'hex') WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "orphan integrity row", want: AlarmReasonOrphanIntegrityRow,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_event_integrity DROP CONSTRAINT lifecycle_event_integrity_event_id_fkey`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_event_integrity(event_id,ticket_id,sequence,canonical_version,previous_hash,entry_hash) VALUES($1,$2,2,1,decode(repeat('00',32),'hex'),decode(repeat('00',32),'hex'))`, uuid.New(), ticketID); err != nil {
					t.Fatal(err)
				}
				// Put the schema back once the subtest has VERIFIED, not here: removing the
				// orphan before verification would leave nothing for the verifier to find.
				// Cleanups run after the subtest's deferred trigger re-enable, so the delete
				// disables the append-only trigger again around itself.
				t.Cleanup(func() {
					for _, stmt := range []string{
						`ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER`,
						`DELETE FROM lifecycle_event_integrity WHERE NOT EXISTS (SELECT 1 FROM lifecycle_events WHERE lifecycle_events.id=lifecycle_event_integrity.event_id)`,
						`ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER`,
						`ALTER TABLE lifecycle_event_integrity ADD CONSTRAINT lifecycle_event_integrity_event_id_fkey FOREIGN KEY (event_id) REFERENCES lifecycle_events(id)`,
					} {
						if _, err := db.ExecContext(context.Background(), stmt); err != nil {
							t.Errorf("restore after orphan case: %s: %v", stmt, err)
						}
					}
				})
			},
		},
		{
			name: "missing head", want: AlarmReasonMissingHead,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_heads WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "head without events", want: AlarmReasonHeadWithoutEvents,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_event_integrity WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM lifecycle_events WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "head sequence mismatch", want: AlarmReasonHeadMismatch,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET last_sequence=2 WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsupported head canonical version", want: AlarmReasonUnsupportedCanonicalVersion,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET canonical_version=99 WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unknown key", want: AlarmReasonUnknownKey,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET key_id='unknown/lifecycle-key' WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid head signature", want: AlarmReasonInvalidHeadSignature,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {
				if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET signature=decode(repeat('33',64),'hex') WHERE ticket_id=$1`, ticketID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing keyring", want: AlarmReasonMissingKeyring,
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) {},
		},
	}

	// These mutations model a database writer who can remove the append-only triggers.
	// Each subtest issues a fresh ticket and restores the triggers after verification.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := issueTicket(t, ctx, st, uuid.New())
			for _, table := range []string{"lifecycle_events", "lifecycle_event_integrity", "lifecycle_heads"} {
				if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				for _, table := range []string{"lifecycle_events", "lifecycle_event_integrity", "lifecycle_heads"} {
					if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ENABLE TRIGGER USER`); err != nil {
						t.Errorf("enable %s triggers: %v", table, err)
					}
				}
			}()
			tt.mutate(t, ctx, db, s.ticketID)
			verifier := st
			if tt.want == AlarmReasonMissingKeyring {
				verifier = New(db, Config{Signer: cfg.Signer, Policy: cfg.Policy})
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var id TicketIdentity
			if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, s.ticketID).Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
				t.Fatal(err)
			}
			err = verifier.verifyTicketChain(ctx, tx, s.ticketID, id)
			if got := alarmReasonFor(err); got != tt.want {
				t.Fatalf("alarmReasonFor(verifyTicketChain) = %q, want %q (err=%v)", got, tt.want, err)
			}
		})
	}

	// The orphan case drops the event FK; its cleanup must have put it back, or every
	// case after it ran on a weaker schema than production.
	var fk int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='lifecycle_event_integrity'::regclass AND conname='lifecycle_event_integrity_event_id_fkey'`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("lifecycle_event_integrity event FK after the branch table: count=%d err=%v", fk, err)
	}

	// verification_unavailable is reachable: a cancellation after BeginTx makes the verifier's
	// first query return the context error. verification_unclassified is not a verifier branch,
	// because verifyTicketChain wraps every query or scan failure and every integrity failure.
	// legacy_quarantine is emitted by degradedScan when it reads a quarantine row, not by this
	// verifier. The remaining verifier reason codes all have table cases above.
	t.Run("verification unavailable", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var id TicketIdentity
		if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, s.ticketID).Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
			t.Fatal(err)
		}
		cancelled, cancelQuery := context.WithCancel(ctx)
		cancelQuery()
		err = st.verifyTicketChain(cancelled, tx, s.ticketID, id)
		if got := alarmReasonFor(err); got != AlarmReasonVerificationUnavailable {
			t.Fatalf("alarmReasonFor(verifyTicketChain) = %q, want %q (err=%v)", got, AlarmReasonVerificationUnavailable, err)
		}
	})
}

func TestIntegrityAlarmDiagnosticLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	// These tests do not run in parallel, so replacing the process default logger is safe.
	assertLog := func(t *testing.T, wantDisposition Decision, drive func(t *testing.T) uuid.UUID) {
		t.Helper()
		var output bytes.Buffer
		previous := slog.Default()
		previousWriter := log.Writer()
		previousFlags := log.Flags()
		slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
		t.Cleanup(func() {
			slog.SetDefault(previous)
			log.SetOutput(previousWriter)
			log.SetFlags(previousFlags)
		})
		ticketID := drive(t)
		for _, field := range []string{ticketID.String(), "disposition=" + string(wantDisposition), "reason_code=" + string(AlarmReasonEntryHashMismatch), "entry hash mismatch"} {
			if !strings.Contains(output.String(), field) {
				t.Fatalf("diagnostic log lacks %q: %s", field, output.String())
			}
		}
	}

	t.Run("operator controlled denial", func(t *testing.T) {
		assertLog(t, DecisionIntegrityOperatorControlled, func(t *testing.T) uuid.UUID {
			org := uuid.New()
			s := issueTicket(t, ctx, st, org)
			if err := st.SetMode(ctx, org, ModeOperatorDeny, "operator"); err != nil {
				t.Fatal(err)
			}
			corruptChain(t, ctx, db, s.ticketID)
			got, err := st.Redeem(ctx, s.redeemInput())
			if err != nil || got.Accepted || got.Decision != DecisionIntegrityOperatorControlled {
				t.Fatalf("Redeem = %+v, %v", got, err)
			}
			return s.ticketID
		})
	})
	t.Run("unverified pass exit", func(t *testing.T) {
		assertLog(t, DecisionExitUnverified, func(t *testing.T) uuid.UUID {
			s := issueTicket(t, ctx, st, uuid.New())
			seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi"})
			corruptChain(t, ctx, db, s.ticketID)
			got, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionExit, time.Now().UTC()))
			if err != nil || got.Accepted || got.Decision != DecisionExitUnverified {
				t.Fatalf("exit Scan = %+v, %v", got, err)
			}
			return s.ticketID
		})
	})
}

func alarmResult(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID, wantReason AlarmReason, wantDisposition Decision) (AlarmReason, Decision, []byte) {
	t.Helper()
	var body []byte
	if err := db.QueryRowContext(ctx, `SELECT envelope FROM lifecycle_integrity_alarm_outbox
		WHERE envelope::jsonb -> 'data' ->> 'ticket_id'=$1
		  AND envelope::jsonb -> 'data' ->> 'reason'=$2
		  AND envelope::jsonb -> 'data' ->> 'disposition'=$3`,
		ticketID.String(), string(wantReason), string(wantDisposition)).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			TicketID    uuid.UUID   `json:"ticket_id"`
			Reason      AlarmReason `json:"reason"`
			Disposition Decision    `json:"disposition"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.TicketID != ticketID {
		t.Fatalf("alarm ticket = %s, want %s", envelope.Data.TicketID, ticketID)
	}
	return envelope.Data.Reason, envelope.Data.Disposition, body
}

func TestIntegrityAlarmReasonAtEveryProducer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)

	t.Run("first degraded admission", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		corruptChain(t, ctx, db, s.ticketID)
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || !got.Accepted || got.Decision != DecisionAdmittedDegraded {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonEntryHashMismatch, DecisionAdmittedDegraded)
	})

	t.Run("quarantine denial", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		corruptChain(t, ctx, db, s.ticketID)
		if _, err := st.Redeem(ctx, s.redeemInput()); err != nil {
			t.Fatal(err)
		}
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || got.Accepted || got.Decision != DecisionIntegrityQuarantined {
			t.Fatalf("second Redeem = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonEntryHashMismatch, DecisionIntegrityQuarantined)
	})

	t.Run("operator controlled denial", func(t *testing.T) {
		org := uuid.New()
		s := issueTicket(t, ctx, st, org)
		if err := st.SetMode(ctx, org, ModeOperatorDeny, "operator"); err != nil {
			t.Fatal(err)
		}
		corruptChain(t, ctx, db, s.ticketID)
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || got.Accepted || got.Decision != DecisionIntegrityOperatorControlled {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonEntryHashMismatch, DecisionIntegrityOperatorControlled)
	})

	t.Run("unverified pass exit", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi"})
		corruptChain(t, ctx, db, s.ticketID)
		got, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionExit, time.Now().UTC()))
		if err != nil || got.Accepted || got.Decision != DecisionExitUnverified {
			t.Fatalf("exit Scan = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonEntryHashMismatch, DecisionExitUnverified)
	})

	t.Run("single ticket read of legacy quarantine", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		seedLegacyQuarantine(t, ctx, db, s)
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || got.Accepted || got.Decision != DecisionIntegrityQuarantined {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonLegacyQuarantine, DecisionIntegrityQuarantined)
	})

	t.Run("pass read of legacy quarantine", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		seedPolicy(t, ctx, st, s, ReEntryPolicy{Mode: "multi"})
		seedLegacyQuarantine(t, ctx, db, s)
		got, err := st.Scan(ctx, scanInput(s, uuid.New(), AdmissionEntry, time.Now().UTC()))
		if err != nil || got.Accepted || got.Decision != DecisionIntegrityQuarantined {
			t.Fatalf("entry Scan = %+v, %v", got, err)
		}
		assertAlarm(t, ctx, db, s.ticketID, AlarmReasonLegacyQuarantine, DecisionIntegrityQuarantined)
	})
}

func assertAlarm(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID, wantReason AlarmReason, wantDisposition Decision) {
	t.Helper()
	reason, disposition, _ := alarmResult(t, ctx, db, ticketID, wantReason, wantDisposition)
	if reason != wantReason || disposition != wantDisposition {
		t.Fatalf("alarm = (%q, %q), want (%q, %q)", reason, disposition, wantReason, wantDisposition)
	}
}

func seedLegacyQuarantine(t *testing.T, ctx context.Context, db *sql.DB, s seeded) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at) VALUES($1,$2,'legacy fixture',now())`, s.ticketID, s.id.OrganizerID); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityAlarmPayloadExcludesDiagnosticText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	const marker = "LEAKMARK-7f3c"

	t.Run("verifier error", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_heads DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE lifecycle_heads SET key_id=$1 WHERE ticket_id=$2`, marker, s.ticketID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE lifecycle_heads ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		var sequence int64
		var headHash, signature []byte
		if err := db.QueryRowContext(ctx, `SELECT last_sequence,last_hash,signature FROM lifecycle_heads WHERE ticket_id=$1`, s.ticketID).Scan(&sequence, &headHash, &signature); err != nil {
			t.Fatal(err)
		}
		if err := st.cfg.Keyring.VerifyHead(s.ticketID, sequence, marker, headHash, signature); err == nil || !strings.Contains(err.Error(), marker) {
			t.Fatal("unknown-key verifier error does not contain the stored key id marker")
		}
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || !got.Accepted || got.Decision != DecisionAdmittedDegraded {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		reason, disposition, body := alarmResult(t, ctx, db, s.ticketID, AlarmReasonUnknownKey, DecisionAdmittedDegraded)
		assertAlarmBody(t, marker, body, reason, disposition, AlarmReasonUnknownKey, DecisionAdmittedDegraded)
	})

	t.Run("quarantine text", func(t *testing.T) {
		s := issueTicket(t, ctx, st, uuid.New())
		if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at) VALUES($1,$2,$3,now())`, s.ticketID, s.id.OrganizerID, marker); err != nil {
			t.Fatal(err)
		}
		got, err := st.Redeem(ctx, s.redeemInput())
		if err != nil || got.Accepted || got.Decision != DecisionIntegrityQuarantined {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		reason, disposition, body := alarmResult(t, ctx, db, s.ticketID, AlarmReasonLegacyQuarantine, DecisionIntegrityQuarantined)
		assertAlarmBody(t, marker, body, reason, disposition, AlarmReasonLegacyQuarantine, DecisionIntegrityQuarantined)
	})
}

func assertAlarmBody(t *testing.T, marker string, body []byte, reason AlarmReason, disposition Decision, wantReason AlarmReason, wantDisposition Decision) {
	t.Helper()
	if reason != wantReason || disposition != wantDisposition {
		t.Fatalf("alarm = (%q, %q), want (%q, %q)", reason, disposition, wantReason, wantDisposition)
	}
	if strings.Contains(string(body), marker) {
		t.Fatalf("alarm payload contains diagnostic marker %q: %s", marker, body)
	}
	if strings.Contains(string(body), `"reason":"`) && !strings.Contains(string(body), fmt.Sprintf(`"reason":"%s"`, wantReason)) {
		t.Fatalf("alarm reason is outside the fixed vocabulary: %s", body)
	}
}

func TestIntegrityAlarmReasonMigrationAndConstraint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 11); err != nil {
		t.Fatalf("apply migrations through 0011: %v", err)
	}
	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())
	seedLegacyQuarantine(t, ctx, db, s)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply migrations through 0012: %v", err)
	}

	var reason string
	var reasonCode sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT reason,reason_code FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, s.ticketID).Scan(&reason, &reasonCode); err != nil {
		t.Fatal(err)
	}
	if reason != "legacy fixture" || reasonCode.Valid {
		t.Fatalf("migrated quarantine row = (%q, %v), want unchanged text and NULL code", reason, reasonCode)
	}
	got, err := st.Redeem(ctx, s.redeemInput())
	if err != nil || got.Accepted || got.Decision != DecisionIntegrityQuarantined {
		t.Fatalf("Redeem = %+v, %v", got, err)
	}
	assertAlarm(t, ctx, db, s.ticketID, AlarmReasonLegacyQuarantine, DecisionIntegrityQuarantined)

	if _, err := db.ExecContext(ctx, `UPDATE lifecycle_integrity_quarantine SET reason_code='sequence_gap' WHERE ticket_id=$1`, s.ticketID); err == nil {
		t.Fatal("append-only quarantine trigger allowed a reason_code UPDATE")
	}
	invalidCodeTicket := issueTicket(t, ctx, st, uuid.New())
	_, err = db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,reason_code,admitted_at) VALUES($1,$2,'bad code','not_a_reason',now())`, invalidCodeTicket.ticketID, invalidCodeTicket.id.OrganizerID)
	if err == nil {
		t.Fatal("reason_code CHECK accepted a value outside the vocabulary")
	}
	var pgErr *pgconn.PgError
	// Name the constraint, not just the SQLSTATE: the table has other CHECKs (the
	// time and event_type checks), and a row that trips one of them would pass a
	// SQLSTATE-only assertion with the reason_code CHECK gone.
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "lifecycle_integrity_quarantine_reason_code_check" {
		t.Fatalf("invalid reason_code insert error = %v, want the reason_code CHECK violation", err)
	}

	var constraint string
	if err := db.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='lifecycle_integrity_quarantine'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%reason_code%'`).Scan(&constraint); err != nil {
		t.Fatal(err)
	}
	quoted := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(constraint, -1)
	gotReasons := make([]AlarmReason, 0, len(quoted))
	for _, match := range quoted {
		gotReasons = append(gotReasons, AlarmReason(match[1]))
	}
	if !sameAlarmReasons(gotReasons, alarmReasonVocabulary()) {
		t.Fatalf("database CHECK values = %v, Go vocabulary = %v (%s)", gotReasons, alarmReasonVocabulary(), constraint)
	}

}

func TestIntegrityAlarmReasonMigrationIsIrreversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	// Pin the migration version. Down rolls back exactly the current head, so
	// Up-to-head would only prove that whichever migration is newest refuses.
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration 0012 rolled back; quarantine reason codes must be retained")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='lifecycle_integrity_quarantine' AND column_name='reason_code'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("failed down attempt removed reason_code (n=%d err=%v)", n, err)
	}
}
