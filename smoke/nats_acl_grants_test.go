//go:build smoke

package smoke_test

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestNATSACLCreateGrantsAreExactlyThePinnedSet asserts the EXACT consumer-create authority of
// EVERY principal in deploy/nats/nats-server.conf (TKT-327, decision D2). It needs no broker: it
// reads the file the broker loads.
//
// A grant counts as create authority when it can MATCH a create subject, not only when it spells
// one out: `$JS.API.CONSUMER.>`, `$JS.API.>` or `>` grant CREATE as surely as the literal does,
// and a text search for "CONSUMER.CREATE" would miss all three. The broker-side attack test
// (TestNATSServiceCannotWidenItsOwnPluralDurable) proves the removed grants are refused for the
// two plural durables; this test pins the whole remaining set.
//
// It fails CLOSED. Every grant inside a principal's permissions block must be a double-quoted
// string: NATS also accepts single-quoted and bare strings, which this parser does not read, so
// either one fails the test as unsupported syntax instead of being skipped. A principal with no
// permissions block, or no publish list, may publish anything, and is treated that way.
//
// The gate pins SMOKE_NATS_ACL_FILE to the repository file (scripts/smoke.sh). Set by hand, it
// points the test at a mutated copy.
func TestNATSACLCreateGrantsAreExactlyThePinnedSet(t *testing.T) {
	path := env("SMOKE_NATS_ACL_FILE", "../deploy/nats/nats-server.conf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read NATS ACL %s: %v", path, err)
	}
	principals, err := natsPrincipals(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	// Expected create authority. A principal not named here, including one added later, must
	// hold none.
	want := map[string][]string{
		// The one allowed exception: the admin principal holds `>` (nats-init, operators; ADR-072 §2
		// does not bind it).
		"platform-admin": {">"},
		// access-slot-policy has a SINGLE FilterSubject, so its create subject carries the filter
		// and this grant pins it to the one legitimate value (ADR-072 §5).
		"access": {"$JS.API.CONSUMER.CREATE.PLATFORM.access-slot-policy.platform.catalog.performance.published"},
	}
	for _, required := range []string{"access", "inventory", "payments", "platform-admin"} {
		if _, ok := principals[required]; !ok {
			t.Fatalf("%s: no user %q; the parser cannot see the principals it is meant to pin", path, required)
		}
	}
	names := make([]string, 0, len(principals))
	for name := range principals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got := principals[name].createGrants(); !slices.Equal(got, want[name]) {
			t.Errorf("%s holds consumer-create grants %q, want exactly %q", name, got, want[name])
		}
	}
}

// natsCreateAPIs are every consumer-create API the broker has: the current one (a single filter is
// a suffix token) and the legacy durable one.
var natsCreateAPIs = []string{"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>"}

// natsPublishPermission is one principal's publish permission as NATS evaluates it.
type natsPublishPermission struct {
	allow []string // nil means NATS's default: everything
	deny  []string
}

