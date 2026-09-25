//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

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
