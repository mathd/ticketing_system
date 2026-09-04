package httpx

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestReadResponseBodyRefusesAnEndlessBody(t *testing.T) {
	if _, err := ReadResponseBody(endlessReader{}, 1<<10); !errors.Is(err, ErrResponseTooLarge) {
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

func TestWriteJSONNoStoreOwnsTheCachePolicy(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Cache-Control", "public, max-age=300")

	WriteJSONNoStore(recorder, http.StatusCreated, map[string]string{"status": "created"})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Body.String(); got != "{\"status\":\"created\"}\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteJSONDefaultNoStorePreservesAnExplicitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, initial, want string
	}{
		{name: "missing policy", want: "no-store"},
		{name: "explicit public policy", initial: "public, max-age=5", want: "public, max-age=5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if tc.initial != "" {
				recorder.Header().Set("Cache-Control", tc.initial)
			}

			WriteJSONDefaultNoStore(recorder, http.StatusOK, struct{}{})

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != tc.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tc.want)
			}
			if got := recorder.Body.String(); got != "{}\n" {
				t.Fatalf("body = %q, want an encoded empty object", got)
			}
		})
	}
}

func TestJSONWritersPreserveCommittedResponseWhenEncodingFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(http.ResponseWriter, int, any)
	}{
		{name: "forced no-store", write: WriteJSONNoStore},
		{name: "default no-store", write: WriteJSONDefaultNoStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()

			tc.write(recorder, http.StatusAccepted, make(chan int))

			if recorder.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("body = %q, want no bytes from an unsupported value", recorder.Body.String())
			}
		})
	}
}
