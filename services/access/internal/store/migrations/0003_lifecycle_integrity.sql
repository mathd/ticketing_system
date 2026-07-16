-- +goose Up
-- Ticket lifecycle trail integrity (ADR-021).
--
-- Read ADR-021 §The trust boundary before attributing a security property to
-- anything below. The adversary is defined as holding write access to THIS
-- database, so every table here is state that adversary owns. What constrains
-- them is the hash chain over data they cannot re-sign — which is why
-- modification and insertion are closed and targeted rollback is not. Quarantine
-- rows, failure counters and epoch signatures are deletable by that adversary;
-- they bound OUR OWN BUGS (a canonicalization drift, a botched rotation) and
-- nothing else. Do not describe them as containment in code, dashboards or docs.
--
-- The append-only triggers below are the same shape as 0001's: DDL, and DDL is
-- removable. They stop our code from doing the wrong thing. They are not
-- evidence, and citing one as tamper-evidence is the exact mistake ADR-021 was
-- written to stop.

-- Companion integrity rows (ADR-021 §D1). lifecycle_events itself is immutable
-- and is NOT rewritten — the chain lives beside it, one row per event. Verifiers
-- assert coverage in BOTH directions, so an event without an integrity row (the
-- append path bypassed) and an integrity row without an event (a forged link)
-- are equally visible.
CREATE TABLE lifecycle_event_integrity (
  event_id uuid PRIMARY KEY REFERENCES lifecycle_events(id),
  ticket_id uuid NOT NULL REFERENCES tickets(id),
  -- Ticket-local. The chain shards per ticket, so there is no organizer-wide
  -- ordering to record: ADR-021 §Consequences surrenders that deliberately, in
  -- exchange for never locking an organizer on the turnstile path.
  sequence bigint NOT NULL CHECK (sequence > 0),
  canonical_version smallint NOT NULL CHECK (canonical_version > 0),
  previous_hash bytea NOT NULL CHECK (octet_length(previous_hash) = 32),
  entry_hash bytea NOT NULL CHECK (octet_length(entry_hash) = 32),
  UNIQUE (ticket_id, sequence)
);

-- One head per ticket. Mutable by design: the head advances on every append.
-- The signature binds ticket, sequence, canonical version and key id — never the
-- head hash alone (ADR-021 §D5), or it would replay onto any ticket that ever
-- reached the same head.
CREATE TABLE lifecycle_heads (
  ticket_id uuid PRIMARY KEY REFERENCES tickets(id),
  organizer_id uuid NOT NULL,
  last_sequence bigint NOT NULL CHECK (last_sequence > 0),
  canonical_version smallint NOT NULL CHECK (canonical_version > 0),
  last_hash bytea NOT NULL CHECK (octet_length(last_hash) = 32),
  key_id text NOT NULL,
  signature bytea NOT NULL CHECK (octet_length(signature) = 64),
  changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lifecycle_heads_organizer_idx ON lifecycle_heads (organizer_id);

-- Epoch signatures (ADR-021 §D5). At rotation the outgoing key signs each head
-- as it stands, and that signature is retained so a later compromise of the new
-- key cannot forge history under a destroyed one.
--
-- It does not need to forge it: it can DELETE this row. Retained where? Here —
-- inside the adversary's database. And because ADR-021 §Consequences surrenders
-- global ticket-set completeness, no rule says a row must exist, so a verifier
-- cannot tell a deleted signature from a ticket that legitimately has none.
-- Epoch signatures raise the work of a current-key compromise from "re-sign a
-- head" to "re-sign a head and delete these rows". They are not containment
-- until an external rotation manifest exists (TKT-11).
CREATE TABLE lifecycle_head_epoch_signatures (
  ticket_id uuid NOT NULL REFERENCES tickets(id),
  key_id text NOT NULL,
  ticket_sequence bigint NOT NULL CHECK (ticket_sequence > 0),
  canonical_version smallint NOT NULL CHECK (canonical_version > 0),
  head_hash bytea NOT NULL CHECK (octet_length(head_hash) = 32),
  signature bytea NOT NULL CHECK (octet_length(signature) = 64),
  sealed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ticket_id, key_id)
);

