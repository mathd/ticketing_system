package httpx

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// The boundary is the whole point, so it is tested as a boundary: exactly the
// limit is accepted, exactly one byte more is refused. A test that only fed a
// hugely oversized body would pass against an off-by-one ceiling.
func TestReadResponseBodyBoundary(t *testing.T) {
	const max = 64
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"one byte under the limit", max - 1, false},
		{"exactly the limit", max, false},
		{"one byte over the limit", max + 1, true},
		{"far over the limit", max * 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), tc.size)
			got, err := ReadResponseBody(bytes.NewReader(body), max)
			if tc.wantErr {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("want ErrResponseTooLarge, got %v", err)
				}
				// An oversize body must not come back truncated: a caller that
				// ignored the error would otherwise classify partial evidence.
				if got != nil {
					t.Fatalf("want no body on refusal, got %d bytes", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.size {
				t.Fatalf("want %d bytes, got %d", tc.size, len(got))
			}
		})
	}
}

// A reader that never ends is the case a duration timeout does not cover. The
// helper must return from it, which io.ReadAll alone would not do.
func TestReadResponseBodyRefusesAnEndlessBody(t *testing.T) {
	endless := endlessReader{}
	if _, err := ReadResponseBody(endless, 1<<10); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestReadResponseBodyRejectsNonPositiveLimit(t *testing.T) {
	for _, max := range []int64{0, -1} {
		if _, err := ReadResponseBody(strings.NewReader("x"), max); err == nil {
			t.Fatalf("limit %d: want an error, got nil", max)
		}
	}
}

// A transport error must surface as itself, not as a size refusal: the two
// carry opposite meanings for a caller classifying a payment side effect.
func TestReadResponseBodyPropagatesReadErrors(t *testing.T) {
	want := errors.New("connection reset")
	_, err := ReadResponseBody(errReader{err: want}, 1<<10)
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Fatal("a transport failure must not be reported as an oversize body")
	}
}

type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = endlessReader{}