// createGrants returns the allow grants that confer consumer-create authority: those that overlap
// a create API that no deny entry fully covers.
func (p natsPublishPermission) createGrants() []string {
	allow := p.allow
	if allow == nil {
		allow = []string{">"}
	}
	var out []string
	for _, g := range allow {
		for _, api := range natsCreateAPIs {
			denied := slices.ContainsFunc(p.deny, func(d string) bool { return subjectSubsumes(d, api) })
			if !denied && subjectsOverlap(g, api) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// natsPrincipals parses authorization.users from a nats-server.conf and returns each user's
// publish permission.
func natsPrincipals(conf string) (map[string]natsPublishPermission, error) {
	toks, err := natsTokens(conf)
	if err != nil {
		return nil, err
	}
	p := &natsConfParser{toks: toks}
	root, err := p.mapBody("")
	if err != nil {
		return nil, err
	}
	users := root.get("authorization").get("users")
	if users == nil || users.kind != "array" {
		return nil, fmt.Errorf("no authorization.users array")
	}
	out := map[string]natsPublishPermission{}
	for _, u := range users.items {
		name := u.get("user")
		if name == nil || name.kind != "dq" {
			return nil, fmt.Errorf("a user block has no double-quoted user name")
		}
		perms := u.get("permissions")
		if perms == nil {
			out[name.s] = natsPublishPermission{} // no permissions: may publish anything
			continue
		}
		if err := perms.allLeavesDoubleQuoted(); err != nil {
			return nil, fmt.Errorf("user %q: %w", name.s, err)
		}
		perm, err := natsPublish(perms.get("publish"))
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", name.s, err)
		}
		out[name.s] = perm
	}
	return out, nil
}

func natsPublish(n *natsNode) (natsPublishPermission, error) {
	switch {
	case n == nil:
		return natsPublishPermission{}, nil // no publish key: may publish anything
	case n.kind == "dq":
		return natsPublishPermission{allow: []string{n.s}}, nil
	case n.kind == "array":
		return natsPublishPermission{allow: n.strings()}, nil
	case n.kind == "map":
		var perm natsPublishPermission
		for _, kv := range n.pairs {
			switch {
			case kv.key == "allow" && kv.val.kind == "array":
				perm.allow = append([]string{}, kv.val.strings()...)
			case kv.key == "deny" && kv.val.kind == "array":
				perm.deny = kv.val.strings()
			default:
				return perm, fmt.Errorf("unsupported publish key %q", kv.key)
			}
		}
		return perm, nil
	}
	return natsPublishPermission{}, fmt.Errorf("unsupported publish form")
}

// natsNode is a parsed config value: a double-quoted string, any other scalar, an array or a map.
type natsNode struct {
	kind  string // "dq", "other", "array", "map"
	s     string
	items []*natsNode
	pairs []natsPair
}

type natsPair struct {
	key string
	val *natsNode
}

func (n *natsNode) get(key string) *natsNode {
	if n == nil || n.kind != "map" {
		return nil
	}
	for _, kv := range n.pairs {
		if kv.key == key {
			return kv.val
		}
	}
	return nil
}

func (n *natsNode) strings() []string {
	out := []string{}
	for _, it := range n.items {
		out = append(out, it.s)
	}
	return out
}

// allLeavesDoubleQuoted is the fail-closed rule: a single-quoted or bare grant is refused.
func (n *natsNode) allLeavesDoubleQuoted() error {
	switch n.kind {
	case "dq":
		return nil
	case "array":
		for _, it := range n.items {
			if err := it.allLeavesDoubleQuoted(); err != nil {
				return err
			}
		}
		return nil
	case "map":
		for _, kv := range n.pairs {
			if err := kv.val.allLeavesDoubleQuoted(); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported syntax in permissions: %s is not a double-quoted string", n.s)
}

type natsToken struct {
	kind string // "dq", "sq", "bare", or the punctuation itself
	s    string
}

// natsTokens splits a config into tokens, dropping `#` and `//` comments.
func natsTokens(conf string) ([]natsToken, error) {
	var toks []natsToken
	for i := 0; i < len(conf); {
		c := conf[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#' || strings.HasPrefix(conf[i:], "//"):
			for i < len(conf) && conf[i] != '\n' {
				i++
			}
		case strings.ContainsRune("{}[]:=,", rune(c)):
			toks = append(toks, natsToken{kind: string(c)})
			i++
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(conf) && conf[j] != c {
				if conf[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(conf) {
				return nil, fmt.Errorf("unterminated string")
			}
			kind := "dq"
			if c == '\'' {
				kind = "sq"
			}
			toks = append(toks, natsToken{kind: kind, s: conf[i+1 : j]})
			i = j + 1
		default:
			j := i
			for j < len(conf) && !strings.ContainsRune(" \t\r\n{}[]:=,\"'#", rune(conf[j])) {
				j++
			}
			toks = append(toks, natsToken{kind: "bare", s: conf[i:j]})
			i = j
		}
	}
	return toks, nil
}

type natsConfParser struct {
	toks []natsToken
	pos  int
}

func (p *natsConfParser) peek() *natsToken {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

// mapBody reads `key [:|=] value` pairs until the closing token ("" means end of input).
func (p *natsConfParser) mapBody(closing string) (*natsNode, error) {
	n := &natsNode{kind: "map"}
	for {
		t := p.peek()
		if t == nil {
			if closing == "" {
				return n, nil
			}
			return nil, fmt.Errorf("missing %q", closing)
		}
		if t.kind == closing {
			p.pos++
			return n, nil
		}
		if t.kind == "," {
			p.pos++
			continue
		}
		if t.kind != "bare" && t.kind != "dq" {
			return nil, fmt.Errorf("unexpected %q where a key belongs", t.kind)
		}
		key := t.s
		p.pos++
		if nt := p.peek(); nt != nil && (nt.kind == ":" || nt.kind == "=") {
			p.pos++
		}
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		n.pairs = append(n.pairs, natsPair{key: key, val: val})
	}
}

func (p *natsConfParser) value() (*natsNode, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("missing value")
	}
	p.pos++
	switch t.kind {
	case "{":
		return p.mapBody("}")
	case "[":
		n := &natsNode{kind: "array"}
		for {
			nt := p.peek()
			if nt == nil {
				return nil, fmt.Errorf("missing \"]\"")
			}
			if nt.kind == "]" {
				p.pos++
				return n, nil
			}
			if nt.kind == "," {
				p.pos++
				continue
			}
			it, err := p.value()
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, it)
		}
	case "dq":
		return &natsNode{kind: "dq", s: t.s}, nil
	case "sq":
		return &natsNode{kind: "other", s: "'" + t.s + "'"}, nil
	case "bare":
		return &natsNode{kind: "other", s: t.s}, nil
	}
	return nil, fmt.Errorf("unexpected %q where a value belongs", t.kind)
}

// subjectsOverlap reports whether some concrete subject matches both NATS subject patterns, with
// `*` matching one token and a trailing `>` one or more.
func subjectsOverlap(a, b string) bool {
	at, bt := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; ; i++ {
		aEnd, bEnd := i >= len(at), i >= len(bt)
		switch {
		case aEnd && bEnd:
			return true
		case !aEnd && at[i] == ">":
			return !bEnd
		case !bEnd && bt[i] == ">":
			return !aEnd
		case aEnd || bEnd:
			return false
		case at[i] == "*" || bt[i] == "*" || at[i] == bt[i]:
			continue
		default:
			return false
		}
	}
}

// subjectSubsumes reports whether every subject matching pattern p also matches pattern d.
func subjectSubsumes(d, p string) bool {
	dt, pt := strings.Split(d, "."), strings.Split(p, ".")
	for i := 0; ; i++ {
		dEnd, pEnd := i >= len(dt), i >= len(pt)
		switch {
		case dEnd && pEnd:
			return true
		case !dEnd && dt[i] == ">":
			return !pEnd
		case dEnd || pEnd:
			return false
		case pt[i] == ">":
			return false // p reaches further than a single-token d
		case dt[i] == "*" || dt[i] == pt[i]:
			continue
		default:
			return false
		}
	}
}

// The matching rules are what make the test above see wildcard grants and deny lists, so they are
// pinned on their own.
func TestNATSSubjectsOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"$JS.API.CONSUMER.CREATE.PLATFORM.x", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.CONSUMER.CREATE.PLATFORM.x.>", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.CONSUMER.>", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.>", "$JS.API.CONSUMER.DURABLE.CREATE.>", true},
		{">", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.*.CREATE.PLATFORM.x", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.CONSUMER.INFO.PLATFORM.*", "$JS.API.CONSUMER.CREATE.>", false},
		{"$JS.API.CONSUMER.MSG.NEXT.PLATFORM.x", "$JS.API.CONSUMER.CREATE.>", false},
		{"$JS.API.CONSUMER.CREATE", "$JS.API.CONSUMER.CREATE.>", false},
		{"platform.access.ticket-issuance.failed", "$JS.API.CONSUMER.CREATE.>", false},
	} {
		if got := subjectsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("subjectsOverlap(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
	for _, tc := range []struct {
		d, p string
		want bool
	}{
		{">", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.>", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.CREATE.>", true},
		{"$JS.API.CONSUMER.CREATE.PLATFORM.*", "$JS.API.CONSUMER.CREATE.>", false},
		{"$JS.API.CONSUMER.CREATE.PLATFORM.x", "$JS.API.CONSUMER.CREATE.>", false},
		{"$JS.API.CONSUMER.INFO.>", "$JS.API.CONSUMER.CREATE.>", false},
	} {
		if got := subjectSubsumes(tc.d, tc.p); got != tc.want {
			t.Errorf("subjectSubsumes(%q, %q) = %t, want %t", tc.d, tc.p, got, tc.want)
		}
	}
}
