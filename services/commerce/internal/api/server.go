package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	apispec "ticketing/services/commerce/api"
	commerceevents "ticketing/services/commerce/internal/events"
	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/contract"
	"ticketing/shared/fakepsp"
	"ticketing/shared/httpx"
)

var errCheckoutConflict = errors.New("checkout request conflicts with existing order")

// errRecoveryInProgress reports that the recovery runner holds this order under a live
// lease. Unlike errCheckoutConflict this is transient: the lease lapses, and the buyer's
// retry then either finds the order resolved or claims it cleanly.
var errRecoveryInProgress = errors.New("order is being recovered; retry shortly")

type Server struct {
	db                                           *sql.DB
	client                                       *http.Client
	catalogURL, inventoryURL, paymentsURL, token string
	publisher                                    commerceevents.Publisher
}

func New(db *sql.DB, client *http.Client, catalog, inventory, payments, token string, publishers ...commerceevents.Publisher) *Server {
	var publisher commerceevents.Publisher
	if len(publishers) > 0 {
		publisher = publishers[0]
	}
	return &Server{db: db, client: client, catalogURL: strings.TrimSuffix(catalog, "/"), inventoryURL: strings.TrimSuffix(inventory, "/"), paymentsURL: strings.TrimSuffix(payments, "/"), token: token, publisher: publisher}
}
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/reservations", s.reserve)
	r.Post("/orders", s.checkout)
	r.Get("/orders/{id}", s.getOrder)
	r.Get("/internal/buyers/{id}/delivery-email", s.deliveryEmail)
	r.Post("/internal/operational-holds/{id}/convert", s.convertOperational)
	validated, err := contract.RequestValidator(apispec.Spec, r)
	if err != nil {
		panic(err)
	}
	return validated
}
func write(w http.ResponseWriter, c int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(c)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if httpx.DecodeJSON(w, r, v, 1<<20) != nil {
		write(w, 400, map[string]string{"error": "invalid body"})
		return false
	}
	return true
}
func (s *Server) call(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return 0, nil, e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, url, body)
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if internal {
		req.Header.Set("X-Internal-Token", s.token)
	}
	resp, e := s.client.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer func() { _ = resp.Body.Close() }()
	out, e := io.ReadAll(resp.Body)
	return resp.StatusCode, out, e
}

type price struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}
type offer struct {
	ID            uuid.UUID `json:"id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	PerformanceID uuid.UUID `json:"performance_id"`
	Price         price     `json:"price"`
}
type reserveRequest struct {
	OrganizerID  uuid.UUID `json:"organizer_id"`
	TicketTypeID uuid.UUID `json:"ticket_type_id"`
	Quantity     int32     `json:"quantity"`
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in reserveRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.TicketTypeID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, 400, map[string]string{"error": "invalid reservation"})
		return
	}
	code, body, err := s.call(r.Context(), http.MethodGet, s.catalogURL+"/internal/ticket-types/"+in.TicketTypeID.String(), "", nil, true)
	if err != nil || code != 200 {
		write(w, 502, map[string]string{"error": "catalog unavailable"})
		return
	}
	var o offer
	if json.Unmarshal(body, &o) != nil || o.OrganizerID != in.OrganizerID || o.Price.Currency != "EUR" || o.Price.Amount < 0 || o.Price.Amount > math.MaxInt64/int64(in.Quantity) {
		write(w, 409, map[string]string{"error": "offer not sellable in EUR"})
		return
	}
	total := o.Price.Amount * int64(in.Quantity)
	holdBody := map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID, "ticket_type_id": in.TicketTypeID, "quantity": in.Quantity, "unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	code, body, err = s.call(r.Context(), http.MethodPost, s.inventoryURL+"/holds", key, holdBody, false)
	if err != nil || (code != 200 && code != 201) {
		write(w, 409, map[string]string{"error": "inventory unavailable"})
		return
	}
	var hold struct {
		ID         uuid.UUID `json:"hold_id"`
		ExpiresAt  time.Time `json:"expires_at"`
		ServerTime time.Time `json:"server_time"`
	}
	if json.Unmarshal(body, &hold) != nil {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("reservation:"+in.OrganizerID.String()+":"+key))
	buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte("buyer:"+id.String()))
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'held') ON CONFLICT(id) DO NOTHING`, id, in.OrganizerID, hold.ID, o.PerformanceID, in.TicketTypeID, buyer, in.Quantity, o.Price.Amount, total, o.Price.Currency)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	write(w, 201, map[string]any{"reservation_id": id, "hold_id": hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": hold.ExpiresAt, "server_time": hold.ServerTime})
}

type checkoutRequest struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	PaymentToken  string    `json:"payment_token"`
}

