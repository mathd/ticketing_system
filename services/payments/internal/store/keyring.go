package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// minSecretLen is the floor the single-key configuration has always applied to
// JOURNAL_SIGNING_KEY. Historical keys carry the same bound: a retired key still
// verifies real money history, so it is not held to a weaker standard than the
// active one.
const minSecretLen = 16

// kidPattern bounds a key id to a printable token.
//
// This is parser and diagnostics hygiene, NOT the argument the access lifecycle
// keyring makes. There (keys.go), the kid is signed INSIDE a newline-separated
// canonical form, so a kid containing a newline shifts every later field and two
// different messages can canonicalize to the same bytes. Payments' canonical form
// (see canonical()) ends at the payload JSON and contains no kid at all, so that
// ambiguity cannot arise here. What does matter: ',' and '=' are the keyring
// grammar's delimiters, and whitespace or control characters make operator tooling
// and error messages ambiguous. No namespace prefix is required — the deployed
// default kid is "local-v1", and a namespace rule would reject it for nothing.
var kidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// Keyring holds the journal's HMAC signing keys: one active key that new entries
// are signed under, plus retired keys retained so their era stays verifiable
// (ADR-016 §Decision 8; ADR-032 §Keyring configuration, rotation and retirement).
//
// Name the adversary (ADR-021). This ring is SECRET material, not public keys —
// which is the whole difference from the access lifecycle keyring it is modelled
// on. There, a verifier holds only public keys and genuinely cannot write history.
// Here, every holder of the ring can forge under every kid in it. What rotation
// buys is that retiring a key does not invalidate its history; it does not buy a
// verifier that lacks signing power, and nothing in this package may be described
// as if it did.
type Keyring struct {
	activeKID string
	keys      map[string][]byte
}

// NewKeyring builds the verification ring from the active key plus an optional
// comma-separated list of retired keys, each "kid=<base64.RawStdEncoding secret>".
// The active key is always a member of its own ring.
//
// Every inconsistency is a construction error rather than a runtime surprise, so
// a malformed ring fails the process at startup the way a missing signing key
// already does. Errors never echo secret material.
func NewKeyring(activeKID string, activeKey []byte, historical string) (*Keyring, error) {
	if activeKID == "" || len(activeKey) == 0 {
		return nil, errors.New("journal keyring: an active key id and active key are required")
	}
	if err := validKID(activeKID); err != nil {
		return nil, err
	}
	if len(activeKey) < minSecretLen {
		return nil, fmt.Errorf("journal keyring: active key must be at least %d bytes", minSecretLen)
	}

	ring := &Keyring{activeKID: activeKID, keys: map[string][]byte{activeKID: copyOf(activeKey)}}
	// material maps the EFFECTIVE HMAC key -> the kid that claimed it, so a duplicate
	// can be reported without printing either secret. Effective, not raw: see
	// effectiveHMACKey.
	material := map[string]string{string(effectiveHMACKey(activeKey)): activeKID}

	// An entirely empty variable means "no historical keys" — the single-key
	// configuration. Everything else is parsed, including a whitespace-only value:
	// treating "   " as unset would let an operator typo boot a single-key ring and
	// leave pre-rotation history unverifiable with no startup error. Inside a non-empty
	// list an empty segment is malformed input, and the contract is fail-closed:
	// silently skipping it would accept "v1=...,," and ",".
	if historical != "" {
		for _, item := range strings.Split(historical, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, errors.New("journal keyring: empty entry in JOURNAL_HISTORICAL_KEYS (stray or doubled comma)")
			}
			// Cut on the FIRST '=' only: kids cannot contain '=' (kidPattern), and a
			// base64 value could in principle carry padding. RawStdEncoding is required
			// below, so padding is rejected rather than silently tolerated.
			kid, encoded, ok := strings.Cut(item, "=")
			if !ok || encoded == "" {
				return nil, errors.New("journal keyring: historical entry must be \"kid=<base64 secret>\"")
			}
			if err := validKID(kid); err != nil {
				return nil, err
			}
			secret, err := base64.RawStdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("journal keyring: historical key %q is not unpadded base64 (base64.RawStdEncoding)", kid)
			}
			if len(secret) < minSecretLen {
				return nil, fmt.Errorf("journal keyring: historical key %q must be at least %d bytes", kid, minSecretLen)
			}
			if _, exists := ring.keys[kid]; exists {
				return nil, fmt.Errorf("journal keyring: duplicate key id %q", kid)
			}
			// Two kids that SIGN THE SAME would let a database writer who holds NO secret
			// relabel a row's key_id between them with verification still passing, because
			// key_id is not inside canonical v1 and the signature does not bind it. The
			// damage is era misattribution — it corrupts retirement accounting and the
			// unknown-key contract — not content forgery. Binding the kid into the
			// signature would be the stronger fix and is a canonical-version change, which
			// ADR-032 puts outside this slice. Rejecting the alias here closes it at the
			// only place kids are ever resolved.
			if other, dup := material[string(effectiveHMACKey(secret))]; dup {
				return nil, fmt.Errorf("journal keyring: key ids %q and %q resolve to the same HMAC key; two ids over one key is an alias, not a rotation", other, kid)
			}
			material[string(effectiveHMACKey(secret))] = kid
			ring.keys[kid] = secret
		}
	}
	return ring, nil
}

