package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/store"
)

// In-process reprocess of quarantined catalog events (TKT-326 D5).
//
// reprocess-quarantine used to republish each row onto catalog's subjects, so a process other
// than catalog could speak for catalog. It now runs the live handler, handle, in this process,
// against the stored envelope. handle is not changed or copied: it takes a recording delivery
// instead of a jetstream.Msg, and the one disposition call it makes is classified into an
// Outcome. Every handle branch makes exactly one disposition call and then returns.

// Outcome is what an in-process reprocess of one quarantined event did.
type Outcome int

const (
	// OutcomeApplied: handle acked the event, so its effect (a projection, or a moot record) is
	// committed. The quarantine row can be resolved.
	OutcomeApplied Outcome = iota + 1
	// OutcomeTerminal: handle terminated the event. No binary will apply it. The row can be
	// resolved, and the envelope stays in the row for inspection until retention prunes it.
	OutcomeTerminal
	// OutcomeDeferred: handle asked for a delayed redelivery, and no store write failed. Examples:
	// an offering event before its pool, a catalog outage, an unusable catalog answer. The row
	// stays pending, so readiness stays down after a restart until a later run applies it.
	OutcomeDeferred
)

// errNoDeliveryMetadata is what the recording delivery answers to Metadata. A quarantine row
// stores no broker delivery count, so the unusable-answer bound cannot be judged. handle's
// existing branch for an unreadable count is a delayed NAK, so the event is deferred and never
// parked (TKT-326 D3, ADR-077).
var errNoDeliveryMetadata = errors.New("reprocess has no broker delivery count")

// errRequarantineRefused stops a reprocessed event from being written back into the quarantine it
// came from. The command gates on SupportsCatalogSchema first, so this is a second guard.
var errRequarantineRefused = errors.New("reprocess refuses to quarantine an event again; this binary does not support its schema")

// Reprocessor returns the in-process processor for one quarantined event. resolver must be a
// working catalog resolver: with a nil resolver, schema-1, schema-4 and schema-5 publications
// terminate (provisionInput), so the command refuses to start without catalog access (TKT-326
// D6). Each call builds its own recorder, store observer and handler, so calls share no state.
func Reprocessor(st catalogStore, resolver PerformanceResolver, log *slog.Logger) func(ctx context.Context, subject string, envelope []byte) (Outcome, error) {
	return func(ctx context.Context, subject string, envelope []byte) (Outcome, error) {
		obs := &observingStore{catalogStore: st}
		rec := &recordingDelivery{subject: subject, data: envelope}
		// A struct literal, not New: no broker, no metric counter (nothing is parked here), and
		// the readiness latch of this throwaway handler is never read.
		c := &Consumer{st: obs, resolver: resolver, log: log}
		c.handle(ctx, rec)
		return classifyReprocess(rec.calls, obs.err)
	}
}

// classifyReprocess maps the one disposition call handle made onto an Outcome (TKT-326 D5):
//
//	ack                               -> applied
//	term                              -> terminal
//	nak                               -> store failure (every immediate NAK follows a failed store write)
//	nak-delay after an observed error -> store failure
//	nak-delay otherwise               -> deferred
//	zero or several calls             -> error
func classifyReprocess(calls []string, storeErr error) (Outcome, error) {
	if len(calls) != 1 {
		return 0, fmt.Errorf("the handler made %d disposition calls %v, want exactly one", len(calls), calls)
	}
	switch calls[0] {
	case "ack":
		return OutcomeApplied, nil
	case "term":
		return OutcomeTerminal, nil
	case "nak":
		return 0, errors.New("store failure: the handler asked for an immediate retry; the log has the cause")
	case "nak-delay":
		if storeErr != nil {
			return 0, fmt.Errorf("store failure: %w", storeErr)
		}
		return OutcomeDeferred, nil
	default:
		return 0, fmt.Errorf("unknown disposition call %q", calls[0])
	}
}

// recordingDelivery stands in for a jetstream.Msg and records the disposition calls.
type recordingDelivery struct {
	subject string
	data    []byte
	calls   []string
}

func (d *recordingDelivery) Subject() string { return d.subject }
func (d *recordingDelivery) Data() []byte    { return d.data }
func (d *recordingDelivery) Metadata() (*jetstream.MsgMetadata, error) {
	return nil, errNoDeliveryMetadata
}
func (d *recordingDelivery) Ack() error { d.calls = append(d.calls, "ack"); return nil }
func (d *recordingDelivery) Nak() error { d.calls = append(d.calls, "nak"); return nil }
func (d *recordingDelivery) NakWithDelay(time.Duration) error {
	d.calls = append(d.calls, "nak-delay")
	return nil
}
func (d *recordingDelivery) Term() error { d.calls = append(d.calls, "term"); return nil }

// observingStore records a failure from the store writes that handle answers with a DELAYED NAK,
// the same call it makes for a deferral. Those writes are exactly three: RecordMootSlot (recordMoot),
// ConsumeMootOffering (applyOffering) and ParkCatalogEvent (unusableAnswer; unreachable here,
// because Metadata fails first). A new store call added before a NakWithDelay in handle must be
// added here too, or its failure reads as a deferral: the row then stays pending, which is safe
// but quiet. Every other store failure in handle is answered with an immediate NAK, which
// classifyReprocess treats as a failure on its own.
type observingStore struct {
	catalogStore
	err error
}

func (o *observingStore) note(err error) {
	if err != nil && o.err == nil {
		o.err = err
	}
}

func (o *observingStore) RecordMootSlot(ctx context.Context, eventID, organizerID, slotID uuid.UUID, source store.MootSource) error {
	err := o.catalogStore.RecordMootSlot(ctx, eventID, organizerID, slotID, source)
	o.note(err)
	return err
}

func (o *observingStore) ConsumeMootOffering(ctx context.Context, eventID, organizerID, slotID, poolID uuid.UUID) (bool, error) {
	moot, err := o.catalogStore.ConsumeMootOffering(ctx, eventID, organizerID, slotID, poolID)
	o.note(err)
	return moot, err
}

func (o *observingStore) ParkCatalogEvent(ctx context.Context, e store.ParkedCatalogEvent) (bool, error) {
	inserted, err := o.catalogStore.ParkCatalogEvent(ctx, e)
	o.note(err)
	return inserted, err
}

// QuarantineCatalogEvent is refused, and the refusal is observed: handle answers it with a
// delayed NAK, which classifyReprocess then reports as a failure, not a deferral.
func (o *observingStore) QuarantineCatalogEvent(context.Context, string, uuid.UUID, int, []byte) error {
	o.note(errRequarantineRefused)
	return errRequarantineRefused
}