func paymentFailureResponse(body []byte, fallbackStatus string) map[string]any {
	out := map[string]any{"status": fallbackStatus}
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) == nil && decoded != nil {
		if status, ok := decoded["status"].(string); ok {
			out["status"] = status
		}
		if replay, ok := decoded["replay"].(bool); ok {
			out["replay"] = replay
		}
	}
	return out
}

type reservation struct {
	ID, OrganizerID, HoldID, BuyerID, SlotID, TicketTypeID uuid.UUID
	Quantity                                               int32
	Amount                                                 int64
	Currency, Status                                       string
}

func (s *Server) load(ctx context.Context, id uuid.UUID) (reservation, error) {
	var x reservation
	err := s.db.QueryRowContext(ctx, `SELECT id,organizer_id,hold_id,buyer_id,slot_id,ticket_type_id,quantity,total_amount,currency,status FROM reservations WHERE id=$1`, id).Scan(&x.ID, &x.OrganizerID, &x.HoldID, &x.BuyerID, &x.SlotID, &x.TicketTypeID, &x.Quantity, &x.Amount, &x.Currency, &x.Status)
	return x, err
}

// publishOwed sends an order's committed envelope inline, for promptness only. It is
// best-effort by design: the outbox already owes the event, so every failure path here
// is recoverable by the drainer and none of them may fail the buyer's request.
//
// It publishes the FROZEN bytes rather than rebuilding the envelope. Rebuilding would
// put a different payload on the wire than the drainer's retry would send, under the
// same deterministic id — defeating the point of freezing it at commit.
func (s *Server) publishOwed(ctx context.Context, order uuid.UUID) {
	if s.publisher == nil {
		return // the drainer owns delivery
	}
	eventID, subject, envelope, ok, err := commercestore.FrozenEnvelope(ctx, s.db, order)
	if err != nil || !ok {
		if err != nil {
			slog.Default().WarnContext(ctx, "read owed completion envelope; left to the outbox drainer",
				"order_id", order, "err", err)
		}
		return // already published, or unreadable: either way the drainer reconciles
	}
	if err := s.publisher.PublishRaw(ctx, subject, eventID, envelope); err != nil {
		slog.Default().WarnContext(ctx, "inline publish of owed completion event failed; left to the outbox drainer",
			"order_id", order, "err", err)
		return
	}
	// Retire the row so the drainer does not republish what just went out. Only
	// retires an unleased row: if a drainer holds it, that drainer owns the outcome.
	if err := commercestore.MarkPublishedByOrder(ctx, s.db, order); err != nil {
		slog.Default().WarnContext(ctx, "retire owed completion event after inline publish",
			"order_id", order, "err", err)
	}
}

func (s *Server) guestReference(ctx context.Context, order uuid.UUID) (uuid.UUID, error) {
	var ref uuid.UUID
	err := s.db.QueryRowContext(ctx, `SELECT guest_order_ref FROM orders WHERE id=$1 AND status='completed'`, order).Scan(&ref)
	return ref, err
}
func (s *Server) fact(ctx context.Context, x reservation, order uuid.UUID, typ string) error {
	fid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(order.String()+":"+typ))
	_, e := s.db.ExecContext(ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, fid, order, x.OrganizerID, x.BuyerID, typ, x.Amount, x.Currency)
	if e != nil {
		return e
	}
	var occurred time.Time
	if e := s.db.QueryRowContext(ctx, `SELECT occurred_at FROM order_facts WHERE fact_id=$1`, fid).Scan(&occurred); e != nil {
		return e
	}
	code, _, e := s.call(ctx, http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{"fact_id": fid, "organizer_id": x.OrganizerID, "fact_type": typ, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "occurred_at": occurred, "payload": map[string]string{"order_id": order.String()}}, true)
	if e != nil || code != 200 {
		return errors.New("journal unavailable")
	}
	return nil
}