-- Per-organizer checkpoint chain (ADR-021 §D2): a signed Merkle root over the
-- ticket heads that changed since the previous checkpoint, chained to it.
--
-- This buys NO rollback detection. An adversary does not rewrite this chain,
-- they TRUNCATE it — and because checkpoints are deltas, the suffix drops
-- without dragging other tickets' heads back, leaving a chain that verifies
-- clean. It exists for one reason: it is the structure TKT-11 can afford to
-- anchor, one root per interval instead of one attestation per ticket head.
-- That is a build-order argument. It is not a security one.
--
-- created_at has no default: it is covered by the signature, so the stored value
-- must be exactly the value that was signed.
CREATE TABLE lifecycle_checkpoints (
  checkpoint_id bigserial PRIMARY KEY,
  organizer_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence > 0),
  previous_root bytea NOT NULL CHECK (octet_length(previous_root) = 32),
  root bytea NOT NULL CHECK (octet_length(root) = 32),
  leaf_count integer NOT NULL CHECK (leaf_count > 0),
  canonical_version smallint NOT NULL CHECK (canonical_version > 0),
  key_id text NOT NULL,
  signature bytea NOT NULL CHECK (octet_length(signature) = 64),
  created_at timestamptz NOT NULL,
  UNIQUE (organizer_id, sequence)
);

-- Signed head snapshots. This is both the checkpoint worker's queue (rows with
-- no checkpoint yet) and the archive that lets a past root be recomputed after
-- the live head has moved on — the current head cannot reconstruct a historical
-- leaf. A separate leaves table would hold exactly this data twice, so there
-- isn't one: checkpoint N's leaf set is the latest change per ticket among the
-- rows assigned to N.
--
-- key_id and signature are NOT redundant with lifecycle_heads, and leaving them
-- out was a real hole (PR #51 review, R1). Everything else here is ordinary
-- mutable-ish operational state, but the checkpoint SIGNS this table's contents:
-- the worker builds a Merkle root over these rows and the verifier recomputes
-- from the same rows, so without a signature ON THE SNAPSHOT both would agree on
-- whatever the table said. Nothing blocks an INSERT here — the trigger below
-- covers UPDATE and DELETE only, and an append-only queue must accept inserts —
-- so a database writer could add a fabricated newer snapshot for a real ticket,
-- watch the dedup select it, and get a signed checkpoint committing to a head
-- that never existed. That breaks the checkpoint's ONLY purpose: TKT-11 anchors
-- these roots, and an anchor over invented heads is worse than no anchor.
--
-- Carrying the head's own signature makes a leaf unforgeable without the private
-- key. A REPLAYED older-but-genuine snapshot is still possible, but that is the
-- rollback class ADR-021 §Threat model already records as undetected — this
-- closes fabrication, not rollback.
--
-- checkpoint_id is the one mutable column (assignment is operational state).
-- Tampering with it changes the recomputed root, which the checkpoint signature
-- catches.
CREATE TABLE lifecycle_head_changes (
  change_id bigserial PRIMARY KEY,
  ticket_id uuid NOT NULL REFERENCES tickets(id),
  organizer_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence > 0),
  head_hash bytea NOT NULL CHECK (octet_length(head_hash) = 32),
  canonical_version smallint NOT NULL CHECK (canonical_version > 0),
  key_id text NOT NULL,
  signature bytea NOT NULL CHECK (octet_length(signature) = 64),
  checkpoint_id bigint REFERENCES lifecycle_checkpoints(checkpoint_id),
  created_at timestamptz NOT NULL DEFAULT now()
);
-- The worker's only claim path: this organizer's pending changes, oldest first.
-- Partial, because assigned rows are never queued again and would only bloat it.
CREATE INDEX lifecycle_head_changes_pending_idx
  ON lifecycle_head_changes (organizer_id, change_id)
  WHERE checkpoint_id IS NULL;
-- The verifier's path: recompute checkpoint N's leaves. Ordered so the latest
-- change per ticket is a DISTINCT ON away.
CREATE INDEX lifecycle_head_changes_checkpoint_idx
  ON lifecycle_head_changes (checkpoint_id, ticket_id, change_id);

