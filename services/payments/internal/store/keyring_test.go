package store

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// The keyring is the whole of TKT-56 Slice 4's configuration surface, so its
// validation is unit-tested with hand-written literals rather than values built by
// the constructor under test: a fixture assembled from Keyring's own output could
// only ever express rings Keyring already accepts, which is the compatibility it
// claims to prove (ADR-032 §Slice 4, and the fixture rule in quality-practices §1).
func TestNewKeyringRejectsInvalidConfiguration(t *testing.T) {
	const good = "0123456789abcdef" // exactly the 16-byte floor main.go already enforces
	for _, tc := range []struct {
		name       string
		activeKID  string
		activeKey  string
		historical string
		wantErr    string
	}{
		{name: "missing active kid", activeKID: "", activeKey: good, wantErr: "active"},
		{name: "missing active key", activeKID: "v2", activeKey: "", wantErr: "active"},
		{name: "short active key", activeKID: "v2", activeKey: "tooshort", wantErr: "16"},
		{name: "invalid active kid charset", activeKID: "v2 spaced", activeKey: good, wantErr: "key id"},
		{name: "kid with list delimiter", activeKID: "v2=x", activeKey: good, wantErr: "key id"},
		{name: "kid with comma", activeKID: "v2,x", activeKey: good, wantErr: "key id"},
		{name: "kid with newline", activeKID: "v2\nx", activeKey: good, wantErr: "key id"},
		{name: "historical entry without separator", activeKID: "v2", activeKey: good, historical: "v1", wantErr: "entry"},
		{name: "historical empty kid", activeKID: "v2", activeKey: good, historical: "=MDEyMzQ1Njc4OWFiY2RlZg", wantErr: "key id"},
		{name: "historical empty value", activeKID: "v2", activeKey: good, historical: "v1=", wantErr: "entry"},
		{name: "historical bad base64", activeKID: "v2", activeKey: good, historical: "v1=not!base64!", wantErr: "base64"},
		{name: "historical padded base64 is not raw", activeKID: "v2", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZg==", wantErr: "base64"},
		{name: "historical short secret", activeKID: "v2", activeKey: good, historical: "v1=c2hvcnQ", wantErr: "16"},
		{name: "duplicate historical kid", activeKID: "v3", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZmc,v1=MDEyMzQ1Njc4OWFiY2RlZmg", wantErr: "duplicate"},
		{name: "historical kid duplicates active", activeKID: "v1", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZmc", wantErr: "duplicate"},
		// Two kids sharing one secret let a database writer holding NO secret relabel a
		// row's key_id between them and still pass verification, because key_id is not
		// inside canonical v1 (see canonical(): it ends at the payload) and the signature
		// therefore does not bind it. That is era misattribution, not content forgery —
		// it corrupts retirement accounting and the unknown-key contract. Rejected at
		// construction because that is the only place kids are resolved.
		{name: "duplicate secret material under two kids", activeKID: "v2", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZg", wantErr: "same HMAC key"},
		// HMAC does not sign with the key you configured: RFC 2104 zero-pads a short key
		// up to the block size and replaces an oversized one with its digest. So these
		// two configurations LOOK distinct byte-for-byte and sign identically — which is
		// the alias the rule above exists to reject. A raw-bytes duplicate check passes
		// them both, leaving key_id relabelling possible under a valid-looking config.
		// "0123456789abcdef" + a trailing NUL:
		{name: "trailing-NUL alias of the active key", activeKID: "v2", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZgA", wantErr: "same HMAC key"},
		// Stray and doubled commas are malformed input, not whitespace to be skipped:
		// the stated contract is that a malformed ring refuses startup.
		{name: "trailing comma", activeKID: "v2", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZmc,", wantErr: "empty entry"},
		{name: "doubled comma", activeKID: "v2", activeKey: good, historical: "v1=MDEyMzQ1Njc4OWFiY2RlZmc,,v0=MDEyMzQ1Njc4OWFiY2RlZmg", wantErr: "empty entry"},
		{name: "comma only", activeKID: "v2", activeKey: good, historical: ",", wantErr: "empty entry"},
		// Whitespace-only is NOT "unset": treating it as such would let a typo boot a
		// single-key ring and leave pre-rotation history unverifiable, with no error.
		{name: "whitespace-only historical value", activeKID: "v2", activeKey: good, historical: "   ", wantErr: "empty entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ring, err := NewKeyring(tc.activeKID, []byte(tc.activeKey), tc.historical)
			if err == nil {
				t.Fatalf("expected error containing %q, got a valid keyring", tc.wantErr)
			}
			if ring != nil {
				t.Fatal("a rejected keyring must be nil, not partially built")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), tc.activeKey) && tc.activeKey != "" {
				t.Fatalf("error echoes secret material: %q", err)
			}
		})
	}
}

func TestNewKeyringAcceptsActiveAndHistorical(t *testing.T) {
	ring, err := NewKeyring("v2", []byte("0123456789abcdef"), "v1=MDEyMzQ1Njc4OWFiY2RlZmc, v0=MDEyMzQ1Njc4OWFiY2RlZmg")
	if err != nil {
		t.Fatalf("valid keyring rejected: %v", err)
	}
	if ring.ActiveKeyID() != "v2" {
		t.Fatalf("active kid = %q, want v2", ring.ActiveKeyID())
	}
	for _, kid := range []string{"v2", "v1", "v0"} {
		if _, err := ring.keyFor(kid); err != nil {
			t.Fatalf("ring should verify under %q: %v", kid, err)
		}
	}
	if _, err := ring.keyFor("v9"); err == nil {
		t.Fatal("unknown kid must be an error, never a skip or a fallback to active")
	}
}

// An empty historical list is the deployed configuration today (compose sets only
// the active pair), so it must keep working unchanged.
func TestNewKeyringWithNoHistoricalKeys(t *testing.T) {
	ring, err := NewKeyring("local-v1", []byte("local-development-journal-key"), "")
	if err != nil {
		t.Fatalf("single-key ring rejected: %v", err)
	}
	if ring.ActiveKeyID() != "local-v1" {
		t.Fatalf("active kid = %q", ring.ActiveKeyID())
	}
	if _, err := ring.keyFor("local-v1"); err != nil {
		t.Fatalf("active key must be a member of its own ring: %v", err)
	}
}

// The ring copies caller-owned bytes: a caller that reuses or zeroes its buffer
// after construction must not be able to change what the journal signs with.
func TestNewKeyringCopiesSecrets(t *testing.T) {
	secret := []byte("0123456789abcdef")
	ring, err := NewKeyring("v1", secret, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := ring.keyFor("v1")
	if err != nil {
		t.Fatal(err)
	}
	stored := string(before)
	for i := range secret {
		secret[i] = 'x'
	}
	after, err := ring.keyFor("v1")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != stored {
		t.Fatal("mutating the caller's slice changed the ring's key material")
	}
}

// An oversized key is replaced by its SHA-256 before signing, so a >block-size key and
// its digest are the same key to HMAC. Separate from the table because the alias has to
// be computed rather than written as a literal.
func TestNewKeyringRejectsOversizedKeyAliasingItsDigest(t *testing.T) {
	long := make([]byte, sha256.BlockSize+1)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	digest := sha256.Sum256(long)

	// Sanity: the two really do sign identically. If this ever stops holding, the
	// rejection below is guarding nothing and should be revisited, not deleted.
	msg := []byte("entry hash stand-in")
	if !hmac.Equal(sign(long, msg), sign(digest[:], msg)) {
		t.Fatal("premise broken: an oversized key no longer signs as its digest")
	}

	_, err := NewKeyring("v2", long, "v1="+base64.RawStdEncoding.EncodeToString(digest[:]))
	if err == nil {
		t.Fatal("a historical key that is the active key's HMAC-equivalent digest was accepted")
	}
	if !strings.Contains(err.Error(), "same HMAC key") {
		t.Fatalf("error %q does not identify the alias", err)
	}
}

// TKT-117. The fingerprint's PROPERTIES live here, not in the cmd package's logging test.
// They are properties of the derivation, so testing them through a logger would compare
// timestamped JSON lines — which is how the first attempt at this test both failed on
// correct behaviour AND passed a different assertion for a reason unrelated to keys
// (docs/learnings: check *why* a test is red).
func TestActiveKeyFingerprint(t *testing.T) {
	fingerprint := func(t *testing.T, key []byte) string {
		t.Helper()
		ring, err := NewKeyring("local-v1", key, "")
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}
		return ring.ActiveKeyFingerprint()
	}

	// The mistake this whole ticket exists for: JOURNAL_SIGNING_KEY is RAW, but the runbook
	// teaches base64 one step earlier, and pasting the encoded text into the raw variable
	// passes every validation. The fingerprint is the only thing that can reveal it.
	t.Run("a base64-pasted key differs from the raw key it encodes", func(t *testing.T) {
		raw := []byte("tkt117-distinctive-journal-secret")
		pasted := []byte(base64.RawStdEncoding.EncodeToString(raw))
		if fingerprint(t, raw) == fingerprint(t, pasted) {
			t.Fatal("raw and base64-pasted keys fingerprint the same; the paste error stays invisible")
		}
	})

	// Computed over the EFFECTIVE HMAC key (plan-final D1). RFC 2104 replaces an
	// over-block-size key with its digest, so these two configurations sign journal entries
	// IDENTICALLY. A fingerprint that distinguished them would tell an operator two
	// interchangeable keys are different — the opposite of the diagnostic's job.
	t.Run("a long key and its digest fingerprint the same, because they sign the same", func(t *testing.T) {
		long := bytes.Repeat([]byte("k"), sha256.BlockSize+1)
		sum := sha256.Sum256(long)
		if fingerprint(t, long) != fingerprint(t, sum[:]) {
			t.Fatal("HMAC-equivalent keys must fingerprint identically: they verify the same history")
		}
	})

	// Shape: 8 lowercase hex characters, and never the key itself in any form.
	t.Run("is eight lowercase hex characters and reveals no key material", func(t *testing.T) {
		key := []byte("tkt117-distinctive-journal-secret")
		got := fingerprint(t, key)
		if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(got) {
			t.Fatalf("fingerprint %q is not 8 lowercase hex characters", got)
		}
		if strings.Contains(got, string(key)) || strings.Contains(string(key), got) {
			t.Fatalf("fingerprint %q overlaps the key material", got)
		}
	})

	// The domain string is what keeps this out of the journal's signing space, so a change
	// to it must be a deliberate versioned act, not a silent edit. Pinned by a golden.
	t.Run("is pinned to its domain string", func(t *testing.T) {
		if journalKeyFingerprintDomain != "journal-keyring-fingerprint-v1" {
			t.Fatalf("domain changed to %q: every fingerprint changes, which is a versioned decision", journalKeyFingerprintDomain)
		}
		// Structural domain separation: Append/Verify only ever HMAC hash()'s output, which
		// is a SHA-256 sum and therefore ALWAYS 32 bytes. This domain is 30. No journal
		// signing input can equal it, by length — before any collision argument.
		if len(journalKeyFingerprintDomain) == sha256.Size {
			t.Fatal("domain is exactly a SHA-256 sum's length; it could collide with a journal signing input")
		}
	})
}
