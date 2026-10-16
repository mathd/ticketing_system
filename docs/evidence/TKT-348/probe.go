package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/api"
	"ticketing/services/inventory/internal/store"
)

const repetitions = 6

type httpObservation struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

type claimRow struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	Quantity    int32      `json:"quantity"`
	Returned    int32      `json:"returned_quantity"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Idempotency string     `json:"idempotency_key"`
}

type observation struct {
	Repetition            int             `json:"repetition"`
	Organizer             string          `json:"organizer_id"`
	Slot                  string          `json:"slot_id"`
	Capacity              int32           `json:"capacity"`
	OperationalHoldID     string          `json:"operational_hold_id"`
	FullPoolPublicRequest httpObservation `json:"full_pool_public_request"`
	Release               struct {
		Quantity  int32  `json:"quantity"`
		Status    string `json:"status"`
		Remaining int32  `json:"remaining"`
	} `json:"operational_release"`
	PublicA  httpObservation `json:"public_request_a"`
	PublicB  httpObservation `json:"public_request_b"`
	Claims   []claimRow      `json:"claim_rows"`
	Counters struct {
		Confirmed             int32  `json:"confirmed_quantity"`
		LiveHeldAndFinalizing int32  `json:"live_held_and_finalizing_quantity"`
		Capacity              int32  `json:"capacity"`
		TargetCapacity        *int32 `json:"target_capacity"`
		InvariantHolds        bool   `json:"confirmed_plus_live_held_finalizing_le_capacity"`
	} `json:"pool_counters"`
}

type report struct {
	Status        string        `json:"status"`
	EvidenceNote  string        `json:"evidence_note"`
	RunnerCommand string        `json:"runner_command"`
	ProbeCommand  string        `json:"probe_command"`
	Repetitions   int           `json:"repetitions"`
	Observations  []observation `json:"observations"`
}

func main() {
	if len(os.Args) != 2 {
		fatalf("usage: probe OUTPUT.json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	base, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		fatalf("parse DATABASE_URL: %v", err)
	}
	admin, err := sql.Open("pgx", base.String())
	if err != nil {
		fatalf("open PostgreSQL admin connection: %v", err)
	}
	defer admin.Close()

	out := report{
		Status: "observed", EvidenceNote: "Generated only after all six real router/store repetitions pass.",
		RunnerCommand: os.Getenv("TKT348_RUNNER_COMMAND"), ProbeCommand: "go run ./cmd/tkt348probe <output-path>",
		Repetitions: repetitions,
	}
	for n := 1; n <= repetitions; n++ {
		got, err := runOne(ctx, base, admin, n)
		if err != nil {
			fatalf("repetition %d: %v", n, err)
		}
		out.Observations = append(out.Observations, got)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatalf("encode result: %v", err)
	}
	if err = os.WriteFile(os.Args[1], append(data, '\n'), 0o600); err != nil {
		fatalf("write result: %v", err)
	}
}

func runOne(ctx context.Context, base *url.URL, admin *sql.DB, n int) (observation, error) {
	var out observation
	out.Repetition = n
	out.Organizer, out.Slot = uuid.NewString(), uuid.NewString()
	out.Capacity = 5
	schema := "tkt348_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		return out, fmt.Errorf("create schema: %w", err)
	}
	defer func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()

	dbURL := *base
	query := dbURL.Query()
	query.Set("search_path", schema)
	dbURL.RawQuery = query.Encode()
	db, err := sql.Open("pgx", dbURL.String())
	if err != nil {
		return out, fmt.Errorf("open isolated database: %w", err)
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return out, fmt.Errorf("run actual Inventory migrations: %w", err)
	}
	st := store.New(db, 10*time.Minute)
	org, slot := uuid.MustParse(out.Organizer), uuid.MustParse(out.Slot)
	if err := st.Provision(ctx, uuid.New(), slot, org, out.Capacity); err != nil {
		return out, fmt.Errorf("provision GA pool: %w", err)
	}
	op, replay, err := st.PlaceOperationalHold(ctx, org, slot, out.Capacity, "house", "tkt-348-spike", "probe", "capacity capture probe", uuid.NewString())
	if err != nil || replay {
		return out, fmt.Errorf("place operational hold: replay=%t err=%v", replay, err)
	}
	out.OperationalHoldID = op.ID.String()
	handler := api.New(st, "", nil).Router(slog.New(slog.NewTextHandler(io.Discard, nil)), true)

	full := publicHold(ctx, handler, org, slot, "full-pool-"+uuid.NewString())
	if full.Status != http.StatusConflict || !strings.Contains(full.Body, "insufficient capacity") {
		return out, fmt.Errorf("full-pool public request returned %d %s, want capacity 409", full.Status, full.Body)
	}
	out.FullPoolPublicRequest = full

	released, replay, err := st.ReleaseOperational(ctx, org, op.ID, 1, "probe", "release one unit", uuid.NewString())
	if err != nil || replay || released.Quantity != out.Capacity-1 || released.Status != "held" {
		return out, fmt.Errorf("release one operational unit: result=%+v replay=%t err=%v", released, replay, err)
	}
	out.Release.Quantity, out.Release.Status, out.Release.Remaining = 1, released.Status, released.Quantity

	// These awaited requests impose the stated order. There is no sleep and no claim
	// that they measure a scheduler race or public-vs-waitlist win frequency.
	out.PublicA = publicHold(ctx, handler, org, slot, "public-a-"+uuid.NewString())
	if out.PublicA.Status != http.StatusCreated {
		return out, fmt.Errorf("public request A returned %d %s, want 201", out.PublicA.Status, out.PublicA.Body)
	}
	out.PublicB = publicHold(ctx, handler, org, slot, "public-b-"+uuid.NewString())
	if out.PublicB.Status != http.StatusConflict || !strings.Contains(out.PublicB.Body, "insufficient capacity") {
		return out, fmt.Errorf("public request B returned %d %s, want capacity 409", out.PublicB.Status, out.PublicB.Body)
	}

	var target sql.NullInt32
	if err := db.QueryRowContext(ctx, `SELECT confirmed_quantity, capacity, target_capacity FROM inventory_pools WHERE slot_id=$1`, slot).
		Scan(&out.Counters.Confirmed, &out.Counters.Capacity, &target); err != nil {
		return out, fmt.Errorf("read pool counters: %w", err)
	}
	if target.Valid {
		out.Counters.TargetCapacity = &target.Int32
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND ((status='held' AND (expires_at IS NULL OR expires_at > now())) OR status='finalizing')`, slot).
		Scan(&out.Counters.LiveHeldAndFinalizing); err != nil {
		return out, fmt.Errorf("read live held/finalizing counter: %w", err)
	}
	out.Counters.InvariantHolds = int64(out.Counters.Confirmed)+int64(out.Counters.LiveHeldAndFinalizing) <= int64(out.Counters.Capacity)
	if !out.Counters.InvariantHolds {
		return out, fmt.Errorf("capacity invariant failed: confirmed=%d live=%d capacity=%d", out.Counters.Confirmed, out.Counters.LiveHeldAndFinalizing, out.Counters.Capacity)
	}
	rows, err := db.QueryContext(ctx, `SELECT id,claim_kind,status,quantity,returned_quantity,expires_at,idempotency_key FROM claims WHERE pool_id=$1 ORDER BY created_at,id`, slot)
	if err != nil {
		return out, fmt.Errorf("read claim rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row claimRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Status, &row.Quantity, &row.Returned, &row.ExpiresAt, &row.Idempotency); err != nil {
			return out, fmt.Errorf("scan claim row: %w", err)
		}
		out.Claims = append(out.Claims, row)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate claim rows: %w", err)
	}
	return out, nil
}

func publicHold(ctx context.Context, handler http.Handler, org, slot uuid.UUID, key string) httpObservation {
	ticketType := uuid.New()
	body, _ := json.Marshal(map[string]any{
		"organizer_id": org, "slot_id": slot, "ticket_type_id": ticketType,
		"quantity": 1, "unit_amount": 0, "currency": "EUR",
	})
	req := httptest.NewRequest(http.MethodPost, "/holds", strings.NewReader(string(body))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return httpObservation{Status: response.Code, Body: response.Body.String()}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
