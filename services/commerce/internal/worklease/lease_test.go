package worklease

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestForBatchUsesEverySizingInput(t *testing.T) {
	const (
		batch        = 3
		callsPerItem = 2
		callTimeout  = 5 * time.Second
		margin       = 7 * time.Second
	)
	got, err := ForBatch(batch, callsPerItem, callTimeout, margin)
	if err != nil {
		t.Fatal(err)
	}
	if want := 37 * time.Second; got != want {
		t.Fatalf("ForBatch() = %s, want %s", got, want)
	}
}

func TestForBatchRejectsInvalidAndOverflowingInputs(t *testing.T) {
	for _, tc := range []struct {
		name                string
		batch, callsPerItem int
		callTimeout, margin time.Duration
		wantError           string
	}{
		{"zero batch", 0, 1, time.Second, 0, "batch must be positive"},
		{"zero calls", 1, 0, time.Second, 0, "calls per item must be positive"},
		{"zero timeout", 1, 1, 0, 0, "call timeout must be positive"},
		{"negative margin", 1, 1, time.Second, -1, "margin must not be negative"},
		{"duration overflow", 2, 1, maxDuration, 0, "batch duration overflows"},
		{"margin leaves no room", 1, 1, time.Nanosecond, maxDuration, "batch duration overflows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ForBatch(tc.batch, tc.callsPerItem, tc.callTimeout, tc.margin)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ForBatch() error = %v, want text %q", err, tc.wantError)
			}
		})
	}
}

func TestForBatchRejectsCallCountOverflowWhenIntCanReachIt(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("two int arguments cannot overflow int64 on this architecture")
	}
	_, err := ForBatch(int(^uint(0)>>1), 2, time.Nanosecond, 0)
	if err == nil || !strings.Contains(err.Error(), "batch call count overflows") {
		t.Fatalf("ForBatch() error = %v, want call-count overflow", err)
	}
}

func TestForBatchAcceptsLargeRepresentableIntProduct(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	callsPerItem := 1
	if strconv.IntSize == 32 {
		// This is the product the old test wrongly called an overflow. Two
		// positive int32 arguments cannot overflow int64.
		callsPerItem = 2
	}
	got, err := ForBatch(maxInt, callsPerItem, time.Nanosecond, 0)
	if err != nil {
		t.Fatalf("ForBatch() rejected a representable duration: %v", err)
	}
	if want := time.Duration(int64(maxInt) * int64(callsPerItem)); got != want {
		t.Fatalf("ForBatch() = %s, want %s", got, want)
	}
}