func (s *Server) claimOrder(ctx context.Context, x reservation, key, fingerprint string) (uuid.UUID, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, x.ID.String()); err != nil {
		return uuid.Nil, "", err
	}
	var id uuid.UUID
	var storedKey, storedFingerprint, status string
	var recoveryClaim uuid.NullUUID
	var recoveryLease sql.NullTime
	// FOR UPDATE, and the recovery columns are read here rather than ignored: the
	// recovery runner decides from a payments lookup that is only durable evidence while
	// no one else can charge this order. Reading the row without locking it would let
	// this replay bind a charge in the window between recovery's lookup and its release.
	err = tx.QueryRowContext(ctx, `SELECT id,idempotency_key,request_fingerprint,status,recovery_claim_id,recovery_lease_until FROM orders WHERE reservation_id=$1 FOR UPDATE`, x.ID).
		Scan(&id, &storedKey, &storedFingerprint, &status, &recoveryClaim, &recoveryLease)
	if errors.Is(err, sql.ErrNoRows) {
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte("order:"+x.OrganizerID.String()+":"+key))
		_, err = tx.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint) VALUES($1,$2,'created',$3,$4)`, id, x.ID, key, fingerprint)
		status = "created"
	} else if err == nil {
		if storedKey != key || storedFingerprint != fingerprint {
			return uuid.Nil, "", errCheckoutConflict
		}
		// A recovery pass holds this order under an unexpired lease. It may already have
		// asked payments whether a charge exists and been told no; binding one now would
		// turn that true answer into a false one, and the seat would be released out from
		// under a captured payment. Recovery's decision is bounded by its lease, so the
		// buyer can retry once it lapses.
		if recoveryClaim.Valid && recoveryLease.Valid && recoveryLease.Time.After(time.Now()) {
			return uuid.Nil, "", errRecoveryInProgress
		}
		// Reopening an existing order is fresh activity on it. Without this the row keeps
		// the timestamp of the checkout that died, stays past recovery's grace period,
		// and recovery can claim it while this request is live — the grace period only
		// protects orders whose updated_at actually moves.
		if _, err = tx.ExecContext(ctx, `UPDATE orders SET updated_at=now() WHERE id=$1`, id); err != nil {
			return uuid.Nil, "", err
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return uuid.Nil, "", errCheckoutConflict
	}
	if err != nil {
		return uuid.Nil, "", err
	}
	return id, status, tx.Commit()
}

func checkoutClaimProblem(err error) (int, string) {
	if errors.Is(err, errCheckoutConflict) {
		return http.StatusConflict, "checkout conflicts with an existing request"
	}
	if errors.Is(err, errRecoveryInProgress) {
		// 409 like the conflict above, but the distinct message matters: this one clears
		// on its own once the recovery lease lapses.
		return http.StatusConflict, "this order is being recovered; retry shortly"
	}
	return http.StatusInternalServerError, "persist checkout"
}

func persistenceReadProblem(err error) (int, string) {
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, "not found"
	}
	return http.StatusServiceUnavailable, "temporarily unavailable"
}

func paymentOutcomeProblem(code int) (int, string, bool) {
	if code == http.StatusConflict {
		return http.StatusConflict, "payment operation in progress; retry with the same idempotency key", true
	}
	return 0, "", false
}

func (s *Server) markUnknown(ctx context.Context, reservationID, orderID uuid.UUID) {
	_, _ = s.db.ExecContext(ctx, `UPDATE reservations SET status='unknown' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, reservationID)
	_, _ = s.db.ExecContext(ctx, `UPDATE orders SET status='payment_unknown',updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, orderID)
}

func (s *Server) markTerminalFailure(ctx context.Context, reservationID, orderID uuid.UUID, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE reservations SET status='failed' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, reservationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE orders SET status=$2,updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, orderID, status); err != nil {
		return err
	}
	return tx.Commit()
}

func terminalCheckoutCode(status string) int {
	if status == "timeout" {
		return http.StatusRequestTimeout
	}
	return http.StatusPaymentRequired
}

func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in checkoutRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ReservationID == uuid.Nil || strings.TrimSpace(in.Name) == "" || !strings.Contains(in.Email, "@") || !fakepsp.ValidToken(in.PaymentToken) {
		write(w, 400, map[string]string{"error": "invalid checkout"})
		return
	}
	x, err := s.load(r.Context(), in.ReservationID)
	if err != nil {
		code, message := persistenceReadProblem(err)
		if code != http.StatusNotFound {
			slog.Default().ErrorContext(r.Context(), "load checkout reservation", "err", err)
			write(w, code, map[string]string{"error": message})
			return
		}
		write(w, code, map[string]string{"error": "reservation " + message})
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s", in.ReservationID, strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email)), in.PaymentToken))))
	order, orderStatus, err := s.claimOrder(r.Context(), x, key, fingerprint)
	if err != nil {
		code, message := checkoutClaimProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "claim checkout order", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	if orderStatus == "completed" {
		ref, e := s.guestReference(r.Context(), order)
		if e != nil {
			write(w, 503, map[string]string{"error": "ticket issuance pending; retry checkout"})
			return
		}
		// The original completion owed this event; if it is still unpublished, send the
		// frozen bytes. If it is already published (or backfilled and drained), this is
		// a no-op — a replay must not mint a second, differently-timestamped copy.
		s.publishOwed(r.Context(), order)
		write(w, 200, map[string]any{"order_id": order, "guest_order_ref": ref, "status": "completed"})
		return
	}
	if orderStatus == "declined" || orderStatus == "timeout" {
		// A previous attempt may have persisted the terminal local outcome while
		// payments was unavailable. Replay the idempotent journal fact before
		// returning the buyer-visible terminal result.
		if err := s.fact(r.Context(), x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return
		}
		write(w, terminalCheckoutCode(orderStatus), map[string]any{"order_id": order, "status": orderStatus, "replay": true})
		return
	}
	if _, err = s.db.ExecContext(r.Context(), `INSERT INTO buyer_pii(buyer_id,name,email) VALUES($1,$2,$3) ON CONFLICT(buyer_id) DO UPDATE SET name=EXCLUDED.name,email=EXCLUDED.email`, x.BuyerID, in.Name, in.Email); err != nil {
		write(w, 500, map[string]string{"error": "persist buyer"})
		return
	}
	if err := s.fact(r.Context(), x, order, "order.created"); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	code, _, err := s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/finalize?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
	if err != nil || code != 200 {
		write(w, 409, map[string]string{"error": "hold expired"})
		return
	}
	if _, err = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='finalizing' WHERE id=$1 AND status IN ('held','finalizing')`, x.ID); err != nil {
		write(w, 500, map[string]string{"error": "persist checkout"})
		return
	}
	charge := map[string]any{"order_id": order, "organizer_id": x.OrganizerID, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "payment_token": in.PaymentToken}
	code, body, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/charges", key, charge, true)
	if err != nil {
		s.markUnknown(r.Context(), x.ID, order)
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	if problemCode, message, active := paymentOutcomeProblem(code); active {
		write(w, problemCode, map[string]any{"order_id": order, "status": "payment_in_progress", "error": message})
		return
	}
	if code == 402 || code == 408 {
		releaseCode, _, releaseErr := s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/release?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
		if releaseErr != nil || releaseCode != 200 {
			write(w, 202, map[string]any{"order_id": order, "status": "release_pending"})
			return
		}
		terminalStatus := "declined"
		if code == http.StatusRequestTimeout {
			terminalStatus = "timeout"
		}
		if err := s.markTerminalFailure(r.Context(), x.ID, order, terminalStatus); err != nil {
			write(w, 500, map[string]string{"error": "persist failure"})
			return
		}
		if err := s.fact(r.Context(), x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return
		}
		out := paymentFailureResponse(body, terminalStatus)
		out["order_id"] = order
		write(w, code, out)
		return
	}
	if code != 200 {
		s.markUnknown(r.Context(), x.ID, order)
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	code, _, err = s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/holds/%s/confirm?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
	if err != nil || code != 200 {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE orders SET status='confirmation_pending',updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, order)
		write(w, 202, map[string]any{"order_id": order, "status": "confirmation_pending"})
		return
	}
	if err := s.fact(r.Context(), x, order, "order.completed"); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	guestRef, err := uuid.NewRandom()
	if err != nil {
		write(w, 500, map[string]string{"error": "mint guest order reference"})
		return
	}
	guestRef, err = commercestore.CompleteOrder(r.Context(), s.db, commercestore.Completion{
		ReservationID: x.ID, OrderID: order, OrganizerID: x.OrganizerID, BuyerID: x.BuyerID,
		SlotID: x.SlotID, TicketTypeID: x.TicketTypeID, Quantity: x.Quantity,
	}, guestRef)
	if err != nil {
		slog.Default().ErrorContext(r.Context(), "persist order completion", "err", err)
		write(w, 500, map[string]string{"error": "persist completion"})
		return
	}
	// The completion commit owes the event, so issuance no longer depends on this
	// publish succeeding: a failure here (or a crash) leaves a claimable outbox row
	// the drainer picks up. Publish inline anyway to keep the happy path prompt — the
	// buyer's ticket should not wait a drain interval — but never fail the checkout
	// on it. The old 503 told a buyer whose money was taken and whose order was
	// committed to "retry checkout", which was both alarming and unnecessary.
	s.publishOwed(r.Context(), order)
	write(w, 200, map[string]any{"order_id": order, "guest_order_ref": guestRef, "status": "completed"})
}

