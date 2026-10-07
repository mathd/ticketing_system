//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestCommerceHoldsOnlyTheOrganizerAssertionPublicKey is TKT-287 COS5.
//
// The asymmetric assertion is only a boundary while commerce cannot MINT one: a
// commerce compromise must be able to verify catalog's organizer assertions and
// forge none. That holds exactly as long as catalog's signing seed never reaches
// commerce's environment, so this reads the environment the RUNNING commerce
// container actually received (docker inspect), not compose.yaml's text — an
// override file or a shared anchor that leaked the seed would show here and
// nowhere else.
//
// Mutation that is evidence: add CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY to
// commerce's environment in compose.yaml (or to the shared &go-env anchor) and
// this fails on both the name and the value.
func TestCommerceHoldsOnlyTheOrganizerAssertionPublicKey(t *testing.T) {
	seed := os.Getenv("SMOKE_CATALOG_ORGANIZER_ASSERTION_SEED")
	if seed == "" {
		t.Fatal("SMOKE_CATALOG_ORGANIZER_ASSERTION_SEED is not set: scripts/stack-env.sh exports it")
	}
	var envList []string
	if err := json.Unmarshal([]byte(inspect(t, fmt.Sprintf("%s-commerce-1", project), "{{json .Config.Env}}")), &envList); err != nil {
		t.Fatalf("decode commerce container env: %v", err)
	}
	env := map[string]string{}
	for _, kv := range envList {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if !strings.HasPrefix(env["COMMERCE_ORGANIZER_ASSERTION_PUBLIC_KEYS"], "catalog-org/") {
		t.Fatalf("commerce has no organizer-assertion public keyring; it cannot verify the staff operations")
	}
	for name, value := range env {
		// Names only in messages: the seed is a secret even in a throwaway stack.
		if name == "CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY" || name == "CATALOG_ORGANIZER_ASSERTION_KEY" {
			t.Errorf("commerce's environment carries %s — it could mint assertions for any organizer", name)
		}
		if strings.Contains(value, seed) {
			t.Errorf("commerce's environment variable %s contains catalog's organizer-assertion signing seed", name)
		}
	}
}
