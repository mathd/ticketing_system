// TKT-356 standalone candidate model. It does not import or exercise the
// ticketing services or scanner application.
package main

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

var base = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
var failedAssertions bool

type credential struct {
	Version int       `json:"v"`
	Ticket  string    `json:"tid"`
	KeyID   string    `json:"kid"`
	NBF     time.Time `json:"nbf"`
	EXP     time.Time `json:"exp"`
}

type signedCredential struct {
	Body string `json:"body"`
	MAC  string `json:"mac"`
}

type staticIdentity struct {
	Version int    `json:"v"`
	Ticket  string `json:"tid"`
	KeyID   string `json:"kid"`
}

func check(name string, got, want any) {
	if fmt.Sprint(got) != fmt.Sprint(want) {
		fmt.Fprintf(os.Stderr, "FAIL %s: got %v, want %v\n", name, got, want)
		failedAssertions = true
		return
	}
	fmt.Printf("PASS %-50s got=%v want=%v\n", name, got, want)
}

func mint(c credential, private ed25519.PrivateKey) signedCredential {
	b, _ := json.Marshal(c)
	return signedCredential{base64.RawURLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, b))}
}

func verify(s signedCredential, keys map[string]ed25519.PublicKey) (credential, bool) {
	var c credential
	b, err := base64.RawURLEncoding.DecodeString(s.Body)
	sig, sigErr := base64.RawURLEncoding.DecodeString(s.MAC)
	if err != nil || sigErr != nil || json.Unmarshal(b, &c) != nil || c.Version != 2 {
		return c, false
	}
	key, ok := keys[c.KeyID]
	return c, ok && ed25519.Verify(key, b, sig)
}

func verifyStaticIdentity(s signedCredential, keys map[string]ed25519.PublicKey) (staticIdentity, bool) {
	var identity staticIdentity
	b, err := base64.RawURLEncoding.DecodeString(s.Body)
	sig, sigErr := base64.RawURLEncoding.DecodeString(s.MAC)
	if err != nil || sigErr != nil || json.Unmarshal(b, &identity) != nil || identity.Version != 1 {
		return identity, false
	}
	key, ok := keys[identity.KeyID]
	return identity, ok && ed25519.Verify(key, b, sig)
}

func offline(c credential, now time.Time, skew time.Duration) bool {
	return !now.Before(c.NBF.Add(-skew)) && now.Before(c.EXP.Add(skew))
}

func online(c credential, now time.Time) bool {
	return !now.Before(c.NBF) && now.Before(c.EXP)
}

func verifyAndAdmit(s signedCredential, keys map[string]ed25519.PublicKey, now time.Time) bool {
	c, ok := verify(s, keys)
	return ok && online(c, now)
}

type occurrence struct {
	ID, Ticket, Gate, Decision string
	At                         time.Time
}

type ledger struct {
	byID       map[string]occurrence
	admitted   map[string]string
	Admissions int
	Refusals   int
}

func (l *ledger) sync(o occurrence) string {
	if o.Decision != "admitted" && !knownLocalRefusal(o.Decision) {
		return "unknown_decision"
	}
	key := o.ID
	if prior, ok := l.byID[key]; ok {
		if prior.At.Equal(o.At) {
			if prior.Ticket == o.Ticket && prior.Gate == o.Gate && prior.Decision == o.Decision {
				return "replay"
			}
			return "occurrence_collision"
		}
		return "occurrence_collision"
	}
	l.byID[key] = o
	if knownLocalRefusal(o.Decision) {
		l.Refusals++
		return "refusal_recorded"
	}
	if priorGate, ok := l.admitted[o.Ticket]; ok {
		if priorGate == o.Gate {
			return "conflict_same_gate"
		}
		return "conflict_second_gate"
	}
	l.admitted[o.Ticket] = o.Gate
	l.Admissions++
	return "admission_recorded"
}

func knownLocalRefusal(decision string) bool {
	switch decision {
	case "expired_credential", "unknown_clock", "not_yet_valid":
		return true
	default:
		return false
	}
}

type clockEvidence struct {
	Present  bool
	LastWall time.Time
	NowWall  time.Time
}

func clockTrusted(e clockEvidence) bool {
	return e.Present && !e.NowWall.Before(e.LastWall)
}

func evaluateLocal(s signedCredential, keys map[string]ed25519.PublicKey, now time.Time, evidence clockEvidence) (string, bool) {
	c, ok := verify(s, keys)
	if !ok {
		return "invalid_credential", false
	}
	if !clockTrusted(evidence) {
		return "unknown_clock", false
	}
	if !offline(c, now, 5*time.Second) {
		if now.Before(c.NBF.Add(-5 * time.Second)) {
			return "not_yet_valid", false
		}
		return "expired_credential", false
	}
	return "admitted", true
}

