//go:build smoke

package smoke_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestNATSACLCreateGrantsAreExactlyThePinnedSet asserts the EXACT consumer-create authority each
// service principal holds in deploy/nats/nats-server.conf (TKT-327, decision D2). It needs no
// broker: it reads the file the broker loads.
//
// A grant counts as create authority when it can MATCH a create subject, not only when it spells
// one out: `$JS.API.CONSUMER.>`, `$JS.API.>` or `>` grant CREATE as surely as the literal does,
// and a text search for "CONSUMER.CREATE" would miss all three. The broker-side attack test
// (TestNATSServiceCannotWidenItsOwnPluralDurable) proves the removed grants are refused for the
// two plural durables; this test pins the whole remaining set, for every service principal.
//
// SMOKE_NATS_ACL_FILE points it at another file, which is how a mutated copy is checked.
func TestNATSACLCreateGrantsAreExactlyThePinnedSet(t *testing.T) {
	path := env("SMOKE_NATS_ACL_FILE", "../deploy/nats/nats-server.conf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read NATS ACL %s: %v", path, err)
	}
	publish := natsPublishGrants(t, string(raw))

	// Every create API the broker has: the current one (the filter, when single, is a suffix
	// token) and the legacy durable one.
	createAPIs := []string{"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>"}
	want := map[string][]string{
		// access-slot-policy has a SINGLE FilterSubject, so its create subject carries the filter
		// and this grant pins it to the one legitimate value (ADR-072 §5).
		"access": {"$JS.API.CONSUMER.CREATE.PLATFORM.access-slot-policy.platform.catalog.performance.published"},
		// inventory-catalog-offering is pre-provisioned by nats-init (TKT-327).
		"inventory": nil,
		// Pure publishers: no $JS.API grant at all (ADR-072 §5).
		"catalog":  nil,
		"commerce": nil,
	}
	for principal, wantCreate := range want {
		grants, ok := publish[principal]
		if !ok {
			t.Fatalf("%s: no user block with a publish list in %s; the parser cannot see the grants it is meant to pin", principal, path)
		}
		var gotCreate []string
		for _, g := range grants {
			for _, api := range createAPIs {
				if subjectsOverlap(g, api) {
					gotCreate = append(gotCreate, g)
					break
				}
			}
		}
		if !slices.Equal(gotCreate, wantCreate) {
			t.Errorf("%s holds consumer-create grants %q, want exactly %q", principal, gotCreate, wantCreate)
		}
	}
}

var (
	natsUserLine    = regexp.MustCompile(`^\s*user:\s*"([^"]+)"`)
	natsQuotedGrant = regexp.MustCompile(`"([^"]*)"`)
)

// natsPublishGrants returns each user's publish allow list from a nats-server.conf in the shape
// this repository writes: one `user: "<name>"` line per block, then `publish: [` with one quoted
// subject per line, closed by `]`. Comments are stripped first: they quote subjects too. A publish
// written as `{ deny: [...] }` (payments) has no allow list and is skipped.
func natsPublishGrants(t *testing.T, conf string) map[string][]string {
	t.Helper()
	grants := map[string][]string{}
	user := ""
	inPublish := false
	for _, line := range strings.Split(conf, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if m := natsUserLine.FindStringSubmatch(line); m != nil {
			user, inPublish = m[1], false
			continue
		}
		trimmed := strings.TrimSpace(line)
		if user != "" && strings.HasPrefix(trimmed, "publish:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "publish:"))
			if !strings.HasPrefix(rest, "[") {
				continue // the deny form
			}
			inPublish = true
			grants[user] = []string{}
			line = rest
			trimmed = rest
		}
		if !inPublish {
			continue
		}
		for _, m := range natsQuotedGrant.FindAllStringSubmatch(line, -1) {
			grants[user] = append(grants[user], m[1])
		}
		if strings.Contains(trimmed, "]") {
			inPublish = false
		}
	}
	return grants
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

// The overlap rule is what makes the test above see wildcard grants, so it is pinned on its own.
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
}