-- A ticket whose chain failed verification: admitted once, then denied
-- (ADR-021 §D6). One row per ticket, append-only, no operator override — a
-- quarantined ticket's later scans are denied even when its organizer is in
-- operator-admit mode.
--
-- Append-only so a degraded admission stays reconstructable AFTER the bug is
-- found: the chain deliberately records nothing here (appending onto an
-- unverified predecessor would poison it permanently), so this row is the only
-- record that a person physically walked in. Against the database adversary that
-- is worth nothing — they delete it. Against a canonicalization bug that
-- admitted a thousand tickets, it is the blast radius.
CREATE TABLE lifecycle_integrity_quarantine (
  ticket_id uuid PRIMARY KEY REFERENCES tickets(id),
  organizer_id uuid NOT NULL,
  reason text NOT NULL,
  admitted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lifecycle_integrity_quarantine_organizer_idx
  ON lifecycle_integrity_quarantine (organizer_id, admitted_at);

-- Operator mode per organizer (ADR-021 §D6). Mutable, and resettable by the
-- database adversary — which is exactly why §D6 says this bounds our bugs and
-- not attackers. Against an attacker fail-open is unbounded, full stop.
--
-- There is no failure counter here: the threshold is "N distinct first-time
-- corrupt tickets within the window", and a quarantine row is created exactly
-- once per such ticket with its admission time. Counting those rows IS the
-- rolling window, so a stored counter would be a second copy of a derivable
-- number, free to drift from it.
CREATE TABLE lifecycle_integrity_organizer_state (
  organizer_id uuid PRIMARY KEY,
  mode text NOT NULL DEFAULT 'normal' CHECK (mode IN ('normal', 'operator_deny', 'operator_admit')),
  mode_set_at timestamptz NOT NULL DEFAULT now(),
  mode_set_by text NOT NULL DEFAULT 'system'
);

-- Transactional outbox for integrity alarms, same shape as commerce's
-- completion_outbox (services/commerce/internal/store/migrations/0003) — an
-- established pattern here, not a new one.
--
-- The row is committed with the quarantine/mode decision, so an admission can
-- never happen without an owed alarm: fail-open (§D6) is only defensible while
-- the alarm reaches someone, and a crash between commit and publish must leave a
-- claimable row rather than a silent bypass.
--
-- envelope is json, not jsonb: jsonb normalizes key order and whitespace, so it
-- preserves the document but not the bytes. event_id is a fresh UUID per alarm —
-- every occurrence matters, so unlike the completion outbox there is nothing to
-- deduplicate; it is Nats-Msg-Id only so a redelivery after a lost claim is not
-- a second alarm.
CREATE TABLE lifecycle_integrity_alarm_outbox (
  event_id uuid PRIMARY KEY,
  subject text NOT NULL,
  envelope json NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz,
  claim_id uuid,
  lease_until timestamptz,
  attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error text,
  dead_lettered_at timestamptz
);
CREATE INDEX lifecycle_integrity_alarm_outbox_claimable_idx
  ON lifecycle_integrity_alarm_outbox (next_attempt_at)
  WHERE published_at IS NULL AND dead_lettered_at IS NULL;

-- Append-only enforcement for the cryptographic tables. Same caveat as 0001:
-- this is DDL, so it constrains our code, not an adversary.
-- +goose StatementBegin
CREATE FUNCTION lifecycle_integrity_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER lifecycle_event_integrity_no_change BEFORE UPDATE OR DELETE ON lifecycle_event_integrity
  FOR EACH ROW EXECUTE FUNCTION lifecycle_integrity_append_only();
CREATE TRIGGER lifecycle_event_integrity_no_truncate BEFORE TRUNCATE ON lifecycle_event_integrity
  FOR EACH STATEMENT EXECUTE FUNCTION lifecycle_integrity_append_only();
CREATE TRIGGER lifecycle_epoch_no_change BEFORE UPDATE OR DELETE ON lifecycle_head_epoch_signatures
  FOR EACH ROW EXECUTE FUNCTION lifecycle_integrity_append_only();
CREATE TRIGGER lifecycle_checkpoints_no_change BEFORE UPDATE OR DELETE ON lifecycle_checkpoints
  FOR EACH ROW EXECUTE FUNCTION lifecycle_integrity_append_only();
CREATE TRIGGER lifecycle_quarantine_no_change BEFORE UPDATE OR DELETE ON lifecycle_integrity_quarantine
  FOR EACH ROW EXECUTE FUNCTION lifecycle_integrity_append_only();

-- head_changes is append-only except for the checkpoint assignment, so it needs
-- its own rule rather than the blanket one above.
-- +goose StatementBegin
CREATE FUNCTION lifecycle_head_changes_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP <> 'UPDATE' THEN
    RAISE EXCEPTION 'lifecycle head change snapshots are append-only';
  END IF;
  IF NEW.change_id IS DISTINCT FROM OLD.change_id
     OR NEW.ticket_id IS DISTINCT FROM OLD.ticket_id
     OR NEW.organizer_id IS DISTINCT FROM OLD.organizer_id
     OR NEW.sequence IS DISTINCT FROM OLD.sequence
     OR NEW.head_hash IS DISTINCT FROM OLD.head_hash
     OR NEW.canonical_version IS DISTINCT FROM OLD.canonical_version
     OR NEW.key_id IS DISTINCT FROM OLD.key_id
     OR NEW.signature IS DISTINCT FROM OLD.signature
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'lifecycle head change snapshots are immutable; only checkpoint assignment may change';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER lifecycle_head_changes_immutable BEFORE UPDATE OR DELETE ON lifecycle_head_changes
  FOR EACH ROW EXECUTE FUNCTION lifecycle_head_changes_snapshot_immutable();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  RAISE EXCEPTION 'cannot roll back the lifecycle integrity trail without destroying adopted chain history';
END $$;
-- +goose StatementEnd
