package accessadmission

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TKT-498. The one strict reader of Access's admission summary. The exchange's pre-money
// check, the abandonment's two ordered reads and the replacement refund dispatch guard all
// call Read; each keeps its own eligibility rule. These cases pin the wire rule they share.

type recordedCall struct {
	method, url, key string
	internal         bool
	body             any
}

func fixedCaller(status int, body string, err error, calls *[]recordedCall) Caller {
	return func(_ context.Context, method, url, key string, in any, internal bool) (int, []byte, error) {
		*calls = append(*calls, recordedCall{method: method, url: url, key: key, internal: internal, body: in})
		return status, []byte(body), err
	}
}

// The read is one internal GET of the order's admission, scoped by the organizer in the query.
func TestReadAsksAccessForTheScopedOrderOnce(t *testing.T) {
	org, order := uuid.New(), uuid.New()
	var calls []recordedCall
	got, err := Read(context.Background(), fixedCaller(http.StatusOK, `{"admitted":true,"issued_count":2}`, nil, &calls),
		"http://access.test", order, org)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Admitted || got.IssuedCount != 2 {
		t.Fatalf("admission = %+v, want admitted with 2 issued", got)
	}
	if len(calls) != 1 {
		t.Fatalf("Read made %d calls, want 1", len(calls))
	}
	want := "http://access.test/internal/orders/" + order.String() + "/admission?organizer_id=" + org.String()
	c := calls[0]
	if c.method != http.MethodGet || c.url != want || c.key != "" || !c.internal || c.body != nil {
		t.Fatalf("call = %+v, want an internal GET of %s with no key or body", c, want)
	}
}

// Every answer that is not a well-formed 200 is ErrUnavailable: the reader never turns a
// failure to ask into a verdict. Each case changes one property of an otherwise valid answer.
func TestReadRefusesEveryAnswerThatIsNotAWellFormed200(t *testing.T) {
	const valid = `{"admitted":false,"issued_count":2}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"transport error", 0, "", errors.New("connection refused")},
		{"404", http.StatusNotFound, valid, nil},
		{"500", http.StatusInternalServerError, valid, nil},
		{"201", http.StatusCreated, valid, nil},
		{"empty body", http.StatusOK, ``, nil},
		{"not JSON", http.StatusOK, `admitted`, nil},
		{"array", http.StatusOK, `[]`, nil},
		{"missing admitted", http.StatusOK, `{"issued_count":2}`, nil},
		{"missing issued_count", http.StatusOK, `{"admitted":false}`, nil},
		{"null admitted", http.StatusOK, `{"admitted":null,"issued_count":2}`, nil},
		{"null issued_count", http.StatusOK, `{"admitted":false,"issued_count":null}`, nil},
		{"admitted as string", http.StatusOK, `{"admitted":"false","issued_count":2}`, nil},
		{"issued_count as string", http.StatusOK, `{"admitted":false,"issued_count":"2"}`, nil},
		{"fractional issued_count", http.StatusOK, `{"admitted":false,"issued_count":1.5}`, nil},
		{"negative issued_count", http.StatusOK, `{"admitted":false,"issued_count":-1}`, nil},
		{"issued_count above the order bound", http.StatusOK, `{"admitted":false,"issued_count":51}`, nil},
		{"unknown field", http.StatusOK, `{"admitted":false,"issued_count":2,"voided":true}`, nil},
		{"trailing value", http.StatusOK, valid + `{}`, nil},
		{"trailing garbage", http.StatusOK, valid + `x`, nil},
		// R2-3. Access reports an admission only for an issued ticket, so this is a contradiction,
		// not "zero tickets".
		{"admitted with no issued ticket", http.StatusOK, `{"admitted":true,"issued_count":0}`, nil},
		// R2-3. A decoder that kept the last of duplicate names would accept each case: its last value is valid.
		{"duplicate admitted", http.StatusOK, `{"admitted":true,"admitted":false,"issued_count":2}`, nil},
		{"duplicate issued_count", http.StatusOK, `{"admitted":false,"issued_count":0,"issued_count":2}`, nil},
		{"duplicate name spelled with an escape", http.StatusOK, `{"admitted":false,"issued_count":0,"issued_\u0063ount":2}`, nil},
		// R3-1. Member names match exactly. A case-insensitive decoder reads each of these as a
		// second value for one field, and the last value is valid.
		{"case variant of issued_count", http.StatusOK, `{"admitted":false,"issued_count":0,"Issued_Count":2}`, nil},
		{"case variant of admitted", http.StatusOK, `{"admitted":true,"ADMITTED":false,"issued_count":2}`, nil},
		{"case variant alone", http.StatusOK, `{"Admitted":false,"issued_count":2}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []recordedCall
			got, err := Read(context.Background(), fixedCaller(tc.status, tc.body, tc.err, &calls),
				"http://access.test", uuid.New(), uuid.New())
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Read = %+v, %v; want ErrUnavailable", got, err)
			}
			if got != (Admission{}) {
				t.Fatalf("a refused answer returned %+v, want the zero admission", got)
			}
		})
	}
}

// The bounds are inclusive: 0 and 50 are well-formed answers. Each caller decides what they
// mean; the reader does not.
func TestReadAcceptsTheBoundaryCounts(t *testing.T) {
	for _, body := range []string{`{"admitted":false,"issued_count":0}`, `{"issued_count":50,"admitted":true}`} {
		var calls []recordedCall
		if _, err := Read(context.Background(), fixedCaller(http.StatusOK, body, nil, &calls),
			"http://access.test", uuid.New(), uuid.New()); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

// Without a configured Access the reader asks nobody and answers ErrUnavailable.
func TestReadWithoutAnAccessURLAsksNobody(t *testing.T) {
	var calls []recordedCall
	_, err := Read(context.Background(), fixedCaller(http.StatusOK, `{"admitted":false,"issued_count":1}`, nil, &calls),
		"", uuid.New(), uuid.New())
	if !errors.Is(err, ErrUnavailable) || len(calls) != 0 {
		t.Fatalf("err=%v calls=%d, want ErrUnavailable and no call", err, len(calls))
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want it to say Access is not configured", err)
	}
}