// convertOperational is the staff entry point for selling out of an operational hold
// (TKT-77 / ADR-023): resolve the offer through the existing catalog seam (no price
// override), have inventory carve a buyer hold out of the operational hold atomically,
// then persist the same reservation shape the public reserve path produces so the
// existing checkout completes it. Deterministic reservation identity + the forwarded
// idempotency key make a crash between the inventory commit and the reservation insert
// repairable by replaying the same request.
func (s *Server) convertOperational(w http.ResponseWriter, r *http.Request) {
	// Fail closed on an unconfigured token; a bad token reads as 404 like deliveryEmail.
	if s.token == "" || r.Header.Get("X-Internal-Token") != s.token {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	sourceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid hold id"})
		return
	}
	var in struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		TicketTypeID uuid.UUID `json:"ticket_type_id"`
		Quantity     int32     `json:"quantity"`
		Actor        string    `json:"actor"`
		Reason       string    `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.TicketTypeID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, 400, map[string]string{"error": "invalid conversion request"})
		return
	}
	code, body, err := s.call(r.Context(), http.MethodGet, s.catalogURL+"/internal/ticket-types/"+in.TicketTypeID.String(), "", nil, true)
	if err != nil || code != 200 {
		write(w, 409, map[string]string{"error": "unknown ticket type"})
		return
	}
	var o offer
	if json.Unmarshal(body, &o) != nil || o.OrganizerID != in.OrganizerID || o.Price.Currency != "EUR" || o.Price.Amount < 0 || o.Price.Amount > math.MaxInt64/int64(in.Quantity) {
		write(w, 409, map[string]string{"error": "offer not sellable in EUR"})
		return
	}
	// slot_id makes ticket-type/slot agreement a precondition inside inventory's locked
	// transaction: a mismatch rejects with the operational hold untouched, instead of
	// discovering it after the carve has committed.
	convBody := map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID, "quantity": in.Quantity, "ticket_type_id": in.TicketTypeID, "unit_amount": o.Price.Amount, "currency": o.Price.Currency, "actor": in.Actor, "reason": in.Reason}
	code, body, err = s.call(r.Context(), http.MethodPost, s.inventoryURL+"/internal/operational-holds/"+sourceID.String()+"/convert", key, convBody, true)
	if err != nil {
		write(w, 409, map[string]string{"error": "inventory unavailable"})
		return
	}
	if code == 404 || code == 409 {
		write(w, code, map[string]string{"error": "conversion rejected"})
		return
	}
	if code != 200 && code != 201 {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	var conv struct {
		Hold struct {
			ID         uuid.UUID `json:"hold_id"`
			SlotID     uuid.UUID `json:"slot_id"`
			Status     string    `json:"status"`
			ExpiresAt  time.Time `json:"expires_at"`
			ServerTime time.Time `json:"server_time"`
		} `json:"hold"`
		SourceRemaining int32 `json:"source_remaining"`
	}
	if json.Unmarshal(body, &conv) != nil || conv.Hold.ID == uuid.Nil {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	// A replay must be judged by the child's lifecycle status, not its timestamp: a
	// confirmed claim keeps its elapsed expires_at forever (a 409 here would instruct
	// staff to carve the same seats twice), finalizing is live regardless of deadline,
	// and a terminal expired/released child must not become a reservation no checkout
	// can complete — that capacity is already back in the public pool (ADR-023).
	if code == 200 {
		switch conv.Hold.Status {
		case "confirmed", "finalizing":
			// live or already sold — the replay is legitimate; persist/return below
		case "held":
			if !conv.Hold.ExpiresAt.After(conv.Hold.ServerTime) {
				write(w, 409, map[string]string{"error": "converted hold expired; place a new conversion"})
				return
			}
		case "expired", "released":
			// Terminal: that capacity is public again; a reservation here could never
			// check out, and re-carving is an explicit new staff operation.
			write(w, 409, map[string]string{"error": "converted hold is no longer usable; place a new conversion"})
			return
		default:
			// Empty or future status (version skew): unknown is not terminal — advising
			// a re-conversion here could double-carve a still-live child (ADR-017's
			// rule: never judge a future variant with today's semantics).
			write(w, 502, map[string]string{"error": "invalid inventory response"})
			return
		}
	}
	total := o.Price.Amount * int64(in.Quantity)
	// Namespaced so a staff key can never collide with a public reserve key.
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("reservation:op-convert:"+in.OrganizerID.String()+":"+key))
	buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte("buyer:"+id.String()))
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'held') ON CONFLICT(id) DO NOTHING`,
		id, in.OrganizerID, conv.Hold.ID, o.PerformanceID, in.TicketTypeID, buyer, in.Quantity, o.Price.Amount, total, o.Price.Currency)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	status := 201
	if code == 200 {
		status = 200
	}
	write(w, status, map[string]any{"reservation_id": id, "hold_id": conv.Hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": conv.Hold.ExpiresAt, "server_time": conv.Hold.ServerTime, "source_remaining": conv.SourceRemaining})
}

func (s *Server) deliveryEmail(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Token") != s.token {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid buyer"})
		return
	}
	var email string
	if err := s.db.QueryRowContext(r.Context(), `SELECT email FROM buyer_pii WHERE buyer_id=$1`, id).Scan(&email); err != nil {
		code, message := persistenceReadProblem(err)
		write(w, code, map[string]string{"error": message})
		return
	}
	write(w, 200, map[string]string{"email": email})
}
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, e := uuid.Parse(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	var status string
	e = s.db.QueryRowContext(r.Context(), `SELECT status FROM orders WHERE id=$1`, id).Scan(&status)
	if e != nil {
		code, message := persistenceReadProblem(e)
		if code != http.StatusNotFound {
			slog.Default().ErrorContext(r.Context(), "load order", "err", e)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	write(w, 200, map[string]any{"order_id": id, "status": status})
}
