# A fingerprint of a symmetric secret is an oracle

**TKT-117, PR #130.** A ticket asked for a short non-reversible fingerprint of the journal signing
key, logged at startup, so an operator could spot a mis-pasted key. The plan gate accepted it. The
code review refuted it. The ticket's premise was the bug.

## What happened

`JOURNAL_SIGNING_KEY` is raw while `JOURNAL_HISTORICAL_KEYS` entries are base64, and the rotation
runbook prints the outgoing key's base64 one step before asking for the new raw key. Pasting step 1
into step 3 passes every validation — it clears the 16-byte floor — so payments boots and signs real
money facts under a key nobody recorded.

The proposed fix: log `HMAC(activeKey, "journal-keyring-fingerprint-v1")[:4]` as 8 hex characters.
Considerable care went into it. The domain string was proven not to collide with journal signing
input (`sign` only ever MACs `hash()`'s output, always 32 bytes; the domain is 30 — different by
length, before any collision argument). The output was truncated so it could not be replayed as a
signature. A test asserted the raw secret and its base64 form were absent from the log.

All of that was true, and none of it was the risk.

**A deterministic function of a secret, published for a fixed public message, is an offline
verification oracle.** Anyone who can read logs computes `HMAC(candidate, domain)[:4]` for candidate
keys and stops when it matches. The defence against that is key entropy — and nothing required any:
`minSecretLen` checks *length*, the runbook never asked for a CSPRNG, and this repo's own default is
the readable string `local-development-journal-key`. The vulnerable precondition shipped by default.

The test could not have caught it either. `strings.Contains(out, rawSecret)` proves the literal
secret is not printed. It says nothing about a *derived verifier*, which is what leaked. It passed
for a reason unrelated to its security claim.

## The rules

**1. Public-key intuitions do not transfer to symmetric keys.** SSH publishes key fingerprints; JWK
has thumbprints; git publishes commit hashes. Those are digests of *public* material, where an
offline oracle proves nothing an attacker did not already have. For a **secret**, the same
construction hands out a free verification function. `keyring.go`'s own header already said this ring
is secret material and every holder can forge under every kid — the fingerprint was designed as
though it were a public-key ring anyway.

**2. Do not describe a secret-derived value as "reveals no key material."** It reveals no key
material *and* enables key recovery against a weak key. Both are true; only the second matters.

**3. Prefer rejecting the mistake to reporting it.** The replacement refuses to start when the active
key **decodes** to a key already in the ring. It is better on every axis: fails closed, fires at the
moment of the error rather than at a `verify-journal` nothing schedules, needs no operator to
remember to compare anything, and discloses nothing. When a diagnostic and a rejection both address
a mistake, the rejection is usually smaller *and* stronger.

## The second lesson: stop enumerating representations

The rejection then took three review passes to get right, because each version asked **"does the
active key look like some particular encoding of a ring secret?"** and was defeated by a
representation it had not enumerated:

| Pass | Comparison | Defeated by |
|---|---|---|
| 1 | the entry's base64 text *as received* | `RawStdEncoding` is non-strict — several texts decode to one secret |
| 2 | the *unpadded canonical* re-encoding | plain `base64` emits **padded** output, a different-length string |
| 3 | — | — |

Each fix was correct about the case in front of it and wrong about the shape of the problem.
Enumerating a fourth representation would have invited a fifth.

**The fix was to change the question**: *does the active key **decode** to a secret already in this
ring?* Comparing decoded bytes collapses every accepted representation — padded, unpadded,
non-canonical, CR/LF-wrapped — into one answer. Pass 4 approved with no findings.

**When successive fixes each close one instance of a class, stop fixing instances.** The signal is
findings that are individually valid, individually shrinking, and all the same *shape*. That is not
convergence toward correctness; it is enumeration, and enumeration of an open set never terminates.
Ask what question the code is asking, and whether a different question has a closed answer.

Scope was then stated precisely instead of broadly: standard base64 padded and unpadded are covered;
URL-safe is **excluded and documented as excluded**, because half-covering it silently would be worse
than not covering it. A guarantee that names its own boundary is worth more than one that implies
more than it delivers.