func main() {
	const seed = "TKT-356 fixed synthetic seed; probe only"
	seedBytes := sha256.Sum256([]byte(seed))
	private := ed25519.NewKeyFromSeed(seedBytes[:])
	public := private.Public().(ed25519.PublicKey)
	keys := map[string]ed25519.PublicKey{"probe-key-1": public}
	old := credential{Version: 2, Ticket: "ticket-probe-001", KeyID: "probe-key-1", NBF: base, EXP: base.Add(30 * time.Second)}
	fresh := credential{Version: 2, Ticket: old.Ticket, KeyID: old.KeyID, NBF: base.Add(30 * time.Second), EXP: base.Add(60 * time.Second)}
	oldSigned := mint(old, private)
	freshSigned := mint(fresh, private)

	fmt.Println("\nOpaque short-code candidate.")
	shortCodes := map[string]struct {
		ticket string
		expiry time.Time
	}{"opaque-probe-7f3a": {ticket: old.Ticket, expiry: old.EXP}}
	lookup := func(code, ticket string, now time.Time) bool {
		entry, ok := shortCodes[code]
		return ok && entry.ticket == ticket && now.Before(entry.expiry)
	}
	check("preloaded opaque code resolves for bound ticket", lookup("opaque-probe-7f3a", old.Ticket, base.Add(time.Second)), true)
	check("opaque code rejects another ticket", lookup("opaque-probe-7f3a", "ticket-other", base.Add(time.Second)), false)
	check("opaque code rejects at expiry", lookup("opaque-probe-7f3a", old.Ticket, old.EXP), false)
	check("offline opaque lookup without mapping", lookup("offline-code-not-preloaded", old.Ticket, base.Add(time.Second)), false)

	fmt.Println("TKT-356 fixed-clock candidate model; all IDs and signing material are synthetic.")
	fmt.Println("Online interval [nbf, exp); candidate offline interval [nbf-5s, exp+5s).")
	for _, x := range []struct {
		name string
		c    credential
		lo   time.Time
		hi   time.Time
	}{{"old", old, base, base.Add(30 * time.Second)}, {"fresh", fresh, base.Add(30 * time.Second), base.Add(60 * time.Second)}} {
		for _, v := range []struct {
			label string
			t     time.Time
			want  bool
		}{{"nbf-1s", x.lo.Add(-time.Second), false}, {"nbf", x.lo, true}, {"nbf+1s", x.lo.Add(time.Second), true}, {"exp-1s", x.hi.Add(-time.Second), true}, {"exp", x.hi, false}, {"exp+1s", x.hi.Add(time.Second), false}} {
			check("online "+x.name+" "+v.label, online(x.c, v.t), v.want)
		}
		for _, v := range []struct {
			label string
			t     time.Time
			want  bool
		}{{"nbf-5s-1s", x.lo.Add(-6 * time.Second), false}, {"nbf-5s", x.lo.Add(-5 * time.Second), true}, {"nbf-5s+1s", x.lo.Add(-4 * time.Second), true}, {"exp+5s-1s", x.hi.Add(4 * time.Second), true}, {"exp+5s", x.hi.Add(5 * time.Second), false}, {"exp+5s+1s", x.hi.Add(6 * time.Second), false}} {
			check("offline "+x.name+" "+v.label, offline(x.c, v.t, 5*time.Second), v.want)
		}
	}

	fmt.Println("\nOld screenshot at true T+35s; device offsets are seconds.")
	for _, offset := range []int{-6, -5, 0, 5, 6} {
		deviceTime := base.Add(35*time.Second + time.Duration(offset)*time.Second)
		check(fmt.Sprintf("old screenshot true=T+35s device-offset=%+ds", offset), offline(old, deviceTime, 5*time.Second), offset < 0)
	}
	fmt.Println("Fresh credential at true T+35s; all tested device offsets fall within [T+25s, T+65s).")
	for _, offset := range []int{-6, -5, 0, 5, 6} {
		deviceTime := base.Add(35*time.Second + time.Duration(offset)*time.Second)
		check(fmt.Sprintf("fresh credential true=T+35s device-offset=%+ds", offset), offline(fresh, deviceTime, 5*time.Second), true)
	}
	check("old screenshot true=T+39s, slowest allowed device=-5s", offline(old, base.Add(34*time.Second), 5*time.Second), true)
	check("old screenshot true=T+40s, slowest allowed device=-5s", offline(old, base.Add(35*time.Second), 5*time.Second), false)

	// Shared-secret time codes are not implemented as a third candidate. This
	// small capability check records that a verifier holding the MAC key can mint.
	shared := []byte("synthetic shared verification secret")
	message := []byte("ticket-probe-001|2026-09-27T12:00:30Z")
	mintMAC := hmac.New(sha256.New, shared)
	mintMAC.Write(message)
	mac := mintMAC.Sum(nil)
	verifyMAC := hmac.New(sha256.New, shared)
	verifyMAC.Write(message)
	check("shared-secret verifier can create accepted code MAC", hmac.Equal(mac, verifyMAC.Sum(nil)), true)

	_, ok := verify(oldSigned, keys)
	check("Ed25519 candidate signature", ok, true)
	tampered := oldSigned
	tampered.MAC = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	_, ok = verify(tampered, keys)
	check("tampered signature refused", ok, false)
	_, ok = verify(oldSigned, map[string]ed25519.PublicKey{"other-key": public})
	check("unknown key refused", ok, false)
	check("online old payload at T+35s", verifyAndAdmit(oldSigned, keys, base.Add(35*time.Second)), false)
	check("online fresh payload at T+35s", verifyAndAdmit(freshSigned, keys, base.Add(35*time.Second)), true)

	// Current qrLinkSigner.verify copies the source predicate: now.Unix() > expiry.
	imageExpiry := base.Unix() + 600
	for _, x := range []struct {
		label string
		delta time.Duration
		want  bool
	}{{"T+599s", -time.Second, true}, {"T+600s", 0, true}, {"T+600.5s", 500 * time.Millisecond, true}, {"T+601s", time.Second, false}} {
		check("image link "+x.label, base.Add(600*time.Second+x.delta).Unix() <= imageExpiry, x.want)
	}
	staticBody, _ := json.Marshal(staticIdentity{Version: 1, Ticket: "ticket-probe-static", KeyID: "probe-key-1"})
	staticSigned := signedCredential{base64.RawURLEncoding.EncodeToString(staticBody), base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, staticBody))}
	staticCheckAt := base.Add(601 * time.Second)
	imageExpired := staticCheckAt.Unix() > imageExpiry
	check("image URL is expired at static identity check time T+601s", imageExpired, true)
	_, staticOK := verifyStaticIdentity(staticSigned, keys)
	check("synthetic no-expiry static signed identity verifies after image URL expiry at T+601s", imageExpired && staticOK, true)

	fmt.Println("\nOffline time evidence model.")
	check("unknown clock evidence refuses", clockTrusted(clockEvidence{}), false)
	check("clock rollback detected", clockTrusted(clockEvidence{Present: true, LastWall: base.Add(5 * time.Second), NowWall: base}), false)
	check("wall advance under toy rollback check", clockTrusted(clockEvidence{Present: true, LastWall: base, NowWall: base.Add(time.Second)}), true)

	fmt.Println("\nOccurrence/reconciliation candidate model.")
	localAt := base.Add(29 * time.Second)
	arrival := base.Add(120 * time.Second)
	check("delayed online receipt sees credential expiry", online(old, arrival), false)
	check("local admission at T+29s passed offline check", offline(old, localAt, 5*time.Second), true)
	l := ledger{byID: map[string]occurrence{}, admitted: map[string]string{}}
	trustedAt := clockEvidence{Present: true, LastWall: base, NowWall: localAt}
	localDecision, gateOpen := evaluateLocal(oldSigned, keys, localAt, trustedAt)
	check("valid local evaluation decision", localDecision, "admitted")
	check("valid local evaluation opens gate", gateOpen, true)
	admission := occurrence{ID: "occurrence-a1", Ticket: old.Ticket, Gate: "gate-a", Decision: localDecision, At: localAt}
	check("delayed admission sync outcome", l.sync(admission), "admission_recorded")
	check("delayed admission count", l.Admissions, 1)
	check("same occurrence repeated", l.sync(admission), "replay")
	amendedReplay := admission
	amendedReplay.Decision = "expired_credential"
	check("same occurrence with changed decision collides", l.sync(amendedReplay), "occurrence_collision")
	secondGate := occurrence{ID: "occurrence-b1", Ticket: old.Ticket, Gate: "gate-b", Decision: "admitted", At: localAt.Add(time.Second)}
	check("distinct occurrence at second gate", l.sync(secondGate), "conflict_second_gate")
	check("second gate does not add admission", l.Admissions, 1)

	missingClock := clockEvidence{}
	missingDecision, missingGate := evaluateLocal(oldSigned, keys, localAt, missingClock)
	check("missing clock local evaluation decision", missingDecision, "unknown_clock")
	check("missing clock local evaluation keeps gate closed", missingGate, false)
	missingRefusal := occurrence{ID: "occurrence-clock-missing", Ticket: old.Ticket, Gate: "gate-a", Decision: missingDecision, At: localAt}
	check("missing clock refusal sync", l.sync(missingRefusal), "refusal_recorded")
	check("same missing clock refusal repeated", l.sync(missingRefusal), "replay")
	check("missing clock refusal does not add admission", l.Admissions, 1)

	rollbackClock := clockEvidence{Present: true, LastWall: base.Add(5 * time.Second), NowWall: base}
	rollbackDecision, rollbackGate := evaluateLocal(oldSigned, keys, localAt, rollbackClock)
	check("rolled-back clock local evaluation decision", rollbackDecision, "unknown_clock")
	check("rolled-back clock local evaluation keeps gate closed", rollbackGate, false)
	rollbackRefusal := occurrence{ID: "occurrence-clock-rollback", Ticket: old.Ticket, Gate: "gate-a", Decision: rollbackDecision, At: localAt}
	check("rolled-back clock refusal sync", l.sync(rollbackRefusal), "refusal_recorded")
	check("same rolled-back clock refusal repeated", l.sync(rollbackRefusal), "replay")
	check("clock refusals do not add admission", l.Admissions, 1)

	earlyAt := base.Add(-6 * time.Second)
	earlySigned := mint(old, private)
	earlyEvidence := clockEvidence{Present: true, LastWall: base.Add(-7 * time.Second), NowWall: earlyAt}
	earlyDecision, earlyGate := evaluateLocal(earlySigned, keys, earlyAt, earlyEvidence)
	check("early signed credential at T-6 local evaluation decision", earlyDecision, "not_yet_valid")
	check("early signed credential keeps gate closed", earlyGate, false)
	earlyRefusal := occurrence{ID: "occurrence-early", Ticket: old.Ticket, Gate: "gate-a", Decision: earlyDecision, At: earlyAt}
	check("early refusal sync", l.sync(earlyRefusal), "refusal_recorded")
	check("same early refusal repeated", l.sync(earlyRefusal), "replay")
	check("early refusal does not add admission", l.Admissions, 1)
	equalLowerBound := base.Add(-5 * time.Second)
	equalDecision, equalGate := evaluateLocal(oldSigned, keys, equalLowerBound, clockEvidence{Present: true, LastWall: equalLowerBound.Add(-time.Second), NowWall: equalLowerBound})
	check("offline lower-bound equality local evaluation decision", equalDecision, "admitted")
	check("offline lower-bound equality opens gate", equalGate, true)

	refusalAt := base.Add(90 * time.Second)
	expired := credential{Version: 2, Ticket: "ticket-probe-expired", KeyID: "probe-key-1", NBF: base, EXP: base.Add(30 * time.Second)}
	expiredSigned := mint(expired, private)
	refusalDecision, refusalGate := evaluateLocal(expiredSigned, keys, refusalAt, clockEvidence{Present: true, LastWall: base, NowWall: refusalAt})
	check("expired local evaluation decision", refusalDecision, "expired_credential")
	check("expired local evaluation keeps gate closed", refusalGate, false)
	refusal := occurrence{ID: "occurrence-c1", Ticket: expired.Ticket, Gate: "gate-c", Decision: refusalDecision, At: refusalAt}
	check("expired refusal sync", l.sync(refusal), "refusal_recorded")
	check("same refusal occurrence repeated", l.sync(refusal), "replay")
	check("refusal does not add admission", l.Admissions, 1)
	check("local refusal occurrence count", l.Refusals, 4)
	unknown := occurrence{ID: "occurrence-unknown", Ticket: "ticket-unknown", Gate: "gate-x", Decision: "future_decision", At: refusalAt}
	beforeUnknown := fmt.Sprintf("rows=%d admissions=%d refusals=%d admitted=%d", len(l.byID), l.Admissions, l.Refusals, len(l.admitted))
	check("unknown decision rejected before ledger mutation", l.sync(unknown), "unknown_decision")
	afterUnknown := fmt.Sprintf("rows=%d admissions=%d refusals=%d admitted=%d", len(l.byID), l.Admissions, l.Refusals, len(l.admitted))
	check("unknown decision leaves ledger unchanged", afterUnknown, beforeUnknown)

	fmt.Println("\nReconciliation admission-skew copied predicate: abs(device-receipt) > 24h.")
	for _, x := range []struct {
		label string
		d     time.Duration
		want  bool
	}{{"+24h-1s", 24*time.Hour - time.Second, false}, {"+24h", 24 * time.Hour, false}, {"+24h+1s", 24*time.Hour + time.Second, true}, {"-24h", -24 * time.Hour, false}, {"-24h-1s", -24*time.Hour - time.Second, true}} {
		delta := x.d
		if delta < 0 {
			delta = -delta
		}
		check("reconciliation flag "+x.label, delta > 24*time.Hour, x.want)
	}
	fmt.Println("LIMIT: model only. The clock-evidence input is synthetic and does not attest real time. No scanner integration, mode enforcement, refusal routing, or production key use.")
	if failedAssertions {
		os.Exit(1)
	}
}
