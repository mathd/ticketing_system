package main

import (
	"testing"
	"time"

	"ticketing/services/access/internal/lifecyclejob"
	accessstore "ticketing/services/access/internal/store"
)

// An ordinary, valid pair of each kind — NOT the values compose used to default
// to. Those three are refused forever now (ai-review S5: they were active
// checked-in defaults, so they are published key material), and a fixture
// carrying one would prove the refusal rather than the loading it names.
//
// Distinct key MATERIAL between the two kinds is the point of ADR-021 §D4, not
// merely a distinct name — so these are two independent pairs, as production is.
const (
	localLifecycleSeed = "TSyq2Zhw6/F/ObLtFOzj85YnEFOjepo/inZ+qOwHn8I"
	localLifecyclePub  = "p63Q9dOq9cAoWHHxRGpPGcJTddQkucFxagmDcplkJ7s"
	localLifecycleKID  = "access-lifecycle/local-v1"
	localQRSeed        = "J/K6Ehl7hRVonf7ggcuiCizcIA8vy3lU8y2wWWFCmBY"
	localQRPub         = "IV1JHVOcYJjnZZtSo3WIjGBbh6a9mWskqy6UchI+6/E"
)

func setLifecycleEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envLifecycleSeed, localLifecycleSeed)
	t.Setenv(envLifecycleKID, localLifecycleKID)
	t.Setenv(envLifecyclePublicKeys, localLifecycleKID+"="+localLifecyclePub)
}

func TestLifecycleSignerRequiresItsOwnNamespace(t *testing.T) {
	setLifecycleEnv(t)
	keyring, err := lifecycleKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lifecycleSigner(keyring); err != nil {
		t.Fatalf("the documented local configuration does not load: %v", err)
	}

	// The QR key id must be unusable here, or a copy-paste could quietly put
	// credential signing and history rewriting under one key.
	t.Setenv(envLifecycleKID, "access-qr/local-v1")
	if _, err = lifecycleSigner(keyring); err == nil {
		t.Fatal("a QR key id was accepted for lifecycle signing")
	}
}

// A seed whose public key is not in the keyring would sign history nothing could
// verify — a failure that would otherwise surface only at the next audit, long
// after the trail it broke.
func TestLifecycleSignerMustBeVerifiableByItsOwnKeyring(t *testing.T) {
	setLifecycleEnv(t)
	t.Setenv(envLifecyclePublicKeys, "access-lifecycle/someone-else="+localLifecyclePub)
	keyring, err := lifecycleKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lifecycleSigner(keyring); err == nil {
		t.Fatal("a signing key absent from the keyring was accepted: it would sign history nothing can verify")
	}
}

// ADR-021 §D4's whole reason for a separate namespace: leaking the QR key must
// not also authorize rewriting history. Shared material would defeat that while
// looking correctly configured.
func TestLifecycleAndQRKeyMaterialAreDistinct(t *testing.T) {
	if localLifecycleSeed == localQRSeed {
		t.Fatal("the lifecycle and QR development seeds are identical: a leaked QR key would also sign the trail (ADR-021 §D4)")
	}
	if localLifecyclePub == localQRPub {
		t.Fatal("the lifecycle and QR development public keys are identical")
	}
}

func TestLifecycleKeyringRejectsQRMaterial(t *testing.T) {
	t.Setenv(envLifecyclePublicKeys, "access-qr/local-v1="+localQRPub)
	if _, err := lifecycleKeyring(); err == nil {
		t.Fatal("the lifecycle keyring accepted QR material")
	}
}

func TestCheckpointIntervalDefaultsToTheADRsSixtySeconds(t *testing.T) {
	t.Setenv(envCheckpointInterval, "")
	interval, err := checkpointInterval()
	if err != nil {
		t.Fatal(err)
	}
	if interval != lifecyclejob.DefaultInterval {
		t.Fatalf("checkpoint interval = %v with no configuration, want the %v default", interval, lifecyclejob.DefaultInterval)
	}
	if interval != 60*time.Second {
		t.Fatalf("checkpoint interval = %v, want 60s (ADR-021 §D3)", interval)
	}
	t.Setenv(envCheckpointInterval, "5s")
	if interval, err = checkpointInterval(); err != nil || interval != 5*time.Second {
		t.Fatalf("interval = %v err = %v", interval, err)
	}
	t.Setenv(envCheckpointInterval, "not-a-duration")
	if _, err = checkpointInterval(); err == nil {
		t.Fatal("a malformed interval was accepted")
	}
	t.Setenv(envCheckpointInterval, "-1s")
	if _, err = checkpointInterval(); err == nil {
		t.Fatal("a negative interval was accepted")
	}
}

