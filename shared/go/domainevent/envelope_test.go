package domainevent_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/shared/domainevent"
)

// The fixtures below are hand-written JSON, never marshalled from Envelope.
// ADR-017 §5b′ is explicit that this is the only kind that can fail: a fixture
// built from the type under test encodes the compatibility it claims to prove.

// A future variant may rename fields, change their types, or restructure `data`
// entirely — that is what a schema bump MEANS (ADR-017 §3). DecodeEnvelope must
// therefore hand back `data` untouched, so a consumer physically cannot judge a
// variant before it has dispatched on `schema`. This is the structural form of
// §5b′: the rule is enforced by the type, not by remembering to obey it.
func TestDecodeEnvelopeLeavesDataRawUntilSchemaDispatch(t *testing.T) {
	// The expected bytes are written out literally rather than derived, for the
	// same reason the fixtures are: anything that recomputes the expectation
	// from the input can only ever agree with itself.
	cases := map[string]string{
		"renamed keys and changed types": `{"order_ref":"ref-1","qty":"2"}`,
		"data is an array":               `[1,2,3]`,
		"data is a scalar":               `"not-an-object"`,
		"data is null":                   `null`,
		"deeply restructured":            `{"envelope":{"nested":{"beyond":"recognition"}}}`,
	}
	for name, wantData := range cases {
		t.Run(name, func(t *testing.T) {
			raw := `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.commerce.order.completed","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":99,"data":` + wantData + `}`
			env, err := domainevent.DecodeEnvelope([]byte(raw))
			if err != nil {
				t.Fatalf("a future variant must decode, not fail: %v", err)
			}
			if env.Schema != 99 {
				t.Fatalf("schema = %d, want 99", env.Schema)
			}
			if string(env.Data) != wantData {
				t.Fatalf("data = %s, want the bytes untouched: %s", env.Data, wantData)
			}
		})
	}
}

// ADR-017 §5b: the poison/skew line has a BOTTOM end. Schema numbers start at 1
// and only climb, so `schema <= 0` is not a variant from the future — it is an
// envelope that omitted `schema` (ADR-009 §5 requires it) or a producer bug, and
// no binary will ever apply it. Without this the parking rule contradicts
// itself, and one malformed message is a free denial of service against any
// consumer that latches readiness on the future.
func TestDecodeEnvelopeRejectsNonPositiveSchema(t *testing.T) {
	cases := map[string]string{
		"schema omitted":  `{"id":"11111111-1111-4111-8111-111111111111","type":"t","data":{}}`,
		"schema zero":     `{"id":"11111111-1111-4111-8111-111111111111","type":"t","schema":0,"data":{}}`,
		"schema negative": `{"id":"11111111-1111-4111-8111-111111111111","type":"t","schema":-1,"data":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := domainevent.DecodeEnvelope([]byte(body)); !errors.Is(err, domainevent.ErrInvalidSchema) {
				t.Fatalf("err = %v, want ErrInvalidSchema", err)
			}
		})
	}
}

// A broken envelope still has to be loggable. Every consumer logs the event id
// and the offending schema on the poison path, so the fields decoded before the
// rule fired come back with the error rather than being swallowed by a zero
// value.
func TestDecodeEnvelopeReturnsDecodedFieldsAlongsideInvalidSchema(t *testing.T) {
	body := `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.commerce.order.completed","schema":-7,"data":{}}`
	env, err := domainevent.DecodeEnvelope([]byte(body))
	if !errors.Is(err, domainevent.ErrInvalidSchema) {
		t.Fatalf("err = %v, want ErrInvalidSchema", err)
	}
	if env.ID != uuid.MustParse("11111111-1111-4111-8111-111111111111") {
		t.Fatalf("id = %s, want the decoded id for the log line", env.ID)
	}
	if env.Schema != -7 || env.Type != "platform.commerce.order.completed" {
		t.Fatalf("env = %+v, want the offending schema and type preserved", env)
	}
}

// Malformed JSON and a broken schema are different dispositions at some
// consumers (access publishes invalid_json vs invalid_contract), so they must
// stay distinguishable. A malformed body is NOT ErrInvalidSchema.
func TestDecodeEnvelopeMalformedJSONIsDistinctFromInvalidSchema(t *testing.T) {
	cases := map[string]string{
		"truncated":       `{"id":"11111111-1111-4111-8111-111111111111","schema":`,
		"not an object":   `[1,2,3]`,
		"id not a uuid":   `{"id":"not-a-uuid","type":"t","schema":1,"data":{}}`,
		"schema a string": `{"id":"11111111-1111-4111-8111-111111111111","type":"t","schema":"1","data":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domainevent.DecodeEnvelope([]byte(body))
			if err == nil {
				t.Fatal("malformed envelope decoded cleanly")
			}
			if errors.Is(err, domainevent.ErrInvalidSchema) {
				t.Fatalf("err = %v, want a decode error distinguishable from ErrInvalidSchema", err)
			}
		})
	}
}

// The envelope's JSON key order is contract (ADR-009 §5), and encoding/json
// emits struct fields in declaration order — so field order in the type IS the
// wire. The per-service goldens prove today's emitters are unchanged; this pins
// the shared declaration itself, which is what they all now route through.
func TestEnvelopeMarshalsInADR009FieldOrder(t *testing.T) {
	body, err := json.Marshal(domainevent.Envelope[map[string]int]{
		ID:         uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Type:       "platform.test.subject",
		OccurredAt: time.Date(2026, 7, 20, 12, 34, 56, 123456789, time.UTC),
		Schema:     1,
		Data:       map[string]int{"n": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"11111111-1111-4111-8111-111111111111","type":"platform.test.subject","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"n":1}}`
	if string(body) != want {
		t.Fatalf("envelope wire shape changed\n got: %s\nwant: %s", body, want)
	}
}
