package worklease

import (
	"fmt"
	"time"
)

const maxDuration = time.Duration(1<<63 - 1)

// ForBatch returns a lease that covers a sequential batch plus scheduling and
// database overhead. It rejects inputs that would wrap time.Duration.
func ForBatch(batch, callsPerItem int, callTimeout, margin time.Duration) (time.Duration, error) {
	switch {
	case batch <= 0:
		return 0, fmt.Errorf("batch must be positive")
	case callsPerItem <= 0:
		return 0, fmt.Errorf("calls per item must be positive")
	case callTimeout <= 0:
		return 0, fmt.Errorf("call timeout must be positive")
	case margin < 0:
		return 0, fmt.Errorf("margin must not be negative")
	}

	batchCalls := int64(batch)
	if int64(callsPerItem) > int64(maxDuration)/batchCalls {
		return 0, fmt.Errorf("batch call count overflows time.Duration")
	}
	batchCalls *= int64(callsPerItem)
	if batchCalls > int64(maxDuration-margin)/int64(callTimeout) {
		return 0, fmt.Errorf("batch duration overflows time.Duration")
	}
	return time.Duration(batchCalls)*callTimeout + margin, nil
}