func TestDegradedPolicyDefaults(t *testing.T) {
	t.Setenv(envFailureThreshold, "")
	t.Setenv(envFailureWindow, "")
	policy, err := lifecyclePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy != accessstore.DefaultPolicy() {
		t.Fatalf("policy = %+v, want %+v", policy, accessstore.DefaultPolicy())
	}
	t.Setenv(envFailureThreshold, "0")
	if _, err = lifecyclePolicy(); err == nil {
		t.Fatal("a zero threshold was accepted: it would flip every organizer on the first bad row")
	}
}

// The bound asks "is the worker dead", not "is it quick" — 2x a sub-second
// interval would fail an audit over a scheduling hiccup.
func TestMaxPendingAgeHasAFloor(t *testing.T) {
	if got := maxPendingAge(time.Second); got != 30*time.Second {
		t.Fatalf("maxPendingAge(1s) = %v, want the 30s floor", got)
	}
	if got := maxPendingAge(60 * time.Second); got != 120*time.Second {
		t.Fatalf("maxPendingAge(60s) = %v, want 2 intervals", got)
	}
}

// verify-lifecycle and seal-lifecycle-epoch build stores with no signer, so they
// are structurally incapable of writing to the trail (ADR-021 §D4/§D7).
func TestVerifyOnlyStoreCannotAppend(t *testing.T) {
	setLifecycleEnv(t)
	keyring, err := lifecycleKeyring()
	if err != nil {
		t.Fatal(err)
	}
	st := accessstore.New(nil, accessstore.Config{Keyring: keyring})
	if _, err = st.BackfillLifecycle(t.Context(), 1); err == nil {
		t.Fatal("a public-key-only store was willing to append to the trail")
	}
}

func TestCommandRegistryInvokesEveryAccessCallback(t *testing.T) {
	var invoked string
	withoutArgs := func(name string) func() error {
		return func() error { invoked = name; return nil }
	}
	withArgs := func(name string) func([]string) error {
		return func([]string) error { invoked = name; return nil }
	}
	callbacks := commandCallbacks{
		migrate: withoutArgs("migrate"), healthcheck: func() int { invoked = "healthcheck"; return 7 },
		lifecycleBackfill: withoutArgs("lifecycle-backfill"), verifyLifecycle: withoutArgs("verify-lifecycle"),
		sealLifecycleEpoch: withoutArgs("seal-lifecycle-epoch"), setLifecycleMode: withArgs("set-lifecycle-mode"),
		keygen: withoutArgs("keygen"), enrolScanner: withArgs("enrol-scanner"),
		revokeScanner: withArgs("revoke-scanner"), listScanners: withArgs("list-scanners"),
	}
	registry := commandRegistry(callbacks)
	names := []string{
		"migrate", "healthcheck", "lifecycle-backfill", "verify-lifecycle", "seal-lifecycle-epoch",
		"set-lifecycle-mode", "keygen", "enrol-scanner", "revoke-scanner", "list-scanners",
	}
	if len(registry) != len(names) {
		t.Fatalf("registry has %d commands, test names %d", len(registry), len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			invoked = ""
			got := execute([]string{name, "tail"}, callbacks, func() error {
				t.Fatal("server ran after a command was selected")
				return nil
			})
			wantExit := 0
			if name == "healthcheck" {
				wantExit = 7
			}
			if got.Name != name || got.Err != nil || got.ExitCode != wantExit {
				t.Fatalf("dispatch result = %+v, want %s with exit %d", got, name, wantExit)
			}
			if invoked != name {
				t.Fatalf("invoked %q, want %q", invoked, name)
			}
		})
	}
}

func TestSetLifecycleModeRejectsBadArguments(t *testing.T) {
	if err := setLifecycleMode([]string{"only-one"}); err == nil {
		t.Fatal("set-lifecycle-mode accepted the wrong number of arguments")
	}
	if err := setLifecycleMode([]string{"not-a-uuid", "normal", "ops"}); err == nil {
		t.Fatal("set-lifecycle-mode accepted a malformed organizer id")
	}
}
