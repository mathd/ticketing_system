package main

import (
	"testing"
	"time"

	"ticketing/services/access/internal/lifecyclejob"
	accessstore "ticketing/services/access/internal/store"
)

// The compose defaults, which are also what a deployment inherits if it sets
// nothing. Distinct key MATERIAL from the QR keys is the point of ADR-021 §D4,
// not merely a distinct name.
const (
	localLifecycleSeed = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"
	localLifecyclePub  = "ebVWLo/mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ"
	localLifecycleKID  = "access-lifecycle/local-v1"
	localQRSeed        = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	localQRPub         = "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik"
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

func TestSubcommandsAreWired(t *testing.T) {
	for _, name := range []string{"migrate", "lifecycle-backfill", "verify-lifecycle", "seal-lifecycle-epoch", "set-lifecycle-mode"} {
		if _, ok := subcommands()[name]; !ok {
			t.Fatalf("subcommand %q is not wired", name)
		}
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
