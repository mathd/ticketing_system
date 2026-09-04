package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"ticketing/services/commerce/internal/bulkrefund"
	"ticketing/services/commerce/internal/exchangesweep"
	"ticketing/services/commerce/internal/recovery"
	"ticketing/services/commerce/internal/reversal"
	"ticketing/services/commerce/internal/store"
)

type claimObservation struct {
	batch int
	lease time.Duration
	calls int
}

type recoveryClaimRecorder struct {
	recovery.Store
	claimObservation
}

func (r *recoveryClaimRecorder) ClaimStuckOrders(_ context.Context, batch int, lease time.Duration) ([]store.StuckOrder, error) {
	r.claimObservation = claimObservation{batch: batch, lease: lease, calls: r.calls + 1}
	return nil, nil
}

type cancellationClaimRecorder struct {
	bulkrefund.Store
	claimObservation
}

func (r *cancellationClaimRecorder) Runs(context.Context, int) ([]store.CancellationRun, error) {
	return nil, nil
}

func (r *cancellationClaimRecorder) Claim(_ context.Context, batch int, lease time.Duration) ([]store.CancellationWork, error) {
	r.claimObservation = claimObservation{batch: batch, lease: lease, calls: r.calls + 1}
	return nil, nil
}

func (r *cancellationClaimRecorder) CompleteRuns(context.Context) (int, error) { return 0, nil }

type reversalClaimRecorder struct {
	reversal.Store
	claimObservation
}

func (r *reversalClaimRecorder) Claim(_ context.Context, batch int, lease time.Duration) ([]store.ClaimedReversal, error) {
	r.claimObservation = claimObservation{batch: batch, lease: lease, calls: r.calls + 1}
	return nil, nil
}

type exchangeClaimRecorder struct {
	exchangesweep.Store
	claimObservation
}

func (r *exchangeClaimRecorder) Claim(_ context.Context, batch int, lease time.Duration) ([]store.ClaimedExchangeReversal, error) {
	r.claimObservation = claimObservation{batch: batch, lease: lease, calls: r.calls + 1}
	return nil, nil
}

func TestProductionWorkerConstructionPassesEachClaimItsBatchAndLease(t *testing.T) {
	config := workerConfig{
		recovery:     workerSettings{batch: 2},
		cancellation: workerSettings{batch: 3},
		reversal:     workerSettings{batch: 4},
		exchange:     workerSettings{batch: 5},
	}
	recoveryClient := &http.Client{Timeout: 7 * time.Second}
	apiClient := &http.Client{Timeout: 11 * time.Second}
	recoveryStore := &recoveryClaimRecorder{}
	cancellationStore := &cancellationClaimRecorder{}
	reversalStore := &reversalClaimRecorder{}
	exchangeStore := &exchangeClaimRecorder{}

	runners, err := constructWorkerRunners(config, recoveryClient, apiClient, workerDependencies{
		recoveryStore: recoveryStore, cancellationStore: cancellationStore,
		reversalStore: reversalStore, exchangeStore: exchangeStore,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runners.recovery.RunOnce(context.Background())
	runners.cancellation.RunOnce(context.Background())
	runners.reversal.RunOnce(context.Background())
	runners.exchange.RunOnce(context.Background())

	// The expected values independently encode the longest call chains: recovery 6,
	// cancellation 4, reversal 2, and exchange 1. Every lease adds a 60s margin.
	want := map[string]claimObservation{
		"recovery":     {batch: 2, lease: 144 * time.Second, calls: 1},
		"cancellation": {batch: 3, lease: 192 * time.Second, calls: 1},
		"reversal":     {batch: 4, lease: 148 * time.Second, calls: 1},
		"exchange":     {batch: 5, lease: 115 * time.Second, calls: 1},
	}
	got := map[string]claimObservation{
		"recovery": recoveryStore.claimObservation, "cancellation": cancellationStore.claimObservation,
		"reversal": reversalStore.claimObservation, "exchange": exchangeStore.claimObservation,
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("%s Claim = %#v, want %#v", name, got[name], expected)
		}
	}
}

func TestWorkerLeaseOverflowFailsStartup(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)
	for _, tc := range []struct {
		name string
		set  func(*workerConfig)
	}{
		{"recovery", func(c *workerConfig) { c.recovery.batch = maxInt }},
		{"cancellation", func(c *workerConfig) { c.cancellation.batch = maxInt }},
		{"reversal", func(c *workerConfig) { c.reversal.batch = maxInt }},
		{"exchange", func(c *workerConfig) { c.exchange.batch = maxInt }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := workerConfig{
				recovery: workerSettings{batch: 1}, cancellation: workerSettings{batch: 1},
				reversal: workerSettings{batch: 1}, exchange: workerSettings{batch: 1},
			}
			tc.set(&config)
			_, err := workerLeasesFor(config,
				&http.Client{Timeout: time.Nanosecond}, &http.Client{Timeout: time.Nanosecond})
			if err == nil || !strings.Contains(err.Error(), tc.name+" lease") {
				t.Fatalf("workerLeasesFor() error = %v, want %q lease overflow", err, tc.name)
			}
		})
	}
}