// effectiveHMACKey returns the key HMAC actually signs with, which is NOT the key you
// configured. RFC 2104 preprocesses it: a key longer than the hash block size is
// replaced by its digest, and a shorter one is zero-padded up to the block size.
//
// So distinct configured secrets can be the SAME key to HMAC: "abc" and "abc\x00" both
// pad to the same block, and a 65-byte key signs identically to its SHA-256. A duplicate
// check comparing RAW bytes therefore misses exactly the aliases it exists to reject,
// leaving the era-misattribution vector above open under a configuration that looks
// perfectly valid. Both cases were confirmed against crypto/hmac before this was
// written; keyring_test.go pins them.
func effectiveHMACKey(key []byte) []byte {
	block := make([]byte, sha256.BlockSize)
	if len(key) > sha256.BlockSize {
		sum := sha256.Sum256(key)
		copy(block, sum[:])
		return block
	}
	copy(block, key)
	return block
}

func validKID(kid string) error {
	if !kidPattern.MatchString(kid) {
		return fmt.Errorf("journal keyring: key id %q must match %s (no whitespace, ',' or '=': they delimit the keyring)", kid, kidPattern)
	}
	return nil
}

func copyOf(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// ActiveKeyID is the key id new entries are signed under.
func (k *Keyring) ActiveKeyID() string { return k.activeKID }

// journalKeyFingerprintDomain is the fixed input the fingerprint HMACs. It is NOT a
// journal signing input, and that is provable rather than asserted: Append and Verify
// only ever HMAC the output of hash() (store.go), which is a SHA-256 sum and therefore
// ALWAYS exactly 32 bytes. This constant is 30 bytes, so no journal signing input can
// equal it — by length, before any collision argument is needed. Changing this string
// changes every fingerprint, so it is versioned; the golden in the payments cmd test
// pins it.
const journalKeyFingerprintDomain = "journal-keyring-fingerprint-v1"

// ActiveKeyFingerprint returns a short, non-reversible identifier for the active signing
// key: the first 4 bytes of HMAC(activeKey, domain), as 8 lowercase hex characters.
//
// What it is FOR. JOURNAL_SIGNING_KEY is raw while JOURNAL_HISTORICAL_KEYS entries are
// base64, and the rotation runbook teaches the base64 encoding one step before setting the
// raw active key. Pasting a base64 blob into the raw variable passes every validation here
// — it is well over minSecretLen — so the service boots and signs money facts under a key
// nobody recorded, and nothing notices until a verify-journal that no deployment schedules.
// This turns that into one startup log line an operator can compare against a value they
// recorded at rotation time.
//
// What it is NOT. It is not a security control and must never be described as one. This
// ring holds SECRET material: every holder can forge under every kid in it (see the type
// comment above, and ADR-021 §the trust boundary). The fingerprint defends against an
// honest operator's paste error, not against an adversary. Eight hex characters are a
// 32-bit identifier, so it is evidence of a MISMATCH, never proof of a match.
//
// Computed over the EFFECTIVE HMAC key, not the configured bytes, so it answers the
// question an operator actually has — "will this key verify my history?" Two configurations
// that HMAC identically are the same key to the journal and print the same fingerprint.
// Truncation is the second barrier: sign() returns a full 32-byte MAC, so 4 bytes of a
// different-domain MAC can never be replayed as a journal signature.
func (k *Keyring) ActiveKeyFingerprint() string {
	mac := sign(effectiveHMACKey(k.keys[k.activeKID]), []byte(journalKeyFingerprintDomain))
	return fmt.Sprintf("%x", mac[:4])
}

// Has reports whether the ring can verify entries signed under kid. Mirrors the
// access lifecycle keyring's Has (keys.go) — it exists so configuration can be
// asserted without exposing key material.
func (k *Keyring) Has(kid string) bool { _, ok := k.keys[kid]; return ok }

// keyFor resolves a stored entry's key id. It returns the ring's own slice rather than a
// copy: every caller is inside this package and treats it as read-only, and copying here
// would allocate once per entry inside Verify's scan over the whole journal. The ingress
// copy in NewKeyring is what stops a CALLER's buffer from mutating the ring; this is the
// narrower internal invariant, stated so it stays deliberate. An unknown kid is an explicit error,
// never a skip and never a silent fallback to the active key: an entry naming a
// key the ring does not hold is exactly the retirement consequence ADR-016
// §Decision 8 requires be visible.
func (k *Keyring) keyFor(kid string) ([]byte, error) {
	key, ok := k.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return key, nil
}

// sign produces the entry signature under the active key.
func (k *Keyring) sign(sum []byte) []byte { return sign(k.keys[k.activeKID], sum) }

// verify checks a stored signature under the key its entry names.
func (k *Keyring) verify(kid string, sum, signature []byte) error {
	key, err := k.keyFor(kid)
	if err != nil {
		return err
	}
	if !hmac.Equal(sign(key, sum), signature) {
		return fmt.Errorf("invalid signature under key %q", kid)
	}
	return nil
}