func TestWorkerLeasesRejectUnboundedClients(t *testing.T) {
	config := workerConfig{
		recovery: workerSettings{batch: 1}, cancellation: workerSettings{batch: 1},
		reversal: workerSettings{batch: 1}, exchange: workerSettings{batch: 1},
	}
	for _, tc := range []struct {
		name                      string
		recoveryClient, apiClient *http.Client
	}{
		{"missing recovery client", nil, &http.Client{Timeout: time.Second}},
		{"unbounded recovery client", &http.Client{}, &http.Client{Timeout: time.Second}},
		{"missing API client", &http.Client{Timeout: time.Second}, nil},
		{"unbounded API client", &http.Client{Timeout: time.Second}, &http.Client{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := workerLeasesFor(config, tc.recoveryClient, tc.apiClient); err == nil {
				t.Fatal("workerLeasesFor() accepted a client without a deadline")
			}
		})
	}
}

var workerEnvironment = []string{
	"OUTBOX_DRAIN_INTERVAL",
	"OUTBOX_DRAIN_BATCH",
	"RECOVERY_INTERVAL",
	"RECOVERY_BATCH",
	"CANCELLATION_REFUND_INTERVAL",
	"CANCELLATION_REFUND_BATCH",
	"REFUND_REVERSAL_INTERVAL",
	"REFUND_REVERSAL_BATCH",
	"EXCHANGE_REVERSAL_INTERVAL",
	"EXCHANGE_REVERSAL_BATCH",
}

func unsetWorkerEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range workerEnvironment {
		t.Setenv(name, "test cleanup placeholder")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func TestWorkerConfigDefaults(t *testing.T) {
	unsetWorkerEnvironment(t)

	got, err := workerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := workerConfig{
		outbox:       workerSettings{interval: 5 * time.Second, batch: 32},
		recovery:     workerSettings{interval: 30 * time.Second, batch: 16},
		cancellation: workerSettings{interval: 10 * time.Second, batch: 8},
		reversal:     workerSettings{interval: time.Minute, batch: 16},
		exchange:     workerSettings{interval: time.Minute, batch: 16},
	}
	if got != want {
		t.Fatalf("worker defaults = %#v, want %#v", got, want)
	}
}

func TestWorkerConfigReadsEveryOverride(t *testing.T) {
	unsetWorkerEnvironment(t)
	for name, value := range map[string]string{
		"OUTBOX_DRAIN_INTERVAL":        "1s",
		"OUTBOX_DRAIN_BATCH":           "2",
		"RECOVERY_INTERVAL":            "3s",
		"RECOVERY_BATCH":               "4",
		"CANCELLATION_REFUND_INTERVAL": "5s",
		"CANCELLATION_REFUND_BATCH":    "6",
		"REFUND_REVERSAL_INTERVAL":     "7s",
		"REFUND_REVERSAL_BATCH":        "8",
		"EXCHANGE_REVERSAL_INTERVAL":   "9s",
		"EXCHANGE_REVERSAL_BATCH":      "10",
	} {
		t.Setenv(name, value)
	}

	got, err := workerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := workerConfig{
		outbox:       workerSettings{interval: time.Second, batch: 2},
		recovery:     workerSettings{interval: 3 * time.Second, batch: 4},
		cancellation: workerSettings{interval: 5 * time.Second, batch: 6},
		reversal:     workerSettings{interval: 7 * time.Second, batch: 8},
		exchange:     workerSettings{interval: 9 * time.Second, batch: 10},
	}
	if got != want {
		t.Fatalf("worker overrides = %#v, want %#v", got, want)
	}
}

func TestWorkerConfigRefusesInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name, environment, value string
	}{
		{"malformed duration", "RECOVERY_INTERVAL", "not-a-duration"},
		{"zero duration", "RECOVERY_INTERVAL", "0s"},
		{"negative duration", "RECOVERY_INTERVAL", "-1s"},
		{"malformed batch", "RECOVERY_BATCH", "many"},
		{"zero batch", "RECOVERY_BATCH", "0"},
		{"negative batch", "RECOVERY_BATCH", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetWorkerEnvironment(t)
			t.Setenv(tc.environment, tc.value)

			_, err := workerConfigFromEnv()
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.environment, tc.value)
			}
			if message := err.Error(); !strings.Contains(message, tc.environment) {
				t.Fatalf("error %q does not name %s", message, tc.environment)
			} else if strings.Contains(message, tc.value) {
				t.Fatalf("error %q echoes the rejected value", message)
			}
		})
	}
}
